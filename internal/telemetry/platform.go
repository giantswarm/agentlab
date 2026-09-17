package telemetry

import (
	"context"
	"strconv"

	"github.com/giantswarm/agentlab/internal/config"
)

// platformSignalType names the signal a lab sends when it installs or
// upgrades the meta chart. It is agentlab's own — kubectl-gs has nothing like
// it — so it sits in agentlab's namespace under the GiantSwarm. prefix, apart
// from the GiantSwarm.command signal the shared usage report reads.
const platformSignalType = "GiantSwarm.agentlab.platform"

// Platform records one install or upgrade of the agent platform: `agentlab
// up` and `agentlab platform` send it once per run, after the config is loaded
// and the chart resolved (the dev channel's build picked), so chartVersion is
// what the run installs. Nothing else sends it — `render`, the proofs and
// every other command say nothing about the platform.
//
// The payload is the meta chart line and the lab's shape: the exact chart
// version and its major, the channel the chart comes from (stable, dev or a
// chart directory), whether the lab renders the legacy 3.x shape, and the
// feature switches of agentlab.yaml as booleans. Never a path (a chart
// directory reports the version its Chart.yaml carries, not where it is), a
// hostname, a user or a token: every value is a version or a bool, so the
// dashboard can count labs per chart line and channel while nothing in a
// signal points back at a machine. Same opt-outs, test mode and client as
// Command, so Flush covers this signal too.
func Platform(ctx context.Context, cfg *config.Config, chartVersion string) {
	if !Enabled() {
		return
	}
	c, err := client()
	if err != nil {
		logf("creating the TelemetryDeck client: %s", err)
		return
	}
	if err := c.SendSignal(ctx, platformSignalType, platformPayload(cfg, chartVersion)); err != nil {
		logf("sending the platform signal: %s", err)
	}
}

// platformPayload is the platform signal's payload. Every value is a string,
// the way TelemetryDeck stores payload parameters and the dashboard groups by
// them: a bool is "true" or "false", the major "4". The switches are the
// effective ones — what the install turns on, not what the file says while
// the component it depends on is off.
func platformPayload(cfg *config.Config, chartVersion string) map[string]interface{} {
	return map[string]interface{}{
		"chartVersion":  chartVersion,
		"chartMajor":    strconv.FormatUint(config.MajorOf(chartVersion), 10),
		"chartChannel":  cfg.ChartChannel(),
		"chartPinned":   strconv.FormatBool(cfg.Platform.ChartPinned),
		"legacyShape":   strconv.FormatBool(cfg.LegacyChart()),
		"agents":        strconv.FormatBool(cfg.Platform.Agents),
		"observability": strconv.FormatBool(cfg.Platform.Observability),
		"fakeFleet":     strconv.FormatBool(cfg.Platform.FakeFleet),
		"modelManager":  strconv.FormatBool(cfg.ModelManagerEnabled()),
		"vmManager":     strconv.FormatBool(cfg.VMManagerEnabled()),
		"klausGateway":  strconv.FormatBool(cfg.KlausGatewayEnabled()),
		"backstage":     strconv.FormatBool(cfg.Backstage.Enabled),
	}
}
