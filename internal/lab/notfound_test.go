package lab

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"oras.land/oras-go/v2/errdef"

	"github.com/giantswarm/agentlab/internal/config"
)

// branchBuild is a branch build's chart tag, the kind a valuesFiles overlay
// pins a component's versionRange to before the registry serves it.
const branchBuild = "2.2.1-r6483a910t20261015221226hbd18dbd"

// testChartVersion is the meta chart version the preload fixtures carry, and
// rerunPlatform the command a stop tells the person to run again.
const (
	testChartVersion = "0.0.0-test"
	rerunPlatform    = "`agentlab platform`"
)

// quickNotFoundRetries makes retryNotFound's waits instant for the test,
// keeping two retries — three attempts.
func quickNotFoundRetries(t *testing.T) {
	t.Helper()
	saved := notFoundRetryDelays
	notFoundRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { notFoundRetryDelays = saved })
}

// manifestUnknownRegistry is a plain-HTTP OCI registry on loopback that
// lists tag but answers every manifest request MANIFEST_UNKNOWN — a branch
// build whose tag shows before its manifest is served. It counts the
// manifest requests.
func manifestUnknownRegistry(t *testing.T, tag string) (host string, manifestRequests *atomic.Int32) {
	t.Helper()
	manifestRequests = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		switch {
		case r.URL.Path == "/v2/":
			_, _ = fmt.Fprint(w, "{}")
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			_, _ = fmt.Fprintf(w, `{"name":"charts/agent-manager","tags":[%q]}`, tag)
		case strings.Contains(r.URL.Path, "/manifests/"):
			manifestRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), manifestRequests
}

// TestChartNotFound: a chart the registry answered is not there — the
// manifest Helm fetches, through its own wrapping, and a tag no range and
// filter admit — is told apart from a registry that does not answer and a
// chart that fails on its own.
func TestChartNotFound(t *testing.T) {
	isolateHelm(t)
	host, _ := manifestUnknownRegistry(t, branchBuild)
	_, _, err := helmTemplateFrom("agent-platform", "agent-manager", "oci://"+host+"/charts/agent-manager", branchBuild, true, map[string]any{}, nil)
	if !chartNotFound(err) || !strings.Contains(err.Error(), `"FetchReference"`) {
		t.Errorf("Helm's render of a tag the registry has no manifest for is not a missing chart: %v", err)
	}
	_, err = pickFilteredTag([]string{"2.2.0"}, ">=2.2.1-0", `.*-r.*`)
	if !chartNotFound(err) {
		t.Errorf("no tag the range and the filter admit is not a missing chart: %v", err)
	}
	for _, other := range []error{
		dialFailure(os.NewSyscallError("connect", syscall.ECONNREFUSED)),
		&schemaRejection{err: errors.New(helmSchemaPrefix + " x")},
		errors.New("execution error at (x/templates/a.yaml:1:1): boom"),
		nil,
	} {
		if chartNotFound(other) {
			t.Errorf("%v is a missing chart", other)
		}
	}
}

// TestRetryNotFound: a render the registry has no chart for is tried again up
// to the bound and goes on once the build is published; any other failure is
// returned at once; a chart still missing past the retries returns its last
// error, still recognisable as missing.
func TestRetryNotFound(t *testing.T) {
	quickNotFoundRetries(t)
	missing := fmt.Errorf(`failed to perform "FetchReference" on source: x:%s: %w`, branchBuild, errdef.ErrNotFound)
	counting := func(failures int, failure error) (func() error, *int) {
		calls := 0
		return func() error {
			calls++
			if calls <= failures {
				return failure
			}
			return nil
		}, &calls
	}
	attempts := 1 + len(notFoundRetryDelays)

	render, calls := counting(attempts-1, missing)
	if err := retryNotFound("rendering agent-manager", render); err != nil || *calls != attempts {
		t.Errorf("a build published by the last attempt: err %v after %d calls, want nil after %d", err, *calls, attempts)
	}

	failing := errors.New("execution error at (x/templates/a.yaml:1:1): boom")
	render, calls = counting(attempts, failing)
	if err := retryNotFound("rendering agent-manager", render); !errors.Is(err, failing) || *calls != 1 {
		t.Errorf("a render that failed on its own: err %v after %d calls, want it at once after 1", err, *calls)
	}

	render, calls = counting(attempts+1, missing)
	if err := retryNotFound("rendering agent-manager", render); !chartNotFound(err) || *calls != attempts {
		t.Errorf("a chart missing past the retries: err %v after %d calls, want the missing chart after %d", err, *calls, attempts)
	}
}

// TestPlatformImagesStopsOnAMissingComponentChart: a component chart the
// registry has no manifest for — the agent-manager branch build of a
// valuesFiles overlay, listed before it is served — is retried and then stops
// the boot before the install, naming the release, the chart and its
// version, the registry's words, what the lab cannot patch without the render
// and the command to run again: the component is never installed without its
// dex-localhost sidecar. A skip in the same pass stays out of the stop.
func TestPlatformImagesStopsOnAMissingComponentChart(t *testing.T) {
	quickRetries(t)
	quickNotFoundRetries(t)
	dir := isolateHelm(t)
	closed := writeClosedSchemaChart(t, dir)
	host, manifestRequests := manifestUnknownRegistry(t, branchBuild)
	chartURL := "oci://" + host + "/charts/agent-manager"
	missing := fluxRelease{Name: waitingComponent, Namespace: platformNamespace, URL: chartURL, Version: branchBuild, PlainHTTP: true, Values: []byte("{}")}
	releases, err := fluxReleases(strings.ReplaceAll(componentManifest(closed, "", "    boom: true\n"), "component", "skipped"))
	if err != nil || len(releases) != 1 {
		t.Fatalf("fixture joined %d releases (%v), want 1", len(releases), err)
	}
	roster := &platformRoster{chart: platformChart{ref: config.ChartRepository, version: testChartVersion}, releases: []fluxRelease{missing, releases[0]}}

	_, renders, err := platformImages(&config.Config{}, roster)
	if err == nil {
		t.Fatal("a component chart the registry has no manifest for did not stop the install")
	}
	if renders != nil {
		t.Errorf("a stopped boot handed renders on: %v", renders)
	}
	for _, want := range []string{
		"1 component chart(s) could not be rendered, the registry has no such chart through the retries",
		waitingComponent + " (" + chartURL + " " + branchBuild + ")",
		`"FetchReference"`,
		"not found",
		"the dex-localhost sidecar, platform.devImages",
		"components.<name>.versionRange",
		rerunPlatform,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the stop does not say %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "skipped") {
		t.Errorf("the stop names the skipped release:\n%s", err)
	}
	if got, attempts := int(manifestRequests.Load()), 1+len(notFoundRetryDelays); got < attempts {
		t.Errorf("the registry was asked for the manifest %d times, want at least one per attempt (%d)", got, attempts)
	}
}
