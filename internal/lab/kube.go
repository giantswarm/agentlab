package lab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/giantswarm/agentlab/pkg/project"
)

// The embedded Kubernetes client. Every call the lab makes to its cluster's
// apiserver — applying rendered manifests, reading a resource's status,
// deleting, patching, waiting for a rollout or a condition, the RBAC and
// identity probes, pod logs, a probe pod — runs in this process through
// client-go, bound to the lab-owned kubeconfig through labRESTClientGetter
// (restclient.go): the shell's KUBECONFIG and current-context play no part,
// and a lab that is not up fails on the missing state/kubeconfig, by name.
// The helpers below are the vocabulary the rest of the package speaks; the
// context comes first everywhere and every wait is bounded.
//
// Writes are server-side apply under the field manager applyFieldManager,
// forced: an object an earlier lab applied through kubectl carries the
// kubectl-client-side-apply manager (one kubectl applied server-side carries
// "kubectl"), and without Force the first re-apply over it would be refused
// as a conflict on every field both set. Taking the fields over is exactly
// what a re-apply means here — the rendered manifest is the whole intent.
// What kubectl reported as created/configured/unchanged is
// applyResult.changed: whether the object was created or its
// resourceVersion moved; a re-apply that changes nothing leaves it alone,
// the apiserver skips the write.
//
// Kinds resolve through a discovery-backed RESTMapper cached in memory
// (never under ~/.kube/cache) and reset after a batch carries CRDs, so the
// kinds a CRD introduces resolve for what follows it — in the same batch
// after the CRD is served, and in the rest of the run.

// applyFieldManager is the field manager of every server-side apply the lab
// performs; `kubectl get -o yaml --show-managed-fields` names it.
const applyFieldManager = "agentlab"

// defaultNamespace is where a namespace-scoped object that names none lands:
// kubectl applies such an object in the kubeconfig's namespace, and the kind
// kubeconfig sets none — "default". Also the namespace kubectl evaluates
// `auth can-i` in when no -n is given.
const defaultNamespace = "default"

// crdEstablishTimeout bounds how long applyManifests waits, after applying a
// CustomResourceDefinition, for the apiserver to serve its kinds before
// applying the objects that follow it.
const crdEstablishTimeout = 60 * time.Second

// pollInterval is the cadence of the bounded waits below (a rollout, a
// condition, an object going away) — kubectl's own for `rollout status`.
const pollInterval = 2 * time.Second

// probeContainer names the one container of a probe pod (runProbePod).
const probeContainer = "probe"

// clientGoToolName names the Kubernetes client among the discovery report's
// embedded tools; its version is the k8s.io/client-go this binary was built
// with, which is the Kubernetes API generation it speaks.
const clientGoToolName = "client-go"

func clientGoToolVersion() string {
	return project.ModuleVersion("k8s.io/client-go")
}

// restartedAtAnnotation is the pod-template annotation `kubectl rollout
// restart` stamps to roll a Deployment.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// defaultContainerAnnotation names the container kubectl reads logs from when
// a pod has several and none is asked for.
const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// gvkCRD identifies a CustomResourceDefinition in a manifest batch.
var gvkCRD = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// The resources the lab reads and writes by GVR. Kinds outside the core and
// apps groups (a HelmRelease, an MCPServer, a ModelConfig) resolve through
// gvrFor from the resource name kubectl takes, so their versions are never
// pinned here.
var (
	gvrNamespaces  = corev1.SchemeGroupVersion.WithResource("namespaces")
	gvrSecrets     = corev1.SchemeGroupVersion.WithResource("secrets")
	gvrConfigMaps  = corev1.SchemeGroupVersion.WithResource("configmaps")
	gvrPods        = corev1.SchemeGroupVersion.WithResource("pods")
	gvrDeployments = appsv1.SchemeGroupVersion.WithResource("deployments")
	gvrCRDs        = gvkCRD.GroupVersion().WithResource("customresourcedefinitions")
)

// kubeClients is the client bundle behind every helper: one REST config for
// the lab cluster, the dynamic client (every object read and write, by GVR),
// the typed clientset (pods and their logs, the authorization and
// authentication reviews), the RESTMapper that turns kinds and resource
// names into GVRs and scopes, and a REST client for the non-resource paths
// (/readyz).
type kubeClients struct {
	cfg       *rest.Config
	dynamic   dynamic.Interface
	clientset kubernetes.Interface
	mapper    meta.RESTMapper
	raw       rest.Interface
	// resetMapper drops the cached discovery so the kinds registered since —
	// a CRD the lab just applied — resolve on the next lookup.
	resetMapper func()
}

var (
	labKubeMu    sync.Mutex
	labKubeCache *kubeClients
	// newLabKube builds the bundle from the lab kubeconfig; a test seam
	// (stubLabKube swaps in fakes).
	newLabKube = buildLabKube
	// clientsetFor builds the typed clientset for a config that is not the
	// lab admin's — a user's token (tokenConfig), an impersonation
	// (asUserConfig); a test seam.
	clientsetFor = func(cfg *rest.Config) (kubernetes.Interface, error) {
		return kubernetes.NewForConfig(cfg)
	}
)

// labKube is the cached client bundle for the lab cluster, built on first
// use from state/kubeconfig. useClusterKubeconfig drops it (resetLabKube)
// when it rewrites that file, so a bundle never outlives the kubeconfig it
// was built from.
func labKube() (*kubeClients, error) {
	labKubeMu.Lock()
	defer labKubeMu.Unlock()
	if labKubeCache != nil {
		return labKubeCache, nil
	}
	k, err := newLabKube()
	if err != nil {
		return nil, err
	}
	labKubeCache = k
	return k, nil
}

// resetLabKube forgets the cached bundle; the next helper rebuilds it from
// the kubeconfig on disk.
func resetLabKube() {
	labKubeMu.Lock()
	defer labKubeMu.Unlock()
	labKubeCache = nil
}

// buildLabKube is the real bundle: the REST config from labRESTClientGetter
// (the lab kubeconfig, QPS 50 / burst 100, the agentlab user agent), the
// apiserver's warnings deduplicated on stderr the way kubectl prints them,
// discovery cached in memory behind a deferred RESTMapper, and kubectl's
// short names (deploy, cm, crd) understood by the mapper.
func buildLabKube() (*kubeClients, error) {
	quietKlog(os.Getenv(helmDebugEnv) != "")
	cfg, err := labRESTClientGetter("").ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the lab kubeconfig: %w", err)
	}
	cfg.WarningHandler = rest.NewWarningWriter(os.Stderr, rest.WarningWriterOptions{Deduplicate: true})
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cached := memory.NewMemCacheClient(dc)
	deferred := restmapper.NewDeferredDiscoveryRESTMapper(cached)
	return &kubeClients{
		cfg:       cfg,
		dynamic:   dyn,
		clientset: cs,
		mapper:    restmapper.NewShortcutExpander(deferred, cached, func(string) {}),
		raw:       cs.Discovery().RESTClient(),
		resetMapper: func() {
			cached.Invalidate()
			deferred.Reset()
		},
	}, nil
}

// resource is the dynamic client for one GVR, in a namespace or at the
// cluster level ("" — a cluster-scoped kind, or every namespace for a list).
func (k *kubeClients) resource(gvr schema.GroupVersionResource, ns string) dynamic.ResourceInterface {
	if ns == "" {
		return k.dynamic.Resource(gvr)
	}
	return k.dynamic.Resource(gvr).Namespace(ns)
}

// describe words a resource for an error: "secrets agent-platform/dex-ca",
// "namespaces demo".
func describe(gvr schema.GroupVersionResource, ns, name string) string {
	if ns == "" {
		return gvr.Resource + " " + name
	}
	return gvr.Resource + " " + ns + "/" + name
}

// gvrFor resolves the resource argument kubectl takes — "secrets", "deploy",
// "crd", "helmreleases.helm.toolkit.fluxcd.io" — to the GVR the apiserver
// serves it under, preferred version first, through discovery. A fully
// qualified name is tried as resource.version.group before resource.group,
// as kubectl does, so a group with dots in it resolves. A miss resets the
// cached discovery once and retries (kubectl invalidates its cache the same
// way): the kinds a Helm install registers — a HelmRelease, an MCPServer, a
// Prometheus — are asked for after the cache was primed by an earlier call.
func gvrFor(resourceArg string) (schema.GroupVersionResource, error) {
	k, err := labKube()
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	return k.gvrFor(resourceArg)
}

func (k *kubeClients) gvrFor(resourceArg string) (schema.GroupVersionResource, error) {
	gvr, err := k.resourcesFor(resourceArg)
	if err != nil {
		k.resetMapper()
		gvr, err = k.resourcesFor(resourceArg)
	}
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("the apiserver serves no resource %q: %w", resourceArg, err)
	}
	return gvr, nil
}

// resourcesFor is one lookup of a resource argument against the mapper as it
// is cached now.
func (k *kubeClients) resourcesFor(resourceArg string) (schema.GroupVersionResource, error) {
	fully, gr := schema.ParseResourceArg(resourceArg)
	if fully != nil {
		if gvrs, err := k.mapper.ResourcesFor(*fully); err == nil && len(gvrs) > 0 {
			return gvrs[0], nil
		}
	}
	gvrs, err := k.mapper.ResourcesFor(gr.WithVersion(""))
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	return gvrs[0], nil
}

// restMapping resolves a kind to its resource and scope, resetting the cached
// discovery once on a miss (see gvrFor): a custom resource is applied after
// the chart that brought its CRD was installed, with the cache primed before.
func (k *kubeClients) restMapping(gk schema.GroupKind, version string) (*meta.RESTMapping, error) {
	mapping, err := k.mapper.RESTMapping(gk, version)
	if err != nil {
		k.resetMapper()
		mapping, err = k.mapper.RESTMapping(gk, version)
	}
	return mapping, err
}

// applyResult is what one server-side apply did: the object, and whether it
// was created or changed (its resourceVersion moved) — a re-apply that
// changes nothing is not a change.
type applyResult struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
	changed   bool
}

// String words the object the way kubectl's apply lines do:
// "deployment.apps/dex", "namespace/demo".
func (r applyResult) String() string {
	kind := strings.ToLower(r.gvk.Kind)
	if r.gvk.Group != "" {
		kind += "." + r.gvk.Group
	}
	return kind + "/" + r.name
}

// anyChanged reports whether any object of a batch was created or changed —
// `kubectl apply` output without an "unchanged" on every line.
func anyChanged(results []applyResult) bool {
	return slices.ContainsFunc(results, func(r applyResult) bool { return r.changed })
}

// applyManifests is `kubectl apply -f` of a multi-document manifest: every
// document server-side applied in order, empty documents skipped, a List
// expanded. A CustomResourceDefinition in the batch is applied and, before
// the first object after it, the apiserver is given up to crdEstablishTimeout
// to serve its kinds (with the mapper reset so it sees them) — so a manifest
// may carry a CRD and its custom resources together. Errors name the object.
func applyManifests(ctx context.Context, manifests []byte) ([]applyResult, error) {
	objs, err := decodeManifests(manifests)
	if err != nil {
		return nil, err
	}
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	var results []applyResult
	var pendingCRDs []*unstructured.Unstructured
	for _, obj := range objs {
		isCRD := obj.GroupVersionKind().GroupKind() == gvkCRD.GroupKind()
		if !isCRD && len(pendingCRDs) > 0 {
			if err := k.awaitCRDs(ctx, pendingCRDs); err != nil {
				return results, err
			}
			pendingCRDs = nil
		}
		res, err := k.apply(ctx, obj)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		if isCRD {
			pendingCRDs = append(pendingCRDs, obj)
		}
	}
	if len(pendingCRDs) > 0 {
		if err := k.awaitCRDs(ctx, pendingCRDs); err != nil {
			return results, err
		}
	}
	return results, nil
}

// decodeManifests splits a multi-document YAML (or JSON) manifest into
// objects, in order: empty documents (`---` with nothing, or comments only)
// are skipped, a List is expanded into its items, and a document without
// apiVersion and kind is refused by its position.
func decodeManifests(manifests []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bufio.NewReader(bytes.NewReader(manifests)), 4096)
	var objs []*unstructured.Unstructured
	for i := 1; ; i++ {
		var ext runtime.RawExtension
		err := dec.Decode(&ext)
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("manifest document %d: %w", i, err)
		}
		raw := bytes.TrimSpace(ext.Raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(raw); err != nil {
			return nil, fmt.Errorf("manifest document %d: %w", i, err)
		}
		if u.IsList() {
			err := u.EachListItem(func(o runtime.Object) error {
				item, ok := o.(*unstructured.Unstructured)
				if !ok {
					return fmt.Errorf("manifest document %d: List item of type %T", i, o)
				}
				objs = append(objs, item)
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		if u.GetKind() == "" || u.GetAPIVersion() == "" {
			return nil, fmt.Errorf("manifest document %d (%s) has no apiVersion/kind", i, u.GetName())
		}
		objs = append(objs, u)
	}
}

// apply server-side applies one object: its kind resolved to a resource and
// a scope through the mapper (a namespace-scoped object without a namespace
// goes to defaultNamespace, as kubectl's would), then a forced apply under
// applyFieldManager. The object is read first so the result can say whether
// the apply created or changed it.
func (k *kubeClients) apply(ctx context.Context, obj *unstructured.Unstructured) (applyResult, error) {
	gvk := obj.GroupVersionKind()
	res := applyResult{gvk: gvk, name: obj.GetName()}
	if res.name == "" {
		return res, fmt.Errorf("%s has no metadata.name", gvk.Kind)
	}
	mapping, err := k.restMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return res, fmt.Errorf("%s %s: %w (ensure the CRDs are installed first)", gvk.Kind, res.name, err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		res.namespace = obj.GetNamespace()
		if res.namespace == "" {
			res.namespace = defaultNamespace
			obj.SetNamespace(defaultNamespace)
		}
	}
	client := k.resource(mapping.Resource, res.namespace)
	before, err := client.Get(ctx, res.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		before = nil
	case err != nil:
		return res, fmt.Errorf("reading %s before applying it: %w", res, err)
	}
	body, err := json.Marshal(obj.Object)
	if err != nil {
		return res, fmt.Errorf("encoding %s: %w", res, err)
	}
	force := true
	after, err := client.Patch(ctx, res.name, types.ApplyPatchType, body, metav1.PatchOptions{
		FieldManager: applyFieldManager,
		Force:        &force,
	})
	if err != nil {
		return res, fmt.Errorf("applying %s: %w", res, err)
	}
	res.changed = before == nil || before.GetResourceVersion() != after.GetResourceVersion()
	return res, nil
}

// applyTyped server-side applies a typed object (a corev1.Namespace, a
// corev1.Secret) built in code: converted to its wire form with the
// apiVersion/kind it carries, minus the empty creationTimestamp and status
// the conversion emits, which are nobody's intent.
func applyTyped(ctx context.Context, obj runtime.Object) (applyResult, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return applyResult{}, err
	}
	k, err := labKube()
	if err != nil {
		return applyResult{}, err
	}
	return k.apply(ctx, u)
}

// toUnstructured is a typed object's wire form with the apiVersion/kind it
// carries, minus the empty creationTimestamp and status the conversion emits.
func toUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: raw}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	return u, nil
}

// awaitCRDs waits until the apiserver serves every kind the given
// CustomResourceDefinitions define (each served version resolving through
// the mapper, reset before every try), bounded by crdEstablishTimeout — the
// gap between a CRD being accepted and its endpoints existing, in which an
// apply of one of its objects fails with "no matches for kind".
func (k *kubeClients) awaitCRDs(ctx context.Context, crds []*unstructured.Unstructured) error {
	type served struct {
		gk       schema.GroupKind
		versions []string
	}
	var want []served
	for _, crd := range crds {
		group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		s := served{gk: schema.GroupKind{Group: group, Kind: kind}}
		for _, v := range versions {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if on, found, _ := unstructured.NestedBool(m, "served"); found && !on {
				continue
			}
			if name, _, _ := unstructured.NestedString(m, "name"); name != "" {
				s.versions = append(s.versions, name)
			}
		}
		want = append(want, s)
	}
	var missing string
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, crdEstablishTimeout, true, func(context.Context) (bool, error) {
		k.resetMapper()
		for _, s := range want {
			for _, v := range s.versions {
				if _, err := k.mapper.RESTMapping(s.gk, v); err != nil {
					missing = s.gk.String() + " " + v
					return false, nil
				}
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("the apiserver does not serve %s %s after its CustomResourceDefinition was applied", missing, crdEstablishTimeout)
	}
	return nil
}

// getObject is `kubectl get <resource> <name> -o json`: the object as it is,
// for the caller to read fields off with unstructured.Nested* and the
// condition helpers. A read that fails carries the apiserver's words (a
// NotFound stays recognisable through apierrors.IsNotFound).
func getObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	obj, err := k.resource(gvr, ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", describe(gvr, ns, name), err)
	}
	return obj, nil
}

// listObjects is `kubectl get <resource> [-l selector]` in a namespace, or
// across all of them with ns "".
func listObjects(ctx context.Context, gvr schema.GroupVersionResource, ns, labelSelector string) ([]unstructured.Unstructured, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	list, err := k.resource(gvr, ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", describe(gvr, ns, labelSelector), err)
	}
	return list.Items, nil
}

// objectExists reports whether the object is there: false without error for
// a NotFound, an error for anything else the read ran into.
func objectExists(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (bool, error) {
	_, err := getObject(ctx, gvr, ns, name)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// condition returns the status.conditions entry of the given type, or nil.
func condition(obj *unstructured.Unstructured, condType string) map[string]any {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t == condType {
			return m
		}
	}
	return nil
}

// conditionStatus is `{.status.conditions[?(@.type=="<type>")].status}` —
// "True", "False", "Unknown", or "" when the condition is not there yet.
func conditionStatus(obj *unstructured.Unstructured, condType string) string {
	if c := condition(obj, condType); c != nil {
		s, _, _ := unstructured.NestedString(c, "status")
		return s
	}
	return ""
}

// conditionMessage is `{.status.conditions[?(@.type=="<type>")].message}`.
func conditionMessage(obj *unstructured.Unstructured, condType string) string {
	if c := condition(obj, condType); c != nil {
		s, _, _ := unstructured.NestedString(c, "message")
		return s
	}
	return ""
}

// deleteObject is `kubectl delete <resource> <name> --ignore-not-found`: a
// missing object is success. With waitGone > 0 it is the CLI's default wait
// too — polling until the object is gone (finalizers run, a namespace's
// contents drained), failing once waitGone passes; 0 is `--wait=false`.
func deleteObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, waitGone time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	client := k.resource(gvr, ns)
	propagation := metav1.DeletePropagationBackground
	err = client.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("deleting %s: %w", describe(gvr, ns, name), err)
	}
	if waitGone <= 0 {
		return nil
	}
	err = wait.PollUntilContextTimeout(ctx, pollInterval, waitGone, true, func(ctx context.Context) (bool, error) {
		_, err := client.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("reading %s while waiting for its deletion: %w", describe(gvr, ns, name), err)
		}
		return false, nil
	})
	if wait.Interrupted(err) {
		return fmt.Errorf("%s is still there %s after its deletion was requested (a finalizer is holding it: `kubectl get %s -o yaml`)",
			describe(gvr, ns, name), waitGone, describe(gvr, ns, name))
	}
	return err
}

// patchObject is `kubectl patch <resource> <name> --type=<merge|json|strategic> -p <body>`.
func patchObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, pt types.PatchType, body []byte) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	if _, err := k.resource(gvr, ns).Patch(ctx, name, pt, body, metav1.PatchOptions{FieldManager: applyFieldManager}); err != nil {
		return fmt.Errorf("patching %s: %w", describe(gvr, ns, name), err)
	}
	return nil
}

// restartDeployment is `kubectl rollout restart deployment/<name>`: the
// restartedAt annotation stamped into the pod template, which rolls the
// pods. Pair it with waitDeploymentRolledOut to wait for the new ones.
func restartDeployment(ctx context.Context, ns, name string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		restartedAtAnnotation, time.Now().UTC().Format(time.RFC3339))
	return patchObject(ctx, gvrDeployments, ns, name, types.MergePatchType, []byte(patch))
}

// waitDeploymentRolledOut is `kubectl rollout status deployment/<name>
// --timeout=<timeout>`: kubectl's own condition (deploymentRolloutStatus),
// polled, its progress lines printed as they change, and its failure — the
// deadline, or the Deployment's own progress deadline exceeded — worded
// with what was last seen.
func waitDeploymentRolledOut(ctx context.Context, ns, name string, timeout time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	var last string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := k.resource(gvrDeployments, ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", describe(gvrDeployments, ns, name), err)
		}
		d := &appsv1.Deployment{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, d); err != nil {
			return false, fmt.Errorf("reading %s: %w", describe(gvrDeployments, ns, name), err)
		}
		msg, done, err := deploymentRolloutStatus(d)
		if err != nil {
			return false, err
		}
		if msg != last {
			note("%s", msg)
			last = msg
		}
		return done, nil
	})
	if wait.Interrupted(err) {
		return fmt.Errorf("%s did not roll out within %s (%s); check `kubectl -n %s get pods`",
			describe(gvrDeployments, ns, name), timeout, last, ns)
	}
	return err
}

// deploymentRolloutStatus is kubectl's rollout-status verdict on a
// Deployment (k8s.io/kubectl polymorphichelpers.DeploymentStatusViewer): a
// progress line and whether the rollout is complete — the controller has
// observed the current generation, every replica is updated, no old replica
// is left, every updated replica is available — or an error once the
// Deployment's progress deadline is exceeded.
func deploymentRolloutStatus(d *appsv1.Deployment) (string, bool, error) {
	if d.Generation > d.Status.ObservedGeneration {
		return "Waiting for deployment spec update to be observed...", false, nil
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return "", false, fmt.Errorf("deployment %q exceeded its progress deadline", d.Name)
		}
	}
	if d.Spec.Replicas != nil && d.Status.UpdatedReplicas < *d.Spec.Replicas {
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated...",
			d.Name, d.Status.UpdatedReplicas, *d.Spec.Replicas), false, nil
	}
	if d.Status.Replicas > d.Status.UpdatedReplicas {
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination...",
			d.Name, d.Status.Replicas-d.Status.UpdatedReplicas), false, nil
	}
	if d.Status.AvailableReplicas < d.Status.UpdatedReplicas {
		return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...",
			d.Name, d.Status.AvailableReplicas, d.Status.UpdatedReplicas), false, nil
	}
	return fmt.Sprintf("deployment %q successfully rolled out", d.Name), true, nil
}

// waitCondition is `kubectl wait --for=condition=<type>[=<status>]
// --timeout=<timeout> <resource>/<name>`: polled until the condition reads
// the wanted status ("True" for the CLI's bare form). A read that fails ends
// the wait with the apiserver's words; the deadline reports the last status
// and message seen.
func waitCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condType, condStatus string, timeout time.Duration) error {
	var lastStatus, lastMessage string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := getObject(ctx, gvr, ns, name)
		if err != nil {
			return false, err
		}
		lastStatus, lastMessage = conditionStatus(obj, condType), conditionMessage(obj, condType)
		return lastStatus == condStatus, nil
	})
	if wait.Interrupted(err) {
		return fmt.Errorf("%s never reached %s=%s within %s (last status %q: %s)",
			describe(gvr, ns, name), condType, condStatus, timeout, lastStatus, lastMessage)
	}
	return err
}

// tokenConfig is a REST config that authenticates to the lab apiserver with
// ONLY the given bearer token — the endpoint and CA from state/kubeconfig,
// no client certificate. Needed because the kind kubeconfig ships the admin
// client certificate, and a client cert always wins over a bearer token: a
// config that carried both would keep authenticating as kubernetes-admin
// (kubeconfig.go). What `--kubeconfig=<token kubeconfig>` did for kubectl.
func tokenConfig(token string) (*rest.Config, error) {
	kc, err := clientcmd.LoadFromFile(labKubeconfig())
	if err != nil {
		return nil, fmt.Errorf("loading the lab kubeconfig: %w", err)
	}
	var cluster *clientcmdapi.Cluster
	if ctx := kc.Contexts[kc.CurrentContext]; ctx != nil {
		cluster = kc.Clusters[ctx.Cluster]
	}
	if cluster == nil {
		for _, c := range kc.Clusters {
			cluster = c
			break
		}
	}
	if cluster == nil {
		return nil, fmt.Errorf("the lab kubeconfig %s names no cluster", labKubeconfig())
	}
	cfg := &rest.Config{
		Host: cluster.Server,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     cluster.CertificateAuthorityData,
			CAFile:     cluster.CertificateAuthority,
			ServerName: cluster.TLSServerName,
		},
		BearerToken: token,
	}
	return tuneRESTConfig(cfg), nil
}

// asUserConfig is the lab admin's REST config impersonating a principal —
// kubectl's `--as=<user>`; for a ServiceAccount the apiserver adds its groups
// itself, as it does for kubectl.
func asUserConfig(username string) (*rest.Config, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(k.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username}
	return cfg, nil
}

// canI is `kubectl auth can-i <verb> <resource> [-n <ns>]` as the identity
// cfg authenticates as: a SelfSubjectAccessReview. The resource argument is
// kubectl's ("pods", "deployments", "helmreleases.helm.toolkit.fluxcd.io")
// and resolves to its API group through discovery, as kubectl's does. ns ""
// is every namespace (`--all-namespaces`); kubectl without -n asks in the
// kubeconfig's namespace, defaultNamespace here.
func canI(ctx context.Context, cfg *rest.Config, verb, resourceArg, ns string) (bool, error) {
	gvr, err := gvrFor(resourceArg)
	if err != nil {
		return false, err
	}
	cs, err := clientsetFor(cfg)
	if err != nil {
		return false, err
	}
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: ns,
				Verb:      verb,
				Group:     gvr.Group,
				Resource:  gvr.Resource,
			},
		},
	}
	result, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("can-i %s %s: %w", verb, resourceArg, err)
	}
	return result.Status.Allowed, nil
}

// canIList is `kubectl auth can-i --list [-n <ns>]` as the identity cfg
// authenticates as: a SelfSubjectRulesReview, the rules as the apiserver
// returns them (ruleRows words them the way kubectl prints them).
func canIList(ctx context.Context, cfg *rest.Config, ns string) (*authorizationv1.SubjectRulesReviewStatus, error) {
	cs, err := clientsetFor(cfg)
	if err != nil {
		return nil, err
	}
	review := &authorizationv1.SelfSubjectRulesReview{
		Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: ns},
	}
	result, err := cs.AuthorizationV1().SelfSubjectRulesReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("can-i --list in %s: %w", ns, err)
	}
	return &result.Status, nil
}

// ruleRows words a rules review the way `kubectl auth can-i --list` prints
// its rows, one per resource: "<resource>[.<group>]  <non-resource URLs>
// <resource names>  <verbs>" — a resource rule starts with the resource
// (selfsubjectaccessreviews.authorization.k8s.io, pods), a non-resource
// rule with its URLs in brackets ([/healthz]).
func ruleRows(status *authorizationv1.SubjectRulesReviewStatus) []string {
	var rows []string
	for _, rule := range status.ResourceRules {
		groups := rule.APIGroups
		if len(groups) == 0 {
			groups = []string{""}
		}
		for _, group := range groups {
			for _, resource := range rule.Resources {
				name := resource
				if group != "" {
					name += "." + group
				}
				rows = append(rows, fmt.Sprintf("%s  []  %v  %v", name, rule.ResourceNames, rule.Verbs))
			}
		}
	}
	for _, rule := range status.NonResourceRules {
		rows = append(rows, fmt.Sprintf("%v  []  %v", rule.NonResourceURLs, rule.Verbs))
	}
	return rows
}

// whoAmI is `kubectl auth whoami` as the identity cfg authenticates as: the
// username and groups the apiserver attributes to it (a SelfSubjectReview) —
// the proof that a Dex token is accepted and what it maps to.
func whoAmI(ctx context.Context, cfg *rest.Config) (string, []string, error) {
	cs, err := clientsetFor(cfg)
	if err != nil {
		return "", nil, err
	}
	review, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("whoami: %w", err)
	}
	return review.Status.UserInfo.Username, review.Status.UserInfo.Groups, nil
}

// targetPods resolves a logs target to pods, sorted by name: "deploy/<name>"
// (or "deployment/<name>") through the Deployment's selector, "pod/<name>"
// that pod, anything else a label selector (`-l app=dex`).
func (k *kubeClients) targetPods(ctx context.Context, ns, target string) ([]corev1.Pod, error) {
	pods := k.clientset.CoreV1().Pods(ns)
	if name, ok := strings.CutPrefix(target, "pod/"); ok {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", describe(gvrPods, ns, name), err)
		}
		return []corev1.Pod{*pod}, nil
	}
	selector := target
	name, isDeploy := strings.CutPrefix(target, "deploy/")
	if !isDeploy {
		name, isDeploy = strings.CutPrefix(target, "deployment/")
	}
	if isDeploy {
		d, err := k.clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", describe(gvrDeployments, ns, name), err)
		}
		sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("%s has an unusable selector: %w", describe(gvrDeployments, ns, name), err)
		}
		selector = sel.String()
	}
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing pods in %s matching %q: %w", ns, selector, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("no pods in %s match %s", ns, target)
	}
	slices.SortFunc(list.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	return list.Items, nil
}

// logContainers picks the containers to read logs from: the named one when
// given (refused when the pod has no such container), else kubectl's default
// container when the pod annotates one, else all of the pod's containers.
func logContainers(pod *corev1.Pod, container string) ([]string, error) {
	var names []string
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	switch {
	case container != "":
		if !slices.Contains(names, container) {
			return nil, fmt.Errorf("pod %s has no container %q (containers: %s)", pod.Name, container, strings.Join(names, ", "))
		}
		return []string{container}, nil
	case len(names) > 1 && slices.Contains(names, pod.Annotations[defaultContainerAnnotation]):
		return []string{pod.Annotations[defaultContainerAnnotation]}, nil
	default:
		return names, nil
	}
}

// logStream is one container's log stream and the prefix its lines carry
// when several streams are interleaved.
type logStream struct {
	pod, container, prefix string
}

// logStreams lists the (pod, container) streams a target and container
// choice resolve to, prefixed with "[pod/container] " once there is more
// than one.
func (k *kubeClients) logStreams(ctx context.Context, ns, target, container string) ([]logStream, error) {
	pods, err := k.targetPods(ctx, ns, target)
	if err != nil {
		return nil, err
	}
	var streams []logStream
	for i := range pods {
		containers, err := logContainers(&pods[i], container)
		if err != nil {
			return nil, err
		}
		for _, c := range containers {
			streams = append(streams, logStream{pod: pods[i].Name, container: c})
		}
	}
	if len(streams) > 1 {
		for i := range streams {
			streams[i].prefix = "[" + streams[i].pod + "/" + streams[i].container + "] "
		}
	}
	return streams, nil
}

// copyLines writes r to w line by line, each line prefixed.
func copyLines(w io.Writer, r io.Reader, prefix string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if _, err := fmt.Fprintf(w, "%s%s\n", prefix, sc.Text()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// podLogs is `kubectl logs <target> [-c <container>] [--since=<since>]`: the
// logs of every pod the target resolves to (targetPods), of the given
// container or the pod's (logContainers), prefixed per stream when there are
// several, since the given duration ago when since > 0.
func podLogs(ctx context.Context, ns, target, container string, since time.Duration) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	streams, err := k.logStreams(ctx, ns, target, container)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, s := range streams {
		opts := &corev1.PodLogOptions{Container: s.container}
		if since > 0 {
			secs := int64(since.Seconds())
			opts.SinceSeconds = &secs
		}
		rc, err := k.clientset.CoreV1().Pods(ns).GetLogs(s.pod, opts).Stream(ctx)
		if err != nil {
			return b.String(), fmt.Errorf("logs of pod %s/%s (%s): %w", ns, s.pod, s.container, err)
		}
		err = copyLines(&b, rc, s.prefix)
		_ = rc.Close()
		if err != nil {
			return b.String(), err
		}
	}
	return b.String(), nil
}

// streamLogs is `kubectl logs -f <target>`: every stream the target resolves
// to followed concurrently into w, lines prefixed with the pod and container
// when there is more than one, until every stream ends or ctx is done.
func streamLogs(ctx context.Context, ns, target string, w io.Writer) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	streams, err := k.logStreams(ctx, ns, target, "")
	if err != nil {
		return err
	}
	var mu sync.Mutex // one writer at a time, so lines never interleave mid-way
	var wg sync.WaitGroup
	errs := make(chan error, len(streams))
	for _, s := range streams {
		wg.Go(func() {
			rc, err := k.clientset.CoreV1().Pods(ns).GetLogs(s.pod, &corev1.PodLogOptions{Container: s.container, Follow: true}).Stream(ctx)
			if err != nil {
				errs <- fmt.Errorf("logs of pod %s/%s (%s): %w", ns, s.pod, s.container, err)
				return
			}
			defer func() { _ = rc.Close() }()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				mu.Lock()
				_, err := fmt.Fprintf(w, "%s%s\n", s.prefix, sc.Text())
				mu.Unlock()
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	if ctx.Err() != nil {
		return nil
	}
	return <-errs
}

// podStateSummary words where a pod is for a probe's failure: its phase and
// the first container's waiting reason ("ImagePullBackOff: ...") or exit
// code.
func podStateSummary(pod *corev1.Pod) string {
	if pod == nil {
		return "never observed"
	}
	s := string(pod.Status.Phase)
	for _, c := range pod.Status.ContainerStatuses {
		switch {
		case c.State.Waiting != nil:
			s += ": " + c.State.Waiting.Reason
			if c.State.Waiting.Message != "" {
				s += ": " + c.State.Waiting.Message
			}
		case c.State.Terminated != nil:
			s += fmt.Sprintf(": exit code %d", c.State.Terminated.ExitCode)
			if c.State.Terminated.Reason != "" && c.State.Terminated.Reason != "Completed" && c.State.Terminated.Reason != "Error" {
				s += " (" + c.State.Terminated.Reason + ")"
			}
		}
	}
	return s
}

// runProbePod is `kubectl run <name> --image=<image> --restart=Never --rm -i
// --command -- <command...>`: a pod that runs the command once, waited for
// until it succeeds or fails (bounded by timeout), its combined output read
// from the container log, the pod deleted afterwards — whatever happened. A
// leftover of the same name from an interrupted run is removed first. The
// output is returned with the error too: a probe's diagnosis is in what it
// printed.
func runProbePod(ctx context.Context, ns, name, image string, command []string, timeout time.Duration) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	pods := k.clientset.CoreV1().Pods(ns)
	propagation := metav1.DeletePropagationBackground
	deleteOpts := metav1.DeleteOptions{PropagationPolicy: &propagation}
	if err := pods.Delete(ctx, name, deleteOpts); err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("removing the leftover probe pod %s/%s: %w", ns, name, err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := pods.Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}); err != nil {
		return "", fmt.Errorf("the leftover probe pod %s/%s is still there %s after its deletion was requested", ns, name, timeout)
	}
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": applyFieldManager}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            probeContainer,
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         command,
			}},
		},
	}
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{FieldManager: applyFieldManager}); err != nil {
		return "", fmt.Errorf("creating probe pod %s/%s: %w", ns, name, err)
	}
	defer func() {
		// Best effort, on a context of its own: the caller's may be done.
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pods.Delete(cleanup, name, deleteOpts)
	}()
	var last *corev1.Pod
	waitErr := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		last = p
		return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed, nil
	})
	out := ""
	if rc, err := pods.GetLogs(name, &corev1.PodLogOptions{Container: probeContainer}).Stream(ctx); err == nil {
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		out = string(raw)
	}
	switch {
	case waitErr != nil:
		return out, fmt.Errorf("probe pod %s/%s did not finish within %s (%s)", ns, name, timeout, podStateSummary(last))
	case last.Status.Phase == corev1.PodFailed:
		return out, fmt.Errorf("probe pod %s/%s failed (%s)", ns, name, podStateSummary(last))
	}
	return out, nil
}

// rawGet is `kubectl get --raw=<path>`: a non-resource path of the apiserver
// (/readyz, /version), as bytes.
func rawGet(ctx context.Context, path string) ([]byte, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	out, err := k.raw.Get().AbsPath(path).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return out, nil
}

// The lab's own idempotent writes of core objects, built in code rather than
// rendered — a namespace, a Secret from files, a TLS Secret — and the one
// read next to them. Server-side applied like everything else, so re-runs
// are clean no-ops.

// kindSecret is a Secret's kind, as a TypeMeta and a valueFrom reference spell it.
const kindSecret = "Secret"

// ensureNamespace idempotently creates a namespace.
func ensureNamespace(ns string) error {
	_, err := applyTyped(context.Background(), &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	})
	return err
}

// ensureSecretFromFiles idempotently applies a generic (Opaque) Secret whose
// data keys are the contents of the given files — `kubectl create secret
// generic --from-file=<key>=<path>`.
func ensureSecretFromFiles(ns, name string, files map[string]string) error {
	data := make(map[string][]byte, len(files))
	for key, path := range files {
		raw, err := os.ReadFile(path) // #nosec G304 -- lab-owned cert paths chosen by the caller
		if err != nil {
			return fmt.Errorf("secret %s/%s key %s: %w", ns, name, key, err)
		}
		data[key] = raw
	}
	return ensureSecret(ns, name, corev1.SecretTypeOpaque, data)
}

// ensureTLSSecret idempotently applies a kubernetes.io/tls Secret from a
// cert and key file — the type the Gateway API's certificateRefs require,
// which ensureSecretFromFiles's generic secrets are not.
func ensureTLSSecret(ns, name, certPath, keyPath string) error {
	cert, err := os.ReadFile(certPath) // #nosec G304 -- lab-owned cert paths chosen by the caller
	if err != nil {
		return fmt.Errorf("tls secret %s/%s: %w", ns, name, err)
	}
	key, err := os.ReadFile(keyPath) // #nosec G304 -- lab-owned cert paths chosen by the caller
	if err != nil {
		return fmt.Errorf("tls secret %s/%s: %w", ns, name, err)
	}
	return ensureSecret(ns, name, corev1.SecretTypeTLS, map[string][]byte{
		corev1.TLSCertKey:       cert,
		corev1.TLSPrivateKeyKey: key,
	})
}

func ensureSecret(ns, name string, secretType corev1.SecretType, data map[string][]byte) error {
	_, err := applyTyped(context.Background(), &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: kindSecret},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       secretType,
		Data:       data,
	})
	return err
}

// secretHasKey reports whether a secret exists and carries the given data key.
func secretHasKey(ns, name, key string) bool {
	secret, err := getObject(context.Background(), gvrSecrets, ns, name)
	if err != nil {
		return false
	}
	value, _, _ := unstructured.NestedString(secret.Object, "data", key)
	return value != ""
}
