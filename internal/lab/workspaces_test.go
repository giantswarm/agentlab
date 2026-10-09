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
	if storage["storageClassName"] != workspacesStorageClass || storage["volumeSnapshotClassName"] != workspacesSnapshotClass {
		t.Errorf("workspaces.storage = %v, want the classes %s", storage, workspacesStorageClass)
	}
	if create, _ := storage["volumeSnapshotClass"].(map[string]any); create["create"] != false {
		t.Errorf("workspaces.storage.volumeSnapshotClass = %v, want create: false (the lab installs the class)", create)
	}
	substrate, _ := block["substrate"].(map[string]any)
	driver, _ := substrate["csiDriver"].(map[string]any)
	want := map[string]any{
		"name":               workspacesCSIDriver,
		"controllerEndpoint": "tcp://csi-hostpath-controller.agentlab-workspaces.svc.cluster.local:50051",
		"nodeSocketOverride": workspacesNodeSocket,
	}
	for key, value := range want {
		if driver[key] != value {
			t.Errorf("workspaces.substrate.csiDriver.%s = %v, want %v", key, driver[key], value)
		}
	}
	tls, _ := driver["tls"].(map[string]any)
	if tls["enabled"] != true || tls["usePodIdentity"] != true || tls["serverName"] != "csi-hostpath-controller.agentlab-workspaces.svc" {
		t.Errorf("workspaces.substrate.csiDriver.tls = %v, want enabled with the pod identity and the service-DNS name", tls)
	}
}

// The workspaces manifest: every image pinned, the controller Service ahead
// of the proxy that fronts it, Substrate's volumes directory mounted
// Bidirectional into the driver, and the driver pinned to the control plane
// or, with a substrate worker, to the worker with its taint tolerated.
func TestWorkspacesManifest(t *testing.T) {
	docs := func(t *testing.T, cfg *config.Config) []map[string]any {
		t.Helper()
		out, err := renderTemplate(cfg, workspacesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var docs []map[string]any
		dec := yaml.NewDecoder(bytes.NewReader(out))
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				break
			}
			if doc != nil {
				docs = append(docs, doc)
			}
		}
		if len(docs) == 0 {
			t.Fatalf("no documents:\n%s", out)
		}
		return docs
	}
	named := func(docs []map[string]any, kind, name string) (map[string]any, int) {
		for i, d := range docs {
			meta, _ := d["metadata"].(map[string]any)
			if d["kind"] == kind && meta["name"] == name {
				return d, i
			}
		}
		return nil, -1
	}
	podSpec := func(t *testing.T, doc map[string]any) map[string]any {
		t.Helper()
		spec, _ := doc["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
		if pod == nil {
			t.Fatalf("no pod spec in %v", doc["kind"])
		}
		return pod
	}

	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	rendered := docs(t, cfg)
	for _, d := range rendered {
		spec, _ := d["spec"].(map[string]any)
		template, _ := spec["template"].(map[string]any)
		pod, _ := template["spec"].(map[string]any)
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
	if _, i := named(rendered, "Service", workspacesControllerService); i < 0 {
		t.Fatalf("no Service %s", workspacesControllerService)
	} else if _, j := named(rendered, "StatefulSet", workspacesProxyStatefulSet); j < 0 {
		t.Fatalf("no StatefulSet %s", workspacesProxyStatefulSet)
	} else if i > j {
		t.Errorf("the Service %s (document %d) comes after the proxy StatefulSet (document %d)", workspacesControllerService, i, j)
	}
	for _, kind := range []string{"StorageClass", "VolumeSnapshotClass", "CSIDriver"} {
		name := workspacesStorageClass
		if kind == "CSIDriver" {
			name = workspacesCSIDriver
		}
		if _, i := named(rendered, kind, name); i < 0 {
			t.Errorf("no %s %s", kind, name)
		}
	}
	if class, _ := named(rendered, "StorageClass", workspacesStorageClass); class["allowVolumeExpansion"] != true {
		t.Errorf("StorageClass %s allowVolumeExpansion = %v, want true: a workspace grows with its repositories", workspacesStorageClass, class["allowVolumeExpansion"])
	}

	plugin, _ := named(rendered, "StatefulSet", workspacesPluginStatefulSet)
	if plugin == nil {
		t.Fatalf("no StatefulSet %s", workspacesPluginStatefulSet)
	}
	pod := podSpec(t, plugin)
	bidirectional, resizer := false, false
	for _, c := range pod["containers"].([]any) {
		container, _ := c.(map[string]any)
		if container["image"] == csiResizerImage {
			resizer = true
		}
		mounts, _ := container["volumeMounts"].([]any)
		for _, m := range mounts {
			mount, _ := m.(map[string]any)
			if mount["mountPath"] == substrateVolumesDir && mount["mountPropagation"] == "Bidirectional" {
				bidirectional = true
			}
		}
	}
	if !resizer {
		t.Errorf("the driver pod carries no csi-resizer sidecar (%s): no volume expansion", csiResizerImage)
	}
	if !bidirectional {
		t.Errorf("the driver mounts %s without mountPropagation Bidirectional: Substrate's actors would not see the published volumes", substrateVolumesDir)
	}
	selector, _ := pod["nodeSelector"].(map[string]any)
	if selector["kubernetes.io/hostname"] != cfg.ControlPlaneNode() {
		t.Errorf("nodeSelector = %v, want the control plane %s without a substrate worker", selector, cfg.ControlPlaneNode())
	}
	if tolerations, _ := pod["tolerations"].([]any); len(tolerations) != 0 {
		t.Errorf("tolerations = %v on the control plane, want none", tolerations)
	}

	cfg.SubstrateNodes = 1
	rendered = docs(t, cfg)
	plugin, _ = named(rendered, "StatefulSet", workspacesPluginStatefulSet)
	pod = podSpec(t, plugin)
	worker := cfg.SubstrateNodeNames()[0]
	if selector, _ := pod["nodeSelector"].(map[string]any); selector["kubernetes.io/hostname"] != worker {
		t.Errorf("nodeSelector = %v, want the substrate worker %s", selector, worker)
	}
	tolerations, _ := pod["tolerations"].([]any)
	if len(tolerations) != 1 {
		t.Fatalf("tolerations = %v, want the substrate worker's taint tolerated", tolerations)
	}
	if toleration, _ := tolerations[0].(map[string]any); toleration["key"] != config.SubstrateNodeKey {
		t.Errorf("toleration = %v, want key %s", toleration, config.SubstrateNodeKey)
	}
}

// The status line words the pieces in place.
func TestWorkspacesStatusString(t *testing.T) {
	node := config.Default().ControlPlaneNode()
	s := &WorkspacesStatus{SnapshotController: "rolled out", Driver: conditionReady, Proxy: conditionReady, Node: node, StorageClass: true, SnapshotClass: true, CSIDriverConfig: "the lab"}
	got := s.String()
	for _, want := range []string{"snapshot controller rolled out", "CSI driver " + workspacesCSIDriver + " Ready on " + node, "mTLS proxy Ready", "StorageClass " + workspacesStorageClass, "VolumeSnapshotClass " + workspacesSnapshotClass, "CSIDriverConfig " + workspacesCSIDriver + " by the lab"} {
		if !strings.Contains(got, want) {
			t.Errorf("status %q lacks %q", got, want)
		}
	}
	bare := (&WorkspacesStatus{SnapshotController: stateMissing, Driver: stateMissing, Proxy: stateMissing}).String()
	for _, want := range []string{"no class", "not registered with Substrate"} {
		if !strings.Contains(bare, want) {
			t.Errorf("status %q lacks %q", bare, want)
		}
	}
}
