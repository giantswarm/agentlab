package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
)

// labDomainLabel is the label a second lab's domain carries in front of the
// shared base: lab2.127.0.0.1.nip.io, lab3.…, every hostname then
// <service>.lab<N>.<base>.
var labDomainLabel = regexp.MustCompile(`^lab[0-9]+\.`)

// loopbackWildcard matches a wildcard DNS domain that names a loopback
// address, 127.0.0.<x>.nip.io or .sslip.io: the services resolve any
// 127.0.0.<n> the same way, so a lab on its own loopback address gets a
// domain that resolves to it.
var loopbackWildcard = regexp.MustCompile(`^127\.0\.0\.[0-9]+\.((?:nip|sslip)\.io)$`)

// DefaultGatewayPort is the edge's port when nothing else holds it.
const DefaultGatewayPort = 443

// DomainChange records platform.domain moved off another lab's.
type DomainChange struct {
	From, To string
	// Address is the loopback address the edge moved to with the domain,
	// empty when it stayed.
	Address string
	// Lab is the registered lab that records From.
	Lab string
}

func (ch DomainChange) String() string {
	s := fmt.Sprintf("platform.domain: %s is the lab %s's, using %s instead (every public hostname moves with it: backstage.%s, muster.%s, …)",
		ch.From, ch.Lab, ch.To, ch.To, ch.To)
	if ch.Address != "" {
		s += fmt.Sprintf("\n  platform.gatewayAddress: %s (the edge serves %s:%d, so the URLs carry no port)", ch.Address, ch.Address, DefaultGatewayPort)
	}
	return s
}

// ChooseFreeDomain moves platform.domain off a domain another registered lab
// records (`recorded`, domain → lab name) and returns the change, nil when
// there is none. Two labs on one domain share every hostname: the browser's
// cookies and HSTS pins for one apply to the other, which serves a
// certificate of another CA, and GitHub App and Dex callback URLs, derived
// from the hostnames, cannot tell them apart.
//
// On a loopback wildcard domain (127.0.0.1.nip.io) lab n takes its own
// loopback address as well: domain lab<n>.127.0.0.<n>.nip.io, the edge on
// 127.0.0.<n>:443, for the first n from 2 whose domain no lab records and
// whose address this machine binds (bindable; Linux routes all of
// 127.0.0.0/8 to loopback, macOS only 127.0.0.1). Without such an address,
// and on any other domain, it is the first free lab<n>.<base>, the edge
// staying where it is.
//
// Meant, like ChooseFreePorts, for a configuration whose cluster does not
// exist: the domain is in the lab CA's name constraints and every rendered
// URL, the address in the node's port mapping. An externally provisioned pair
// (platform.tls) is never moved: its certificate covers the domain it was
// issued for.
func (c *Config) ChooseFreeDomain(recorded map[string]string, bindable func(addr string) bool) *DomainChange {
	from := c.Platform.Domain
	lab := recorded[from]
	if lab == "" || c.Platform.TLS.Set() {
		return nil
	}
	base := labDomainLabel.ReplaceAllString(from, "")
	if m := loopbackWildcard.FindStringSubmatch(base); m != nil {
		for n := 2; n < 255; n++ {
			addr := "127.0.0." + strconv.Itoa(n)
			to := "lab" + strconv.Itoa(n) + "." + addr + "." + m[1]
			if recorded[to] != "" || !bindable(addr) {
				continue
			}
			c.Platform.Domain, c.Platform.GatewayAddress, c.Platform.GatewayPort = to, addr, DefaultGatewayPort
			return &DomainChange{From: from, To: to, Address: addr, Lab: lab}
		}
	}
	for n := 2; ; n++ {
		to := "lab" + strconv.Itoa(n) + "." + base
		if recorded[to] == "" {
			c.Platform.Domain = to
			return &DomainChange{From: from, To: to, Lab: lab}
		}
	}
}

// LoopbackBindable reports whether this machine can bind addr: a listener on
// an ephemeral port of it opens.
func LoopbackBindable(addr string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(addr, "0"))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
