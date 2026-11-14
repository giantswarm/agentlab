//go:build linux

package shaper

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// reconcileEvery is how often the veth and its CAKE are checked and
	// repaired: a recreated mirror has a new veth.
	reconcileEvery = 10 * time.Second
	// logEvery bounds the back-off and problem lines in the shaper's log.
	logEvery = 30 * time.Second
	// publishEvery is how often the state file is written.
	publishEvery = time.Second
)

// daemon is one running shaper.
type daemon struct {
	cfg  Config
	log  *log.Logger
	nl   *rtnl
	png  *pinger
	ctl  *controller
	refl []reflector

	mode    Mode
	modeSig string
	net     netID

	dev     netLink // the mirror's veth; index 0 while none is found
	noCake  bool    // the kernel refused CAKE on it
	last    qdisc   // CAKE's counters at the last tick
	lastAt  time.Time
	applied float64 // CAKE's bandwidth: 0 unlimited, -1 unknown
	rate    float64 // the cap this tick asked for
	problem string

	ticks       int
	reconcileAt int // the tick that reconciles next
	cuts        int // back-offs since the last log line
	cutLog      time.Time
	probLog     time.Time

	pubBytes  uint64 // the counters at the last publish
	pubAt     time.Time
	mirrorBps float64
	published time.Time
	pubMode   string
}

// Run is the shaper: it finds the mirror's veth, puts CAKE on it and sets
// its bandwidth every tick until ctx ends, then takes CAKE off again — the
// veth back to what docker made it, the mirror unlimited.
func Run(ctx context.Context, cfg Config, logger *log.Logger) error {
	if err := cfg.Check(); err != nil {
		return err
	}
	nl, err := dialRoute()
	if err != nil {
		return err
	}
	defer nl.close()
	png, err := newPinger(cfg.Reflectors)
	if err != nil {
		return err
	}
	d := &daemon{
		cfg: cfg, log: logger, nl: nl, png: png,
		ctl:     newController(cfg.Min, cfg.Max, cfg.Start, cfg.Target),
		refl:    make([]reflector, len(cfg.Reflectors)),
		mode:    Mode{Kind: ModeAuto},
		applied: -1,
	}
	logger.Printf("shaping the registry mirror's downloads: auto keeps them between %s and %s, backing off at +%s over the latency baseline",
		FormatRate(cfg.Min), FormatRate(cfg.Max), cfg.Target)
	go png.receive()
	go png.send(ctx, cfg.Interval)

	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		d.tick(time.Now())
		select {
		case <-ctx.Done():
			png.close()
			d.teardown()
			return nil
		case <-t.C:
		}
	}
}

// Cleanup takes CAKE off the veth of the mirror whose process is pid: what
// a shaper that was killed left behind.
func Cleanup(pid int, logger *log.Logger) error {
	nl, err := dialRoute()
	if err != nil {
		return err
	}
	defer nl.close()
	dev, err := nl.mirrorVeth(pid)
	if err != nil {
		return err
	}
	return removeCake(nl, dev, logger)
}

// removeCake takes CAKE off a veth that has it.
func removeCake(nl *rtnl, dev netLink, logger *log.Logger) error {
	q, err := nl.rootQdisc(dev.index)
	if err != nil {
		return err
	}
	if q.kind != cakeKind {
		return nil
	}
	if err := nl.deleteRoot(dev.index); err != nil {
		return fmt.Errorf("taking CAKE off %s: %w", dev.name, err)
	}
	logger.Printf("took CAKE off %s: the mirror's downloads are not limited", dev.name)
	return nil
}

func (d *daemon) tick(now time.Time) {
	if d.ticks >= d.reconcileAt {
		d.reconcile(now)
		d.reconcileAt = d.ticks + max(int(reconcileEvery/d.cfg.Interval), 1)
	}
	d.ticks++

	if id := readNetID(d.net); !id.same(d.net) || d.ticks == 1 {
		d.log.Printf("network: %s; the latency baselines and the mirror's cap start over", id)
		d.net, d.refl = id, make([]reflector, len(d.cfg.Reflectors))
		d.ctl.reset()
	} else {
		d.net = id // the gateway's MAC may only now be in the ARP cache
	}
	d.readMode()
	got, limited := d.mirrorRate(now)
	d.ctl.observe(linkDelay(now, d.refl, d.png.collect(now)))

	var reason string
	switch d.mode.Kind {
	case ModeOff:
		d.rate, reason = 0, "off: the mirror is not limited (`agentlab shaper auto` shapes it again)"
	case ModePause:
		d.rate, reason = d.cfg.Pause, "paused (`agentlab shaper auto` lets it follow the link again)"
	default:
		if !hasSignal(now, d.refl) {
			// No reflector answered on this network: a network that drops
			// ICMP gives the controller nothing to steer by, and the mirror
			// keeps what it had without the shaper.
			d.ctl.reset()
			d.rate, reason = d.mode.Max, "no latency probe answers on this network (ICMP dropped?), so the mirror is not limited"
			if d.mode.Max > 0 {
				reason = "no latency probe answers on this network (ICMP dropped?), so the mirror is held at its max"
			}
			break
		}
		reason = d.ctl.step(now, got, limited, d.mode.Max)
		d.rate = d.ctl.rate
		if d.ctl.cutAt.Equal(now) {
			d.logCut(now, got)
		}
	}
	d.apply()
	d.publish(now, reason)
}

// reconcile finds the mirror's veth and has CAKE on it.
func (d *daemon) reconcile(now time.Time) {
	dev, err := d.nl.mirrorVeth(d.cfg.MirrorPID)
	if err != nil {
		d.dev = netLink{}
		d.report(now, "finding the mirror's veth: "+err.Error())
		return
	}
	if dev.index != d.dev.index {
		d.log.Printf("the mirror's downloads arrive on %s", dev.name)
		d.lastAt, d.applied, d.noCake = time.Time{}, -1, false
	}
	d.dev = dev
	q, err := d.nl.rootQdisc(dev.index)
	if err != nil {
		d.report(now, fmt.Sprintf("reading the qdisc of %s: %v", dev.name, err))
		return
	}
	if q.kind != cakeKind {
		if err := d.nl.replaceCake(dev.index, d.rate); err != nil {
			d.noCake = true
			d.applied = -1
			d.report(now, cakeProblem(dev.name, err))
			return
		}
		d.noCake, d.applied, d.lastAt = false, d.rate, time.Time{}
		d.log.Printf("put CAKE on %s (was %s) at %s", dev.name, q.kind, FormatRate(d.rate))
	}
	d.report(now, "")
}

// cakeProblem words why the kernel refused CAKE on the veth.
func cakeProblem(dev string, err error) string {
	switch {
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.EOPNOTSUPP):
		return fmt.Sprintf("the kernel has no CAKE qdisc (sch_cake), so the mirror's downloads on %s are not shaped (%v)", dev, err)
	case errors.Is(err, unix.EPERM):
		return fmt.Sprintf("no CAP_NET_ADMIN in the host's network namespace, so the mirror's downloads on %s are not shaped (%v)", dev, err)
	default:
		return fmt.Sprintf("putting CAKE on %s: %v", dev, err)
	}
}

// report records the problem that keeps the shaper from shaping ("" for
// none), logging a new one at once and a lasting one at most every logEvery.
func (d *daemon) report(now time.Time, problem string) {
	changed := problem != d.problem
	d.problem = problem
	if problem == "" {
		return
	}
	if changed || now.Sub(d.probLog) > logEvery {
		d.log.Printf("problem: %s", problem)
		d.probLog = now
	}
}

// readMode takes the mode the person left, whenever the file changed.
func (d *daemon) readMode() {
	if d.cfg.StateDir == "" {
		return
	}
	file := filepath.Join(d.cfg.StateDir, ModeFile)
	sig := "absent"
	if fi, err := os.Lstat(file); err == nil {
		sig = fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
	}
	if sig == d.modeSig {
		return
	}
	d.modeSig = sig
	m, err := readModeFile(file)
	if err != nil {
		d.log.Printf("the mode file %s: %v; staying on auto", file, err)
	}
	if m != d.mode {
		d.log.Printf("mode: %s", m)
		if m.Kind == ModeAuto && d.mode.Kind != ModeAuto {
			d.ctl.reset()
		}
		d.mode = m
	}
}

// mirrorRate is the mirror's throughput since the last tick and whether CAKE
// dropped or marked any of its packets meanwhile.
func (d *daemon) mirrorRate(now time.Time) (float64, bool) {
	if d.dev.index == 0 || d.noCake {
		d.lastAt = time.Time{}
		return 0, false
	}
	q, err := d.nl.rootQdisc(d.dev.index)
	if err != nil || q.kind != cakeKind {
		d.lastAt = time.Time{}
		d.reconcileAt = d.ticks // CAKE is gone: put it back on the next tick
		return 0, false
	}
	prev, prevAt := d.last, d.lastAt
	d.last, d.lastAt = q, now
	dt := now.Sub(prevAt).Seconds()
	// A recreated qdisc starts its counters over.
	if prevAt.IsZero() || dt <= 0 || q.bytes < prev.bytes || q.dropped < prev.dropped {
		return 0, false
	}
	return float64(q.bytes-prev.bytes) * 8 / dt, q.dropped > prev.dropped
}

// apply sets CAKE's bandwidth to the tick's cap, unless it is within 1 % of
// what is set.
func (d *daemon) apply() {
	if d.dev.index == 0 || d.noCake {
		return
	}
	rate := d.rate
	if d.applied >= 0 && (rate == d.applied || rate > 0 && d.applied > 0 && math.Abs(rate-d.applied) < 0.01*d.applied) {
		return
	}
	if err := d.nl.changeCake(d.dev.index, rate); err != nil {
		d.applied = -1
		d.reconcileAt = d.ticks // the qdisc may be gone: reconcile on the next tick
		return
	}
	d.applied = rate
}

func (d *daemon) logCut(now time.Time, got float64) {
	d.cuts++
	if now.Sub(d.cutLog) < logEvery {
		return
	}
	queue, _ := d.ctl.Queue()
	d.log.Printf("the mirror backs off: link queue at +%d ms with the mirror at %s, now capped at %s (%d back-offs since the last line)",
		queue.Milliseconds(), formatThroughput(got), FormatRate(d.ctl.rate), d.cuts)
	d.cuts, d.cutLog = 0, now
}

// publish writes the state every publishEvery, and at once when the mode
// changed (`agentlab shaper pause` waits for it).
func (d *daemon) publish(now time.Time, reason string) {
	if d.cfg.StateDir == "" {
		return
	}
	mode := d.mode.String()
	if now.Sub(d.published) < publishEvery && mode == d.pubMode {
		return
	}
	if !d.lastAt.IsZero() && !d.pubAt.IsZero() && d.last.bytes >= d.pubBytes && d.lastAt.After(d.pubAt) {
		d.mirrorBps = float64(d.last.bytes-d.pubBytes) * 8 / d.lastAt.Sub(d.pubAt).Seconds()
	} else if d.lastAt.IsZero() {
		d.mirrorBps = 0
	}
	d.pubBytes, d.pubAt = d.last.bytes, d.lastAt
	delay, known := d.ctl.Queue()
	st := State{
		Mode: mode, Device: d.dev.name, Shaping: d.dev.index != 0 && !d.noCake && d.problem == "",
		CapBps: d.rate, MirrorBps: d.mirrorBps,
		DelayMs: float64(delay.Microseconds()) / 1000, DelayKnown: known,
		TargetMs: float64(d.cfg.Target.Microseconds()) / 1000, Reason: reason,
		Network: d.net.String(), Problem: d.problem, Updated: now,
	}
	for i, r := range d.refl {
		rs := ReflectorState{Addr: d.cfg.Reflectors[i].String(), Lost: r.lost}
		if base, ok := r.base.value(now); ok {
			rs.BaseMs = float64(base.Microseconds()) / 1000
		}
		rs.LastMs = float64(r.last.Microseconds()) / 1000
		st.Reflectors = append(st.Reflectors, rs)
	}
	if err := writeState(d.cfg.StateDir, st); err != nil && now.Sub(d.probLog) > logEvery {
		d.log.Printf("problem: publishing the state: %v", err)
		d.probLog = now
	}
	d.published, d.pubMode = now, mode
}

// teardown takes CAKE off the veth and withdraws the state.
func (d *daemon) teardown() {
	if d.dev.index != 0 {
		if err := removeCake(d.nl, d.dev, d.log); err != nil {
			d.log.Printf("problem: %v", err)
		}
	}
	if d.cfg.StateDir != "" {
		_ = os.Remove(filepath.Join(d.cfg.StateDir, StateFile))
	}
	d.log.Printf("stopped: the mirror's downloads are not limited")
}
