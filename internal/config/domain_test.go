package config

import "testing"

const (
	defaultDomain = "127.0.0.1.nip.io"
	lab2Domain    = "lab2.127.0.0.2.nip.io"
	lab3Domain    = "lab3.127.0.0.3.nip.io"
	secondLab     = DefaultClusterName + "-2"
	secondAddress = "127.0.0.2"
)

func TestChooseFreeDomain(t *testing.T) {
	linux := func(string) bool { return true }
	macOS := func(addr string) bool { return addr == DefaultGatewayAddress }
	for name, tc := range map[string]struct {
		domain   string
		recorded map[string]string
		bindable func(string) bool
		tls      bool
		want     string // "" for no change
		wantAddr string
	}{
		"the only lab keeps the default":      {defaultDomain, nil, linux, false, "", ""},
		"a second lab takes 127.0.0.2":        {defaultDomain, map[string]string{defaultDomain: DefaultClusterName}, linux, false, lab2Domain, secondAddress},
		"a third lab skips a taken lab2":      {defaultDomain, map[string]string{defaultDomain: DefaultClusterName, lab2Domain: secondLab}, linux, false, lab3Domain, "127.0.0.3"},
		"a lab domain counts from its base":   {lab2Domain, map[string]string{defaultDomain: DefaultClusterName, lab2Domain: secondLab}, linux, false, lab3Domain, "127.0.0.3"},
		"a free lab domain stays":             {lab3Domain, map[string]string{defaultDomain: DefaultClusterName, lab2Domain: secondLab}, linux, false, "", ""},
		"without 127.0.0.2 the domain alone":  {defaultDomain, map[string]string{defaultDomain: DefaultClusterName}, macOS, false, "lab2." + defaultDomain, ""},
		"an own certificate's domain stays":   {defaultDomain, map[string]string{defaultDomain: DefaultClusterName}, linux, true, "", ""},
		"another base keeps its own name":     {"lab.example.com", map[string]string{"lab.example.com": DefaultClusterName}, linux, false, "lab2.lab.example.com", ""},
		"sslip.io takes the address like nip": {"127.0.0.1.sslip.io", map[string]string{"127.0.0.1.sslip.io": DefaultClusterName}, linux, false, "lab2.127.0.0.2.sslip.io", secondAddress},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Platform.Domain = tc.domain
			cfg.Platform.GatewayPort = 8443
			if tc.tls {
				cfg.Platform.TLS = PlatformTLS{CertFile: "tls.crt", KeyFile: "tls.key"}
			}
			ch := cfg.ChooseFreeDomain(tc.recorded, tc.bindable)
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
			if cfg.Platform.GatewayAddress != tc.wantAddr {
				t.Errorf("gatewayAddress %q, want %q", cfg.Platform.GatewayAddress, tc.wantAddr)
			}
			// A lab on its own address serves 443; one sharing 127.0.0.1
			// keeps the port the ports step gave it.
			if wantPort := map[bool]int{true: DefaultGatewayPort, false: 8443}[tc.wantAddr != ""]; cfg.Platform.GatewayPort != wantPort {
				t.Errorf("gatewayPort %d, want %d", cfg.Platform.GatewayPort, wantPort)
			}
		})
	}
}
