package lab

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// A stage clock: every read returns the time and advances it by the next of
// its ticks, so a tick is what passes between two reads.
type stageClock struct {
	at    time.Time
	ticks []time.Duration
}

func (c *stageClock) now() time.Time {
	t := c.at
	if len(c.ticks) > 0 {
		c.at, c.ticks = c.at.Add(c.ticks[0]), c.ticks[1:]
	}
	return t
}

func testProofStages(ticks ...time.Duration) (*proofStages, *bytes.Buffer) {
	var out bytes.Buffer
	clock := &stageClock{at: time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC), ticks: ticks}
	return &proofStages{now: clock.now, out: &out}, &out
}

// summaryRows are the summary's lines with their columns collapsed to one
// space, read without the padding of the name column.
func summaryRows(out *bytes.Buffer) string {
	var rows []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		rows = append(rows, strings.Join(strings.Fields(line), " "))
	}
	return strings.Join(rows, "\n")
}

func wantRows(t *testing.T, out *bytes.Buffer, rows ...string) {
	t.Helper()
	got := summaryRows(out)
	for _, want := range rows {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}

// TestProofStagesSummary: every stage is listed with its outcome and the
// time from its begin to the next stage's, under the wall time of the run,
// and a proof that passes every stage closes without an error.
func TestProofStagesSummary(t *testing.T) {
	// The reads: begin a, begin b (ends a), begin c (ends b), close (ends
	// c), the summary's wall time.
	p, out := testProofStages(80*time.Millisecond, 1500*time.Millisecond, 70*time.Second, 0)
	p.begin(stageDexToken, "Logging in")
	p.begin(stageMusterSession, "MCP initialize")
	p.begin(stageMCPToolCall, "Calling list")
	if err := p.close(nil); err != nil {
		t.Fatalf("close(nil) = %v", err)
	}
	wantRows(t, out,
		"3 stages in 1m12s",
		"PASS 80ms "+stageDexToken,
		"PASS 1.5s "+stageMusterSession,
		"PASS 1m10s "+stageMCPToolCall,
	)
}

// TestProofStagesFail: the error that ends the proof fails the stage under
// way, the summary says so, and the error returned names that stage — the
// stages before it passed.
func TestProofStagesFail(t *testing.T) {
	p, out := testProofStages(80*time.Millisecond, 30*time.Millisecond, 0)
	p.begin(stageDexToken, "Logging in")
	p.begin(stageMusterSession, "MCP initialize")
	cause := errors.New("muster is not reachable at https://muster.example — run `agentlab platform` first")
	err := p.close(cause)
	if err == nil || !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), stageMusterSession+": ") {
		t.Errorf("close(err) = %v, want %q wrapping the cause", err, stageMusterSession+": "+cause.Error())
	}
	wantRows(t, out,
		"PASS 80ms "+stageDexToken,
		"FAIL 30ms "+stageMusterSession,
	)
}

// TestProofStagesSkip: a stage the configuration leaves out is listed as
// skipped with the reason, singly (skip) and as a block (leaveOut), and
// never fails the proof — the configuration is the one allowance.
func TestProofStagesSkip(t *testing.T) {
	// The reads: begin a, begin b (ends a), skip (ends b), leaveOut, begin
	// c, close (ends c), the wall time.
	p, out := testProofStages(80*time.Millisecond, 5*time.Millisecond, 0, 0, 20*time.Millisecond, 0)
	p.begin(stageDexToken, "Logging in")
	p.begin(stageControllerIdentity, "The controller route")
	p.skip("the 3.x connectivity chart renders no GRPCRoute")
	p.leaveOut("platform.observability is off", stagePrometheusTools, stagePromQL)
	p.begin(stageOAuthSignIn, "Per-server OAuth sign-in")
	if err := p.close(nil); err != nil {
		t.Fatalf("close(nil) = %v, want no error: a configured skip is allowed", err)
	}
	wantRows(t, out,
		"5 stages in 105ms",
		"PASS 80ms "+stageDexToken,
		"SKIP 5ms "+stageControllerIdentity+" (the 3.x connectivity chart renders no GRPCRoute)",
		"SKIP 0ms "+stagePrometheusTools+" (platform.observability is off)",
		"SKIP 0ms "+stagePromQL+" (platform.observability is off)",
		"PASS 20ms "+stageOAuthSignIn,
	)
}

// TestProofStagesFailAfterSkip: an error after a skipped stage still ends
// the proof with it, and a skipped stage is never re-judged.
func TestProofStagesFailAfterSkip(t *testing.T) {
	p, out := testProofStages(10*time.Millisecond, 0, 0)
	p.begin(stageFamilies, "Infrastructure families")
	p.skip("an upgrade seed registers the family-less mcp-kubernetes")
	cause := errors.New("muster aggregates no x_prometheus_ tools")
	if err := p.close(cause); !errors.Is(err, cause) {
		t.Errorf("close(err) = %v, want the cause", err)
	}
	wantRows(t, out, "SKIP 10ms "+stageFamilies+" (an upgrade seed registers the family-less mcp-kubernetes)")
	if got := summaryRows(out); strings.Contains(got, "FAIL") {
		t.Errorf("the skipped stage was re-judged:\n%s", got)
	}
}

func TestTookString(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                     "0ms",
		999 * time.Millisecond:                "999ms",
		time.Second:                           "1.0s",
		1143 * time.Millisecond:               "1.1s",
		59*time.Second + 950*time.Millisecond: "60.0s",
		time.Minute:                           "1m0s",
		70*time.Second + 400*time.Millisecond: "1m10s",
	} {
		if got := tookString(d); got != want {
			t.Errorf("tookString(%s) = %q, want %q", d, got, want)
		}
	}
}
