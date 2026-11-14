package shaper

import (
	"fmt"
	"slices"
	"time"
)

// The controller's tuning. A tick is one interval (500 ms).
const (
	cutFactor     = 0.75             // a back-off keeps this share of what the mirror got
	severeCut     = 0.5              // and this share when the queue is past severeFactor×target
	probeFast     = 1.15             // growth per tick while far below the last congestion point
	probeSlow     = 1.02             // growth per tick near it
	settleFactor  = 0.97             // what the mirror keeps of its throughput while the queue rises below the target
	busyShare     = 0.9              // at this share of its cap the shaper is what holds the mirror back
	activeShare   = 0.25             // below this share of its cap lately the mirror is idle
	cutRefractory = time.Second      // the queue needs time to drain before the next cut shows
	ceilingMemory = time.Minute      // how long a congestion point slows the probing near it
	severeFactor  = 3                // a queue this many times the target over the settle window cuts deeper and at once
	lostDelay     = time.Second      // the delay a lost probe counts as
	probeTimeout  = time.Second      // a probe without an answer after this is lost
	bucketLen     = 30 * time.Second // the baseline is the lowest RTT over buckets×bucketLen
	buckets       = 10
)

// baseline is the lowest RTT a reflector answered with over the last five
// minutes, kept in buckets so an old minimum ages out once the path changes.
type baseline struct {
	min   [buckets]time.Duration
	epoch [buckets]int64 // which bucketLen period the slot holds; 0 = empty
}

func (b *baseline) observe(now time.Time, rtt time.Duration) {
	e := now.UnixNano()/int64(bucketLen) + 1
	i := e % buckets
	if b.epoch[i] != e {
		b.epoch[i], b.min[i] = e, rtt
		return
	}
	b.min[i] = min(b.min[i], rtt)
}

func (b *baseline) value(now time.Time) (time.Duration, bool) {
	e := now.UnixNano()/int64(bucketLen) + 1
	var v time.Duration
	ok := false
	for i := range b.min {
		if b.epoch[i] == 0 || e-b.epoch[i] >= buckets {
			continue
		}
		if !ok || b.min[i] < v {
			v, ok = b.min[i], true
		}
	}
	return v, ok
}

// reflector is one host the latency is measured against.
type reflector struct {
	base baseline
	last time.Duration // last RTT, 0 = none on this network yet
	lost int           // probes lost in a row
}

// probeAcc is what the pinger collected for one reflector since the last tick.
type probeAcc struct {
	min     time.Duration // lowest RTT among the answers
	answers int
	lost    int
}

// linkDelay folds one tick's probe results into the reflectors and returns the
// tick's queueing delay: the lowest, over the reflectors that reported, of
// their RTT above baseline. A queue on the shared path raises every probe;
// Wi-Fi contention and one reflector's own trouble spike only some, so the
// lowest is the one that tells. A reflector that answered before on this
// network and now lost a probe reports lostDelay; one that never answered (it
// drops ICMP, or the network is down) reports nothing.
func linkDelay(now time.Time, refl []reflector, acc []probeAcc) (time.Duration, bool) {
	var lowest time.Duration
	known := false
	report := func(d time.Duration) {
		if !known || d < lowest {
			lowest, known = d, true
		}
	}
	for i := range refl {
		r, a := &refl[i], acc[i]
		switch {
		case a.answers > 0:
			r.base.observe(now, a.min)
			r.last, r.lost = a.min, 0
			base, _ := r.base.value(now)
			report(a.min - base)
		case a.lost > 0:
			r.lost += a.lost
			if _, ok := r.base.value(now); ok {
				report(lostDelay)
			}
		}
	}
	return lowest, known
}

// hasSignal says whether any reflector answered on this network within the
// baseline's window: without one the latency says nothing, and the shaper
// leaves the mirror unlimited rather than guess.
func hasSignal(now time.Time, refl []reflector) bool {
	for i := range refl {
		if _, ok := refl[i].base.value(now); ok {
			return true
		}
	}
	return false
}

// controller moves the mirror's cap. The mirror grows while the link shows
// no queue and the shaper is what holds it back, and is cut to a share of
// what it got as soon as the link's queue grows, whoever fills it: the
// person's traffic always wins, the mirror takes the rest.
//
// The queue is read as the lowest per-tick delay over a short window: a
// standing queue lifts every tick, a contended Wi-Fi only some. On a busy
// home Wi-Fi with the downloads at their floor, the per-tick delay was above
// +30 ms in 25 % of the ticks, the lowest over two seconds in 2 %.
type controller struct {
	min, max, start float64
	target          time.Duration

	rate      float64
	ceiling   float64         // the rate the mirror last congested the link at; 0 = none known
	ceilingAt time.Time       // when, so it is forgotten after ceilingMemory
	delays    []time.Duration // the last cutWindow ticks' delays, oldest first
	known     bool            // whether the last tick had a delay
	recent    [4]float64      // the mirror's throughput over the last ticks, newest first
	cutAt     time.Time       // the last back-off
}

const (
	settleWindow = 2 // ticks: the queue over the last second steers settling and severe cuts
	cutWindow    = 4 // ticks: the queue over the last two seconds steers cuts
)

func newController(minRate, maxRate, start float64, target time.Duration) *controller {
	c := &controller{min: minRate, max: maxRate, start: start, target: target}
	c.reset()
	return c
}

// reset starts over, for a new network or a return to auto.
func (c *controller) reset() {
	c.rate, c.ceiling, c.ceilingAt, c.delays, c.known, c.cutAt, c.recent = c.start, 0, time.Time{}, nil, false, time.Time{}, [4]float64{}
}

// queue is the lowest delay over the last n ticks; false until there are n.
func (c *controller) queue(n int) (time.Duration, bool) {
	if len(c.delays) < n {
		return 0, false
	}
	return slices.Min(c.delays[len(c.delays)-n:]), true
}

// Queue is the link's queue as the controller sees it, for the status: over
// the cut window, or over what there is so far.
func (c *controller) Queue() (time.Duration, bool) {
	if q, ok := c.queue(cutWindow); ok {
		return q, true
	}
	if len(c.delays) == 0 {
		return 0, false
	}
	return slices.Min(c.delays), true
}

// observe records one tick's delay (known=false: no reflector reported).
func (c *controller) observe(delay time.Duration, known bool) {
	c.known = known
	if !known {
		return
	}
	c.delays = append(c.delays, delay)
	if len(c.delays) > cutWindow {
		c.delays = c.delays[1:]
	}
}

// step takes the mirror's achieved throughput over the tick and whether CAKE
// dropped or marked its packets, moves the rate within min..limit (limit 0:
// the controller's max) from the delays observed so far, and says why.
func (c *controller) step(now time.Time, got float64, limited bool, limit float64) string {
	if limit <= 0 || limit > c.max {
		limit = c.max
	}
	copy(c.recent[1:], c.recent[:])
	c.recent[0] = got
	reason := c.decide(now, got, limited)
	c.rate = min(max(c.rate, c.min), limit)
	return reason
}

func (c *controller) decide(now time.Time, mirror float64, limited bool) string {
	if c.ceiling > 0 && now.Sub(c.ceilingAt) > ceilingMemory {
		c.ceiling = 0
	}
	if !c.known {
		return "no latency probe answered this tick, holding"
	}
	short, shortOK := c.queue(settleWindow)
	long, longOK := c.queue(cutWindow)
	newest := c.delays[len(c.delays)-1]
	severe := shortOK && short > severeFactor*c.target
	// got is what the link gave the mirror lately (one tick's throughput is
	// noisy); the mirror is active when that is a real share of its cap.
	got := slices.Max(c.recent[:])
	active := got >= activeShare*c.rate
	// The shaper is what holds the mirror back when CAKE had to drop or mark
	// its packets, or when it delivers close to the cap. The first is the
	// signal at low rates, where CAKE's ingress mode counts drops as sent and
	// TCP gets well below the cap.
	busy := limited || mirror >= busyShare*c.rate
	switch {
	case severe || longOK && long > c.target:
		if now.Sub(c.cutAt) < cutRefractory {
			return "link queue building, waiting for the last cut to show"
		}
		c.markCeiling(now, got, active, limited)
		f, q := cutFactor, long
		if severe {
			f, q = severeCut, short // a deep queue: get out of the way fast
		}
		// Below what an active mirror got; an idle mirror's unused cap only
		// decays, so its next pull doesn't start from the floor.
		if active {
			c.rate = min(c.rate, got)
		}
		c.rate *= f
		c.cutAt = now
		return fmt.Sprintf("link queue at +%d ms, the mirror backs off", q.Milliseconds())
	case !shortOK:
		return "measuring the link"
	case active && (short > c.target/2 || !limited && newest > c.target/2):
		// The link is filling up: settle just below what it gave the mirror,
		// before the queue reaches the target. One tick is enough when CAKE
		// wasn't dropping: then the link, not the shaper, held the mirror
		// back, and the queue is real.
		c.markCeiling(now, got, active, limited)
		c.rate = min(c.rate, got) * settleFactor
		return "link filling up, the mirror settles"
	case !busy:
		return "the mirror is below its cap"
	case newest > c.target/2:
		// One tick up while CAKE holds the mirror back: noise, or someone
		// else's burst. Wait for the second that tells them apart.
		return "link busy for a moment, holding"
	default:
		// Fast while far from where the link filled up last, slow around it.
		f := probeFast
		if c.ceiling > 0 && c.rate >= 0.9*c.ceiling && c.rate <= 1.1*c.ceiling {
			f = probeSlow
		}
		c.rate *= f
		return "link has room, the mirror may take more"
	}
}

// markCeiling remembers what the link gave the mirror when it filled up, so
// the probing slows down just below it. Only when the link held an active
// mirror back: if CAKE was dropping, the mirror got no more than its own cap,
// and the queue was someone else's, or Wi-Fi noise, and says nothing about
// the link.
func (c *controller) markCeiling(now time.Time, got float64, active, limited bool) {
	if active && !limited {
		c.ceiling, c.ceilingAt = min(c.rate, got), now
	}
}
