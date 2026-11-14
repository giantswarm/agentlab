package shaper

import (
	"encoding/hex"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// netID tells networks apart, so latency baselines and the mirror's cap
// start over on a new one: the default route's interface and gateway, and
// the gateway's MAC (many home routers share one gateway address).
type netID struct{ iface, gw, mac string }

// same is false only for a different network: a MAC missing on either side
// (not in the ARP cache yet) does not tell networks apart.
func (n netID) same(o netID) bool {
	return n.iface == o.iface && n.gw == o.gw && (n.mac == o.mac || n.mac == "" || o.mac == "")
}

func (n netID) String() string {
	switch {
	case n.iface == "":
		return "offline"
	case n.gw == "":
		return n.iface + " (point-to-point)"
	default:
		return n.iface + " via " + n.gw
	}
}

// parseDefaultRoute picks the IPv4 default route with the lowest metric from
// /proc/net/route.
func parseDefaultRoute(table string) (iface, gw string) {
	best := -1
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		metric, err2 := strconv.Atoi(f[6])
		if err1 != nil || err2 != nil || flags&0x1 == 0 { // RTF_UP
			continue
		}
		if best >= 0 && metric >= best {
			continue
		}
		best, iface, gw = metric, f[0], ""
		if flags&0x2 != 0 { // RTF_GATEWAY
			// The kernel prints the address in host order, little-endian here.
			if raw, err := hex.DecodeString(f[2]); err == nil && len(raw) == 4 {
				gw = netip.AddrFrom4([4]byte{raw[3], raw[2], raw[1], raw[0]}).String()
			}
		}
	}
	return iface, gw
}

// parseARP finds ip's MAC on iface in /proc/net/arp; "" if incomplete.
func parseARP(table, ip, iface string) string {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 6 && f[0] == ip && f[5] == iface && f[3] != "00:00:00:00:00:00" {
			return f[3]
		}
	}
	return ""
}

// readNetID reads the current network of the shaper's network namespace —
// the host's, which it shares; a MAC that dropped out of the ARP cache for a
// moment keeps the previous one, so it isn't a new network.
func readNetID(prev netID) netID {
	var id netID
	if b, err := os.ReadFile("/proc/net/route"); err == nil {
		id.iface, id.gw = parseDefaultRoute(string(b))
	}
	if id.gw != "" {
		if b, err := os.ReadFile("/proc/net/arp"); err == nil {
			id.mac = parseARP(string(b), id.gw, id.iface)
		}
		if id.mac == "" && id.iface == prev.iface && id.gw == prev.gw {
			id.mac = prev.mac
		}
	}
	return id
}
