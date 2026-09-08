package lab

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// Fixture refs, hoisted so the linter's constant check stays quiet.
const (
	devMusterRef        = "muster:dev-1a2b"
	devMusterName       = "docker.io/library/muster"
	devMusterFull       = devMusterName + ":dev-1a2b"
	devBackstageRef     = "backstage-dev:tools-84e5"
	devBackstageFull    = "docker.io/library/" + devBackstageRef
	chartMusterImage    = "gsoci.azurecr.io/giantswarm/muster"
	chartBackstageImage = "gsoci.azurecr.io/giantswarm/backstage"
)

// The dev-image swap spells the ref the way containerd lists a side-loaded
// image, split into kustomize's newName + newTag (or digest).
func TestDevImageOverride(t *testing.T) {
	cases := []struct {
		ref  string
		want kustomizeImage
	}{
		{devMusterRef, kustomizeImage{Name: chartMusterImage, NewName: devMusterName, NewTag: "dev-1a2b"}},
		{"giantswarm/muster:dev", kustomizeImage{Name: chartMusterImage, NewName: "docker.io/giantswarm/muster", NewTag: "dev"}},
		{"localhost:5000/muster:dev", kustomizeImage{Name: chartMusterImage, NewName: "localhost:5000/muster", NewTag: "dev"}},
		{"gsoci.azurecr.io/giantswarm/muster:5.14.1", kustomizeImage{Name: chartMusterImage, NewName: chartMusterImage, NewTag: "5.14.1"}},
		{"muster@sha256:" + strings.Repeat("a", 64), kustomizeImage{Name: chartMusterImage, NewName: devMusterName, Digest: "sha256:" + strings.Repeat("a", 64)}},
	}
	for _, c := range cases {
		if got := devImageOverride(chartMusterImage, c.ref); got != c.want {
			t.Errorf("devImageOverride(%q) = %+v, want %+v", c.ref, got, c.want)
		}
	}
}

func TestFullImageRef(t *testing.T) {
	cases := map[string]string{
		devMusterRef:                          devMusterFull,
		"giantswarm/muster:dev":               "docker.io/giantswarm/muster:dev",
		"docker.io/library/muster:dev":        "docker.io/library/muster:dev",
		"gsoci.azurecr.io/giantswarm/x:1":     "gsoci.azurecr.io/giantswarm/x:1",
		"localhost/muster:dev":                "localhost/muster:dev",
		"registry:5000/team/muster:dev":       "registry:5000/team/muster:dev",
		"alpine/socat:1.8.1.3":                "docker.io/alpine/socat:1.8.1.3",
		"ghcr.io/fluxcd/helm-controller:v1.6": "ghcr.io/fluxcd/helm-controller:v1.6",
	}
	for ref, want := range cases {
		if got := fullImageRef(ref); got != want {
			t.Errorf("fullImageRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

// componentPostRenderers is the lab's whole patch set as the chart forwards
// it: Flux's postRenderers shape per component, the hostNetwork patches on
// muster and Backstage, the sidecar on every MCP server, the UI NodePort on
// kagent — and, for a dev image, the kustomize image override plus the pull
// policy patch on its container, nothing on the others.
func TestComponentPostRenderers(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Agents = true
	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama}}
	cfg.Platform.DevImages = map[string]string{componentMuster: devMusterRef, componentBackstage: devBackstageRef}
	rendered, err := componentPostRenderers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parse := func(component string) postRenderer {
		raw, ok := rendered[component]
		if !ok {
			t.Fatalf("no postRenderers for %s (have %v)", component, mapKeysSorted(rendered))
		}
		var list []postRenderer
		if err := yaml.Unmarshal([]byte(raw), &list); err != nil {
			t.Fatalf("%s: %v\n%s", component, err, raw)
		}
		if len(list) != 1 {
			t.Fatalf("%s: want one postRenderers entry, got %d", component, len(list))
		}
		return list[0]
	}
	patchOn := func(component, kind, name string) []string {
		var patches []string
		for _, p := range parse(component).Kustomize.Patches {
			if p.Target[kindKey] == kind && p.Target[nameKey] == name {
				patches = append(patches, string(p.Patch))
			}
		}
		return patches
	}

	for _, c := range []string{componentMuster, componentBackstage} {
		got := patchOn(c, kindDeployment, c)
		if len(got) == 0 || !strings.Contains(got[0], "hostNetwork: true") || !strings.Contains(got[0], "maxSurge: 0") {
			t.Errorf("%s: want the hostNetwork + maxSurge 0 patch, got %q", c, got)
		}
	}
	for _, c := range []string{componentMCPKubernetes, modelManagerMCPServer, agentManagerMCPServer} {
		got := patchOn(c, kindDeployment, c)
		if len(got) != 1 || !strings.Contains(got[0], "name: "+dexLocalhostContainer) || !strings.Contains(got[0], "TCP6-LISTEN:32000,fork,reuseaddr") {
			t.Errorf("%s: want the dex-localhost sidecar patch on the lab Dex port, got %q", c, got)
		}
		if imgs := parse(c).Kustomize.Images; len(imgs) != 0 {
			t.Errorf("%s: no dev image configured, want no image override, got %+v", c, imgs)
		}
	}
	// The port entry must carry port AND protocol: a Service's ports list
	// merges by both, and an entry without the protocol is dropped silently.
	if got := patchOn(componentKagent, kindService, "kagent-ui"); len(got) != 1 || !strings.Contains(got[0], "nodePort: 30880") ||
		!strings.Contains(got[0], "port: 8080\n      protocol: TCP") {
		t.Errorf("kagent: want the kagent-ui NodePort pin on the chart's 8080/TCP port entry, got %q", got)
	}

	// The connectivity chart's kagent metrics Service selector, corrected to
	// kagent's instance label (HACKS.md U19) — with agents and observability on.
	if got := patchOn(connectivityComponent, kindService, kagentMetricsService); len(got) != 1 || !strings.Contains(got[0], "app.kubernetes.io/instance: kagent") {
		t.Errorf("connectivity: want the kagent metrics selector patch, got %q", got)
	}

	// The dev images: the override to the fully qualified local ref and the
	// pull policy on the chart's container.
	muster := parse(componentMuster)
	if want := []kustomizeImage{{Name: chartMusterImage, NewName: devMusterName, NewTag: "dev-1a2b"}}; len(muster.Kustomize.Images) != 1 || muster.Kustomize.Images[0] != want[0] {
		t.Errorf("muster dev image: got %+v, want %+v", muster.Kustomize.Images, want)
	}
	if got := patchOn(componentMuster, kindDeployment, componentMuster); len(got) != 2 || !strings.Contains(got[1], "name: muster\n          imagePullPolicy: IfNotPresent") {
		t.Errorf("muster: want the pull-policy patch on container muster after the hostNetwork patch, got %q", got)
	}
	backstage := parse(componentBackstage)
	if len(backstage.Kustomize.Images) != 1 || backstage.Kustomize.Images[0].Name != chartBackstageImage || backstage.Kustomize.Images[0].NewTag != "tools-84e5" {
		t.Errorf("backstage dev image: got %+v", backstage.Kustomize.Images)
	}
	if refs := devImageRefs(cfg); len(refs) != 2 || refs[0] != devBackstageFull || refs[1] != devMusterFull {
		t.Errorf("devImageRefs = %v", refs)
	}

	// Without agents and managed models the optional servers render nothing.
	cfg.Platform.Agents, cfg.Platform.ModelManager.Enabled, cfg.Platform.DevImages = false, false, nil
	rendered, err = componentPostRenderers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{modelManagerMCPServer, agentManagerMCPServer, connectivityComponent} {
		if _, ok := rendered[c]; ok {
			t.Errorf("%s: off, want no postRenderers", c)
		}
	}
	// Every dev-image component has a render target — the config's list and
	// the lab's table cannot drift apart.
	for _, c := range config.DevImageComponents {
		if _, ok := devImageTargets[c]; !ok {
			t.Errorf("config.DevImageComponents names %s, but devImageTargets has no render target for it", c)
		}
	}
}

func mapKeysSorted(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// The node check compares fully qualified refs as crictl lists them.
func TestMissingImages(t *testing.T) {
	have := []string{devMusterFull, "gsoci.azurecr.io/giantswarm/backstage:0.243.1"}
	want := []string{devMusterFull, devBackstageFull}
	if got := missingImages(have, want); len(got) != 1 || got[0] != devBackstageFull {
		t.Errorf("missingImages = %v", got)
	}
	if got := missingImages(have, []string{devMusterFull}); len(got) != 0 {
		t.Errorf("nothing missing, got %v", got)
	}
}
