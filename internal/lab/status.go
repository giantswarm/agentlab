package lab

import (
	"os"
	"sync"
	"time"

	"agentlab/internal/config"
)

// Status is a point-in-time probe of every lab component, for the TUI
// dashboard. The *Up fields mean "answers right now"; whether a component is
// enabled at all comes from the config, so the dashboard can tell "off by
// configuration" from "down".
type Status struct {
	CertsPresent      bool
	CATrusted         bool // the lab CA is in the system trust store (`agentlab trust`)
	ClusterUp         bool
	DexUp             bool
	MusterUp          bool
	AgentsUp          bool // the kagent UI on its host port
	BackstageUp       bool
	KubeconfigPresent bool // kubeconfig.oidc from a previous login
}

// Probe runs every check concurrently, so the slowest probe (an HTTP timeout,
// 2s) bounds the wall time and the TUI can call this on a ticker.
func Probe(cfg *config.Config) Status {
	var s Status
	if _, err := os.Stat(caCertPath); err == nil {
		s.CertsPresent = true
	}
	if _, err := os.Stat("kubeconfig.oidc"); err == nil {
		s.KubeconfigPresent = true
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		s.ClusterUp = kindClusterExists(cfg.ClusterName)
	})
	if s.CertsPresent {
		wg.Go(func() {
			s.CATrusted = SystemTrusted()
		})
	}
	// No certs yet means Dex cannot be serving its cert either; skip the HTTP
	// probes instead of failing them slowly.
	if client, err := labHTTPClient(2 * time.Second); err == nil {
		wg.Go(func() {
			s.DexUp = httpUp(client, cfg.Issuer()+"/.well-known/openid-configuration")
		})
		if cfg.Platform.Enabled {
			wg.Go(func() {
				s.MusterUp = httpUp(client, cfg.MusterBaseURL()+"/.well-known/oauth-authorization-server")
			})
		}
		if cfg.Platform.Enabled && cfg.Platform.Agents {
			wg.Go(func() {
				s.AgentsUp = httpUp(client, cfg.KagentUIBaseURL())
			})
		}
		if cfg.Backstage.Enabled {
			wg.Go(func() {
				s.BackstageUp = httpUp(client, cfg.BackstageBaseURL())
			})
		}
	}
	wg.Wait()
	return s
}
