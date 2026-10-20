package config

import (
	"fmt"
	"regexp"
	"strconv"
)

// labDomainLabel is the label a second lab's domain carries in front of the
// shared base: lab2.127.0.0.1.nip.io, lab3.…, every hostname then
// <service>.lab<N>.<base>.
var labDomainLabel = regexp.MustCompile(`^lab[0-9]+\.`)

// DomainChange records platform.domain moved off another lab's.
type DomainChange struct {
	From, To string
	// Lab is the registered lab that records From.
	Lab string
}

func (ch DomainChange) String() string {
	return fmt.Sprintf("platform.domain: %s is the lab %s's, using %s instead (every public hostname moves with it: backstage.%s, muster.%s, …)",
		ch.From, ch.Lab, ch.To, ch.To, ch.To)
}

// ChooseFreeDomain moves platform.domain off a domain another registered lab
// records (`recorded`, domain → lab name), to the first free lab<N>.<base>
// from N = 2, and returns the change, nil when there is none. Two labs on one
// domain share every hostname: the browser's cookies and HSTS pins for one
// apply to the other, which serves a certificate of another CA, and
// GitHub App and Dex callback URLs, derived from the hostnames, cannot tell
// them apart. Meant, like ChooseFreePorts, for a configuration whose cluster
// does not exist: the domain is in the lab CA's name constraints and every
// rendered URL. An externally provisioned pair (platform.tls) is never moved:
// its certificate covers the domain it was issued for.
func (c *Config) ChooseFreeDomain(recorded map[string]string) *DomainChange {
	from := c.Platform.Domain
	lab := recorded[from]
	if lab == "" || c.Platform.TLS.Set() {
		return nil
	}
	base := labDomainLabel.ReplaceAllString(from, "")
	for n := 2; ; n++ {
		to := "lab" + strconv.Itoa(n) + "." + base
		if recorded[to] == "" {
			c.Platform.Domain = to
			return &DomainChange{From: from, To: to, Lab: lab}
		}
	}
}
