package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/agentlab/internal/config"
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
	status := &cobra.Command{Use: "status", Run: func(*cobra.Command, []string) {}}
	postRender := &cobra.Command{Use: "post-render", Hidden: true, Run: func(*cobra.Command, []string) {}}
	root.AddCommand(up, status, postRender)
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
	Flush(context.Background())

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
	if _, ok := payload["deploymentID"]; ok {
		t.Errorf("payload.deploymentID %v from a command that entered no lab", payload["deploymentID"])
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
	EnterLab("3f0c9a4e-5d1b-4b8e-9c2a-7e6f1d0b8a55", true)
	Flush(context.Background())

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
	sender, pending, held, awaitLab, deploymentID, labOff = nil, nil, false, false, "", false
	t.Cleanup(func() {
		endpoint = ""
		sender, pending, held, awaitLab, deploymentID, labOff = nil, nil, false, false, "", false
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
	Flush(context.Background())

	signals := received(t, got)
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1", len(signals))
	}
	if user, _ := signals[0]["clientUser"].(string); len(user) != 64 {
		t.Errorf("clientUser %q is not a SHA-256 hex digest", user)
	}
}

// unreachable makes every new connection hang the way a black-holed route
// does: the dial never completes until the request gives up. The library's
// client uses http.DefaultTransport, so swapping it is the network going
// away for the test.
func unreachable(t *testing.T) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// stderr captures what the process writes to stderr while fn runs.
func stderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	prevLog := log.Writer()
	log.SetOutput(w)
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	os.Stderr = prev
	log.SetOutput(prevLog)
	_ = w.Close()
	return <-out
}

// TestAnUnreachableEndpointCostsNoWaitAndNoOutput: with the ingest endpoint
// out of reach, a command that has outlived connectTimeout finishes at once,
// and nothing about the failed delivery reaches the command's output — not
// even in test mode, which the lab machines run under.
func TestAnUnreachableEndpointCostsNoWaitAndNoOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a signal reached an endpoint that is out of reach")
	}))
	defer srv.Close()
	enable(t, srv)
	unreachable(t)

	var waited time.Duration
	out := stderr(t, func() {
		Command(lab(t, "up"))
		EnterLab("", true)         // the signal leaves as the command enters its lab
		time.Sleep(connectTimeout) // the command's own work
		start := time.Now()
		Flush(context.Background())
		waited = time.Since(start)
	})
	if waited > 50*time.Millisecond {
		t.Errorf("Flush waited %s for an endpoint that is out of reach", waited)
	}
	if out != "" {
		t.Errorf("telemetry wrote to stderr:\n%s", out)
	}
}

// TestAShortCommandWaitsOnlyForTheConnection: a command that finishes before
// a connection could have been made waits for one no longer than
// connectTimeout from when its signal set out, not the whole flush budget.
func TestAShortCommandWaitsOnlyForTheConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	enable(t, srv)
	unreachable(t)

	start := time.Now()
	Command(lab(t, "up"))
	Flush(context.Background())
	if waited := time.Since(start); waited < connectTimeout || waited >= flushTimeout {
		t.Errorf("Flush waited %s on an endpoint out of reach, want about %s", waited, connectTimeout)
	}
}

// TestDebugModeReportsTheFailure: AGENTLAB_TELEMETRY_DEBUG is where a failed
// delivery is told.
func TestDebugModeReportsTheFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	enable(t, srv)
	unreachable(t)
	t.Setenv(DebugEnv, "1")

	out := stderr(t, func() {
		Command(lab(t, "up"))
		Flush(context.Background())
	})
	if !strings.Contains(out, "telemetry: ") {
		t.Errorf("debug mode said nothing about the failed delivery; stderr:\n%s", out)
	}
}

// TestEnterLabSendsTheCommandSignalWithTheDeployment: a command's signal
// waits for its lab, then leaves at once, naming the lab's deployment — and so
// does every signal after it. Flush sends nothing twice.
func TestEnterLabSendsTheCommandSignalWithTheDeployment(t *testing.T) {
	got := capture(t)
	const id = "3f0c9a4e-5d1b-4b8e-9c2a-7e6f1d0b8a55"

	Command(lab(t, "up"))
	select {
	case signals := <-got:
		t.Fatalf("the command signal left before the command entered its lab: %v", signals)
	case <-time.After(100 * time.Millisecond):
	}

	EnterLab(id, true)
	signals := received(t, got)
	if len(signals) != 1 || signals[0]["type"] != signalType {
		t.Fatalf("got %v, want the one command signal", signals)
	}
	payload, _ := signals[0]["payload"].(map[string]any)
	if payload["deploymentID"] != id {
		t.Errorf("payload.deploymentID %v, want %s", payload["deploymentID"], id)
	}

	Platform(context.Background(), config.Default(), config.DefaultChartVersion)
	Flush(context.Background())
	signals = received(t, got)
	if len(signals) != 1 || signals[0]["type"] != platformSignalType {
		t.Fatalf("got %v after Flush, want the platform signal alone", signals)
	}
	payload, _ = signals[0]["payload"].(map[string]any)
	if payload["deploymentID"] != id {
		t.Errorf("platform payload.deploymentID %v, want %s", payload["deploymentID"], id)
	}
}

func TestNewDeploymentDrawsDistinctIDsAndSalts(t *testing.T) {
	a, err := NewDeployment()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDeployment()
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.Salt == b.Salt {
		t.Errorf("two deployments share an ID or a salt: %+v %+v", a, b)
	}
	if a.ID == a.Salt || len(a.Salt) != 64 {
		t.Errorf("salt %q is not 32 random bytes apart from the ID %q", a.Salt, a.ID)
	}
}

// TestUpWaitsForTheDeploymentItStarts: the command that starts a deployment
// (StartsDeployment) sends nothing on entering its lab, which may still name
// the deployment of a cluster that is gone, and leaves with the one it starts.
func TestUpWaitsForTheDeploymentItStarts(t *testing.T) {
	got := capture(t)
	const old, started = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	up := lab(t, "up")
	up.Annotations = map[string]string{StartsDeployment: ""}

	Command(up)
	EnterLab(old, true)
	select {
	case signals := <-got:
		t.Fatalf("up's signal left on entering its lab: %v", signals)
	case <-time.After(100 * time.Millisecond):
	}

	StartDeployment(started)
	signals := received(t, got)
	payload, _ := signals[0]["payload"].(map[string]any)
	if payload["deploymentID"] != started {
		t.Errorf("up's payload.deploymentID %v, want the started %s", payload["deploymentID"], started)
	}
}

// TestALabWithoutADeploymentSendsOnEntering: a command entering a lab that
// records no deployment (one booted before deployments existed) sends at
// once, without one, rather than at Flush — which an interrupted command
// never reaches.
func TestALabWithoutADeploymentSendsOnEntering(t *testing.T) {
	got := capture(t)

	Command(lab(t, "up"))
	EnterLab("", true)
	signals := received(t, got)
	payload, _ := signals[0]["payload"].(map[string]any)
	if _, ok := payload["deploymentID"]; ok {
		t.Errorf("payload.deploymentID %v for a lab without one", payload["deploymentID"])
	}
}

// TestALabWithUsageDataOffSendsNothing: entering a lab whose agentlab.yaml
// turns usage data off drops the command signal and every later one, the
// platform signal included, and Enabled says so.
func TestALabWithUsageDataOffSendsNothing(t *testing.T) {
	got := capture(t)

	Command(lab(t, "up"))
	EnterLab("3f0c9a4e-5d1b-4b8e-9c2a-7e6f1d0b8a55", false)
	if Enabled() {
		t.Error("Enabled() true in a lab with usage data off")
	}
	Platform(context.Background(), config.Default(), config.DefaultChartVersion)
	Flush(context.Background())
	select {
	case signals := <-got:
		t.Fatalf("a lab with usage data off sent %v", signals)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestALabCommandWaitsForItsConfiguration: a command that runs against a
// lab (AwaitLab) sends nothing until it has read the lab's configuration —
// a first run's form may turn usage data off — and then follows it.
func TestALabCommandWaitsForItsConfiguration(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("usage data %v", on), func(t *testing.T) {
			got := capture(t)

			Command(lab(t, "status"))
			AwaitLab()
			select {
			case signals := <-got:
				t.Fatalf("the signal left before the lab's configuration was read: %v", signals)
			case <-time.After(100 * time.Millisecond):
			}

			EnterLab("", on)
			Flush(context.Background())
			select {
			case signals := <-got:
				if !on {
					t.Fatalf("sent %v in a lab with usage data off", signals)
				}
				if len(signals) != 1 || signals[0]["type"] != signalType {
					t.Fatalf("got %v, want the one command signal", signals)
				}
			case <-time.After(300 * time.Millisecond):
				if on {
					t.Fatal("sent nothing in a lab with usage data on")
				}
			}
		})
	}
}

// TestALabCommandFailingBeforeItsConfigurationSendsNothing: a command that
// fails between entering its lab and reading its configuration (an aborted
// form, a configure that saves nothing) sends nothing at Flush: the lab's
// setting is unknown.
func TestALabCommandFailingBeforeItsConfigurationSendsNothing(t *testing.T) {
	got := capture(t)

	Command(lab(t, "status"))
	AwaitLab()
	Flush(context.Background())
	select {
	case signals := <-got:
		t.Fatalf("sent %v without the lab's setting", signals)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestEnvOptOutNamesTheVariable(t *testing.T) {
	for _, tc := range []struct{ optOut, doNotTrack, want string }{
		{"", "", ""},
		{"1", "", OptOutEnv},
		{"", "1", doNotTrackEnv},
		{"", "0", ""},
		{"", "false", ""},
	} {
		t.Setenv(OptOutEnv, tc.optOut)
		t.Setenv(doNotTrackEnv, tc.doNotTrack)
		if got := EnvOptOut(); got != tc.want {
			t.Errorf("OPTOUT=%q DO_NOT_TRACK=%q: EnvOptOut() = %q, want %q", tc.optOut, tc.doNotTrack, got, tc.want)
		}
	}
}
