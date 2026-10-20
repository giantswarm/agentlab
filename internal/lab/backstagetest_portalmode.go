package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

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
			on, _ = strconv.ParseBool(v)
		}
	}
	return on, nil
}

// unknownImage names an image the Deployment does not report.
const unknownImage = "unknown"

// backstageVersion is the version an image runs: its tag, a digest dropped,
// or the image itself when it carries no tag.
func backstageVersion(image string) string {
	if image == "" {
		return unknownImage
	}
	ref, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return image
}

// checkServedPortal compares the portal mode Backstage serves with
// backstage.portal, and names the cause on a mismatch: a lab whose specs
// expect the shell and get the classic portal proves nothing about the shell.
func checkServedPortal(want string, served bool, image string) error {
	shell := want != config.PortalClassic
	if served == shell {
		return nil
	}
	version := backstageVersion(image)
	if image == "" {
		image = unknownImage
	}
	if shell {
		return fmt.Errorf("backstage.portal is %s, but the lab runs Backstage %s (image %s), which serves the classic Platform Portal only: its frontend config carries no app.portals.agentPlatform. The Agent Platform shell needs Backstage 3.x — install an agent-platform release whose components.backstage range resolves 3.x (`agentlab configure --defaults --chart-version %s`, then `agentlab platform`), swap in a 3.x build with platform.devImages, or keep this Backstage with `agentlab configure --portal %s`", want, version, image, config.DefaultChartVersion, config.PortalClassic)
	}
	return fmt.Errorf("backstage.portal is %s, but Backstage %s serves the Agent Platform shell (app.portals.agentPlatform on, image %s): `agentlab platform` rolls the overlay out", want, version, image)
}

// portalModeTimeout bounds how long checkPortalMode waits for the portal to
// answer through the edge after an install or a roll; a mismatch is final at
// the first answer.
const portalModeTimeout = 2 * time.Minute

// checkPortalMode reads which portal Backstage serves, from the frontend
// config its public index.html injects, and fails unless it is the one
// backstage.portal configures. It returns the image the portal runs.
func checkPortalMode(cfg *config.Config) (string, error) {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return "", err
	}
	var served bool
	var lastErr error
	ctx := context.Background()
	err = wait.PollUntilContextTimeout(ctx, pollInterval, portalModeTimeout, true, func(context.Context) (bool, error) {
		served, lastErr = fetchServedPortal(client, cfg.BackstageBaseURL()+"/")
		return lastErr == nil, nil
	})
	if err != nil {
		return "", fmt.Errorf("the portal's index.html within %s: %w", portalModeTimeout, lastErr)
	}
	image := deploymentImage(platformNamespace, componentBackstage)
	return image, checkServedPortal(cfg.Backstage.Portal, served, image)
}

// fetchServedPortal reads index.html and reports whether it turns the shell on.
func fetchServedPortal(client *http.Client, url string) (bool, error) {
	resp, err := client.Get(url)
	if err != nil {
		return false, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("answered %d: %.300s", resp.StatusCode, body)
	}
	return servedAgentPlatform(body)
}

// proveServedPortal is backstage-test's check, before any sign-in.
func proveServedPortal(cfg *config.Config) error {
	image, err := checkPortalMode(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("PASS: the portal serves the %s portal backstage.portal configures (Backstage %s)\n", portalMode(cfg), backstageVersion(image))
	return nil
}

// portalMode is backstage.portal spelled out, the empty default included.
func portalMode(cfg *config.Config) string {
	if cfg.Backstage.AgentPlatformPortal() {
		return config.PortalAgentPlatform
	}
	return config.PortalClassic
}

// proveNoHSTS is backstage-test's check that the edge drops the portal's
// HSTS header (postrenderers.go, patch 8): a browser that kept it would
// refuse the hostname once the lab CA changes, with no way past the warning.
func proveNoHSTS(cfg *config.Config) error {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return err
	}
	resp, err := client.Get(cfg.BackstageBaseURL() + "/")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if err := checkNoHSTS(resp.Header); err != nil {
		return fmt.Errorf("%s: %w", cfg.BackstageBaseURL(), err)
	}
	fmt.Println("PASS: the portal sends no HSTS through the edge")
	return nil
}

// checkNoHSTS refuses a response that carries the HSTS header.
func checkNoHSTS(h http.Header) error {
	if v := h.Get(hstsHeader); v != "" {
		return fmt.Errorf("the edge passes %s: %s — the connectivity chart's backstage route lost the lab's ResponseHeaderModifier (a rule beyond the first?)", hstsHeader, v)
	}
	return nil
}
