// Package telemetry reports one anonymous usage signal per agentlab command
// to TelemetryDeck, the way kubectl-gs does: which command ran, on which
// agentlab version, operating system and architecture, under a hashed
// machine identifier that lets Giant Swarm count users without knowing who
// they are. Nothing about the lab — its configuration, users, clusters,
// arguments or flags — leaves the machine. See README "Usage data".
//
// Setting AGENTLAB_TELEMETRY_OPTOUT (any value) or the console convention
// DO_NOT_TRACK=1 disables it. Reporting never blocks or fails a command.
package telemetry

import (
	"log"
	"os"
	"strings"

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
)

// endpoint overrides the TelemetryDeck ingest URL; tests point it at a local
// server. Empty means the library's default.
var endpoint string

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
// Fire-and-forget: the HTTP request runs on its own goroutine while the
// command does its work, and a failure to deliver is swallowed — telemetry
// must never get in the user's way. Same shape as kubectl-gs: signal type
// GiantSwarm.command with the command path and the app version, plus the
// OS, architecture and SDK version the library adds.
//
// Not every invocation is a person using the lab: hidden commands are
// plumbing (__complete runs on every TAB press), `completion` runs on every
// shell start once sourced from a profile, and `help` is help. None of those
// count.
func Command(cmd *cobra.Command) {
	if !Enabled() || !UserFacing(cmd) {
		return
	}
	testMode := os.Getenv(TestModeEnv) != ""
	client, err := newClient(testMode)
	if err != nil {
		if testMode {
			log.Printf("telemetry: creating the TelemetryDeck client: %s", err)
		}
		return
	}
	err = client.SendSignal(cmd.Context(), signalType, map[string]interface{}{
		"appVersion": project.Version(),
		"command":    cmd.CommandPath(),
	})
	if err != nil && testMode {
		log.Printf("telemetry: sending the usage signal: %s", err)
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
	return telemetrydeck.NewClient(appID, opts...)
}
