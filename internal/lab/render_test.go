package lab

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/giantswarm/agentlab/internal/config"
)

// The umbrella values hand every host model server to the one model-manager:
// two or more backends render `model-manager.backends` with one endpoint
// block each; a single one renders the chart's one-backend form.
func TestModelManagerValuesRenderBackends(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Enabled, cfg.Platform.Agents = true, true
	cfg.Platform.ModelManager = config.ModelManager{Enabled: true, Backends: []string{ollama, lemonade}}
	endpoints := map[string]string{ollama: "http://172.21.0.1:11434", lemonade: "http://172.21.0.1:13305"}
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
	out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", func(d *tmplData) {
		d.ModelManagerEndpoints = map[string]string{ollama: "http://172.21.0.1:11434"}
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
	for _, c := range []string{"agentgateway", "agent-platform-mcps", componentMCPKubernetes, componentKagent, agentManagerMCPServer, componentBackstage, modelManagerMCPServer} {
		if at("components", c, "enabled") != true {
			t.Errorf("components.%s.enabled = %v, want true", c, at("components", c, "enabled"))
		}
	}
	if at("components", "dicebear", "enabled") != false {
		t.Errorf("components.dicebear.enabled = %v, want false", at("components", "dicebear", "enabled"))
	}
	for _, c := range []string{componentMuster, componentBackstage, componentKagent, componentMCPKubernetes, modelManagerMCPServer, agentManagerMCPServer} {
		if _, ok := at("components", c, "postRenderers").([]any); !ok {
			t.Errorf("components.%s.postRenderers missing: %v", c, at("components", c))
		}
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
		at("modelManager", "route", "enabled") != true || at(componentBackstage, "extraScopes") == nil {
		t.Errorf("wiring keys: mcp-kubernetes.kubernetesAudience=%v kagent.controllerRoute.enabled=%v modelManager.route.enabled=%v backstage.extraScopes=%v",
			at(componentMCPKubernetes, "kubernetesAudience"), at(componentKagent, "controllerRoute", "enabled"), at("modelManager", "route", "enabled"), at(componentBackstage, "extraScopes"))
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

// The kagent controller ServiceMonitor follows observability on the stable
// channel and is off on the dev channel, whose kagent serves no metrics
// listener; the chart-level gate stays with observability either way.
func TestKagentServiceMonitorFollowsChannel(t *testing.T) {
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
	if got := kagentMonitor(render()); got != true {
		t.Errorf("stable channel with observability: kagent.serviceMonitor.enabled = %v, want true", got)
	}
	cfg.Platform.ChartBranch = devChannelBranch
	v := render()
	if got := kagentMonitor(v); got != false {
		t.Errorf("dev channel: kagent.serviceMonitor.enabled = %v, want false", got)
	}
	if got := v["global"].(map[string]any)["observability"].(map[string]any)["metrics"].(map[string]any)["serviceMonitor"].(map[string]any)["enabled"]; got != true {
		t.Errorf("dev channel: the chart-level monitor gate must still follow observability, got %v", got)
	}
	cfg.Platform.Observability = false
	cfg.Platform.ChartBranch = ""
	if got := kagentMonitor(render()); got != false {
		t.Errorf("without observability: kagent.serviceMonitor.enabled = %v, want false", got)
	}
}

// On the dev channel the WorkerPool's worker image follows the lab's
// Substrate pin — control plane and workers at one version; the stable
// channel, whose kagent creates no WorkerPool, renders no such key (the
// released chart's schema does not know it).
func TestKagentWorkerImageFollowsSubstratePin(t *testing.T) {
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
	if _, ok := render()["kagent"].(map[string]any)["substrateWorkerPool"]; ok {
		t.Error("stable channel: kagent.substrateWorkerPool rendered, but the released chart has no WorkerPool")
	}
	cfg.Platform.ChartBranch = devChannelBranch
	pool, ok := render()["kagent"].(map[string]any)["substrateWorkerPool"].(map[string]any)
	if !ok {
		t.Fatal("dev channel: kagent.substrateWorkerPool not rendered")
	}
	if got := pool["workerImage"]; got != ateomGVisorImage {
		t.Errorf("dev channel: kagent.substrateWorkerPool.workerImage = %v, want %s", got, ateomGVisorImage)
	}
	if !strings.HasSuffix(ateomGVisorImage, ":"+substrateVersion) {
		t.Errorf("the worker image %s must carry the Substrate pin %s", ateomGVisorImage, substrateVersion)
	}
}

// The Substrate values are the chart's defaults with the lab's two
// deviations spelled out: no Namespace rendered (the lab creates it ahead of
// the chart) and no atelet extra args (no local registry).
func TestSubstrateValuesRender(t *testing.T) {
	out, err := renderTemplate(config.Default(), substrateValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if values["createNamespace"] != false {
		t.Errorf("createNamespace = %v, want false", values["createNamespace"])
	}
	if args, _ := values["atelet"].(map[string]any)["extraArgs"].([]any); len(args) != 0 {
		t.Errorf("atelet.extraArgs = %v, want none", args)
	}
	for _, store := range []string{"postgres", "rustfs"} {
		if size := values[store].(map[string]any)["storageSize"]; size != "1Gi" {
			t.Errorf("%s.storageSize = %v, want 1Gi", store, size)
		}
	}
}
