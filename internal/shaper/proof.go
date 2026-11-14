package shaper

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

// The namespace proof (`agentlab shaper-test`, docs/bandwidth.md "The
// namespace proof"): the shaper against a link with a deep FIFO, standing
// in for a bloated DSL modem, where the mirror pulls four streams and the
// person downloads one and pings the far side. This file holds the plan and
// the verdicts; proof_linux.go builds the namespaces and runs it.

// ErrProofFailed is a proof that ran and measured a criterion missed: the
// run said which.
var ErrProofFailed = errors.New("the namespace proof failed")

// ProofOptions is one run of the proof.
type ProofOptions struct {
	// Binary is this agentlab: the shaper and the traffic run it.
	Binary string
	// Baseline also runs the topology without the shaper, for comparison.
	Baseline bool
	// Out is where the run reports.
	Out io.Writer
}

// The topology and the run's timeline.
const (
	proofLinkRate   = 20e6                   // the link, bits per second
	proofLinkDelay  = 20 * time.Millisecond  // its one-way delay towards the person
	proofFIFO       = 700                    // netem's queue in packets: ~420 ms at 20 Mbit/s
	proofDuration   = 98 * time.Second       // the mirror pulls this long
	proofPersonFrom = 30 * time.Second       // the person's download starts here
	proofPersonFor  = 25 * time.Second       // and runs this long
	proofRamp       = 5 * time.Second        // the cap's ramp, left out of the latency
	proofSettle     = 10 * time.Second       // after the person's download, left out too
	proofMirrorRuns = 4                      // the mirror's streams
	proofPing       = 500 * time.Millisecond // the person's ping interval
)

// The verdicts: the person's single download gets at least half of the link
// while the mirror pulls, and the person's median latency while the mirror
// pulls alone stays within proofLatencyMargin of the base.
const (
	proofPersonShare   = 0.5
	proofLatencyMargin = 20 * time.Millisecond
)

// rttSample is one of the person's pings: when, since the start, and its
// round trip; lost when it never came back.
type rttSample struct {
	at   time.Duration
	rtt  time.Duration
	lost bool
}

// proofRun is what one run measured. The mirror's samples count from the
// run's start, the person's from their download's.
type proofRun struct {
	name     string
	mirror   []Sample
	person   []Sample
	pings    []rttSample
	shaperLn string // the shaper's last state, for the report
}

// mean is the average bits per second of the samples whose second ends in
// (from, to].
func mean(samples []Sample, from, to time.Duration) float64 {
	var sum float64
	n := 0
	for _, s := range samples {
		end := time.Duration(s.T) * time.Second
		if end > from && end <= to {
			sum += s.Bps
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// mirrorAlone reports whether a moment of the run counts as the mirror
// pulling alone for the latency: after the cap's ramp, before the person's
// download, and from proofSettle after it until the end.
func mirrorAlone(at time.Duration) bool {
	return at >= proofRamp && at < proofPersonFrom || at >= proofPersonFrom+proofPersonFor+proofSettle && at < proofDuration
}

// latency is the median and p90 round trip of the pings while the mirror
// pulls alone; a lost ping counts as a second.
func latency(pings []rttSample) (median, p90 time.Duration, n int) {
	var rtts []time.Duration
	for _, p := range pings {
		if !mirrorAlone(p.at) {
			continue
		}
		rtt := p.rtt
		if p.lost {
			rtt = time.Second
		}
		rtts = append(rtts, rtt)
	}
	if len(rtts) == 0 {
		return 0, 0, 0
	}
	slices.Sort(rtts)
	return rtts[len(rtts)/2], rtts[len(rtts)*9/10], len(rtts)
}

// proofVerdict is one run's numbers and, for the shaped run, whether they
// meet the acceptance criteria.
type proofVerdict struct {
	personDownload, mirrorAlone, mirrorShared float64
	median, p90                               time.Duration
	failures                                  []string
}

// judge computes a run's numbers against the base round trip.
func judge(r proofRun, base time.Duration) proofVerdict {
	v := proofVerdict{
		personDownload: mean(r.person, 0, proofPersonFor),
		mirrorShared:   mean(r.mirror, proofPersonFrom, proofPersonFrom+proofPersonFor),
		mirrorAlone: (mean(r.mirror, proofRamp, proofPersonFrom)*float64(proofPersonFrom-proofRamp) +
			mean(r.mirror, proofPersonFrom+proofPersonFor+proofSettle, proofDuration)*float64(proofDuration-proofPersonFrom-proofPersonFor-proofSettle)) /
			float64(proofDuration-proofPersonFor-proofSettle-proofRamp),
	}
	v.median, v.p90, _ = latency(r.pings)
	if v.personDownload < proofPersonShare*proofLinkRate {
		v.failures = append(v.failures, fmt.Sprintf("the person's download got %s while the mirror pulled, want at least %s (half the link)",
			formatThroughput(v.personDownload), FormatRate(proofPersonShare*proofLinkRate)))
	}
	if v.median > base+proofLatencyMargin {
		v.failures = append(v.failures, fmt.Sprintf("the person's median latency while the mirror pulled alone was %s, want at most %s (base %s + %s)",
			v.median.Round(time.Millisecond), (base+proofLatencyMargin).Round(time.Millisecond), base.Round(time.Millisecond), proofLatencyMargin))
	}
	return v
}

// report writes a run's line of the table.
func (v proofVerdict) report(w io.Writer, name string) {
	_, _ = fmt.Fprintf(w, "    %-12s the person's download %s · latency median %s, p90 %s · the mirror alone %s, beside the person %s\n",
		name, formatThroughput(v.personDownload), v.median.Round(time.Millisecond), v.p90.Round(time.Millisecond),
		formatThroughput(v.mirrorAlone), formatThroughput(v.mirrorShared))
}
