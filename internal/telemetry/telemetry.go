// Package telemetry reports one anonymous usage signal per agentlab command
// to TelemetryDeck, the way kubectl-gs does: which command ran, on which
// agentlab version, operating system and architecture, under a hashed
// machine identifier that lets Giant Swarm count users without knowing who
// they are. Nothing about the lab — its configuration, users, clusters,
// arguments or flags — leaves the machine. See docs/telemetry.md.
//
// Setting AGENTLAB_TELEMETRY_OPTOUT (any value) or the console convention
// DO_NOT_TRACK=1 disables it. Reporting never fails a command: the signal
// travels while the command works, and a command that finishes first waits
// for it at most half a second (Flush) before the process exits.
package telemetry

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/telemetrydeck-go"
	"github.com/spf13/cobra"

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

// endpoint overrides the TelemetryDeck ingest URL; tests point it at a local
// server. Empty means the library's default.
var endpoint string

// sender is the client Command sent through, kept for Flush; nil until then.
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
	client, err := newClient(testMode())
	if err != nil {
		logf("creating the TelemetryDeck client: %s", err)
		return
	}
	sender = client
	err = client.SendSignal(cmd.Context(), signalType, map[string]interface{}{
		"appVersion": project.Version(),
		"command":    cmd.CommandPath(),
	})
	if err != nil {
		logf("sending the usage signal: %s", err)
	}
}

// Flush lets the signal Command sent finish its trip before the process
// exits, waiting at most flushTimeout (and no longer than ctx allows): a
// sub-second command such as `agentlab version` would otherwise exit before
// its request has left the machine. Never an error for the caller — a signal
// that did not make it is dropped, and said so only in test mode.
func Flush(ctx context.Context) {
	if sender == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	if err := sender.Flush(ctx); err != nil {
		logf("the usage signal was not delivered within %s: %s", flushTimeout, err)
	}
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
	return telemetrydeck.NewClient(appID, opts...)
}
