// Package telemetry reports anonymous usage signals to TelemetryDeck, the way
// kubectl-gs does. One per agentlab command (Command): which command ran, on
// which agentlab version, operating system and architecture, under a hashed
// machine identifier that lets Giant Swarm count users without knowing who
// they are. And one per platform install (Platform): the meta chart line the
// lab installs and its feature switches, as versions and booleans. Once a
// command has entered a lab (EnterLab), every signal names the lab's
// deployment, a random ID the lab's Backstage sends with its own signals.
// Nothing else about the lab — its users, clusters, paths, arguments or flags
// — leaves the machine. See docs/telemetry.md.
//
// Setting AGENTLAB_TELEMETRY_OPTOUT (any value) or the console convention
// DO_NOT_TRACK=1 disables both. Reporting never fails a command and never
// speaks on its output: the signals travel while the command works, a command
// that finishes first waits for them at most half a second (Flush) before the
// process exits, and not at all once the endpoint has proven out of reach.
// AGENTLAB_TELEMETRY_DEBUG tells on stderr what happened to them.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http/httptrace"
	"os"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/telemetrydeck-go"
	"github.com/spf13/cobra"

	"github.com/giantswarm/agentlab/internal/telemetry/machineid"
	"github.com/giantswarm/agentlab/pkg/project"
)

// appID names the agentlab app in Giant Swarm's TelemetryDeck organization.
// It is not a secret: it only says where signals are filed (kubectl-gs
// carries its own in cmd/root.go the same way). Empty disables reporting.
// Created 2026-09-07 by Team Honeybadger, which administers the organization.
var appID = "89699F74-9A72-46BF-BF5A-7949901FBB36"

const (
	// OptOutEnv disables usage data collection when set to any value.
	OptOutEnv = "AGENTLAB_TELEMETRY_OPTOUT"

	// TestModeEnv files the signals as test data (kept apart from production
	// numbers in the TelemetryDeck dashboard), for developing the lab.
	TestModeEnv = "AGENTLAB_TELEMETRY_TESTMODE"

	// DebugEnv logs what happens to the signals on stderr: a delivery that
	// failed, a rejected request with its answer, an endpoint out of reach —
	// for proving the integration. Without it telemetry prints nothing.
	DebugEnv = "AGENTLAB_TELEMETRY_DEBUG"

	// doNotTrackEnv is the cross-tool console convention
	// (https://consoledonottrack.com): "1" (anything but "0"/"false") opts out.
	doNotTrackEnv = "DO_NOT_TRACK"

	// signalType is the same as kubectl-gs's, so one usage report reads both.
	signalType = "GiantSwarm.command"

	// flushTimeout caps how long a finished command waits for its signal to
	// leave the machine. One round trip to the ingest endpoint takes a few
	// hundred milliseconds at most, and a slow exit is felt from about half a
	// second on; a command that ran longer than this finds nothing to wait for.
	flushTimeout = 500 * time.Millisecond

	// connectTimeout is how long after the first signal set out Flush waits
	// for its request to hold a connection to the endpoint (name lookup, TCP
	// and TLS) before it counts the endpoint as out of reach and stops
	// waiting: a black-holed route would otherwise cost every short command
	// the whole flushTimeout. A cold connection to the ingest endpoint takes
	// about 120ms from Europe.
	connectTimeout = 300 * time.Millisecond
)

// userSalt prefixes the string agentlab hashes into its user identifier. It
// is not a secret and cannot be one — agentlab is a public repo. Its one job
// is domain separation: the same computer hashes to a different identifier
// for agentlab than for kubectl-gs or any other TelemetryDeck reporter.
//
// It is not a defence against guessing the inputs, and the hash should not be
// treated as one. The machine identifiers are derived from hardware or
// written at install time, not drawn at random, and the user name alongside
// them carries little entropy; someone holding both for a given computer can
// confirm a row in the dashboard. That is no worse than what the MAC-derived
// default gave, but it is a confirmation the salt does not prevent.
//
// The trailing version marks the layout of the hashed string: changing either
// resets every user in the dashboard.
const userSalt = "agentlab-telemetry-v1"

// machineID and osUser are the inputs to userID, as vars so tests can pin
// them (appID and endpoint are swapped the same way).
var (
	machineID = machineid.ID
	osUser    = userName
)

// endpoint overrides the TelemetryDeck ingest URL; tests point it at a local
// server. Empty means the library's default.
var endpoint string

// sender is the client every signal goes through (client), kept for Flush;
// nil until the first signal. route watches whether its requests reach the
// endpoint, created with it.
var (
	sender *telemetrydeck.Client
	route  *reach
)

// StartsDeployment is the annotation of the command that starts a lab's
// deployment (`up`): its signal waits for StartDeployment, past EnterLab.
const StartsDeployment = "agentlab.giantswarm.io/telemetry-starts-deployment"

// pending is the command signal Command recorded and EnterLab,
// StartDeployment or Flush sends; awaitStart holds it past EnterLab for a
// command that starts a deployment. deploymentID is the deployment named
// last, on every signal after it.
var (
	pending      func()
	awaitStart   bool
	deploymentID string
)

// reach records whether any signal's request got a connection to the ingest
// endpoint: since is when the first signal set out, connected is closed by
// the first connection.
type reach struct {
	since     time.Time
	connected chan struct{}
	once      sync.Once
}

func newReach() *reach {
	return &reach{since: time.Now(), connected: make(chan struct{})}
}

// trace has ctx's requests tell r when they hold a connection, a fresh or a
// reused one. A nil ctx (a command that never ran through Execute) is
// context.Background, as the library takes it.
func (r *reach) trace(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { r.once.Do(func() { close(r.connected) }) },
	})
}

func (r *reach) reached() bool {
	select {
	case <-r.connected:
		return true
	default:
		return false
	}
}

// Enabled reports whether this process sends usage signals.
func Enabled() bool {
	if appID == "" || os.Getenv(OptOutEnv) != "" {
		return false
	}
	if v := strings.TrimSpace(os.Getenv(doNotTrackEnv)); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return false
	}
	return true
}

// Command records one execution of a user-facing command ("agentlab up").
// The signal leaves once the command has entered its lab (EnterLab), so that
// it names the lab's deployment — for `up`, once it has started one
// (StartDeployment) — or at Flush for a command that runs against no lab; its
// HTTP request runs on its own goroutine, and a failure to
// deliver is swallowed — telemetry must never get in the user's way. Same
// shape as kubectl-gs: signal type GiantSwarm.command with the command path
// and the app version in the payload, plus the OS, architecture and SDK
// version the library adds. The version and commit also go out as
// TelemetryDeck.AppInfo.version and .buildNumber (the library's
// WithAppVersion/WithBuildNumber): those are the parameters the TelemetryDeck
// dashboard's standard "App Versions" insight reads — the payload's
// appVersion is what the shared usage report queries.
//
// Not every invocation is a person using the lab: hidden commands are
// plumbing (__complete runs on every TAB press), `completion` runs on every
// shell start once sourced from a profile, and `help` is help. None of those
// count.
func Command(cmd *cobra.Command) {
	if !Enabled() || !UserFacing(cmd) {
		return
	}
	ctx := cmd.Context()
	_, awaitStart = cmd.Annotations[StartsDeployment]
	pending = func() {
		send(ctx, signalType, map[string]interface{}{
			"appVersion": project.Version(),
			"command":    cmd.CommandPath(),
		})
	}
}

// EnterLab names the deployment of the lab the command runs against: every
// signal from here on carries it as deploymentID, and the command's own
// signal (Command) leaves now — unless the command starts a deployment
// (StartsDeployment). An empty ID, a lab without a recorded deployment, sends
// the command signal without one.
func EnterLab(id string) {
	deploymentID = id
	if !awaitStart {
		sendPending()
	}
}

// StartDeployment is EnterLab for the command that starts the lab's
// deployment: its signal leaves now, naming id.
func StartDeployment(id string) {
	deploymentID, awaitStart = id, false
	sendPending()
}

// sendPending sends the command signal Command recorded, once.
func sendPending() {
	if p := pending; p != nil {
		pending = nil
		p()
	}
}

// send hands one signal to the process's client, traced for Flush. A failure
// is only logged (logf): telemetry never gets in the user's way.
func send(ctx context.Context, signalType string, payload map[string]interface{}) {
	if deploymentID != "" {
		payload["deploymentID"] = deploymentID
	}
	c, err := client()
	if err != nil {
		Logf("creating the TelemetryDeck client: %s", err)
		return
	}
	if err := c.SendSignal(route.trace(ctx), signalType, payload); err != nil {
		Logf("sending %s: %s", signalType, err)
	}
}

// Flush sends the command signal if it has not left yet — a command that
// runs against no lab, or one that failed before entering its lab — and lets
// the signals sent so far (Command, Platform) finish their trip before the
// process exits, waiting at most flushTimeout (and no longer than
// ctx allows): a sub-second command such as `agentlab version` would
// otherwise exit before its request has left the machine. An endpoint none of
// them has connected to within connectTimeout of the first one setting out is
// out of reach, and Flush stops waiting for it then — at once for a command
// whose signal set out longer ago than that (one that entered its lab early);
// a signal Flush itself sends costs at most connectTimeout. Never an error for the caller — a signal that
// did not make it is dropped, and said so only under DebugEnv.
func Flush(ctx context.Context) {
	sendPending()
	if sender == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	s, flushed := sender, make(chan error, 1)
	go func() { flushed <- s.Flush(ctx) }()

	unreachable := time.NewTimer(time.Until(route.since.Add(connectTimeout)))
	defer unreachable.Stop()
	select {
	case err := <-flushed:
		if err != nil {
			Logf("a usage signal was not delivered within %s: %s", flushTimeout, err)
		}
		return
	case <-unreachable.C:
	}
	if !route.reached() {
		Logf("the ingest endpoint was not reached within %s; not waiting for the usage signal", connectTimeout)
		return
	}
	if err := <-flushed; err != nil {
		Logf("a usage signal was not delivered within %s: %s", flushTimeout, err)
	}
}

// client is the process's TelemetryDeck client: created by the first signal,
// shared by every later one, so one Flush waits for all of them.
func client() (*telemetrydeck.Client, error) {
	if sender == nil {
		c, err := newClient(TestMode())
		if err != nil {
			return nil, err
		}
		sender, route = c, newReach()
	}
	return sender, nil
}

// userID identifies one person on one computer: the identifier the OS keeps
// for the machine, the OS user name and the salt. False when the machine
// exposes none, leaving the library to derive its own.
//
// The layout is fixed, with empty fields rather than omitted ones, so that a
// machine whose user name is briefly unreadable keeps the identifier it had.
//
// What TelemetryDeck stores is one hash further out than what this returns:
// the library hashes the value given to WithUserID again. Harmless, but a
// digest reproduced from this function alone will not match the dashboard's.
func userID() (string, bool) {
	machine, ok := machineID()
	if !ok {
		return "", false
	}
	sum := sha256.Sum256([]byte(userSalt + "|" + machine + "|" + osUser()))
	return hex.EncodeToString(sum[:]), true
}

// userName is the OS user, so that two people sharing a computer count as
// two. user.Current reads the password database in pure Go (and answers
// DOMAIN\user on Windows); the environment is the fallback.
func userName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, env := range []string{"USER", "USERNAME"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

// AppID is the TelemetryDeck app agentlab files its signals under; the lab's
// Backstage reports into the same app.
func AppID() string {
	return appID
}

// TestMode says whether AGENTLAB_TELEMETRY_TESTMODE is set.
func TestMode() bool {
	return os.Getenv(TestModeEnv) != ""
}

// debug says whether AGENTLAB_TELEMETRY_DEBUG is set.
func debug() bool {
	return os.Getenv(DebugEnv) != ""
}

// Logf reports a telemetry problem on stderr under DebugEnv; every other run,
// test mode included, stays silent, telemetry being nobody's concern but the
// lab's developers and never part of a command's output.
func Logf(format string, args ...any) {
	if debug() {
		log.Printf("telemetry: "+format, args...)
	}
}

// UserFacing says whether cmd is something a person runs on purpose: not
// hidden, and not cobra's built-in completion or help trees.
func UserFacing(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Hidden {
			return false
		}
		if c.HasParent() && (c.Name() == "completion" || c.Name() == "help") {
			return false
		}
	}
	return true
}

func newClient(testMode bool) (*telemetrydeck.Client, error) {
	var opts []func(*telemetrydeck.Client)
	if endpoint != "" {
		opts = append(opts, telemetrydeck.WithEndpoint(endpoint))
	}
	if testMode {
		opts = append(opts, telemetrydeck.WithTestMode())
	}
	if debug() {
		opts = append(opts, telemetrydeck.WithLogger(log.New(os.Stderr, "telemetry: ", 0)))
	}
	opts = append(opts,
		telemetrydeck.WithAppVersion(project.Version()),
		telemetrydeck.WithBuildNumber(project.ShortSHA()),
	)
	if id, ok := userID(); ok {
		opts = append(opts, telemetrydeck.WithUserID(id))
	} else {
		Logf("this computer exposes no stable identifier; the library's own stands in")
	}
	return telemetrydeck.NewClient(appID, opts...)
}
