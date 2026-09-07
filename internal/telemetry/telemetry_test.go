package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/agentlab/pkg/project"
)

// testAppID stands in for the compiled-in app ID so the tests run whatever
// the source carries (an empty ID must switch reporting off, see the last test).
const testAppID = "00000000-0000-4000-8000-000000000000"

func withAppID(t *testing.T, id string) {
	t.Helper()
	prev := appID
	appID = id
	t.Cleanup(func() { appID = prev })
}

// lab returns a root command shaped like agentlab's, with the built-ins cobra
// adds and a hidden plumbing command (post-render here; cobra's __complete is
// hidden the same way and only exists during Execute), and the named leaf.
func lab(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "agentlab"}
	up := &cobra.Command{Use: "up", Run: func(*cobra.Command, []string) {}}
	postRender := &cobra.Command{Use: "post-render", Hidden: true, Run: func(*cobra.Command, []string) {}}
	root.AddCommand(up, postRender)
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()
	cmd, _, err := root.Find(path)
	if err != nil {
		t.Fatalf("find %v: %v", path, err)
	}
	return cmd
}

func TestUserFacingSkipsPlumbingCompletionAndHelp(t *testing.T) {
	for _, tc := range []struct {
		path []string
		want bool
	}{
		{[]string{"up"}, true},
		{[]string{"post-render"}, false},
		{[]string{"completion"}, false},
		{[]string{"completion", "zsh"}, false},
		{[]string{"help"}, false},
	} {
		if got := userFacing(lab(t, tc.path...)); got != tc.want {
			t.Errorf("userFacing(%v) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestEnabledHonoursTheOptOuts(t *testing.T) {
	withAppID(t, testAppID)
	t.Setenv(OptOutEnv, "")
	t.Setenv(doNotTrackEnv, "")
	if !Enabled() {
		t.Fatal("enabled by default")
	}
	t.Setenv(OptOutEnv, "1")
	if Enabled() {
		t.Errorf("%s set: still enabled", OptOutEnv)
	}
	t.Setenv(OptOutEnv, "")
	for _, v := range []string{"1", "true", "yes"} {
		t.Setenv(doNotTrackEnv, v)
		if Enabled() {
			t.Errorf("%s=%s: still enabled", doNotTrackEnv, v)
		}
	}
	for _, v := range []string{"0", "false", ""} {
		t.Setenv(doNotTrackEnv, v)
		if !Enabled() {
			t.Errorf("%s=%q: disabled", doNotTrackEnv, v)
		}
	}
}

// TestCommandPostsOneSignal captures the ingest request the way TelemetryDeck
// would receive it: one signal, kubectl-gs's type and payload shape, the user
// hashed, test mode flagged.
func TestCommandPostsOneSignal(t *testing.T) {
	withAppID(t, testAppID)
	t.Setenv(OptOutEnv, "")
	t.Setenv(doNotTrackEnv, "")
	t.Setenv(TestModeEnv, "1")

	got := make(chan []map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var signals []map[string]any
		if err := json.Unmarshal(body, &signals); err != nil {
			t.Errorf("body is not a signal array: %v\n%s", err, body)
		}
		got <- signals
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	endpoint = srv.URL
	defer func() { endpoint = "" }()

	Command(lab(t, "up"))

	select {
	case signals := <-got:
		if len(signals) != 1 {
			t.Fatalf("got %d signals, want 1", len(signals))
		}
		s := signals[0]
		if s["appID"] != appID {
			t.Errorf("appID %v", s["appID"])
		}
		if s["type"] != signalType {
			t.Errorf("type %v, want %s", s["type"], signalType)
		}
		if s["isTestMode"] != true {
			t.Errorf("isTestMode %v, want true under %s", s["isTestMode"], TestModeEnv)
		}
		if user, _ := s["clientUser"].(string); len(user) != 64 {
			t.Errorf("clientUser %q is not a SHA-256 hex digest", user)
		}
		payload, _ := s["payload"].(map[string]any)
		if payload["command"] != "agentlab up" {
			t.Errorf("payload.command %v", payload["command"])
		}
		if payload["appVersion"] != project.Version() {
			t.Errorf("payload.appVersion %v, want %s", payload["appVersion"], project.Version())
		}
		for _, k := range []string{"TelemetryDeck.Device.operatingSystem", "TelemetryDeck.Device.architecture", "TelemetryDeck.SDK.nameAndVersion"} {
			if payload[k] == "" || payload[k] == nil {
				t.Errorf("payload lacks %s", k)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no signal reached the endpoint")
	}
}

func TestCommandSendsNothingWhenOptedOutOrUnconfigured(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- r.URL.Path }))
	defer srv.Close()
	endpoint = srv.URL
	defer func() { endpoint = "" }()

	withAppID(t, testAppID)
	t.Setenv(OptOutEnv, "1")
	t.Setenv(doNotTrackEnv, "")
	Command(lab(t, "up"))

	t.Setenv(OptOutEnv, "")
	t.Setenv(doNotTrackEnv, "1")
	Command(lab(t, "up"))

	t.Setenv(doNotTrackEnv, "")
	Command(lab(t, "post-render"))

	withAppID(t, "")
	Command(lab(t, "up"))

	select {
	case p := <-hit:
		t.Fatalf("a signal was sent (%s) despite the opt-out / plumbing / empty app ID", p)
	case <-time.After(300 * time.Millisecond):
	}
}
