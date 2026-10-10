package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// chartAgentgatewayControllerImage is the controller's name as the
// agentgateway chart renders it (the table's fallback too).
const chartAgentgatewayControllerImage = "gsoci.azurecr.io/giantswarm/agentgateway-upstream/controller"

// agentgateway chart renders reduced to what the swap reads: the controller
// Deployment the chart renders, and nothing of the proxy, which the
// controller creates at run time.
const agentgatewayRender = `---
# Source: agentgateway/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agentgateway-controller
spec:
  template:
    spec:
      containers:
        - name: controller
          image: "gsoci.azurecr.io/giantswarm/agentgateway-upstream/controller:2.3.3"
          env:
            - name: AGW_PROXY_IMAGE_REGISTRY
              value: gsoci.azurecr.io
            - name: AGW_PROXY_IMAGE_REPOSITORY
              value: giantswarm/agentgateway-upstream/agentgateway
            - name: AGW_PROXY_IMAGE_TAG
              value: "2.3.3"
`

// The proxy's dev image is the three values the chart hands the controller,
// split off the ref the build is side-loaded under: a build of this host
// under localhost/, a registry ref as written, its port kept with the
// registry and the tag after the last colon of the path.
func TestAgentgatewayDevImage(t *testing.T) {
	cfg := config.Default()
	if got := agentgatewayDevImage(cfg); got != nil {
		t.Fatalf("without the target: %+v", got)
	}
	cases := map[string]agentgatewayProxyImage{
		"agentgateway:dev-9e0f":                      {Registry: localhostName, Repository: agentgatewayComponent, Tag: "dev-9e0f"},
		"giantswarm/agentgateway:dev":                {Registry: localhostName, Repository: "giantswarm/agentgateway", Tag: devTag},
		"localhost:5001/agentgateway:dev":            {Registry: "localhost:5001", Repository: agentgatewayComponent, Tag: devTag},
		"gsoci.azurecr.io/giantswarm/agentgateway-upstream/agentgateway:2.3.3": {Registry: "gsoci.azurecr.io", Repository: "giantswarm/agentgateway-upstream/agentgateway", Tag: "2.3.3"},
	}
	for ref, want := range cases {
		cfg.Platform.DevImages = map[string]string{agentgatewayComponent: ref}
		got := agentgatewayDevImage(cfg)
		if got == nil || *got != want {
			t.Errorf("agentgatewayDevImage(%q) = %+v, want %+v", ref, got, want)
			continue
		}
		if got.ref() != labImageRef(ref) {
			t.Errorf("ref() of %q = %q, want the side-loaded name %q", ref, got.ref(), labImageRef(ref))
		}
	}
}

// The merged values must still carry the proxy image under
// agentgateway.proxy.image: an overlay that sets any of the three wins the
// merge and is reported; without the target nothing is checked.
func TestAgentgatewayDevImageCheckValues(t *testing.T) {
	p := &agentgatewayProxyImage{Registry: localhostName, Repository: agentgatewayComponent, Tag: "dev-9e0f"}
	values := func(registry, repository, tag string) map[string]any {
		return map[string]any{agentgatewayComponent: map[string]any{"proxy": map[string]any{"image": map[string]any{proxyImageRegistryKey: registry, proxyImageRepositoryKey: repository, proxyImageTagKey: tag}}}}
	}
	if err := p.checkValues(values(localhostName, agentgatewayComponent, "dev-9e0f")); err != nil {
		t.Errorf("the proxy image survived, got %v", err)
	}
	if err := p.checkValues(values("gsoci.azurecr.io", agentgatewayComponent, "dev-9e0f")); err == nil || !strings.Contains(err.Error(), "platform.valuesFiles") || !strings.Contains(err.Error(), proxyImageRegistryKey) {
		t.Errorf("an overlay's registry must be reported, got %v", err)
	}
	if err := p.checkValues(map[string]any{}); err == nil {
		t.Error("values without the block must be reported")
	}
	var none *agentgatewayProxyImage
	if err := none.checkValues(map[string]any{}); err != nil {
		t.Errorf("without the target nothing is checked, got %v", err)
	}
	// The run's devImages checks the proxy before the harness.
	dev := &devImages{proxy: p}
	if err := dev.checkValues(config.Default(), values("gsoci.azurecr.io", agentgatewayComponent, "dev-9e0f")); err == nil || !strings.Contains(err.Error(), agentgatewayComponent) {
		t.Errorf("devImages.checkValues must report the proxy, got %v", err)
	}
}

// The controller target's name is read off the agentgateway component's
// render — the release its patch lands on — not off a render of its own key.
func TestResolveDevImageNamesByRelease(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.DevImages = map[string]string{config.DevImageAgentgatewayController: "agentgateway-controller:dev", agentgatewayComponent: "agentgateway:dev"}
	names, err := resolveDevImageNames(cfg, map[string]string{agentgatewayComponent: agentgatewayRender})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[config.DevImageAgentgatewayController]; got != chartAgentgatewayControllerImage {
		t.Errorf("controller: resolved %q, want %q", got, chartAgentgatewayControllerImage)
	}
	if _, ok := names[agentgatewayComponent]; ok {
		t.Errorf("the proxy target resolves no Deployment name, got %q", names[agentgatewayComponent])
	}
	// The release rendered, but not the controller Deployment: refused by
	// the release's name.
	_, err = resolveDevImageNames(cfg, map[string]string{agentgatewayComponent: strings.ReplaceAll(agentgatewayRender, "name: controller\n", "name: manager\n")})
	if err == nil || !strings.Contains(err.Error(), "the agentgateway chart's render has no Deployment agentgateway-controller with a container controller") {
		t.Errorf("unmatched target: want the refusal naming the release and the Deployment, got %v", err)
	}
	if got := devImageTargets[config.DevImageAgentgatewayController].releaseOf(config.DevImageAgentgatewayController); got != agentgatewayComponent {
		t.Errorf("releaseOf = %q", got)
	}
	if got := devImageTargets[componentMuster].releaseOf(componentMuster); got != componentMuster {
		t.Errorf("a target of its own release: releaseOf = %q", got)
	}
}
