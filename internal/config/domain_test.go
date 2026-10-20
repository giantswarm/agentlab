package config

import "testing"

const (
	defaultDomain = "127.0.0.1.nip.io"
	lab2Domain    = "lab2." + defaultDomain
	lab3Domain    = "lab3." + defaultDomain
)

func TestChooseFreeDomain(t *testing.T) {
	for name, tc := range map[string]struct {
		domain   string
		recorded map[string]string
		tls      bool
		want     string // "" for no change
	}{
		"the only lab keeps the default":    {defaultDomain, nil, false, ""},
		"a second lab moves to lab2":        {defaultDomain, map[string]string{defaultDomain: "agentlab"}, false, lab2Domain},
		"a third lab skips a taken lab2":    {defaultDomain, map[string]string{defaultDomain: "agentlab", lab2Domain: "agentlab-2"}, false, lab3Domain},
		"a lab domain counts from its base": {lab2Domain, map[string]string{defaultDomain: "agentlab", lab2Domain: "agentlab-2"}, false, lab3Domain},
		"a free lab domain stays":           {lab3Domain, map[string]string{defaultDomain: "agentlab", lab2Domain: "agentlab-2"}, false, ""},
		"an own certificate's domain stays": {defaultDomain, map[string]string{defaultDomain: "agentlab"}, true, ""},
		"another base keeps its own name":   {"lab.example.com", map[string]string{"lab.example.com": "agentlab"}, false, "lab2.lab.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Platform.Domain = tc.domain
			if tc.tls {
				cfg.Platform.TLS = PlatformTLS{CertFile: "tls.crt", KeyFile: "tls.key"}
			}
			ch := cfg.ChooseFreeDomain(tc.recorded)
			switch {
			case tc.want == "" && ch != nil:
				t.Errorf("moved to %s, want no change", ch.To)
			case tc.want == "" && cfg.Platform.Domain != tc.domain:
				t.Errorf("domain %s, want %s kept", cfg.Platform.Domain, tc.domain)
			case tc.want != "" && (ch == nil || ch.To != tc.want || cfg.Platform.Domain != tc.want):
				t.Errorf("change %+v, domain %s; want %s", ch, cfg.Platform.Domain, tc.want)
			case tc.want != "" && ch.Lab != tc.recorded[tc.domain]:
				t.Errorf("change names lab %q, want %q", ch.Lab, tc.recorded[tc.domain])
			}
		})
	}
}
