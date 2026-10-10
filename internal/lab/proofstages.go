package lab

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// proofStages is the record of a proof's stages. Each begins with its step
// line and ends judged: PASS when the next stage begins or the proof closes
// without an error, FAIL on the error that ends the proof, SKIP with the
// reason when the lab's own configuration leaves it without a subject — a
// component agentlab.yaml turns off, a chart line without the route. The
// summary at the end names every stage with its outcome and duration: on a
// warm lab the whole platform proof takes about a second, which the
// whole-second stamps of the step lines cannot show, and a run that left a
// stage out must not read as a pass. What the lab's live state cannot run —
// a fixture user that is not configured, a release that is not there, a read
// that failed — is never skipped: it fails, naming what is missing. The
// configuration is the one allowance, and the summary quotes it.
type proofStages struct {
	stages []*proofStage
	now    func() time.Time
	out    io.Writer
}

// A proofStage is one stage: the short name the summary lists it by, its
// outcome with the reason (a skip's why, a failure's error) and how long it
// took.
type proofStage struct {
	name    string
	outcome stageOutcome
	reason  string
	began   time.Time
	took    time.Duration
}

type stageOutcome string

const (
	stagePass stageOutcome = "PASS"
	stageSkip stageOutcome = "SKIP"
	stageFail stageOutcome = "FAIL"
)

func newProofStages() *proofStages {
	return &proofStages{now: time.Now, out: os.Stdout}
}

// begin ends the stage under way as passed and starts the next at the same
// instant: name is the summary's short name, the step line is formatted from
// the rest.
func (p *proofStages) begin(name, format string, a ...any) {
	at := p.now()
	p.endAt(at, stagePass, "")
	p.stages = append(p.stages, &proofStage{name: name, began: at})
	step(format, a...)
}

// skip ends the stage under way as left out by the configuration, with why.
func (p *proofStages) skip(format string, a ...any) {
	reason := fmt.Sprintf(format, a...)
	note("skipped: %s", reason)
	p.end(stageSkip, reason)
}

// leaveOut records stages the configuration leaves out as a whole — a
// component that is off — skipped with the one reason, in one note.
func (p *proofStages) leaveOut(reason string, names ...string) {
	at := p.now()
	p.endAt(at, stagePass, "")
	note("skipped %s: %s", strings.Join(names, ", "), reason)
	for _, name := range names {
		p.stages = append(p.stages, &proofStage{name: name, outcome: stageSkip, reason: reason, began: at})
	}
}

// end judges the stage under way now, once.
func (p *proofStages) end(outcome stageOutcome, reason string) {
	p.endAt(p.now(), outcome, reason)
}

func (p *proofStages) endAt(at time.Time, outcome stageOutcome, reason string) {
	s := p.current()
	if s == nil || s.outcome != "" {
		return
	}
	s.outcome, s.reason, s.took = outcome, reason, at.Sub(s.began)
}

func (p *proofStages) current() *proofStage {
	if len(p.stages) == 0 {
		return nil
	}
	return p.stages[len(p.stages)-1]
}

// close ends the proof with the error its body returned: the stage under way
// fails with it, or passes without one. The summary is printed either way,
// and the error returned names the stage that failed.
func (p *proofStages) close(err error) error {
	if err != nil {
		p.end(stageFail, err.Error())
	} else {
		p.end(stagePass, "")
	}
	p.summarize()
	if err == nil {
		return nil
	}
	for _, s := range p.stages {
		if s.outcome == stageFail {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return err
}

// summarize prints every stage with its outcome and duration, under the
// wall time from the first stage's start.
func (p *proofStages) summarize() {
	if len(p.stages) == 0 {
		return
	}
	width := 0
	for _, s := range p.stages {
		width = max(width, len(s.name))
	}
	_, _ = fmt.Fprintf(p.out, "==> %s %d stages in %s\n", elapsedStamp(), len(p.stages), tookString(p.now().Sub(p.stages[0].began)))
	for _, s := range p.stages {
		line := fmt.Sprintf("    %s %7s  %-*s", s.outcome, tookString(s.took), width, s.name)
		if s.outcome == stageSkip {
			line += "  (" + s.reason + ")"
		}
		_, _ = fmt.Fprintln(p.out, strings.TrimRight(line, " "))
	}
}

// tookString words a duration at the precision a proof on a warm lab needs:
// milliseconds under a second, tenths under a minute, whole seconds beyond.
func tookString(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}
