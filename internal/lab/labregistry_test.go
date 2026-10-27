package lab

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab registry of the cluster `agentlab`, by its container and its
// kind-network endpoint, and the version the fixtures pin.
const (
	testLabRegistry         = "agentlab-registry"
	testLabRegistryEndpoint = testLabRegistry + ":5000"
	testKagentVersion       = "0.9.0"
)

// TestLocalizeLabRegistry: a release whose OCIRepository names the lab
// registry by its kind-network name — an overlay pointing a component at a
// chart pushed there — is rendered on the host through the registry's
// host-published port over plain HTTP, its URL (the HelmRelease's) left as
// it is; a release of any other registry, one rendered from a directory and
// a chart without the mapping are left alone.
func TestLocalizeLabRegistry(t *testing.T) {
	cfg := &config.Config{ClusterName: "agentlab"}
	cfg.Platform.DevRegistryPort = 5017
	registry := labRegistryAddressFor(cfg)
	const kagentURL = "oci://" + testLabRegistryEndpoint + "/charts/kagent"
	fresh := func() []fluxRelease {
		return []fluxRelease{
			{Name: componentMuster, URL: config.ChartRepository, Version: ">=5.0.0 <6.0.0"},
			{Name: componentKagent, URL: kagentURL, Version: testKagentVersion},
			{Name: config.ConnectivityChartName, URL: "oci://" + testLabRegistryEndpoint + "/charts/agent-platform-connectivity", LocalChart: "/src/agent-platform-connectivity"},
			{Name: "another-lab", URL: "oci://agentlab-2-registry:5000/charts/kagent"},
		}
	}

	releases := fresh()
	localizeLabRegistry(releases, registry)
	kagent := releases[1]
	if want := "oci://localhost:5017/charts/kagent"; kagent.HostURL != want || kagent.hostURL() != want {
		t.Errorf("the lab registry's release renders from %q (hostURL %q), want %q", kagent.HostURL, kagent.hostURL(), want)
	}
	if !kagent.PlainHTTP || kagent.LabRegistry != testLabRegistry || kagent.URL != kagentURL {
		t.Errorf("the lab registry's release: PlainHTTP %v, LabRegistry %q, URL %q; want plain HTTP, the container, the kind-network URL kept", kagent.PlainHTTP, kagent.LabRegistry, kagent.URL)
	}
	if label := kagent.chartLabel(""); !strings.Contains(label, kagentURL) || !strings.Contains(label, "localhost:5017") {
		t.Errorf("the label names neither the HelmRelease's URL nor the host's: %s", label)
	}
	for _, i := range []int{0, 2, 3} {
		if rel := releases[i]; rel.HostURL != "" || rel.PlainHTTP || rel.hostURL() != rel.URL {
			t.Errorf("%s (%s) was mapped onto the lab registry: %+v", rel.Name, rel.URL, rel)
		}
	}

	unmapped := fresh()
	localizeLabRegistry(unmapped, labRegistryAddress{})
	if unmapped[1].HostURL != "" {
		t.Errorf("a chart without the lab registry's address mapped %s to %q", unmapped[1].URL, unmapped[1].HostURL)
	}
}

// TestFluxReleasesReadsInsecure: an OCIRepository's spec.insecure reaches
// the host's render as plain HTTP, as source-controller reads it.
func TestFluxReleasesReadsInsecure(t *testing.T) {
	plain := strings.Replace(componentManifest("oci://172.18.0.3:5000/charts/component", "", "    known: a\n"), "  ref:\n", "  insecure: true\n  ref:\n", 1)
	for manifest, want := range map[string]bool{plain: true, componentManifest("oci://gsoci.azurecr.io/charts/component", "", "    known: a\n"): false} {
		releases, err := fluxReleases(manifest)
		if err != nil || len(releases) != 1 {
			t.Fatalf("joined %d releases (%v), want 1", len(releases), err)
		}
		if releases[0].PlainHTTP != want {
			t.Errorf("%s: PlainHTTP = %v, want %v", releases[0].URL, releases[0].PlainHTTP, want)
		}
	}
}

// TestRenderFluxReleaseFromTheLabRegistry: the render of a lab-registry
// release asks the host-published address over plain HTTP — the tag list
// for a filtered range and the chart itself — never the kind-network name.
// The fake registry answers 404, so the render fails as a registry that
// answered, not as an unreachable one.
func TestRenderFluxReleaseFromTheLabRegistry(t *testing.T) {
	isolateHelm(t)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	releases := []fluxRelease{{Name: componentKagent, Namespace: kagentNamespace, URL: "oci://" + testLabRegistryEndpoint + "/charts/kagent", Version: testKagentVersion}}
	localizeLabRegistry(releases, labRegistryAddress{container: testLabRegistry, endpoint: testLabRegistryEndpoint, host: host})
	_, _, err := renderFluxRelease(releases[0], nil)
	if err == nil {
		t.Fatal("a chart the registry does not hold rendered")
	}
	if registryUnreachable(err) || strings.Contains(err.Error(), testLabRegistryEndpoint) {
		t.Errorf("the render did not reach the host-published registry: %v", err)
	}
	if _, err := helmChartTagsFrom(releases[0].HostURL, true); err == nil || registryUnreachable(err) {
		t.Errorf("the tag list did not reach the registry over plain HTTP: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slicesContainsPrefix(paths, "/v2/charts/kagent/") {
		t.Errorf("the registry saw no request for the chart over plain HTTP: %v", paths)
	}
}

func slicesContainsPrefix(items []string, prefix string) bool {
	for _, s := range items {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// TestUnreachableLabRegistryNamesTheContainer: a lab-registry release whose
// render the host-published port refuses stops the boot naming the
// container and the address the host read it through, not the host's DNS.
func TestUnreachableLabRegistryNamesTheContainer(t *testing.T) {
	quickRetries(t)
	isolateHelm(t)
	closed := closedRegistry(t)
	releases := []fluxRelease{{Name: componentKagent, Namespace: kagentNamespace, URL: "oci://" + testLabRegistryEndpoint + "/charts/kagent", Version: testKagentVersion}}
	localizeLabRegistry(releases, labRegistryAddress{container: testLabRegistry, endpoint: testLabRegistryEndpoint, host: closed})
	_, _, _, errs := fluxReleaseImages(releases, nil)
	err := judgeRenderFailures(platformChart{ref: config.ChartRepository, version: releasedChartVersion}, errs)
	if err == nil {
		t.Fatal("an unreachable lab registry did not stop the boot")
	}
	for _, want := range []string{"lab registry " + testLabRegistry + " (read on this host as " + closed + ")", "docker ps -a", "platform.devRegistryPort", rerunPlatform} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the stop does not say %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "DNS") {
		t.Errorf("the stop blames the host's DNS for the lab registry:\n%s", err)
	}
}
