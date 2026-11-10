package lab

import (
	"errors"
	"testing"
)

// stubPlatformChartValues points the chart loader at v (or err) for the test
// and clears the memo, restored afterwards.
func stubPlatformChartValues(t *testing.T, v platformChartValues, err error) {
	t.Helper()
	prev := platformChartValuesProbe
	resetPlatformChartValues := func() {
		platformChartValuesMu.Lock()
		platformChartValuesCache = map[string]platformChartValues{}
		platformChartValuesMu.Unlock()
	}
	resetPlatformChartValues()
	platformChartValuesProbe = func(platformChart) (platformChartValues, error) { return v, err }
	t.Cleanup(func() {
		platformChartValuesProbe = prev
		resetPlatformChartValues()
	})
}

// TestPlatformChartValuesOfLoadsOnce: a chart is loaded once per run, a
// failed load is asked again, and two charts are two loads.
func TestPlatformChartValuesOfLoadsOnce(t *testing.T) {
	stubPlatformChartValues(t, platformChartValues{}, nil)
	loads := map[string]int{}
	answers := []error{errors.New("registry unreachable")}
	platformChartValuesProbe = func(chart platformChart) (platformChartValues, error) {
		loads[chart.String()]++
		if len(answers) > 0 {
			err := answers[0]
			answers = answers[1:]
			return platformChartValues{}, err
		}
		return platformChartValues{values: map[string]any{componentSkillManager: map[string]any{}}}, nil
	}
	chart := platformChart{ref: "oci://example.test/agent-platform", version: "4.178.0"}
	if _, err := platformChartValuesOf(chart); err == nil {
		t.Fatal("the failed load: want its error")
	}
	for range 2 {
		v, err := platformChartValuesOf(chart)
		if err != nil || !v.carries(componentSkillManager) {
			t.Fatalf("platformChartValuesOf = %+v, %v; want the loaded values", v, err)
		}
	}
	other := platformChart{ref: t.TempDir()}
	if _, err := platformChartValuesOf(other); err != nil {
		t.Fatal(err)
	}
	if loads[chart.String()] != 2 || loads[other.String()] != 1 {
		t.Errorf("loads = %v, want 2 for %s (one failed, one kept) and 1 for %s", loads, chart, other)
	}
}
