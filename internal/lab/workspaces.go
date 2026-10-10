package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// Workspace storage in the lab (platform.workspaces): a read-write-many
// StorageClass that serves git, what a workspace's volume is made of on an
// installation (EFS, Azure Files over NFS), on the kind cluster. A workspace
// is one volume shared by its sync and every Session on it: bare mirrors of
// its repositories and a directory per Session, which the Session's actor
// mounts at a sub-path through Agent Substrate — its own directory
// read-write, the mirrors read-only. Substrate uses no PVC for that mount:
// ate-api-server calls the driver's controller over the network and atelet
// the node plugin's socket, both found through a cluster-scoped
// CSIDriverConfig. The kind cluster has the local-path provisioner alone,
// read-write-once and no CSI driver. The switch installs, before the platform
// chart (workspaces.yaml.tmpl), the way agent-substrate/substrate's own kind
// setup does for its driver:
//
//   - an in-cluster NFS server on the control plane, exporting a directory of
//     the node;
//   - the NFS CSI driver (kubernetes-csi/csi-driver-nfs): the controller
//     beside the server, the node plugin on every node with Substrate's
//     directory /var/lib/ate mounted Bidirectional so atelet sees the mounts
//     the plugin makes;
//   - an mTLS proxy in the controller's pod, behind the Service whose DNS
//     name Substrate's service-DNS signer certifies, which admits
//     ate-api-server's pod identity alone;
//   - the StorageClass `agentlab-workspaces`: a directory of the export per
//     volume, NFSv4.1, no snapshots (a Session is a directory on the
//     workspace's volume, not a clone of it).
//
// The platform values name the class and the driver (the `workspaces:`
// block of agent-platform-values.yaml.tmpl) once the chart carries the key
// (chartCarriesWorkspaces): the chart then renders the CSIDriverConfig that
// registers the driver with Substrate. Until it does, the lab applies that
// one object itself after the install (ensureWorkspacesCSIDriverConfig); the
// lab's copy goes the moment a chart that renders its own is installed.
// docs/workspaces.md is the human account.

const (
	// workspacesNamespace holds the NFS server and the driver.
	workspacesNamespace = "agentlab-workspaces"
	// workspacesStorageClass is the read-write-many class the chart's
	// workspaces values name.
	workspacesStorageClass = "agentlab-workspaces"
	// workspacesCSIDriver is the driver's name: csi-driver-nfs's.
	workspacesCSIDriver = "nfs.csi.k8s.io"
	// workspacesDriverVersion is the csi-driver-nfs release the manifests
	// follow (deploy/<version>/) and the driver image's tag.
	workspacesDriverVersion = "v4.13.4"
	// workspacesNFSServer is the NFS server's Deployment and Service.
	workspacesNFSServer = "nfs-server"
	// workspacesExportDir is the directory of the control plane the server
	// exports: every volume is a directory under it.
	workspacesExportDir = "/var/lib/agentlab-workspaces"
	// workspacesControllerService fronts the controller's mTLS proxy; the
	// CSIDriverConfig's controllerEndpoint is its DNS name and port.
	workspacesControllerService = "csi-nfs-controller"
	workspacesControllerPort    = 50051
	// workspacesProxyPort is the proxy's listener: on the node's network,
	// like the controller pod it sits in.
	workspacesProxyPort = 10000
	// workspacesController and workspacesNodePlugin are the driver's
	// Deployment and DaemonSet.
	workspacesController = "csi-nfs-controller"
	workspacesNodePlugin = "csi-nfs-node"
	// workspacesNodeSocketDir is where the node plugin registers with the
	// kubelet; the CSIDriverConfig's nodeSocketOverride is its socket.
	workspacesNodeSocketDir = "/var/lib/kubelet/plugins/csi-nfsplugin"
	// substrateVolumesDir is where atelet asks the node plugin to publish an
	// actor's volume: the node plugin mounts it Bidirectional.
	substrateVolumesDir = "/var/lib/ate"
	// ateAPIServerSPIFFEID is the client identity the proxy admits.
	ateAPIServerSPIFFEID = "spiffe://cluster.local/ns/ate-system/sa/ate-api-server"
	// workspacesTemplate renders everything.
	workspacesTemplate = "workspaces.yaml.tmpl"
	// csiDriverConfigResource is Substrate's registration of a driver.
	csiDriverConfigResource = "csidriverconfigs.ate.dev"
	// workspacesRolloutTimeout bounds each workload's rollout.
	workspacesRolloutTimeout = 5 * time.Minute
)

// The images, every one pinned. The sidecars and Envoy are gsoci mirrors;
// the driver is csi-driver-nfs's own image on gsoci; the NFS server is the
// csi-driver-nfs project's kind fixture (a kernel nfsd on Alpine), pinned by
// digest since its tags move.
const (
	nfsServerImage        = "docker.io/itsthenetwork/nfs-server-alpine:latest@sha256:7fa99ae65c23c5af87dd4300e543a86b119ed15ba61422444207efc7abd0ba20"
	nfsPluginImage        = "gsoci.azurecr.io/giantswarm/nfsplugin:" + workspacesDriverVersion
	csiRegistrarImage     = "gsoci.azurecr.io/giantswarm/csi-node-driver-registrar:v2.17.0"
	csiLivenessProbeImage = "gsoci.azurecr.io/giantswarm/livenessprobe:v2.19.0"
	csiProvisionerImage   = "gsoci.azurecr.io/giantswarm/csi-provisioner:v6.3.0"
	csiResizerImage       = "gsoci.azurecr.io/giantswarm/csi-resizer:v2.2.1"
	csiProxyImage         = "gsoci.azurecr.io/giantswarm/envoy:v1.35.9"
)

// WorkspacesStorageClass is the class for the configure summary.
const WorkspacesStorageClass = workspacesStorageClass

// stateNotRead and stateMissing word a status that could not be read and
// an object that is not there.
const (
	stateNotRead = "not read"
	stateMissing = "missing"
)

type workspacesImages struct {
	NFSServer, NFSPlugin, Registrar, LivenessProbe, Provisioner, Resizer, Proxy string
}

// workspacesValues are the names workspaces.yaml.tmpl and the values
// template's `workspaces:` block render.
type workspacesValues struct {
	Namespace, Driver, DriverVersion, StorageClass string
	NFSServer, NFSServerHost, ExportDir            string
	ControllerService, Controller, NodePlugin      string
	ControllerPort, ProxyPort                      int
	// ControllerEndpoint, ServerName and NodeSocket are the CSIDriverConfig's
	// controllerEndpoint, tls.serverName and nodeSocketOverride.
	ControllerEndpoint, ServerName, NodeSocket  string
	NodeSocketDir, SubstrateDir, ClientSPIFFEID string
	// Node is the control plane, where the NFS server and the controller
	// run: the export is a directory of that node.
	Node   string
	Images workspacesImages
}

func workspacesValuesFor(cfg *config.Config) workspacesValues {
	return workspacesValues{
		Namespace:          workspacesNamespace,
		Driver:             workspacesCSIDriver,
		DriverVersion:      workspacesDriverVersion,
		StorageClass:       workspacesStorageClass,
		NFSServer:          workspacesNFSServer,
		NFSServerHost:      workspacesNFSServer + "." + workspacesNamespace + ".svc.cluster.local",
		ExportDir:          workspacesExportDir,
		ControllerService:  workspacesControllerService,
		Controller:         workspacesController,
		NodePlugin:         workspacesNodePlugin,
		ControllerPort:     workspacesControllerPort,
		ProxyPort:          workspacesProxyPort,
		ControllerEndpoint: fmt.Sprintf("tcp://%s.%s.svc.cluster.local:%d", workspacesControllerService, workspacesNamespace, workspacesControllerPort),
		ServerName:         workspacesControllerServerName(),
		NodeSocket:         "unix://" + workspacesNodeSocketDir + "/csi.sock",
		NodeSocketDir:      workspacesNodeSocketDir,
		SubstrateDir:       substrateVolumesDir,
		ClientSPIFFEID:     ateAPIServerSPIFFEID,
		Node:               WorkspacesNode(cfg),
		Images: workspacesImages{
			NFSServer:     nfsServerImage,
			NFSPlugin:     nfsPluginImage,
			Registrar:     csiRegistrarImage,
			LivenessProbe: csiLivenessProbeImage,
			Provisioner:   csiProvisionerImage,
			Resizer:       csiResizerImage,
			Proxy:         csiProxyImage,
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

// WorkspacesNode is the node the NFS server and the driver's controller run
// on: the control plane, whose disk holds the export. The node plugin runs on
// every node, so the actors' workers may be anywhere.
func WorkspacesNode(cfg *config.Config) string {
	return cfg.ControlPlaneNode()
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

// workspacesUp installs the workspace storage: the rendered manifest (the
// NFS server, the driver, its proxy, the class), its images side-loaded
// first like every platform image; the server's and the node plugin's
// rollouts are waited for. The controller's is not: its proxy's certificate
// comes from Substrate's signers, which the platform install brings —
// waitWorkspacesProxy, after the install. An idempotent re-apply on a lab
// that has it.
func workspacesUp(cfg *config.Config) error {
	ctx := context.Background()
	node := WorkspacesNode(cfg)
	step("Installing the workspace storage (an NFS server on %s, the NFS CSI driver %s behind its mTLS proxy, the read-write-many StorageClass %s)", node, workspacesDriverVersion, workspacesStorageClass)
	if err := workspacesHostPreflight(); err != nil {
		return err
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
	if err := waitDeploymentRolledOut(ctx, workspacesNamespace, workspacesNFSServer, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the NFS server: %w (the kind node loads the host kernel's nfsd for it; `kubectl -n %s logs deploy/%s`)", err, workspacesNamespace, workspacesNFSServer)
	}
	if err := waitDaemonSetReady(ctx, workspacesNamespace, workspacesNodePlugin, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the NFS CSI node plugin: %w", err)
	}
	note("NFS server exporting %s of %s; node plugin %s Ready on every node (Substrate's %s mounted Bidirectional); StorageClass %s (read-write-many, NFSv4.1)",
		workspacesExportDir, node, workspacesCSIDriver, substrateVolumesDir, workspacesStorageClass)
	return nil
}

// workspacesHostPreflight checks the host kernel can serve NFS: the kind node
// shares it, and the NFS server in the cluster is the kernel's nfsd, loaded
// on the first export; the client side is the kernel's nfs module. A module
// already loaded passes; else modprobe's dry run must find it. A host
// without modprobe is left to the rollout's own verdict.
func workspacesHostPreflight() error {
	for _, module := range []string{"nfsd", "nfs"} {
		if _, err := os.Stat("/sys/module/" + module); err == nil {
			continue
		}
		modprobe, err := exec.LookPath("modprobe")
		if err != nil {
			return nil
		}
		if out, err := exec.Command(modprobe, "-n", "-q", module).CombinedOutput(); err != nil { // #nosec G204 -- modprobe from PATH with the two fixed module names
			return fmt.Errorf("the host kernel has no %s module (%v%s): the lab's NFS server and the driver's mounts run on the host's kernel; install the kernel's NFS modules (on Debian and Ubuntu the kernel's `linux-modules-extra`, nfs-kernel-server brings the tools) or reboot into a kernel that has them", module, err, strings.TrimSpace(" "+string(out)))
		}
	}
	return nil
}

// waitWorkspacesProxy waits for the driver's controller after the platform
// install: its mTLS proxy's serving certificate is a PodCertificateRequest
// the service-DNS signer of Substrate's podcertificate-controller answers,
// and its client trust the pod-identity CA's live ClusterTrustBundle —
// neither exists before the chart, and the kubelet holds the pod until they
// do.
func waitWorkspacesProxy(ctx context.Context) error {
	step("Waiting for the workspace storage's controller and its mTLS proxy (their certificate from Substrate's service-DNS signer)")
	if err := waitDeploymentRolledOut(ctx, workspacesNamespace, workspacesController, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the NFS CSI controller: %w (the signer issues a certificate only to a pod the Service %s selects; `agentlab pods -n %s`, `kubectl -n %s describe deploy/%s`)",
			err, workspacesControllerService, workspacesNamespace, workspacesNamespace, workspacesController)
	}
	note("controller Ready: its proxy serves %s:%d with the certificate of %s, admitting %s alone",
		workspacesControllerService, workspacesControllerPort, workspacesControllerServerName(), ateAPIServerSPIFFEID)
	return nil
}

// workspacesCSIDriverConfig is the CSIDriverConfig that registers the lab's
// driver with Substrate, as the chart's workspaces.substrate.csiDriver
// values render it: the lab applies it while the chart cannot.
func workspacesCSIDriverConfig() *unstructured.Unstructured {
	v := workspacesValuesFor(config.Default())
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion("ate.dev/v1alpha1")
	u.SetKind("CSIDriverConfig")
	u.SetName(workspacesCSIDriver)
	u.SetLabels(map[string]string{managedByLabel: managedByAgentlabValue})
	u.Object[crSpec] = map[string]any{
		"driverName":         workspacesCSIDriver,
		"controllerEndpoint": v.ControllerEndpoint,
		"nodeSocketOverride": v.NodeSocket,
		"tls": map[string]any{
			"enabled":        true,
			"usePodIdentity": true,
			"serverName":     v.ServerName,
		},
	}
	return u
}

// ensureWorkspacesCSIDriverConfig applies the lab's CSIDriverConfig after
// the install, on a chart that does not render one; the CRD comes with the
// chart's substrate-crds component. A chart that carries the key renders
// the object itself from the lab's values: nothing to apply.
func ensureWorkspacesCSIDriverConfig(ctx context.Context, chartCarries bool) error {
	if chartCarries {
		return nil
	}
	if _, err := gvrFor(csiDriverConfigResource); err != nil {
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
	if obj.GetLabels()[managedByLabel] != managedByAgentlabValue {
		return nil
	}
	note("removing the CSIDriverConfig %s the lab applied", workspacesCSIDriver)
	return deleteObject(ctx, gvr, "", workspacesCSIDriver, fixtureDeleteWait)
}

// The cluster-scoped objects of the workspace storage, as kubectl resource
// arguments and names: what workspacesDown removes after the namespace.
var workspacesClusterObjects = []struct{ resource, name string }{
	{"storageclasses.storage.k8s.io", workspacesStorageClass},
	{"csidrivers.storage.k8s.io", workspacesCSIDriver},
	{"clusterrolebindings.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-provisioner"},
	{"clusterroles.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-provisioner"},
	{"clusterrolebindings.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-resizer"},
	{"clusterroles.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-resizer"},
}

// workspacesDown removes the workspace storage from a lab that has it: the
// lab's CSIDriverConfig, the namespace with everything in it and the
// cluster-scoped objects, so a lab with the switch off carries nothing of
// it. Quiet on a lab that never had it. A volume a pod still mounts keeps
// the namespace Terminating until the pod is gone: `agentlab
// workspaces-test` removes its own. The export's bytes on the node go with
// cleanWorkspacesNode.
func workspacesDown(ctx context.Context) error {
	exists, err := objectExists(ctx, gvrNamespaces, "", workspacesNamespace)
	if err != nil {
		return err
	}
	if !exists && !clusterObjectExists(ctx, "storageclasses.storage.k8s.io", workspacesStorageClass) {
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
			continue
		}
		if err := deleteObject(ctx, gvr, "", o.name, fixtureDeleteWait); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("removing %s %s: %w", o.resource, o.name, err)
		}
	}
	note("workspace storage removed: namespace %s, the StorageClass, the CSIDriver and the driver's roles", workspacesNamespace)
	return nil
}

// cleanWorkspacesNode unmounts what the driver published under Substrate's
// directory on a node and removes the export's bytes — on `agentlab down`
// before the node is deleted (a mount the node still holds can keep the
// container from being removed), on `agentlab platform-down` and when the
// switch goes off. Best effort through `docker exec`; a node that is not
// running, or has no export, has nothing to clean.
func cleanWorkspacesNode(node string) {
	script := fmt.Sprintf(`for m in $(awk '$2 ~ "^%s/" {print $2}' /proc/mounts | sort -r); do umount -f "$m" 2>/dev/null; done; rm -rf %s 2>/dev/null; true`,
		substrateVolumesDir, workspacesExportDir)
	_, _ = outputQuiet(dockerBin, "exec", node, "sh", "-c", script)
}

// waitDaemonSetReady is `kubectl rollout status daemonset/<name>`: every
// scheduled pod of the current generation Ready, bounded by timeout; the
// last read's state is in the error.
func waitDaemonSetReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	var last *appsv1.DaemonSet
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		ds, err := k.clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		last = ds
		return daemonSetReady(ds), nil
	}); err != nil {
		return fmt.Errorf("daemonset %s/%s is not ready after %s (%s): `agentlab pods -n %s`", ns, name, timeout, daemonSetStatus(last), ns)
	}
	return nil
}

// daemonSetReady reports every desired pod of the current generation
// scheduled, updated and Ready.
func daemonSetReady(ds *appsv1.DaemonSet) bool {
	if ds.Generation > ds.Status.ObservedGeneration {
		return false
	}
	desired := ds.Status.DesiredNumberScheduled
	return desired > 0 && ds.Status.UpdatedNumberScheduled >= desired && ds.Status.NumberReady >= desired
}

// daemonSetStatus words a DaemonSet's rollout state.
func daemonSetStatus(ds *appsv1.DaemonSet) string {
	if ds == nil {
		return stateNotRead
	}
	return fmt.Sprintf("%d of %d pods ready, %d updated", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled, ds.Status.UpdatedNumberScheduled)
}

// WorkspacesStatus is the workspace storage as `agentlab status` reports it.
type WorkspacesStatus struct {
	// NFSServer, Controller and NodePlugin are the workloads' rollout states
	// in a word: Ready, or what is not. The controller carries the mTLS
	// proxy.
	NFSServer  string `json:"nfsServer"`
	Controller string `json:"controller"`
	NodePlugin string `json:"nodePlugin"`
	// Node is the node the server and the controller run on.
	Node string `json:"node,omitempty"`
	// StorageClass reports whether the class exists.
	StorageClass bool `json:"storageClass"`
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
	for _, w := range []struct {
		name string
		into *string
	}{{workspacesNFSServer, &s.NFSServer}, {workspacesController, &s.Controller}} {
		d, err := k.clientset.AppsV1().Deployments(workspacesNamespace).Get(ctx, w.name, metav1.GetOptions{})
		switch {
		case err != nil:
			*w.into = readState(err)
		default:
			if _, done, _ := deploymentRolloutStatus(d); done {
				*w.into = conditionReady
			} else {
				*w.into = fmt.Sprintf("%d of %d replicas available", d.Status.AvailableReplicas, d.Status.Replicas)
			}
			s.Node = d.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"]
		}
	}
	ds, err := k.clientset.AppsV1().DaemonSets(workspacesNamespace).Get(ctx, workspacesNodePlugin, metav1.GetOptions{})
	switch {
	case err != nil:
		s.NodePlugin = readState(err)
	case daemonSetReady(ds):
		s.NodePlugin = fmt.Sprintf("%s on %d node(s)", conditionReady, ds.Status.NumberReady)
	default:
		s.NodePlugin = daemonSetStatus(ds)
	}
	s.StorageClass = clusterObjectExists(ctx, "storageclasses.storage.k8s.io", workspacesStorageClass)
	if gvr, err := gvrFor(csiDriverConfigResource); err == nil {
		if obj, err := getObject(ctx, gvr, "", workspacesCSIDriver); err == nil {
			s.CSIDriverConfig = "the chart"
			if obj.GetLabels()[managedByLabel] == managedByAgentlabValue {
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
	class := "no StorageClass"
	if s.StorageClass {
		class = "StorageClass " + workspacesStorageClass
	}
	registered := "not registered with Substrate (no CSIDriverConfig)"
	if s.CSIDriverConfig != "" {
		registered = "CSIDriverConfig " + workspacesCSIDriver + " by " + s.CSIDriverConfig
	}
	return fmt.Sprintf("NFS server %s on %s; CSI controller %s %s with its mTLS proxy; node plugin %s; %s; %s",
		s.NFSServer, orNone(s.Node), workspacesCSIDriver, s.Controller, s.NodePlugin, class, registered)
}

// workspacesHint is the platform-up summary for the workspaces switch.
func workspacesHint(cfg *config.Config, chartCarries bool) string {
	if !cfg.WorkspacesEnabled() {
		return "  Workspace storage is off (platform.workspaces in agentlab.yaml; `agentlab configure --workspaces` turns it on)."
	}
	registered := "registered with Substrate by the lab's CSIDriverConfig (the chart carries no workspaces values yet)"
	if chartCarries {
		registered = "registered with Substrate by the chart's CSIDriverConfig (the lab's workspaces values)"
	}
	hint := fmt.Sprintf("  Workspace storage: an NFS server on %s and the NFS CSI driver behind its mTLS proxy, %s;\n"+
		"  the read-write-many StorageClass %s. Proof: `agentlab workspaces-test --storage-only`.", WorkspacesNode(cfg), registered, workspacesStorageClass)
	if chartCarries {
		hint += fmt.Sprintf("\n  The workspace-manager's provider instances: %s (the base URL %s; `agentlab platform-test` lists them as the admin).",
			strings.Join(cfg.WorkspaceProviders(), ", "), cfg.WorkspaceManagerBaseURL())
		if cfg.WorkspaceFakeProvider() {
			hint += fmt.Sprintf("\n  The lab's GitHub serves the instance %s at %s, its credentials the lab's own.", config.WorkspaceProviderFake, cfg.GitHubFakeURL())
		}
	}
	return hint
}
