package lab

import (
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// secondEdgeAddress is a second lab's loopback address.
const secondEdgeAddress = "127.0.0.2"

// The lab's own clients dial its hostnames on the address the edge is
// published on; anything else is dialed as asked.
func TestDialLabAddr(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Domain = "lab2.127.0.0.2.nip.io"
	cfg.Platform.GatewayAddress = secondEdgeAddress
	SetPlatform(cfg)
	t.Cleanup(func() { SetPlatform(&config.Config{}) })
	for in, want := range map[string]string{
		"muster.lab2.127.0.0.2.nip.io:443": secondEdgeAddress + ":443",
		"lab2.127.0.0.2.nip.io:443":        secondEdgeAddress + ":443",
		"muster.127.0.0.1.nip.io:443":      "muster.127.0.0.1.nip.io:443",
		"localhost:32003":                  "localhost:32003",
	} {
		if got := dialLabAddr(in); got != want {
			t.Errorf("dialLabAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
