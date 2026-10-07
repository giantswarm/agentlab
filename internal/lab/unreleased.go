package lab

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	"helm.sh/helm/v4/pkg/action"
	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
)

// unreleasedAnnotation is the meta chart's Chart.yaml annotation a dev build
// names the component releases it waits for with: JSON, component ->
// {versionRange, waitsFor}, waitsFor the range's floor. The chart's branch
// check (tests/verify-components-charts.py) writes it whenever a component's
// versionRange admits no published chart yet; a chart without it waits for
// nothing.
const unreleasedAnnotation = "agent-platform.giantswarm.io/unreleased"

// The values the verdict reads: components.<name> and its keys.
const (
	valuesComponents   = "components"
	valuesVersionRange = "versionRange"
	valuesSemverFilter = "semverFilter"
)

// unreleasedComponent is one entry of unreleasedAnnotation.
type unreleasedComponent struct {
	VersionRange string `json:"versionRange"`
	WaitsFor     string `json:"waitsFor"`
}

// releasePrerelease is the pre-release a published meta chart may carry
// (X.Y.Z-rc.N); any other marks a development build.
var releasePrerelease = regexp.MustCompile(`^rc\.[0-9]+$`)

// refuseUnreleasedComponents stops a boot whose meta chart is a development
// build that waits for a component release, before anything is applied: Flux
// would find no tag in that component's range minutes into the install, after
// the components ahead of it are up, and nothing names what it waits for. The
// chart's own pre-install hook says the same, but only once Helm runs it —
// after the lab's namespace, Secrets and side-loads.
//
// values are the lab's values as the install passes them; the chart's
// defaults are merged in here, the way the chart's templates see them. A
// chart that cannot be loaded is left to the render that follows, which
// reports it in Helm's words.
func refuseUnreleasedComponents(chart platformChart, values map[string]any) error {
	c, err := loadPlatformChart(chart)
	if err != nil || c == nil || c.Metadata == nil {
		return nil
	}
	merged, err := chartutil.CoalesceValues(c, values)
	if err != nil {
		return nil
	}
	waits, err := unreleasedWaits(chart.version == "", c.Metadata.Version, c.Metadata.Annotations, merged)
	if err != nil {
		return fmt.Errorf("%s: %w", chart, err)
	}
	if len(waits) == 0 {
		return nil
	}
	return fmt.Errorf("%s is a development build that waits for a component release, so nothing it installs could come up:\n\n%s\n\n"+
		"Install a build whose ranges are published, or point the component at a published range or a dev channel "+
		"(components.<name>.versionRange / semverFilter in platform.valuesFiles)",
		chart, indent(strings.Join(waits, "\n"), "  "))
}

// loadPlatformChart loads the meta chart the install would: the directory,
// or the registry tag (pulled into Helm's content cache once per digest);
// nil for a chart that is not apiVersion v2.
func loadPlatformChart(chart platformChart) (*chartv2.Chart, error) {
	h, err := newHelmOp(platformNamespace)
	if err != nil {
		return nil, err
	}
	install := action.NewInstall(h.cfg)
	var c *chartv2.Chart
	err = retryUnreachable(fmt.Sprintf("loading %s", chart), func() error {
		ch, err := h.loadChart(&install.ChartPathOptions, chart.ref, chart.version)
		c, _ = ch.(*chartv2.Chart)
		return err
	})
	return c, err
}

// unreleasedWaits is the verdict of the chart's hook
// (templates/hooks/unreleased-components.yaml), one line per component that
// waits: a component counts while it is enabled and its versionRange is still
// the recorded one with no semverFilter — values that point it elsewhere have
// chosen what to install. Only a development build waits: a release passed
// the tag pipeline's floor check, so an annotation it carries is stale. A
// chart directory (platform.chartPath) is a development build whatever its
// Chart.yaml version says: a checkout passed no release gate.
func unreleasedWaits(directory bool, version string, annotations map[string]string, values map[string]any) ([]string, error) {
	raw := annotations[unreleasedAnnotation]
	if raw == "" || (!directory && !isDevBuild(version)) {
		return nil, nil
	}
	var unreleased map[string]unreleasedComponent
	if err := json.Unmarshal([]byte(raw), &unreleased); err != nil {
		return nil, fmt.Errorf("the Chart.yaml annotation %s is not the JSON the chart's check writes: %w", unreleasedAnnotation, err)
	}
	components, _ := values[valuesComponents].(map[string]any)
	var waits []string
	for _, name := range slices.Sorted(maps.Keys(unreleased)) {
		u := unreleased[name]
		c, _ := components[name].(map[string]any)
		if !componentEnabled(components, name) || stringValue(c, valuesVersionRange) != u.VersionRange || stringValue(c, valuesSemverFilter) != "" {
			continue
		}
		waits = append(waits, fmt.Sprintf("%s %s (components.%s.versionRange %q admits no published chart)", name, u.WaitsFor, name, u.VersionRange))
	}
	return waits, nil
}

// isDevBuild reports whether a meta chart version is a development build:
// a pre-release other than rc.N. An unparsable version is not judged one.
func isDevBuild(version string) bool {
	v, err := semver.StrictNewVersion(version)
	return err == nil && v.Prerelease() != "" && !releasePrerelease.MatchString(v.Prerelease())
}

// componentEnabled mirrors the chart's agent-platform.componentEnabled: a
// component without an entry or an explicit switch is on, except the kagent
// line's CRDs and Substrate, which follow components.kagent.
func componentEnabled(components map[string]any, name string) bool {
	c, ok := components[name].(map[string]any)
	if !ok {
		return true
	}
	if on, set := c[valuesEnabled]; set {
		b, _ := on.(bool)
		return b
	}
	if slices.Contains([]string{"kagent-crds", substrateRelease, "substrate-crds"}, name) {
		return componentEnabled(components, "kagent")
	}
	return true
}

// stringValue is m[key] as a string, empty when absent or not a string.
func stringValue(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
