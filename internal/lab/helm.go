package lab

import (
	"fmt"
	"strconv"
	"strings"
)

// ensureHelmSupportsPlatform fails fast when the installed Helm cannot
// install the platform chart. Helm 4's `--wait` is kstatus over the chart's
// objects, custom resources included: `helm upgrade --install --wait` of the
// agent-platform chart returns when the FluxInstance and every component
// HelmRelease are Ready, which is what the platform install relies on and
// what the chart documents as measured. Helm 3's --wait skips custom
// resources — the command would return before a single component exists —
// and the chart's own README lists a Helm 3 install as not measured. Called
// before any cluster work so a Helm 3 user is not told after a five-minute
// boot.
func ensureHelmSupportsPlatform() error {
	raw, err := outputQuiet("helm", "version", "--template", "{{.Version}}")
	if err != nil {
		return fmt.Errorf("probing the helm version (`helm version`): %w", err)
	}
	version := strings.TrimSpace(raw)
	major, err := helmMajor(version)
	if err != nil {
		return err
	}
	if major < 4 {
		return fmt.Errorf("helm %s cannot install the agent platform — Helm >= 4 is required:\n"+
			"the install relies on Helm 4's --wait, which waits on the chart's Flux custom\n"+
			"resources (the platform is Ready when the command returns); Helm 3's does not,\n"+
			"and the agent-platform chart documents a Helm 3 install as not measured", version)
	}
	return nil
}

// helmMajor extracts the major version from `helm version --template
// {{.Version}}` output ("v4.2.2").
func helmMajor(version string) (int, error) {
	majorStr, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), ".")
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return 0, fmt.Errorf("unparseable helm version %q", version)
	}
	return major, nil
}
