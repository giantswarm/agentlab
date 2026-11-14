//go:build linux

package shaper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The namespace proof's topology, in a user, network and mount namespace of
// its own (no root, nothing on the host touched):
//
//	internet ─ wan1 ══ 20 Mbit/s, 20 ms, ~420 ms FIFO ══ wan0 ─ host ─ br-lab ─ veth-mirror ═ eth0 ─ mirror
//
// The host namespace is the proof's own and plays the person's machine: the
// person's download and pings start there, and the shaper runs there as on a
// lab's host. The mirror namespace stands in for agentlab-mirror on the kind
// network, its four streams pulled by a process the shaper finds the veth by.
const (
	proofInternet   = "internet"
	proofMirror     = "mirror"
	proofServer     = "172.16.0.1"
	proofServerPort = "5201"
)

// proofReflectors are the shaper's reflectors, on the internet side.
var proofReflectors = []string{"198.18.0.1", "198.18.0.2", "198.18.0.3", "198.18.0.4"}

// Proof runs the namespace proof: it starts this agentlab again as root of a
// new user namespace with network and mount namespaces of its own, which
// builds the topology and runs it there.
func Proof(ctx context.Context, opts ProofOptions) error {
	for _, tool := range []string{"ip", "tc"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("the namespace proof builds its topology with %s (iproute2), which is not on PATH", tool)
		}
	}
	args := []string{"shaper-test", "--in-namespace"}
	if opts.Baseline {
		args = append(args, "--baseline")
	}
	cmd := exec.CommandContext(ctx, opts.Binary, args...) // #nosec G204 -- this agentlab itself
	cmd.Stdout, cmd.Stderr = opts.Out, opts.Out
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit):
		return ErrProofFailed
	default:
		return fmt.Errorf("starting the proof in a user and network namespace of its own (are unprivileged user namespaces off? sysctl kernel.unprivileged_userns_clone): %w", err)
	}
}

// ProofInNamespace is the proof inside its namespaces.
func ProofInNamespace(ctx context.Context, opts ProofOptions) error {
	w := opts.Out
	if err := proofTopology(); err != nil {
		return err
	}
	stopServer, err := proofServe(ctx, opts.Binary)
	if err != nil {
		return err
	}
	defer stopServer()
	base, err := proofBase(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "    a %s link with %s of delay and a FIFO of %d packets (~%s); the mirror pulls %d streams for %s, the person downloads one from second %d for %s and pings every %s; base round trip %s\n",
		FormatRate(proofLinkRate), proofLinkDelay, proofFIFO, time.Duration(float64(proofFIFO*1514*8)/proofLinkRate*float64(time.Second)).Round(10*time.Millisecond),
		proofMirrorRuns, proofDuration, int(proofPersonFrom.Seconds()), proofPersonFor, proofPing, base.Round(100*time.Microsecond))

	var runs []proofRun
	if opts.Baseline {
		_, _ = fmt.Fprintf(w, "==> without the shaper (%s)\n", proofDuration)
		r, err := proofOnce(ctx, w, opts.Binary, false)
		if err != nil {
			return err
		}
		runs = append(runs, r)
	}
	_, _ = fmt.Fprintf(w, "==> with the shaper (%s)\n", proofDuration)
	r, err := proofOnce(ctx, w, opts.Binary, true)
	if err != nil {
		return err
	}
	runs = append(runs, r)

	_, _ = fmt.Fprintf(w, "==> results (throughput of the %s link; latency while the mirror pulls alone, base %s)\n", FormatRate(proofLinkRate), base.Round(time.Millisecond))
	var verdict proofVerdict
	for _, r := range runs {
		verdict = judge(r, base)
		verdict.report(w, r.name)
	}
	if last := runs[len(runs)-1]; last.shaperLn != "" {
		_, _ = fmt.Fprintf(w, "    the shaper's last state: %s\n", last.shaperLn)
	}
	if len(verdict.failures) > 0 {
		for _, f := range verdict.failures {
			_, _ = fmt.Fprintf(w, "FAIL %s\n", f)
		}
		return ErrProofFailed
	}
	_, _ = fmt.Fprintf(w, "PASS the person's download got at least half the link while the mirror pulled, and their median latency stayed within +%s of the base\n", proofLatencyMargin)
	return nil
}

// proofTopologyScript is the topology, one ip or tc command a line: the
// namespaces (internet, mirror), the uplink whose internet side sends the
// downloads through netem's rate-limited FIFO (server, delay, rate, FIFO),
// and the kind network, a bridge with the mirror's veth on it.
const proofTopologyScript = `ip netns add %[1]s
ip netns add %[2]s
ip link set lo up
ip -n %[1]s link set lo up
ip -n %[2]s link set lo up
ip link add wan0 type veth peer name wan1 netns %[1]s
ip addr add 172.16.0.2/30 dev wan0
ip link set wan0 up
ip -n %[1]s addr add %[3]s/30 dev wan1
ip -n %[1]s link set wan1 up
tc -n %[1]s qdisc add dev wan1 root netem delay %[4]s rate %[5]s limit %[6]d
ip route add default via %[3]s
ip -n %[1]s route add 10.10.0.0/24 via 172.16.0.2
ip link add br-lab type bridge
ip addr add 10.10.0.1/24 dev br-lab
ip link set br-lab up
ip link add veth-mirror type veth peer name eth0 netns %[2]s
ip link set veth-mirror master br-lab up
ip -n %[2]s addr add 10.10.0.2/24 dev eth0
ip -n %[2]s link set eth0 up
ip -n %[2]s route add default via 10.10.0.1
`

// proofTopology builds the namespaces, the link and the bridge.
func proofTopology() error {
	// `ip netns` keeps its namespaces under /run/netns: a /run of the
	// proof's own mount namespace, so nothing lands on the host's.
	if err := unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mounting a /run of the proof's own: %w", err)
	}
	if err := os.MkdirAll("/run/netns", 0o755); err != nil { // #nosec G301 -- the proof's own tmpfs
		return err
	}
	script := fmt.Sprintf(proofTopologyScript, proofInternet, proofMirror, proofServer,
		strconv.Itoa(int(proofLinkDelay.Milliseconds()))+"ms", RateArg(proofLinkRate), proofFIFO)
	var steps [][]string
	for line := range strings.Lines(script) {
		if f := strings.Fields(line); len(f) > 0 {
			steps = append(steps, f)
		}
	}
	for _, r := range proofReflectors {
		steps = append(steps, []string{"ip", "-n", proofInternet, "addr", "add", r + "/32", "dev", "lo"})
	}
	for _, s := range steps {
		if out, err := exec.Command(s[0], s[1:]...).CombinedOutput(); err != nil { // #nosec G204 -- the proof's fixed topology
			return fmt.Errorf("%s: %w: %s", strings.Join(s, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644) // #nosec G306 -- a sysctl of the proof's own network namespace
}

// proofServe starts the bulk source in the internet namespace and waits for
// it to answer.
func proofServe(ctx context.Context, binary string) (func(), error) {
	addr := net.JoinHostPort(proofServer, proofServerPort)
	srv := exec.CommandContext(ctx, "ip", "netns", "exec", proofInternet, binary, "shaper-test", "--role", "serve", "--listen", "0.0.0.0:"+proofServerPort) // #nosec G204 -- this agentlab itself
	var log bytes.Buffer
	srv.Stdout, srv.Stderr = &log, &log
	if err := srv.Start(); err != nil {
		return nil, err
	}
	stop := func() { _ = srv.Process.Kill(); _ = srv.Wait() }
	for range 50 {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			_ = c.Close()
			return stop, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	return nil, fmt.Errorf("the bulk source in the internet namespace did not answer on %s: %s", addr, strings.TrimSpace(log.String()))
}

// proofBase is the idle round trip to the server: the lowest of ten pings.
func proofBase(ctx context.Context) (time.Duration, error) {
	pings, err := pingSeries(ctx, netip.MustParseAddr(proofServer), time.Now(), 10, 100*time.Millisecond)
	if err != nil {
		return 0, err
	}
	var rtts []time.Duration
	for _, p := range pings {
		if !p.lost {
			rtts = append(rtts, p.rtt)
		}
	}
	if len(rtts) == 0 {
		return 0, errors.New("no ping to the server came back on the idle link")
	}
	return slices.Min(rtts), nil
}

// proofOnce runs the timeline once, with or without the shaper.
func proofOnce(ctx context.Context, w io.Writer, binary string, shaped bool) (proofRun, error) {
	r := proofRun{name: "unshaped"}
	if shaped {
		r.name = "shaped"
	}
	start := time.Now().Add(3 * time.Second)
	at := strconv.FormatInt(start.UnixMilli(), 10)
	from := net.JoinHostPort(proofServer, proofServerPort)

	mirror := exec.CommandContext(ctx, "ip", "netns", "exec", proofMirror, binary, "shaper-test", "--role", "fetch", // #nosec G204 -- this agentlab itself
		"--from", from, "--streams", strconv.Itoa(proofMirrorRuns), "--start-at", at, "--for", proofDuration.String())
	mirrorSamples, err := startFetch(mirror)
	if err != nil {
		return r, fmt.Errorf("starting the mirror's pull: %w", err)
	}
	var shaperCmd *exec.Cmd
	var shaperLog bytes.Buffer
	stateDir := ""
	if shaped {
		if stateDir, err = os.MkdirTemp("/run", "shaper-"); err != nil {
			return r, err
		}
		shaperCmd = exec.CommandContext(ctx, binary, "shaper", "run", "--mirror-pid", strconv.Itoa(mirror.Process.Pid), // #nosec G204 -- this agentlab itself
			"--reflectors", strings.Join(proofReflectors, ","), "--state-dir", stateDir)
		shaperCmd.Stdout, shaperCmd.Stderr = &shaperLog, &shaperLog
		if err := shaperCmd.Start(); err != nil {
			return r, fmt.Errorf("starting the shaper: %w", err)
		}
	}
	person := exec.CommandContext(ctx, binary, "shaper-test", "--role", "fetch", // #nosec G204 -- this agentlab itself
		"--from", from, "--streams", "1", "--start-at", strconv.FormatInt(start.Add(proofPersonFrom).UnixMilli(), 10), "--for", proofPersonFor.String())
	personSamples, err := startFetch(person)
	if err != nil {
		return r, fmt.Errorf("starting the person's download: %w", err)
	}
	pings, err := pingSeries(ctx, netip.MustParseAddr(proofServer), start, int(proofDuration/proofPing), proofPing)
	if err != nil {
		return r, err
	}
	r.pings = pings
	r.mirror, r.person = <-mirrorSamples, <-personSamples
	_ = mirror.Wait()
	_ = person.Wait()
	if shaperCmd != nil {
		if st, err := ReadState(stateDir); err == nil {
			r.shaperLn = st.Summary()
		}
		_ = shaperCmd.Process.Signal(syscall.SIGTERM)
		_ = shaperCmd.Wait()
		for line := range strings.Lines(shaperLog.String()) {
			// The log's timestamps are the shaper's; its lines say what
			// it did when.
			_, _ = fmt.Fprintf(w, "    shaper: %s", line)
		}
	}
	return r, nil
}

// startFetch starts a fetcher and collects its samples until it exits.
func startFetch(cmd *exec.Cmd) (<-chan []Sample, error) {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan []Sample, 1)
	go func() {
		var samples []Sample
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var s Sample
			if json.Unmarshal(sc.Bytes(), &s) == nil {
				samples = append(samples, s)
			}
		}
		_, _ = io.Copy(io.Discard, out)
		done <- samples
	}()
	return done, nil
}

// pingSeries sends n echoes to target, interval apart from start, and
// returns each one's round trip, or lost when none came back within a
// second.
func pingSeries(ctx context.Context, target netip.Addr, start time.Time, n int, interval time.Duration) ([]rttSample, error) {
	p, err := newPinger([]netip.Addr{target})
	if err != nil {
		return nil, err
	}
	defer p.close()
	type sent struct{ at time.Time }
	var mu sync.Mutex
	sentAt := map[uint16]sent{}
	got := map[uint16]time.Duration{}
	go func() {
		buf := make([]byte, 1500)
		for {
			k, _, err := p.conn.ReadFrom(buf)
			if err != nil {
				return
			}
			now := time.Now()
			id, seq, ok := parseEchoReply(buf[:k])
			if !ok || id != p.id {
				continue
			}
			mu.Lock()
			if s, found := sentAt[seq]; found {
				got[seq] = now.Sub(s.at)
			}
			mu.Unlock()
		}
	}()
	dst := &net.IPAddr{IP: target.AsSlice()}
	for i := range n {
		at := start.Add(time.Duration(i) * interval)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Until(at)):
		}
		seq := uint16(i + 1) // #nosec G115 -- a few hundred pings
		mu.Lock()
		sentAt[seq] = sent{at: time.Now()}
		mu.Unlock()
		_, _ = p.conn.WriteTo(echoRequest(p.id, seq), dst)
	}
	time.Sleep(time.Second)
	mu.Lock()
	defer mu.Unlock()
	samples := make([]rttSample, n)
	for i := range n {
		seq := uint16(i + 1) // #nosec G115 -- a few hundred pings
		rtt, ok := got[seq]
		samples[i] = rttSample{at: time.Duration(i) * interval, rtt: rtt, lost: !ok || rtt > time.Second}
	}
	return samples, nil
}
