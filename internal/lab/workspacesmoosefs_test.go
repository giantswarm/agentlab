package lab

import (
	"bytes"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// mooseFSManifest renders the workspace storage with the MooseFS storage
// and returns a finder of its objects by kind and name.
func mooseFSManifest(t *testing.T) (*config.Config, func(kind, name string) map[string]any) {
	t.Helper()
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Storage = config.WorkspaceStorageMooseFS
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
	return cfg, func(kind, name string) map[string]any {
		for _, d := range rendered {
			meta, _ := d["metadata"].(map[string]any)
			if d["kind"] == kind && meta["name"] == name {
				return d
			}
		}
		return nil
	}
}

func manifestPodSpec(doc map[string]any) map[string]any {
	spec, _ := doc["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	pod, _ := template["spec"].(map[string]any)
	return pod
}

func manifestContainers(doc map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := manifestPodSpec(doc)["containers"].([]any)
	for _, c := range list {
		container, _ := c.(map[string]any)
		name, _ := container["name"].(string)
		out[name] = container
	}
	return out
}

func manifestHostPaths(doc map[string]any) map[string]string {
	out := map[string]string{}
	list, _ := manifestPodSpec(doc)["volumes"].([]any)
	for _, v := range list {
		volume, _ := v.(map[string]any)
		name, _ := volume["name"].(string)
		if hp, ok := volume["hostPath"].(map[string]any); ok {
			out[name], _ = hp["path"].(string)
		}
	}
	return out
}

// mountedBidirectional reports whether the container mounts path
// Bidirectional.
func mountedBidirectional(container map[string]any, path string) bool {
	mounts, _ := container["volumeMounts"].([]any)
	for _, m := range mounts {
		mount, _ := m.(map[string]any)
		if mount["mountPath"] == path && mount["mountPropagation"] == string(corev1.MountPropagationBidirectional) {
			return true
		}
	}
	return false
}

// The MooseFS manifest: no NFS server or hostpath driver; the master, its
// metadata seeded and kept on the control plane, and the chunkserver
// dialling the master's Service; the controller pod with the provisioner,
// the resizer, the driver (its filesystem mount Bidirectional under
// mooseFSDir) and the proxy; the node plugin with the pods directory and
// Substrate's mounted Bidirectional; both plugins given the master's address
// as flags; the roles bound to the controller's account; a class without
// NFS parameters.
func TestWorkspacesManifestMooseFS(t *testing.T) {
	cfg, find := mooseFSManifest(t)
	named := func(kind, name string) map[string]any {
		t.Helper()
		d := find(kind, name)
		if d == nil {
			t.Fatalf("no %s %s in the manifest", kind, name)
		}
		return d
	}
	for _, name := range []string{workspacesNFSServer, workspacesController, hostPathController} {
		if find("Deployment", name) != nil {
			t.Errorf("Deployment %s rendered with the MooseFS storage", name)
		}
	}
	for _, name := range []string{workspacesNodePlugin, hostPathNodePlugin} {
		if find("DaemonSet", name) != nil {
			t.Errorf("DaemonSet %s rendered with the MooseFS storage", name)
		}
	}

	masterHost := mooseFSMaster + "." + workspacesNamespace + ".svc.cluster.local"
	master := named("Deployment", mooseFSMaster)
	if selector, _ := manifestPodSpec(master)["nodeSelector"].(map[string]any); selector["kubernetes.io/hostname"] != cfg.ControlPlaneNode() {
		t.Errorf("master nodeSelector = %v, want the control plane %s", selector, cfg.ControlPlaneNode())
	}
	if got := manifestHostPaths(master)["metadata"]; got != mooseFSDir+"/master" {
		t.Errorf("master metadata hostPath = %q, want %s/master", got, mooseFSDir)
	}
	if inits, _ := manifestPodSpec(master)["initContainers"].([]any); len(inits) != 1 {
		t.Errorf("master initContainers = %v, want the metadata seed", inits)
	}
	named("Service", mooseFSMaster)
	chunkserver := named("Deployment", mooseFSChunkserver)
	env, _ := manifestContainers(chunkserver)["chunkserver"]["env"].([]any)
	if len(env) != 1 || env[0].(map[string]any)["value"] != masterHost {
		t.Errorf("chunkserver env = %v, want MASTER_HOST %s", env, masterHost)
	}
	if paths := manifestHostPaths(chunkserver); paths["chunks"] != mooseFSDir+"/chunks" || paths["state"] != mooseFSDir+"/chunkserver" {
		t.Errorf("chunkserver hostPaths = %v, want its chunks and its state under %s", paths, mooseFSDir)
	}

	driver := named("CSIDriver", mooseFSCSIDriver)
	if spec, _ := driver["spec"].(map[string]any); spec["attachRequired"] != false {
		t.Errorf("CSIDriver attachRequired = %v, want false (no attacher runs)", spec["attachRequired"])
	}
	class := named("StorageClass", workspacesStorageClass)
	if class["provisioner"] != mooseFSCSIDriver {
		t.Errorf("StorageClass provisioner = %v, want %s", class["provisioner"], mooseFSCSIDriver)
	}
	for _, key := range []string{"parameters", "mountOptions"} {
		if v, ok := class[key]; ok {
			t.Errorf("StorageClass %s = %v, want none", key, v)
		}
	}

	wantFlags := []any{"--master-host=" + masterHost, "--plugin-data-dir=" + mooseFSWorkingDir}
	controller := named("Deployment", mooseFSController)
	if account := manifestPodSpec(controller)["serviceAccountName"]; account != mooseFSController {
		t.Errorf("controller serviceAccountName = %v, want %s", account, mooseFSController)
	}
	controllerContainers := manifestContainers(controller)
	for _, name := range []string{"csi-provisioner", "csi-resizer", "moosefs", workspacesProxyContainer} {
		if controllerContainers[name] == nil {
			t.Errorf("controller has no container %s", name)
		}
	}
	plugin := controllerContainers["moosefs"]
	args, _ := plugin["args"].([]any)
	for _, flag := range append(wantFlags, "--mode=controller") {
		if !slices.Contains(args, flag) {
			t.Errorf("controller driver args = %v, want %v", args, flag)
		}
	}
	if !mountedBidirectional(plugin, "/mnt") || manifestHostPaths(controller)["mfs-mount"] != mooseFSDir+"/mnt" {
		t.Errorf("controller driver mounts the filesystem at /mnt = %v, want %s/mnt Bidirectional", manifestHostPaths(controller), mooseFSDir)
	}

	node := named("DaemonSet", mooseFSNodePlugin)
	nodePlugin := manifestContainers(node)["moosefs"]
	args, _ = nodePlugin["args"].([]any)
	for _, flag := range append(wantFlags, "--mode=node") {
		if !slices.Contains(args, flag) {
			t.Errorf("node plugin args = %v, want %v", args, flag)
		}
	}
	for _, path := range []string{"/var/lib/kubelet/pods", substrateVolumesDir} {
		if !mountedBidirectional(nodePlugin, path) {
			t.Errorf("node plugin does not mount %s Bidirectional", path)
		}
	}
	if manifestHostPaths(node)["socket-dir"] != mooseFSNodeSocketDir {
		t.Errorf("node plugin socket-dir = %v, want %s", manifestHostPaths(node)["socket-dir"], mooseFSNodeSocketDir)
	}

	for _, role := range []string{"provisioner", "resizer"} {
		named("ClusterRole", "agentlab-workspaces-csi-moosefs-"+role)
		binding := named("ClusterRoleBinding", "agentlab-workspaces-csi-moosefs-"+role)
		subjects, _ := binding["subjects"].([]any)
		subject, _ := subjects[0].(map[string]any)
		if subject["name"] != mooseFSController {
			t.Errorf("ClusterRoleBinding %s subject = %v, want %s", role, subject["name"], mooseFSController)
		}
	}
	named("ServiceAccount", mooseFSController)
	named("ServiceAccount", mooseFSNodePlugin)
	named("ConfigMap", "csi-moosefs-proxy")
}

// The MooseFS registration with Substrate: moosefs-csi, the proxy's Service,
// the node plugin's socket and the Service's service-DNS name, in the
// values and in the CSIDriverConfig the lab applies.
func TestWorkspacesMooseFSRegistration(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Storage = config.WorkspaceStorageMooseFS
	v := workspacesValuesFor(cfg)
	type registration struct{ driver, endpoint, socket, serverName string }
	want := registration{
		driver:     mooseFSCSIDriver,
		endpoint:   "tcp://csi-moosefs-controller.agentlab-workspaces.svc.cluster.local:50051",
		socket:     "unix://" + mooseFSNodeSocketDir + "/csi.sock",
		serverName: "csi-moosefs-controller.agentlab-workspaces.svc",
	}
	if got := (registration{v.Driver, v.ControllerEndpoint, v.NodeSocket, v.ServerName}); got != want {
		t.Errorf("registration values = %+v, want %+v", got, want)
	}
	if !v.MooseFS || v.HostPath {
		t.Errorf("MooseFS, HostPath = %v, %v, want true, false", v.MooseFS, v.HostPath)
	}
	if b := workspacesBackendOfDriver(mooseFSCSIDriver); b.storage != config.WorkspaceStorageMooseFS || b.nfsServer {
		t.Errorf("workspacesBackendOfDriver(%s) = %+v, want the MooseFS backend", mooseFSCSIDriver, b)
	}

	u := workspacesCSIDriverConfig(workspacesBackendFor(cfg))
	spec, _ := u.Object[crSpec].(map[string]any)
	if u.GetName() != mooseFSCSIDriver || spec["driverName"] != mooseFSCSIDriver || spec["controllerEndpoint"] != want.endpoint || spec["nodeSocketOverride"] != want.socket {
		t.Errorf("CSIDriverConfig %s spec = %v, want moosefs-csi's endpoint and socket", u.GetName(), spec)
	}
	if tls, _ := spec["tls"].(map[string]any); tls["serverName"] != want.serverName {
		t.Errorf("CSIDriverConfig tls = %v, want serverName %s", tls, want.serverName)
	}
}
