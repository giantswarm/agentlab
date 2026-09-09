package lab

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The dev channel (docs/platform.md "Dev channel"): platform.chartBranch
// names an agent-platform branch, and the lab follows that branch's newest
// dev build — the chart gitsemver publishes for every commit of a branch
// with branch publishing on (giantswarm/github `gen.ci.branchPublish`), the
// way a Flux OCIRepository with `semver: "*-*"` and a `semverFilter` on the
// branch would. Resolution happens where a version is chosen — `configure`,
// `up`, `platform` — and writes the tag it picked into platform.chartVersion,
// so everything downstream (the render, the image preload, the install, a
// re-run, `helm history`) sees one exact version, as on the stable channel.

// devTagRe is the shape of gitsemver's (v2.0.1) dev prerelease:
// `dev.<branch>.<YYYY-MM-DD>.<HH-MM-SS>[.h<sha7>]`, the branch a lowercase
// [a-z0-9-] name (config.SanitizeBranch), possibly shortened with a `--`
// marker in its middle to fit gitsemver's 63-character version budget. The
// date and time compare lexically per semver identifier, so within a branch
// the highest version is the newest build.
var devTagRe = regexp.MustCompile(`^dev\.([a-z0-9-]+)\.\d{4}-\d{2}-\d{2}\.\d{2}-\d{2}-\d{2}(?:\.h[0-9a-f]{7})?$`)

// devTruncationMarker is what gitsemver puts in place of a long branch
// name's dropped middle.
const devTruncationMarker = "--"

// devTagFilter is THE definition of "a dev build of this branch": the
// predicate a chart version's prerelease must satisfy. Today it reads
// gitsemver's `dev.<sanitized branch>.<date>.<time>.h<sha>` schema (the
// one architect ships); the RFC's successor schema
// `-b<crc32(branch) 8 hex>t<YYYYMMDDHHMMSS>c<sha7>` is switched here, in
// this one function, when gitsemver ships it — nothing else in the lab
// knows what a dev tag looks like.
func devTagFilter(branch string) func(prerelease string) bool {
	want := config.SanitizeBranch(branch)
	return func(prerelease string) bool {
		m := devTagRe.FindStringSubmatch(prerelease)
		if m == nil {
			return false
		}
		got := m[1]
		if got == want {
			return true
		}
		// gitsemver keeps a long branch's head and tail around the marker;
		// a sanitized name never contains "--" itself, so the marker is
		// unambiguous. Either side may be empty when the budget was tight.
		head, tail, truncated := strings.Cut(got, devTruncationMarker)
		return truncated && len(head)+len(tail) < len(want) &&
			strings.HasPrefix(want, head) && strings.HasSuffix(want, tail)
	}
}

// pickDevTag chooses the newest dev build of a branch among a chart
// repository's tags: the highest semver version whose prerelease the branch's
// devTagFilter accepts. Tags that are no version, releases and other
// branches' builds are skipped; the order of the list does not matter.
func pickDevTag(tags []string, branch string) (string, bool) {
	matches := devTagFilter(branch)
	var best *semver.Version
	for _, tag := range tags {
		v, err := semver.StrictNewVersion(tag)
		if err != nil || !matches(v.Prerelease()) {
			continue
		}
		if best == nil || v.GreaterThan(best) {
			best = v
		}
	}
	if best == nil {
		return "", false
	}
	return best.Original(), true
}

// ResolveChartVersion follows platform.chartBranch: it lists the chart
// repository's tags, picks the branch's newest dev build and writes it into
// cfg.Platform.ChartVersion, reporting whether that changed the config (the
// caller saves). A pinned lab (platform.chartPinned) keeps its recorded
// version; the stable channel (no chartBranch) is left alone. No build of
// the branch is a refusal with the two ways out: wait for the branch's
// publish, or pin a tag by hand.
func ResolveChartVersion(cfg *config.Config) (changed bool, err error) {
	branch := cfg.Platform.ChartBranch
	if branch == "" {
		return false, nil
	}
	if cfg.Platform.ChartPinned {
		note("chart agent-platform %s (branch %s, pinned — `agentlab platform --pin=false` follows the branch again)", cfg.Platform.ChartVersion, branch)
		return false, nil
	}
	tags, err := helmChartTags(config.ChartRepository)
	if err != nil {
		return false, fmt.Errorf("resolving the dev channel of branch %s: %w", branch, err)
	}
	tag, ok := pickDevTag(tags, branch)
	if !ok {
		return false, fmt.Errorf("no dev build of branch %s in %s (no tag of the form X.Y.Z-dev.%s.<date>.<time>.h<sha> among %d);\n"+
			"either the branch's publish has not run yet (agent-platform needs `gen.ci.branchPublish` in giantswarm/github and a commit on the branch),\n"+
			"or pin a build by hand: `agentlab configure --chart-version <full dev tag>` after `--chart-branch \"\"`",
			branch, config.ChartRepository, config.SanitizeBranch(branch), len(tags))
	}
	changed = tag != cfg.Platform.ChartVersion
	cfg.Platform.ChartVersion = tag
	note("chart agent-platform %s (branch %s)", tag, branch)
	return changed, nil
}
