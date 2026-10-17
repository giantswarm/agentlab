package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// frontendConfigRe extracts the frontend config Backstage's app-backend
// injects into its public index.html, the config the app reads at start.
var frontendConfigRe = regexp.MustCompile(`(?s)<script type="backstage\.io/config">(.*?)</script>`)

// servedAgentPlatform reports whether index.html's injected frontend config
// turns the Agent Platform shell on (app.portals.agentPlatform). The last
// config that sets it wins, as in Backstage's ConfigReader.fromConfigs. A
// Backstage before the shell (2.x) declares no app.portals, so its
// app-backend injects none, whatever the lab's overlay sets.
func servedAgentPlatform(indexHTML []byte) (bool, error) {
	m := frontendConfigRe.FindSubmatch(indexHTML)
	if m == nil {
		return false, fmt.Errorf("index.html carries no injected frontend config (<script type=\"backstage.io/config\">)")
	}
	var configs []struct {
		Data struct {
			App struct {
				Portals struct {
					AgentPlatform any `json:"agentPlatform"`
				} `json:"portals"`
			} `json:"app"`
		} `json:"data"`
	}
	if err := json.Unmarshal(m[1], &configs); err != nil {
		return false, fmt.Errorf("the injected frontend config is not the expected JSON: %w", err)
	}
	on := false
	for _, c := range configs {
		switch v := c.Data.App.Portals.AgentPlatform.(type) {
		case bool:
			on = v
		case string:
			on = v == "true"
		}
	}
	return on, nil
}

// checkServedPortal compares the portal mode Backstage serves with
// backstage.portal, and names the cause on a mismatch: a lab whose specs
// expect the shell and get the classic portal proves nothing about the shell.
func checkServedPortal(want string, served bool, image string) error {
	shell := want != config.PortalClassic
	if served == shell {
		return nil
	}
	if image == "" {
		image = "unknown"
	}
	if shell {
		return fmt.Errorf("backstage.portal is %s, but the portal serves the classic Platform Portal only: its frontend config carries no app.portals.agentPlatform (image %s). A Backstage before 3.x has no Agent Platform shell — install an agent-platform release whose components.backstage range resolves 3.x (`agentlab configure --defaults --chart-version %s`, then `agentlab platform`), or swap in a 3.x build with platform.devImages", want, image, config.DefaultChartVersion)
	}
	return fmt.Errorf("backstage.portal is %s, but the portal serves the Agent Platform shell (app.portals.agentPlatform on, image %s): `agentlab platform` rolls the overlay out", want, image)
}

// proveServedPortal reads, before any sign-in, which portal Backstage serves
// and fails unless it is the one backstage.portal configures.
func proveServedPortal(cfg *config.Config) error {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return err
	}
	resp, err := client.Get(cfg.BackstageBaseURL() + "/")
	if err != nil {
		return fmt.Errorf("the portal's index.html: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("the portal's index.html: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the portal's index.html answered %d: %.300s", resp.StatusCode, body)
	}
	served, err := servedAgentPlatform(body)
	if err != nil {
		return err
	}
	image := deploymentImage(platformNamespace, componentBackstage)
	if err := checkServedPortal(cfg.Backstage.Portal, served, image); err != nil {
		return err
	}
	mode := config.PortalClassic
	if served {
		mode = config.PortalAgentPlatform
	}
	fmt.Printf("PASS: the portal serves the %s portal backstage.portal configures (image %s)\n", mode, image)
	return nil
}
