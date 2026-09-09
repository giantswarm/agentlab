package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
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
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	restfake "k8s.io/client-go/rest/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The embedded client is tested against client-go's fakes: the dynamic fake
// for every object read and write, the typed fake for pods, logs and the
// reviews, a REST mapper built from a fixed list (the fake discovery serves
// none), and a fake REST client for the non-resource paths. No apiserver, no
// envtest. What the fakes cannot do — server-side apply's field management
// and resourceVersion bookkeeping — fakeApply stands in for, so the
// created/changed/unchanged verdict is exercised.

// Names the fixtures share; constants so the linter's literal count stays
// quiet.
const (
	testNS         = "demo"
	widgetGroup    = "example.com"
	widgetKind     = "Widget"
	fakeLogLine    = "fake logs"
	fakeKindServer = "https://127.0.0.1:34547"
	appLabel       = "app"
	fieldStatus    = "status"
	fieldType      = "type"
	condReady      = "Ready"
	condFalse      = "False"
	// The keys of an unstructured object the tests build by hand, and
	// helm-controller's message for a release it gave up on.
	fieldAPIVersion  = "apiVersion"
	fieldKind        = "kind"
	fieldMetadata    = "metadata"
	fieldMessage     = "message"
	fieldNamespace   = "namespace"
	retriesExhausted = "install retries exhausted"
	verbGet          = "get"
	verbList         = "list"
	verbCreate       = "create"
)

var (
	widgetGVK = schema.GroupVersionKind{Group: widgetGroup, Version: "v1", Kind: widgetKind}
	widgetGVR = schema.GroupVersionResource{Group: widgetGroup, Version: "v1", Resource: "widgets"}
	// The custom kinds the lab reads through gvrFor, as the fake apiserver
	// serves them: Flux's HelmRelease, muster's MCPServer, the operator's
	// Prometheus.
	fluxHelmReleaseGVK = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	fluxHelmReleaseGVR = fluxHelmReleaseGVK.GroupVersion().WithResource("helmreleases")
	musterMCPServerGVK = schema.GroupVersionKind{Group: "muster.giantswarm.io", Version: "v1alpha1", Kind: "MCPServer"}
	musterMCPServerGVR = musterMCPServerGVK.GroupVersion().WithResource("mcpservers")
	prometheusGVK      = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "Prometheus"}
	prometheusGVR      = prometheusGVK.GroupVersion().WithResource("prometheuses")
	serviceMonitorGVK  = prometheusGVK.GroupVersion().WithKind("ServiceMonitor")
	serviceMonitorGVR  = prometheusGVK.GroupVersion().WithResource("servicemonitors")
)

// fakeLab is a kubeClients bundle on fakes, installed as the lab's client for
// one test, with the knobs the tests turn: how often the mapper was reset,
// how often /readyz was asked, and a hook run on every mapper reset (the
// CRD-wait test registers its kind there).
type fakeLab struct {
	*kubeClients
	dyn     *dynamicfake.FakeDynamicClient
	cs      *kubefake.Clientset
	mapper  *meta.DefaultRESTMapper
	resets  int
	readyz  int
	onReset func(f *fakeLab)
}

// stubLabKube makes k the lab's client bundle for the test — whatever
// useClusterKubeconfig resets in between, the next labKube() rebuilds it
// from this seam.
func stubLabKube(t *testing.T, k *kubeClients) {
	t.Helper()
	prev := newLabKube
	newLabKube = func() (*kubeClients, error) { return k, nil }
	resetLabKube()
	t.Cleanup(func() {
		newLabKube = prev
		resetLabKube()
	})
}

// newFakeLab builds the fake bundle, seeded with objects (typed or
// unstructured; they land in the dynamic fake) and installs it.
func newFakeLab(t *testing.T, objects ...runtime.Object) *fakeLab {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	// Seeds land in the tracker as they are given, so typed ones are turned
	// into what the apiserver hands the dynamic client: unstructured, with
	// their apiVersion/kind.
	seeds := make([]runtime.Object, 0, len(objects))
	for _, obj := range objects {
		if _, ok := obj.(*unstructured.Unstructured); ok {
			seeds = append(seeds, obj)
			continue
		}
		gvks, _, err := sch.ObjectKinds(obj)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: raw}
		u.SetGroupVersionKind(gvks[0])
		seeds = append(seeds, u)
	}
	// The dynamic fake wants a scheme of unstructured kinds (a typed List
	// type would refuse the unstructured items); the typed scheme above only
	// names the seeds' kinds.
	usch := runtime.NewScheme()
	for gvk := range sch.AllKnownTypes() {
		if strings.HasSuffix(gvk.Kind, "List") {
			usch.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
		} else {
			usch.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(usch, map[schema.GroupVersionResource]string{
		gvrCRDs:            "CustomResourceDefinitionList",
		widgetGVR:          "WidgetList",
		fluxHelmReleaseGVR: "HelmReleaseList",
		musterMCPServerGVR: "MCPServerList",
		prometheusGVR:      "PrometheusList",
		serviceMonitorGVR:  "ServiceMonitorList",
		gvrModelConfigs:    "ModelConfigList",
		gvrAgents:          "AgentList",
	}, seeds...)
	dyn.PrependReactor("patch", "*", fakeApply(dyn.Tracker()))

	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("Namespace"),
		corev1.SchemeGroupVersion.WithKind("Node"),
		gvkCRD,
	} {
		mapper.Add(gvk, meta.RESTScopeRoot)
	}
	for _, gvk := range []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("Secret"),
		corev1.SchemeGroupVersion.WithKind("ConfigMap"),
		corev1.SchemeGroupVersion.WithKind("Pod"),
		appsv1.SchemeGroupVersion.WithKind(kindDeployment),
		fluxHelmReleaseGVK,
		musterMCPServerGVK,
		prometheusGVK,
		serviceMonitorGVK,
		// kagent's ModelConfig and Agent, which the proofs read and write
		// (crds_test.go).
		gvkModelConfig,
		gvkAgent,
	} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}

	f := &fakeLab{dyn: dyn, cs: kubefake.NewClientset(), mapper: mapper}
	f.kubeClients = &kubeClients{
		cfg:       &rest.Config{Host: fakeKindServer, TLSClientConfig: rest.TLSClientConfig{CertData: []byte("admin")}},
		dynamic:   dyn,
		clientset: f.cs,
		mapper:    mapper,
		raw: &restfake.RESTClient{
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
			Client: restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/readyz" {
					f.readyz++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
				}
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("404 page not found"))}, nil
			}),
		},
		resetMapper: func() {
			f.resets++
			if f.onReset != nil {
				f.onReset(f)
			}
		},
	}
	stubLabKube(t, f.kubeClients)
	return f
}

// fakeApply stands in for the apiserver's server-side apply on the dynamic
// fake, whose tracker keeps no resourceVersion: a missing object is created
// at resourceVersion 1; an existing one is replaced by what was applied and
// gets a new resourceVersion only when that differs from what is stored. It
// also insists on the lab's field manager and Force — the contract every
// apply of the lab must meet.
func fakeApply(tracker clienttesting.ObjectTracker) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		patch, ok := action.(clienttesting.PatchActionImpl)
		if !ok || patch.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		opts := patch.PatchOptions
		if opts.FieldManager != applyFieldManager || opts.Force == nil || !*opts.Force {
			return true, nil, fmt.Errorf("apply without the lab's forced field manager: %+v", opts)
		}
		applied := &unstructured.Unstructured{}
		if err := applied.UnmarshalJSON(patch.GetPatch()); err != nil {
			return true, nil, err
		}
		gvr, ns, name := patch.GetResource(), patch.GetNamespace(), patch.GetName()
		existing, err := tracker.Get(gvr, ns, name)
		switch {
		case apierrors.IsNotFound(err):
			applied.SetResourceVersion("1")
			if err := tracker.Create(gvr, applied, ns); err != nil {
				return true, nil, err
			}
		case err != nil:
			return true, nil, err
		default:
			current, ok := existing.(*unstructured.Unstructured)
			if !ok {
				return true, nil, fmt.Errorf("tracker holds a %T", existing)
			}
			applied.SetResourceVersion(current.GetResourceVersion())
			if !reflect.DeepEqual(applied.Object, current.Object) {
				rv, _ := strconv.Atoi(current.GetResourceVersion())
				applied.SetResourceVersion(strconv.Itoa(rv + 1))
			}
			if err := tracker.Update(gvr, applied, ns); err != nil {
				return true, nil, err
			}
		}
		obj, err := tracker.Get(gvr, ns, name)
		return true, obj, err
	}
}

// stored reads an object back from the dynamic fake.
func (f *fakeLab) stored(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := f.dyn.Tracker().Get(gvr, ns, name)
	if err != nil {
		t.Fatalf("%s: %v", describe(gvr, ns, name), err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("%s: stored as %T", describe(gvr, ns, name), obj)
	}
	return u
}

const (
	nsManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: demo
`
	opaqueManifest = `apiVersion: v1
kind: Secret
metadata:
  name: creds
type: Opaque
data:
  greeting: aGk=
`
)

func deployManifest(image string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: dex
  namespace: demo
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: dex
          image: ` + image + "\n"
}

// TestDecodeManifests: a multi-document manifest splits in order; an empty
// document (a bare separator), a comments-only one and a leading separator
// are skipped; a List expands into its items; a document without a kind is
// refused by its position.
func TestDecodeManifests(t *testing.T) {
	manifest := "---\n" + nsManifest + "---\n\n---\n# only a comment\n---\n" + opaqueManifest + "---\n" + `apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: a
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: b
`
	objs, err := decodeManifests([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.GetKind()+"/"+o.GetName())
	}
	want := []string{"Namespace/demo", "Secret/creds", "ConfigMap/a", "ConfigMap/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded %v, want %v", got, want)
	}
	if objs[0].GetName() != testNS || objs[1].GetName() != "creds" {
		t.Errorf("objects lost their names: %v", got)
	}

	if _, err := decodeManifests([]byte(nsManifest + "---\nmetadata:\n  name: nokind\n")); err == nil || !strings.Contains(err.Error(), "document 2") {
		t.Errorf("a document without a kind must be refused by position, got %v", err)
	}
	if objs, err := decodeManifests(nil); err != nil || len(objs) != 0 {
		t.Errorf("nothing decodes to nothing, got %v %v", objs, err)
	}
}

// TestApplyManifestsChanged: the first apply creates every object (a
// namespace-scoped one without a namespace lands in default), a re-apply of
// the same manifest changes nothing, and a manifest that differs in one
// object changes that one only. Every write goes out as a forced apply
// under the lab's field manager — fakeApply refuses anything else.
func TestApplyManifestsChanged(t *testing.T) {
	f := newFakeLab(t)
	ctx := context.Background()
	manifest := nsManifest + "---\n" + opaqueManifest + "---\n" + deployManifest("dex:1")

	results, err := applyManifests(ctx, []byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range results {
		got = append(got, fmt.Sprintf("%s ns=%s changed=%v", r, r.namespace, r.changed))
	}
	want := []string{"namespace/demo ns= changed=true", "secret/creds ns=default changed=true", "deployment.apps/dex ns=demo changed=true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("first apply = %v, want %v", got, want)
	}
	if !anyChanged(results) {
		t.Error("anyChanged must report the creations")
	}
	if secret := f.stored(t, gvrSecrets, defaultNamespace, "creds"); secret.GetNamespace() != defaultNamespace {
		t.Errorf("the namespace-less secret landed in %q, want %s", secret.GetNamespace(), defaultNamespace)
	}

	results, err = applyManifests(ctx, []byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if anyChanged(results) {
		t.Errorf("a re-apply of the same manifest reported a change: %+v", results)
	}

	results, err = applyManifests(ctx, []byte(manifest[:len(manifest)-len("dex:1\n")]+"dex:2\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if want := r.gvk.Kind == kindDeployment; r.changed != want {
			t.Errorf("%s changed=%v, want %v", r, r.changed, want)
		}
	}
	if img, _, _ := unstructured.NestedString(f.stored(t, gvrDeployments, testNS, componentDex).Object, "spec", "template", "spec", "containers"); img != "" {
		t.Errorf("containers is a list, not a string: %q", img)
	}
}

// TestApplyManifestsCRDThenCR: a batch that carries a CRD and one of its
// objects applies both — after the CRD the mapper is reset until the new
// kind resolves (here: on the second reset), and only then is the object
// applied. A kind no CRD introduced fails naming it and the fix.
func TestApplyManifestsCRDThenCR(t *testing.T) {
	f := newFakeLab(t)
	f.onReset = func(f *fakeLab) {
		if f.resets >= 2 {
			f.mapper.Add(widgetGVK, meta.RESTScopeNamespace)
		}
	}
	crd := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
    - name: v1alpha1
      served: false
      storage: false
`
	widget := `apiVersion: example.com/v1
kind: Widget
metadata:
  name: w
  namespace: demo
spec:
  size: 3
`
	results, err := applyManifests(context.Background(), []byte(crd+"---\n"+widget))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].gvk != gvkCRD || results[1].gvk != widgetGVK {
		t.Fatalf("results = %+v", results)
	}
	if f.resets != 2 {
		t.Errorf("mapper reset %d times, want 2 (the miss, then the hit)", f.resets)
	}
	if size, _, _ := unstructured.NestedInt64(f.stored(t, widgetGVR, testNS, "w").Object, "spec", "size"); size != 3 {
		t.Errorf("widget spec.size = %d, want 3 (integers must survive the decode)", size)
	}

	f2 := newFakeLab(t)
	_, err = applyManifests(context.Background(), []byte(widget))
	if err == nil {
		t.Fatal("an unknown kind must fail the apply")
	}
	for _, want := range []string{"Widget w", "no matches for kind", "ensure the CRDs are installed first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-kind error %q lacks %q", err, want)
		}
	}
	if f2.resets != 1 {
		t.Errorf("the mapper was reset %d times for an unknown kind, want 1 (the miss's one retry; no CRD in the batch to wait for)", f2.resets)
	}
}

// TestMapperMissResetsOnce: a resource or kind the cached discovery does not
// know — the CRD arrived after the cache was primed, as the kinds a Helm
// install brings do — resets the cache once and resolves on the retry, for
// gvrFor and for an apply alike; a kind nobody serves costs one reset and
// fails as before.
func TestMapperMissResetsOnce(t *testing.T) {
	f := newFakeLab(t)
	f.onReset = func(f *fakeLab) { f.mapper.Add(widgetGVK, meta.RESTScopeNamespace) }
	gvr, err := gvrFor("widgets.example.com")
	if err != nil || gvr != widgetGVR {
		t.Fatalf("gvrFor after the CRD arrived = %v, %v; want %v", gvr, err, widgetGVR)
	}
	if f.resets != 1 {
		t.Errorf("mapper reset %d times, want 1 (the miss)", f.resets)
	}
	if _, err := gvrFor("widgets.example.com"); err != nil || f.resets != 1 {
		t.Errorf("a known resource must resolve from the cache: %v, resets %d", err, f.resets)
	}

	f2 := newFakeLab(t)
	f2.onReset = func(f *fakeLab) { f.mapper.Add(widgetGVK, meta.RESTScopeNamespace) }
	results, err := applyManifests(context.Background(), []byte(`apiVersion: example.com/v1
kind: Widget
metadata:
  name: w
  namespace: demo
`))
	if err != nil || len(results) != 1 || !results[0].changed {
		t.Fatalf("applying a kind the cache learns on reset: %+v, %v", results, err)
	}
	if f2.resets != 1 {
		t.Errorf("mapper reset %d times for the apply, want 1", f2.resets)
	}

	f3 := newFakeLab(t)
	if _, err := gvrFor("gadgets.example.com"); err == nil || !strings.Contains(err.Error(), `serves no resource "gadgets.example.com"`) {
		t.Errorf("an unserved resource: %v", err)
	}
	if f3.resets != 1 {
		t.Errorf("mapper reset %d times for an unserved resource, want 1", f3.resets)
	}
}

// TestEnsureHelpers: the lab's own core objects — a namespace, a generic
// secret from files, a TLS secret — are applied typed, without the empty
// creationTimestamp/status the conversion emits, and secretHasKey reads the
// result.
func TestEnsureHelpers(t *testing.T) {
	f := newFakeLab(t)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	for path, content := range map[string]string{certPath: "CERT", keyPath: "KEY"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := ensureNamespace(testNS); err != nil {
		t.Fatal(err)
	}
	ns := f.stored(t, gvrNamespaces, "", testNS)
	if _, has := ns.Object[fieldStatus]; has {
		t.Errorf("namespace applied with a status: %v", ns.Object)
	}
	if _, has, _ := unstructured.NestedFieldNoCopy(ns.Object, "metadata", "creationTimestamp"); has {
		t.Errorf("namespace applied with a creationTimestamp: %v", ns.Object)
	}
	if err := ensureNamespace(testNS); err != nil {
		t.Fatalf("re-ensuring the namespace: %v", err)
	}

	if err := ensureSecretFromFiles(testNS, "dex-ca", map[string]string{"ca.crt": certPath}); err != nil {
		t.Fatal(err)
	}
	ca := f.stored(t, gvrSecrets, testNS, "dex-ca")
	if typ, _, _ := unstructured.NestedString(ca.Object, "type"); typ != string(corev1.SecretTypeOpaque) {
		t.Errorf("generic secret type = %q", typ)
	}
	if data, _, _ := unstructured.NestedString(ca.Object, "data", "ca.crt"); data != "Q0VSVA==" {
		t.Errorf("data.ca.crt = %q, want the file base64-encoded", data)
	}
	if err := ensureSecretFromFiles(testNS, "dex-ca", map[string]string{"ca.crt": filepath.Join(dir, "missing")}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a missing file must fail by name, got %v", err)
	}

	if err := ensureTLSSecret(testNS, "edge", certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	tls := f.stored(t, gvrSecrets, testNS, "edge")
	if typ, _, _ := unstructured.NestedString(tls.Object, "type"); typ != string(corev1.SecretTypeTLS) {
		t.Errorf("tls secret type = %q, want %s", typ, corev1.SecretTypeTLS)
	}
	if key, _, _ := unstructured.NestedString(tls.Object, "data", corev1.TLSPrivateKeyKey); key != "S0VZ" {
		t.Errorf("data.tls.key = %q", key)
	}

	if !secretHasKey(testNS, "edge", corev1.TLSCertKey) {
		t.Error("secretHasKey misses tls.crt")
	}
	if secretHasKey(testNS, "edge", "nope") || secretHasKey(testNS, "absent", corev1.TLSCertKey) {
		t.Error("secretHasKey reports a key or a secret that is not there")
	}
}

// TestGetListExists: reads through the dynamic client by GVR, the NotFound
// kept recognisable, a list filtered by label.
func TestGetListExists(t *testing.T) {
	newFakeLab(t,
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNS, Labels: map[string]string{appLabel: "x"}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: testNS}},
	)
	ctx := context.Background()
	obj, err := getObject(ctx, gvrConfigMaps, testNS, "a")
	if err != nil || obj.GetName() != "a" {
		t.Fatalf("getObject = %v, %v", obj, err)
	}
	_, err = getObject(ctx, gvrConfigMaps, testNS, "zzz")
	if err == nil || !apierrors.IsNotFound(err) || !strings.Contains(err.Error(), "configmaps demo/zzz") {
		t.Errorf("missing object: err = %v, want a NotFound naming it", err)
	}
	for name, want := range map[string]bool{"a": true, "zzz": false} {
		if exists, err := objectExists(ctx, gvrConfigMaps, testNS, name); err != nil || exists != want {
			t.Errorf("objectExists(%s) = %v, %v; want %v", name, exists, err, want)
		}
	}
	items, err := listObjects(ctx, gvrConfigMaps, testNS, appLabel+"=x")
	if err != nil || len(items) != 1 || items[0].GetName() != "a" {
		t.Errorf("listObjects(app=x) = %v, %v", items, err)
	}
	items, err = listObjects(ctx, gvrConfigMaps, "", "")
	if err != nil || len(items) != 2 {
		t.Errorf("listObjects(all) = %d items, %v", len(items), err)
	}
}

// TestDeleteObjectWait: deleting what is not there is success; a delete that
// waits returns once the object is gone; one that never goes fails after
// the bound, naming the object.
func TestDeleteObjectWait(t *testing.T) {
	f := newFakeLab(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: testNS}})
	ctx := context.Background()
	if err := deleteObject(ctx, gvrConfigMaps, testNS, "nope", time.Second); err != nil {
		t.Errorf("deleting a missing object: %v, want success", err)
	}
	if err := deleteObject(ctx, gvrConfigMaps, testNS, "a", time.Second); err != nil {
		t.Errorf("deleting with a wait: %v", err)
	}
	if _, err := f.dyn.Tracker().Get(gvrConfigMaps, testNS, "a"); !apierrors.IsNotFound(err) {
		t.Errorf("object still stored after the delete: %v", err)
	}

	// A finalizer that never lets go: the delete is accepted, the object stays.
	held := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "held", Namespace: testNS}}
	f2 := newFakeLab(t, held)
	f2.dyn.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	err := deleteObject(ctx, gvrConfigMaps, testNS, "held", 20*time.Millisecond)
	if err == nil {
		t.Fatal("an object that never goes must fail the wait")
	}
	for _, want := range []string{"configmaps demo/held", "still there", "finalizer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("wait error %q lacks %q", err, want)
		}
	}
	if err := deleteObject(ctx, gvrConfigMaps, testNS, "held", 0); err != nil {
		t.Errorf("--wait=false must not wait: %v", err)
	}
}

// TestDeploymentRolloutStatus is kubectl's rollout-status verdict, case by
// case.
func TestDeploymentRolloutStatus(t *testing.T) {
	one := int32(1)
	d := func(gen, observed int64, replicas, updated, available int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: componentDex, Generation: gen},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: observed, Replicas: replicas, UpdatedReplicas: updated, AvailableReplicas: available},
		}
	}
	cases := []struct {
		name string
		d    *appsv1.Deployment
		msg  string
		done bool
	}{
		{"spec not observed", d(2, 1, 1, 1, 1), "spec update to be observed", false},
		{"new replica not updated", d(1, 1, 1, 0, 0), "0 out of 1 new replicas have been updated", false},
		{"old replica pending", d(1, 1, 2, 1, 1), "1 old replicas are pending termination", false},
		{"updated not available", d(1, 1, 1, 1, 0), "0 of 1 updated replicas are available", false},
		{"rolled out", d(1, 1, 1, 1, 1), `deployment "dex" successfully rolled out`, true},
	}
	for _, c := range cases {
		msg, done, err := deploymentRolloutStatus(c.d)
		if err != nil || done != c.done || !strings.Contains(msg, c.msg) {
			t.Errorf("%s: (%q, %v, %v), want %q done=%v", c.name, msg, done, err, c.msg, c.done)
		}
	}
	stuck := d(1, 1, 1, 0, 0)
	stuck.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded"}}
	if _, _, err := deploymentRolloutStatus(stuck); err == nil || !strings.Contains(err.Error(), "exceeded its progress deadline") {
		t.Errorf("progress deadline: err = %v", err)
	}
}

// TestWaitDeploymentRolledOut reads the Deployment through the dynamic
// client: complete at once, a deadline with the last progress line, a
// missing Deployment as the apiserver's NotFound.
func TestWaitDeploymentRolledOut(t *testing.T) {
	one := int32(1)
	done := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{Name: componentDex, Namespace: testNS, Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	pending := done.DeepCopy()
	pending.Name = "slow"
	pending.Status.AvailableReplicas = 0
	newFakeLab(t, done, pending)
	ctx := context.Background()
	if err := waitDeploymentRolledOut(ctx, testNS, componentDex, time.Second); err != nil {
		t.Errorf("a rolled-out deployment: %v", err)
	}
	err := waitDeploymentRolledOut(ctx, testNS, "slow", 20*time.Millisecond)
	if err == nil {
		t.Fatal("a pending rollout must hit the deadline")
	}
	for _, want := range []string{"deployments demo/slow", "did not roll out within", "0 of 1 updated replicas are available", "kubectl -n demo get pods"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("deadline error %q lacks %q", err, want)
		}
	}
	if err := waitDeploymentRolledOut(ctx, testNS, "absent", time.Second); err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("a missing deployment: %v, want the apiserver's NotFound", err)
	}
}

// TestRestartDeployment stamps kubectl's restartedAt annotation into the pod
// template.
func TestRestartDeployment(t *testing.T) {
	f := newFakeLab(t, &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{Name: componentDex, Namespace: testNS},
	})
	if err := restartDeployment(context.Background(), testNS, componentDex); err != nil {
		t.Fatal(err)
	}
	stamp, _, _ := unstructured.NestedString(f.stored(t, gvrDeployments, testNS, componentDex).Object, "spec", "template", "metadata", "annotations", restartedAtAnnotation)
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("restartedAt = %q: %v", stamp, err)
	}
}

// TestConditions: the Ready condition's status and message off an object, ""
// when the condition is not there.
func TestConditions(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		fieldStatus: map[string]any{"conditions": []any{
			map[string]any{fieldType: "Reconciling", fieldStatus: "Unknown"},
			map[string]any{fieldType: condReady, fieldStatus: condFalse, fieldMessage: retriesExhausted},
		}},
	}}
	if s := conditionStatus(obj, condReady); s != condFalse {
		t.Errorf("Ready status = %q", s)
	}
	if m := conditionMessage(obj, condReady); m != retriesExhausted {
		t.Errorf("Ready message = %q", m)
	}
	if s, m := conditionStatus(obj, "Healthy"), conditionMessage(obj, "Healthy"); s != "" || m != "" {
		t.Errorf("absent condition = %q %q, want empty", s, m)
	}
	if s := conditionStatus(&unstructured.Unstructured{Object: map[string]any{}}, condReady); s != "" {
		t.Errorf("no status at all = %q", s)
	}
}

// TestWaitCondition: met at once; the deadline names the last status and
// message; a missing object ends the wait with the apiserver's words.
func TestWaitCondition(t *testing.T) {
	ready := &unstructured.Unstructured{}
	ready.SetAPIVersion("v1")
	ready.SetKind("ConfigMap")
	ready.SetNamespace(testNS)
	ready.SetName("ok")
	_ = unstructured.SetNestedSlice(ready.Object, []any{map[string]any{fieldType: condReady, fieldStatus: "True"}}, fieldStatus, "conditions")
	notReady := ready.DeepCopy()
	notReady.SetName("nope")
	_ = unstructured.SetNestedSlice(notReady.Object, []any{map[string]any{fieldType: condReady, fieldStatus: condFalse, fieldMessage: "waiting on the model"}}, fieldStatus, "conditions")
	newFakeLab(t, ready, notReady)
	ctx := context.Background()
	if err := waitCondition(ctx, gvrConfigMaps, testNS, "ok", condReady, "True", time.Second); err != nil {
		t.Errorf("met condition: %v", err)
	}
	err := waitCondition(ctx, gvrConfigMaps, testNS, "nope", condReady, "True", 20*time.Millisecond)
	if err == nil {
		t.Fatal("an unmet condition must hit the deadline")
	}
	for _, want := range []string{"configmaps demo/nope", "never reached Ready=True", `"False"`, "waiting on the model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("deadline error %q lacks %q", err, want)
		}
	}
	if err := waitCondition(ctx, gvrConfigMaps, testNS, "absent", condReady, "True", time.Second); err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("a missing object: %v, want the apiserver's NotFound", err)
	}
}

// TestGvrFor resolves kubectl's resource arguments through the mapper:
// the API group filled in, a fully qualified name honoured, an unknown one
// refused by name.
func TestGvrFor(t *testing.T) {
	newFakeLab(t)
	for arg, want := range map[string]schema.GroupVersionResource{
		gvrDeployments.Resource: gvrDeployments,
		gvrSecrets.Resource:     gvrSecrets,
		"secret":                gvrSecrets,
		gvrDeployments.Resource + "." + gvrDeployments.Group: gvrDeployments,
		fluxHelmReleaseResource:                              fluxHelmReleaseGVR,
		musterMCPServerResource:                              musterMCPServerGVR,
		modelConfigResource:                                  gvrModelConfigs,
	} {
		if got, err := gvrFor(arg); err != nil || got != want {
			t.Errorf("gvrFor(%q) = %v, %v; want %v", arg, got, err, want)
		}
	}
	if _, err := gvrFor("kustomizations.kustomize.toolkit.fluxcd.io"); err == nil || !strings.Contains(err.Error(), "kustomizations.kustomize.toolkit.fluxcd.io") {
		t.Errorf("an unserved resource must fail by name, got %v", err)
	}
}

// TestCanI: `auth can-i` becomes a SelfSubjectAccessReview as the given
// identity — the verb, the resource with its API group resolved through the
// mapper (deployments -> apps), the namespace ("" for every namespace) —
// and the answer is the apiserver's.
func TestCanI(t *testing.T) {
	newFakeLab(t)
	cs := kubefake.NewClientset()
	var attrs []*authorizationv1.ResourceAttributes
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review, ok := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if !ok {
			return true, nil, fmt.Errorf("created a %T", action.(clienttesting.CreateAction).GetObject())
		}
		attrs = append(attrs, review.Spec.ResourceAttributes)
		review.Status.Allowed = review.Spec.ResourceAttributes.Verb != verbCreate
		return true, review, nil
	})
	seen := stubClientsetFor(t, cs)
	cfg := &rest.Config{Host: "https://127.0.0.1:34547", BearerToken: "tok"}
	ctx := context.Background()

	allowed, err := canI(ctx, cfg, verbCreate, gvrDeployments.Resource, "kube-system")
	if err != nil || allowed {
		t.Errorf("create deployments: %v, %v; want no", allowed, err)
	}
	allowed, err = canI(ctx, cfg, verbList, gvrPods.Resource, "")
	if err != nil || !allowed {
		t.Errorf("list pods --all-namespaces: %v, %v; want yes", allowed, err)
	}
	if _, err := canI(ctx, cfg, verbGet, "unicorns", ""); err == nil {
		t.Error("a resource the apiserver does not serve must fail the check")
	}
	want := []*authorizationv1.ResourceAttributes{
		{Namespace: "kube-system", Verb: verbCreate, Group: gvrDeployments.Group, Resource: gvrDeployments.Resource},
		{Namespace: "", Verb: verbList, Group: "", Resource: gvrPods.Resource},
	}
	if !reflect.DeepEqual(attrs, want) {
		t.Errorf("reviews sent = %+v, want %+v", attrs, want)
	}
	if len(*seen) != 2 || (*seen)[0] != cfg {
		t.Errorf("the review client was not built from the caller's config: %v", *seen)
	}
}

// stubClientsetFor makes cs the typed clientset every identity-bound helper
// (canI, canIList, whoAmI) builds, recording the configs it was asked for.
func stubClientsetFor(t *testing.T, cs *kubefake.Clientset) *[]*rest.Config {
	t.Helper()
	var seen []*rest.Config
	prev := clientsetFor
	clientsetFor = func(cfg *rest.Config) (kubernetes.Interface, error) {
		seen = append(seen, cfg)
		return cs, nil
	}
	t.Cleanup(func() { clientsetFor = prev })
	return &seen
}

// TestCanIListRows: `auth can-i --list` becomes a SelfSubjectRulesReview in
// the namespace, and its rows read like kubectl's — a resource rule by
// resource.group, a non-resource rule by its URLs in brackets — so the
// proof that skips selfsubject* and [...] rows works on them.
func TestCanIListRows(t *testing.T) {
	cs := kubefake.NewClientset()
	var namespaces []string
	cs.PrependReactor("create", "selfsubjectrulesreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review, ok := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
		if !ok {
			return true, nil, errors.New("not a rules review")
		}
		namespaces = append(namespaces, review.Spec.Namespace)
		review.Status = authorizationv1.SubjectRulesReviewStatus{
			ResourceRules: []authorizationv1.ResourceRule{
				{Verbs: []string{verbCreate}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}},
				{Verbs: []string{verbGet, verbList}, Resources: []string{gvrPods.Resource}},
				{Verbs: []string{verbGet}, APIGroups: []string{gvrDeployments.Group}, Resources: []string{gvrDeployments.Resource}, ResourceNames: []string{componentDex}},
			},
			NonResourceRules: []authorizationv1.NonResourceRule{{Verbs: []string{verbGet}, NonResourceURLs: []string{"/healthz", "/api"}}},
		}
		return true, review, nil
	})
	stubClientsetFor(t, cs)
	status, err := canIList(context.Background(), &rest.Config{}, "kagent")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(namespaces, []string{"kagent"}) {
		t.Errorf("review namespaces = %v", namespaces)
	}
	rows := ruleRows(status)
	want := []string{
		"selfsubjectaccessreviews.authorization.k8s.io  []  []  [create]",
		"selfsubjectrulesreviews.authorization.k8s.io  []  []  [create]",
		"pods  []  []  [get list]",
		"deployments.apps  []  [dex]  [get]",
		"[/healthz /api]  []  [get]",
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
}

// TestWhoAmI: `auth whoami` is a SelfSubjectReview; the username and groups
// are the apiserver's.
func TestWhoAmI(t *testing.T) {
	cs := kubefake.NewClientset()
	cs.PrependReactor("create", "selfsubjectreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{
			UserInfo: authenticationv1.UserInfo{Username: "oidc:admin@lab.local", Groups: []string{"oidc:platform-admins", "system:authenticated"}},
		}}, nil
	})
	stubClientsetFor(t, cs)
	user, groups, err := whoAmI(context.Background(), &rest.Config{})
	if err != nil || user != "oidc:admin@lab.local" || !reflect.DeepEqual(groups, []string{"oidc:platform-admins", "system:authenticated"}) {
		t.Errorf("whoAmI = %q %v %v", user, groups, err)
	}
}

// TestTokenConfig: the token-only config carries the lab endpoint and CA
// from state/kubeconfig, the bearer token, and NO client certificate — the
// kind kubeconfig's admin cert would win over the token — plus the lab's
// tuning; asUserConfig keeps the admin's credentials and adds the
// impersonated user.
func TestTokenConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labKubeconfigPath, []byte(fakeKindKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := tokenConfig("tok")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != fakeKindServer || string(cfg.CAData) != "foo" {
		t.Errorf("endpoint/CA = %q %q", cfg.Host, cfg.CAData)
	}
	if cfg.BearerToken != "tok" {
		t.Errorf("bearer token = %q", cfg.BearerToken)
	}
	tls := cfg.TLSClientConfig
	if tls.CertData != nil || tls.KeyData != nil || tls.CertFile != "" || tls.KeyFile != "" {
		t.Errorf("the token config carries a client certificate: %+v", tls)
	}
	if cfg.QPS != 50 || cfg.Burst != 100 || !strings.HasPrefix(cfg.UserAgent, "agentlab/") {
		t.Errorf("tuning missing: QPS %v burst %v agent %q", cfg.QPS, cfg.Burst, cfg.UserAgent)
	}

	f := newFakeLab(t)
	as, err := asUserConfig("system:serviceaccount:agent-platform:agent-manager")
	if err != nil {
		t.Fatal(err)
	}
	if as.Impersonate.UserName != "system:serviceaccount:agent-platform:agent-manager" {
		t.Errorf("impersonation = %+v", as.Impersonate)
	}
	if string(as.CertData) != "admin" || f.cfg.Impersonate.UserName != "" {
		t.Errorf("asUserConfig must copy the admin config, not alter it: %+v / %+v", as.TLSClientConfig, f.cfg.Impersonate)
	}

	_ = os.Remove(labKubeconfigPath)
	if _, err := tokenConfig("tok"); err == nil || !strings.Contains(err.Error(), labKubeconfigPath) {
		t.Errorf("a lab that is not up must fail on the kubeconfig by name, got %v", err)
	}
}

// TestRawGet: a non-resource path through the REST client; a failure names
// the path.
func TestRawGet(t *testing.T) {
	f := newFakeLab(t)
	out, err := rawGet(context.Background(), "/readyz")
	if err != nil || string(out) != "ok" || f.readyz != 1 {
		t.Errorf("rawGet(/readyz) = %q, %v (asked %d times)", out, err, f.readyz)
	}
	if _, err := rawGet(context.Background(), "/nope"); err == nil || !strings.Contains(err.Error(), "GET /nope") {
		t.Errorf("a 404 must fail naming the path, got %v", err)
	}
}

// TestLogContainers: the container asked for (refused when absent),
// kubectl's default-container annotation, else every container.
func TestLogContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "muster-0"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: componentMuster}, {Name: dexLocalhostContainer}}},
	}
	if got, err := logContainers(pod, dexLocalhostContainer); err != nil || !reflect.DeepEqual(got, []string{dexLocalhostContainer}) {
		t.Errorf("named container = %v, %v", got, err)
	}
	if _, err := logContainers(pod, "nope"); err == nil || !strings.Contains(err.Error(), componentMuster+", "+dexLocalhostContainer) {
		t.Errorf("an absent container must be refused naming the pod's, got %v", err)
	}
	if got, err := logContainers(pod, ""); err != nil || !reflect.DeepEqual(got, []string{componentMuster, dexLocalhostContainer}) {
		t.Errorf("no choice, no annotation = %v, %v; want every container", got, err)
	}
	pod.Annotations = map[string]string{defaultContainerAnnotation: componentMuster}
	if got, err := logContainers(pod, ""); err != nil || !reflect.DeepEqual(got, []string{componentMuster}) {
		t.Errorf("default-container annotation = %v, %v", got, err)
	}
}

// TestPodLogs: a deploy/<name> target resolves through the Deployment's
// selector to its pods, several streams are prefixed, a single one is not,
// and a target matching nothing says so.
func TestPodLogs(t *testing.T) {
	f := newFakeLab(t)
	ctx := context.Background()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: componentMuster, Namespace: testNS},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appLabel: componentMuster}}},
	}
	pod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{appLabel: componentMuster}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: componentMuster}}},
		}
	}
	for _, obj := range []runtime.Object{deploy, pod("muster-b"), pod("muster-a")} {
		if err := f.cs.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	out, err := podLogs(ctx, testNS, "deploy/muster", "", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[muster-a/muster] " + fakeLogLine + "\n[muster-b/muster] " + fakeLogLine + "\n"; out != want {
		t.Errorf("deploy logs = %q, want %q", out, want)
	}
	out, err = podLogs(ctx, testNS, "pod/muster-a", componentMuster, 0)
	if err != nil || out != fakeLogLine+"\n" {
		t.Errorf("single pod logs = %q, %v", out, err)
	}
	if _, err := podLogs(ctx, testNS, appLabel+"=nothing", "", 0); err == nil || !strings.Contains(err.Error(), "no pods in demo match app=nothing") {
		t.Errorf("an empty selector must fail by selector, got %v", err)
	}
	var b strings.Builder
	if err := streamLogs(ctx, testNS, appLabel+"="+componentMuster, &b); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], fakeLogLine) {
		t.Errorf("streamed = %q", b.String())
	}
}

// TestRunProbePod: the pod is created with restartPolicy Never, waited for
// until it finishes, its log returned, and it is deleted afterwards; a
// failed pod returns its log with the failure.
func TestRunProbePod(t *testing.T) {
	f := newFakeLab(t)
	phase := corev1.PodSucceeded
	f.cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return true, nil, errors.New("not a pod")
		}
		if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "busybox" {
			return true, nil, fmt.Errorf("unexpected probe pod spec: %+v", pod.Spec)
		}
		pod.Status.Phase = phase
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}
		return false, nil, nil
	})
	ctx := context.Background()
	out, err := runProbePod(ctx, testNS, "probe", "busybox", []string{"wget", "-qO-", "http://host/"}, time.Second)
	if err != nil || out != fakeLogLine {
		t.Errorf("succeeded probe = %q, %v", out, err)
	}
	if _, err := f.cs.CoreV1().Pods(testNS).Get(ctx, "probe", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the probe pod was not deleted afterwards: %v", err)
	}
	phase = corev1.PodFailed
	out, err = runProbePod(ctx, testNS, "probe", "busybox", []string{"false"}, time.Second)
	if err == nil || out != fakeLogLine || !strings.Contains(err.Error(), "failed (Failed: exit code 1)") {
		t.Errorf("failed probe = %q, %v", out, err)
	}
}
