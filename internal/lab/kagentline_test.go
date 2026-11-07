package lab

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/labs"
)

// The lines and charts the tests judge.
const (
	testOldLine      = "1.2"
	testOldRange     = ">=1.2.0 <1.3.0"
	testOldChart     = "agent-platform " + sessionlessChart
	testCurLine      = "1.31"
	testCurRange     = ">=1.31.0 <1.32.0"
	testCurChart     = "agent-platform 4.175.1"
	testFirstSession = "4.121.0"
)

// TestLineOfRange: the line is the floor's major.minor, an exact pin's too;
// a range without a floor names none.
func TestLineOfRange(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{testCurRange, testCurLine},
		{testOldRange, testOldLine},
		{">= 1.3.4-rc.1", sessionAPILine},
		{"1.3.4-rc.1", sessionAPILine},
		{"v1.26.0", "1.26"},
		{">=0.11.0-gs.12 <0.11.1-0", "0.11"},
		{"*", ""},
		{"<2.0.0", ""},
		{"", ""},
	} {
		if got := lineOfRange(tc.in); got != tc.want {
			t.Errorf("lineOfRange(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDrivenHere: on 4.x the Session API lines are this agentlab's, the
// lines before them (kagent 0.11 included) sessionlessAgentlab's; the 3.x
// chart is driven here whatever its kagent.
func TestDrivenHere(t *testing.T) {
	for _, tc := range []struct {
		line   string
		legacy bool
		want   bool
	}{
		{"1.0", false, false}, {testOldLine, false, false}, {"0.11", false, false}, {sessionlessLine, false, false},
		{sessionAPILine, false, true}, {testCurLine, false, true}, {"2.0", false, true},
		{"0.10", true, true}, {sessionlessLine, true, true},
		{"", false, true}, {"garbage", false, true},
	} {
		if got := drivenHere(tc.line, tc.legacy); got != tc.want {
			t.Errorf("drivenHere(%q, legacy %v) = %v, want %v", tc.line, tc.legacy, got, tc.want)
		}
	}
}

// TestLabKagentLineFromPin: without a record a released 4.x pin up to
// sessionlessChart says a line before the Session API; a later pin, a 3.x
// pin, a chart directory and a lab without agents say nothing.
func TestLabKagentLineFromPin(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Platform.ChartVersion = sessionlessChart
	line := labKagentLine(dir, cfg)
	if line == nil || !line.FromPin || line.Line != sessionlessLine || drivenHere(line.Line, false) {
		t.Fatalf("labKagentLine(%s) = %+v, want a line before the Session API from the pin", sessionlessChart, line)
	}
	if err := undrivenLineError(*line); !strings.Contains(err.Error(), "a kagent line before "+sessionAPILine) {
		t.Errorf("refusal from the pin = %v", err)
	}
	for what, mutate := range map[string]func(*config.Config){
		testFirstSession: func(c *config.Config) { c.Platform.ChartVersion = testFirstSession },
		"3.x":            func(c *config.Config) { c.Platform.ChartVersion = legacyChartVersion },
		"a chartPath":    func(c *config.Config) { c.Platform.ChartPath = dir },
		"no agents":      func(c *config.Config) { c.Platform.Agents = false },
	} {
		c := config.Default()
		c.Platform.ChartVersion = sessionlessChart
		mutate(c)
		if got := labKagentLine(dir, c); got != nil {
			t.Errorf("%s: labKagentLine = %+v, want none", what, got)
		}
	}
	// A record wins over the pin.
	if err := recordKagentLine(dir, KagentLine{Line: testOldLine, Range: testOldRange, Chart: testOldChart}); err != nil {
		t.Fatal(err)
	}
	if got := labKagentLine(dir, cfg); got == nil || got.FromPin || got.Line != testOldLine {
		t.Errorf("with a record: %+v", got)
	}
}

// TestKagentLineRecord: written, read back, removed for a lab without
// agents; a missing or empty record reads as none.
func TestKagentLineRecord(t *testing.T) {
	dir := t.TempDir()
	if readKagentLine(dir) != nil {
		t.Fatal("a fresh lab reads a line")
	}
	want := KagentLine{Line: testCurLine, Range: testCurRange, Chart: testCurChart}
	if err := recordKagentLine(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := readKagentLine(dir); got == nil || *got != want {
		t.Fatalf("readKagentLine = %+v, want %+v", got, want)
	}
	if err := recordKagentLine(dir, KagentLine{}); err != nil {
		t.Fatal(err)
	}
	if readKagentLine(dir) != nil {
		t.Error("the record of a lab without agents stays")
	}
	if err := recordKagentLine(dir, KagentLine{}); err != nil {
		t.Errorf("removing an absent record: %v", err)
	}
}

// TestRefuseUndrivenLab: turn and the proofs stop on a recorded 1.2 line,
// naming the release that drives it; a seed and a Session API line pass.
func TestRefuseUndrivenLab(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := config.Default()
	if err := RefuseUndrivenLab(cfg); err != nil {
		t.Errorf("a lab with no record is refused: %v", err)
	}
	if err := recordKagentLine(".", KagentLine{Line: testOldLine, Range: testOldRange, Chart: testOldChart}); err != nil {
		t.Fatal(err)
	}
	err := RefuseUndrivenLab(cfg)
	if err == nil {
		t.Fatal("a 1.2 lab is not refused")
	}
	for _, want := range []string{testOldChart, "the 1.2 line", "agentlab " + sessionlessAgentlab, "--chart-version " + config.DefaultChartVersion} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}
	cfg.Platform.UpgradeSeed = true
	if err := RefuseUndrivenLab(cfg); err != nil {
		t.Errorf("an upgrade seed is refused: %v", err)
	}
	cfg.Platform.UpgradeSeed = false
	if err := recordKagentLine(".", KagentLine{Line: testCurLine, Range: testCurRange, Chart: testCurChart}); err != nil {
		t.Fatal(err)
	}
	if err := RefuseUndrivenLab(cfg); err != nil {
		t.Errorf("a 1.31 lab is refused: %v", err)
	}
}

// TestListKagentLine: two labs side by side, one on the 1.2 line and one on
// the 1.31 line, and one whose line is not recorded yet; the 1.2 lab names
// the agentlab that drives it, in text and JSON.
func TestListKagentLine(t *testing.T) {
	old := writeLab(t, "clusterName: line12\n")
	cur := writeLab(t, "clusterName: line131\n")
	fresh := writeLab(t, "clusterName: unrecorded\n")
	stubListProbes(t, map[string]string{"line12": stateRunning, "line131": stateRunning, "unrecorded": stateNotCreated}, map[string]string{})
	for dir, line := range map[string]KagentLine{
		old: {Line: testOldLine, Range: testOldRange, Chart: testOldChart},
		cur: {Line: testCurLine, Range: testCurRange, Chart: testCurChart},
	} {
		if err := recordKagentLine(dir, line); err != nil {
			t.Fatal(err)
		}
	}
	listed := ListLabs([]labs.Lab{{Name: "line12", Dir: old}, {Name: "line131", Dir: cur}, {Name: "unrecorded", Dir: fresh}}, cur)
	byName := map[string]ListedLab{}
	for _, l := range listed {
		byName[l.Name] = l
	}
	if l := byName["line12"]; l.KagentLine == nil || l.KagentLine.Line != testOldLine || l.Driven {
		t.Errorf("old = %+v, want the 1.2 line, not driven here", l)
	}
	if l := byName["line131"]; l.KagentLine == nil || l.KagentLine.Line != testCurLine || !l.Driven {
		t.Errorf("cur = %+v, want the 1.31 line, driven here", l)
	}
	if l := byName["unrecorded"]; l.KagentLine != nil || !l.Driven {
		t.Errorf("new = %+v, want no line", l)
	}

	var out bytes.Buffer
	if err := PrintLabs(&out, listed, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kagent      " + testOldLine + " line (" + testOldRange + ") — driven by agentlab " + sessionlessAgentlab + ", not this binary",
		"kagent      " + testCurLine + " line (" + testCurRange + ")\n",
		"kagent      not recorded yet",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("PrintLabs lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := PrintLabs(&out, listed, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"line": "`+testOldLine+`"`) || !strings.Contains(out.String(), `"driven": false`) {
		t.Errorf("JSON lacks the line:\n%s", out.String())
	}
}

// TestListKagentLineWithoutAgents: no kagent row for a lab that runs none.
func TestListKagentLineWithoutAgents(t *testing.T) {
	dir := writeLab(t, "clusterName: noagents\nplatform:\n  enabled: true\n  agents: false\n")
	stubListProbes(t, map[string]string{"noagents": stateRunning}, map[string]string{})
	var out bytes.Buffer
	if err := PrintLabs(&out, ListLabs([]labs.Lab{{Name: "noagents", Dir: dir}}, ""), false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "kagent ") {
		t.Errorf("a lab without agents lists a kagent line:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, StateDir)); err == nil {
		t.Error("list wrote into the lab")
	}
}

// TestRefuseUndrivenLineChart: the install's judge on a chart directory
// whose default kagent range is the 1.2 line, and an overlay moving it on.
func TestRefuseUndrivenLineChart(t *testing.T) {
	dir := t.TempDir()
	chartDir := filepath.Join(dir, "agent-platform")
	if err := os.MkdirAll(chartDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		chartYAML:   "apiVersion: v2\nname: agent-platform\nversion: 4.120.1\n",
		chartValues: "components:\n  kagent:\n    versionRange: \">=1.2.0 <1.3.0\"\n",
	} {
		if err := os.WriteFile(filepath.Join(chartDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	chart := platformChart{ref: chartDir}
	line, err := refuseUndrivenLine(cfg, chart, map[string]any{})
	if err == nil || line.Line != testOldLine {
		t.Fatalf("refuseUndrivenLine = %+v, %v; want the 1.2 line refused", line, err)
	}
	overlay := map[string]any{valuesComponents: map[string]any{componentKagent: map[string]any{valuesVersionRange: testCurRange}}}
	if line, err := refuseUndrivenLine(cfg, chart, overlay); err != nil || line.Line != testCurLine {
		t.Errorf("with the overlay = %+v, %v; want the 1.31 line", line, err)
	}
	cfg.Platform.UpgradeSeed = true
	if _, err := refuseUndrivenLine(cfg, chart, map[string]any{}); err != nil {
		t.Errorf("the seed is refused: %v", err)
	}
	cfg.Platform.UpgradeSeed = false
	cfg.Platform.Agents = false
	if line, err := refuseUndrivenLine(cfg, chart, map[string]any{}); err != nil || line.Line != "" {
		t.Errorf("without agents = %+v, %v; want no line", line, err)
	}
}

// TestRefuseUndrivenPin: configure, up and platform leave a lab pinned to a
// line before the Session API to the agentlab that drives it; a seed, a
// later pin and the 3.x line pass.
func TestRefuseUndrivenPin(t *testing.T) {
	cfg := config.Default()
	if err := RefuseUndrivenPin(cfg); err != nil {
		t.Errorf("the default pin is refused: %v", err)
	}
	cfg.Platform.ChartVersion = "4.119.1"
	err := RefuseUndrivenPin(cfg)
	if err == nil || !strings.Contains(err.Error(), "agentlab "+sessionlessAgentlab) || !strings.Contains(err.Error(), "agent-platform 4.119.1") {
		t.Errorf("4.119.1 = %v, want refused naming %s", err, sessionlessAgentlab)
	}
	cfg.Platform.UpgradeSeed = true
	if err := RefuseUndrivenPin(cfg); err != nil {
		t.Errorf("an upgrade seed is refused: %v", err)
	}
	cfg.Platform.UpgradeSeed = false
	for _, pin := range []string{testFirstSession, legacyChartVersion} {
		cfg.Platform.ChartVersion = pin
		if err := RefuseUndrivenPin(cfg); err != nil {
			t.Errorf("%s is refused: %v", pin, err)
		}
	}
}
