package lab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// stubWorkspacesProbe points the chart probe at a fixed answer for the test:
// a current chart, which takes the workspaces block and its Organizations'
// list, or one without either.
func stubWorkspacesProbe(t *testing.T, carries bool) {
	t.Helper()
	stubWorkspacesChart(t, chartWorkspaces{Carries: carries, Organizations: carries})
}

// stubWorkspacesChart points the chart probe at w for the test and clears
// the memo, restored afterwards.
func stubWorkspacesChart(t *testing.T, w chartWorkspaces) {
	t.Helper()
	prev := chartWorkspacesProbe
	chartWorkspacesMu.Lock()
	chartWorkspacesCache = map[string]chartWorkspaces{}
	chartWorkspacesMu.Unlock()
	chartWorkspacesProbe = func(platformChart) (chartWorkspaces, error) { return w, nil }
	t.Cleanup(func() {
		chartWorkspacesProbe = prev
		chartWorkspacesMu.Lock()
		chartWorkspacesCache = map[string]chartWorkspaces{}
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

// The sessions' cleanup window reaches the workspace-manager as
// sessions.cleanupAfter only when the lab sets one.
func TestPlatformValuesSessionsCleanupAfter(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cleanupAfter := func(t *testing.T, cfg *config.Config) any {
		t.Helper()
		out, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		wm, _ := values["workspace-manager"].(map[string]any)
		sessions, _ := wm["sessions"].(map[string]any)
		return sessions["cleanupAfter"]
	}
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	if got := cleanupAfter(t, cfg); got != nil {
		t.Errorf("sessions.cleanupAfter = %v without the key, want the chart's", got)
	}
	cfg.Platform.Workspaces.SessionsCleanupAfter = "7m"
	if got := cleanupAfter(t, cfg); got != "7m" {
		t.Errorf("sessions.cleanupAfter = %v, want 7m", got)
	}
}

// The refresh threshold reaches the workspace-manager as sync.refreshAfter
// only when the lab sets one.
func TestPlatformValuesSyncRefreshAfter(t *testing.T) {
	stubWorkspacesProbe(t, true)
	refreshAfter := func(t *testing.T, cfg *config.Config) any {
		t.Helper()
		out, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		wm, _ := values["workspace-manager"].(map[string]any)
		sync, _ := wm["sync"].(map[string]any)
		return sync["refreshAfter"]
	}
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	if got := refreshAfter(t, cfg); got != nil {
		t.Errorf("sync.refreshAfter = %v without the key, want the manager's", got)
	}
	cfg.Platform.Workspaces.SyncRefreshAfter = "1m"
	if got := refreshAfter(t, cfg); got != "1m" {
		t.Errorf("sync.refreshAfter = %v, want 1m", got)
	}
}

// The lab's Organization: on a chart that forwards workspaces.organizations
// the list form there and nothing on the subchart (the chart refuses a
// workspace-manager.organizations of its own); on a chart without the list
// the subchart's map. Either way the platform admins and the developers,
// never the viewers.
func TestPlatformValuesWorkspaceOrganization(t *testing.T) {
	cfg := workspaceLab(config.WorkspaceProviderFake)
	wantGroups := []any{platformAdminsGroup, "developers"}

	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true})
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	block, _ := values["workspaces"].(map[string]any)
	orgs, _ := block["organizations"].([]any)
	if len(orgs) != 1 {
		t.Fatalf("workspaces.organizations = %v, want the lab's one", block["organizations"])
	}
	org, _ := orgs[0].(map[string]any)
	if org["name"] != config.WorkspaceOrganization || !reflect.DeepEqual(org["memberGroups"], wantGroups) {
		t.Errorf("workspaces.organizations[0] = %v, want %s with %v", org, config.WorkspaceOrganization, wantGroups)
	}
	wm, _ := workspaceManagerBlock(t, values)
	if _, ok := wm["organizations"]; ok {
		t.Errorf("workspace-manager.organizations rendered beside workspaces.organizations, which the chart refuses: %v", wm["organizations"])
	}

	stubWorkspacesChart(t, chartWorkspaces{Carries: true})
	values, _, _, _ = renderWorkspaceTemplates(t, cfg, nil)
	block, _ = values["workspaces"].(map[string]any)
	if _, ok := block["organizations"]; ok {
		t.Errorf("workspaces.organizations rendered for a chart without the list: %v", block["organizations"])
	}
	wm, _ = workspaceManagerBlock(t, values)
	byName, _ := wm["organizations"].(map[string]any)
	lab, _ := byName[config.WorkspaceOrganization].(map[string]any)
	if !reflect.DeepEqual(lab["memberGroups"], wantGroups) {
		t.Errorf("workspace-manager.organizations = %v, want %s with %v", wm["organizations"], config.WorkspaceOrganization, wantGroups)
	}
}

// The probe reads a key path off the schema's nested properties or the
// default values.
func TestChartValuesCarry(t *testing.T) {
	schema := []byte(`{"properties":{"workspaces":{"properties":{"organizations":{"type":"array"}}}}}`)
	if !chartValuesCarry(schema, nil, valuesWorkspaces, valuesWorkspacesOrganizations) {
		t.Error("the schema's workspaces.organizations not found")
	}
	if !chartValuesCarry(schema, nil, valuesWorkspaces) {
		t.Error("the schema's workspaces not found")
	}
	old := []byte(`{"properties":{"workspaces":{"properties":{"enabled":{}}}}}`)
	if chartValuesCarry(old, nil, valuesWorkspaces, valuesWorkspacesOrganizations) {
		t.Error("workspaces.organizations found on a schema without it")
	}
	values := map[string]any{"workspaces": map[string]any{"organizations": []any{}}}
	if !chartValuesCarry(nil, values, valuesWorkspaces, valuesWorkspacesOrganizations) {
		t.Error("the values' workspaces.organizations not found")
	}
	if chartValuesCarry(nil, map[string]any{"workspaces": true}, valuesWorkspaces, valuesWorkspacesOrganizations) {
		t.Error("a key path through a scalar found")
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

// The hand-over to the chart is decided by what its components render: a
// connectivity render that takes the workspaces values and carries no
// CSIDriverConfig of the driver (agent-platform 4.125.0 to 4.127.0-rc.2)
// leaves the registration to the lab, one that carries it to the chart.
func TestRendersWorkspacesCSIDriverConfig(t *testing.T) {
	const none = `apiVersion: v1
kind: ConfigMap
metadata:
  name: agent-platform-workspaces
data:
  storageClassName: agentlab-workspaces
---
apiVersion: ate.dev/v1alpha1
kind: CSIDriverConfig
metadata:
  name: efs.csi.aws.com
`
	const registers = none + `---
# Source: agent-platform-connectivity/templates/workspaces/csidriverconfig.yaml
apiVersion: ate.dev/v1alpha1
kind: CSIDriverConfig
metadata:
  name: nfs.csi.k8s.io
  labels:
    app.kubernetes.io/managed-by: Helm
spec:
  driverName: nfs.csi.k8s.io
`
	for _, tc := range []struct {
		name    string
		renders map[string]string
		driver  string
		want    bool
	}{
		{"no renders", nil, workspacesCSIDriver, false},
		{"values taken, nothing rendered", map[string]string{config.ConnectivityChartName: none, substrateRelease: "kind: Deployment\nmetadata:\n  name: ate-api-server\n"}, workspacesCSIDriver, false},
		{"the chart renders it", map[string]string{config.ConnectivityChartName: registers}, workspacesCSIDriver, true},
		{"the companion's, the chart renders it", map[string]string{config.ConnectivityChartName: none}, companionCSIDriver, true},
		{"the companion's, no renders", nil, companionCSIDriver, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rendersWorkspacesCSIDriverConfig(tc.renders, tc.driver); got != tc.want {
				t.Errorf("rendersWorkspacesCSIDriverConfig = %v, want %v", got, tc.want)
			}
		})
	}
}

var (
	gvkCSIDriverConfig = substrateGroupVersion.WithKind(kindCSIDriverConfig)
	gvrCSIDriverConfig = gvkCSIDriverConfig.GroupVersion().WithResource("csidriverconfigs")
)

// csiDriverConfigObject is a live CSIDriverConfig of the lab's driver with
// the given managed-by label ("" for none).
func csiDriverConfigObject(managedBy string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvkCSIDriverConfig)
	u.SetName(workspacesCSIDriver)
	if managedBy != "" {
		u.SetLabels(map[string]string{managedByLabel: managedBy})
	}
	return u
}

// The lab applies its CSIDriverConfig whenever the chart renders none — a
// hand-applied object becomes the lab's, an earlier chart's is waited out
// while its upgrade removes it — and leaves the registration alone when the
// chart renders one or a Helm-managed object outlasts the wait.
func TestEnsureWorkspacesCSIDriverConfig(t *testing.T) {
	prev := csiDriverConfigHandoverTimeout
	csiDriverConfigHandoverTimeout = 3 * time.Second
	t.Cleanup(func() { csiDriverConfigHandoverTimeout = prev })
	for _, tc := range []struct {
		name         string
		live         *unstructured.Unstructured
		chartRenders bool
		// helmRemoves deletes the live object during the wait, as
		// helm-controller's upgrade to a chart that renders none does.
		helmRemoves bool
		want        string
	}{
		{"chart renders none, nothing live", nil, false, false, registrantLab},
		{"chart renders none, applied by hand", csiDriverConfigObject(""), false, false, registrantLab},
		{"chart renders none, the lab's from before", csiDriverConfigObject(managedByAgentlabValue), false, false, registrantLab},
		{"chart renders none, an earlier chart's removed by its upgrade", csiDriverConfigObject("Helm"), false, true, registrantLab},
		{"chart renders none, Helm's outlasts the wait", csiDriverConfigObject("Helm"), false, false, registrantChart},
		{"chart renders one", csiDriverConfigObject("Helm"), true, false, registrantChart},
		{"chart renders one, not live yet", nil, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fakeLab
			if tc.live != nil {
				f = newFakeLab(t, tc.live)
			} else {
				f = newFakeLab(t)
			}
			f.mapper.Add(gvkCSIDriverConfig, meta.RESTScopeRoot)
			ctx := context.Background()
			if tc.helmRemoves {
				go func() {
					time.Sleep(500 * time.Millisecond)
					_ = f.dyn.Resource(gvrCSIDriverConfig).Delete(ctx, workspacesCSIDriver, metav1.DeleteOptions{})
				}()
			}
			if err := ensureWorkspacesCSIDriverConfig(ctx, config.Default(), tc.chartRenders); err != nil {
				t.Fatal(err)
			}
			if got := readWorkspacesRegistrant(ctx, workspacesCSIDriver); got != tc.want {
				t.Errorf("registrant = %q, want %q", got, tc.want)
			}
			if tc.want != registrantLab {
				return
			}
			obj, err := f.dyn.Resource(gvrCSIDriverConfig).Get(ctx, workspacesCSIDriver, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			endpoint, _, _ := unstructured.NestedString(obj.Object, crSpec, "controllerEndpoint")
			if want := workspacesValuesFor(config.Default()).ControllerEndpoint; endpoint != want {
				t.Errorf("spec.controllerEndpoint = %q, want %q", endpoint, want)
			}
		})
	}
}

// The platform summary and status word the registration from one source,
// the live registrant: "not registered" while nothing registered the driver,
// whatever the chart's values take.
func TestWorkspacesHintRegistration(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	for _, registrant := range []string{"", registrantLab, registrantChart} {
		want := workspacesRegistration(workspacesCSIDriver, registrant)
		if hint := workspacesHint(cfg, true, registrant); !strings.Contains(hint, want) {
			t.Errorf("summary %q lacks %q", hint, want)
		}
		if status := (&WorkspacesStatus{CSIDriverConfig: registrant}).String(); !strings.Contains(status, want) {
			t.Errorf("status %q lacks %q", status, want)
		}
	}
	if got := workspacesRegistration(workspacesCSIDriver, ""); !strings.Contains(got, "not registered") {
		t.Errorf("no registrant worded %q", got)
	}
}

// companionConfig is a lab with workspaces on and the chart's CSI companion
// as Substrate's driver.
func companionConfig() *config.Config {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.CSICompanion = true
	return cfg
}

// With the companion on, the values hand Substrate's registration to the
// chart's companion: no csiDriver (the chart refuses one beside a preset of
// another driver), the efs preset with the lab's NFS plugin, its arguments
// in both modes under the preset's driver name.
func TestPlatformValuesCSICompanion(t *testing.T) {
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true, CSICompanion: true})
	out, err := renderTemplate(companionConfig(), platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Workspaces struct {
			Substrate map[string]any `yaml:"substrate"`
		} `yaml:"workspaces"`
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if driver, ok := values.Workspaces.Substrate["csiDriver"]; ok {
		t.Errorf("workspaces.substrate.csiDriver = %v beside the companion: the registration is the companion's", driver)
	}
	raw, err := yaml.Marshal(values.Workspaces.Substrate["csiCompanion"])
	if err != nil {
		t.Fatal(err)
	}
	var companion struct {
		Enabled bool   `yaml:"enabled"`
		Preset  string `yaml:"preset"`
		Image   struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
		Controller struct {
			Args []string `yaml:"args"`
		} `yaml:"controller"`
		Node struct {
			Args []string `yaml:"args"`
		} `yaml:"node"`
	}
	if err := yaml.Unmarshal(raw, &companion); err != nil {
		t.Fatal(err)
	}
	if !companion.Enabled || companion.Preset != companionPreset {
		t.Errorf("csiCompanion enabled %v preset %q, want enabled with %s", companion.Enabled, companion.Preset, companionPreset)
	}
	if got := "gsoci.azurecr.io/" + companion.Image.Repository + ":" + companion.Image.Tag; got != nfsPluginImage {
		t.Errorf("csiCompanion image = %s, want the lab's NFS plugin %s", got, nfsPluginImage)
	}
	for mode, args := range map[string][]string{"controller": companion.Controller.Args, "node": companion.Node.Args} {
		for _, want := range []string{"--endpoint=$(CSI_ENDPOINT)", "--drivername=" + companionCSIDriver} {
			if !slices.Contains(args, want) {
				t.Errorf("csiCompanion.%s.args = %v, want %s", mode, args, want)
			}
		}
	}
	if !slices.Contains(companion.Node.Args, "--nodeid=$(KUBE_NODE_NAME)") || !slices.Contains(companion.Controller.Args, "--nodeid=$(CSI_NODE_NAME)") {
		t.Errorf("csiCompanion args %v / %v: want each mode's node name from the preset's environment", companion.Controller.Args, companion.Node.Args)
	}
}

// The lab's NFS driver serves its volumes under the driver name the
// registration names: its own, or the companion preset's.
func TestWorkspacesManifestDriverName(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"the lab's own registration", func() *config.Config {
			cfg := config.Default()
			cfg.Platform.Workspaces.Enabled = true
			return cfg
		}(), workspacesCSIDriver},
		{"the chart's companion", companionConfig(), companionCSIDriver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderTemplate(tc.cfg, workspacesTemplate, nil)
			if err != nil {
				t.Fatal(err)
			}
			var csiDriver, provisioner string
			dec := yaml.NewDecoder(bytes.NewReader(out))
			for {
				var doc struct {
					Kind     string `yaml:"kind"`
					Metadata struct {
						Name string `yaml:"name"`
					} `yaml:"metadata"`
					Provisioner string `yaml:"provisioner"`
				}
				if err := dec.Decode(&doc); err != nil {
					break
				}
				switch doc.Kind {
				case "CSIDriver":
					csiDriver = doc.Metadata.Name
				case "StorageClass":
					provisioner = doc.Provisioner
				}
			}
			drivernames := strings.Count(string(out), `"--drivername=`+tc.want+`"`)
			if csiDriver != tc.want || provisioner != tc.want || drivernames != 2 {
				t.Errorf("CSIDriver %q, StorageClass provisioner %q, %d driver containers with --drivername=%s; want %s throughout, both containers", csiDriver, provisioner, drivernames, tc.want, tc.want)
			}
		})
	}
}

// A companion is refused on a chart without its values, before anything of
// the storage changes; off, or on a chart that has them, nothing is.
func TestCheckWorkspacesCSICompanion(t *testing.T) {
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true})
	if err := checkWorkspacesCSICompanion(companionConfig()); err == nil || !strings.Contains(err.Error(), "workspaces.substrate.csiCompanion") {
		t.Errorf("a chart without the companion's values: %v, want a refusal naming the key", err)
	}
	off := companionConfig()
	off.Platform.Workspaces.CSICompanion = false
	if err := checkWorkspacesCSICompanion(off); err != nil {
		t.Errorf("the switch off: %v", err)
	}
	stubWorkspacesChart(t, chartWorkspaces{Carries: true, Organizations: true, CSICompanion: true})
	if err := checkWorkspacesCSICompanion(companionConfig()); err != nil {
		t.Errorf("a chart with the companion's values: %v", err)
	}
}

// The companion's registration is the chart's alone: a render without it is
// refused, since the lab's own would name the other driver.
func TestEnsureWorkspacesCSIDriverConfigCompanion(t *testing.T) {
	f := newFakeLab(t)
	f.mapper.Add(gvkCSIDriverConfig, meta.RESTScopeRoot)
	ctx := context.Background()
	if err := ensureWorkspacesCSIDriverConfig(ctx, companionConfig(), false); err == nil {
		t.Error("the companion with a render that registers nothing: no error")
	}
	if err := ensureWorkspacesCSIDriverConfig(ctx, companionConfig(), true); err != nil {
		t.Errorf("the companion registered by the chart: %v", err)
	}
	if got := readWorkspacesRegistrant(ctx, workspacesCSIDriver); got != "" {
		t.Errorf("the lab applied its own registration (%q) beside the companion", got)
	}
}

// The status names the driver the class serves and the companion's state,
// and the hint the companion's path.
func TestWorkspacesCompanionWording(t *testing.T) {
	s := &WorkspacesStatus{NFSServer: conditionReady, Controller: conditionReady, NodePlugin: conditionReady, StorageClass: true, Driver: companionCSIDriver, Companion: conditionReady, CSIDriverConfig: registrantChart}
	got := s.String()
	for _, want := range []string{"CSI controller " + companionCSIDriver, "CSIDriverConfig " + companionCSIDriver + " by the chart", "CSI companion (efs preset, the lab's NFS plugin) Ready"} {
		if !strings.Contains(got, want) {
			t.Errorf("status %q lacks %q", got, want)
		}
	}
	if hint := workspacesHint(companionConfig(), true, registrantChart); !strings.Contains(hint, "CSI companion") || !strings.Contains(hint, "CSIDriverConfig "+companionCSIDriver) {
		t.Errorf("summary %q lacks the companion", hint)
	}
	if e := substrateDriverEndpointFor(companionConfig()); e.serverName != "csi-companion-controller.agent-platform.svc" || e.namespace != platformNamespace {
		t.Errorf("the companion's endpoint = %+v", e)
	}
}

// TestWorkspaceManagerSkip: a lab with the switch off or a chart without the
// workspaces values has no workspace-manager to prove, said as a skip; a chart
// the probe could not load is no such chart, and the probe's failure is
// returned instead — and not memoized, so the next call probes again.
func TestWorkspaceManagerSkip(t *testing.T) {
	cfg := config.Default()
	if reason, err := workspaceManagerSkip(cfg); err != nil || !strings.Contains(reason, "platform.workspaces is off") {
		t.Errorf("workspaces off: skip %q, %v; want the switch named", reason, err)
	}

	cfg.Platform.Workspaces.Enabled = true
	stubWorkspacesProbe(t, false)
	if reason, err := workspaceManagerSkip(cfg); err != nil || !strings.Contains(reason, "takes no workspaces values") {
		t.Errorf("a chart without the values: skip %q, %v; want it refused with the reason", reason, err)
	}

	stubWorkspacesProbe(t, true)
	if reason, err := workspaceManagerSkip(cfg); err != nil || reason != "" {
		t.Errorf("a chart with the values: skip %q, %v; want none", reason, err)
	}

	stubWorkspacesProbe(t, true)
	answers := []error{tokenClosed}
	chartWorkspacesProbe = func(platformChart) (chartWorkspaces, error) {
		if len(answers) > 0 {
			err := answers[0]
			answers = answers[1:]
			return chartWorkspaces{}, err
		}
		return chartWorkspaces{Carries: true}, nil
	}
	reason, err := workspaceManagerSkip(cfg)
	if !errors.Is(err, io.EOF) || reason != "" {
		t.Errorf("a chart the probe could not load: skip %q, %v; want the probe's failure, not a skip", reason, err)
	}
	if err != nil && strings.Contains(err.Error(), "takes no workspaces values") {
		t.Errorf("a chart the probe could not load is worded as one without the values: %v", err)
	}
	if reason, err := workspaceManagerSkip(cfg); err != nil || reason != "" {
		t.Errorf("the probe once the registry answers: skip %q, %v; want none", reason, err)
	}
}
