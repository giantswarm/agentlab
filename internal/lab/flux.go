package lab

import (
	"agentlab/internal/config"
)

// fluxChartVersion pins the fluxcd-community flux2 chart (Flux v2.9.1),
// installed from ghcr anonymously. Bump deliberately; the lab has no BOM for
// this one.
const fluxChartVersion = "2.19.0"

// fluxUp installs the minimal Flux the Backstage agent create flow needs:
// source-controller and helm-controller only (see flux-values.yaml.tmpl).
//
// This deliberately does NOT contradict the lab's no-GitOps stance: nothing
// here watches git, and the platform itself still installs with plain helm.
// The create flow's Deploy button applies a composed OCIRepository +
// HelmRelease straight to the cluster (scaffolder kube:apply via the
// agent-deployment Template), and these two controllers are the engine that
// turns those CRs into an installed agent chart. Without them the apply
// fails on the missing CRDs; with CRDs alone it would "succeed" and silently
// deliver nothing, which is worse.
func fluxUp(cfg *config.Config) error {
	if _, _, err := renderManifest(cfg, "flux-values.yaml.tmpl"); err != nil {
		return err
	}
	step("Installing Flux source+helm controllers (flux2 chart %s, for the agent create flow)", fluxChartVersion)
	// Same host-cache -> node rule as the platform images (see preload.go):
	// a separate Helm release, so the platform's chart-derived lane cannot
	// see these images. Best-effort — anything missed is pulled in-node
	// under the --wait timeout, and the snapshot manifest catches it for
	// the next boot.
	if rendered, err := outputQuiet("helm", "template", "flux",
		"oci://ghcr.io/fluxcd-community/charts/flux2",
		"--version", fluxChartVersion,
		"-n", "flux-system", "-f", StateDir+"/flux-values.yaml"); err == nil {
		if imgs := scrapeImages(rendered); len(imgs) > 0 {
			if res := sideloadImages(cfg, hostPullImages(imgs)); res.n > 0 {
				note("side-loaded %d flux images (%s)", res.n, res.d)
			}
		}
	}
	return runQuiet("helm", "upgrade", "--install", "flux",
		"oci://ghcr.io/fluxcd-community/charts/flux2",
		"--version", fluxChartVersion,
		"-n", "flux-system", "--create-namespace",
		"-f", StateDir+"/flux-values.yaml",
		"--wait", "--timeout", "5m")
}
