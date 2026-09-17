package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// withUserIdentity pins the two inputs to userID so a test can assert on the
// digest without depending on the machine it runs on.
func withUserIdentity(t *testing.T, machine string, user string) {
	t.Helper()
	prevMachine, prevUser := machineID, osUser
	machineID = func() (string, bool) { return machine, machine != "" }
	osUser = func() string { return user }
	t.Cleanup(func() { machineID, osUser = prevMachine, prevUser })
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
		if got := UserFacing(lab(t, tc.path...)); got != tc.want {
			t.Errorf("UserFacing(%v) = %v, want %v", tc.path, got, tc.want)
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
	got := capture(t)
	// Pinned, because a CI container has no /etc/machine-id and would take
	// the fallback path — which TestCommandFallsBackToTheLibraryIdentifier
	// covers on purpose, and this test must not drift into.
	withUserIdentity(t, "12a0d395-dbb9-3050-b357-f0f9f3185660", "tester")

	Command(lab(t, "up"))

	signals := received(t, got)
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
	// The library hashes once more over what WithUserID was given, so
	// this is the assertion that agentlab's own identifier was wired in
	// at all: without it a missing WithUserID looks identical, the
	// library's derived digest being 64 hex characters too.
	//
	// No salt in the sum below, because agentlab does not call
	// WithHashSalt and the library's own defaults to "". Adding one
	// there fails here, and this is the line to change.
	want, ok := userID()
	if !ok {
		t.Fatal("the pinned identity did not reach userID")
	}
	sum := sha256.Sum256([]byte(want))
	if user, _ := s["clientUser"].(string); user != hex.EncodeToString(sum[:]) {
		t.Errorf("clientUser %q, want sha256 of agentlab's identifier %s", user, hex.EncodeToString(sum[:]))
	}
	payload, _ := s["payload"].(map[string]any)
	if payload["command"] != "agentlab up" {
		t.Errorf("payload.command %v", payload["command"])
	}
	if payload["appVersion"] != project.Version() {
		t.Errorf("payload.appVersion %v, want %s", payload["appVersion"], project.Version())
	}
	// The dashboard's standard "App Versions" insight reads the reserved
	// parameter, not the payload key the usage report queries.
	if payload["TelemetryDeck.AppInfo.version"] != project.Version() {
		t.Errorf("payload[TelemetryDeck.AppInfo.version] %v, want %s", payload["TelemetryDeck.AppInfo.version"], project.Version())
	}
	if sha := project.ShortSHA(); sha != "" && payload["TelemetryDeck.AppInfo.buildNumber"] != sha {
		t.Errorf("payload[TelemetryDeck.AppInfo.buildNumber] %v, want %s", payload["TelemetryDeck.AppInfo.buildNumber"], sha)
	}
	for _, k := range []string{"TelemetryDeck.Device.operatingSystem", "TelemetryDeck.Device.architecture", "TelemetryDeck.SDK.nameAndVersion"} {
		if payload[k] == "" || payload[k] == nil {
			t.Errorf("payload lacks %s", k)
		}
	}
}

func TestCommandSendsNothingWhenOptedOutOrUnconfigured(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- r.URL.Path }))
	defer srv.Close()
	enable(t, srv)

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

// enable turns reporting on for one test, pointed at srv, with no client
// left over from an earlier test (the first signal creates one for the
// process, and every later one rides it).
func enable(t *testing.T, srv *httptest.Server) {
	t.Helper()
	withAppID(t, testAppID)
	t.Setenv(OptOutEnv, "")
	t.Setenv(doNotTrackEnv, "")
	t.Setenv(TestModeEnv, "1")
	endpoint = srv.URL
	sender = nil
	t.Cleanup(func() {
		endpoint = ""
		sender = nil
	})
}

// capture enables reporting pointed at an ingest endpoint of the test's own
// and hands back what it receives, one batch of signals per request, decoded
// the way TelemetryDeck would see them.
func capture(t *testing.T) <-chan []map[string]any {
	t.Helper()
	got := make(chan []map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var signals []map[string]any
		if err := json.Unmarshal(body, &signals); err != nil {
			t.Errorf("body is not a signal array: %v\n%s", err, body)
		}
		got <- signals
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	enable(t, srv)
	return got
}

// received is the next request's signals, or a failed test when none comes.
func received(t *testing.T, got <-chan []map[string]any) []map[string]any {
	t.Helper()
	select {
	case signals := <-got:
		return signals
	case <-time.After(5 * time.Second):
		t.Fatal("no signal reached the endpoint")
		return nil
	}
}

// TestFlushSeesTheSignalOut: a command that finishes before the endpoint has
// answered waits for the answer, so nothing is lost.
func TestFlushSeesTheSignalOut(t *testing.T) {
	got := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // a slow network
		got <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	enable(t, srv)

	Flush(context.Background()) // nothing sent yet: returns at once

	Command(lab(t, "up"))
	start := time.Now()
	Flush(context.Background())
	waited := time.Since(start)

	select {
	case <-got:
	default:
		t.Fatalf("Flush returned after %s without the signal having reached the endpoint", waited)
	}
	if waited >= flushTimeout {
		t.Errorf("Flush waited %s, the whole budget, for a signal that was answered after 100ms", waited)
	}
}

// TestFlushIsBounded: an endpoint that never answers costs the command's exit
// no more than the budget, and no error.
func TestFlushIsBounded(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		release()
		srv.Close()
	}()
	enable(t, srv)

	Command(lab(t, "up"))
	start := time.Now()
	Flush(context.Background())
	if waited := time.Since(start); waited < flushTimeout || waited > flushTimeout+time.Second {
		t.Errorf("Flush waited %s on a stalled endpoint, want about %s", waited, flushTimeout)
	}
}

func TestUserIDIsStableSaltedAndDropsWhenUnknown(t *testing.T) {
	const (
		machine = "12a0d395-dbb9-3050-b357-f0f9f3185660"
		user    = "tester"
	)

	t.Run("it is a stable hex digest", func(t *testing.T) {
		withUserIdentity(t, machine, user)
		first, ok := userID()
		if !ok {
			t.Fatal("userID() gave up on a machine that has an identifier")
		}
		if len(first) != 64 || first != strings.ToLower(first) {
			t.Errorf("userID() = %q, want 64 lower-case hex characters", first)
		}
		if second, _ := userID(); second != first {
			t.Errorf("userID() is not stable: %q then %q", first, second)
		}
	})

	// The digest is pinned so that changing the salt or the layout of the
	// hashed string cannot pass unnoticed: either one re-identifies every
	// agentlab installation in the TelemetryDeck dashboard.
	t.Run("the digest is pinned to the salt and the layout", func(t *testing.T) {
		withUserIdentity(t, machine, user)
		const want = "1cd584bd30211c4eefeb327acfbdeb6d890d7039398a7b2410b4b51c96e447d1"
		if got, _ := userID(); got != want {
			t.Errorf("userID() = %q, want %q — if this is a deliberate change, every user is reset by it", got, want)
		}
	})

	t.Run("the salt is applied", func(t *testing.T) {
		withUserIdentity(t, machine, user)
		unsalted := sha256.Sum256([]byte(machine + "|" + user))
		if got, _ := userID(); got == hex.EncodeToString(unsalted[:]) {
			t.Error("userID() hashes the inputs without the salt")
		}
	})

	t.Run("it distinguishes machines and users", func(t *testing.T) {
		withUserIdentity(t, machine, user)
		base, _ := userID()

		withUserIdentity(t, "9f1c2b4e-0000-4000-8000-0123456789ab", user)
		if other, _ := userID(); other == base {
			t.Error("two machines share one identifier")
		}

		withUserIdentity(t, machine, "someone-else")
		if other, _ := userID(); other == base {
			t.Error("two users on one machine share one identifier")
		}
	})

	t.Run("a machine without an identifier gets none", func(t *testing.T) {
		withUserIdentity(t, "", user)
		if got, ok := userID(); got != "" || ok {
			t.Errorf("userID() = (%q, %v), want (\"\", false)", got, ok)
		}
	})
}

// TestCommandFallsBackToTheLibraryIdentifier is the promise that telemetry
// never breaks: a machine that exposes no identifier still reports, under the
// one the library derives for itself.
func TestCommandFallsBackToTheLibraryIdentifier(t *testing.T) {
	got := capture(t)
	withUserIdentity(t, "", "tester")

	Command(lab(t, "up"))

	signals := received(t, got)
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1", len(signals))
	}
	if user, _ := signals[0]["clientUser"].(string); len(user) != 64 {
		t.Errorf("clientUser %q is not a SHA-256 hex digest", user)
	}
}
