package lab

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// A kagent chart render of each line, reduced to what the name resolution
// reads: the controller Deployment and its containers.
const (
	lineKagentRender = `---
# Source: kagent/templates/ui-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-ui
spec:
  template:
    spec:
      containers:
        - name: ui
          image: "ghcr.io/giantswarm/kagent/ui:0.11.0-gs.3"
---
# Source: kagent/templates/controller-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-controller
spec:
  template:
    spec:
      containers:
        - name: controller
          image: ghcr.io/giantswarm/kagent/controller:0.11.0-gs.3
          imagePullPolicy: IfNotPresent
`
	wrapperKagentRender = `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: kagent-controller
data:
  IMAGE_TAG: 0.10.1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-controller
spec:
  template:
    spec:
      containers:
        - name: controller
          image: gsoci.azurecr.io/giantswarm/kagent-controller:0.10.1
`
)

// The controller override replaces whatever name the line the lab follows
// renders on Deployment kagent-controller's `controller` container: the fork's
// name on the kagent line, the wrapper's on 3.x. A target whose chart rendered
// without that Deployment/container is refused (kustomize would drop the
// override silently); one whose chart did not render at all falls back to the
// table's name.
func TestResolveDevImageNames(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.DevImages = map[string]string{componentKagent: devControllerRef, config.DevImageHarness: devHarnessRef}

	names, err := resolveDevImageNames(cfg, map[string]string{componentKagent: lineKagentRender})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[componentKagent]; got != lineControllerImage {
		t.Errorf("kagent line: resolved %q, want %q", got, lineControllerImage)
	}
	if _, ok := names[config.DevImageHarness]; ok {
		t.Errorf("the harness target resolves no Deployment name, got %q", names[config.DevImageHarness])
	}

	names, err = resolveDevImageNames(cfg, map[string]string{componentKagent: wrapperKagentRender})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names[componentKagent], "gsoci.azurecr.io/giantswarm/kagent-controller"; got != want {
		t.Errorf("3.x wrapper: resolved %q, want %q", got, want)
	}

	// The chart rendered, but not the Deployment/container the swap patches.
	_, err = resolveDevImageNames(cfg, map[string]string{componentKagent: strings.ReplaceAll(lineKagentRender, "name: controller\n", "name: manager\n")})
	if err == nil || !strings.Contains(err.Error(), "no Deployment kagent-controller with a container controller") || !strings.Contains(err.Error(), "match nothing") {
		t.Errorf("unmatched target: want the refusal naming the Deployment and the silent drop, got %v", err)
	}

	// No render for the component at all: the table's name, unverified.
	names, err = resolveDevImageNames(cfg, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[componentKagent]; got != devImageTargets[componentKagent].image {
		t.Errorf("no render: want the table's fallback %q, got %q", devImageTargets[componentKagent].image, got)
	}
	names, err = resolveDevImageNames(cfg, nil)
	if err != nil || names[componentKagent] != devImageTargets[componentKagent].image {
		t.Errorf("nil renders: want the table's fallback, got %v %v", names, err)
	}
}

func TestRenderedContainerImage(t *testing.T) {
	if img, ok := renderedContainerImage(lineKagentRender, "kagent-controller", "controller"); !ok || img != "ghcr.io/giantswarm/kagent/controller:0.11.0-gs.3" {
		t.Errorf("controller image = %q, %v", img, ok)
	}
	if img, ok := renderedContainerImage(lineKagentRender, "kagent-ui", "ui"); !ok || img != "ghcr.io/giantswarm/kagent/ui:0.11.0-gs.3" {
		t.Errorf("quoted image = %q, %v", img, ok)
	}
	if _, ok := renderedContainerImage(lineKagentRender, "kagent-controller", "ui"); ok {
		t.Error("the ui container is not on the controller Deployment")
	}
	if _, ok := renderedContainerImage("# nothing rendered\n", "kagent-controller", "controller"); ok {
		t.Error("an empty render has no Deployment")
	}
	// A document that is no object (a stray scalar) does not stop the scan.
	if img, ok := renderedContainerImage("---\njust a string\n"+lineKagentRender, "kagent-controller", "controller"); !ok || img == "" {
		t.Error("a non-object document must be skipped, not fatal")
	}
}

func TestImageName(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/giantswarm/kagent/controller:0.11.0-gs.3":                "ghcr.io/giantswarm/kagent/controller",
		"gsoci.azurecr.io/giantswarm/kagent-controller:0.10.1":            "gsoci.azurecr.io/giantswarm/kagent-controller",
		"localhost:5001/golang-adk@sha256:" + strings.Repeat("a", 64):     "localhost:5001/golang-adk",
		"localhost:5001/golang-adk:dev@sha256:" + strings.Repeat("a", 64): "localhost:5001/golang-adk",
		"registry:5000/team/muster":                                       "registry:5000/team/muster",
		componentMuster:                                                   componentMuster,
	}
	for ref, want := range cases {
		if got := imageName(ref); got != want {
			t.Errorf("imageName(%q) = %q, want %q", ref, got, want)
		}
	}
}

// The lab registry serves the harness image under the ref's path, the ref's
// own registry dropped, its tag kept; a digest ref gets a tag of its own.
func TestDevImageRepoTag(t *testing.T) {
	cases := []struct{ ref, repo, tag string }{
		{"golang-adk:dev-139-abc1234", devHarnessRepo, "dev-139-abc1234"},
		{"giantswarm/kagent/golang-adk:dev", "giantswarm/kagent/golang-adk", devTag},
		{"ghcr.io/giantswarm/kagent/golang-adk:0.11.0-gs.3", "giantswarm/kagent/golang-adk", "0.11.0-gs.3"},
		{"localhost:5000/golang-adk:dev", devHarnessRepo, devTag},
		{"registry:5000/team/golang-adk:dev", "team/golang-adk", devTag},
		{"golang-adk@sha256:" + strings.Repeat("ab", 32), devHarnessRepo, "sha256-abababababab"},
	}
	for _, c := range cases {
		repo, tag := devImageRepoTag(c.ref)
		if repo != c.repo || tag != c.tag {
			t.Errorf("devImageRepoTag(%q) = %q, %q; want %q, %q", c.ref, repo, tag, c.repo, c.tag)
		}
	}
}

// The registry's spellings: the container named after the cluster on the
// kind network (atelet's replacement endpoint), the host's loopback port for
// docker push and the Harness ref.
func TestDevRegistryNames(t *testing.T) {
	cfg := config.Default()
	cfg.ClusterName = "agentlab-dev2"
	cfg.Platform.DevRegistryPort = 5011
	if got := devRegistryName(cfg); got != "agentlab-dev2-registry" {
		t.Errorf("name = %q", got)
	}
	if got := devRegistryEndpoint(cfg); got != "agentlab-dev2-registry:5000" {
		t.Errorf("endpoint = %q", got)
	}
	if got := devRegistryHost(cfg); got != "localhost:5011" {
		t.Errorf("host = %q", got)
	}
	if got := devRegistryURL(cfg); got != "http://127.0.0.1:5011" {
		t.Errorf("url = %q", got)
	}
}

// The pinned digest is the registry's own answer for the pushed tag's
// manifest (Docker-Content-Digest of a HEAD with the manifest media types),
// and anything but a sha256 digest is refused.
func TestRegistryManifestDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3f", 32)
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/v2/giantswarm/kagent/golang-adk/manifests/dev-139" {
			http.NotFound(w, r)
			return
		}
		accept = r.Header.Get("Accept")
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	got, err := registryManifestDigest(srv.URL, "giantswarm/kagent/golang-adk", "dev-139")
	if err != nil || got != digest {
		t.Errorf("digest = %q, %v", got, err)
	}
	for _, mt := range []string{"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.v2+json"} {
		if !strings.Contains(accept, mt) {
			t.Errorf("Accept lacks %s: %q", mt, accept)
		}
	}
	if _, err := registryManifestDigest(srv.URL, "giantswarm/kagent/golang-adk", "missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing tag must fail with the status, got %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", "md5:abc")
	}))
	defer bad.Close()
	if _, err := registryManifestDigest(bad.URL, "x", "y"); err == nil || !strings.Contains(err.Error(), "not a sha256 digest") {
		t.Errorf("a non-sha256 digest must be refused, got %v", err)
	}
}

// The merged values must still pin the Harness to the dev image: an overlay
// that sets kagent.harness.image wins the merge and is reported.
func TestDevImagesCheckValues(t *testing.T) {
	cfg := config.Default()
	pinned := "localhost:5001/golang-adk@sha256:" + strings.Repeat("3f", 32)
	dev := &devImages{harness: pinned}
	// The merged values with kagent.harness.image set to img and atelet's
	// extraArgs as given.
	merged := func(img any, extraArgs []any) map[string]any {
		return map[string]any{
			componentKagent: map[string]any{config.DevImageHarness: map[string]any{"image": img}},
			"substrate":     map[string]any{"atelet": map[string]any{"extraArgs": extraArgs}},
		}
	}
	flag := registryFlag(cfg)
	if flag != "--localhost-registry-replacement=agentlab-registry:5000" {
		t.Errorf("registryFlag = %q", flag)
	}
	if err := dev.checkValues(cfg, merged(pinned, []any{flag})); err != nil {
		t.Errorf("the pin and the flag survived, got %v", err)
	}
	overridden := merged("ghcr.io/giantswarm/kagent/golang-adk@sha256:"+strings.Repeat("a2", 32), []any{flag})
	if err := dev.checkValues(cfg, overridden); err == nil || !strings.Contains(err.Error(), "platform.valuesFiles") {
		t.Errorf("an overlay's pin must be reported, got %v", err)
	}
	// Lists replace in a Helm merge: an overlay's own extraArgs drop the flag.
	if err := dev.checkValues(cfg, merged(pinned, []any{"--v=2"})); err == nil || !strings.Contains(err.Error(), "extraArgs") {
		t.Errorf("an overlay replacing the atelet args must be reported, got %v", err)
	}
	if err := dev.checkValues(cfg, merged(pinned, nil)); err == nil {
		t.Error("missing atelet args must be reported")
	}
	if err := (&devImages{}).checkValues(cfg, overridden); err != nil {
		t.Errorf("without a harness target nothing is checked, got %v", err)
	}
	if got := nestedString(map[string]any{componentKagent: "flat"}, componentKagent, config.DevImageHarness, "image"); got != "" {
		t.Errorf("nestedString through a non-map = %q", got)
	}
}

func TestRevisionChange(t *testing.T) {
	if got := revisionChange("", "r2"); got != "r2" {
		t.Errorf("first revision: %q", got)
	}
	if got := revisionChange("r1", "r2"); got != "r1 -> r2" {
		t.Errorf("moved revision: %q", got)
	}
	if got := revisionChange("r2", "r2"); got != "r2" {
		t.Errorf("unchanged revision: %q", got)
	}
}

// The values template carries the swap the way the chart forwards it: the
// Harness pin under kagent.harness.image while a harness dev image is
// configured, the atelet flag under substrate.atelet whenever the agents are.
func TestValuesTemplateHarnessDevImage(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Agents = true
	cfg.Platform.DevImages = map[string]string{config.DevImageHarness: devHarnessRef}
	pinned := "localhost:5001/golang-adk@sha256:" + strings.Repeat("3f", 32)
	dev := &devImages{harness: pinned}
	mutate, err := dev.templateData(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderTemplate(cfg, platformValuesTemplate, mutate)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(out)
	const flagBlock = "  atelet:\n    extraArgs:\n      - --localhost-registry-replacement=agentlab-registry:5000\n"
	for _, want := range []string{"    image: " + pinned + "\n", flagBlock} {
		if !strings.Contains(rendered, want) {
			t.Errorf("values lack %q:\n%s", want, excerptAround(rendered, "  harness:"))
		}
	}
	// Without the target the Harness keeps the chart's digest, but the atelet
	// flag stays: inert, and in place before any swap.
	cfg.Platform.DevImages = nil
	out, err = renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "\n    image: ") || !strings.Contains(s, flagBlock) {
		t.Errorf("without a harness dev image the values must not pin the Harness and must still carry the atelet flag:\n%s", excerptAround(s, "substrate:"))
	}
	// No agents, no Harness, no flag.
	cfg.Platform.Agents = false
	out, err = renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "localhost-registry-replacement") {
		t.Errorf("without agents the values must not carry the atelet flag")
	}
}
