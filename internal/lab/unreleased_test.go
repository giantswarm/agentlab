package lab

import (
	"strings"
	"testing"
)

// waitsAnnotation is a dev build's annotation for agent-manager, the shape
// tests/verify-components-charts.py writes.
const waitsAnnotation = `{"agent-manager":{"versionRange":">=1.10.0 <2.0.0","waitsFor":"1.10.0"}}`

// waitingComponent is the component waitsAnnotation names.
const waitingComponent = "agent-manager"

// TestUnreleasedWaits: the verdict of the chart's hook, read by the lab — a
// development build waits for every recorded component still enabled at its
// recorded range with no semverFilter; a release, a missing annotation and
// values that point the component elsewhere wait for nothing.
func TestUnreleasedWaits(t *testing.T) {
	const devVersion = "4.115.1-r7fb489f8t20261006135307h75833ed"
	annotations := map[string]string{unreleasedAnnotation: waitsAnnotation}
	recorded := func(extra map[string]any) map[string]any {
		c := map[string]any{valuesVersionRange: ">=1.10.0 <2.0.0"}
		for k, v := range extra {
			c[k] = v
		}
		return map[string]any{valuesComponents: map[string]any{waitingComponent: c}}
	}
	for _, tc := range []struct {
		name        string
		directory   bool
		version     string
		annotations map[string]string
		values      map[string]any
		wait        bool
	}{
		{name: "dev build at the recorded range", version: devVersion, annotations: annotations, values: recorded(nil), wait: true},
		{name: "explicitly enabled", version: devVersion, annotations: annotations, values: recorded(map[string]any{valuesEnabled: true}), wait: true},
		{name: "chart directory with a release version", directory: true, version: "4.117.0-rc.1", annotations: annotations, values: recorded(nil), wait: true},
		{name: "release", version: "4.117.0", annotations: annotations, values: recorded(nil)},
		{name: "release candidate", version: "4.117.0-rc.1", annotations: annotations, values: recorded(nil)},
		{name: "no annotation", version: devVersion, values: recorded(nil)},
		{name: "disabled", version: devVersion, annotations: annotations, values: recorded(map[string]any{valuesEnabled: false})},
		{name: "another range", version: devVersion, annotations: annotations, values: map[string]any{valuesComponents: map[string]any{waitingComponent: map[string]any{valuesVersionRange: ">=1.9.0 <2.0.0"}}}},
		{name: "dev channel", version: devVersion, annotations: annotations, values: recorded(map[string]any{valuesSemverFilter: "-r.*"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits, err := unreleasedWaits(tc.directory, tc.version, tc.annotations, tc.values)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wait {
				if len(waits) > 0 {
					t.Errorf("waits = %v, want none", waits)
				}
				return
			}
			want := `agent-manager 1.10.0 (components.agent-manager.versionRange ">=1.10.0 <2.0.0" admits no published chart)`
			if len(waits) != 1 || waits[0] != want {
				t.Errorf("waits = %v, want [%s]", waits, want)
			}
		})
	}
}

// TestUnreleasedWaitsMalformed: an annotation that is not the check's JSON is
// an error naming it, never a silent pass.
func TestUnreleasedWaitsMalformed(t *testing.T) {
	_, err := unreleasedWaits(true, "1.0.0", map[string]string{unreleasedAnnotation: waitingComponent}, nil)
	if err == nil || !strings.Contains(err.Error(), unreleasedAnnotation) {
		t.Errorf("err = %v, want one naming %s", err, unreleasedAnnotation)
	}
}

// TestComponentEnabled mirrors agent-platform.componentEnabled: on without
// an entry or a switch; the kagent line's CRDs and Substrate, entered without
// a switch, follow kagent.
func TestComponentEnabled(t *testing.T) {
	components := map[string]any{
		"kagent":         map[string]any{valuesEnabled: false},
		waitingComponent: map[string]any{valuesVersionRange: agentChartRange},
		musterValues:     map[string]any{valuesEnabled: false},
		substrateRelease: map[string]any{valuesVersionRange: "0.x"},
		"kagent-crds":    map[string]any{},
	}
	for name, want := range map[string]bool{
		waitingComponent: true,
		"absent":         true,
		musterValues:     false,
		substrateRelease: false,
		"kagent-crds":    false,
		"substrate-crds": true, // no entry: on, like any component without one
	} {
		if got := componentEnabled(components, name); got != want {
			t.Errorf("componentEnabled(%s) = %v, want %v", name, got, want)
		}
	}
}

// TestRefuseUnreleasedComponents: a chart directory carrying the annotation
// is refused with the component and the version it waits for, its defaults
// merged with the lab's values the way the chart sees them; values that
// point the component at a published range install as before.
func TestRefuseUnreleasedComponents(t *testing.T) {
	dir := isolateHelm(t)
	chartDir := writeChartFiles(t, dir, "agent-platform", map[string]string{
		chartYAML:   "apiVersion: v2\nname: agent-platform\nversion: 1.1.35\nannotations:\n  " + unreleasedAnnotation + ": '" + waitsAnnotation + "'\n",
		chartValues: "components:\n  agent-manager:\n    versionRange: \">=1.10.0 <2.0.0\"\n",
	})
	chart := platformChart{ref: chartDir}

	err := refuseUnreleasedComponents(chart, map[string]any{valuesComponents: map[string]any{musterValues: map[string]any{valuesEnabled: true}}})
	if err == nil {
		t.Fatal("want a refusal for agent-manager 1.10.0")
	}
	for _, want := range []string{"development build that waits for a component release", "agent-manager 1.10.0", chartDir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%v", want, err)
		}
	}

	published := map[string]any{valuesComponents: map[string]any{waitingComponent: map[string]any{valuesVersionRange: ">=1.9.0 <2.0.0"}}}
	if err := refuseUnreleasedComponents(chart, published); err != nil {
		t.Errorf("a published range must install: %v", err)
	}
}
