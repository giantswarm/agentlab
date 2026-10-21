package lab

import (
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/giantswarm/agentlab/internal/config"
)

// The umbrella values hand every host model server to the one model-manager:
// two or more backends render `model-manager.backends` with one endpoint
// block each; a single one renders the chart's one-backend form.
// ollamaLabEndpoint is the host Ollama as the kind docker network resolves it
// in these fixtures.
const ollamaLabEndpoint = "http://172.21.0.1:11434"

// upgradeSeedVersion is a 4.x release below the families floor, the line an
// upgrade proof seeds.
const upgradeSeedVersion = "4.66.5"

func TestModelManagerValuesRenderBackends(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Enabled, cfg.Platform.Agents = true, true
	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama, lemonade}}
	endpoints := map[string]string{ollama: ollamaLabEndpoint, lemonade: "http://172.21.0.1:13305"}
	render := func() string {
		out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", func(d *tmplData) { d.ModelManagerEndpoints = endpoints })
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	got := render()
	want := "model-manager:\n  backends:\n    - ollama\n    - lemonade\n  ollama:\n    endpoint: \"http://172.21.0.1:11434\"\n  lemonade:\n    endpoint: \"http://172.21.0.1:13305\"\n  muster:"
	if !strings.Contains(got, want) {
		t.Fatalf("two backends: want\n%s\nin\n%s", want, excerptAround(got, "model-manager:\n"))
	}

	// Every host server the lab knows, in the canonical order.
	cfg.Platform.ModelManager.Backends = []string{ollama, lemonade, lmstudio}
	endpoints[lmstudio] = "http://172.21.0.1:1234"
	got = render()
	want = "model-manager:\n  backends:\n    - ollama\n    - lemonade\n    - lmstudio\n  ollama:\n    endpoint: \"http://172.21.0.1:11434\"\n  lemonade:\n    endpoint: \"http://172.21.0.1:13305\"\n  lmstudio:\n    endpoint: \"http://172.21.0.1:1234\"\n  muster:"
	if !strings.Contains(got, want) {
		t.Fatalf("three backends: want\n%s\nin\n%s", want, excerptAround(got, "model-manager:\n"))
	}

	cfg.Platform.ModelManager.Backends = []string{lemonade}
	got = render()
	want = "model-manager:\n  backend: lemonade\n  lemonade:\n    endpoint: \"http://172.21.0.1:13305\"\n  muster:"
	if !strings.Contains(got, want) || strings.Contains(got, "backends:") {
		t.Fatalf("one backend: want the chart's one-backend form\n%s\nin\n%s", want, excerptAround(got, "model-manager:\n"))
	}

	// A single lmstudio also renders the one-backend form.
	cfg.Platform.ModelManager.Backends = []string{lmstudio}
	got = render()
	want = "model-manager:\n  backend: lmstudio\n  lmstudio:\n    endpoint: \"http://172.21.0.1:1234\"\n  muster:"
	if !strings.Contains(got, want) || strings.Contains(got, "backends:") {
		t.Fatalf("one lmstudio backend: want\n%s\nin\n%s", want, excerptAround(got, "model-manager:\n"))
	}
}

func excerptAround(s, marker string) string {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return s
	}
	end := min(len(s), i+400)
	return s[i:end]
}

// The lab-owned edge Service serves the public port in-cluster: exactly one
// port at 443, and off 443 a second one on gatewayPort mapped onto the same
// 443 listener, its NodePort pinned like the first.
func TestGatewayEdgeServicePorts(t *testing.T) {
	type port struct {
		Name       string `yaml:"name"`
		Port       int    `yaml:"port"`
		TargetPort int    `yaml:"targetPort"`
		NodePort   int    `yaml:"nodePort"`
	}
	render := func(gatewayPort int) []port {
		cfg := config.Default()
		cfg.Platform.GatewayPort = gatewayPort
		out, err := renderTemplate(cfg, "gateway-nodeport.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var svc struct {
			Spec struct {
				Ports []port `yaml:"ports"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(out, &svc); err != nil {
			t.Fatalf("gatewayPort %d: %v\n%s", gatewayPort, err, out)
		}
		return svc.Spec.Ports
	}

	if got := render(443); len(got) != 1 || got[0] != (port{"https", 443, 443, config.GatewayNodePort}) {
		t.Fatalf("443: want the single NodePort entry, got %+v", got)
	}
	got := render(8443)
	if len(got) != 2 || got[0] != (port{"https", 443, 443, config.GatewayNodePort}) {
		t.Fatalf("8443: want the NodePort entry plus one, got %+v", got)
	}
	if got[1] != (port{"https-public", 8443, 443, config.GatewayPublicNodePort}) {
		t.Fatalf("8443: want the public port onto the 443 listener with its pinned nodePort, got %+v", got[1])
	}
}

// The platform values are the agent-platform meta chart in its lab shape:
// the bundled engine on, self-management off, gitops.namespace left to the
// release namespace, the standalone umbrella's contract set explicitly, the
// lab's per-component postRenderers in place of the retired post-renderer
// binary, and the connectivity wiring keys where the meta chart reads them.
func TestPlatformValuesLabShape(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama}}
	cfg.Platform.DevImages = map[string]string{componentKagent: "kagent-controller:dev-9f8e"}
	// The second render of `agentlab platform`: the sidecar's targets as the
	// rule found them on the component renders — the servers the lab turns
	// on and cluster-manager, which only an overlay does.
	sidecars := map[string][]dexLocalhostTarget{
		componentMCPKubernetes: {{componentMCPKubernetes, dexIssuerURLVar}},
		modelManagerMCPServer:  {{modelManagerMCPServer, dexIssuerURLFlag}},
		agentManagerMCPServer:  {{agentManagerMCPServer, dexIssuerURLFlag}},
		clusterManager:         {{clusterManager, dexIssuerURLFlag}},
	}
	postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), sidecars)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", func(d *tmplData) {
		d.ModelManagerEndpoints = map[string]string{ollama: ollamaLabEndpoint}
		d.PostRenderers = postRenderers
	})
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	at := func(path ...string) any {
		var cur any = values
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		return cur
	}
	if at("gitops", "self", "enabled") != false {
		t.Errorf("gitops.self.enabled = %v, want false (the lab installs unreleased charts; a self HelmRelease would replace them)", at("gitops", "self", "enabled"))
	}
	if at("gitops", "namespace") != nil {
		t.Errorf("gitops.namespace = %v, want unset with the engine on", at("gitops", "namespace"))
	}
	if at("components", "flux", "enabled") != true {
		t.Errorf("components.flux.enabled = %v, want true", at("components", "flux", "enabled"))
	}
	for _, c := range []string{agentgatewayComponent, "agent-platform-mcps", componentMCPKubernetes, componentKagent, agentManagerMCPServer, componentBackstage, modelManagerMCPServer} {
		if at("components", c, "enabled") != true {
			t.Errorf("components.%s.enabled = %v, want true", c, at("components", c, "enabled"))
		}
	}
	if at("components", "dicebear", "enabled") != false {
		t.Errorf("components.dicebear.enabled = %v, want false", at("components", "dicebear", "enabled"))
	}
	for _, c := range []string{componentMuster, componentBackstage, componentKagent, componentMCPKubernetes, modelManagerMCPServer, agentManagerMCPServer, clusterManager} {
		if _, ok := at("components", c, "postRenderers").([]any); !ok {
			t.Errorf("components.%s.postRenderers missing: %v", c, at("components", c))
		}
	}
	// cluster-manager has no block in the roster without its dev image: its
	// patch renders through the trailing range, and nothing but the patch —
	// the chart's default and the overlay decide `enabled`. Its audience is
	// in place either way: the meta chart forwards the block only while the
	// component is on.
	if comp, _ := at("components", clusterManager).(map[string]any); len(comp) != 1 {
		t.Errorf("components.%s: want the postRenderers only, got %v", clusterManager, comp)
	}
	if got, _ := at(clusterManager, "muster", "mcpServer", "auth", "requiredAudiences").([]any); len(got) != 1 || got[0] != config.KubernetesClientID {
		t.Errorf("%s.muster.mcpServer.auth.requiredAudiences = %v, want [%s] whatever turns the component on", clusterManager, got, config.KubernetesClientID)
	}
	// agentgateway carries no patch without a dev image of its controller,
	// and no proxy values without one of its proxy.
	if comp, _ := at("components", agentgatewayComponent).(map[string]any); len(comp) != 1 {
		t.Errorf("components.%s: want `enabled` only, got %v", agentgatewayComponent, comp)
	}
	if at(agentgatewayComponent) != nil {
		t.Errorf("%s block rendered without a proxy dev image: %v", agentgatewayComponent, at(agentgatewayComponent))
	}
	// The dev image rides the component's postRenderers as a kustomize image.
	kagent := at("components", componentKagent, "postRenderers").([]any)[0].(map[string]any)["kustomize"].(map[string]any)
	if imgs, _ := kagent["images"].([]any); len(imgs) != 1 || imgs[0].(map[string]any)["newTag"] != "dev-9f8e" {
		t.Errorf("kagent dev image not in its postRenderers: %v", kagent["images"])
	}
	// The contract the standalone umbrella set for the lab, now explicit.
	if at("ingress", "mode") != "agentgateway-muster" || at("agent-platform-mcps", "agentgateway", "viaMuster") != true {
		t.Errorf("ingress.mode/viaMuster: %v / %v", at("ingress", "mode"), at("agent-platform-mcps", "agentgateway", "viaMuster"))
	}
	if at("ingress", "httpRoute", "timeouts", "request") != "0s" {
		t.Errorf("ingress.httpRoute.timeouts.request = %v, want 0s", at("ingress", "httpRoute", "timeouts", "request"))
	}
	if at("networkPolicy", "enabled") != false || at(componentMCPKubernetes, "ciliumNetworkPolicy", "enabled") != false {
		t.Errorf("network policies must be off in kind: %v / %v", at("networkPolicy", "enabled"), at(componentMCPKubernetes, "ciliumNetworkPolicy", "enabled"))
	}
	// The wiring keys sit where the meta chart reads them (top-level blocks),
	// not under components.<name> as the standalone umbrella had them.
	if at(componentMCPKubernetes, "kubernetesAudience") != config.KubernetesClientID || at(componentKagent, "controllerRoute", "enabled") != true ||
		at(componentBackstage, "extraScopes") == nil {
		t.Errorf("wiring keys: mcp-kubernetes.kubernetesAudience=%v kagent.controllerRoute.enabled=%v backstage.extraScopes=%v",
			at(componentMCPKubernetes, "kubernetesAudience"), at(componentKagent, "controllerRoute", "enabled"), at(componentBackstage, "extraScopes"))
	}
	// model-manager is reached through muster only: the lab renders no REST
	// route for it.
	if at("modelManager", "route") != nil {
		t.Errorf("modelManager.route = %v, want unset (model-manager is reached through muster)", at("modelManager", "route"))
	}
	for _, c := range []string{componentMCPKubernetes, componentKagent, componentBackstage, modelManagerMCPServer} {
		comp := at("components", c).(map[string]any)
		for key := range comp {
			if key != "enabled" && key != "postRenderers" {
				t.Errorf("components.%s.%s: wiring keys belong in the top-level %s block", c, key, c)
			}
		}
	}
	if at("global", "identity", "issuerUrl") != cfg.Issuer() || at("global", "identity", "ca", "secretName") != "dex-ca" {
		t.Errorf("global.identity: %v", at("global", "identity"))
	}
}

// Every patch reaches its component exactly once, whatever the lab's
// toggles: a component the roster names a block for renders it there — or,
// while its toggle is off, renders the block as `enabled: false` and no patch
// (model-manager, backstage) — and one the roster leaves out under these
// settings (cluster-manager, which only an overlay turns on) renders through
// the trailing range. A key rendered twice would not parse (yaml.v3 refuses
// a duplicate mapping key), which is what pins ExtraPostRenderers to the
// template's conditions.
func TestPlatformValuesExtraPostRenderers(t *testing.T) {
	sidecars := map[string][]dexLocalhostTarget{
		componentMCPKubernetes: {{componentMCPKubernetes, dexIssuerURLVar}},
		modelManagerMCPServer:  {{modelManagerMCPServer, dexIssuerURLFlag}},
		agentManagerMCPServer:  {{agentManagerMCPServer, dexIssuerURLFlag}},
		vmManagerMCPServer:     {{vmManagerMCPServer, dexIssuerURLFlag}},
		clusterManager:         {{clusterManager, dexIssuerURLFlag}},
	}
	for name, cfg := range map[string]*config.Config{
		"everything off": func() *config.Config {
			cfg := config.Default()
			cfg.Platform.Agents, cfg.Backstage.Enabled, cfg.Platform.Observability = false, false, false
			return cfg
		}(),
		"everything on": func() *config.Config {
			cfg := config.Default()
			cfg.Platform.Agents, cfg.Backstage.Enabled = true, true
			cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama}}
			cfg.Platform.VMManager.Enabled = true
			cfg.Platform.KlausGateway.Enabled = true
			cfg.Platform.DevImages = map[string]string{componentMuster: devMusterRef, componentBackstage: devBackstageRef, clusterManager: "cluster-manager:dev", config.DevImageAgentgatewayController: "agentgateway-controller:dev"}
			return cfg
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), sidecars)
			if err != nil {
				t.Fatal(err)
			}
			out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) {
				d.PostRenderers = postRenderers
				d.ModelManagerEndpoints = map[string]string{ollama: ollamaLabEndpoint}
			})
			if err != nil {
				t.Fatal(err)
			}
			var values struct {
				Components map[string]map[string]any `yaml:"components"`
			}
			if err := yaml.Unmarshal(out, &values); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			for component := range sidecars {
				comp := values.Components[component]
				if component == modelManagerMCPServer && !cfg.ModelManagerEnabled() {
					if _, patched := comp["postRenderers"]; comp["enabled"] != false || patched {
						t.Errorf("components.%s with the toggle off: want enabled false and no patch, got %v", component, comp)
					}
					continue
				}
				list, ok := comp["postRenderers"].([]any)
				if !ok || len(list) != 1 {
					t.Errorf("components.%s.postRenderers: want the one entry, got %v", component, comp)
				}
			}
			// cluster-manager's block is the roster's with its dev image
			// (enabled, patched) and the range's without (the patch alone).
			if _, on := cfg.Platform.DevImages[clusterManager]; (values.Components[clusterManager]["enabled"] == true) != on {
				t.Errorf("components.%s.enabled with the dev image %v: got %v", clusterManager, on, values.Components[clusterManager])
			}
			// The controller's patch lands on the roster's agentgateway block,
			// which the range never doubles.
			if _, on := cfg.Platform.DevImages[config.DevImageAgentgatewayController]; on {
				if list, ok := values.Components[agentgatewayComponent]["postRenderers"].([]any); !ok || len(list) != 1 {
					t.Errorf("components.%s.postRenderers with a controller dev image: want the one entry, got %v", agentgatewayComponent, values.Components[agentgatewayComponent])
				}
			}
			// The blocks that are always there render their patches only
			// while their toggle is on; the range never doubles them.
			for _, component := range []string{componentKagent, componentBackstage} {
				comp := values.Components[component]
				if _, ok := comp["enabled"]; !ok {
					t.Errorf("components.%s: want the enabled key, got %v", component, comp)
				}
			}
		})
	}
}

// The dev-image targets the roster turns on or patches by name:
// platform.devImages.cluster-manager turns the component the chart ships off
// on, with the sidecar the rule found, the image override and the pull-policy
// patch in its roster block (not the range), next to the lab's audience for
// it; platform.devImages.agentgateway renders the proxy image as the chart's
// proxy.image values, no patch; platform.devImages.agentgateway-controller
// rides the agentgateway component's postRenderers.
func TestPlatformValuesDevImageTargets(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.DevImages = map[string]string{
		clusterManager:                        "cluster-manager:dev-3e4f",
		agentgatewayComponent:                 "agentgateway:dev-7c8d",
		config.DevImageAgentgatewayController: "gsoci.azurecr.io/giantswarm/agentgateway-upstream/controller:2.3.3",
	}
	sidecars := map[string][]dexLocalhostTarget{
		componentMCPKubernetes: {{componentMCPKubernetes, dexIssuerURLVar}},
		clusterManager:         {{clusterManager, dexIssuerURLFlag}},
	}
	postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), sidecars)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.PostRenderers = postRenderers })
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	at := func(path ...string) any {
		var cur any = values
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		return cur
	}
	kustomize := func(component string) map[string]any {
		t.Helper()
		prs, _ := at("components", component, "postRenderers").([]any)
		if len(prs) != 1 {
			t.Fatalf("components.%s.postRenderers: want the one entry, got %v", component, at("components", component))
		}
		return prs[0].(map[string]any)["kustomize"].(map[string]any)
	}

	if at("components", clusterManager, "enabled") != true {
		t.Errorf("components.%s.enabled = %v, want true with its dev image", clusterManager, at("components", clusterManager, "enabled"))
	}
	cm := kustomize(clusterManager)
	if imgs, _ := cm["images"].([]any); len(imgs) != 1 || imgs[0].(map[string]any)["name"] != "gsoci.azurecr.io/giantswarm/cluster-manager" || imgs[0].(map[string]any)["newName"] != "localhost/cluster-manager" || imgs[0].(map[string]any)["newTag"] != "dev-3e4f" {
		t.Errorf("cluster-manager dev image override: %v", cm["images"])
	}
	if patches, _ := cm["patches"].([]any); len(patches) != 2 || !strings.Contains(fmt.Sprint(patches[0]), dexLocalhostContainer) || !strings.Contains(fmt.Sprint(patches[1]), "imagePullPolicy: IfNotPresent") {
		t.Errorf("cluster-manager: want the sidecar and the pull-policy patch, got %v", cm["patches"])
	}
	if got, _ := at(clusterManager, "muster", "mcpServer", "auth", "requiredAudiences").([]any); len(got) != 1 || got[0] != config.KubernetesClientID {
		t.Errorf("%s audience = %v", clusterManager, got)
	}

	proxy, _ := at(agentgatewayComponent, "proxy", "image").(map[string]any)
	if proxy["registry"] != localhostName || proxy["repository"] != "agentgateway" || proxy["tag"] != "dev-7c8d" {
		t.Errorf("agentgateway.proxy.image = %v, want the side-loaded name split into the chart's three values", proxy)
	}
	agw := kustomize(agentgatewayComponent)
	if imgs, _ := agw["images"].([]any); len(imgs) != 1 || imgs[0].(map[string]any)["name"] != chartAgentgatewayControllerImage || imgs[0].(map[string]any)["newTag"] != "2.3.3" {
		t.Errorf("agentgateway controller override: %v", agw["images"])
	}
	if patches, _ := agw["patches"].([]any); len(patches) != 1 || !strings.Contains(fmt.Sprint(patches[0]), "name: agentgateway-controller") || !strings.Contains(fmt.Sprint(patches[0]), "name: controller") {
		t.Errorf("agentgateway: want the controller's pull-policy patch alone, got %v", agw["patches"])
	}
	// The roster block carries cluster-manager now, so the range must not:
	// the parse above would have failed on a doubled key; the data says so
	// too.
	data, err := newTmplData(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data.PostRenderers = postRenderers
	if extra := data.ExtraPostRenderers(); len(extra) != 0 {
		t.Errorf("ExtraPostRenderers = %v, want none: every patched component has a roster block", extra)
	}
}

// platform.modelManager off is the chart's component off: the roster always
// states components.model-manager.enabled, so the chart's default — on since
// agent-platform 4.24.0, with no backend — cannot install a model-manager the
// lab has no backend for (in the lab it would only crash-loop on OIDC
// discovery against the Dex localhost address); the component's own blocks
// (`model-manager:`, `modelManager:`) render only while it is on. The 3.x
// roster knows the component too, so the flag renders on that line as well.
func TestPlatformValuesModelManagerToggle(t *testing.T) {
	render := func(t *testing.T, cfg *config.Config) map[string]any {
		t.Helper()
		out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) {
			d.ModelManagerEndpoints = map[string]string{ollama: ollamaLabEndpoint}
		})
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values
	}
	component := func(values map[string]any) map[string]any {
		comp, _ := values["components"].(map[string]any)[modelManagerMCPServer].(map[string]any)
		return comp
	}
	for name, chartVersion := range map[string]string{"current line": config.DefaultChartVersion, "3.x line": legacyChartVersion} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.Agents = true
			cfg.Platform.ChartVersion = chartVersion
			cfg.Platform.ModelManager.Enabled = false
			values := render(t, cfg)
			if component(values)["enabled"] != false {
				t.Errorf("components.model-manager = %v, want enabled: false stated (the chart's default is on)", component(values))
			}
			for _, key := range []string{modelManagerMCPServer, "modelManager"} {
				if _, ok := values[key]; ok {
					t.Errorf("%s: want no block while the component is off, got %v", key, values[key])
				}
			}

			cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama}}
			values = render(t, cfg)
			if component(values)["enabled"] != true {
				t.Errorf("components.model-manager = %v, want enabled: true", component(values))
			}
			if _, ok := values[modelManagerMCPServer]; !ok {
				t.Errorf("%s: want the block while the component is on", modelManagerMCPServer)
			}
			// The connectivity chart's modelManager block carried the REST
			// route only; model-manager is reached through muster.
			if _, ok := values["modelManager"]; ok {
				t.Errorf("modelManager: want no block (no route), got %v", values["modelManager"])
			}
		})
	}
}

// The lab's mcp-prometheus release is a Flux HelmRelease through the
// platform's engine: pinned chart, the tenant identity, the sidecar patch,
// the workloads in monitoring.
func TestMCPPrometheusRelease(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, mcpPrometheusTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	releases, err := fluxReleases(string(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 1 {
		t.Fatalf("want one release, got %d", len(releases))
	}
	rel := releases[0]
	if rel.Name != mcpPrometheusRelease || rel.Namespace != observabilityNamespace || rel.Version != mcpPrometheusChartVersion ||
		rel.URL != "oci://gsoci.azurecr.io/charts/giantswarm/mcp-prometheus" {
		t.Errorf("release: %+v", rel)
	}
	if !strings.Contains(string(rel.Values), "PROMETHEUS_URL") || !strings.Contains(string(rel.Values), "prometheus-operated."+observabilityNamespace) {
		t.Errorf("values: %s", rel.Values)
	}
	s := string(out)
	for _, want := range []string{"serviceAccountName: agent-platform-flux", "name: " + dexLocalhostContainer, "TCP6-LISTEN:32000,fork,reuseaddr", "namespace: " + platformNamespace} {
		if !strings.Contains(s, want) {
			t.Errorf("mcp-prometheus.yaml lacks %q", want)
		}
	}
}

// platform.mcpPrometheusChartVersion pins the lab's mcp-prometheus chart in
// the OCIRepository; unset, the release this agentlab pins.
func TestMCPPrometheusChartVersionOverride(t *testing.T) {
	for _, tc := range []struct{ set, want string }{
		{"", mcpPrometheusChartVersion},
		{"0.8.1-rc.1", "0.8.1-rc.1"},
	} {
		cfg := config.Default()
		cfg.Platform.MCPPrometheusChartVersion = tc.set
		out, err := renderTemplate(cfg, mcpPrometheusTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		releases, err := fluxReleases(string(out))
		if err != nil {
			t.Fatal(err)
		}
		if len(releases) != 1 {
			t.Fatalf("want one release, got %d", len(releases))
		}
		if got := releases[0].Version; got != tc.want {
			t.Errorf("set %q: want chart %s, got %s", tc.set, tc.want, got)
		}
	}
}

// platform.devImages.mcp-prometheus swaps the image of the lab's own
// mcp-prometheus through that HelmRelease's postRenderers (next to the
// dex-localhost sidecar), never as a meta chart component's values.
func TestMCPPrometheusDevImage(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.DevImages = map[string]string{config.DevImageMCPPrometheus: "mcp-prometheus:dev"}
	out, err := renderTemplate(cfg, mcpPrometheusTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"name: gsoci.azurecr.io/giantswarm/mcp-prometheus",
		"newName: localhost/mcp-prometheus",
		"newTag: dev",
		"imagePullPolicy: IfNotPresent",
		"name: " + dexLocalhostContainer,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("mcp-prometheus.yaml lacks %q:\n%s", want, s)
		}
	}
	renderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := renderers[mcpPrometheusRelease]; ok {
		t.Errorf("the meta chart's values must carry no %s postRenderers, got %s", mcpPrometheusRelease, got)
	}
}

// platform.alertmanager runs the Alertmanager with an always-firing alert and
// points mcp-prometheus at it; off (the default) renders neither.
func TestAlertmanagerSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		cfg := config.Default()
		cfg.Platform.Alertmanager = on
		kps, err := renderTemplate(cfg, "kube-prometheus-stack-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values struct {
			KPS struct {
				Alertmanager struct {
					Enabled bool `yaml:"enabled"`
				} `yaml:"alertmanager"`
				Rules map[string]struct {
					Groups []struct {
						Rules []struct {
							Alert string `yaml:"alert"`
						} `yaml:"rules"`
					} `yaml:"groups"`
				} `yaml:"additionalPrometheusRulesMap"`
			} `yaml:"kube-prometheus-stack"`
		}
		if err := yaml.Unmarshal(kps, &values); err != nil {
			t.Fatalf("%v\n%s", err, kps)
		}
		if values.KPS.Alertmanager.Enabled != on {
			t.Errorf("alertmanager=%v: alertmanager.enabled = %v", on, values.KPS.Alertmanager.Enabled)
		}
		watchdog := false
		for _, rule := range values.KPS.Rules {
			for _, g := range rule.Groups {
				for _, r := range g.Rules {
					watchdog = watchdog || r.Alert == watchdogAlert
				}
			}
		}
		if watchdog != on {
			t.Errorf("alertmanager=%v: %s rule rendered = %v", on, watchdogAlert, watchdog)
		}
		mcp, err := renderTemplate(cfg, mcpPrometheusTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(mcp), "http://alertmanager-operated."+observabilityNamespace+".svc.cluster.local:9093"); got != on {
			t.Errorf("alertmanager=%v: ALERTMANAGER_URL rendered = %v", on, got)
		}
	}
}

// The kind config carries what Substrate's WorkerPools need from the
// apiserver — the PodCertificateRequest API and ClusterTrustBundle projection,
// pod identities and trust bundles projected into the ateom workers — and
// points containerd at /etc/containerd/certs.d, where a node learns about the
// local registry. All of it is fixed at `kind create`, so it renders always.
// Decoded into kind's own config type, so the field names are the ones the
// embedded kind reads.
func TestKindConfigSubstrateGates(t *testing.T) {
	out, err := renderTemplate(config.Default(), "kind-config.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var kindCfg v1alpha4.Cluster
	if err := yaml.Unmarshal(out, &kindCfg); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, gate := range []string{"ClusterTrustBundle", "ClusterTrustBundleProjection", "PodCertificateRequest"} {
		if !kindCfg.FeatureGates[gate] {
			t.Errorf("featureGates.%s = %v, want true", gate, kindCfg.FeatureGates[gate])
		}
	}
	if got := kindCfg.RuntimeConfig["certificates.k8s.io/v1beta1"]; got != "true" {
		t.Errorf("runtimeConfig[certificates.k8s.io/v1beta1] = %q, want \"true\"", got)
	}
	if len(kindCfg.ContainerdConfigPatches) != 1 || !strings.Contains(kindCfg.ContainerdConfigPatches[0], `config_path = "/etc/containerd/certs.d"`) {
		t.Errorf("containerdConfigPatches = %q, want the one certs.d config_path patch", kindCfg.ContainerdConfigPatches)
	}
}

// The edge binds the lab's own loopback address (platform.gatewayAddress), so
// a second lab serves 443 too; every other mapping stays on 127.0.0.1, where
// the issuer's localhost resolves.
func TestKindConfigGatewayAddress(t *testing.T) {
	for addr, want := range map[string]string{"": config.DefaultGatewayAddress, secondEdgeAddress: secondEdgeAddress} {
		cfg := config.Default()
		cfg.Platform.GatewayAddress = addr
		out, err := renderTemplate(cfg, "kind-config.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var kindCfg v1alpha4.Cluster
		if err := yaml.Unmarshal(out, &kindCfg); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, m := range kindCfg.Nodes[0].ExtraPortMappings {
			wantAddr := config.DefaultGatewayAddress
			if m.ContainerPort == config.GatewayNodePort {
				wantAddr = want
			}
			if m.ListenAddress != wantAddr {
				t.Errorf("gatewayAddress %q: mapping %d->%d listens on %s, want %s", addr, m.HostPort, m.ContainerPort, m.ListenAddress, wantAddr)
			}
		}
	}
}

// The API server's host address is fixed, so a recreated lab keeps the
// 127.0.0.1:<port> an agent sandbox's allow list names.
func TestKindConfigPinsAPIServer(t *testing.T) {
	cfg := config.Default()
	cfg.APIServerPort = 6543
	out, err := renderTemplate(cfg, "kind-config.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var kindCfg v1alpha4.Cluster
	if err := yaml.Unmarshal(out, &kindCfg); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := kindCfg.Networking; got.APIServerAddress != "127.0.0.1" || got.APIServerPort != 6543 {
		t.Errorf("networking = %s:%d, want 127.0.0.1:6543", got.APIServerAddress, got.APIServerPort)
	}
}

// etcd skips fsync, so the host's writeback never stalls the lease renewals
// of every leader-elected controller at once. The flag rides the one
// ClusterConfiguration patch, in the kubeadm generation's list form.
func TestKindConfigEtcdSkipsFsync(t *testing.T) {
	out, err := renderTemplate(config.Default(), "kind-config.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var kindCfg v1alpha4.Cluster
	if err := yaml.Unmarshal(out, &kindCfg); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	type arg struct{ Name, Value string }
	for _, patch := range kindCfg.KubeadmConfigPatches {
		var cc struct {
			Kind string
			Etcd struct {
				Local struct {
					ExtraArgs []arg `yaml:"extraArgs"`
				}
			}
		}
		if err := yaml.Unmarshal([]byte(patch), &cc); err != nil {
			t.Fatalf("%v\n%s", err, patch)
		}
		if cc.Kind != "ClusterConfiguration" {
			continue
		}
		if !reflect.DeepEqual(cc.Etcd.Local.ExtraArgs, []arg{{"unsafe-no-fsync", strconv.FormatBool(true)}}) {
			t.Errorf("etcd.local.extraArgs = %v, want unsafe-no-fsync=true", cc.Etcd.Local.ExtraArgs)
		}
		return
	}
	t.Fatal("no ClusterConfiguration patch rendered")
}

// The kagent controller ServiceMonitor stays off on every channel — the
// kagent line's controller serves no metrics listener, and platform-test
// expects a kagent target iff a monitor exists — while the chart-level gate
// keeps following observability.
func TestKagentServiceMonitorStaysOff(t *testing.T) {
	cfg := config.Default()
	render := func() map[string]any {
		out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values
	}
	kagentMonitor := func(v map[string]any) any {
		return v["kagent"].(map[string]any)["serviceMonitor"].(map[string]any)["enabled"]
	}
	v := render()
	if got := kagentMonitor(v); got != false {
		t.Errorf("stable channel with observability: kagent.serviceMonitor.enabled = %v, want false (the line serves no /metrics)", got)
	}
	if got := v["global"].(map[string]any)["observability"].(map[string]any)["metrics"].(map[string]any)["serviceMonitor"].(map[string]any)["enabled"]; got != true {
		t.Errorf("the chart-level monitor gate must still follow observability, got %v", got)
	}
	cfg.Platform.ChartBranch = devChannelBranch
	if got := kagentMonitor(render()); got != false {
		t.Errorf("dev channel: kagent.serviceMonitor.enabled = %v, want false", got)
	}
}

// The lab runs no OTLP gateway, so the platform values empty the chart's
// default collector endpoint whether or not platform.observability brings the
// Prometheus stack: every `auto` exporter (Substrate's included) resolves off
// instead of logging resolver errors for the kube-system otlp-gateway.
func TestPlatformValuesExportNoOTLP(t *testing.T) {
	for _, observability := range []bool{true, false} {
		cfg := config.Default()
		cfg.Platform.Observability = observability
		out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values struct {
			Global struct {
				Observability struct {
					Traces struct {
						OTLP map[string]any `yaml:"otlp"`
					} `yaml:"traces"`
				} `yaml:"observability"`
			} `yaml:"global"`
		}
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if got, ok := values.Global.Observability.Traces.OTLP["endpoint"]; !ok || got != "" {
			t.Errorf("observability=%v: global.observability.traces.otlp.endpoint = %v (set: %v), want \"\"", observability, got, ok)
		}
	}
}

// A released 3.x meta chart (platform.chartVersion below 4.0.0 on the stable
// channel) gets the 3.x lab shape: the chart's closed root schema and the
// kagent 0.10 wrapper it resolves refuse every key of the current line, so
// none of them render — no platform Postgres or CNPG operator, no Substrate,
// and a kagent block with the plain controller route, the local-dev auth
// mode and the monitor following observability (the 0.10 controller serves
// /metrics). Everything else is the current line's render, byte for byte:
// the legacy values are exactly the 4.x values minus those keys.
func TestPlatformValuesLegacyChartShape(t *testing.T) {
	// The GitHub token keys the 4.x line alone takes (githubtoken.go) stay
	// out of both renders: no githubToken.source is recorded — that half is
	// TestGitHubTokenLegacyChartTakesNoKeys'.
	render := func(cfg *config.Config) (map[string]any, string) {
		out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values, string(out)
	}
	current := config.Default()
	legacy := config.Default()
	legacy.Platform.ChartVersion = legacyChartVersion
	if !legacy.LegacyChart() || current.LegacyChart() {
		t.Fatalf("LegacyChart(): 3.23.1=%v %s=%v", legacy.LegacyChart(), current.Platform.ChartVersion, current.LegacyChart())
	}
	currentValues, currentOut := render(current)
	legacyValues, legacyOut := render(legacy)

	kagent := legacyValues["kagent"].(map[string]any)
	for _, key := range []string{"postgres", "substrate"} {
		if _, ok := legacyValues[key]; ok {
			t.Errorf("3.x shape: root %s rendered (the 3.x root schema is closed)", key)
		}
	}
	if _, ok := legacyValues["components"].(map[string]any)["cloudnative-pg"]; ok {
		t.Error("3.x shape: components.cloudnative-pg rendered (kagent 0.10 runs its bundled Postgres)")
	}
	for _, key := range []string{fieldHarness, "database", "substrateWorkerPool"} {
		if _, ok := kagent[key]; ok {
			t.Errorf("3.x shape: kagent.%s rendered (the 0.10 wrapper's schema is closed)", key)
		}
	}
	if _, ok := kagent["controllerRoute"].(map[string]any)["jwtAuthentication"]; ok || kagent["controllerRoute"].(map[string]any)["enabled"] != true {
		t.Errorf("3.x shape: kagent.controllerRoute = %v, want enabled without jwtAuthentication", kagent["controllerRoute"])
	}
	controller := kagent["controller"].(map[string]any)
	for _, key := range []string{"volumes", "volumeMounts"} {
		if _, ok := controller[key]; ok {
			t.Errorf("3.x shape: kagent.controller.%s rendered (no platform Postgres to mount)", key)
		}
	}
	if _, ok := controller["authorization"]; ok {
		t.Error("3.x shape: kagent.controller.authorization rendered (the 0.10 wrapper's schema is closed)")
	}
	// The 4.x controller reviews callers as the lab's API server names them:
	// oidc: before the user and every group (kind-config.yaml.tmpl), or every
	// RoleBinding of the lab grants nothing at the controller.
	currentAuthz, _ := currentValues["kagent"].(map[string]any)["controller"].(map[string]any)["authorization"].(map[string]any)
	if currentAuthz["mode"] != authorizationModeKubernetes || currentAuthz["groupsClaim"] != tenantsTestGroupsClaim ||
		currentAuthz["usernamePrefix"] != apiserverUserPrefix || currentAuthz["groupsPrefix"] != apiserverUserPrefix {
		t.Errorf("4.x shape: kagent.controller.authorization = %v, want mode kubernetes, groupsClaim groups, usernamePrefix and groupsPrefix oidc:", currentAuthz)
	}
	if got := controller["auth"].(map[string]any)["mode"]; got != "unsecure" {
		t.Errorf("3.x shape: kagent.controller.auth.mode = %v, want unsecure (no JWT policy in front)", got)
	}
	if got := kagent["serviceMonitor"].(map[string]any)["enabled"]; got != true {
		t.Errorf("3.x shape: kagent.serviceMonitor.enabled = %v, want observability (%v)", got, legacy.Platform.Observability)
	}
	if got := kagent["providers"].(map[string]any)["default"]; got != "anthropic" {
		t.Errorf("3.x shape: kagent.providers.default = %v, want anthropic", got)
	}
	if got := kagent["ui"].(map[string]any)["service"].(map[string]any)["type"]; got != "NodePort" {
		t.Errorf("3.x shape: kagent.ui.service.type = %v, want NodePort", got)
	}

	// The legacy render is the current one minus the 4.x keys — nothing else
	// moves between the shapes.
	expected := currentValues
	delete(expected, "postgres")
	delete(expected, "substrate")
	delete(expected["components"].(map[string]any), "cloudnative-pg")
	delete(expected["mcp-kubernetes"].(map[string]any), "mcpServer")
	expectedKagent := expected["kagent"].(map[string]any)
	delete(expectedKagent, fieldHarness)
	delete(expectedKagent, "database")
	delete(expectedKagent, "substrateWorkerPool")
	delete(expectedKagent["controllerRoute"].(map[string]any), "jwtAuthentication")
	delete(expectedKagent["controller"].(map[string]any), "volumes")
	delete(expectedKagent["controller"].(map[string]any), "volumeMounts")
	delete(expectedKagent["controller"].(map[string]any), "authorization")
	expectedKagent["controller"].(map[string]any)["auth"].(map[string]any)["mode"] = "unsecure"
	expectedKagent["serviceMonitor"].(map[string]any)["enabled"] = legacy.Platform.Observability
	dropOverlayChecksum(t, expected)
	if !reflect.DeepEqual(expected, legacyValues) {
		t.Errorf("the 3.x values differ from the 4.x values in more than the 4.x keys and the overlay's checksum:\n--- 4.x minus the keys\n%s\n--- 3.x\n%s", mustYAML(t, expected), mustYAML(t, legacyValues))
	}

	// The switch follows the chart, not the channel: a 4.x release, a 5.x
	// one and a 4.x dev build render the current line's bytes, a dev build
	// of a 3.x-line branch the 3.x line's.
	for name, tc := range map[string]struct {
		mutate func(*config.Config)
		want   string
	}{
		"4.x release": {func(c *config.Config) { c.Platform.ChartVersion = "4.93.0" }, currentOut},
		"5.x release": {func(c *config.Config) { c.Platform.ChartVersion = "5.0.0" }, currentOut},
		"dev build of main": {func(c *config.Config) {
			c.Platform.ChartVersion, c.Platform.ChartBranch = "4.106.1-rbf28cd64t20261001120000h7f841be", testRefMain
		}, currentOut},
		"dev build of a 3.x-line branch": {func(c *config.Config) {
			c.Platform.ChartVersion, c.Platform.ChartBranch = "3.23.5-r1f2e3d4ct20261001120000h7f841be", "fix-v3-gateway"
		}, legacyOut},
	} {
		cfg := config.Default()
		tc.mutate(cfg)
		if _, out := render(cfg); out != tc.want {
			t.Errorf("%s: the render is not its line's", name)
		}
	}
	// An upgrade seed's 4.x release below the families floor renders the
	// current line's values without mcp-kubernetes.mcpServer, which its
	// mcp-kubernetes chart refuses.
	seed := config.Default()
	seed.Platform.ChartVersion, seed.Platform.UpgradeSeed = upgradeSeedVersion, true
	seedValues, _ := render(seed)
	seedExpected, _ := render(current)
	delete(seedExpected["mcp-kubernetes"].(map[string]any), "mcpServer")
	dropOverlayChecksum(t, seedExpected)
	if !reflect.DeepEqual(seedExpected, seedValues) {
		t.Errorf("the upgrade seed's values differ from the 4.x values in more than mcp-kubernetes.mcpServer and the overlay's checksum:\n--- 4.x minus the key\n%s\n--- seed\n%s", mustYAML(t, seedExpected), mustYAML(t, seedValues))
	}
	// A chart directory renders its roster's line, whatever pin is left
	// over, plus its connectivity source
	// (TestPlatformValuesLocalConnectivityChart) and nothing else: Substrate
	// in the roster is the current line, none the 3.x line.
	for name, tc := range map[string]struct {
		roster string
		want   *config.Config
	}{
		"4.x chart directory": {"components:\n  kagent: {}\n  substrate: {}\n", current},
		"3.x chart directory": {"components:\n  kagent: {}\n", legacy},
	} {
		directory := config.Default()
		directory.Platform.ChartVersion = legacyChartVersion
		if tc.want == legacy {
			directory.Platform.ChartVersion = config.DefaultChartVersion
		}
		directory.Platform.ChartPath = writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{
			chartYAML:     metaChartYAML,
			"values.yaml": tc.roster,
		})
		directoryValues, _ := render(directory)
		components, _ := directoryValues["components"].(map[string]any)
		connectivity, _ := components[config.ConnectivityChartName].(map[string]any)
		if _, set := connectivity["repository"]; !set {
			t.Errorf("%s: the render names no connectivity source", name)
		}
		for _, key := range connectivitySourceKeys {
			delete(connectivity, key)
		}
		// Rendered afresh: the comparison above stripped the 4.x keys off
		// the first render's values.
		wantValues, _ := render(tc.want)
		if !reflect.DeepEqual(directoryValues, wantValues) {
			t.Errorf("%s: the render differs from its line's in more than the connectivity source:\n--- directory\n%s\n--- want\n%s", name, mustYAML(t, directoryValues), mustYAML(t, wantValues))
		}
	}
	if legacyOut == currentOut {
		t.Error("the 3.x render must differ from the current line's")
	}
}

func mustYAML(t *testing.T, v any) string {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The 4.x line's topology is the lab's shape: Agent Substrate and the
// platform Postgres come with the chart, so the lab renders no worker image
// of its own (the chart pins the Substrate version), turns the CNPG
// component on with the agents, puts kagent's database on the platform
// Cluster and mounts its derived Secret, names the Harness's snapshot store
// (the chart's bundled RustFS) and runs the controller route with the JWT
// Strict policy against the lab Dex — trusted-proxy at the controller.
func TestPlatformValuesFourXTopology(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	at := func(path ...string) any {
		var cur any = values
		for _, key := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		return cur
	}
	if at("kagent", "substrateWorkerPool", "workerImage") != nil {
		t.Error("kagent.substrateWorkerPool.workerImage rendered: the chart pins the worker image with its Substrate version, the lab must not")
	}
	// The pool's CPU feature-set pin is the lab's one worker-pool value. A
	// render without a cluster falls back to the binary's architecture; the
	// node's is what `agentlab platform` renders (clusterWorkerPoolArch).
	if got := at("kagent", "substrateWorkerPool", "template", "nodeSelector", workerPoolArchLabel); got != runtime.GOARCH {
		t.Errorf("the WorkerPool's arch pin = %v, want the render's fallback %s", got, runtime.GOARCH)
	}
	if at("kagent", "substrateWorkerPool", "template", "resources") != nil {
		t.Error("kagent.substrateWorkerPool.template.resources rendered: Helm merges the map, so the worker resources stay the chart's")
	}
	if at("components", "cloudnative-pg", "enabled") != true || at("postgres", "enabled") != true || at("postgres", "namespace") != kagentNamespace {
		t.Errorf("the platform Postgres: components.cloudnative-pg.enabled=%v postgres.enabled=%v postgres.namespace=%v", at("components", "cloudnative-pg", "enabled"), at("postgres", "enabled"), at("postgres", "namespace"))
	}
	if at("postgres", "instances") != 1 || at("postgres", "vector", "enabled") != true || at("postgres", "vector", "extensionImage", "reference") == "" {
		t.Errorf("the lab's Cluster: one instance with the vector extension image, got instances=%v vector=%v", at("postgres", "instances"), at("postgres", "vector"))
	}
	if at("kagent", "database", "postgres", "urlFile") != "/etc/cnpg/uri" || at("kagent", "database", "postgres", "bundled", "enabled") != false {
		t.Errorf("kagent.database.postgres = %v, want the urlFile mount and the bundled instance off", at("kagent", "database", "postgres"))
	}
	volumes, _ := at("kagent", "controller", "volumes").([]any)
	if len(volumes) != 1 || volumes[0].(map[string]any)["secret"].(map[string]any)["secretName"] != "kagent-pg-kagent-v2-app" {
		t.Errorf("kagent.controller.volumes must mount the derived kagent-pg-kagent-v2-app Secret, got %v", volumes)
	}
	if at("kagent", "harness", "snapshotLocation") != "s3://ate-snapshots/kagent" || at("substrate", "rustfs", "enabled") != true {
		t.Errorf("snapshot store: kagent.harness.snapshotLocation=%v substrate.rustfs.enabled=%v", at("kagent", "harness", "snapshotLocation"), at("substrate", "rustfs", "enabled"))
	}
	// The lab registry rewrite rides in the same substrate block (a second
	// `substrate:` key would replace the first in the values merge) and the
	// Harness keeps the chart's digest until a `harness` dev image pins it.
	// The image-cache policy (ateletImageCacheArgs) follows it in the same
	// list — the host disk must not evict the Harness image.
	wantArgs := append([]any{testRegistryFlag}, anySlice(ateletImageCacheArgs)...)
	if args, _ := at("substrate", "atelet", "extraArgs").([]any); !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("substrate.atelet.extraArgs = %v, want the lab registry rewrite then the image-cache policy %v", args, wantArgs)
	}
	if at("kagent", "harness", "image") != nil {
		t.Errorf("kagent.harness.image = %v, want the chart's pin without a harness dev image", at("kagent", "harness", "image"))
	}
	if at("kagent", "controller", "auth", "mode") != "trusted-proxy" {
		t.Errorf("kagent.controller.auth.mode = %v, want trusted-proxy (the JWT policy is the first layer)", at("kagent", "controller", "auth", "mode"))
	}
	jwt := at("kagent", "controllerRoute", "jwtAuthentication")
	if at("kagent", "controllerRoute", "jwtAuthentication", "enabled") != true || at("kagent", "controllerRoute", "jwtAuthentication", "mode") != "Strict" ||
		at("kagent", "controllerRoute", "jwtAuthentication", "jwks", "host") != "dex.dex.svc.cluster.local" || at("kagent", "controllerRoute", "jwtAuthentication", "jwks", "path") != "/dex/keys" ||
		at("kagent", "controllerRoute", "jwtAuthentication", "jwks", "tls", "enabled") != true {
		t.Errorf("kagent.controllerRoute.jwtAuthentication = %v, want Strict against the lab Dex's JWKS over TLS", jwt)
	}
	if at("gateway", "jwksEgress", "enabled") != true || at("gateway", "jwksEgress", "namespace") != "dex" {
		t.Errorf("gateway.jwksEgress = %v, want the dex namespace declared", at("gateway", "jwksEgress"))
	}
	// Without agents nothing of it renders: no runtime, no Postgres.
	cfg.Platform.Agents = false
	cfg.Platform.ModelManager.Enabled = false
	out, err = renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cloudnative-pg") || strings.Contains(string(out), "\npostgres:") || strings.Contains(string(out), "\nsubstrate:") {
		t.Errorf("without agents the Postgres and Substrate blocks must not render:\n%s", excerptAround(string(out), "postgres"))
	}
}

// The klaus-gateway component (platform.klausGateway) renders only when on:
// the component entry with the Slack Web API patch (and a dev image's
// override when one is named), and the `klausGateway:` block with the
// in-cluster a2a target, Slack on the placeholder Secret, no web key, the OBO
// link store in a Secret with the lab's keys Secret, the ServiceMonitor
// following observability.
func TestPlatformValuesKlausGateway(t *testing.T) {
	render := func(cfg *config.Config) func(path ...string) any {
		out, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return func(path ...string) any {
			var cur any = values
			for _, key := range path {
				m, ok := cur.(map[string]any)
				if !ok {
					return nil
				}
				cur = m[key]
			}
			return cur
		}
	}
	const block = "klausGateway"
	off := render(config.Default())
	if off("components", klausGatewayComponent) != nil || off(block) != nil {
		t.Fatalf("off by default: components.klaus-gateway=%v klausGateway=%v", off("components", klausGatewayComponent), off(block))
	}

	cfg := config.Default()
	cfg.Platform.KlausGateway.Enabled = true
	cfg.Platform.Observability = false
	at := render(cfg)
	if at("components", klausGatewayComponent, "enabled") != true {
		t.Errorf("components.klaus-gateway.enabled = %v", at("components", klausGatewayComponent, "enabled"))
	}
	// The lab's two patches without a dev image: the Slack Web API base on the
	// Service in front of klaus-gateway-test's fake, and the lab CA trusted
	// for muster's public URL.
	prs, _ := at("components", klausGatewayComponent, "postRenderers").([]any)
	if len(prs) != 1 || !strings.Contains(fmt.Sprint(prs), "name: "+slackAPIBaseEnv) || !strings.Contains(fmt.Sprint(prs), "value: "+klausGatewaySlackAPIBase) ||
		!strings.Contains(fmt.Sprint(prs), "name: "+sslCertFileEnv) || !strings.Contains(fmt.Sprint(prs), "secretName: "+labCASecret) {
		t.Errorf("without a dev image the lab patches the Slack Web API base and the lab CA: %v", at("components", klausGatewayComponent, "postRenderers"))
	}
	// The block's keys as dotted paths under klausGateway.
	checks := map[string]any{
		"a2a.enabled":            true,
		"a2a.url":                klausGatewayInClusterTarget,
		"a2a.namespace":          kagentNamespace,
		"a2a.defaultAgent":       klausGatewayTestAgent,
		"slack.enabled":          true,
		"slack.mode":             "events",
		"slack.secretName":       klausGatewaySlackPlaceholder,
		"obo.enabled":            true,
		"obo.store":              oboStoreSecretBackend,
		"obo.musterUrl":          cfg.MusterBaseURL(),
		"obo.callbackBaseUrl":    "https://agentgateway." + cfg.Platform.Domain,
		"obo.existingSecret":     klausGatewayOBOKeys,
		"serviceMonitor.enabled": false,
	}
	for path, want := range checks {
		if got := at(append([]string{block}, strings.Split(path, ".")...)...); got != want {
			t.Errorf("%s.%s = %v, want %v", block, path, got, want)
		}
	}
	if at(block, "web") != nil {
		t.Errorf("the Slack-only gateway's closed schema has no web key: %v", at(block, "web"))
	}
	if at(block, "agentgatewayRoute") != nil {
		t.Errorf("the edge route for the channels stays the meta chart's default (off): %v", at(block, "agentgatewayRoute"))
	}
	// The refusal notices: the interval alone, the chart derives the rest;
	// a chart before the notices' floor gets no refusals block, which its
	// klaus-gateway's closed schema would refuse.
	if refusals, _ := at(block, "refusals").(map[string]any); len(refusals) != 1 || refusals["interval"] != "10s" {
		t.Errorf("klausGateway.refusals = %v, want the interval 10s alone", at(block, "refusals"))
	}
	older := *cfg
	older.Platform.ChartVersion = "4.153.0"
	if got := render(&older)(block, "refusals"); got != nil {
		t.Errorf("klausGateway.refusals on agent-platform 4.153.0 = %v, want none", got)
	}

	// A dev image rides the component's postRenderers as a kustomize image
	// override on the chart's image name, with the pull-policy patch on the
	// Deployment's container.
	cfg.Platform.DevImages = map[string]string{klausGatewayComponent: "klaus-gateway:dev-5a6b"}
	at = render(cfg)
	prs, _ = at("components", klausGatewayComponent, "postRenderers").([]any)
	if len(prs) != 1 {
		t.Fatalf("postRenderers with a dev image: %v", at("components", klausGatewayComponent, "postRenderers"))
	}
	kustomize := prs[0].(map[string]any)["kustomize"].(map[string]any)
	imgs, _ := kustomize["images"].([]any)
	if len(imgs) != 1 || imgs[0].(map[string]any)["name"] != "gsoci.azurecr.io/giantswarm/klaus-gateway" || imgs[0].(map[string]any)["newTag"] != "dev-5a6b" {
		t.Errorf("klaus-gateway dev image override: %v", kustomize["images"])
	}
	patches, _ := kustomize["patches"].([]any)
	if len(patches) != 3 || !strings.Contains(fmt.Sprint(patches[0]), slackAPIBaseEnv) || !strings.Contains(fmt.Sprint(patches[1]), sslCertFileEnv) ||
		!strings.Contains(fmt.Sprint(patches[2]), "imagePullPolicy: IfNotPresent") || !strings.Contains(fmt.Sprint(patches[2]), "name: klaus-gateway") {
		t.Errorf("klaus-gateway Slack Web API, lab CA and pull-policy patches: %v", kustomize["patches"])
	}
}

// The serving switch's render: the llm-d components and the modelServing
// switch on, the classic controller off with the llm-d controller rendering
// the shared objects and naming the models Gateway, the lab's CPU preset on
// its runtime image, the models Gateway with the lab Dex's JWKS, the kserve
// backend in model-manager's list (alone, in the one-backend form, when no
// host server is on), the models host rewritten to the Gateway's data plane
// inside pods — and none of it while the switch is off.
func TestPlatformValuesServing(t *testing.T) {
	render := func(t *testing.T, cfg *config.Config) (map[string]any, string) {
		t.Helper()
		out, err := renderTemplate(cfg, platformValuesTemplate, nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values, string(out)
	}
	components := func(values map[string]any) map[string]any {
		comps, _ := values["components"].(map[string]any)
		return comps
	}
	cfg := config.Default()
	cfg.Platform.ChartVersion = "4.40.0"
	cfg.Platform.ModelManager.Enabled = false
	values, _ := render(t, cfg)
	for _, key := range []string{"kserve-crd", "kserve-resources", "kserve-llmisvc-crd", "kserve-llmisvc-resources", "kserve-runtime-configs", "modelServing"} {
		if _, ok := components(values)[key]; ok {
			t.Errorf("components.%s rendered while platform.serving is off", key)
		}
	}
	if _, ok := values["modelServing"]; ok {
		t.Errorf("modelServing block rendered while platform.serving is off: %v", values["modelServing"])
	}

	cfg.Platform.Serving.Enabled = true
	values, raw := render(t, cfg)
	// The llm-d control plane alone: the classic KServe controller went with
	// the classic InferenceService path (giantswarm/agent-platform#574), and a
	// roster entry for it fails the chart's render.
	for _, key := range []string{"kserve-llmisvc-crd", "kserve-llmisvc-resources", "kserve-runtime-configs", "modelServing"} {
		comp, _ := components(values)[key].(map[string]any)
		if comp["enabled"] != true {
			t.Errorf("components.%s = %v, want enabled: true", key, comp)
		}
	}
	for _, key := range []string{"kserve-crd", "kserve-resources"} {
		if _, ok := components(values)[key]; ok {
			t.Errorf("components.%s rendered: the classic KServe controller is gone from the chart, and the entry is refused", key)
		}
		if _, ok := values[key]; ok {
			t.Errorf("a %s values block rendered: the chart's schema refuses it", key)
		}
	}
	if !strings.Contains(raw, "kserve-llmisvc-resources:\n  kserve:\n    storage:\n      resources:\n        limits:\n          memory: 8Gi") {
		t.Error("the storage-initializer's memory limit is not raised on the llm-d control plane's release (no Kyverno policy does it here)")
	}
	ms, _ := values["modelServing"].(map[string]any)
	presets, _ := ms["presets"].([]any)
	if len(presets) != 1 {
		t.Fatalf("modelServing.presets = %v, want the lab preset alone", presets)
	}
	preset := presets[0].(map[string]any)
	if name := preset["metadata"].(map[string]any)["name"]; name != servingPresetName {
		t.Errorf("the lab preset is %v, want %s", name, servingPresetName)
	}
	spec := preset["spec"].(map[string]any)
	if gpus := spec["resources"].(map[string]any)["gpus"]; gpus != 0 {
		t.Errorf("the lab preset requests %v GPUs, want 0", gpus)
	}
	if !strings.Contains(raw, "image: "+servingRuntimeImage) {
		t.Errorf("the lab preset does not name the CPU runtime %s", servingRuntimeImage)
	}
	gateway, _ := ms["modelsGateway"].(map[string]any)
	if gateway["enabled"] != true || gateway["name"] != modelsGatewayName {
		t.Errorf("modelServing.modelsGateway = %v, want enabled with name %s", gateway, modelsGatewayName)
	}
	if !strings.Contains(raw, "host: dex.dex.svc.cluster.local") {
		t.Error("the models Gateway's JWT policy does not name the lab Dex's JWKS")
	}
	mm, _ := values["model-manager"].(map[string]any)
	if mm["backend"] != "kserve" {
		t.Errorf("model-manager = %v, want the one-backend form with kserve (no host server)", mm)
	}
	if _, ok := mm["kserve"]; ok {
		t.Error("model-manager.kserve rendered with an endpoint: the kserve backend has none")
	}

	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama}}
	out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.ModelManagerEndpoints = map[string]string{ollama: ollamaLabEndpoint} })
	if err != nil {
		t.Fatal(err)
	}
	want := "model-manager:\n  backends:\n    - ollama\n    - kserve\n  ollama:\n    endpoint: \"http://172.21.0.1:11434\"\n  muster:"
	if !strings.Contains(string(out), want) {
		t.Errorf("a host server and the serving switch: want\n%s\nin\n%s", want, excerptAround(string(out), "model-manager:\n"))
	}

	coredns, err := renderTemplate(cfg, "coredns.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Anchored at the start, ahead of the wildcard: a pod's search
	// expansions of the models host reach the Gateway, never the edge.
	domain := strings.ReplaceAll(cfg.Platform.Domain, ".", `\.`)
	models := strings.Index(string(coredns), "name regex ^"+modelsGatewayName+`\.`+domain+`\. `+modelsGatewayName+".agent-platform.svc.cluster.local")
	wildcard := strings.Index(string(coredns), `name regex ^(.*)\.`+domain+`\.$ agentgateway-edge.agent-platform.svc.cluster.local`)
	if wildcard < 0 {
		t.Errorf("CoreDNS lost the anchored wildcard rewrite to the edge:\n%s", coredns)
	}
	if models < 0 || models > wildcard {
		t.Errorf("CoreDNS does not rewrite the models host to the Gateway's data plane ahead of the edge wildcard:\n%s", coredns)
	}
}

// connectivitySourceKeys are the keys a chartPath lab sets on the
// connectivity component to install the checkout's chart.
var connectivitySourceKeys = []string{"repository", "insecure", "versionRange"}

// A chartPath lab's values point the connectivity component at the lab
// registry (the kind-network name pods pull from, plain HTTP) at the meta
// chart directory's own version — the knobs the meta chart admits on a
// component released with it for a chart pushed by hand. A registry chart
// sets nothing for the component: its connectivity is published with it.
func TestPlatformValuesLocalConnectivityChart(t *testing.T) {
	cfg := config.Default()
	cfg.ClusterName = "agentlab-x"
	components := func() map[string]any {
		t.Helper()
		out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values struct {
			Components map[string]any `yaml:"components"`
		}
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return values.Components
	}
	// The source keys alone: the component's postRenderers (the HSTS patch)
	// are there either way.
	source := func() map[string]any {
		t.Helper()
		connectivity, _ := components()[config.ConnectivityChartName].(map[string]any)
		out := map[string]any{}
		for _, key := range connectivitySourceKeys {
			if v, ok := connectivity[key]; ok {
				out[key] = v
			}
		}
		return out
	}
	if got := source(); len(got) != 0 {
		t.Errorf("a registry chart's values name a connectivity source: %v", got)
	}

	cfg.Platform.ChartPath = writeChartFiles(t, t.TempDir(), "agent-platform", map[string]string{
		chartYAML: metaChartYAML,
	})
	got := source()
	want := map[string]any{"repository": "oci://agentlab-x-registry:5000/charts", "insecure": true, "versionRange": placeholderChartVersion}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("components.%s = %v, want %v", config.ConnectivityChartName, got, want)
	}
}

// Pods reach this machine by the lab host name (agentlab#299): the Corefile
// carries its hosts entry once the kind gateway is known, and none before.
func TestCoreDNSLabHostEntry(t *testing.T) {
	cfg := config.Default()
	coredns, err := renderTemplate(cfg, "coredns.yaml.tmpl", func(d *tmplData) { d.LabHostIP = "172.21.0.1" })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(coredns), "hosts {\n            172.21.0.1 "+labHostName+"\n            fallthrough\n        }") {
		t.Errorf("CoreDNS does not resolve %s to the gateway:\n%s", labHostName, coredns)
	}
	coredns, err = renderTemplate(cfg, "coredns.yaml.tmpl", func(d *tmplData) { d.LabHostIP = "" })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(coredns), labHostName) {
		t.Errorf("a hosts entry with no gateway known:\n%s", coredns)
	}
	if strings.HasSuffix(labHostName, "."+cfg.Platform.Domain) {
		t.Errorf("%s is under the lab domain, so the wildcard rewrite to the edge takes it", labHostName)
	}
}

// dropOverlayChecksum removes the checksum of the lab overlay's
// extraAppConfig entry, which the backstage chart of an older line refuses.
func dropOverlayChecksum(t *testing.T, values map[string]any) {
	t.Helper()
	entries := values["backstage"].(map[string]any)["backstage"].(map[string]any)["extraAppConfig"].([]any)
	overlay := entries[len(entries)-1].(map[string]any)
	if overlay["configMapRef"] != labAppConfigMap || overlay["checksum"] == "" {
		t.Fatalf("the last extraAppConfig entry is not the overlay with its checksum: %v", overlay)
	}
	delete(overlay, "checksum")
}

// platform.devImages.workspace-manager rides the roster's trailing range:
// the component follows platform.workspaces (the chart's workspaces.enabled)
// and the roster names no block for it, so its entry carries the image
// override and the pull-policy patch on Deployment workspace-manager and no
// `enabled` the chart would refuse.
func TestPlatformValuesWorkspaceManagerDevImage(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.DevImages = map[string]string{workspaceManager: "workspace-manager:dev-5a6b"}
	postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.PostRenderers = postRenderers })
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Components map[string]map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wm := values.Components[workspaceManager]
	if _, ok := wm["enabled"]; ok {
		t.Errorf("components.%s.enabled = %v: the component follows workspaces.enabled, the roster sets none", workspaceManager, wm["enabled"])
	}
	prs, _ := wm["postRenderers"].([]any)
	if len(prs) != 1 {
		t.Fatalf("components.%s.postRenderers: want the one entry, got %v", workspaceManager, wm)
	}
	k := prs[0].(map[string]any)["kustomize"].(map[string]any)
	if imgs, _ := k["images"].([]any); len(imgs) != 1 || imgs[0].(map[string]any)["name"] != "gsoci.azurecr.io/giantswarm/workspace-manager" || imgs[0].(map[string]any)["newName"] != "localhost/workspace-manager" || imgs[0].(map[string]any)["newTag"] != "dev-5a6b" {
		t.Errorf("workspace-manager dev image override: %v", k["images"])
	}
	patches, _ := k["patches"].([]any)
	if len(patches) != 2 || !strings.Contains(fmt.Sprint(patches[0]), "name: workspace-manager\n          imagePullPolicy: IfNotPresent") {
		t.Fatalf("workspace-manager: want the pull-policy and the sync-image patch on container workspace-manager, got %v", k["patches"])
	}
	if env := workspaceManagerPatchEnv(t, patches[1]); env[syncImageEnv] != "localhost/workspace-manager:dev-5a6b" || env[syncImagePullPolicyEnv] != "IfNotPresent" {
		t.Errorf("workspace-manager sync image: want %s=localhost/workspace-manager:dev-5a6b and %s=IfNotPresent, got %v", syncImageEnv, syncImagePullPolicyEnv, env)
	}
}

// Without platform.devImages.workspace-manager the manager is told no sync
// image, so its sync Jobs keep the chart's.
func TestPlatformValuesWorkspaceManagerSyncImageChartDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.DevImages = map[string]string{"muster": "muster:dev-1a2b"}
	postRenderers, err := componentPostRenderers(cfg, defaultDevImageNames(cfg), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.PostRenderers = postRenderers })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), syncImageEnv) {
		t.Errorf("the values set %s without the workspace-manager dev image:\n%s", syncImageEnv, out)
	}
}

// workspaceManagerPatchEnv parses one postRenderers patch on Deployment
// workspace-manager and returns its container workspace-manager's env.
func workspaceManagerPatchEnv(t *testing.T, entry any) map[string]string {
	t.Helper()
	e := entry.(map[string]any)
	if target := e["target"].(map[string]any); target[kindKey] != kindDeployment || target[nameKey] != workspaceManager {
		t.Fatalf("patch target: want Deployment %s, got %v", workspaceManager, target)
	}
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `yaml:"name"`
						Env  []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(e["patch"].(string)), &d); err != nil {
		t.Fatalf("%v\n%s", err, e["patch"])
	}
	env := map[string]string{}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != workspaceManager {
			t.Errorf("patch container %q, want %s", c.Name, workspaceManager)
		}
		for _, v := range c.Env {
			env[v.Name] = v.Value
		}
	}
	return env
}
