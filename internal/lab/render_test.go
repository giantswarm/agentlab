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
	if _, ok := at("kagent", "substrateWorkerPool").(map[string]any); ok {
		t.Error("kagent.substrateWorkerPool rendered: the chart pins the worker image with its Substrate version, the lab must not")
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
