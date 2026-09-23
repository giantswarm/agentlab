package lab

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/giantswarm/agentlab/internal/config"
)

// closedRegistry is an oci:// registry address nothing listens on — a port
// that was just free on loopback — so every dial is refused, deterministically
// and without leaving the host: the lab's DNS failure in miniature.
func closedRegistry(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// gsoci and gsociAPI are the chart registry the fixtures fail to reach, by
// name and by its OCI distribution API root.
const (
	gsoci    = "gsoci.azurecr.io"
	gsociAPI = "https://" + gsoci + "/v2/"
)

// dialFailure is a failed TCP dial as net.Dial reports one.
func dialFailure(err error) *net.OpError { return &net.OpError{Op: opDial, Net: "tcp", Err: err} }

// quickRetries makes retryUnreachable's waits instant for the test, keeping
// two retries — three attempts.
func quickRetries(t *testing.T) {
	t.Helper()
	saved := renderRetryDelays
	renderRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { renderRetryDelays = saved })
}

// TestRegistryUnreachable: the failures a retry can mend — the resolver, the
// dialer, a timeout, a registry that answers 5xx or 429 — are told apart from
// a registry that answered and a chart that failed, by the error's type, and
// through Helm's own wrapping of a real refused dial.
func TestRegistryUnreachable(t *testing.T) {
	dnsErr := &net.DNSError{Err: noSuchHost, Name: gsoci, IsNotFound: true}
	dial := dialFailure(dnsErr)
	get := &url.Error{Op: opGet, URL: gsociAPI + "charts/giantswarm/mcp-kubernetes/tags/list", Err: dial}
	status := func(code int) error {
		return fmt.Errorf("failed to perform %q on source: %w", "FetchReference", &errcode.ErrorResponse{Method: "GET", StatusCode: code})
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a name the resolver cannot answer, as Helm wraps it", fmt.Errorf("helm template x: %w", get), true},
		{"a refused dial", dialFailure(os.NewSyscallError("connect", syscall.ECONNREFUSED)), true},
		{"a timeout", &url.Error{Op: opGet, URL: gsociAPI, Err: os.ErrDeadlineExceeded}, true},
		{"a registry answering 503", status(503), true},
		{"a registry answering 429", status(429), true},
		{"a tag that is not there", status(404), false},
		{"a denied pull", status(401), false},
		{"a chart that refuses its values", &schemaRejection{err: errors.New(helmSchemaPrefix + " x")}, false},
		{"a template that fails", errors.New("execution error at (x/templates/a.yaml:1:1): boom"), false},
		{"no error", nil, false},
	} {
		if got := registryUnreachable(tc.err); got != tc.want {
			t.Errorf("%s: registryUnreachable = %v, want %v (%v)", tc.name, got, tc.want, tc.err)
		}
	}

	isolateHelm(t)
	ref := "oci://" + closedRegistry(t) + "/charts/component"
	_, _, err := helmTemplate("default", "component", ref, "0.1.0", map[string]any{}, nil)
	if !registryUnreachable(err) {
		t.Errorf("Helm's render against a registry nothing listens on is not unreachable: %v", err)
	}
	if _, err := helmChartTags(ref); !registryUnreachable(err) {
		t.Errorf("Helm's tag listing against a registry nothing listens on is not unreachable: %v", err)
	}
}

// TestRetryUnreachable: a render the resolver or the dialer refused is tried
// again up to the bound and succeeds once the network is back; any other
// failure is returned at once, and so is a timeout or a status the transport
// has already retried; an outage that outlasts the retries returns its last
// error, still recognisable as unreachable.
func TestRetryUnreachable(t *testing.T) {
	quickRetries(t)
	refused := dialFailure(os.NewSyscallError("connect", syscall.ECONNREFUSED))
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

	render, calls := counting(2, refused)
	if err := retryUnreachable("rendering component", render); err != nil || *calls != 3 {
		t.Errorf("a registry back on the last attempt: err %v after %d calls, want nil after 3", err, *calls)
	}

	failing := errors.New("execution error at (x/templates/a.yaml:1:1): boom")
	render, calls = counting(renderAttempts(), failing)
	if err := retryUnreachable("rendering component", render); !errors.Is(err, failing) || *calls != 1 {
		t.Errorf("a render that failed on its own: err %v after %d calls, want it at once after 1", err, *calls)
	}

	render, calls = counting(renderAttempts()+1, refused)
	if err := retryUnreachable("rendering component", render); !registryUnreachable(err) || *calls != renderAttempts() {
		t.Errorf("an outage past the retries: err %v after %d calls, want the refused dial after %d", err, *calls, renderAttempts())
	}

	// What oras-go's transport already retried five times per request is
	// not retried again: unreachable, and returned at once.
	for _, spent := range []error{
		&url.Error{Op: opGet, URL: gsociAPI, Err: os.ErrDeadlineExceeded},
		dialFailure(&net.DNSError{Err: "i/o timeout", Name: gsoci, IsTimeout: true}),
		&errcode.ErrorResponse{Method: "GET", StatusCode: 503},
	} {
		render, calls = counting(renderAttempts(), spent)
		if err := retryUnreachable("rendering component", render); !registryUnreachable(err) || *calls != 1 {
			t.Errorf("%v: err %v after %d calls, want it unreachable at once after 1", spent, err, *calls)
		}
	}
}

// TestPlatformImagesStopsOnAnUnreachableRegistry: a component chart the
// registry does not answer for through the retries stops the boot before the
// install — naming the release, the chart, the dialer's words, what the lab
// cannot patch without the render and the command that picks the boot up
// again — instead of noting a skip and installing the component unpatched.
// A skip in the same pass stays a skip, and a refusal next to an unreachable
// chart is reported with it.
func TestPlatformImagesStopsOnAnUnreachableRegistry(t *testing.T) {
	quickRetries(t)
	dir := isolateHelm(t)
	closed := writeClosedSchemaChart(t, dir)
	registry := closedRegistry(t)
	cfg := &config.Config{}
	chart := platformChart{ref: config.ChartRepository, version: "0.0.0-test"}

	release := func(name, url, values string) fluxRelease {
		t.Helper()
		releases, err := fluxReleases(strings.ReplaceAll(componentManifest(url, "", values), "component", name))
		if err != nil || len(releases) != 1 {
			t.Fatalf("fixture %s joined %d releases (%v), want 1", name, len(releases), err)
		}
		return releases[0]
	}
	unreachable := release("mcp-kubernetes", "oci://"+registry+"/charts/mcp-kubernetes", "    known: a\n")
	skipped := release("skipped", closed, "    boom: true\n")
	refused := release("refused", closed, "    nosuchkey: 1\n")
	rendered := release("rendered", closed, "    known: a\n")
	roster := func(releases ...fluxRelease) *platformRoster {
		return &platformRoster{chart: chart, releases: releases}
	}

	_, renders, err := platformImages(cfg, roster(unreachable, skipped, rendered))
	if err == nil {
		t.Fatal("a component chart the registry did not answer for did not stop the install")
	}
	if renders != nil {
		t.Errorf("a stopped boot handed renders on: %v", renders)
	}
	for _, want := range []string{
		"1 component chart(s) could not be rendered",
		"through the retries",
		"mcp-kubernetes (oci://" + registry + "/charts/mcp-kubernetes 0.1.0)",
		connectionRefused,
		"the dex-localhost sidecar, platform.devImages",
		"network and DNS for " + registry,
		"`agentlab platform`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the stop does not say %q:\n%s", want, err)
		}
	}
	for _, unwanted := range []string{"skipped", "rendered (", "..."} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the stop says %q — a skip, a render or a truncation that is no part of it:\n%s", unwanted, err)
		}
	}

	_, _, err = platformImages(cfg, roster(refused, unreachable))
	if err == nil || !strings.Contains(err.Error(), "could not be rendered") || !strings.Contains(err.Error(), "nosuchkey") {
		t.Errorf("an unreachable chart and a refusal in one pass are not both reported:\n%v", err)
	}

	// Without the unreachable chart the same pass proceeds: the skip is a
	// note and the skipped release is the one the sidecar rule did not read.
	withSkip := roster(skipped, rendered)
	_, renders, err = platformImages(cfg, withSkip)
	if err != nil {
		t.Fatalf("a skip stopped the install: %v", err)
	}
	if got := withSkip.unrendered(renders); len(got) != 1 || got[0] != "skipped" {
		t.Errorf("the releases without a render are %v, want [skipped]", got)
	}
}

// TestRenderPlatformRosterUnreachable: the meta chart's own render retries an
// unreachable registry and hands the caller an error it recognises as such,
// worded with the chart, the host and the command to run again.
func TestRenderPlatformRosterUnreachable(t *testing.T) {
	quickRetries(t)
	isolateHelm(t)
	registry := closedRegistry(t)
	chart := platformChart{ref: "oci://" + registry + "/charts/agent-platform", version: "4.49.0"}
	_, err := renderPlatformRoster(chart, map[string]any{})
	if !registryUnreachable(err) {
		t.Fatalf("the meta chart's render against a registry nothing listens on is not unreachable: %v", err)
	}
	stop := chartUnreachableError(chart, err, "agentlab up").Error()
	for _, want := range []string{"cannot render", "4.49.0", "through the retries", connectionRefused, "network and DNS for " + registry, "`agentlab up`"} {
		if !strings.Contains(stop, want) {
			t.Errorf("the stop does not say %q:\n%s", want, stop)
		}
	}
}

// TestUnreachableCause: the cause a retry note names is the dialer's or the
// resolver's own words, not Helm's invocation around them.
func TestUnreachableCause(t *testing.T) {
	dnsErr := &net.DNSError{Err: noSuchHost, Name: gsoci, IsNotFound: true}
	err := fmt.Errorf("helm template mcp-kubernetes: %w", &url.Error{Op: opGet, URL: gsociAPI, Err: dialFailure(dnsErr)})
	if got, want := fmt.Sprint(unreachableCause(err)), "dial tcp: lookup gsoci.azurecr.io: no such host"; got != want {
		t.Errorf("unreachableCause = %q, want %q", got, want)
	}
}
