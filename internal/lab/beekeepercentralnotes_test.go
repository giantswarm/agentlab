package lab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only the watch's central feed lines count, each saying every wanted part.
func TestFeedWatchCentralLines(t *testing.T) {
	w := &feedWatch{out: &lockedBuffer{}}
	_, _ = w.out.Write([]byte(strings.Join([]string{
		`CENTRAL answer.recorded (pia-agent): note #3 answered by "dev@lab.local" via cli: wait for Monday, run AB [dev@lab.local on agentlab-pia]`,
		`MAILBOX lab#3 waits for ana-agent, delivered once it takes it: wait for Monday, run AB`,
		`CENTRAL lease.claim (pia-agent): agentlab-shared: after-AB [dev@lab.local on agentlab-pia]`,
		"",
	}, "\n")))
	if n := len(w.centralLines("wait for Monday, run AB")); n != 1 {
		t.Errorf("answer lines = %d, want 1", n)
	}
	if n := len(w.centralLines("answer.recorded", "dev@lab.local", "run AB")); n != 1 {
		t.Errorf("answer.recorded lines = %d, want 1", n)
	}
	if n := len(w.centralLines("answer.recorded", "after-AB")); n != 0 {
		t.Errorf("lines saying both = %d, want 0", n)
	}
}

// The watch's mark is read from the machine's state, whatever its key.
func TestFeedMark(t *testing.T) {
	dir := t.TempDir()
	p := &centralProof{anaCfg: filepath.Join(dir, "config.yaml")}
	if got := p.feedMark(); got != "" {
		t.Errorf("mark without a file = %q", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "seen.watch.ana-agent.json"), []byte(`{"feedID":"1792701838448345396"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.feedMark(); got != "1792701838448345396" {
		t.Errorf("mark = %q", got)
	}
}
