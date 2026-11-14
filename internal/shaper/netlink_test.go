package shaper

import (
	"bytes"
	"strings"
	"testing"
)

// nested builds a nested attribute's payload from attributes.
func nested(attrs ...[]byte) []byte { return bytes.Join(attrs, nil) }

// The request CAKE is replaced with carries the shaper's settings; a
// change carries the bandwidth alone, so the kernel keeps the rest. tc's
// bandwidth is bytes per second.
func TestCakeRequest(t *testing.T) {
	full := cakeRequest(7, 20e6, true)
	if got := nativeEndian.Uint32(full[4:]); got != 7 {
		t.Errorf("ifindex = %d, want 7", got)
	}
	if got := nativeEndian.Uint32(full[12:]); got != tcHRoot {
		t.Errorf("parent = %#x, want the root", got)
	}
	attrs := parseAttrs(full[sizeofTcMsg:])
	if kind := attrString(attr(attrs, tcaKind)); kind != cakeKind {
		t.Errorf("kind = %q", kind)
	}
	opts := parseAttrs(attr(attrs, tcaOptions))
	want := map[uint16]uint64{
		tcaCakeBaseRate64: 2_500_000, tcaCakeDiffservMode: cakeDiffservBesteffort,
		tcaCakeFlowMode: cakeFlowDualDst, tcaCakeNat: 1, tcaCakeIngress: 1,
	}
	for typ, v := range want {
		data := attr(opts, typ)
		var got uint64
		switch len(data) {
		case 8:
			got = nativeEndian.Uint64(data)
		case 4:
			got = uint64(nativeEndian.Uint32(data))
		default:
			t.Errorf("option %d: %d bytes", typ, len(data))
			continue
		}
		if got != v {
			t.Errorf("option %d = %d, want %d", typ, got, v)
		}
	}
	change := parseAttrs(attr(parseAttrs(cakeRequest(7, 0, false)[sizeofTcMsg:]), tcaOptions))
	if len(change) != 1 || change[0].typ != tcaCakeBaseRate64 || nativeEndian.Uint64(change[0].data) != 0 {
		t.Errorf("a change to unlimited carries %+v, want the bandwidth 0 alone", change)
	}
	if got := bytesPerSecond(1); got != 1 {
		t.Errorf("a positive rate rounds to %d bytes/s, want at least 1 (0 is unlimited)", got)
	}
}

// parseQdisc reads CAKE's bandwidth, the bytes it delivered and what it
// dropped or marked out of an RTM_NEWQDISC as the kernel nests it.
func TestParseQdisc(t *testing.T) {
	basic := make([]byte, 16) // struct gnet_stats_basic, padded
	nativeEndian.PutUint64(basic, 123456)
	nativeEndian.PutUint32(basic[8:], 90)
	queue := make([]byte, 20) // struct gnet_stats_queue
	nativeEndian.PutUint32(queue[8:], 7)
	tin := func(marks uint32) []byte {
		return nested(encodeAttr(2, u32(90)), encodeAttr(tcaCakeTinStatsEcnMarkedPackets, u32(marks)))
	}
	body := append(tcMsg(3), nested(
		encodeAttr(tcaKind, cString(cakeKind)),
		encodeAttr(tcaOptions, nested(encodeAttr(tcaCakeBaseRate64, u64(2_500_000)), encodeAttr(tcaCakeNat, u32(1)))),
		encodeAttr(tcaStats2|0x8000, nested(
			encodeAttr(tcaStatsBasic, basic),
			encodeAttr(tcaStatsQueue, queue),
			encodeAttr(tcaStatsApp, nested(
				encodeAttr(2, u64(1)),
				encodeAttr(tcaCakeStatsTinStats, nested(encodeAttr(1, tin(3)))),
			)),
		)),
	)...)
	q, err := parseQdisc(body)
	if err != nil {
		t.Fatal(err)
	}
	if q.kind != cakeKind || q.rate != 20e6 || q.bytes != 123456 || q.dropped != 10 {
		t.Errorf("qdisc = %+v; want cake at 20 Mbit/s, 123456 bytes, 10 dropped or marked", q)
	}
	noqueue := append(tcMsg(3), encodeAttr(tcaKind, cString("noqueue"))...)
	if q, err := parseQdisc(noqueue); err != nil || q.kind != "noqueue" || q.bytes != 0 {
		t.Errorf("noqueue = %+v, %v", q, err)
	}
	if _, err := parseQdisc([]byte{1, 2}); err == nil {
		t.Error("a short message parsed")
	}
}

// The mirror's veth is the one whose peer lives in the mirror's namespace:
// a link dump's IFLA_LINK_NETNSID says which.
func TestPeerVeth(t *testing.T) {
	link := func(index uint32, name, kind string, netnsid uint32, has bool) []byte {
		b := make([]byte, sizeofIfInfomsg)
		nativeEndian.PutUint32(b[4:], index)
		b = append(b, encodeAttr(iflaIfname, cString(name))...)
		b = append(b, encodeAttr(iflaLinkinfo|0x8000, encodeAttr(iflaInfoKind, cString(kind)))...)
		if has {
			b = append(b, encodeAttr(iflaLinkNetnsid, u32(netnsid))...)
		}
		return b
	}
	var links []netLink
	for _, body := range [][]byte{
		link(1, "lo", "", 0, false),
		link(4, "br-0123", "bridge", 0, false),
		link(17, "veth1111111", vethKind, 2, true),
		link(23, "veth2222222", vethKind, 5, true),
		link(30, "vxlan0", "vxlan", 5, true),
	} {
		l, ok := parseLink(body)
		if !ok {
			t.Fatalf("parseLink failed on %x", body)
		}
		links = append(links, l)
	}
	if links[2].name != "veth1111111" || links[2].index != 17 || links[2].kind != vethKind || links[2].netnsid != 2 || !links[2].hasNetnsid {
		t.Fatalf("parsed link = %+v", links[2])
	}
	if dev, err := peerVeth(links, 5); err != nil || dev.name != "veth2222222" {
		t.Errorf("peerVeth(5) = %+v, %v; want veth2222222 (a vxlan to the same namespace is no veth)", dev, err)
	}
	if _, err := peerVeth(links, 9); err == nil {
		t.Error("no veth leads to namespace 9, but one was found")
	}
	links = append(links, netLink{index: 31, name: "veth3333333", kind: vethKind, netnsid: 5, hasNetnsid: true})
	if _, err := peerVeth(links, 5); err == nil || !strings.Contains(err.Error(), "veth2222222, veth3333333") {
		t.Errorf("two veths into the mirror's namespace: %v; want an error naming both", err)
	}
}

func TestNsidMessages(t *testing.T) {
	req := nsidRequest(4242)
	if pid := attr(parseAttrs(req[sizeofRtGenmsg:]), netnsaPid); len(pid) != 4 || nativeEndian.Uint32(pid) != 4242 {
		t.Errorf("request carries pid %x", pid)
	}
	reply := append(make([]byte, sizeofRtGenmsg), encodeAttr(netnsaNsid, u32(uint32(0xffffffff)))...)
	if id, ok := parseNsid(reply); !ok || id != -1 {
		t.Errorf("an unassigned nsid = %d, %v; want -1", id, ok)
	}
}

// splitMessages cuts a datagram into its messages and errnoOf reads the
// kernel's answer code.
func TestSplitMessages(t *testing.T) {
	ack := append(header(sizeofNlMsghdr+4, nlmsgError, 0, 9, 77), u32(0)...)
	refusal := append(header(sizeofNlMsghdr+4, nlmsgError, 0, 9, 77), u32(uint32(0xfffffffe))...) // -ENOENT
	msgs, seqs, pids := splitMessages(append(ack, refusal...))
	if len(msgs) != 2 || seqs[1] != 9 || pids[0] != 77 {
		t.Fatalf("split = %+v %v %v", msgs, seqs, pids)
	}
	if errnoOf(msgs[0].body) != 0 || errnoOf(msgs[1].body) != -2 {
		t.Errorf("codes = %d, %d; want 0 and -2", errnoOf(msgs[0].body), errnoOf(msgs[1].body))
	}
	if msgs, _, _ := splitMessages(ack[:10]); len(msgs) != 0 {
		t.Error("a truncated header parsed")
	}
}

// rootOf picks the device's root qdisc out of a dump of every qdisc: the
// fallback for a kernel that answers a GET of one qdisc with nothing.
func TestRootOf(t *testing.T) {
	msg := func(ifindex int, parent uint32, kind string) nlMsg {
		b := tcMsg(ifindex)
		nativeEndian.PutUint32(b[12:], parent)
		return nlMsg{typ: rtmNewQdisc, body: append(b, encodeAttr(tcaKind, cString(kind))...)}
	}
	dump := []nlMsg{
		msg(2, tcHRoot, "fq"),
		msg(9, 0xfffffff1, "clsact"),
		msg(9, tcHRoot, cakeKind),
		{typ: nlmsgDone},
	}
	if q, ok := rootOf(dump, 9); !ok || q.kind != cakeKind || q.ifindex != 9 {
		t.Errorf("rootOf(9) = %+v, %v; want the cake root, not the clsact", q, ok)
	}
	if _, ok := rootOf(dump, 4); ok {
		t.Error("a device the dump does not name has a root qdisc")
	}
}
