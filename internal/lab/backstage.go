package lab

import (
	"fmt"
	"slices"

	"github.com/giantswarm/agentlab/internal/config"
)

// BackstageUp is the retired bespoke deployment path: Backstage now deploys
// as part of the platform install — the umbrella chart's backstage component,
// wired to Dex through global.identity and published through the agentgateway
// edge. The subcommand remains as a signpost.
func BackstageUp(cfg *config.Config) error {
	if !cfg.Backstage.Enabled {
		return fmt.Errorf("backstage.enabled is false in %s — enable it (agentlab configure), then run `agentlab up`", config.File)
	}
	fmt.Println("Backstage deploys with the platform now (the umbrella chart's backstage component).")
	fmt.Printf("Run `agentlab up` (or `agentlab platform`) — it installs and verifies Backstage on %s.\n", cfg.BackstageBaseURL())
	return nil
}

// nodeHasImage checks the kind node's containerd for an exact repo:tag match.
func nodeHasImage(node, image string) bool {
	tags, err := nodeImageTags(node)
	if err != nil {
		return false
	}
	return slices.Contains(tags, image)
}
