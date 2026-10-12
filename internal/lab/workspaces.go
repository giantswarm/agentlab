package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
// (chartCarriesWorkspaces). Taking the key is not registering the driver:
// whether the chart renders the CSIDriverConfig that registers it with
// Substrate is read off the component renders the install is about to
// apply (rendersWorkspacesCSIDriverConfig). While none does, the lab applies
// that one object itself after the install (ensureWorkspacesCSIDriverConfig);
// the lab's copy goes before the install of a chart whose render carries
// its own. docs/workspaces.md is the human account.
//
// With platform.workspaces.csiCompanion the chart's CSI companion
// (workspaces.substrate.csiCompanion of the connectivity chart) is
// Substrate's driver instead: the path that proves a companion chart change
// in the lab. The companion stands in for an installation's file driver
// through one of its presets, and the CSIDriverConfig it derives names that
// preset's driver, which Substrate mounts a volume with only when the volume
// is that driver's. So the lab's NFS driver serves the volumes under the efs
// preset's driver name (csi-driver-nfs's --drivername), and the companion
// runs the efs preset with the lab's NFS plugin image and arguments in place
// of the EFS driver, which crashloops without EFS: the companion's own
// controller behind its mTLS proxy, its node plugin on the Substrate workers
// and its registration, all from the chart's templates, mount the lab's
// NFS volumes (companionValuesFor).

const (
	// workspacesNamespace holds the NFS server and the driver.
	workspacesNamespace = "agentlab-workspaces"
	// workspacesStorageClass is the read-write-many class the chart's
	// workspaces values name.
	workspacesStorageClass = "agentlab-workspaces"
	// workspacesCSIDriver is the driver's name: csi-driver-nfs's.
	workspacesCSIDriver = "nfs.csi.k8s.io"
	// companionCSIDriver is the driver of the CSI companion's preset
	// companionPreset, the name the lab's NFS driver serves the volumes
	// under while the companion is Substrate's driver.
	companionCSIDriver = "efs.csi.aws.com"
	companionPreset    = "efs"
	// companionController and companionNode are the companion's Deployment
	// and DaemonSet in the connectivity release's namespace.
	companionController = "csi-companion-controller"
	companionNode       = "csi-companion-node"
	// companionProxyPort is the companion's mTLS proxy listener, the
	// chart's default.
	companionProxyPort = 10000
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
	// csiDriversResource is the cluster's registration of a CSI driver.
	csiDriversResource = "csidrivers.storage.k8s.io"
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
	// nfsPluginRepository is nfsPluginImage's repository on gsoci, the
	// companion's image.repository.
	nfsPluginRepository = "giantswarm/nfsplugin"
)

// workspacesCSIDrivers are the names the lab's NFS driver serves volumes
// under: its own, and the companion preset's.
var workspacesCSIDrivers = []string{workspacesCSIDriver, companionCSIDriver}

// isWorkspacesCSIDriver reports whether a CSI driver name is one the lab's
// NFS driver serves volumes under.
func isWorkspacesCSIDriver(name string) bool {
	return slices.Contains(workspacesCSIDrivers, name)
}

// workspacesCSIDriverFor is the name the lab's NFS driver serves the
// volumes under: the companion preset's driver while the companion is
// Substrate's driver, csi-driver-nfs's own otherwise.
func workspacesCSIDriverFor(cfg *config.Config) string {
	if cfg.WorkspacesCSICompanion() {
		return companionCSIDriver
	}
	return workspacesCSIDriver
}

// companionValues are the connectivity chart's
// workspaces.substrate.csiCompanion values the lab renders: the efs preset
// with the lab's NFS plugin in place of the EFS driver, csi-driver-nfs's
// arguments in each mode under the preset's driver name. The preset's
// environment brings the node's name to both pods (CSI_NODE_NAME in the
// controller, KUBE_NODE_NAME in the node plugin); the socket is the
// chart's CSI_ENDPOINT.
type companionValues struct {
	Preset, ImageRepository, ImageTag string
	ControllerArgs, NodeArgs          []string
}

func companionValuesFor() companionValues {
	drivername := "--drivername=" + companionCSIDriver
	return companionValues{
		Preset:          companionPreset,
		ImageRepository: nfsPluginRepository,
		ImageTag:        workspacesDriverVersion,
		ControllerArgs:  []string{"--v=2", "--nodeid=$(CSI_NODE_NAME)", "--endpoint=$(CSI_ENDPOINT)", drivername},
		NodeArgs:        []string{"--v=2", "--nodeid=$(KUBE_NODE_NAME)", "--endpoint=$(CSI_ENDPOINT)", drivername},
	}
}

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
	// Organization and OrganizationGroups are the lab's workspace-manager
	// Organization and its member groups (config.WorkspaceOrganization).
	Organization       string
	OrganizationGroups []string
	// Companion is the chart's CSI companion as Substrate's driver
	// (platform.workspaces.csiCompanion); nil while the lab registers its
	// own driver.
	Companion *companionValues
}

func workspacesValuesFor(cfg *config.Config) workspacesValues {
	return workspacesValues{
		Namespace:          workspacesNamespace,
		Driver:             workspacesCSIDriverFor(cfg),
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
		Organization:       config.WorkspaceOrganization,
		OrganizationGroups: config.WorkspaceOrganizationGroups,
		Companion:          companionFor(cfg),
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

// companionFor is the companion's values while it is Substrate's driver.
func companionFor(cfg *config.Config) *companionValues {
	if !cfg.WorkspacesCSICompanion() {
		return nil
	}
	v := companionValuesFor()
	return &v
}

// substrateDriverEndpoint is the controller endpoint Substrate's
// registration names, as the proof reaches its mTLS proxy: the proxy's pod
// (namespace, label selector, the controller's name), its listener and the
// TLS server name the registration verifies.
type substrateDriverEndpoint struct {
	namespace, selector, controller, serverName string
	proxyPort                                   int
}

// substrateDriverEndpointFor is the lab's own controller behind its proxy,
// or the chart's CSI companion's (its Service <name>.<namespace>.svc, the
// chart's default proxy port).
func substrateDriverEndpointFor(cfg *config.Config) substrateDriverEndpoint {
	if cfg.WorkspacesCSICompanion() {
		return substrateDriverEndpoint{
			namespace:  platformNamespace,
			selector:   "app.kubernetes.io/name=csi-companion,app.kubernetes.io/component=controller",
			controller: companionController,
			serverName: companionController + "." + platformNamespace + ".svc",
			proxyPort:  companionProxyPort,
		}
	}
	return substrateDriverEndpoint{
		namespace:  workspacesNamespace,
		selector:   "app.kubernetes.io/name=" + workspacesController,
		controller: workspacesController,
		serverName: workspacesControllerServerName(),
		proxyPort:  workspacesProxyPort,
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

// chartWorkspaces is what the meta chart about to be installed takes of the
// lab's workspaces values: Carries, a `workspaces` key — its values schema
// names it at the root (the schema's root refuses unknown keys, so a block
// on a chart without it fails the install at render), or its default values
// carry it; Organizations, the list `workspaces.organizations` beneath it,
// which the chart forwards as the workspace-manager's organizations map and
// from which it refuses a `workspace-manager.organizations` of its own (from
// agent-platform 4.127.0). A chart that carries the key without the list
// takes the map on the subchart.
// CSICompanion, the CSI companion's block `workspaces.substrate.csiCompanion`
// (agent-platform 4.128.0 and newer), which platform.workspaces.csiCompanion
// needs.
type chartWorkspaces struct {
	Carries, Organizations, CSICompanion bool
}

// chartWorkspacesOf probes the chart once: memoized per chart, since a
// registry chart is pulled once per digest; a test points
// chartWorkspacesProbe at a stub.
func chartWorkspacesOf(chart platformChart) (chartWorkspaces, error) {
	chartWorkspacesMu.Lock()
	defer chartWorkspacesMu.Unlock()
	key := chart.String()
	if w, ok := chartWorkspacesCache[key]; ok {
		return w, nil
	}
	w, err := chartWorkspacesProbe(chart)
	if err != nil {
		return chartWorkspaces{}, err
	}
	chartWorkspacesCache[key] = w
	return w, nil
}

var (
	chartWorkspacesMu    sync.Mutex
	chartWorkspacesCache = map[string]chartWorkspaces{}
	// chartWorkspacesProbe loads the chart and reads its schema and values;
	// the tests replace it.
	chartWorkspacesProbe = func(chart platformChart) (chartWorkspaces, error) {
		c, err := loadPlatformChart(chart)
		if err != nil {
			return chartWorkspaces{}, err
		}
		if c == nil {
			return chartWorkspaces{}, nil
		}
		return chartWorkspaces{
			Carries:       chartValuesCarry(c.Schema, c.Values, valuesWorkspaces),
			Organizations: chartValuesCarry(c.Schema, c.Values, valuesWorkspaces, valuesWorkspacesOrganizations),
			CSICompanion:  chartValuesCarry(c.Schema, c.Values, valuesWorkspaces, valuesWorkspacesSubstrate, valuesWorkspacesCSICompanion),
		}, nil
	}
)

// valuesWorkspaces is the chart's workspaces key, valuesWorkspacesOrganizations
// the Organizations' list beneath it, valuesWorkspacesSubstrate and
// valuesWorkspacesCSICompanion the path of the CSI companion's block.
const (
	valuesWorkspaces              = "workspaces"
	valuesWorkspacesOrganizations = "organizations"
	valuesWorkspacesSubstrate     = "substrate"
	valuesWorkspacesCSICompanion  = "csiCompanion"
)

// workspacesChartFor is chartWorkspacesOf for the configured chart when the
// switch is on, nothing otherwise; a chart that cannot be probed is a note
// and nothing, so the lab registers the driver with Substrate itself.
func workspacesChartFor(cfg *config.Config) chartWorkspaces {
	if !cfg.WorkspacesEnabled() {
		return chartWorkspaces{}
	}
	w, err := chartWorkspacesOf(platformChartFor(cfg))
	if err != nil {
		note("the platform chart's workspaces values could not be probed (%v): the lab registers the driver with Substrate itself", err)
		return chartWorkspaces{}
	}
	return w
}

// checkWorkspacesCSICompanion refuses platform.workspaces.csiCompanion on a
// chart without the companion's values, before anything of the storage
// changes: the schema's root would refuse the block at render.
func checkWorkspacesCSICompanion(cfg *config.Config) error {
	if !cfg.WorkspacesCSICompanion() || workspacesChartFor(cfg).CSICompanion {
		return nil
	}
	return fmt.Errorf("platform.workspaces.csiCompanion is on but the platform chart %s carries no workspaces.substrate.csiCompanion (agent-platform 4.128.0 and newer): pin a chart that does (platform.chartVersion or platform.chartPath), or `agentlab configure --workspaces-csi-companion=false`", platformChartFor(cfg))
}

// workspacesChartCarries is whether the configured chart takes the lab's
// workspaces block (workspacesChartFor).
func workspacesChartCarries(cfg *config.Config) bool {
	return workspacesChartFor(cfg).Carries
}

// chartValuesCarry reports whether a chart's values schema names the key
// path (each key a property of the one before, from the root), or its
// default values carry it.
func chartValuesCarry(schema []byte, values map[string]any, path ...string) bool {
	if valuesCarry(values, path) {
		return true
	}
	if len(schema) == 0 {
		return false
	}
	raw := json.RawMessage(schema)
	for _, key := range path {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return false
		}
		next, ok := s.Properties[key]
		if !ok {
			return false
		}
		raw = next
	}
	return true
}

// valuesCarry reports whether values hold the key path, each key a mapping's.
func valuesCarry(values map[string]any, path []string) bool {
	for i, key := range path {
		v, ok := values[key]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		if values, ok = v.(map[string]any); !ok {
			return false
		}
	}
	return false
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
	driver := workspacesCSIDriverFor(cfg)
	step("Installing the workspace storage (an NFS server on %s, the NFS CSI driver %s as %s behind its mTLS proxy, the read-write-many StorageClass %s)", node, workspacesDriverVersion, driver, workspacesStorageClass)
	if err := workspacesHostPreflight(); err != nil {
		return err
	}
	// A class of the other driver name (platform.workspaces.csiCompanion
	// switched) cannot be changed in place: a StorageClass's provisioner is
	// immutable, and the volumes are the other driver's. The storage goes
	// in the one order first (teardown.go), its volumes with it.
	if live := liveWorkspacesCSIDriver(ctx); live != "" && live != driver {
		note("the workspace storage serves its volumes as %s, the configuration as %s: removing it with its volumes first", live, driver)
		if err := workspacesTeardown(ctx, cfg); err != nil {
			return err
		}
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
		workspacesExportDir, node, driver, substrateVolumesDir, workspacesStorageClass)
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
func waitWorkspacesProxy(ctx context.Context, cfg *config.Config) error {
	step("Waiting for the workspace storage's controller and its mTLS proxy (their certificate from Substrate's service-DNS signer)")
	if err := waitDeploymentRolledOut(ctx, workspacesNamespace, workspacesController, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the NFS CSI controller: %w (the signer issues a certificate only to a pod the Service %s selects; `agentlab pods -n %s`, `kubectl -n %s describe deploy/%s`)",
			err, workspacesControllerService, workspacesNamespace, workspacesNamespace, workspacesController)
	}
	note("controller Ready: its proxy serves %s:%d with the certificate of %s, admitting %s alone",
		workspacesControllerService, workspacesControllerPort, workspacesControllerServerName(), ateAPIServerSPIFFEID)
	if cfg.WorkspacesCSICompanion() {
		return waitWorkspacesCompanion(ctx)
	}
	return nil
}

// waitWorkspacesCompanion waits for the chart's CSI companion: its
// controller behind the mTLS proxy and its node plugin on every Substrate
// worker, both running the lab's NFS plugin. helm-controller's readiness
// wait has passed them already; this names them in the summary, and a
// release that rolled back without them fails here.
func waitWorkspacesCompanion(ctx context.Context) error {
	step("Waiting for the chart's CSI companion (%s preset, the lab's NFS plugin as %s)", companionPreset, companionCSIDriver)
	if err := waitDeploymentRolledOut(ctx, platformNamespace, companionController, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the CSI companion's controller: %w (`kubectl -n %s logs deploy/%s --all-containers`)", err, platformNamespace, companionController)
	}
	if err := waitDaemonSetReady(ctx, platformNamespace, companionNode, workspacesRolloutTimeout); err != nil {
		return fmt.Errorf("the CSI companion's node plugin: %w", err)
	}
	note("CSI companion Ready: controller %s and node plugin %s in %s; the chart registers %s with Substrate", companionController, companionNode, platformNamespace, companionCSIDriver)
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

// rendersWorkspacesCSIDriverConfig reports whether the component renders the
// install is about to apply (keyed by release name, platformImages) carry a
// CSIDriverConfig of the driver: the chart registers it with Substrate
// itself. A chart can take the workspaces values and render none from them,
// so the values schema does not decide it; no renders (the offline render
// failed) is none, and the lab keeps its own.
func rendersWorkspacesCSIDriverConfig(renders map[string]string, driver string) bool {
	for _, rendered := range renders {
		dec := yaml.NewDecoder(strings.NewReader(rendered))
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
			}
			// The end of the render, or a document Helm rendered that the
			// decoder cannot read, ends this render's scan.
			if err := dec.Decode(&doc); err != nil {
				break
			}
			if doc.Kind == kindCSIDriverConfig && doc.Metadata.Name == driver {
				return true
			}
		}
	}
	return false
}

// kindCSIDriverConfig is Substrate's driver registration kind.
const kindCSIDriverConfig = "CSIDriverConfig"

// ensureWorkspacesCSIDriverConfig applies the lab's CSIDriverConfig after
// the install while the chart renders none (rendersWorkspacesCSIDriverConfig);
// the CRD comes with the chart's substrate-crds component. A chart that
// renders one registers the driver from the lab's values: nothing to apply.
// A Helm-managed object live while the renders carry none is an earlier
// chart's that helm-controller's upgrade is about to remove: the lab waits
// for it to go (an apply over it would go with it) and applies its own; one
// that outlasts the wait stays the chart's. One applied by hand becomes the
// lab's.
//
// The companion is registered by the chart alone: its renders without the
// registration are refused, since the lab's own would name the other
// driver.
func ensureWorkspacesCSIDriverConfig(ctx context.Context, cfg *config.Config, chartRenders bool) error {
	if chartRenders {
		return nil
	}
	if cfg.WorkspacesCSICompanion() {
		return fmt.Errorf("platform.workspaces.csiCompanion is on but the chart renders no CSIDriverConfig %s: the companion's registration is the chart's (workspaces.substrate.csiCompanion with the substrate component on)", companionCSIDriver)
	}
	gvr, err := gvrFor(csiDriverConfigResource)
	if err != nil {
		return fmt.Errorf("the CSIDriverConfig API of Substrate is not served (%v): the agents runtime brings it; is platform.agents on and the substrate-crds component installed?", err)
	}
	if obj, err := getObject(ctx, gvr, "", workspacesCSIDriver); err == nil && csiDriverConfigRegistrant(obj) == registrantChart {
		if !waitChartCSIDriverConfigGone(ctx, gvr, csiDriverConfigHandoverTimeout) {
			note("CSIDriverConfig %s is the chart's and stayed through %s although the chart renders none: the lab applies none", workspacesCSIDriver, csiDriverConfigHandoverTimeout)
			return nil
		}
		note("the earlier chart's CSIDriverConfig %s is gone with its upgrade", workspacesCSIDriver)
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	res, err := k.apply(ctx, workspacesCSIDriverConfig())
	if err != nil {
		return fmt.Errorf("the CSIDriverConfig %s: %w", workspacesCSIDriver, err)
	}
	note("%s (the chart renders no CSIDriverConfig for %s, so the lab registers the driver with Substrate itself: controllerEndpoint %s, tls.serverName %s)",
		res, workspacesCSIDriver, workspacesValuesFor(config.Default()).ControllerEndpoint, workspacesControllerServerName())
	return nil
}

// csiDriverConfigHandoverTimeout bounds the wait for an earlier chart's
// CSIDriverConfig to go: helm-controller's upgrade of the connectivity
// release, bounded by its action timeout. A variable so tests shorten it.
var csiDriverConfigHandoverTimeout = fluxActionTimeout

// waitChartCSIDriverConfigGone waits until no Helm-managed CSIDriverConfig
// of the driver is live; false when one still is after timeout.
func waitChartCSIDriverConfigGone(ctx context.Context, gvr schema.GroupVersionResource, timeout time.Duration) bool {
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		obj, err := getObject(ctx, gvr, "", workspacesCSIDriver)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return err == nil && csiDriverConfigRegistrant(obj) != registrantChart, nil
	})
	return err == nil
}

// Who registered the driver with Substrate, as status and the platform
// summary word it.
const (
	registrantLab   = "the lab"
	registrantChart = "the chart"
	registrantHand  = "hand"
)

// csiDriverConfigRegistrant words who manages a live CSIDriverConfig: the
// lab's label, Helm's (a chart's release), or neither (applied by hand).
func csiDriverConfigRegistrant(obj *unstructured.Unstructured) string {
	switch obj.GetLabels()[managedByLabel] {
	case managedByAgentlabValue:
		return registrantLab
	case "Helm":
		return registrantChart
	default:
		return registrantHand
	}
}

// readWorkspacesRegistrant reads who registered the driver with Substrate:
// a registrant word, or "" when no CSIDriverConfig of it exists or the API
// is not served.
func readWorkspacesRegistrant(ctx context.Context, driver string) string {
	gvr, err := gvrFor(csiDriverConfigResource)
	if err != nil {
		return ""
	}
	obj, err := getObject(ctx, gvr, "", driver)
	if err != nil {
		return ""
	}
	return csiDriverConfigRegistrant(obj)
}

// workspacesRegistration words the driver's registration with Substrate,
// one line for `agentlab status` and the platform summary alike.
func workspacesRegistration(driver, registrant string) string {
	if registrant == "" {
		return "not registered with Substrate (no CSIDriverConfig)"
	}
	return "registered with Substrate (CSIDriverConfig " + driver + " by " + registrant + ")"
}

// liveWorkspacesCSIDriver is the driver the lab's StorageClass provisions
// with, read off the cluster: what the volumes are served as. "" without
// the class.
func liveWorkspacesCSIDriver(ctx context.Context) string {
	k, err := labKube()
	if err != nil {
		return ""
	}
	sc, err := k.clientset.StorageV1().StorageClasses().Get(ctx, workspacesStorageClass, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return sc.Provisioner
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
	{csiDriversResource, workspacesCSIDriver},
	{csiDriversResource, companionCSIDriver},
	{"clusterrolebindings.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-provisioner"},
	{"clusterroles.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-provisioner"},
	{"clusterrolebindings.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-resizer"},
	{"clusterroles.rbac.authorization.k8s.io", "agentlab-workspaces-csi-nfs-resizer"},
}

// workspacesDown removes the workspace storage from a lab that has it: the
// lab's CSIDriverConfig, the namespace with everything in it and the
// cluster-scoped objects, so a lab with the switch off carries nothing of
// it. Quiet on a lab that never had it. A volume a pod still mounts keeps
// the namespace Terminating until the pod is gone, and a node still
// mounting the server once it stops cannot be stopped: callers run it as a
// step of workspacesTeardown (teardown.go), after the volumes are released.
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
	// Driver is the name the class provisions the volumes with: the lab's
	// NFS driver as itself, or as the CSI companion's preset driver.
	Driver string `json:"driver,omitempty"`
	// Companion is the chart's CSI companion in a word when it runs
	// (platform.workspaces.csiCompanion): Ready, or what is not.
	Companion string `json:"companion,omitempty"`
	// CSIDriverConfig says who registered the driver with Substrate: "the
	// lab", "the chart", "hand", or "" when nothing did.
	CSIDriverConfig string `json:"csiDriverConfig,omitempty"`
	// SessionAdmission words the rig a Session on a workspace volume is
	// admitted with (workspacegrant.go): kagent's jwt admission, the
	// manager's grant key and the claude Harness.
	SessionAdmission string `json:"sessionAdmission,omitempty"`
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
	s.Driver = liveWorkspacesCSIDriver(ctx)
	s.StorageClass = s.Driver != ""
	s.Companion = readCompanionState(ctx, k)
	s.CSIDriverConfig = readWorkspacesRegistrant(ctx, cmp.Or(s.Driver, workspacesCSIDriver))
	s.SessionAdmission = readSessionAdmission(ctx)
	return s, nil
}

// readCompanionState words the chart's CSI companion: Ready when its
// controller is rolled out and its node plugin Ready, else what is not; ""
// when no companion runs.
func readCompanionState(ctx context.Context, k *kubeClients) string {
	d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, companionController, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return ""
	}
	if err != nil {
		return readState(err)
	}
	if _, done, _ := deploymentRolloutStatus(d); !done {
		return fmt.Sprintf("controller %d of %d replicas available", d.Status.AvailableReplicas, d.Status.Replicas)
	}
	ds, err := k.clientset.AppsV1().DaemonSets(platformNamespace).Get(ctx, companionNode, metav1.GetOptions{})
	switch {
	case err != nil:
		return "node plugin " + readState(err)
	case !daemonSetReady(ds):
		return "node plugin " + daemonSetStatus(ds)
	}
	return conditionReady
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
	driver := cmp.Or(s.Driver, workspacesCSIDriver)
	line := fmt.Sprintf("NFS server %s on %s; CSI controller %s %s with its mTLS proxy; node plugin %s; %s; %s",
		s.NFSServer, orNone(s.Node), driver, s.Controller, s.NodePlugin, class, workspacesRegistration(driver, s.CSIDriverConfig))
	if s.Companion != "" {
		line += fmt.Sprintf("; the chart's CSI companion (%s preset, the lab's NFS plugin) %s", companionPreset, s.Companion)
	}
	if s.SessionAdmission != "" {
		line += "; " + s.SessionAdmission
	}
	return line
}

// workspacesHint is the platform-up summary for the workspaces switch. The
// registration is the live one (readWorkspacesRegistrant), worded as
// `agentlab status` words it; chartCarries is whether the chart takes the
// workspaces values, which bring the workspace-manager's providers.
func workspacesHint(cfg *config.Config, chartCarries bool, registrant string) string {
	if !cfg.WorkspacesEnabled() {
		return "  Workspace storage is off (platform.workspaces in agentlab.yaml; `agentlab configure --workspaces` turns it on)."
	}
	hint := fmt.Sprintf("  Workspace storage: an NFS server on %s and the NFS CSI driver behind its mTLS proxy, %s;\n"+
		"  the read-write-many StorageClass %s. Proof: `agentlab workspaces-test --storage-only`.", WorkspacesNode(cfg), workspacesRegistration(workspacesCSIDriverFor(cfg), registrant), workspacesStorageClass)
	if cfg.WorkspacesCSICompanion() {
		hint += fmt.Sprintf("\n  Substrate's driver is the chart's CSI companion (%s preset, the lab's NFS plugin serving %s). Proof: `agentlab workspaces-test --storage-only`, its actors mounting through the companion.", companionPreset, companionCSIDriver)
	}
	if rig := workspaceAdmissionValuesFor(cfg, chartCarries); rig != nil {
		hint += "\n" + rig.hint()
	}
	if chartCarries {
		hint += fmt.Sprintf("\n  The workspace-manager's provider instances: %s (the base URL %s; `agentlab platform-test` lists them as the admin).",
			strings.Join(cfg.WorkspaceProviders(), ", "), cfg.WorkspaceManagerBaseURL())
		if cfg.WorkspaceFakeProvider() {
			hint += fmt.Sprintf("\n  The lab's GitHub serves the instance %s at %s, its credentials the lab's own.", config.WorkspaceProviderFake, cfg.GitHubFakeURL())
		}
	}
	return hint
}
