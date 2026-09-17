package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/pkg/project"
)

// The platform signal's payload keys, as docs/telemetry.md lists them, and
// the two values a switch takes on the wire.
const (
	keyChartVersion  = "chartVersion"
	keyChartMajor    = "chartMajor"
	keyChartChannel  = "chartChannel"
	keyChartPinned   = "chartPinned"
	keyLegacyShape   = "legacyShape"
	keyAgents        = "agents"
	keyObservability = "observability"
	keyFakeFleet     = "fakeFleet"
	keyModelManager  = "modelManager"
	keyVMManager     = "vmManager"
	keyKlausGateway  = "klausGateway"
	keyBackstage     = "backstage"
	on, off          = "true", "false"
	legacyVersion    = "3.23.1"
)

// platformKeys is the platform signal's payload beside the parameters the
// library adds: exactly these, every one a version or a bool. A new key is a
// docs/telemetry.md change too.
var platformKeys = []string{keyChartVersion, keyChartMajor, keyChartChannel, keyChartPinned, keyLegacyShape,
	keyAgents, keyObservability, keyFakeFleet, keyModelManager, keyVMManager, keyKlausGateway, keyBackstage}

// versionOrBool is the shape of a payload value: a version (a dev tag has
// dots, hyphens and a hash), a number, true/false, a channel name. Not a
// path, not an e-mail address, not a token.
var versionOrBool = regexp.MustCompile(`^[A-Za-z0-9.+-]*$`)

// TestPlatformPostsTheChartLine: one signal per install with the chart line
// and the switches as strings, one shape per channel — and nothing that
// points back at the machine: a chart directory reports its Chart.yaml
// version, never its path.
func TestPlatformPostsTheChartLine(t *testing.T) {
	const (
		devTag  = "4.29.0-dev.main.2026-09-17.10-00-00.habc1234"
		homeDir = "/home/someone/src/agent-platform/helm/agent-platform"
	)
	for _, tc := range []struct {
		name         string
		configure    func(cfg *config.Config)
		chartVersion string
		want         map[string]string
	}{
		{
			name:         "a released 4.x chart, the default lab",
			configure:    func(*config.Config) {},
			chartVersion: config.DefaultChartVersion,
			want: map[string]string{keyChartVersion: config.DefaultChartVersion, keyChartMajor: "4", keyChartChannel: config.ChartChannelStable,
				keyChartPinned: off, keyLegacyShape: off, keyAgents: on, keyObservability: on, keyFakeFleet: off,
				keyModelManager: off, keyVMManager: off, keyKlausGateway: off, keyBackstage: on},
		},
		{
			name: "a released 3.x chart, the legacy shape, every switch flipped",
			configure: func(cfg *config.Config) {
				cfg.Platform.ChartVersion = legacyVersion
				cfg.Platform.Agents, cfg.Platform.Observability, cfg.Platform.FakeFleet = false, false, true
				cfg.Platform.ModelManager.Enabled, cfg.Platform.VMManager.Enabled, cfg.Platform.KlausGateway.Enabled = true, true, true
				cfg.Backstage.Enabled = false
			},
			chartVersion: legacyVersion,
			// model-manager and klaus-gateway come with the agents: off while
			// the agents are — the effective switch, not the file's key.
			want: map[string]string{keyChartVersion: legacyVersion, keyChartMajor: "3", keyChartChannel: config.ChartChannelStable,
				keyChartPinned: off, keyLegacyShape: on, keyAgents: off, keyObservability: off, keyFakeFleet: on,
				keyModelManager: off, keyVMManager: on, keyKlausGateway: off, keyBackstage: off},
		},
		{
			name: "the dev channel, pinned at a build",
			configure: func(cfg *config.Config) {
				cfg.Platform.ChartBranch = "main"
				cfg.Platform.ChartVersion = devTag
				cfg.Platform.ChartPinned = true
			},
			chartVersion: devTag,
			want: map[string]string{keyChartVersion: devTag, keyChartMajor: "4", keyChartChannel: config.ChartChannelDev,
				keyChartPinned: on, keyLegacyShape: off},
		},
		{
			name: "a chart directory: its Chart.yaml version, never its path",
			configure: func(cfg *config.Config) {
				cfg.Platform.ChartPath = homeDir
				cfg.Platform.ChartVersion = legacyVersion // ignored while chartPath is set
			},
			chartVersion: "4.30.0",
			want:         map[string]string{keyChartVersion: "4.30.0", keyChartMajor: "4", keyChartChannel: config.ChartChannelPath, keyLegacyShape: off},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := capture(t)
			cfg := config.Default()
			tc.configure(cfg)

			Platform(context.Background(), cfg, tc.chartVersion)

			signals := received(t, got)
			if len(signals) != 1 {
				t.Fatalf("got %d signals, want 1", len(signals))
			}
			s := signals[0]
			if s["type"] != platformSignalType {
				t.Errorf("type %v, want %s", s["type"], platformSignalType)
			}
			if s["isTestMode"] != true {
				t.Errorf("isTestMode %v, want true under %s", s["isTestMode"], TestModeEnv)
			}
			payload, _ := s["payload"].(map[string]any)
			for k, want := range tc.want {
				if payload[k] != want {
					t.Errorf("payload.%s = %v, want %q", k, payload[k], want)
				}
			}
			for _, k := range platformKeys {
				if _, ok := payload[k]; !ok {
					t.Errorf("payload lacks %s", k)
				}
			}
			// What must never leave the machine: anything from the config
			// that names a place or a person. The keys are exactly the
			// documented ones, and every value is a version or a bool.
			leaks := []string{cfg.Platform.ChartPath, cfg.Platform.Domain, cfg.ClusterName}
			for _, u := range cfg.Users {
				leaks = append(leaks, u.Email, u.Password)
			}
			for k, v := range payload {
				if strings.HasPrefix(k, "TelemetryDeck.") {
					continue
				}
				if !slices.Contains(platformKeys, k) {
					t.Errorf("payload carries %q, which docs/telemetry.md does not list", k)
				}
				str, ok := v.(string)
				if !ok || !versionOrBool.MatchString(str) {
					t.Errorf("payload.%s = %#v, want a version or a bool as a string", k, v)
				}
				for _, leak := range leaks {
					if leak != "" && strings.Contains(str, leak) {
						t.Errorf("payload.%s = %q carries %q from the config", k, str, leak)
					}
				}
			}
			if payload["TelemetryDeck.AppInfo.version"] != project.Version() {
				t.Errorf("payload[TelemetryDeck.AppInfo.version] %v, want %s", payload["TelemetryDeck.AppInfo.version"], project.Version())
			}
		})
	}
}

func TestPlatformSendsNothingWhenOptedOutOrUnconfigured(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- r.URL.Path }))
	defer srv.Close()
	enable(t, srv)
	cfg := config.Default()

	t.Setenv(OptOutEnv, "1")
	Platform(context.Background(), cfg, config.DefaultChartVersion)

	t.Setenv(OptOutEnv, "")
	t.Setenv(doNotTrackEnv, "1")
	Platform(context.Background(), cfg, config.DefaultChartVersion)

	t.Setenv(doNotTrackEnv, "")
	withAppID(t, "")
	Platform(context.Background(), cfg, config.DefaultChartVersion)

	select {
	case p := <-hit:
		t.Fatalf("a platform signal was sent (%s) despite the opt-out / empty app ID", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestPlatformRidesTheCommandsClient: the platform signal goes through the
// client the command signal created, so the one Flush at exit waits for both
// — a lab whose install fails right after resolving the chart still reports
// the line it was about to install.
func TestPlatformRidesTheCommandsClient(t *testing.T) {
	types := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // a slow network
		body, _ := io.ReadAll(r.Body)
		var signals []map[string]any
		if err := json.Unmarshal(body, &signals); err != nil {
			t.Errorf("body is not a signal array: %v\n%s", err, body)
		}
		for _, s := range signals {
			typ, _ := s["type"].(string)
			types <- typ
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	enable(t, srv)

	Command(lab(t, "up"))
	first := sender
	Platform(context.Background(), config.Default(), config.DefaultChartVersion)
	if sender != first {
		t.Fatal("Platform created a client of its own; Flush would wait for the command signal only")
	}
	Flush(context.Background())

	var got []string
	for range 2 {
		select {
		case typ := <-types:
			got = append(got, typ)
		default:
			t.Fatalf("Flush returned with %v delivered, want both signals", got)
		}
	}
	want := []string{signalType, platformSignalType}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("delivered %v, want %v", got, want)
	}
}
