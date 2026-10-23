package lab

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The central notes step: a decision one person's machine files for the
// other through the CLI (note add --for <email>) is listed and queued on
// the other's machine as <context>#<n> and answered there with note
// answer; the filer's standby watch, restarted while the answer was
// given, prints its answer.recorded line from the central feed exactly
// once. A second decision is withdrawn by its filer, never by its
// addressee.

// CentralPartNotes runs the central notes step alone (--part).
const CentralPartNotes = "notes"

// CentralParts are the parts the central proof runs alone.
var CentralParts = []string{CentralPartNotes}

// centralNotesWait bounds the wait for a line of the watch: its feed
// subscription opens through muster and reads at each update.
const centralNotesWait = 90 * time.Second

// centralFeedID is a feed event id in a watch's mark.
var centralFeedID = regexp.MustCompile(`[0-9]{19}`)

// feedWatch is one run of the filer's standby watch.
type feedWatch struct {
	cmd *exec.Cmd
	out *lockedBuffer
}

// centralLines are the watch's lines of the central feed that say each of
// want.
func (w *feedWatch) centralLines(want ...string) []string {
	var lines []string
	for _, l := range strings.Split(w.out.String(), "\n") {
		if !strings.Contains(l, "CENTRAL ") {
			continue
		}
		all := true
		for _, s := range want {
			all = all && strings.Contains(l, s)
		}
		if all {
			lines = append(lines, l)
		}
	}
	return lines
}

// await waits for the watch's central line saying each of want.
func (w *feedWatch) await(what string, want ...string) error {
	if !waitFor(int(centralNotesWait/time.Second), time.Second, func() bool { return len(w.centralLines(want...)) > 0 }) {
		return fmt.Errorf("the watch printed no central line of %s (%q) within %s: %s", what, want, centralNotesWait, excerptEnds(w.out.String(), 800))
	}
	note("watch: %s", strings.TrimSpace(w.centralLines(want...)[0]))
	return nil
}

// startFeedWatch starts the filer's standby watch as its agent, which
// keeps its mark (the last feed event it printed) in the machine's state.
func (p *centralProof) startFeedWatch() (*feedWatch, error) {
	cmd, out, err := startStandbyWatch(p.bin, p.anaCfg, p.ca, withoutClaudeSession(os.Environ()), "--as", centralAnaAgent)
	if err != nil {
		return nil, err
	}
	note("standby watch %d as %s on %s", cmd.Process.Pid, centralAnaAgent, p.anaCfg)
	return &feedWatch{cmd: cmd, out: out}, nil
}

// feedMark is the feed event id the filer's watch keeps in its mark.
func (p *centralProof) feedMark() string {
	marks, _ := filepath.Glob(filepath.Join(filepath.Dir(p.anaCfg), "state", "seen.watch.*.json"))
	for _, m := range marks {
		if raw, err := os.ReadFile(m); err == nil { //nolint:gosec // the run directory's own mark
			if id := centralFeedID.Find(raw); id != nil {
				return string(id)
			}
		}
	}
	return ""
}

// sentinel puts an event into the central feed: the other person's
// machine claims and releases the shared installation for purpose.
func (p *centralProof) sentinel(purpose string) error {
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbLease, verbClaim, centralEnvironment, "-p", purpose}, "claimed "+centralEnvironment); err != nil {
		return err
	}
	return p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbLease, verbRelease, centralEnvironment})
}

// fileFor files a decision on the filer's machine for the other person
// through the CLI and returns its central number.
func (p *centralProof) fileFor(question, run string) (int, error) {
	args := []string{verbNote, "add", "--for", p.pia, "--due", "3h",
		"--status-quo", "The shared lane rolls one release a night; run " + run + " of the agentlab central proof asks.",
		"--why", "Only the person it is for decides what rolls on the shared installation.",
		"--option", centralRollOption,
		"--option", "Wait: nothing rolls before Monday",
		"--default", "keep the lane closed until Monday",
		"--ref", centralRepo + "#1", question}
	out, code := p.beekeeper(p.anaCfg, centralAnaAgent, args...)
	if code != 0 {
		return 0, fmt.Errorf("beekeeper note add --for %s: exit %d: %s", p.pia, code, excerpt(out, 400))
	}
	var id int
	if _, err := fmt.Sscanf(out, "note #%d (central "+centralContext, &id); err != nil {
		return 0, fmt.Errorf("beekeeper note add --for %s filed no central note: %s", p.pia, excerpt(out, 400))
	}
	note("beekeeper note add --for %s: %s", p.pia, strings.TrimSpace(out))
	return id, nil
}

func (p *centralProof) centralNotes() error {
	run := randomSuffix()
	question := "Central notes proof " + run + ": may the shared lane roll tonight?"
	answer := "wait for Monday, run " + run
	before, during, after := "before-"+run, "during-"+run, "after-"+run

	// A watch without a mark starts at the feed's newest event: the feed
	// has one before the first run.
	if err := p.sentinel(before); err != nil {
		return err
	}
	w, err := p.startFeedWatch()
	if err != nil {
		return err
	}
	defer func() { stopWatch(w.cmd) }()
	if !waitFor(int(centralNotesWait/time.Second), time.Second, func() bool { return p.feedMark() != "" }) {
		return fmt.Errorf("the watch kept no feed mark within %s: %s", centralNotesWait, excerptEnds(w.out.String(), 800))
	}
	note("watch run 1 starts at feed event %s", p.feedMark())

	id, err := p.fileFor(question, run)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%d", centralContext, id)
	if err := w.await("the filing", "note.add", fmt.Sprintf("#%d", id)); err != nil {
		return err
	}
	stopWatch(w.cmd)
	mark := p.feedMark()
	note("watch run 1 stopped at feed event %s", mark)

	// While the watch is down: listed and queued on the other machine, not
	// answerable by its filer, answered by its addressee.
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbNote, beekeeperVerbList}, ref, question); err != nil {
		return err
	}
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{"guide", "queue"}, ref); err != nil {
		return err
	}
	if out, code := p.beekeeper(p.anaCfg, centralAnaAgent, verbNote, "answer", ref, "--choice", "1", "roll it"); code == 0 {
		return fmt.Errorf("%s answered %s, a decision for %s: %s", p.ana, ref, p.pia, excerpt(out, 300))
	}
	note("%s's answer to %s is refused: it is %s's", p.ana, ref, p.pia)
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbNote, "answer", ref, "--choice", "2", answer}, "note "+ref+" answered as "+p.pia); err != nil {
		return err
	}
	if out, _ := p.beekeeper(p.piaCfg, centralPiaAgent, verbNote, beekeeperVerbList); strings.Contains(out, question) {
		return fmt.Errorf("%s is still open after its answer: %s", ref, excerpt(out, 400))
	}
	if err := p.sentinel(during); err != nil {
		return err
	}

	// Run 2 prints what happened while it was down, the answer once.
	if w, err = p.startFeedWatch(); err != nil {
		return err
	}
	if err := w.await("the answer", "answer.recorded", p.pia, answer); err != nil {
		return err
	}
	if err := w.await("the event after the answer", during); err != nil {
		return err
	}
	if err := p.sentinel(after); err != nil {
		return err
	}
	if err := w.await("a later event", after); err != nil {
		return err
	}
	if n := len(w.centralLines(answer)); n != 1 {
		return fmt.Errorf("watch run 2 printed the answer %d times, want once: %s", n, excerptEnds(w.out.String(), 800))
	}
	if n := len(w.centralLines("note.add", fmt.Sprintf("#%d", id))); n != 0 {
		return fmt.Errorf("watch run 2 printed the filing run 1 printed again: %s", excerptEnds(w.out.String(), 800))
	}
	stopWatch(w.cmd)
	note("watch run 2 resumed after %s and printed the answer once", mark)

	// Run 3 prints neither again.
	final := "final-" + run
	if err := p.sentinel(final); err != nil {
		return err
	}
	if w, err = p.startFeedWatch(); err != nil {
		return err
	}
	if err := w.await("the event while it was down", final); err != nil {
		return err
	}
	for _, s := range []string{answer, after} {
		if n := len(w.centralLines(s)); n != 0 {
			return fmt.Errorf("watch run 3 printed %q again: %s", s, excerptEnds(w.out.String(), 800))
		}
	}
	stopWatch(w.cmd)
	note("watch run 3 printed neither the answer nor run 2's events again")

	// A second decision: its addressee cannot withdraw it, its filer does.
	second := "Central notes proof " + run + ": may the shared lane roll on Sunday?"
	id2, err := p.fileFor(second, run)
	if err != nil {
		return err
	}
	ref2 := fmt.Sprintf("%s#%d", centralContext, id2)
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbNote, beekeeperVerbList}, ref2); err != nil {
		return err
	}
	if out, code := p.beekeeper(p.piaCfg, centralPiaAgent, verbNote, "done", ref2); code == 0 {
		return fmt.Errorf("%s withdrew %s, %s's decision: %s", p.pia, ref2, p.ana, excerpt(out, 300))
	}
	note("%s's withdrawal of %s is refused: it is %s's", p.pia, ref2, p.ana)
	if err := p.expect(p.anaCfg, centralAnaAgent, 0, []string{verbNote, "done", ref2}, "note "+ref2+" done"); err != nil {
		return err
	}
	if out, _ := p.beekeeper(p.piaCfg, centralPiaAgent, verbNote, beekeeperVerbList); strings.Contains(out, second) {
		return fmt.Errorf("%s is still listed after its withdrawal: %s", ref2, excerpt(out, 400))
	}
	note("%s withdrew %s; %s no longer lists it", p.ana, ref2, p.pia)
	return nil
}

// centralPark is step 8: the admin's registered agent, given a task, files
// a decision for the other person, folds a second one into it by its
// <context>#<n> and parks on it; the park stands while the decision is
// open, and once its addressee answers, the filer's watch says AGENT
// RESUMABLE once with the answer word for word.
func (p *centralProof) centralPark() error {
	run := randomSuffix()
	if err := p.expect(p.anaCfg, centralSession, 0, []string{"agents", "register"}); err != nil {
		return err
	}
	if err := p.expect(p.anaCfg, centralAnaAgent, 0, []string{"agents", "assign", centralSession, "central park proof " + run}, "assigned to "+centralSession); err != nil {
		return err
	}
	question := "Central park proof " + run + ": may the shared lane roll tonight?"
	id, err := p.fileFor(question, run)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("%s#%d", centralContext, id)
	folded := "Central park proof " + run + ": may agentlab roll with it?"
	if err := p.expect(p.anaCfg, centralSession, 0, []string{verbNote, "add", "--for", p.pia, "--due", "3h",
		"--status-quo", "agentlab rolls with the shared lane; run " + run + " of the agentlab central proof asks.",
		"--why", "Only the person it is for decides what rolls on the shared installation.",
		"--option", centralRollOption, "--option", "Wait: nothing rolls before Monday",
		"--default", "keep the lane closed until Monday", "--ref", centralRepo + "#1",
		"--fold-into", ref, folded}, "note "+ref+" (folded"); err != nil {
		return err
	}
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbNote, beekeeperVerbList, "--full"}, ref, question, folded); err != nil {
		return err
	}
	note("%s folded into %s; %s lists one note with both questions", folded, ref, p.pia)
	if err := p.expect(p.anaCfg, centralSession, 0, []string{"agents", "park", "--on", ref, p.pia + "'s answer"}, "parked on "+ref); err != nil {
		return err
	}
	if err := p.expect(p.anaCfg, centralSession, exitRefused, []string{"agents", "park", "--on", centralContext + "#999999", "nothing"}, "is not open"); err != nil {
		return err
	}
	watchOnce := func() (string, error) {
		out, code := p.beekeeper(p.anaCfg, centralSession, "watch", "--once")
		if code != 0 && code != exitRefused {
			return "", fmt.Errorf("watch --once: exit %d: %s", code, excerpt(out, 400))
		}
		return out, nil
	}
	out, err := watchOnce()
	if err != nil {
		return err
	}
	if strings.Contains(out, "AGENT RESUMABLE") {
		return fmt.Errorf("the park on the open %s settled: %s", ref, excerpt(out, 400))
	}
	note("%s's agent parked on %s; the watch leaves it parked while it is open", p.ana, ref)
	answer := "wait for Monday, park run " + run
	if err := p.expect(p.piaCfg, centralPiaAgent, 0, []string{verbNote, "answer", ref, "--choice", "2", answer}, "note "+ref+" answered as "+p.pia); err != nil {
		return err
	}
	if out, err = watchOnce(); err != nil {
		return err
	}
	var resumable []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "AGENT RESUMABLE") {
			resumable = append(resumable, l)
		}
	}
	if len(resumable) != 1 || !strings.Contains(resumable[0], ref+" answered by "+p.pia) || !strings.Contains(resumable[0], answer) {
		return fmt.Errorf("the watch's AGENT RESUMABLE after %s's answer: %q in %s", p.pia, resumable, excerpt(out, 800))
	}
	note("watch: %s", strings.TrimSpace(resumable[0]))
	if out, err = watchOnce(); err != nil {
		return err
	}
	if strings.Contains(out, "AGENT RESUMABLE") {
		return fmt.Errorf("the watch said the settled park again: %s", excerpt(out, 400))
	}
	return p.expect(p.anaCfg, centralSession, 0, []string{"agents", "idle"})
}
