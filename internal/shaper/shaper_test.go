package shaper

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	for in, want := range map[string]float64{
		"128kbit": 128e3, "20mbit": 20e6, "1.5gbit": 1.5e9, "500k": 500e3,
		"8M": 8e6, " 1g ": 1e9, "1000000": 1e6, "64bit": 64,
	} {
		got, err := ParseRate(in)
		if err != nil || got != want {
			t.Errorf("ParseRate(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "fast", "-1mbit", "0", "10mbps", "1e999mbit", "1e308gbit"} {
		if _, err := ParseRate(in); err == nil {
			t.Errorf("ParseRate(%q) took it", in)
		}
	}
}

func TestFormatRate(t *testing.T) {
	for bps, want := range map[float64]string{
		0: "unlimited", 128e3: "128 kbit/s", 8e6: "8.0 Mbit/s", 2.5e9: "2.50 Gbit/s",
	} {
		if got := FormatRate(bps); got != want {
			t.Errorf("FormatRate(%v) = %q, want %q", bps, got, want)
		}
	}
	if got := formatThroughput(10); got != "nothing" {
		t.Errorf("an idle mirror's throughput = %q, want nothing", got)
	}
}

func TestModeRoundTrip(t *testing.T) {
	for _, in := range []string{"auto", "auto 20mbit", "pause", "off"} {
		m, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", in, err)
		}
		back, err := ParseMode(m.String())
		if err != nil || back != m {
			t.Errorf("%q -> %v -> %v (%v)", in, m, back, err)
		}
	}
	if m, err := ParseMode("auto 20mbit"); err != nil || m.Max != 20e6 || m.String() != "auto 20mbit" {
		t.Errorf("auto 20mbit = %+v (%s), %v", m, m, err)
	}
	for bps, want := range map[float64]string{5e6: "5mbit", 1.5e6: "1500kbit", 2.5e9: "2500mbit", 1e9: "1gbit", 64: "64bit"} {
		if got := RateArg(bps); got != want {
			t.Errorf("RateArg(%v) = %q, want %q", bps, got, want)
		}
	}
	if m, err := ParseMode(""); err != nil || m.Kind != ModeAuto {
		t.Errorf("empty mode = %+v, %v; want auto", m, err)
	}
	for _, in := range []string{"fast", "pause 1mbit", "auto 1 2", "auto nope"} {
		if _, err := ParseMode(in); err == nil {
			t.Errorf("ParseMode(%q) took it", in)
		}
	}
}

func TestReadModeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ModeFile)
	if m, err := readModeFile(path); err != nil || m.Kind != ModeAuto {
		t.Errorf("no mode file = %v, %v; want auto", m, err)
	}
	if err := WriteMode(dir, Mode{Kind: ModeAuto, Max: 5e6}); err != nil {
		t.Fatal(err)
	}
	if m, err := readModeFile(path); err != nil || m.String() != "auto 5mbit" {
		t.Errorf("mode file with auto 5mbit = %v, %v", m, err)
	}
	if err := os.WriteFile(path, []byte("turbo junk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err := readModeFile(path); err == nil || m.Kind != ModeAuto || strings.Contains(err.Error(), "junk") {
		t.Errorf("mode file without a mode = %v, %v; want auto and an error that doesn't echo it", m, err)
	}
	// A link is not followed, even to a valid mode file.
	if err := os.WriteFile(path, []byte("pause\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if m, err := readModeFile(link); err == nil || m.Kind != ModeAuto {
		t.Errorf("symlinked mode file = %v, %v; want auto and an error", m, err)
	}
	// A FIFO neither counts nor blocks the shaper's tick loop.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		m, err := readModeFile(fifo)
		if err == nil || m.Kind != ModeAuto {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO as the mode file counted, or gave no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO as the mode file blocked")
	}
}

func TestConfigCheck(t *testing.T) {
	if err := DefaultConfig().Check(); err != nil {
		t.Fatalf("the default configuration: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no reflectors":  func(c *Config) { c.Reflectors = nil },
		"IPv6 reflector": func(c *Config) { c.Reflectors = []netip.Addr{netip.MustParseAddr("::1")} },
		"floor above":    func(c *Config) { c.Min, c.Max = 10e6, 1e6 },
		"start outside":  func(c *Config) { c.Start = 100e3 },
		"target noise":   func(c *Config) { c.Target = time.Millisecond },
		"fast interval":  func(c *Config) { c.Interval = 10 * time.Millisecond },
		"no mirror pid":  func(c *Config) { c.MirrorPID = 0 },
	} {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: Check took it", name)
		}
	}
}

// homeGateway is the gateway address many home routers share, behind the
// laptop's Wi-Fi.
const (
	homeGateway = "192.168.178.1"
	wifi        = "wlan0"
)

func TestParseDefaultRoute(t *testing.T) {
	table := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	01B2A8C0	0003	0	0	200	00000000	0	0	0
tun0	0000000A	0100000A	0003	0	0	200	000000FF	0	0	0
usb0	00000000	012BA8C0	0003	0	0	1024	00000000	0	0	0
wlan0	00B2A8C0	00000000	0001	0	0	200	00FFFFFF	0	0	0
`
	iface, gw := parseDefaultRoute(table)
	if iface != wifi || gw != homeGateway {
		t.Errorf("default route = %s via %s", iface, gw)
	}
	ptp := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\ntun0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n"
	if iface, gw := parseDefaultRoute(ptp); iface != "tun0" || gw != "" {
		t.Errorf("point-to-point default = %q via %q", iface, gw)
	}
	if iface, _ := parseDefaultRoute("Iface\n"); iface != "" {
		t.Errorf("no default route, got %q", iface)
	}
}

func TestParseARP(t *testing.T) {
	arp := `IP address       HW type     Flags       HW address            Mask     Device
192.168.178.1    0x1         0x2         3c:a6:2f:01:02:03     *        wlan0
172.21.0.2       0x1         0x2         02:42:ac:15:00:02     *        br-2f5c7b46f765
192.168.178.9    0x1         0x0         00:00:00:00:00:00     *        wlan0
`
	if mac := parseARP(arp, homeGateway, wifi); mac != "3c:a6:2f:01:02:03" {
		t.Errorf("gateway MAC = %q", mac)
	}
	if mac := parseARP(arp, "192.168.178.9", "wlan0"); mac != "" {
		t.Errorf("incomplete entry gave %q", mac)
	}
}

func TestNetIDSame(t *testing.T) {
	home := netID{wifi, homeGateway, "3c:a6:2f:01:02:03"}
	elsewhere := netID{wifi, homeGateway, "dc:39:6f:aa:bb:cc"}
	if !home.same(netID{wifi, homeGateway, ""}) {
		t.Error("a MAC not yet in the ARP cache made it a new network")
	}
	if home.same(elsewhere) {
		t.Error("another router behind the same address is the same network")
	}
	if home.same(netID{"tun0", "10.0.0.1", ""}) {
		t.Error("another default route is the same network")
	}
}

func TestEchoPacket(t *testing.T) {
	p := echoRequest(0xbeef, 7)
	if icmpChecksum(p) != 0 {
		t.Error("checksum does not verify")
	}
	reply := append([]byte{}, p...)
	reply[0] = 0
	if id, seq, ok := parseEchoReply(reply); !ok || id != 0xbeef || seq != 7 {
		t.Errorf("reply = %x %d %v", id, seq, ok)
	}
	if _, _, ok := parseEchoReply(p); ok {
		t.Error("a request parsed as a reply")
	}
}

func TestStateLines(t *testing.T) {
	st := State{
		Mode: "auto", Device: "veth1a2b3c", Shaping: true, CapBps: 42e6, MirrorBps: 39.8e6,
		DelayMs: 6, DelayKnown: true, TargetMs: 30,
		Reason: "link has room, the mirror may take more", Network: "wlan0 via 192.168.178.1",
		Reflectors: []ReflectorState{{Addr: "1.1.1.1", BaseMs: 12, LastMs: 18}},
	}
	for _, want := range []string{"the mirror capped at 42.0 Mbit/s", "getting 39.8 Mbit/s", "+6 ms over baseline", "CAKE on veth1a2b3c", "1.1.1.1 18/12 ms"} {
		if got := st.Describe(); !strings.Contains(got, want) {
			t.Errorf("Describe lacks %q:\n%s", want, got)
		}
	}
	if got, want := st.Summary(), "the mirror capped at 42.0 Mbit/s (auto), getting 39.8 Mbit/s, the link +6 ms over its baseline: link has room, the mirror may take more"; got != want {
		t.Errorf("Summary\n got  %s\n want %s", got, want)
	}
	st.CapBps, st.MirrorBps, st.DelayKnown = 0, 0, false
	st.Reason = "no latency probe answers on this network (ICMP dropped?), so the mirror is not limited"
	if got := st.Summary(); !strings.Contains(got, "the mirror not limited") || !strings.Contains(got, "getting nothing") || !strings.Contains(got, "ICMP dropped") {
		t.Errorf("unlimited without a signal: %s", got)
	}
	st.Shaping, st.Problem = false, "the kernel has no CAKE qdisc (sch_cake)"
	if got := st.Summary(); got != "not shaping: the kernel has no CAKE qdisc (sch_cake)" {
		t.Errorf("without CAKE: %s", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := State{Mode: "pause", CapBps: 128e3, Updated: time.Unix(1_800_000_000, 0).UTC()}
	if err := writeState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(dir)
	if err != nil || got.Mode != st.Mode || got.CapBps != st.CapBps || !got.Updated.Equal(st.Updated) {
		t.Errorf("ReadState = %+v, %v; want %+v", got, err, st)
	}
	if _, err := os.Stat(filepath.Join(dir, StateFile+".tmp")); !os.IsNotExist(err) {
		t.Errorf("the temporary file stayed behind: %v", err)
	}
}

// The proof's verdicts: the mirror's samples count from the run's start, the
// person's from their download's; latency leaves out the cap's ramp and the
// settling after the person's download.
func TestProofVerdict(t *testing.T) {
	var mirror, person []Sample
	for s := 1; s <= int(proofDuration.Seconds()); s++ {
		bps := 16e6
		if s > 30 && s <= 55 {
			bps = 6e6
		}
		mirror = append(mirror, Sample{T: s, Bps: bps})
	}
	for s := 1; s <= int(proofPersonFor.Seconds()); s++ {
		person = append(person, Sample{T: s, Bps: 12e6})
	}
	var pings []rttSample
	for i := range int(proofDuration / proofPing) {
		at := time.Duration(i) * proofPing
		rtt := 30 * time.Millisecond
		if at < proofRamp || at >= proofPersonFrom && at < proofPersonFrom+proofPersonFor+proofSettle {
			rtt = 300 * time.Millisecond // left out
		}
		pings = append(pings, rttSample{at: at, rtt: rtt})
	}
	v := judge(proofRun{mirror: mirror, person: person, pings: pings}, 20*time.Millisecond)
	if v.personDownload != 12e6 || v.mirrorShared != 6e6 || v.mirrorAlone != 16e6 {
		t.Errorf("throughput: person %v, mirror beside %v, alone %v", v.personDownload, v.mirrorShared, v.mirrorAlone)
	}
	if v.median != 30*time.Millisecond || len(v.failures) != 0 {
		t.Errorf("median %v, failures %v; want 30ms and none", v.median, v.failures)
	}
	for i := range person {
		person[i].Bps = 4e6
	}
	if v := judge(proofRun{mirror: mirror, person: person, pings: pings}, 5*time.Millisecond); len(v.failures) != 2 {
		t.Errorf("a starved person at 4 of 20 Mbit/s and +25 ms: %v, want both criteria failed", v.failures)
	}
}
