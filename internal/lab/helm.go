package lab

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// helmFloor is the Helm the platform install needs. Helm 4's `--wait` is
// kstatus over the chart's objects, custom resources included: `helm upgrade
// --install --wait` of the agent-platform chart returns when the FluxInstance
// and every component HelmRelease are Ready, which is what the platform
// install relies on and what the chart documents as measured. Helm 3's --wait
// skips custom resources — the command would return before a single component
// exists — and the chart's own README lists a Helm 3 install as not measured.
const helmFloor = "4.0.0"

// helmWhy is the one-clause reason behind helmFloor, worded for the person who
// has to install it.
const helmWhy = "the platform install relies on Helm 4's --wait, which waits on the chart's Flux custom resources (the platform is Ready when the command returns); Helm 3's does not"

// ensureHelmSupportsPlatform fails fast when the installed Helm cannot install
// the platform chart. `agentlab configure` refuses the same Helm before it asks
// anything (Discovery.Preflight); this is the guard of the lifecycle commands,
// whose agentlab.yaml may predate a change of the machine's tools — called
// before any cluster work so a Helm 3 user is not told after a five-minute
// boot.
func ensureHelmSupportsPlatform() error {
	version, err := probeHelmVersion()
	if err != nil {
		return fmt.Errorf("probing the helm version (`helm version`): %w", err)
	}
	below, err := belowFloor(version, helmFloor)
	if err != nil {
		return err
	}
	if below {
		return fmt.Errorf("helm %s cannot install the agent platform — Helm >= %s is required:\n%s", version, helmFloor, helmWhy)
	}
	return nil
}

// probeHelmVersion is `helm version --template {{.Version}}` ("v4.2.2").
func probeHelmVersion() (string, error) {
	raw, err := outputQuiet("helm", "version", "--template", "{{.Version}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// belowFloor reports whether a tool's reported version is below the floor the
// lab needs. The version is the first word of what the tool printed, with or
// without a leading v; a pre-release of the floor counts as reaching it
// (v4.0.0-rc.1 is a Helm 4). An unparseable version is an error, never a
// pass.
func belowFloor(version, floor string) (bool, error) {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return false, errors.New("empty version")
	}
	v, err := semver.NewVersion(fields[0])
	if err != nil {
		return false, fmt.Errorf("unparseable version %q", version)
	}
	release, _ := v.SetPrerelease("")
	return release.LessThan(semver.MustParse(floor)), nil
}
