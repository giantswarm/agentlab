package lab

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// Workspace storage in the lab (platform.workspaces): what a Session's clone
// of a workspace is made of on an installation, on the kind node. A
// workspace is a volume, snapshotted with CSI VolumeSnapshots, and each
// Session gets a clone of the latest snapshot as an Agent Substrate external
// volume — no PVC: ate-api-server calls the driver's controller service over
// the network and atelet the node plugin's socket, both found through a
// cluster-scoped CSIDriverConfig. The kind cluster has the local-path
// provisioner alone: no CSI driver, no snapshot CRDs, no snapshot
// controller, no VolumeSnapshotClass. The switch installs them before the
// platform chart (workspaces.yaml.tmpl, volumesnapshot-crds.yaml), the way
// agent-substrate/substrate's own kind setup does:
//
//   - the external-snapshotter's CRDs and its snapshot controller;
//   - the CSI hostpath driver on ONE node (the hostpath driver keeps a
//     volume's bytes on the node it runs on, so the node that mounts them
//     for an actor must be the same: the single-node lab's control plane,
//     or the one substrateNodes worker the WorkerPool's workers land on —
//     substrateNodes above one is refused, config.Validate), with
//     Substrate's directory /var/lib/ate mounted Bidirectional so atelet
//     sees the mounts the node plugin makes;
//   - an mTLS proxy in front of the driver's controller socket, behind the
//     Service whose DNS name Substrate's service-DNS signer certifies, which
//     admits ate-api-server's pod identity alone;
//   - the StorageClass and VolumeSnapshotClass `agentlab-workspaces`.
//
// The platform values name them (the `workspaces:` block of
// agent-platform-values.yaml.tmpl) once the chart carries the key
// (chartCarriesWorkspaces): the chart then renders the CSIDriverConfig that
// registers the driver with Substrate. Until it does, the lab applies that
// one object itself after the install (ensureWorkspacesCSIDriverConfig), so
// the actor half of `agentlab workspaces-test` runs; the lab's copy goes the
// moment a chart that renders its own is installed. docs/workspaces.md is
// the human account.

const (
	// workspacesNamespace holds the snapshot controller, the driver and its
	// proxy. `agentlab workspaces-test` creates its own next to it.
	workspacesNamespace = "agentlab-workspaces"
	// workspacesStorageClass and workspacesSnapshotClass are the classes
	// the chart's workspaces values name: the workspace volume, its
	// snapshots and a Session's clone all on one driver, so a snapshot
	// handle is always restorable by the clone's driver.
	workspacesStorageClass  = "agentlab-workspaces"
	workspacesSnapshotClass = "agentlab-workspaces"
	// workspacesCSIDriver is the CSI hostpath driver's name: the
	// StorageClass's provisioner, the CSIDriver object, the
	// CSIDriverConfig's name and driverName.
	workspacesCSIDriver = "hostpath.csi.k8s.io"
	// workspacesControllerService fronts the proxy: its DNS name is the
	// CSIDriverConfig's controllerEndpoint and, as <service>.<namespace>.svc,
	// the one DNS name the service-DNS signer puts on the proxy's
	// certificate — the tls.serverName ate-api-server verifies.
	workspacesControllerService = "csi-hostpath-controller"
	workspacesControllerPort    = 50051
	// workspacesProxyPort is the proxy's mTLS listener, the Service's target.
	workspacesProxyPort = 10000
	// The workloads: the driver's StatefulSet with its sidecars, the proxy's
	// StatefulSet, the snapshot controller's Deployment.
	workspacesPluginStatefulSet  = "csi-hostpathplugin"
	workspacesProxyStatefulSet   = "csi-hostpath-proxy"
	snapshotControllerDeployment = "snapshot-controller"
	// workspacesNodeSocket is the node plugin's socket on the node, the
	// CSIDriverConfig's nodeSocketOverride: the registrar's
	// kubelet-registration-path.
	workspacesNodeSocket = "unix:///var/lib/kubelet/plugins/csi-hostpath/csi.sock"
	// workspacesDataDir is where the driver keeps a volume's bytes on the
	// node, <dir>/<volume id>; the proof reads an actor's file there.
	workspacesDataDir = "/var/lib/csi-hostpath-data"
	// substrateVolumesDir is Substrate's directory on the node, which
	// atelet bind-mounts a published volume from into the actor's sandbox.
	substrateVolumesDir = "/var/lib/ate"
	// ateAPIServerSPIFFEID is the pod identity Substrate's pod-identity
	// signer gives ate-api-server (spiffe://<trust domain>/ns/<ns>/sa/<sa>),
	// the one client the proxy admits.
	ateAPIServerSPIFFEID = "spiffe://cluster.local/ns/ate-system/sa/ate-api-server"
	// workspacesTemplate renders everything but the CRDs.
	workspacesTemplate = "workspaces.yaml.tmpl"
	// csiDriverConfigResource is Substrate's CSIDriverConfig API, resolved
	// through discovery like every custom kind the lab reads.
	csiDriverConfigResource = "csidriverconfigs.ate.dev"
	// workspacesRolloutTimeout bounds each workload's rollout: the images
	// are side-loaded, so a rollout that takes longer is stuck.
	workspacesRolloutTimeout = 5 * time.Minute
)

// The image pins (bumped deliberately, like the observability charts'): the
// external-snapshotter line's controller and csi-snapshotter at one release,
// the hostpath driver with the sidecars its upstream deploy pins, Envoy for
// the proxy. gsoci carries every image but the hostpath driver's, which
// comes from registry.k8s.io; all are side-loaded host cache -> node like
// every platform image (preload.go).
const (
	snapshotControllerImage = "gsoci.azurecr.io/giantswarm/snapshot-controller:v8.6.0"
	csiSnapshotterImage     = "gsoci.azurecr.io/giantswarm/csi-snapshotter:v8.6.0"
	csiHostPathImage        = "registry.k8s.io/sig-storage/hostpathplugin:v1.17.1"
	csiRegistrarImage       = "gsoci.azurecr.io/giantswarm/csi-node-driver-registrar:v2.17.0"
	csiLivenessProbeImage   = "gsoci.azurecr.io/giantswarm/livenessprobe:v2.19.0"
	csiAttacherImage        = "gsoci.azurecr.io/giantswarm/csi-attacher:v4.12.0"
	csiProvisionerImage     = "gsoci.azurecr.io/giantswarm/csi-provisioner:v6.3.0"
	csiResizerImage         = "gsoci.azurecr.io/giantswarm/csi-resizer:v2.2.1"
	csiProxyImage           = "gsoci.azurecr.io/giantswarm/envoy:v1.35.9"
)

// WorkspacesStorageClass and WorkspacesSnapshotClass are the classes for the
// configure summary.
const (
	WorkspacesStorageClass  = workspacesStorageClass
	WorkspacesSnapshotClass = workspacesSnapshotClass
)

//go:embed templates/volumesnapshot-crds.yaml
var volumeSnapshotCRDs []byte

// workspacesImages are the pins as the template renders them.
type workspacesImages struct {
	SnapshotController, HostPath, Registrar, LivenessProbe, Attacher, Provisioner, Resizer, Snapshotter, Proxy string
}

// workspacesValues are the names workspaces.yaml.tmpl and the values
// template's `workspaces:` block render.
type workspacesValues struct {
	Namespace, Driver, StorageClass, SnapshotClass string
	ControllerService, Plugin, Proxy, SnapshotController  string
	ControllerPort, ProxyPort                            int
	// ControllerEndpoint, ServerName and NodeSocket are the CSIDriverConfig's
	// controllerEndpoint, tls.serverName and nodeSocketOverride.
	ControllerEndpoint, ServerName, NodeSocket string
	DataDir, SubstrateDir, ClientSPIFFEID      string
	// Node is the node the driver and its proxy are pinned to;
	// OnSubstrateWorker says it is the substrateNodes worker, whose taint
	// they tolerate.
	Node              string
	OnSubstrateWorker bool
	Images            workspacesImages
}

func workspacesValuesFor(cfg *config.Config) workspacesValues {
	node, onWorker := workspacesNode(cfg)
	return workspacesValues{
		Namespace:          workspacesNamespace,
		Driver:             workspacesCSIDriver,
		StorageClass:       workspacesStorageClass,
		SnapshotClass:      workspacesSnapshotClass,
		ControllerService:  workspacesControllerService,
		Plugin:             workspacesPluginStatefulSet,
		Proxy:              workspacesProxyStatefulSet,
		SnapshotController: snapshotControllerDeployment,
		ControllerPort:     workspacesControllerPort,
		ProxyPort:          workspacesProxyPort,
		ControllerEndpoint: fmt.Sprintf("tcp://%s.%s.svc.cluster.local:%d", workspacesControllerService, workspacesNamespace, workspacesControllerPort),
		ServerName:         workspacesControllerServerName(),
		NodeSocket:         workspacesNodeSocket,
		DataDir:            workspacesDataDir,
		SubstrateDir:       substrateVolumesDir,
		ClientSPIFFEID:     ateAPIServerSPIFFEID,
		Node:               node,
		OnSubstrateWorker:  onWorker,
		Images: workspacesImages{
			SnapshotController: snapshotControllerImage,
			HostPath:           csiHostPathImage,
			Registrar:          csiRegistrarImage,
			LivenessProbe:      csiLivenessProbeImage,
			Attacher:           csiAttacherImage,
			Provisioner:        csiProvisionerImage,
			Resizer:            csiResizerImage,
			Snapshotter:        csiSnapshotterImage,
			Proxy:              csiProxyImage,
		},
	}
}

// workspacesControllerServerName is the DNS name the service-DNS signer
// certifies for the proxy, <service>.<namespace>.svc: the TLS server name
// ate-api-server verifies. Spelled out — Substrate's examples name
// <service>.<namespace>.svc.cluster.local, which the certificate does not
// carry, and an unset name would skip the verification.
func workspacesControllerServerName() string {
	return workspacesControllerService + "." + workspacesNamespace + ".svc"
}

// workspacesNode is the node the driver is pinned to: the one substrateNodes
// worker, where the WorkerPool's workers (and so the actors) land, else the
// control plane of the single-node lab. The second value says it is the
// worker, whose NoSchedule taint the driver's pods then tolerate.
func workspacesNode(cfg *config.Config) (string, bool) {
	if names := cfg.SubstrateNodeNames(); len(names) == 1 {
		return names[0], true
	}
	return cfg.ControlPlaneNode(), false
}

// WorkspacesNode is workspacesNode for the configure summary.
func WorkspacesNode(cfg *config.Config) string {
	node, _ := workspacesNode(cfg)
	return node
}

// chartCarriesWorkspaces reports whether the meta chart about to be installed
// takes a `workspaces` key: its values schema names it at the root (the
// schema's root refuses unknown keys, so a block on a chart without it fails
// the install at render), or its default values carry it. Memoized per
// chart, since a registry chart is pulled once per digest; a test points
// chartWorkspacesProbe at a stub.
func chartCarriesWorkspaces(chart platformChart) (bool, error) {
	chartWorkspacesMu.Lock()
	defer chartWorkspacesMu.Unlock()
	key := chart.String()
	if carries, ok := chartWorkspacesCache[key]; ok {
		return carries, nil
	}
	carries, err := chartWorkspacesProbe(chart)
	if err != nil {
		return false, err
	}
	chartWorkspacesCache[key] = carries
	return carries, nil
}

var (
	chartWorkspacesMu    sync.Mutex
	chartWorkspacesCache = map[string]bool{}
	// chartWorkspacesProbe loads the chart and reads its schema and values;
	// the tests replace it.
	chartWorkspacesProbe = func(chart platformChart) (bool, error) {
		c, err := loadPlatformChart(chart)
		if err != nil {
			return false, err
		}
		if c == nil {
			return false, nil
		}
		return chartValuesCarry(c.Schema, c.Values, valuesWorkspaces), nil
	}
)

// valuesWorkspaces is the chart's workspaces key.
const valuesWorkspaces = "workspaces"

// workspacesChartCarries is chartCarriesWorkspaces for the configured chart
// when the switch is on, false otherwise; a chart that cannot be probed is a
// note and false, so the lab registers the driver with Substrate itself.
func workspacesChartCarries(cfg *config.Config) bool {
	if !cfg.WorkspacesEnabled() {
		return false
	}
	carries, err := chartCarriesWorkspaces(platformChartFor(cfg))
	if err != nil {
		note("the platform chart's workspaces values could not be probed (%v): the lab registers the driver with Substrate itself", err)
		return false
	}
	return carries
}

// chartValuesCarry reports whether a chart's values schema names key among
// its root properties, or its default values carry it.
func chartValuesCarry(schema []byte, values map[string]any, key string) bool {
	if _, ok := values[key]; ok {
		return true
	}
	if len(schema) == 0 {
		return false
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return false
	}
	_, ok := s.Properties[key]
	return ok
}

// workspacesUp installs the workspace storage: the snapshot CRDs, then the
// rendered manifest (the snapshot controller, the driver, its proxy, the
// classes), its images side-loaded first like every platform image; the
// controller's and the driver's rollouts are waited for. The proxy's pod is
// not: its certificate comes from Substrate's signers, which the platform
// install brings — waitWorkspacesProxy, after the install. An idempotent
// re-apply on a lab that has it.
func workspacesUp(cfg *config.Config) error {
	ctx := context.Background()
	node, _ := workspacesNode(cfg)
	step("Installing the workspace storage (the CSI snapshot controller, the CSI hostpath driver on %s behind its mTLS proxy, the classes %s)", node, workspacesStorageClass)
	if _, err := applyManifests(ctx, volumeSnapshotCRDs); err != nil {
		return fmt.Errorf("the CSI snapshot CRDs: %w", err)
	}
	manifest, _, err := renderManifest(cfg, workspacesTemplate)
	if err != nil {
		return err
	}
	if imgs := scrapeImages(string(manifest)); len(imgs) > 0 {
		if res := sideloadImages(cfg, hostPullImages(imgs)); res.n > 0 {
			note("side-loaded %d workspace storage images (%s)", res.n, res.d)
		}
	}
	if _, err := applyManifests(ctx, manifest); err != nil {
		return fmt.Errorf("the workspace storage: %w", err)
	}
	if err := waitDeploymentRolledOut(ctx, workspacesNamespace, snapshotControllerDeployment, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the snapshot controller: %w", err)
	}
	if err := waitStatefulSetReady(ctx, workspacesNamespace, workspacesPluginStatefulSet, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the CSI hostpath driver: %w", err)
	}
	note("snapshot controller rolled out; CSI driver %s Ready on %s (volumes under %s, Substrate's %s mounted Bidirectional); StorageClass and VolumeSnapshotClass %s",
		workspacesCSIDriver, node, workspacesDataDir, substrateVolumesDir, workspacesStorageClass)
	return nil
}

// waitWorkspacesProxy waits for the mTLS proxy's pod after the platform
// install: its serving certificate is a PodCertificateRequest the
// service-DNS signer of Substrate's podcertificate-controller answers, and
// its client trust the pod-identity CA's live ClusterTrustBundle — neither
// exists before the chart.
func waitWorkspacesProxy(ctx context.Context) error {
	step("Waiting for the workspace storage's mTLS proxy (its certificate from Substrate's service-DNS signer)")
	if err := waitStatefulSetReady(ctx, workspacesNamespace, workspacesProxyStatefulSet, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the CSI controller proxy: %w (the signer issues a certificate only to a pod the Service %s selects; `agentlab pods -n %s`, `kubectl -n %s describe pod %s-0`)",
			err, workspacesControllerService, workspacesNamespace, workspacesNamespace, workspacesProxyStatefulSet)
	}
	note("proxy Ready: %s serves %s:%d with the certificate of %s, admitting %s alone",
		workspacesProxyStatefulSet, workspacesControllerService, workspacesControllerPort, workspacesControllerServerName(), ateAPIServerSPIFFEID)
	return nil
}

// workspacesCSIDriverConfig is the CSIDriverConfig that registers the lab's
// driver with Substrate, as the chart's workspaces.substrate.csiDriver
// values render it: the lab applies it while the chart cannot.
func workspacesCSIDriverConfig() *unstructured.Unstructured {
	v := workspacesValuesFor(config.Default())
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1alpha1",
		"kind":       "CSIDriverConfig",
		crMetadata: map[string]any{
			nameKey:  workspacesCSIDriver,
			"labels": map[string]any{managedByLabel: managedByAgentlabValue},
		},
		crSpec: map[string]any{
			"driverName":         workspacesCSIDriver,
			"controllerEndpoint": v.ControllerEndpoint,
			"nodeSocketOverride": v.NodeSocket,
			"tls": map[string]any{
				"enabled":        true,
				"usePodIdentity": true,
				"serverName":     v.ServerName,
			},
		},
	}}
}

// ensureWorkspacesCSIDriverConfig applies the lab's CSIDriverConfig after
// the install, on a chart that does not render one; the CRD comes with the
// chart's substrate-crds component. A chart that carries the key renders
// the object itself from the lab's values: nothing to apply.
func ensureWorkspacesCSIDriverConfig(ctx context.Context, chartCarries bool) error {
	if chartCarries {
		return nil
	}
	gvr, err := gvrFor(csiDriverConfigResource)
	if err != nil {
		return fmt.Errorf("the CSIDriverConfig API of Substrate is not served (%v): the agents runtime brings it; is platform.agents on and the substrate-crds component installed?", err)
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	res, err := k.apply(ctx, workspacesCSIDriverConfig())
	if err != nil {
		return fmt.Errorf("the CSIDriverConfig %s: %w", workspacesCSIDriver, err)
	}
	note("%s (the chart carries no workspaces values yet, so the lab registers the driver with Substrate itself: controllerEndpoint %s, tls.serverName %s)",
		res, workspacesValuesFor(config.Default()).ControllerEndpoint, workspacesControllerServerName())
	_ = gvr
	return nil
}

// removeLabWorkspacesCSIDriverConfig deletes the CSIDriverConfig the lab
// applied, before a chart that renders its own is installed (Helm refuses
// to adopt an object another manager created) and when the switch goes off.
// Nothing to do without the CRD or the lab's label on the object.
func removeLabWorkspacesCSIDriverConfig(ctx context.Context) error {
	gvr, err := gvrFor(csiDriverConfigResource)
	if err != nil {
		return nil
	}
	obj, err := getObject(ctx, gvr, "", workspacesCSIDriver)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if obj.GetLabels()["app.kubernetes.io/managed-by"] != managedByAgentlabValue {
		return nil
	}
	note("removing the CSIDriverConfig %s the lab applied (the chart renders its own now)", workspacesCSIDriver)
	return deleteObject(ctx, gvr, "", workspacesCSIDriver, fixtureDeleteWait)
}

// The cluster-scoped objects of the workspace storage besides the CRDs, as
// kubectl resource arguments and names: what workspacesDown removes after
// the namespace. The classes go before the CRDs that define them.
var workspacesClusterObjects = []struct{ resource, name string }{
	{"volumesnapshotclasses.snapshot.storage.k8s.io", workspacesSnapshotClass},
	{"storageclasses.storage.k8s.io", workspacesStorageClass},
	{"csidrivers.storage.k8s.io", workspacesCSIDriver},
	{"clusterrolebindings.rbac.authorization.k8s.io", "agentlab-workspaces-csi-hostpathplugin"},
	{"clusterroles.rbac.authorization.k8s.io", "agentlab-workspaces-csi-hostpathplugin"},
	{"clusterrolebindings.rbac.authorization.k8s.io", "agentlab-workspaces-snapshot-controller"},
	{"clusterroles.rbac.authorization.k8s.io", "agentlab-workspaces-snapshot-controller"},
}

// workspacesDown removes the workspace storage from a lab that has it: the
// lab's CSIDriverConfig, the namespace with everything in it, the
// cluster-scoped objects and the snapshot CRDs, so a lab with the switch off
// carries nothing of it. Quiet on a lab that never had it. A volume the
// driver still publishes keeps the namespace Terminating until its pod is
// gone: `agentlab workspaces-test` removes its own.
func workspacesDown(ctx context.Context) error {
	exists, err := objectExists(ctx, gvrNamespaces, "", workspacesNamespace)
	if err != nil {
		return err
	}
	crds := workspacesCRDNames()
	if !exists && !anyObjectExists(ctx, "customresourcedefinitions.apiextensions.k8s.io", crds) {
		return nil
	}
	step("Removing the workspace storage (platform.workspaces is off)")
	if err := removeLabWorkspacesCSIDriverConfig(ctx); err != nil {
		return err
	}
	if err := deleteNamespace(ctx, workspacesNamespace); err != nil {
		return err
	}
	for _, o := range workspacesClusterObjects {
		gvr, err := gvrFor(o.resource)
		if err != nil {
			continue // the CRD is gone already, and so is everything of its kind
		}
		if err := deleteObject(ctx, gvr, "", o.name, fixtureDeleteWait); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s %s: %w", o.resource, o.name, err)
		}
	}
	gvr, err := gvrFor("customresourcedefinitions.apiextensions.k8s.io")
	if err != nil {
		return err
	}
	for _, name := range crds {
		if err := deleteObject(ctx, gvr, "", name, fixtureDeleteWait); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing the CRD %s: %w (a VolumeSnapshot left behind holds it; delete it first)", name, err)
		}
	}
	note("workspace storage removed: namespace %s, the classes, the CSIDriver and the snapshot CRDs", workspacesNamespace)
	return nil
}

// workspacesCRDNames are the snapshot CRDs' names, read off the bundle.
func workspacesCRDNames() []string {
	objs, err := decodeManifests(volumeSnapshotCRDs)
	if err != nil {
		return nil
	}
	var names []string
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	return names
}

// anyObjectExists reports whether any of the named cluster-scoped objects of
// a resource exists; a resource the apiserver does not serve has none.
func anyObjectExists(ctx context.Context, resource string, names []string) bool {
	gvr, err := gvrFor(resource)
	if err != nil {
		return false
	}
	for _, name := range names {
		if exists, err := objectExists(ctx, gvr, "", name); err == nil && exists {
			return true
		}
	}
	return false
}

// cleanWorkspacesNode unmounts what the driver published under Substrate's
// directory on a node and removes the volumes' bytes — on `agentlab down`
// before the node is deleted (a bind mount the node still holds can keep the
// container from being removed) and on `agentlab platform-down`. Best
// effort through `docker exec`; a node that is not running has nothing to
// clean.
func cleanWorkspacesNode(node string) {
	script := fmt.Sprintf(`for m in $(awk '$2 ~ "^%s/" {print $2}' /proc/mounts | sort -r); do umount -f "$m" 2>/dev/null; done; rm -rf %s/* 2>/dev/null; true`,
		substrateVolumesDir, workspacesDataDir)
	if _, err := outputQuiet(dockerBin, "exec", node, "sh", "-c", script); err != nil {
		return
	}
}

// waitStatefulSetReady is `kubectl rollout status statefulset/<name>`: every
// replica of the current revision Ready, bounded by timeout; the last read
// state is in the error.
func waitStatefulSetReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	var last *appsv1.StatefulSet
	var readErr error
	waitErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		last, readErr = k.clientset.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return false, nil
		}
		return statefulSetReady(last), nil
	})
	if waitErr == nil {
		return nil
	}
	if readErr != nil {
		return fmt.Errorf("statefulset %s/%s: %w", ns, name, readErr)
	}
	return fmt.Errorf("statefulset %s/%s is not Ready after %s (%s)", ns, name, timeout, statefulSetStatus(last))
}

// statefulSetReady reports whether a StatefulSet has rolled out: the status
// current, every replica on the update revision and Ready.
func statefulSetReady(s *appsv1.StatefulSet) bool {
	if s == nil || s.Status.ObservedGeneration < s.Generation {
		return false
	}
	replicas := int32(1)
	if s.Spec.Replicas != nil {
		replicas = *s.Spec.Replicas
	}
	return s.Status.UpdatedReplicas >= replicas && s.Status.ReadyReplicas >= replicas &&
		(s.Status.UpdateRevision == "" || s.Status.CurrentRevision == s.Status.UpdateRevision)
}

// statefulSetStatus words a StatefulSet's rollout state for an error.
func statefulSetStatus(s *appsv1.StatefulSet) string {
	if s == nil {
		return stateNotRead
	}
	return fmt.Sprintf("%d of %d replicas ready, %d updated", s.Status.ReadyReplicas, s.Status.Replicas, s.Status.UpdatedReplicas)
}

// WorkspacesStatus is the workspace storage as `agentlab status` reports it.
type WorkspacesStatus struct {
	// SnapshotController, Driver and Proxy are the workloads' rollout states
	// in a word: Ready, or what is not.
	SnapshotController string `json:"snapshotController"`
	Driver             string `json:"driver"`
	Proxy              string `json:"proxy"`
	// Node is the node the driver is pinned to.
	Node string `json:"node,omitempty"`
	// StorageClass and SnapshotClass report whether the classes exist.
	StorageClass  bool `json:"storageClass"`
	SnapshotClass bool `json:"volumeSnapshotClass"`
	// CSIDriverConfig says who registered the driver with Substrate: "the
	// lab", "the chart", or "" when nothing did.
	CSIDriverConfig string `json:"csiDriverConfig,omitempty"`
}

// readWorkspacesStatus reads the workspace storage's live state; nil when
// the namespace does not exist (the switch is off and nothing is left).
func readWorkspacesStatus(ctx context.Context) (*WorkspacesStatus, error) {
	exists, err := objectExists(ctx, gvrNamespaces, "", workspacesNamespace)
	if err != nil || !exists {
		return nil, err
	}
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	s := &WorkspacesStatus{}
	if d, err := k.clientset.AppsV1().Deployments(workspacesNamespace).Get(ctx, snapshotControllerDeployment, metav1.GetOptions{}); err != nil {
		s.SnapshotController = readState(err)
	} else if _, done, _ := deploymentRolloutStatus(d); done {
		s.SnapshotController = conditionReady
	} else {
		s.SnapshotController = fmt.Sprintf("%d of %d replicas available", d.Status.AvailableReplicas, d.Status.Replicas)
	}
	for _, w := range []struct {
		name string
		into *string
	}{{workspacesPluginStatefulSet, &s.Driver}, {workspacesProxyStatefulSet, &s.Proxy}} {
		ss, err := k.clientset.AppsV1().StatefulSets(workspacesNamespace).Get(ctx, w.name, metav1.GetOptions{})
		switch {
		case err != nil:
			*w.into = readState(err)
		case statefulSetReady(ss):
			*w.into = conditionReady
			s.Node = ss.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		default:
			*w.into = statefulSetStatus(ss)
		}
	}
	s.StorageClass = clusterObjectExists(ctx, "storageclasses.storage.k8s.io", workspacesStorageClass)
	s.SnapshotClass = clusterObjectExists(ctx, "volumesnapshotclasses.snapshot.storage.k8s.io", workspacesSnapshotClass)
	if gvr, err := gvrFor(csiDriverConfigResource); err == nil {
		if obj, err := getObject(ctx, gvr, "", workspacesCSIDriver); err == nil {
			s.CSIDriverConfig = "the chart"
			if obj.GetLabels()["app.kubernetes.io/managed-by"] == managedByAgentlabValue {
				s.CSIDriverConfig = "the lab"
			}
		}
	}
	return s, nil
}

// readState words a workload read's failure: missing, or the error.
func readState(err error) string {
	if apierrors.IsNotFound(err) {
		return stateMissing
	}
	return "unreadable: " + err.Error()
}

// clusterObjectExists reports whether a cluster-scoped object exists; a
// resource the apiserver does not serve has none.
func clusterObjectExists(ctx context.Context, resource, name string) bool {
	gvr, err := gvrFor(resource)
	if err != nil {
		return false
	}
	exists, err := objectExists(ctx, gvr, "", name)
	return err == nil && exists
}

// String words the status in one line, for `agentlab status` and `list`.
func (s *WorkspacesStatus) String() string {
	classes := []string{}
	if s.StorageClass {
		classes = append(classes, "StorageClass "+workspacesStorageClass)
	}
	if s.SnapshotClass {
		classes = append(classes, "VolumeSnapshotClass "+workspacesSnapshotClass)
	}
	registered := "not registered with Substrate (no CSIDriverConfig)"
	if s.CSIDriverConfig != "" {
		registered = "CSIDriverConfig " + workspacesCSIDriver + " by " + s.CSIDriverConfig
	}
	return fmt.Sprintf("snapshot controller %s; CSI driver %s %s on %s; mTLS proxy %s; %s; %s",
		s.SnapshotController, workspacesCSIDriver, s.Driver, orNone(s.Node), s.Proxy, orNoneOf(strings.Join(classes, ", "), "no class"), registered)
}

// orNoneOf is s, or the given word when s is empty.
func orNoneOf(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

// workspacesHint is the platform-up summary for the workspaces switch.
func workspacesHint(cfg *config.Config, chartCarries bool) string {
	if !cfg.WorkspacesEnabled() {
		return "  Workspace storage is off (platform.workspaces in agentlab.yaml; `agentlab configure --workspaces` turns it on)."
	}
	node, _ := workspacesNode(cfg)
	registered := "registered with Substrate by the lab's CSIDriverConfig (the chart carries no workspaces values yet)"
	if chartCarries {
		registered = "registered with Substrate by the chart's CSIDriverConfig (the lab's workspaces values)"
	}
	return fmt.Sprintf("  Workspace storage: the CSI snapshot controller and the CSI hostpath driver on %s behind its mTLS proxy, %s;\n"+
		"  StorageClass and VolumeSnapshotClass %s. Proof: `agentlab workspaces-test --storage-only`.", node, registered, workspacesStorageClass)
}

// stateNotRead and stateMissing word a status that could not be read and
// an object that is not there.
const (
	stateNotRead = "not read"
	stateMissing = "missing"
)

// substrateNodeTaintValue is the value of the substrate workers' taint and
// label (config.SubstrateNodeKey), what a pod on them tolerates.
const substrateNodeTaintValue = "true"

// substrateNodeToleration tolerates the substrate workers' taint.
func substrateNodeToleration() corev1.Toleration {
	return corev1.Toleration{Key: config.SubstrateNodeKey, Operator: corev1.TolerationOpEqual, Value: substrateNodeTaintValue, Effect: corev1.TaintEffectNoSchedule}
}

// gvrVolumeSnapshots is the snapshot API the proof drives.
var gvrVolumeSnapshots = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
