package lab

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// stubWorkspacesProbe points the chart probe at a fixed answer for the test
// and clears the memo, restored afterwards.
func stubWorkspacesProbe(t *testing.T, carries bool) {
	t.Helper()
	prev := chartWorkspacesProbe
	chartWorkspacesMu.Lock()
	chartWorkspacesCache = map[string]bool{}
	chartWorkspacesMu.Unlock()
	chartWorkspacesProbe = func(platformChart) (bool, error) { return carries, nil }
	t.Cleanup(func() {
		chartWorkspacesProbe = prev
		chartWorkspacesMu.Lock()
		chartWorkspacesCache = map[string]bool{}
		chartWorkspacesMu.Unlock()
	})
}

// The values template's workspaces block: absent with the switch off,
// absent for a chart whose values take no workspaces key (the schema's root
// refuses unknown keys), and the lab's names for a chart that takes it.
func TestPlatformValuesWorkspaces(t *testing.T) {
	render := func(t *testing.T, cfg *config.Config) map[string]any {
		t.Helper()
		out, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values
	}
	stubWorkspacesProbe(t, true)
	cfg := config.Default()
	if _, ok := render(t, cfg)["workspaces"]; ok {
		t.Error("a workspaces block rendered while platform.workspaces is off")
	}

	cfg.Platform.Workspaces.Enabled = true
	stubWorkspacesProbe(t, false)
	if block, ok := render(t, cfg)["workspaces"]; ok {
		t.Errorf("a workspaces block rendered for a chart without the key: %v", block)
	}

	stubWorkspacesProbe(t, true)
	block, _ := render(t, cfg)["workspaces"].(map[string]any)
	if block["enabled"] != true {
		t.Fatalf("workspaces.enabled = %v, want true", block["enabled"])
	}
	storage, _ := block["storage"].(map[string]any)
	if storage["storageClassName"] != workspacesStorageClass {
		t.Errorf("workspaces.storage.storageClassName = %v, want %s", storage["storageClassName"], workspacesStorageClass)
	}
	if create, _ := storage["storageClass"].(map[string]any); create["create"] != false {
		t.Errorf("workspaces.storage.storageClass = %v, want create: false (the lab installs the class)", create)
	}
	substrate, _ := block["substrate"].(map[string]any)
	driver, _ := substrate["csiDriver"].(map[string]any)
	want := map[string]any{
		"name":               workspacesCSIDriver,
		"controllerEndpoint": "tcp://csi-nfs-controller.agentlab-workspaces.svc.cluster.local:50051",
		"nodeSocketOverride": "unix://" + workspacesNodeSocketDir + "/csi.sock",
	}
	for key, value := range want {
		if driver[key] != value {
			t.Errorf("workspaces.substrate.csiDriver.%s = %v, want %v", key, driver[key], value)
		}
	}
	tls, _ := driver["tls"].(map[string]any)
	if tls["enabled"] != true || tls["usePodIdentity"] != true || tls["serverName"] != "csi-nfs-controller.agentlab-workspaces.svc" {
		t.Errorf("workspaces.substrate.csiDriver.tls = %v, want enabled with the pod identity and the service-DNS name", tls)
	}
}

// The workspaces manifest: every image pinned and none from ghcr.io, the
// pieces in place, a read-write-many class without snapshots, the NFS
// server and the controller on the control plane, and Substrate's volumes
// directory mounted Bidirectional into the node plugin on every node.
func TestWorkspacesManifest(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
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
	named := func(kind, name string) map[string]any {
		for _, d := range rendered {
			meta, _ := d["metadata"].(map[string]any)
			if d["kind"] == kind && meta["name"] == name {
				return d
			}
		}
		t.Fatalf("no %s %s in the manifest", kind, name)
		return nil
	}
	podSpec := func(doc map[string]any) map[string]any {
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		return pod
	}

	for _, d := range rendered {
		if d["kind"] == "VolumeSnapshotClass" {
			t.Error("a VolumeSnapshotClass rendered: a Session is a directory on the workspace's volume, not a snapshot's clone")
		}
		pod := podSpec(d)
		containers, _ := pod["containers"].([]any)
		for _, c := range containers {
			container, _ := c.(map[string]any)
			image, _ := container["image"].(string)
			if !strings.Contains(image, ":") || strings.HasSuffix(image, ":latest") {
				t.Errorf("%s %v: image %q is not pinned", d["kind"], container["name"], image)
			}
			if strings.HasPrefix(image, "ghcr.io/") {
				t.Errorf("%s %v: image %q from ghcr.io", d["kind"], container["name"], image)
			}
		}
	}

	named("CSIDriver", workspacesCSIDriver)
	named("Service", workspacesControllerService)
	class := named("StorageClass", workspacesStorageClass)
	if class["provisioner"] != workspacesCSIDriver {
		t.Errorf("StorageClass provisioner = %v, want %s", class["provisioner"], workspacesCSIDriver)
	}
	if options, _ := class["mountOptions"].([]any); len(options) == 0 || options[0] != "nfsvers=4.1" {
		t.Errorf("StorageClass mountOptions = %v, want NFSv4.1 first", options)
	}

	for _, kind := range []string{"Deployment"} {
		for _, name := range []string{workspacesNFSServer, workspacesController} {
			selector, _ := podSpec(named(kind, name))["nodeSelector"].(map[string]any)
			if selector["kubernetes.io/hostname"] != cfg.ControlPlaneNode() {
				t.Errorf("%s %s nodeSelector = %v, want the control plane %s, whose disk holds the export", kind, name, selector, cfg.ControlPlaneNode())
			}
		}
	}

	node := podSpec(named("DaemonSet", workspacesNodePlugin))
	if selector, _ := node["nodeSelector"].(map[string]any); selector["kubernetes.io/hostname"] != nil {
		t.Errorf("the node plugin is pinned to %v; it runs on every node, wherever the actors' workers are", selector["kubernetes.io/hostname"])
	}
	bidirectional := false
	for _, c := range node["containers"].([]any) {
		container, _ := c.(map[string]any)
		mounts, _ := container["volumeMounts"].([]any)
		for _, m := range mounts {
			mount, _ := m.(map[string]any)
			if mount["mountPath"] == substrateVolumesDir && mount["mountPropagation"] == "Bidirectional" {
				bidirectional = true
			}
		}
	}
	if !bidirectional {
		t.Errorf("the node plugin mounts %s without mountPropagation Bidirectional: atelet would not see the published volumes", substrateVolumesDir)
	}
}

// The status line words the pieces in place.
func TestWorkspacesStatusString(t *testing.T) {
	node := config.Default().ControlPlaneNode()
	s := &WorkspacesStatus{NFSServer: conditionReady, Controller: conditionReady, NodePlugin: conditionReady, Node: node, StorageClass: true, CSIDriverConfig: "the lab"}
	got := s.String()
	for _, want := range []string{"NFS server Ready on " + node, "CSI controller " + workspacesCSIDriver + " Ready with its mTLS proxy", "node plugin Ready", "StorageClass " + workspacesStorageClass, "CSIDriverConfig " + workspacesCSIDriver + " by the lab"} {
		if !strings.Contains(got, want) {
			t.Errorf("status %q lacks %q", got, want)
		}
	}
	bare := (&WorkspacesStatus{NFSServer: stateMissing, Controller: stateMissing, NodePlugin: stateMissing}).String()
	for _, want := range []string{"no StorageClass", "not registered with Substrate"} {
		if !strings.Contains(bare, want) {
			t.Errorf("status %q lacks %q", bare, want)
		}
	}
}
