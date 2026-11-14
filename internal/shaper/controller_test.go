package shaper

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestBaselineAgesOut(t *testing.T) {
	var b baseline
	t0 := time.Unix(1_800_000_000, 0)
	b.observe(t0, ms(40))
	b.observe(t0.Add(time.Second), ms(20))
	b.observe(t0.Add(2*time.Minute), ms(30))
	if v, ok := b.value(t0.Add(3 * time.Minute)); !ok || v != ms(20) {
		t.Errorf("baseline = %v, %v; want the 20ms minimum", v, ok)
	}
	// Five minutes on, the 20 ms bucket is gone and the later one counts.
	if v, ok := b.value(t0.Add(5*time.Minute + bucketLen)); !ok || v != ms(30) {
		t.Errorf("baseline after the window = %v, %v; want 30ms", v, ok)
	}
	if _, ok := b.value(t0.Add(time.Hour)); ok {
		t.Error("an hour later the baseline should be empty")
	}
}

func TestLinkDelay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	refl := make([]reflector, 4)
	if hasSignal(now, refl) {
		t.Error("no reflector answered yet, but a signal")
	}
	// Baselines 10, 20, 30, 40 ms.
	linkDelay(now, refl, []probeAcc{{ms(10), 1, 0}, {ms(20), 1, 0}, {ms(30), 1, 0}, {ms(40), 1, 0}})
	if !hasSignal(now, refl) {
		t.Error("every reflector answered, but no signal")
	}

	// Deltas 5, 10, 1, 160: a queue would lift them all, so the lowest counts.
	d, ok := linkDelay(now, refl, []probeAcc{{ms(15), 1, 0}, {ms(30), 2, 0}, {ms(31), 1, 0}, {ms(200), 1, 0}})
	if !ok || d != ms(1) {
		t.Errorf("delay = %v, %v; want 1ms", d, ok)
	}
	// Lost probes don't outvote a reflector that answered at its baseline...
	d, ok = linkDelay(now, refl, []probeAcc{{0, 0, 1}, {0, 0, 1}, {ms(30), 1, 0}, {0, 0, 0}})
	if !ok || d != 0 {
		t.Errorf("delay with two losses = %v, %v; want 0", d, ok)
	}
	if refl[0].lost != 1 || refl[2].lost != 0 {
		t.Errorf("lost counters = %d, %d", refl[0].lost, refl[2].lost)
	}
	// ...but when every reflector with a baseline loses its probe, that's a queue.
	if d, ok = linkDelay(now, refl, []probeAcc{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}, {0, 0, 1}}); !ok || d != lostDelay {
		t.Errorf("all lost = %v, %v; want %v", d, ok, lostDelay)
	}
	// A reflector that never answered on this network says nothing: a
	// network that drops ICMP leaves the shaper without a signal.
	fresh := make([]reflector, 2)
	if _, ok := linkDelay(now, fresh, []probeAcc{{0, 0, 3}, {0, 0, 0}}); ok {
		t.Error("lost probes without a baseline should leave the delay unknown")
	}
	if hasSignal(now, fresh) {
		t.Error("only lost probes, but a signal")
	}
	// The signal ages out with the baselines.
	if hasSignal(now.Add(time.Hour), refl) {
		t.Error("an hour without answers, but still a signal")
	}
}

func TestControllerSteps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newController(1e6, 1e9, 8e6, ms(30))
	// limited: CAKE dropped the mirror's packets, i.e. the shaper held it back.
	tick := func(delay time.Duration, got float64, limited bool) string {
		now = now.Add(500 * time.Millisecond)
		c.observe(delay, true)
		return c.step(now, got, limited, 0)
	}
	quiet := func() {
		for range cutWindow {
			tick(ms(2), c.rate, true)
		}
	}

	if tick(ms(2), 8e6, true); c.rate != 8e6 {
		t.Fatalf("first tick, no 1 s window yet: rate %v, want it held", c.rate)
	}
	if tick(ms(2), 8e6, true); c.rate != 8e6*probeFast {
		t.Fatalf("quiet link, CAKE holding the mirror back: rate %v, want %v", c.rate, 8e6*probeFast)
	}
	r := c.rate
	if tick(ms(2), 1e6, false); c.rate != r {
		t.Errorf("mirror below its cap: rate moved %v -> %v", r, c.rate)
	}

	// While CAKE holds the mirror back, one noisy tick only pauses the
	// probing, and it takes a second one to settle.
	quiet()
	r = c.rate
	if tick(ms(200), r, true); c.rate != r {
		t.Errorf("one spike: rate %v, want it held", c.rate)
	}
	if tick(ms(2), r, true); c.rate != r*probeFast {
		t.Errorf("quiet again: rate %v, want a probe to %v", c.rate, r*probeFast)
	}
	r = c.rate
	tick(ms(20), r, true)
	if tick(ms(20), r, true); c.rate != r*settleFactor {
		t.Errorf("a second above half the target: rate %v, want a settle to %v", c.rate, r*settleFactor)
	}

	// When the link, not CAKE, held the mirror back, one tick is enough, and
	// it marks the link's capacity.
	quiet()
	r = c.rate
	if tick(ms(20), 0.95*r, false); c.rate != 0.95*r*settleFactor || c.ceiling != 0.95*r {
		t.Errorf("link-bound tick above half the target: rate %v ceiling %v, want %v and %v", c.rate, c.ceiling, 0.95*r*settleFactor, 0.95*r)
	}

	// It takes two seconds above the target to cut.
	quiet()
	for i := range 3 {
		tick(ms(40), c.rate, true)
		if c.cutAt.Equal(now) {
			t.Fatalf("cut after %d ticks above the target", i+1)
		}
	}
	r = c.rate
	if tick(ms(40), r, true); c.rate != r*cutFactor {
		t.Errorf("two seconds above target: rate %v, want a cut to %v", c.rate, r*cutFactor)
	}
	r = c.rate
	if tick(ms(40), r, true); c.rate != r {
		t.Errorf("cut again inside the refractory time: %v -> %v", r, c.rate)
	}

	// A deep queue halves an active mirror from what it got (a link-bound
	// mirror settles on the first deep tick, the second one is the cut)...
	c = newController(1e6, 1e9, 8e6, ms(30))
	quiet()
	c.rate = 40e6
	tick(ms(2), 20e6, false)
	if tick(time.Second, 20e6, false); c.rate != 20e6*settleFactor {
		t.Errorf("first deep tick, mirror at 20 of 40: rate %v, want a settle to %v", c.rate, 20e6*settleFactor)
	}
	if tick(time.Second, 20e6, false); c.rate != 20e6*settleFactor*severeCut {
		t.Errorf("second deep tick: rate %v, want %v", c.rate, 20e6*settleFactor*severeCut)
	}
	// ...and an idle mirror's unused cap only by half, so its next pull
	// doesn't start from the floor.
	c = newController(1e6, 1e9, 8e6, ms(30))
	c.rate = 40e6
	for range cutWindow {
		tick(ms(2), 0, false)
	}
	tick(time.Second, 0, false)
	if tick(time.Second, 0, false); c.rate != 40e6*severeCut {
		t.Errorf("deep queue, idle mirror: rate %v, want %v", c.rate, 40e6*severeCut)
	}

	// No answers holds; a mode cap clamps.
	c.rate = 50e6
	c.observe(0, false)
	if c.step(now, 50e6, true, 0); c.rate != 50e6 {
		t.Errorf("unknown delay moved the rate to %v", c.rate)
	}
	if c.step(now, 50e6, true, 20e6); c.rate != 20e6 {
		t.Errorf("auto 20mbit: rate %v", c.rate)
	}

	// Probing is slow only around the last congestion point.
	c = newController(1e6, 1e9, 8e6, ms(30))
	c.observe(0, true)
	c.observe(0, true)
	c.ceiling, c.ceilingAt = 10e6, now
	for rate, want := range map[float64]float64{5e6: probeFast, 9.5e6: probeSlow, 10.5e6: probeSlow, 12e6: probeFast} {
		c.rate = rate
		c.step(now, rate, true, 0)
		if got := c.rate / rate; got != want {
			t.Errorf("probe factor at %v with the ceiling at 10M: %v, want %v", rate, got, want)
		}
	}
	// The ceiling is forgotten after a minute.
	c.rate = 9.5e6
	c.step(now.Add(ceilingMemory+time.Second), 9.5e6, true, 0)
	if c.ceiling != 0 || c.rate != 9.5e6*probeFast {
		t.Errorf("a stale ceiling still slows the probing: ceiling %v, rate %v", c.ceiling, c.rate)
	}
}

func TestControllerLimitedAtLowRates(t *testing.T) {
	// At low rates CAKE's ingress mode counts its drops as sent, so TCP gets
	// well below the cap: the drops alone say the mirror wants more.
	c := newController(256e3, 1e9, 460e3, 30*time.Millisecond)
	c.observe(0, true)
	c.observe(0, true)
	c.step(time.Unix(1_800_000_000, 0), 0.3e6, false, 0)
	if c.rate != 460e3 {
		t.Errorf("no drops, mirror at 65%% of the cap: rate %v, want it held", c.rate)
	}
	c.step(time.Unix(1_800_000_001, 0), 0.3e6, true, 0)
	if c.rate != 460e3*probeFast {
		t.Errorf("CAKE dropping, quiet link: rate %v, want %v", c.rate, 460e3*probeFast)
	}
}

// simLink is a fluid model of a bottleneck with a deep FIFO, a train's LTE
// or a cable modem: a queue fills when what arrives exceeds the capacity,
// and the delay is the queue over the capacity. The mirror sends what the
// shaper lets through; the person either sends a fixed rate (a call,
// browsing) or is greedy (a big download that keeps the queue at
// greedyQueue).
type simLink struct {
	capacity, queue, buffer float64 // bit/s, bit, bit
	person                  float64 // bit/s; < 0: greedy
	greedyQueue             float64 // seconds of queue a greedy person keeps
}

// run advances dt with the mirror offering mirrorRate and returns what the
// mirror and the person got and the delay at the end.
func (l *simLink) run(dt time.Duration, mirrorRate float64) (mirror, person, delay float64) {
	const sub = 10 * time.Millisecond
	steps := int(dt / sub)
	for range steps {
		s := sub.Seconds()
		u := l.person
		if u < 0 { // greedy: fill the link and keep its queue
			u = max(l.capacity-mirrorRate, 0) + (l.greedyQueue*l.capacity-l.queue)/s
			u = max(u, 0)
		}
		in := mirrorRate + u
		out := l.capacity
		if l.queue <= 0 && in < l.capacity {
			out = in
		}
		share := 1.0
		if in > 0 {
			share = mirrorRate / in
		}
		mirror += out * share * s
		person += out * (1 - share) * s
		l.queue = min(max(l.queue+(in-out)*s, 0), l.buffer)
	}
	return mirror / dt.Seconds(), person / dt.Seconds(), l.queue / l.capacity
}

// contendedWiFi is the lowest per-tick delay over four reflectors above
// their baselines, in ms, recorded on a laptop's Wi-Fi at a busy home
// network with the lab's downloads held at their floor: contention noise,
// no queue of the lab's. A 60 Mbit/s DSL behind it, which the unshaped lab
// bloated by +226 ms.
var contendedWiFi = []float64{
	59.2, 26.1, 7.3, 124.4, 22.8, 44.1, 0.6, 0.5, 2.0, 2.3, 10.4, 0.3, 36.1, 0.2, 37.1, 13.5, 26.0,
	86.2, 26.9, 48.2, 22.1, 30.1, 16.7, 3.3, 2.7, 14.7, 0.3, 3.9, 0.1, 15.5, 27.0, 1.1, 12.1, 0.0,
	0.3, 0.5, 14.2, 0.2, 31.0, 32.2, 11.8, 12.8, 74.7, 27.9, 5.3, 8.6, 21.2, 32.3, 0.3, 13.9, 1.1,
	30.8, 0.2, 50.5, 14.7, 7.9, 36.9, 14.6, 15.2, 5.8, 27.8, 58.5, 4.6, 12.0, 3.7, 0.0, 0.8, 12.7,
	4.0, 4.7, 16.5, 23.2, 0.4, 0.0, 0.5, 10.6, 28.4, 19.5, 0.6, 37.8, 0.0, 47.1, 13.7, 1.0, 24.1,
	30.0, 19.2, 27.4, 22.6, 15.9, 0.7, 0.3, 7.4, 0.9, 9.2, 15.2, 0.0, 52.1, 43.9, 56.2, 66.5, 56.8,
	31.1, 14.0, 20.6, 30.3, 22.3, 41.7, 1.2, 3.7, 0.4, 7.5, 5.7, 21.2, 38.6, 51.1, 12.1, 0.9, 10.8,
	5.1, 21.8, 62.7, 25.9, 30.7, 1.0, 49.2, 53.1, 16.0, 9.2, 2.1, 3.9, 19.6, 16.5, 3.1, 21.9, 4.7,
	0.2, 14.7, 0.5, 8.6, 22.9, 24.4, 44.4, 38.9, 59.3, 16.7, 37.2, 2.0, 6.2, 136.1, 0.1, 7.7, 0.7,
	1.2, 3.4, 121.1,
}

// The controller against the fluid model: it fills an idle link, leaves a
// person's traffic alone, is down to its floor within seconds of the
// person's own big download and has the link back soon after. The recorded
// contended Wi-Fi is the regression test of the lowest-over-a-window
// signal: a median read that noise as a full link and held the downloads at
// their floor.
func TestControllerOnASimulatedLink(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity float64
		noise    []float64
		mirror   float64       // the share of an idle link the mirror must get
		recover  time.Duration // how soon after the person's download it must get it again
	}{
		{"train", 3e6, nil, 0.8, 30 * time.Second},
		{"dsl", 20e6, nil, 0.8, 30 * time.Second},
		{"fiber", 300e6, nil, 0.8, 30 * time.Second},
		// Replayed noise is pessimistic: it was recorded with some of the
		// person's own traffic on the link, and lands on top of the model's.
		{"contended-wifi", 60e6, contendedWiFi, 0.6, 75 * time.Second},
		{"contended-wifi-train", 3e6, contendedWiFi, 0.6, 75 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) { simulate(t, tc.capacity, tc.noise, tc.mirror, tc.recover) })
	}
}

// simulate runs the controller against simLink, adding noise (ms per tick,
// cycled) to the delay it sees but not to the queue it is judged by.
func simulate(t *testing.T, capacity float64, noise []float64, mirrorShare float64, recoverIn time.Duration) {
	const floor = 256e3
	target := 30 * time.Millisecond
	link := &simLink{capacity: capacity, buffer: capacity * 0.5} // 500 ms of buffer
	c := newController(floor, 2.5e9, 8e6, target)
	now := time.Unix(1_800_000_000, 0)
	tickLen := 500 * time.Millisecond
	mbit := func(v float64) float64 { return v / 1e6 }
	n := 0
	tick := func() (mirror, person, delay float64) {
		rate := c.rate
		mirror, person, delay = link.run(tickLen, rate)
		now = now.Add(tickLen)
		seen := delay
		if len(noise) > 0 {
			seen += noise[n%len(noise)] / 1000
		}
		n++
		c.observe(time.Duration(seen*float64(time.Second)), true)
		// The mirror always wants more; CAKE drops when it is what holds
		// the mirror back, i.e. when the link gave the mirror all of its cap.
		c.step(now, mirror, mirror >= 0.999*rate, 0)
		return mirror, person, delay
	}

	type phase struct{ mirror, person, delay, hot, ticks float64 }
	run := func(seconds int) phase {
		var p phase
		for range seconds * 2 {
			mirror, person, delay := tick()
			p.mirror, p.person, p.delay, p.ticks = p.mirror+mirror, p.person+person, p.delay+delay, p.ticks+1
			if delay > target.Seconds() {
				p.hot++
			}
		}
		p.mirror, p.person, p.delay, p.hot = p.mirror/p.ticks, p.person/p.ticks, 1000*p.delay/p.ticks, 100*p.hot/p.ticks
		return p
	}

	run(30) // find the link
	alone := run(60)
	t.Logf("mirror alone: %.2f of %.0f Mbit/s, queue %.0f ms on average, above target %.0f%% of the time",
		mbit(alone.mirror), mbit(capacity), alone.delay, alone.hot)
	if alone.mirror < mirrorShare*capacity {
		t.Errorf("the mirror alone got %.2f Mbit/s of %.0f; it should use most of an idle link", mbit(alone.mirror), mbit(capacity))
	}
	if alone.hot > 10 {
		t.Errorf("the mirror alone kept the queue above target %.0f%% of the time", alone.hot)
	}

	link.person = 0.75 * capacity // a call and some browsing
	run(5)
	shared := run(60)
	t.Logf("person asks %.2f: gets %.2f, mirror %.2f, queue %.0f ms on average, above target %.0f%% of the time",
		mbit(link.person), mbit(shared.person), mbit(shared.mirror), shared.delay, shared.hot)
	if shared.person < 0.95*link.person {
		t.Errorf("the person got %.2f Mbit/s of the %.2f they asked for", mbit(shared.person), mbit(link.person))
	}
	if shared.hot > 15 {
		t.Errorf("with the person on the link the queue sat above target %.0f%% of the time", shared.hot)
	}

	link.person, link.greedyQueue = -1, 0.1 // a big download of the person's own
	var yielded time.Duration
	for yielded = 0; c.rate > floor && yielded < time.Minute; yielded += tickLen {
		tick()
	}
	greedy := run(30)
	t.Logf("person's own download: the mirror is at its floor after %s; then person %.2f, mirror %.2f", yielded, mbit(greedy.person), mbit(greedy.mirror))
	if yielded > 10*time.Second {
		t.Errorf("the mirror took %s to yield to the person's download", yielded)
	}
	if greedy.mirror > 1.2*floor {
		t.Errorf("the mirror took %.2f Mbit/s from the person's download; it should sit at its floor", mbit(greedy.mirror))
	}

	link.person = 0 // the person is done: the mirror takes the link back
	var back time.Duration
	for back = 5 * time.Second; run(5).mirror < mirrorShare*capacity && back <= 2*time.Minute; back += 5 * time.Second {
	}
	t.Logf("person gone: the mirror has %.0f%% of the link again after %s", 100*mirrorShare, back)
	if back > recoverIn {
		t.Errorf("after the person left the mirror took %s to get %.0f%% of the link again, want %s", back, 100*mirrorShare, recoverIn)
	}
}
