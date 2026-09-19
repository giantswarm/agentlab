package lab

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// connectivityComponent is the meta chart's component name of the
// agent-platform-connectivity chart, also its release name — named here only
// to assert the lab renders no patch for it.
const connectivityComponent = "agent-platform-connectivity"

// clusterManager is the meta chart's cluster-manager component — a release
// the lab has no toggle for (an overlay turns it on) and no name for: the
// sidecar rule finds it on its render like any other.
const clusterManager = "cluster-manager"

// Fixture refs, hoisted so the linter's constant check stays quiet.
const (
	devMusterRef        = "muster:dev-1a2b"
	devMusterName       = "localhost/muster"
	devMusterFull       = devMusterName + ":dev-1a2b"
	devBackstageRef     = "backstage-dev:tools-84e5"
	devBackstageFull    = "localhost/" + devBackstageRef
	chartMusterImage    = "gsoci.azurecr.io/giantswarm/muster"
	chartBackstageImage = "gsoci.azurecr.io/giantswarm/backstage"
	devControllerRef    = "kagent-controller:dev-139"
	devHarnessRepo      = "golang-adk"
	devHarnessRef       = devHarnessRepo + ":dev-139"
	devTag              = "dev"
	// lineControllerImage is the controller's name on the kagent line, as its
	// chart renders it (the table's fallback is the 3.x wrapper's).
	lineControllerImage = "gsoci.azurecr.io/giantswarm/kagent/controller"
)

// The dev-image swap names the ref the build is side-loaded under — the lab's
// localhost namespace for a local build, a registry ref as it is — split into
// kustomize's newName + newTag (or digest).
func TestDevImageOverride(t *testing.T) {
	cases := []struct {
		ref  string
		want kustomizeImage
	}{
		{devMusterRef, kustomizeImage{Name: chartMusterImage, NewName: devMusterName, NewTag: "dev-1a2b"}},
		{"giantswarm/muster:dev", kustomizeImage{Name: chartMusterImage, NewName: "localhost/giantswarm/muster", NewTag: devTag}},
		{"localhost:5000/muster:dev", kustomizeImage{Name: chartMusterImage, NewName: "localhost:5000/muster", NewTag: devTag}},
		{"gsoci.azurecr.io/giantswarm/muster:5.14.1", kustomizeImage{Name: chartMusterImage, NewName: chartMusterImage, NewTag: "5.14.1"}},
		{"muster@sha256:" + strings.Repeat("a", 64), kustomizeImage{Name: chartMusterImage, NewName: devMusterName, Digest: "sha256:" + strings.Repeat("a", 64)}},
	}
	for _, c := range cases {
		if got := devImageOverride(chartMusterImage, c.ref); got != c.want {
			t.Errorf("devImageOverride(%q) = %+v, want %+v", c.ref, got, c.want)
		}
	}
}

// A build of this host lives in the lab's localhost namespace; a ref that
// names a registry — a dot, a port, localhost itself — is left alone.
func TestLabImageRef(t *testing.T) {
	cases := map[string]string{
		devMusterRef:                          devMusterFull,
		"giantswarm/muster:dev":               "localhost/giantswarm/muster:dev",
		"gsoci.azurecr.io/giantswarm/x:1":     "gsoci.azurecr.io/giantswarm/x:1",
		"localhost/muster:dev":                "localhost/muster:dev",
		"localhost:5001/muster:dev":           "localhost:5001/muster:dev",
		"registry:5000/team/muster:dev":       "registry:5000/team/muster:dev",
		"ghcr.io/fluxcd/helm-controller:v1.6": "ghcr.io/fluxcd/helm-controller:v1.6",
	}
	for ref, want := range cases {
		if got := labImageRef(ref); got != want {
			t.Errorf("labImageRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

// componentPostRenderers is the lab's whole patch set as the chart forwards
// it: Flux's postRenderers shape per component, the hostNetwork patches on
// muster and Backstage, the sidecar on every Deployment the rule selected,
// the UI NodePort on kagent — and, for a dev image, the kustomize image
// override on the name the render resolved plus the pull policy patch on its
// container, nothing on the others. The `harness` target is no post-renderer:
// it pins the platform Harness through the values.
func TestComponentPostRenderers(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Agents = true
	cfg.Platform.Observability = true
	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama}}
	cfg.Platform.DevImages = map[string]string{
		componentMuster:        devMusterRef,
		componentBackstage:     devBackstageRef,
		componentKagent:        devControllerRef,
		config.DevImageHarness: devHarnessRef,
	}
	names := defaultDevImageNames(cfg)
	// The controller's name as the kagent line's render says it (devimages.go).
	names[componentKagent] = lineControllerImage
	// The sidecar's targets as the rule found them on the renders.
	sidecars := map[string][]dexLocalhostTarget{
		componentMCPKubernetes: {{componentMCPKubernetes, dexIssuerURLVar}},
		modelManagerMCPServer:  {{modelManagerMCPServer, dexIssuerURLFlag}},
		agentManagerMCPServer:  {{agentManagerMCPServer, dexIssuerURLFlag}},
		clusterManager:         {{clusterManager, dexIssuerURLFlag}},
	}
	rendered, err := componentPostRenderers(cfg, names, sidecars)
	if err != nil {
		t.Fatal(err)
	}
	// The harness is not a component: nothing renders under its key.
	if raw, ok := rendered[config.DevImageHarness]; ok {
		t.Errorf("harness: the Harness pins its image through the values, got postRenderers:\n%s", raw)
	}
	// The connectivity chart is not patched: its kagent controller metrics
	// Service selects kagent's own release since agent-platform 3.20.2.
	if raw, ok := rendered[connectivityComponent]; ok {
		t.Errorf("connectivity: the lab patches nothing on the wiring chart, got postRenderers:\n%s", raw)
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
	for _, c := range []string{componentMCPKubernetes, modelManagerMCPServer, agentManagerMCPServer, clusterManager} {
		got := patchOn(c, kindDeployment, c)
		if len(got) != 1 || !strings.Contains(got[0], "name: "+dexLocalhostContainer) || !strings.Contains(got[0], "TCP6-LISTEN:32000,fork,reuseaddr") || !strings.Contains(got[0], "initContainers:") || !strings.Contains(got[0], "restartPolicy: Always") {
			t.Errorf("%s: want the dex-localhost native-sidecar patch on the lab Dex port, got %q", c, got)
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
	// The controller override replaces the name the line renders, not the
	// table's wrapper name, and relaxes the pull policy on container
	// `controller` of Deployment kagent-controller.
	kagent := parse(componentKagent)
	if want := (kustomizeImage{Name: lineControllerImage, NewName: "localhost/kagent-controller", NewTag: "dev-139"}); len(kagent.Kustomize.Images) != 1 || kagent.Kustomize.Images[0] != want {
		t.Errorf("kagent dev image: got %+v, want %+v", kagent.Kustomize.Images, want)
	}
	if got := patchOn(componentKagent, kindDeployment, "kagent-controller"); len(got) != 1 || !strings.Contains(got[0], "name: controller\n          imagePullPolicy: IfNotPresent") {
		t.Errorf("kagent: want the pull-policy patch on container controller, got %q", got)
	}
	// The side-load list is the Deployment targets: the harness image goes to
	// the registry instead.
	if refs := devImageRefs(cfg); len(refs) != 3 || refs[0] != devBackstageFull || refs[1] != "localhost/kagent-controller:dev-139" || refs[2] != devMusterFull {
		t.Errorf("devImageRefs = %v", refs)
	}
	if got := harnessDevImage(cfg); got != devHarnessRef {
		t.Errorf("harnessDevImage = %q", got)
	}
	// A configured target whose name did not resolve renders no override
	// (resolveDevImageNames refuses that case before the render; here the
	// table simply has no entry).
	delete(names, componentMuster)
	rendered, err = componentPostRenderers(cfg, names, sidecars)
	if err != nil {
		t.Fatal(err)
	}
	if imgs := parse(componentMuster).Kustomize.Images; len(imgs) != 0 {
		t.Errorf("muster without a resolved name: want no override, got %+v", imgs)
	}

	// Without the renders (a plain `agentlab render`) no sidecar is patched
	// — the rule has nothing to read — and no dev image, no override.
	cfg.Platform.Agents, cfg.Platform.ModelManager.Enabled, cfg.Platform.DevImages = false, false, nil
	rendered, err = componentPostRenderers(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{componentMCPKubernetes, modelManagerMCPServer, agentManagerMCPServer, clusterManager} {
		if _, ok := rendered[c]; ok {
			t.Errorf("%s: no render to read, want no postRenderers", c)
		}
	}
	// Every Deployment target of the config's list has a render target — the
	// list and the lab's table cannot drift apart; the harness is the one
	// key that is no Deployment.
	for _, c := range config.DevImageComponents {
		if _, ok := devImageTargets[c]; !ok && c != config.DevImageHarness {
			t.Errorf("config.DevImageComponents names %s, but devImageTargets has no render target for it", c)
		}
	}
	if _, ok := devImageTargets[config.DevImageHarness]; ok {
		t.Errorf("the harness target is no Deployment; devImageTargets must not list it")
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

// The sidecar rule reads the platform's OAuth contract off the component
// renders: a Deployment whose containers are told the lab Dex's localhost
// address through a name they dial it by — the managers' --dex-issuer-url,
// mcp-kubernetes' DEX_ISSUER_URL, oauth2-proxy's OIDC_ISSUER_URL — is a
// target, keyed by that name as the Deployment spells it; one told the
// address under another name only — the authorization-server identity a
// resource server names in its metadata (OAUTH_AUTHORIZATION_SERVER) — is
// not, nor is one on the host network (the chart's or the lab's). The
// annotation decides instead where it is set, on the Deployment or its pod
// template, and documents that are not Deployments or not objects are
// skipped.
func TestDexLocalhostTargets(t *testing.T) {
	const optedIn = "opted-in"
	cfg := config.Default()
	renders := map[string]string{
		clusterManager: `# Source: cluster-manager/templates/serviceaccount.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cluster-manager
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cluster-manager
spec:
  template:
    spec:
      containers:
        - name: cluster-manager
          image: gsoci.azurecr.io/giantswarm/cluster-manager:0.4.2
          args:
            - --enable-oauth=true
            - --oauth-provider=dex
            - --dex-issuer-url=https://localhost:32000/dex
---
apiVersion: v1
kind: Service
metadata:
  name: cluster-manager
`,
		componentMCPKubernetes: `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mcp-kubernetes
spec:
  template:
    spec:
      containers:
        - name: mcp-kubernetes
          env:
            - name: DEX_ISSUER_URL
              value: "https://localhost:32000/dex"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mcp-kubernetes-helper
spec:
  template:
    spec:
      containers:
        - name: helper
          args: ["--nothing"]
`,
		// oauth2-proxy (the kagent UI) is told the issuer in a variable its
		// argument expands.
		componentKagent: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-oauth2-proxy
spec:
  template:
    spec:
      containers:
        - name: oauth2-proxy
          args: ["--provider=oidc", "--oidc-issuer-url=$(OIDC_ISSUER_URL)"]
          env:
            - name: OIDC_ISSUER_URL
              value: https://localhost:32000/dex
`,
		// A resource server that pins the lab Dex as the authorization
		// server it names in its metadata never dials it: no target.
		"repo-manager": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: repo-manager
spec:
  template:
    spec:
      containers:
        - name: repo-manager
          env:
            - name: OAUTH_ENABLED
              value: "true"
            - name: OAUTH_AUTHORIZATION_SERVER
              value: https://localhost:32000/dex/apps/repo-manager
`,
		// The annotation keeps the sidecar off a Deployment the rule would
		// select …
		"opted-out": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: opted-out
  annotations:
    agentlab.giantswarm.io/dex-localhost: "false"
spec:
  template:
    spec:
      containers:
        - name: x
          env:
            - name: DEX_ISSUER_URL
              value: https://localhost:32000/dex
`,
		// … and, on the pod template, puts it on one it would skip.
		optedIn: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: opted-in
spec:
  template:
    metadata:
      annotations:
        agentlab.giantswarm.io/dex-localhost: "true"
    spec:
      containers:
        - name: x
          env:
            - name: ISSUER
              value: https://localhost:32000/dex
`,
		// muster is told the address too, and reaches it through the lab's
		// hostNetwork patch.
		componentMuster: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: muster
spec:
  template:
    spec:
      containers:
        - name: muster
          args: ["--oauth-issuer=https://localhost:32000/dex"]
`,
		// A chart that runs its pod on the host network needs no bridge.
		"host-networked": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: host-networked
spec:
  template:
    spec:
      hostNetwork: true
      containers:
        - name: x
          args: ["--dex-issuer-url=https://localhost:32000/dex"]
`,
		// Another Dex port is another lab's, or a real installation's; the
		// flag and its value as two arguments are read as one.
		"other-port": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: another-lab\nspec:\n  template:\n    spec:\n      containers:\n        - name: x\n          args: [\"--dex-issuer-url\", \"https://localhost:32001/dex\"]\n",
		// A scalar document, then the object: the scalar is skipped.
		"scalar-first": "just a string\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: after-scalar\nspec:\n  template:\n    spec:\n      containers:\n        - name: x\n          env:\n            - name: DEX_ISSUER_URL\n              value: https://localhost:32000/dex\n",
		"empty":        "",
	}
	got, err := dexLocalhostTargets(cfg, renders)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]dexLocalhostTarget{
		clusterManager:         {{clusterManager, dexIssuerURLFlag}},
		componentMCPKubernetes: {{componentMCPKubernetes, dexIssuerURLVar}},
		componentKagent:        {{"kagent-oauth2-proxy", oidcIssuerURLVar}},
		optedIn:                {{optedIn, dexLocalhostAnnotation + "=true"}},
		"scalar-first":         {{"after-scalar", dexIssuerURLVar}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dexLocalhostTargets = %v, want %v", got, want)
	}
	// The rule follows the lab's Dex port; the annotation selects whatever
	// the address.
	cfg.DexPort = 32001
	got, err = dexLocalhostTargets(cfg, renders)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]dexLocalhostTarget{"other-port": {{"another-lab", dexIssuerURLFlag}}, optedIn: want[optedIn]}; !reflect.DeepEqual(got, want) {
		t.Errorf("dexLocalhostTargets on port 32001 = %v, want %v", got, want)
	}
	// Nothing to read, nothing to patch.
	if got, err := dexLocalhostTargets(cfg, nil); err != nil || len(got) != 0 {
		t.Errorf("dexLocalhostTargets(nil) = %v, %v", got, err)
	}
	// An annotation the rule cannot read is the error, naming the Deployment.
	renders = map[string]string{"typo": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: typo\n  annotations:\n    agentlab.giantswarm.io/dex-localhost: nope\nspec:\n  template:\n    spec:\n      containers:\n        - name: x\n"}
	if _, err := dexLocalhostTargets(cfg, renders); err == nil || !strings.Contains(err.Error(), "Deployment typo") || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("dexLocalhostTargets with an unreadable annotation: err = %v", err)
	}
}
