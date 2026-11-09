package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
	chartutil "helm.sh/helm/v4/pkg/chart/common/util"

	"github.com/giantswarm/agentlab/internal/config"
)

// A lab's kagent line is the major.minor of the kagent its chart installs,
// read off components.kagent.versionRange (the chart's default, or an
// overlay's) — `>=1.31.0 <1.32.0` is the 1.31 line. The line decides which
// kagent API the lab serves, and so which agentlab can drive it: this
// release's `turn` and proofs speak the Session API (a v1alpha3 Agent, a
// Session per conversation), which kagent serves from the 1.3 line on; the
// 4.x charts before it serve AgentTemplates and AgentInstances, which
// agentlab drove up to sessionlessAgentlab. The 3.x chart's kagent is
// driven here still. docs/cli.md "One binary per kagent line".
const (
	// sessionAPILine is the first kagent line that serves the Session API.
	sessionAPILine = "1.3"
	// sessionlessAgentlab is the last agentlab release that drives the
	// kagent lines before sessionAPILine.
	sessionlessAgentlab = "v0.84.6"
	// sessionlessChart is the last agent-platform release on those lines.
	sessionlessChart = "4.120.1"
	// kagentLineFile is the line the last install put in place, in state/:
	// what `list` shows without a cluster, and what `turn` and the proofs
	// judge before they start.
	kagentLineFile = "kagent-line.json"
)

// KagentLine is the line a lab's last install put in place.
type KagentLine struct {
	// Line is the kagent major.minor, `1.31`.
	Line string `json:"line"`
	// Range is the components.kagent.versionRange it was read from.
	Range string `json:"range,omitempty"`
	// Chart is the meta chart that installed it.
	Chart string `json:"chart"`
	// FromPin marks a line no install recorded, judged from the chart pin
	// alone: a 4.x release up to sessionlessChart, the lab of an agentlab
	// before the record. Line is then sessionlessLine.
	FromPin bool `json:"fromPin,omitempty"`
}

// sessionlessLine is the Line of a FromPin record: a line before the Session
// API, which the pin alone cannot narrow down.
const sessionlessLine = "<" + sessionAPILine

// rangeVersionRe finds a range's floor (`>=X.Y.Z…`) or an exact pin
// (`X.Y.Z…`, the way an overlay holds a component on one build).
var rangeVersionRe = regexp.MustCompile(`^\s*(?:>=\s*|=\s*)?v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)`)

// lineOfRange is the major.minor of a version range's floor; empty for a
// range with no floor (`*`, `<2.0.0`), which names no line.
func lineOfRange(versionRange string) string {
	m := rangeVersionRe.FindStringSubmatch(versionRange)
	if m == nil {
		return ""
	}
	v, err := semver.NewVersion(m[1])
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d.%d", v.Major(), v.Minor())
}

// drivenHere reports whether this agentlab drives a lab on the line: the
// 3.x chart's (legacy) whatever its kagent, and on 4.x every line from
// sessionAPILine on. A line that is no version is not judged.
func drivenHere(line string, legacy bool) bool {
	if legacy || line == "" {
		return true
	}
	if line == sessionlessLine {
		return false
	}
	v, err := semver.NewVersion(line + ".0")
	if err != nil {
		return true
	}
	return !v.LessThan(semver.MustParse(sessionAPILine + ".0"))
}

// undrivenLineError words a lab on a line this agentlab does not drive.
func undrivenLineError(line KagentLine) error {
	what := fmt.Sprintf("kagent %s (the %s line)", line.Range, line.Line)
	if line.FromPin {
		what = "a kagent line before " + sessionAPILine
	}
	return fmt.Errorf("%s installs %s, whose API is AgentTemplates and AgentInstances; "+
		"this agentlab drives kagent's Session API, served from the %s line on (agent-platform releases after %s).\n\n"+
		"  Drive this lab with agentlab %s, kept beside this one (docs/cli.md \"One binary per kagent line\"),\n"+
		"  or pin a chart on the %s line or later: `agentlab configure --chart-version %s` and run this again",
		line.Chart, what, sessionAPILine, sessionlessChart, sessionlessAgentlab, sessionAPILine, config.DefaultChartVersion)
}

// chartKagentLine is the kagent line the chart installs with the lab's
// values: the chart's defaults coalesced with them, as its templates see
// them. Empty when the lab runs no agents, or the chart cannot be loaded
// (the render that follows reports that in Helm's words).
func chartKagentLine(cfg *config.Config, chart platformChart, values map[string]any) (line, versionRange string) {
	if !cfg.Platform.Agents {
		return "", ""
	}
	c, err := loadPlatformChart(chart)
	if err != nil || c == nil {
		return "", ""
	}
	merged, err := chartutil.CoalesceValues(c, values)
	if err != nil {
		return "", ""
	}
	components, _ := merged[valuesComponents].(map[string]any)
	kagent, _ := components[componentKagent].(map[string]any)
	versionRange = stringValue(kagent, valuesVersionRange)
	return lineOfRange(versionRange), versionRange
}

// refuseUndrivenLine stops an install of a chart whose kagent line this
// agentlab does not drive, before anything is applied, and returns the line
// it found. An upgrade proof's seed (platform.upgradeSeed) installs an older
// line on purpose, to be upgraded by this agentlab: it passes with a note.
func refuseUndrivenLine(cfg *config.Config, chart platformChart, values map[string]any) (KagentLine, error) {
	line, versionRange := chartKagentLine(cfg, chart, values)
	record := KagentLine{Line: line, Range: versionRange, Chart: chart.String()}
	if drivenHere(line, cfg.LegacyChart()) {
		return record, nil
	}
	if cfg.Platform.UpgradeSeed {
		note("the upgrade seed installs the kagent %s line, which this agentlab installs and upgrades but whose turns it does not drive (agentlab %s does)", line, sessionlessAgentlab)
		return record, nil
	}
	return record, undrivenLineError(record)
}

// recordKagentLine writes the line an install is putting in place into the
// lab's state/; a lab without agents removes an earlier record.
func recordKagentLine(dir string, line KagentLine) error {
	path := filepath.Join(dir, StateDir, kagentLineFile)
	if line.Line == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(line, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// readKagentLine is the line recorded in a lab's directory; nil when no
// install recorded one (a lab installed by an agentlab before the record,
// or not yet installed).
func readKagentLine(dir string) *KagentLine {
	b, err := os.ReadFile(filepath.Join(dir, StateDir, kagentLineFile)) // #nosec G304 -- the lab's own state
	if err != nil {
		return nil
	}
	var line KagentLine
	if json.Unmarshal(b, &line) != nil || line.Line == "" {
		return nil
	}
	return &line
}

// labKagentLine is the lab's line: the record, else what the chart pin
// says alone (pinKagentLine). nil when neither tells, or without agents.
func labKagentLine(dir string, cfg *config.Config) *KagentLine {
	if !cfg.Platform.Agents {
		return nil
	}
	if line := readKagentLine(dir); line != nil {
		return line
	}
	return pinKagentLine(cfg)
}

// pinKagentLine is what the chart pin says alone: a released 4.x chart up
// to sessionlessChart is on a line before the Session API (the lab of an
// agentlab that wrote no record, or one about to be configured onto such a
// chart). nil for any other pin, a chart directory or branch, the 3.x line
// and a lab without agents — the install's judge reads those charts.
func pinKagentLine(cfg *config.Config) *KagentLine {
	if !cfg.Platform.Agents || cfg.LegacyChart() || cfg.ChartChannel() != config.ChartChannelStable {
		return nil
	}
	v, err := semver.NewVersion(cfg.Platform.ChartVersion)
	if err != nil || v.GreaterThan(semver.MustParse(sessionlessChart)) {
		return nil
	}
	return &KagentLine{Line: sessionlessLine, Chart: "agent-platform " + cfg.Platform.ChartVersion, FromPin: true}
}

// RefuseUndrivenPin stops a command that would write a lab whose chart pin
// alone says a line this agentlab does not drive — `configure` saving it,
// `up` and `platform` before their first step: the agentlab.yaml it writes
// is one the line's agentlab no longer reads, and the files it renders are
// not that agentlab's. An upgrade seed passes.
func RefuseUndrivenPin(cfg *config.Config) error {
	line := pinKagentLine(cfg)
	if line == nil || cfg.Platform.UpgradeSeed {
		return nil
	}
	return undrivenLineError(*line)
}

// RefuseUndrivenLab stops `turn` and the proofs on a lab whose recorded
// kagent line this agentlab does not drive — they would fail on an API it
// does not speak, and a verdict must never come from the wrong binary. The
// lab is the current directory (enterLab). An upgrade seed is left alone:
// its proofs are what the upgrade is judged by.
func RefuseUndrivenLab(cfg *config.Config) error {
	line := labKagentLine(".", cfg)
	if line == nil || drivenHere(line.Line, cfg.LegacyChart()) || cfg.Platform.UpgradeSeed {
		return nil
	}
	return undrivenLineError(*line)
}

// lineNote is `list`'s kagent row: the line with the agentlab that drives
// it when that is not this one.
func lineNote(line *KagentLine, driven bool) string {
	var b strings.Builder
	switch {
	case line == nil:
		return "not recorded yet (the next `agentlab platform` records it)"
	case line.FromPin:
		fmt.Fprintf(&b, "a line before %s (the pin %s; no install recorded it)", sessionAPILine, line.Chart)
	default:
		fmt.Fprintf(&b, "%s line (%s)", line.Line, line.Range)
	}
	if !driven {
		fmt.Fprintf(&b, " — driven by agentlab %s, not this binary", sessionlessAgentlab)
	}
	return b.String()
}
