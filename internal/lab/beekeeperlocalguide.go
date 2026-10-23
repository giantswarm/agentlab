package lab

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The local guide step: the person's own beekeeper on the host, as their
// machine runs it, and a real Claude Code session as their guide. The
// guide's question goes out with `beekeeper converse`, the person's reply in
// its Slack thread reaches their mailbox, the standby watch's subscription
// to it through muster delivers the reply into the running guide session at
// its next turn, and the guide answers in the same thread.

const (
	// localGuideHost is the machine of the local guide, its roster address
	// local:<host>/<name>.
	localGuideHost = "agentlab-guide"
	localGuideAsk  = "Which window suits the lab's upgrade?"
	localGuideSays = "Thursday after the standup."
	// localGuideAnswer starts the guide's answer, which quotes the reply.
	localGuideAnswer = "Received:"
	// localGuideWait covers the guide's turn on the reply.
	localGuideWait          = 3 * time.Minute
	localGuideWatchInterval = "5s"
)

// localGuide is the person's machine: its beekeeper, configuration and
// standby watch, and the Claude Code session that is its guide.
type localGuide struct {
	bin, cfg, ca, dir string
	name, short       string
	transcript        string
	watch             *exec.Cmd
	watchOut          *lockedBuffer
}

// startLocalGuide writes the person's machine, starts the guide as a
// background Claude Code session on the smallest model, registers it and
// starts the standby watch.
func startLocalGuide(runDir, bin, ca, email, endpoint, token, run string) (*localGuide, error) {
	cfgFile, err := centralMachine(runDir, "guide", email, endpoint, token)
	if err != nil {
		return nil, err
	}
	if err := quietWatchConfig(cfgFile); err != nil {
		return nil, err
	}
	g := &localGuide{bin: bin, cfg: cfgFile, ca: ca, dir: filepath.Join(filepath.Dir(cfgFile), "session"), name: "Lab guide " + run}
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return nil, err
	}
	reply := fmt.Sprintf("SSL_CERT_FILE=%s %s --config %s --as '%s' converse \"%s <their text>\"", ca, bin, cfgFile, g.name, localGuideAnswer)
	brief := fmt.Sprintf("You are %s, a throwaway test session of an agentlab proof. Reply with the single word ok and end your turn. "+
		"A later message that starts with 'Message from' and says 'via slack' is your person writing to you in Slack: answer it by running exactly one Bash command, "+
		"with the text after the message's id in place of <their text>, and nothing else: %s", g.name, reply)
	cmd := exec.Command("claude", "--bg", "-n", g.name, "--model", "haiku", brief) //nolint:gosec // the proof's own guide
	cmd.Dir = g.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("claude --bg: %v: %s", err, excerpt(string(out), 300))
	}
	m := regexp.MustCompile(`backgrounded · ([0-9a-f]{8})`).FindStringSubmatch(string(out))
	if m == nil {
		return nil, fmt.Errorf("claude --bg printed no session: %s", excerpt(string(out), 300))
	}
	g.short = m[1]
	note("guide %q: background session %s in %s", g.name, g.short, g.dir)
	home, err := os.UserHomeDir()
	if err != nil {
		return g, err
	}
	projects := filepath.Join(home, ".claude", "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(g.dir, "-"))
	if !waitFor(60, time.Second, func() bool {
		matches, _ := filepath.Glob(filepath.Join(projects, g.short+"*.jsonl"))
		if len(matches) == 1 {
			g.transcript = matches[0]
		}
		return g.transcript != "" && g.said("ok")
	}) {
		return g, fmt.Errorf("guide %s: no first turn in %s within a minute", g.short, projects)
	}
	id := strings.TrimSuffix(filepath.Base(g.transcript), ".jsonl")
	reg := exec.Command(bin, flagConfig, cfgFile, "agents", "register", "--name", g.name) //nolint:gosec // the proof's beekeeper
	reg.Env = append(os.Environ(), "CLAUDE_CODE_SESSION_ID="+id, "CLAUDE_CODE_SESSION_NAME="+g.name, "SSL_CERT_FILE="+ca)
	if out, err := reg.CombinedOutput(); err != nil {
		return g, fmt.Errorf("agents register: %v: %s", err, excerpt(string(out), 300))
	}
	if g.watch, g.watchOut, err = startStandbyWatch(bin, cfgFile, ca, os.Environ()); err != nil {
		return g, err
	}
	note("standby watch %d on %s", g.watch.Process.Pid, cfgFile)
	return g, nil
}

// said reports whether the guide's transcript holds text.
func (g *localGuide) said(text string) bool {
	f, err := os.Open(g.transcript)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if strings.Contains(sc.Text(), text) {
			return true
		}
	}
	return false
}

// converse runs the person's beekeeper as the guide: converse text.
func (g *localGuide) converse(text string) (string, error) {
	cmd := exec.Command(g.bin, flagConfig, g.cfg, "--as", g.name, "converse", text) //nolint:gosec // the proof's beekeeper
	cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+g.ca, "CLAUDE_CODE_SESSION_ID=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stop ends the watch and the guide's session and takes it off the roster.
func (g *localGuide) stop() {
	stopWatch(g.watch)
	if g.short != "" {
		_ = exec.Command("claude", "stop", g.short).Run() //nolint:gosec // the proof's own guide
	}
}

// localGuideConversation is the step: the guide's question through the
// person's beekeeper, their reply in the thread delivered into the running
// guide session by the standby watch, the guide's answer in the thread.
func (p *beekeeperProof) localGuideConversation(g *localGuide) error {
	var text string
	var err error
	// The watch publishes the guide to the central roster at its first poll.
	if !waitFor(24, 5*time.Second, func() bool {
		text, err = g.converse(localGuideAsk)
		return err == nil
	}) {
		return fmt.Errorf("beekeeper converse as %s: %v: %s; the watch says: %s", g.name, err, excerpt(text, 300), excerptEnds(g.output(), 600))
	}
	r := guideThread{channel: directChannel(p.askerSlack)}
	if _, err := fmt.Sscanf(text, "converse: opened conversation %s", &r.id); err != nil {
		return fmt.Errorf("beekeeper converse answered %q, not an opened conversation", excerpt(text, 200))
	}
	m, err := p.message(r.channel, localGuideAsk, "the local guide's direct message", decisionsWait, func(slackMessage) bool { return true })
	if err != nil {
		return err
	}
	r.ts = m.TS
	note("conversation %s: the guide's question, from its own beekeeper, in %s", r.id, r.channel)

	if err := p.replyInThread(r, p.askerSlack, localGuideSays); err != nil {
		return err
	}
	stamp := "Message from " + p.asker + " via slack"
	// The session takes the message while the watch's send still runs: the
	// watch says its delivery once the send returned.
	delivered := "MAILBOX to " + g.name
	if !waitFor(int(localGuideWait/time.Second), time.Second, func() bool {
		return g.said(stamp) && g.said(localGuideSays) && strings.Contains(g.output(), delivered)
	}) {
		return fmt.Errorf("the reply did not reach the running guide session %s within %s (in its transcript: %t); the watch says: %s",
			g.short, localGuideWait, g.said(stamp), excerptEnds(g.output(), 600))
	}
	note("the reply is in the running guide session, stamped %q", stamp)
	// The guide's turn on the reply is a model's: it gets localGuideWait.
	if !waitFor(int(localGuideWait/time.Second), time.Second, func() bool {
		for _, m := range p.fake.thread(r.channel, r.ts) {
			if strings.Contains(m.shown(), localGuideAnswer) {
				return true
			}
		}
		return false
	}) {
		return fmt.Errorf("the guide's answer did not appear in conversation %s within %s; the watch says: %s", r.id, localGuideWait, excerptEnds(g.output(), 600))
	}
	note("the guide's answer, from its own session, is in the same thread")
	return nil
}

// quietWatchConfig appends to a machine's configuration a short watch
// interval and the host's own chores off: the proof's watch only reads the
// central instance.
func quietWatchConfig(cfgFile string) error {
	extra := fmt.Sprintf("watch: {interval: %s}\ndoctor: {goCacheMaxGiB: -1, dockerImagesMaxGiB: -1, dockerBuildCacheMaxGiB: -1, freeEvery: -1s, worktreesEvery: -1s}\n", localGuideWatchInterval)
	f, err := os.OpenFile(cfgFile, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the run directory's own configuration
	if err != nil {
		return err
	}
	if _, err := f.WriteString(extra); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// startStandbyWatch starts a machine's standby watch (args before the verb,
// --as <agent> for one) with env, the person's CA trusted and outside any
// Claude Code session; its output collects in the returned buffer.
func startStandbyWatch(bin, cfgFile, ca string, env []string, args ...string) (*exec.Cmd, *lockedBuffer, error) {
	out := &lockedBuffer{}
	cmd := exec.Command(bin, append([]string{flagConfig, cfgFile}, append(args, "watch", "--standby")...)...) //nolint:gosec // the proof's beekeeper
	cmd.Env = append(slices.Clone(env), "SSL_CERT_FILE="+ca, "CLAUDE_CODE_SESSION_ID=")
	cmd.Stdout, cmd.Stderr = out, out
	// Its own process group: stopWatch ends the commands it runs too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, out, nil
}

// stopWatch ends a watch started by startStandbyWatch and the commands it
// runs: SIGTERM, then a kill after 30 seconds, and its process group's
// leftovers killed.
func stopWatch(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
}

// output is what the watch printed so far.
func (g *localGuide) output() string { return g.watchOut.String() }

// lockedBuffer is a buffer the watch writes to while the proof reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
