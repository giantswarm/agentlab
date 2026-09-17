// Package telemetry reports anonymous usage signals to TelemetryDeck, the way
// kubectl-gs does. One per agentlab command (Command): which command ran, on
// which agentlab version, operating system and architecture, under a hashed
// machine identifier that lets Giant Swarm count users without knowing who
// they are. And one per platform install (Platform): the meta chart line the
// lab installs and its feature switches, as versions and booleans. Nothing
// else about the lab — its users, clusters, paths, arguments or flags —
// leaves the machine. See docs/telemetry.md.
//
// Setting AGENTLAB_TELEMETRY_OPTOUT (any value) or the console convention
// DO_NOT_TRACK=1 disables both. Reporting never fails a command: the signals
// travel while the command works, and a command that finishes first waits
// for them at most half a second (Flush) before the process exits.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"os/user"
	"strings"
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
	// numbers in the TelemetryDeck dashboard) and logs delivery errors to
	// stderr — for developing the lab, and for proving the integration.
	TestModeEnv = "AGENTLAB_TELEMETRY_TESTMODE"

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
// nil until the first signal.
var sender *telemetrydeck.Client

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
// The HTTP request runs on its own goroutine while the command does its work
// (Flush, at the end, gives a fast command a bounded moment to see it out),
// and a failure to deliver is swallowed — telemetry must never get in the
// user's way. Same shape as kubectl-gs: signal type
// GiantSwarm.command with the command path and the app version in the
// payload, plus the OS, architecture and SDK version the library adds. The
// version and commit also go out as TelemetryDeck.AppInfo.version and
// .buildNumber (the library's WithAppVersion/WithBuildNumber): those are the
// parameters the TelemetryDeck dashboard's standard "App Versions" insight
// reads — the payload's appVersion is what the shared usage report queries.
//
// Not every invocation is a person using the lab: hidden commands are
// plumbing (__complete runs on every TAB press), `completion` runs on every
// shell start once sourced from a profile, and `help` is help. None of those
// count.
func Command(cmd *cobra.Command) {
	if !Enabled() || !UserFacing(cmd) {
		return
	}
	c, err := client()
	if err != nil {
		logf("creating the TelemetryDeck client: %s", err)
		return
	}
	err = c.SendSignal(cmd.Context(), signalType, map[string]interface{}{
		"appVersion": project.Version(),
		"command":    cmd.CommandPath(),
	})
	if err != nil {
		logf("sending the usage signal: %s", err)
	}
}

// Flush lets the signals sent so far (Command, Platform) finish their trip
// before the process exits, waiting at most flushTimeout (and no longer than
// ctx allows): a sub-second command such as `agentlab version` would
// otherwise exit before its request has left the machine. Never an error for
// the caller — a signal that did not make it is dropped, and said so only in
// test mode.
func Flush(ctx context.Context) {
	if sender == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	if err := sender.Flush(ctx); err != nil {
		logf("a usage signal was not delivered within %s: %s", flushTimeout, err)
	}
}

// client is the process's TelemetryDeck client: created by the first signal,
// shared by every later one, so one Flush waits for all of them.
func client() (*telemetrydeck.Client, error) {
	if sender == nil {
		c, err := newClient(testMode())
		if err != nil {
			return nil, err
		}
		sender = c
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

// testMode says whether AGENTLAB_TELEMETRY_TESTMODE is set.
func testMode() bool {
	return os.Getenv(TestModeEnv) != ""
}

// logf reports a telemetry problem on stderr in test mode; production runs
// stay silent, telemetry being nobody's concern but the lab's developers.
func logf(format string, args ...any) {
	if testMode() {
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
		opts = append(opts,
			telemetrydeck.WithTestMode(),
			telemetrydeck.WithLogger(log.New(os.Stderr, "telemetry: ", 0)),
		)
	}
	opts = append(opts,
		telemetrydeck.WithAppVersion(project.Version()),
		telemetrydeck.WithBuildNumber(project.ShortSHA()),
	)
	if id, ok := userID(); ok {
		opts = append(opts, telemetrydeck.WithUserID(id))
	} else {
		logf("this computer exposes no stable identifier; the library's own stands in")
	}
	return telemetrydeck.NewClient(appID, opts...)
}
