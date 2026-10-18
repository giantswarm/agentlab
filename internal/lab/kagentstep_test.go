package lab

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The releases the tests step to.
const (
	testKagentFrom  = "1.13.0"
	testKagentTo    = "1.14.0"
	testKagentRC    = "1.15.1-rc.2"
	testKagentNext  = "1.15.0"
	testChartBefore = "4.142.0"
	testChartRC     = "4.145.1-rc.6"
	testLabValues   = "lab-values.yaml"
)

// TestKagentStepOverlay: the overlay pins kagent and kagent-crds alone to the
// exact release; the chart moves them together.
func TestKagentStepOverlay(t *testing.T) {
	raw, err := kagentStepOverlay(testKagentRC)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Components map[string]struct {
			VersionRange string `yaml:"versionRange"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("the overlay is no YAML: %v\n%s", err, raw)
	}
	if len(values.Components) != 2 {
		t.Errorf("the overlay names %d components, want kagent and kagent-crds alone:\n%s", len(values.Components), raw)
	}
	for _, component := range []string{componentKagent, kagentCRDsRelease} {
		if got := values.Components[component].VersionRange; got != testKagentRC {
			t.Errorf("components.%s.versionRange = %q, want the exact release", component, got)
		}
	}
}

// TestKagentStepConfig: --to adds the overlay after the lab's values files,
// --chart moves the chart version, both do both; neither, a range, a chart
// step on a chart directory are refused; the lab's configuration stays as
// it is.
func TestKagentStepConfig(t *testing.T) {
	overlay := filepath.Join(StateDir, kagentStepValues)
	for _, tc := range []struct {
		name               string
		opts               KagentStepOptions
		chartPath          string
		wantValues, wantCV string
		wantErr            string
	}{
		{name: "kagent pin", opts: KagentStepOptions{To: "v" + testKagentTo}, wantValues: testLabValues + "," + overlay, wantCV: testChartBefore},
		{name: "chart candidate", opts: KagentStepOptions{Chart: testChartRC}, wantValues: testLabValues, wantCV: testChartRC},
		{name: "pin on a candidate", opts: KagentStepOptions{To: testKagentRC, Chart: testChartRC}, wantValues: testLabValues + "," + overlay, wantCV: testChartRC},
		{name: "no step", wantErr: "name the step"},
		{name: "a range", opts: KagentStepOptions{To: ">=1.14.0"}, wantErr: "an exact kagent release"},
		{name: "a chart range", opts: KagentStepOptions{Chart: "4.145"}, wantErr: "an exact agent-platform release"},
		{name: "chart on a directory", opts: KagentStepOptions{Chart: testChartRC}, chartPath: "../agent-platform/helm/agent-platform", wantErr: "drop platform.chartPath"},
		{name: "unknown dev image", opts: KagentStepOptions{To: testKagentTo, DevImages: map[string]string{"kagent-ctl": "x:dev"}}, wantErr: "unknown target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.ChartVersion = testChartBefore
			cfg.Platform.ValuesFiles = []string{testLabValues}
			cfg.Platform.ChartPath = tc.chartPath
			opts := tc.opts
			stepped, err := kagentStepConfig(cfg, &opts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(stepped.Platform.ValuesFiles, ","); got != tc.wantValues {
				t.Errorf("stepped values files = %s, want %s", got, tc.wantValues)
			}
			if stepped.Platform.ChartVersion != tc.wantCV {
				t.Errorf("stepped chart = %s, want %s", stepped.Platform.ChartVersion, tc.wantCV)
			}
			if strings.HasPrefix(opts.To, "v") {
				t.Errorf("--to not normalized: %s", opts.To)
			}
			if len(cfg.Platform.ValuesFiles) != 1 || cfg.Platform.ChartVersion != testChartBefore {
				t.Errorf("the lab's configuration changed: %v, %s", cfg.Platform.ValuesFiles, cfg.Platform.ChartVersion)
			}
		})
	}
}

// TestKagentStepIsStep: --to must be newer than the kagent the lab runs,
// --chart another chart than the one in place.
func TestKagentStepIsStep(t *testing.T) {
	before := kagentRunning{chart: testChartBefore, kagent: testKagentFrom + "+a9dbe3d860c5"}
	for _, tc := range []struct {
		opts KagentStepOptions
		ok   bool
	}{
		{KagentStepOptions{To: testKagentTo}, true},
		{KagentStepOptions{To: testKagentRC}, true},
		{KagentStepOptions{To: testKagentFrom}, false},
		{KagentStepOptions{To: "1.12.0"}, false},
		{KagentStepOptions{Chart: testChartRC}, true},
		{KagentStepOptions{Chart: testChartBefore}, false},
	} {
		r := &kagentStepRun{before: before, opts: tc.opts}
		if err := r.isStep(); (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v, want ok=%v", tc.opts, err, tc.ok)
		}
	}
}

// TestKagentStepped: after the upgrade the kagent chart is --to's, the meta
// chart --chart's; a chart candidate without --to must move kagent forward.
func TestKagentStepped(t *testing.T) {
	before := kagentRunning{chart: testChartBefore, kagent: testKagentFrom}
	for _, tc := range []struct {
		name    string
		after   kagentRunning
		opts    KagentStepOptions
		wantErr string
	}{
		{"pinned", kagentRunning{chart: testChartBefore, kagent: testKagentTo}, KagentStepOptions{To: testKagentTo}, ""},
		{"pinned, with build metadata", kagentRunning{chart: testChartBefore, kagent: testKagentTo + "+3177c6907334"}, KagentStepOptions{To: testKagentTo}, ""},
		{"pin not applied", kagentRunning{chart: testChartBefore, kagent: testKagentFrom}, KagentStepOptions{To: testKagentTo}, "not " + testKagentTo},
		{"candidate steps kagent", kagentRunning{chart: testChartRC, kagent: testKagentNext}, KagentStepOptions{Chart: testChartRC}, ""},
		{"candidate keeps kagent", kagentRunning{chart: testChartRC, kagent: testKagentFrom}, KagentStepOptions{Chart: testChartRC}, ""},
		{"candidate keeps kagent, with build metadata", kagentRunning{chart: testChartRC, kagent: testKagentFrom + "+3177c6907334"}, KagentStepOptions{Chart: testChartRC}, ""},
		{"candidate steps kagent back", kagentRunning{chart: testChartRC, kagent: "1.12.0"}, KagentStepOptions{Chart: testChartRC}, "steps kagent back"},
		{"candidate not installed", kagentRunning{chart: testChartBefore, kagent: testKagentNext}, KagentStepOptions{Chart: testChartRC}, "not " + testChartRC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := kagentStepped(before, tc.after, tc.opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}

// TestKagentStepChartOnly: a --chart step that keeps kagent is chart-only; a
// kagent move or a --to pin is not.
func TestKagentStepChartOnly(t *testing.T) {
	before := kagentRunning{chart: testChartBefore, kagent: testKagentFrom}
	for _, tc := range []struct {
		after kagentRunning
		opts  KagentStepOptions
		want  bool
	}{
		{kagentRunning{chart: testChartRC, kagent: testKagentFrom + "+3177c6907334"}, KagentStepOptions{Chart: testChartRC}, true},
		{kagentRunning{chart: testChartRC, kagent: testKagentNext}, KagentStepOptions{Chart: testChartRC}, false},
		{kagentRunning{chart: testChartRC, kagent: testKagentTo}, KagentStepOptions{Chart: testChartRC, To: testKagentTo}, false},
	} {
		r := &kagentStepRun{before: before, after: tc.after, opts: tc.opts}
		if got := r.chartOnly(); got != tc.want {
			t.Errorf("%s with %+v: chartOnly = %v, want %v", tc.after, tc.opts, got, tc.want)
		}
	}
}

// TestKagentStepTemplate: the proof's agent is an AgentTemplate and an Agent
// of the platform Harness, both labelled agentlab's.
func TestKagentStepTemplate(t *testing.T) {
	manifest := kagentStepTemplate(defaultModelConfig)
	for _, want := range []string{"kind: AgentTemplate", "kind: Agent\n", "name: " + kagentStepAgent, "name: " + defaultModelConfig, "name: " + kagentHarness, managedByLabel} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, manifest)
		}
	}
}
