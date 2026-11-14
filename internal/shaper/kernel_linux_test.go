//go:build linux

package shaper

import (
	"context"
	"errors"
	"io"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// inNamespaceEnv marks the test binary re-run inside its own user and
// network namespace.
const inNamespaceEnv = "AGENTLAB_SHAPER_TEST_IN_NAMESPACE"

// What a veth pair is made with (linux/netlink.h, linux/if_link.h,
// linux/veth.h); the shaper itself never makes one.
const (
	nlmFExcl     = 0x200
	iflaNetNsPid = 19
	iflaInfoData = 2
	vethInfoPeer = 1
)

// addVeth makes the veth pair name/peer with the peer already in pid's network
// namespace. It goes over netlink, not ip: BusyBox's ip, the one CI images
// carry, ignores the peer's name.
func addVeth(nl *rtnl, name, peer string, pid int) error {
	ifname := func(n string) []byte { return encodeAttr(iflaIfname, append([]byte(n), 0)) }
	peerMsg := append(make([]byte, sizeofIfInfomsg), ifname(peer)...)
	peerMsg = append(peerMsg, encodeAttr(iflaNetNsPid, u32(uint32(pid)))...) // #nosec G115 -- a pid
	info := append(encodeAttr(iflaInfoKind, []byte(vethKind)), encodeAttr(iflaInfoData, encodeAttr(vethInfoPeer, peerMsg))...)
	body := append(make([]byte, sizeofIfInfomsg), ifname(name)...)
	body = append(body, encodeAttr(iflaLinkinfo, info)...)
	_, err := nl.do(rtmNewLink, nlmFCreate|nlmFExcl, body)
	return err
}

// The shaper's kernel side against the kernel: in an unprivileged user and
// network namespace (skipped where the kernel or a sandbox refuses one) a
// veth pair stands in for the mirror's, its peer in a third namespace held
// by a child process. mirrorVeth finds the host side by the child's pid,
// CAKE goes on with the shaper's settings, an echo through it counts, its
// bandwidth changes without losing the settings, and it comes off again —
// by the daemon's teardown and by Cleanup.
func TestKernelSideInANamespace(t *testing.T) {
	if os.Getenv(inNamespaceEnv) != "1" {
		if _, err := exec.LookPath("ip"); err != nil {
			t.Skip("no ip to address the veth pair with")
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestKernelSideInANamespace$", "-test.v") // #nosec G204 G702 -- the test binary itself
		cmd.Env = append(os.Environ(), inNamespaceEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Skipf("no unprivileged user namespace here: %v", err)
		}
		if strings.Contains(string(out), "--- SKIP") {
			t.Skipf("inside the namespace:\n%s", out)
		}
		if err != nil {
			t.Fatalf("inside the namespace: %v\n%s", err, out)
		}
		t.Logf("inside the namespace:\n%s", out)
		return
	}

	ip := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil { // #nosec G204 -- fixed test arguments
			t.Fatalf("ip %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	// The mirror: a process in a network namespace of its own, which brings
	// up the veth's end once it arrives there.
	holder := exec.Command("sh", "-c", `ip link set lo up
while ! ip addr add 10.99.0.2/24 dev eth0 2>/dev/null; do sleep 0.05; done
ip link set eth0 up && exec sleep 60`)
	holder.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
	if err := holder.Start(); err != nil {
		t.Fatalf("starting the mirror's stand-in: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	pid := holder.Process.Pid

	nl, err := dialRoute()
	if err != nil {
		t.Fatal(err)
	}
	defer nl.close()
	if err := addVeth(nl, "vmirror", "eth0", pid); err != nil {
		t.Fatalf("making the veth pair: %v", err)
	}
	ip("addr", "add", "10.99.0.1/24", "dev", "vmirror")
	ip("link", "set", "vmirror", "up")
	if _, err := nl.mirrorVeth(os.Getpid()); err == nil {
		t.Error("a process of this namespace has no veth leading to it, but one was found")
	}
	dev, err := nl.mirrorVeth(pid)
	if err != nil || dev.name != "vmirror" {
		t.Fatalf("mirrorVeth = %+v, %v; want vmirror", dev, err)
	}
	if q, err := nl.rootQdisc(dev.index); err != nil || q.kind == cakeKind {
		t.Fatalf("before: %+v, %v", q, err)
	}

	if err := nl.replaceCake(dev.index, 20e6); err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EOPNOTSUPP) {
			t.Skipf("the kernel has no sch_cake: %v", err)
		}
		t.Fatalf("replaceCake: %v", err)
	}
	q, err := nl.rootQdisc(dev.index)
	if err != nil || q.kind != cakeKind || q.rate != 20e6 {
		t.Fatalf("after replace: %+v, %v; want cake at 20 Mbit/s", q, err)
	}
	tcShows := func(want ...string) {
		t.Helper()
		if _, err := exec.LookPath("tc"); err != nil {
			return
		}
		out, err := exec.Command("tc", "qdisc", "show", "dev", "vmirror", "root").CombinedOutput()
		if err != nil {
			t.Fatalf("tc: %v: %s", err, out)
		}
		for _, w := range want {
			if !strings.Contains(string(out), w) {
				t.Errorf("tc shows %q, want %q in it", strings.TrimSpace(string(out)), w)
			}
		}
	}
	tcShows("bandwidth 20Mbit", "besteffort", "dual-dsthost", " nat ", "ingress")

	// Echoes to the mirror's side leave through CAKE, so its counters move,
	// and come back to the pinger once the stand-in has brought its end up.
	png, err := newPinger([]netip.Addr{netip.MustParseAddr("10.99.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	go png.receive()
	ctx, cancel := context.WithCancel(context.Background())
	go png.send(ctx, 100*time.Millisecond)
	answered := false
	for range 50 {
		time.Sleep(100 * time.Millisecond)
		if acc := png.collect(time.Now()); acc[0].answers > 0 {
			answered = true
			break
		}
	}
	cancel()
	png.close()
	if !answered {
		t.Error("no echo came back through CAKE")
	}
	if q, err := nl.rootQdisc(dev.index); err != nil || q.bytes == 0 {
		t.Errorf("after the echoes: %+v, %v; want bytes counted", q, err)
	}

	if err := nl.changeCake(dev.index, 5e6); err != nil {
		t.Fatalf("changeCake: %v", err)
	}
	if q, err := nl.rootQdisc(dev.index); err != nil || q.rate != 5e6 {
		t.Errorf("after change: %+v, %v; want 5 Mbit/s", q, err)
	}
	tcShows("bandwidth 5Mbit", "dual-dsthost", " nat ", "ingress")
	if err := nl.changeCake(dev.index, 0); err != nil {
		t.Fatalf("changeCake to unlimited: %v", err)
	}
	if q, err := nl.rootQdisc(dev.index); err != nil || q.kind != cakeKind || q.rate != 0 {
		t.Errorf("unlimited: %+v, %v", q, err)
	}
	tcShows("unlimited")

	quiet := log.New(io.Discard, "", 0)
	if err := removeCake(nl, dev, quiet); err != nil {
		t.Fatalf("removeCake: %v", err)
	}
	if q, err := nl.rootQdisc(dev.index); err != nil || q.kind == cakeKind {
		t.Errorf("after teardown: %+v, %v; want CAKE gone", q, err)
	}
	if err := removeCake(nl, dev, quiet); err != nil {
		t.Errorf("a second teardown: %v; want nothing to do", err)
	}
	// What a killed shaper left, Cleanup takes off.
	if err := nl.replaceCake(dev.index, 1e6); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(pid, quiet); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if q, err := nl.rootQdisc(dev.index); err != nil || q.kind == cakeKind {
		t.Errorf("after Cleanup: %+v, %v; want CAKE gone", q, err)
	}
}
