package lab

import (
	"bytes"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// withRootless fixes rootlessPodmanOnLinux for one test.
func withRootless(t *testing.T, rootless bool) {
	t.Helper()
	prev := rootlessPodmanOnLinux
	rootlessPodmanOnLinux = func() bool { return rootless }
	t.Cleanup(func() { rootlessPodmanOnLinux = prev })
}

// An empty platform.workspaces.storage is hostpath under rootless Podman for
// the single-node lab without the companion, nfs otherwise; a set one wins.
func TestWorkspacesStorageFor(t *testing.T) {
	for _, tc := range []struct {
		name           string
		rootless       bool
		storage        string
		substrateNodes int
		companion      bool
		want           string
	}{
		{"rootful engine", false, "", 0, false, config.WorkspaceStorageNFS},
		{"rootless Podman", true, "", 0, false, config.WorkspaceStorageHostPath},
		{"rootless Podman with workers", true, "", 1, false, config.WorkspaceStorageNFS},
		{"rootless Podman with the companion", true, "", 0, true, config.WorkspaceStorageNFS},
		{"nfs set under rootless Podman", true, config.WorkspaceStorageNFS, 0, false, config.WorkspaceStorageNFS},
		{"hostpath set on a rootful engine", false, config.WorkspaceStorageHostPath, 0, false, config.WorkspaceStorageHostPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRootless(t, tc.rootless)
			cfg := config.Default()
			cfg.Platform.Workspaces.Enabled = true
			cfg.Platform.Workspaces.Storage = tc.storage
			cfg.Platform.Workspaces.CSICompanion = tc.companion
			cfg.SubstrateNodes = tc.substrateNodes
			if got := workspacesStorageFor(cfg); got != tc.want {
				t.Errorf("workspacesStorageFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// The hostpath manifest: no NFS server, the hostpath driver's DaemonSet on
// the control plane with its volumes under the export directory and
// Substrate's directory mounted Bidirectional, the controller Deployment
// the mTLS proxy alone on the driver's socket, the roles bound to the
// DaemonSet's account, and a class without NFS parameters.
func TestWorkspacesManifestHostPath(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Storage = config.WorkspaceStorageHostPath
	out, err := renderTemplate(cfg, workspacesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rendered []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc != nil {
			rendered = append(rendered, doc)
		}
	}
	find := func(kind, name string) map[string]any {
		for _, d := range rendered {
			meta, _ := d["metadata"].(map[string]any)
			if d["kind"] == kind && meta["name"] == name {
				return d
			}
		}
		return nil
	}
	named := func(kind, name string) map[string]any {
		t.Helper()
		d := find(kind, name)
		if d == nil {
			t.Fatalf("no %s %s in the manifest", kind, name)
		}
		return d
	}
	podSpec := func(doc map[string]any) map[string]any {
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		return pod
	}
	containers := func(doc map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		list, _ := podSpec(doc)["containers"].([]any)
		for _, c := range list {
			container, _ := c.(map[string]any)
			name, _ := container["name"].(string)
			out[name] = container
		}
		return out
	}
	hostPaths := func(doc map[string]any) map[string]string {
		out := map[string]string{}
		list, _ := podSpec(doc)["volumes"].([]any)
		for _, v := range list {
			volume, _ := v.(map[string]any)
			name, _ := volume["name"].(string)
			if hp, ok := volume["hostPath"].(map[string]any); ok {
				out[name], _ = hp["path"].(string)
			}
		}
		return out
	}

	for _, name := range []string{workspacesNFSServer, workspacesController} {
		if find("Deployment", name) != nil {
			t.Errorf("Deployment %s rendered with the hostpath storage", name)
		}
	}
	if find("DaemonSet", workspacesNodePlugin) != nil {
		t.Errorf("DaemonSet %s rendered with the hostpath storage", workspacesNodePlugin)
	}

	driver := named("CSIDriver", hostPathCSIDriver)
	if spec, _ := driver["spec"].(map[string]any); spec["attachRequired"] != false {
		t.Errorf("CSIDriver attachRequired = %v, want false (the driver runs without its attach step)", spec["attachRequired"])
	}
	class := named("StorageClass", workspacesStorageClass)
	if class["provisioner"] != hostPathCSIDriver {
		t.Errorf("StorageClass provisioner = %v, want %s", class["provisioner"], hostPathCSIDriver)
	}
	for _, key := range []string{"parameters", "mountOptions"} {
		if v, ok := class[key]; ok {
			t.Errorf("StorageClass %s = %v, want none (a bind mount takes no NFS options)", key, v)
		}
	}

	node := named("DaemonSet", hostPathNodePlugin)
	if selector, _ := podSpec(node)["nodeSelector"].(map[string]any); selector["kubernetes.io/hostname"] != cfg.ControlPlaneNode() {
		t.Errorf("DaemonSet nodeSelector = %v, want the control plane %s", selector, cfg.ControlPlaneNode())
	}
	if account := podSpec(node)["serviceAccountName"]; account != hostPathNodePlugin {
		t.Errorf("DaemonSet serviceAccountName = %v, want %s", account, hostPathNodePlugin)
	}
	nodeContainers := containers(node)
	for _, name := range []string{"hostpath", "csi-provisioner", "csi-resizer", "node-driver-registrar", "liveness-probe"} {
		if nodeContainers[name] == nil {
			t.Errorf("DaemonSet has no container %s", name)
		}
	}
	plugin := nodeContainers["hostpath"]
	if args, _ := plugin["args"].([]any); !slices.Contains(args, any("--drivername="+hostPathCSIDriver)) {
		t.Errorf("hostpath args = %v, want --drivername=%s", args, hostPathCSIDriver)
	}
	mounts, _ := plugin["volumeMounts"].([]any)
	var substrate bool
	for _, m := range mounts {
		mount, _ := m.(map[string]any)
		if mount["mountPath"] == substrateVolumesDir && mount["mountPropagation"] == "Bidirectional" {
			substrate = true
		}
	}
	if !substrate {
		t.Errorf("hostpath mounts %v, want %s Bidirectional", mounts, substrateVolumesDir)
	}
	nodeVolumes := hostPaths(node)
	if nodeVolumes["data-dir"] != workspacesExportDir || nodeVolumes["socket-dir"] != hostPathNodeSocketDir {
		t.Errorf("DaemonSet hostPaths = %v, want data-dir %s and socket-dir %s", nodeVolumes, workspacesExportDir, hostPathNodeSocketDir)
	}

	controller := named("Deployment", hostPathController)
	if c := containers(controller); len(c) != 1 || c[workspacesProxyContainer] == nil {
		t.Errorf("controller containers = %v, want the proxy %s alone", c, workspacesProxyContainer)
	}
	if hostPaths(controller)["socket-dir"] != hostPathNodeSocketDir {
		t.Errorf("controller socket-dir = %v, want the driver's %s", hostPaths(controller)["socket-dir"], hostPathNodeSocketDir)
	}
	service := named("Service", hostPathControllerService)
	if spec, _ := service["spec"].(map[string]any); spec != nil {
		if selector, _ := spec["selector"].(map[string]any); selector["app.kubernetes.io/name"] != hostPathController {
			t.Errorf("Service selector = %v, want the controller %s", selector, hostPathController)
		}
	}
	named("ConfigMap", "csi-hostpath-proxy")

	for _, role := range []string{"provisioner", "resizer"} {
		named("ClusterRole", "agentlab-workspaces-csi-hostpath-"+role)
		binding := named("ClusterRoleBinding", "agentlab-workspaces-csi-hostpath-"+role)
		subjects, _ := binding["subjects"].([]any)
		subject, _ := subjects[0].(map[string]any)
		if subject["name"] != hostPathNodePlugin {
			t.Errorf("ClusterRoleBinding %s subject = %v, want %s", role, subject["name"], hostPathNodePlugin)
		}
	}
	named("ServiceAccount", hostPathNodePlugin)
}

// The hostpath registration with Substrate: the driver, the proxy's Service,
// the driver's socket and the Service's service-DNS name, in the values and
// in the CSIDriverConfig the lab applies.
func TestWorkspacesHostPathRegistration(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Storage = config.WorkspaceStorageHostPath
	v := workspacesValuesFor(cfg)
	want := map[string]string{
		"Driver":             hostPathCSIDriver,
		"ControllerEndpoint": "tcp://csi-hostpath-controller.agentlab-workspaces.svc.cluster.local:50051",
		"NodeSocket":         "unix://" + hostPathNodeSocketDir + "/csi.sock",
		"ServerName":         "csi-hostpath-controller.agentlab-workspaces.svc",
	}
	got := map[string]string{"Driver": v.Driver, "ControllerEndpoint": v.ControllerEndpoint, "NodeSocket": v.NodeSocket, "ServerName": v.ServerName}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
	if !v.HostPath {
		t.Error("HostPath = false, want true")
	}

	u := workspacesCSIDriverConfig(workspacesBackendFor(cfg))
	spec, _ := u.Object[crSpec].(map[string]any)
	if u.GetName() != hostPathCSIDriver || spec["driverName"] != hostPathCSIDriver || spec["controllerEndpoint"] != want["ControllerEndpoint"] || spec["nodeSocketOverride"] != want["NodeSocket"] {
		t.Errorf("CSIDriverConfig %s spec = %v, want the hostpath driver's endpoint and socket", u.GetName(), spec)
	}
	if tls, _ := spec["tls"].(map[string]any); tls["serverName"] != want["ServerName"] {
		t.Errorf("CSIDriverConfig tls = %v, want serverName %s", tls, want["ServerName"])
	}
}

// The hostpath driver names a volume's directory after its volume handle: a
// directory is a live volume's by the handle as by the PersistentVolume's name.
func TestSplitWorkspacesExportDirsVolumeHandle(t *testing.T) {
	pvs := []corev1.PersistentVolume{{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-ws"},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: workspacesStorageClass,
			ClaimRef:         &corev1.ObjectReference{Namespace: "org-a", Name: "ws"},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: hostPathCSIDriver, VolumeHandle: "6c1f0e9a-live"},
			},
		},
	}}
	live, left := splitWorkspacesExportDirs([]string{"6c1f0e9a-live", "0b7d2c4e-gone"}, pvs)
	if len(live) != 1 || live[0] != "6c1f0e9a-live (org-a/ws)" {
		t.Errorf("live = %v, want the handle's directory with its claim", live)
	}
	if len(left) != 1 || left[0] != "0b7d2c4e-gone" {
		t.Errorf("left = %v, want the directory no volume backs", left)
	}
}
