package lab

import (
	"strings"
	"testing"
	"time"
)

func TestJudgeIsolation(t *testing.T) {
	own := "/workspace/" + sessionsOwnFilePrefix + "abc"
	if err := judgeIsolation("abc", []string{own}, probeRefused); err != nil {
		t.Fatalf("own file only, mirrors refused: %v", err)
	}
	for name, c := range map[string]struct {
		seen   []string
		write  string
		wanted string
	}{
		"another Session's file": {[]string{own, "/workspace/" + sessionsOwnFilePrefix + "def"}, probeRefused, "sees other Sessions' files"},
		"own file missing":       {nil, probeRefused, "finds its own file"},
		"mirrors writable":       {[]string{own}, "accepted", "a write into the mirrors was accepted"},
		"mirrors not tried":      {[]string{own}, "", "not tried"},
	} {
		err := judgeIsolation("abc", c.seen, c.write)
		if err == nil || !strings.Contains(err.Error(), c.wanted) {
			t.Errorf("%s: judgeIsolation = %v, want %q", name, err, c.wanted)
		}
	}
}

func TestParseSessionsBranches(t *testing.T) {
	reply := "```\n" + probeMarker + " pwd=/workspace\n" +
		probeMarker + " repo=" + proofPushed + " aaaa\n" +
		probeMarker + " branch=" + proofPushed + " `bbbb`\n" +
		probeMarker + " branch=" + proofStill + " " + probeNone + "\n```"
	got := parseSessionsBranches(reply)
	if got[proofPushed] != "bbbb" || got[proofStill] != probeNone || len(got) != 2 {
		t.Fatalf("parseSessionsBranches = %v", got)
	}
	if v, err := parseSessionsTurn(reply); err != nil || v.heads[proofPushed] != "aaaa" || len(v.heads) != 1 {
		t.Fatalf("the branch lines leak into the heads: %+v, %v", v, err)
	}
	if cmd := sessionsProofFirstTurnCommand("tok"); !strings.Contains(cmd, "origin/"+proofBranch) || !strings.Contains(cmd, sessionsOwnFilePrefix+"tok") {
		t.Fatalf("the first turn's command:\n%s", cmd)
	}
}

func TestJudgeFirstTurns(t *testing.T) {
	heads := map[string]string{proofPushed: "main1", proofStill: "main2"}
	ok := func(id string) *sessionRun {
		return &sessionRun{ID: id, Directory: workspacesTestMountPath,
			Heads:    map[string]string{proofPushed: "main1", proofStill: "main2"},
			Branches: map[string]string{proofPushed: "pushed", proofStill: probeNone}}
	}
	if err := judgeFirstTurns([]*sessionRun{ok("a"), ok("b")}, heads, proofPushed, "pushed"); err != nil {
		t.Fatalf("two good Sessions: %v", err)
	}
	stale := ok("c")
	stale.Branches[proofPushed] = "old"
	extra := ok("d")
	extra.Heads["example-org/third"] = "x"
	elsewhere := ok("e")
	elsewhere.Directory = "/data/workspace"
	moved := ok("f")
	moved.Heads[proofStill] = "moved"
	err := judgeFirstTurns([]*sessionRun{ok("a"), ok("a"), stale, extra, elsewhere, moved}, heads, proofPushed, "pushed")
	for _, w := range []string{"Session a started twice", "c's clone " + proofPushed + " has origin/" + proofBranch + " at \"old\"", "d holds a clone example-org/third", "e works in /data/workspace", "f's clone " + proofStill + " is at \"moved\""} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("judgeFirstTurns = %v, want %q", err, w)
		}
	}
}

func TestJudgeSetupFetches(t *testing.T) {
	good := &sessionRun{ID: "a", Fetched: []string{proofPushed}}
	if err := judgeSetupFetches([]*sessionRun{good}, proofPushed); err != nil {
		t.Fatal(err)
	}
	none := &sessionRun{ID: "b"}
	both := &sessionRun{ID: "c", Fetched: []string{proofPushed, proofStill}}
	twice := &sessionRun{ID: "d", Fetched: []string{proofPushed, proofPushed}}
	err := judgeSetupFetches([]*sessionRun{good, none, both, twice}, proofPushed)
	for _, id := range []string{"b's setup fetched []", "c's setup fetched", "d's setup fetched"} {
		if err == nil || !strings.Contains(err.Error(), id) {
			t.Errorf("judgeSetupFetches = %v, want %q", err, id)
		}
	}
}

func TestJudgeFreshAndMirrorsKept(t *testing.T) {
	want := map[string]string{proofPushed: "h"}
	fresh := sessionsTurnValues{pwd: workspacesTestMountPath, heads: map[string]string{proofPushed: "h"}}
	if err := judgeFresh(fresh, "tok", want); err != nil {
		t.Fatal(err)
	}
	stale := fresh
	stale.own = []string{sessionsOwnFilePrefix + "tok"}
	if err := judgeFresh(stale, "tok", want); err == nil || !strings.Contains(err.Error(), "not fresh") {
		t.Fatalf("a directory kept: %v", err)
	}
	before := map[string]map[string]string{proofPushed: {probeHeadKey: "a"}}
	if err := judgeMirrorsKept(before, before); err != nil {
		t.Fatal(err)
	}
	if err := judgeMirrorsKept(before, map[string]map[string]string{proofPushed: {probeHeadKey: "b"}}); err == nil {
		t.Fatal("a moved mirror judged kept")
	}
}

func TestWorkspaceManagerFlagJudges(t *testing.T) {
	args := []string{"workspace-manager", "run", sessionsKagentArg + "kagent-controller.kagent.svc:8083", sessionsCleanupArg + "12m"}
	if err := judgeKagentAddress(args); err != nil {
		t.Fatal(err)
	}
	d, err := judgeCleanupWindow(args, SessionsMinCleanupWindow, SessionsMaxCleanupWindow)
	if err != nil || d != 12*time.Minute {
		t.Fatalf("judgeCleanupWindow = %s, %v", d, err)
	}
	for _, window := range []string{"720h", "1m"} {
		if _, err := judgeCleanupWindow([]string{sessionsCleanupArg + window}, SessionsMinCleanupWindow, SessionsMaxCleanupWindow); err == nil || !strings.Contains(err.Error(), "--workspaces-cleanup-after "+formatWindow(sessionsExampleCleanupWindow)) {
			t.Errorf("window %s: %v", window, err)
		}
	}
	if _, err := judgeCleanupWindow(args[:2], SessionsMinCleanupWindow, SessionsMaxCleanupWindow); err == nil {
		t.Error("no window flag judged fine")
	}
	if sessionsExampleCleanupWindow < SessionsMinCleanupWindow || sessionsExampleCleanupWindow > SessionsMaxCleanupWindow {
		t.Fatalf("the suggested window %s is outside the range", sessionsExampleCleanupWindow)
	}
	if d, err := judgeSessionPoll([]string{"serve", sessionsPollArg + "30s"}); err != nil || d != 30*time.Second {
		t.Fatalf("judgeSessionPoll = %s, %v", d, err)
	}
	for _, poll := range []string{"soon", "0s", "5m"} {
		if _, err := judgeSessionPoll([]string{sessionsPollArg + poll}); err == nil {
			t.Errorf("poll %s judged fine", poll)
		}
	}
}

func TestSessionsTimingLines(t *testing.T) {
	at := time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC)
	lines := sessionsTimingLines([]*sessionRun{{
		ID: "s1", Harness: claudeHarness, StartCalled: at, StartAnswered: at.Add(time.Second), FirstTurnSent: at.Add(2 * time.Second),
		DirectorySetUp: at.Add(3500 * time.Millisecond), FirstTurnEnded: at.Add(20 * time.Second), SetupSeconds: 1.5,
	}})
	want := "timing session=s1 harness=claude startCalled=2026-10-12T22:00:00Z startAnswered=2026-10-12T22:00:01Z firstTurnSent=2026-10-12T22:00:02Z directorySetUp=2026-10-12T22:00:03.5Z firstTurnEnded=2026-10-12T22:00:20Z setupSeconds=1.500"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("sessionsTimingLines =\n%v\nwant\n%s", lines, want)
	}
}
