package lab

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCountFetches(t *testing.T) {
	docs, infra := "/"+syncMovingRepo+".git/", "/"+syncPrivateRepo+".git/"
	reqs := []githubRequest{
		{Method: http.MethodGet, Path: docs + "info/refs", Status: 200},
		{Method: http.MethodPost, Path: docs + gitUploadPack, Status: 200},
		{Method: http.MethodPost, Path: docs + gitUploadPack, Status: 200},
		{Method: http.MethodGet, Path: "/" + syncMovingRepo + "/info/refs", Status: 200},
		{Method: http.MethodGet, Path: infra + "info/refs", Status: 200},
		{Method: http.MethodGet, Path: githubFakeAPIPath + "/orgs/" + syncOSS + "/repos", Status: 200},
		{Method: http.MethodPost, Path: docs + "info/refs", Status: 405},
	}
	got := countFetches(reqs)
	if got[syncMovingRepo] != 2 || got[syncPrivateRepo] != 1 || len(got) != 2 {
		t.Fatalf("countFetches = %v, want docs 2, platform-infra 1", got)
	}
}

func TestJudgeFetches(t *testing.T) {
	const moved = syncMovingRepo
	if err := judgeFetches(map[string]int{moved: 10}, moved, 10); err != nil {
		t.Fatalf("one fetch per Session: %v", err)
	}
	for name, c := range map[string]struct {
		fetches map[string]int
		want    string
	}{
		"too few":     {map[string]int{moved: 9}, "fetched 9 times, want once per Session (10)"},
		"no fetch":    {map[string]int{}, "fetched 0 times"},
		"another one": {map[string]int{moved: 10, syncPrivateRepo: 1}, syncPrivateRepo + " fetched 1 times, want never"},
	} {
		err := judgeFetches(c.fetches, moved, 10)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: judgeFetches = %v, want %q", name, err, c.want)
		}
	}
}

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

func TestParseSessionsTurn(t *testing.T) {
	reply := "Here you go:\n```\n" + probeMarker + " pwd=/workspace\n" +
		probeMarker + " repo=" + syncMovingRepo + " 0123456789abcdef0123456789abcdef01234567\n" +
		probeMarker + " repo=" + syncPrivateRepo + " fedcba9876543210fedcba9876543210fedcba98\n" +
		probeMarker + " entries=.kagent AGENTS.md agentlab-oss agentlab-org\n" +
		probeMarker + " own-write=abc\n```"
	v, err := parseSessionsTurn(reply)
	if err != nil {
		t.Fatal(err)
	}
	if v.pwd != "/workspace" || v.ownWrite != "abc" || len(v.heads) != 2 || v.heads[syncMovingRepo] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("parseSessionsTurn = %+v", v)
	}
	if _, err := parseSessionsTurn("I would rather not."); err == nil {
		t.Fatal("a reply without the pwd line parsed")
	}
}

func TestJudgeFirstTurns(t *testing.T) {
	const head = "pushed"
	want := map[string]string{syncMovingRepo: head, syncPrivateRepo: "infra"}
	ok := func(id string) *sessionRun {
		return &sessionRun{ID: id, Directory: workspacesTestMountPath, Heads: map[string]string{syncMovingRepo: head, syncPrivateRepo: "infra"}}
	}
	if err := judgeFirstTurns([]*sessionRun{ok("a"), ok("b")}, want); err != nil {
		t.Fatalf("two good Sessions: %v", err)
	}
	stale := ok("c")
	stale.Heads[syncMovingRepo] = "old"
	extra := ok("d")
	extra.Heads["agentlab-org/other"] = "x"
	elsewhere := ok("e")
	elsewhere.Directory = "/data/workspace"
	err := judgeFirstTurns([]*sessionRun{ok("a"), ok("a"), stale, extra, elsewhere}, want)
	for _, w := range []string{"Session a started twice", "c's clone " + syncMovingRepo + " is at \"old\"", "d holds a clone agentlab-org/other", "e works in /data/workspace"} {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("judgeFirstTurns = %v, want %q", err, w)
		}
	}
}

func TestParseSessionDirectories(t *testing.T) {
	state := base64.StdEncoding.EncodeToString([]byte(`{"setUp":"2026-10-12T22:00:01Z","cloned":["` + syncMovingRepo + `"],"fetched":["` + syncMovingRepo + `"]}`))
	out := "dir s1 state=" + state + " own=" + sessionsOwnFilePrefix + "abc,\n" +
		"dir s2 state= own=\n" +
		"noise\n"
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("dirs = %v", dirs)
	}
	s1 := dirs["s1"]
	if s1.state == nil || !s1.state.SetUp.Equal(time.Date(2026, 10, 12, 22, 0, 1, 0, time.UTC)) || len(s1.own) != 1 || s1.state.Fetched[0] != syncMovingRepo {
		t.Fatalf("s1 = %+v", s1)
	}
	if dirs["s2"].state != nil || len(dirs["s2"].own) != 0 {
		t.Fatalf("s2 = %+v", dirs["s2"])
	}
	if _, err := parseSessionDirectories("dir s3 state=!!! own="); err == nil {
		t.Fatal("a broken setup record parsed")
	}
}

func TestJudgeFresh(t *testing.T) {
	want := map[string]string{syncMovingRepo: "c0ffee"}
	fresh := sessionsTurnValues{pwd: workspacesTestMountPath, heads: want}
	if err := judgeFresh(fresh, "abc", want); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	kept := fresh
	kept.own = []string{sessionsOwnFilePrefix + "abc"}
	if err := judgeFresh(kept, "abc", want); err == nil || !strings.Contains(err.Error(), "not fresh") {
		t.Fatalf("kept file: %v", err)
	}
	empty := sessionsTurnValues{pwd: workspacesTestMountPath}
	if err := judgeFresh(empty, "abc", want); err == nil || !strings.Contains(err.Error(), "holds no clone "+syncMovingRepo) {
		t.Fatalf("no clones: %v", err)
	}
}

func TestJudgeMirrorsKept(t *testing.T) {
	before := map[string]map[string]string{syncMovingRepo: {probeHeadKey: "a"}, syncPrivateRepo: {probeHeadKey: "b"}}
	if err := judgeMirrorsKept(before, before); err != nil {
		t.Fatal(err)
	}
	after := map[string]map[string]string{syncMovingRepo: {probeHeadKey: "c"}}
	err := judgeMirrorsKept(before, after)
	if err == nil || !strings.Contains(err.Error(), syncMovingRepo+" moved from a to c") || !strings.Contains(err.Error(), syncPrivateRepo+" is gone") {
		t.Fatalf("judgeMirrorsKept = %v", err)
	}
}

func TestWorkspaceManagerFlagJudges(t *testing.T) {
	args := []string{"workspace-manager", "run", sessionsKagentArg + "kagent-controller.kagent.svc:8083", sessionsCleanupArg + "12m"}
	if err := judgeKagentAddress(args); err != nil {
		t.Fatal(err)
	}
	if err := judgeKagentAddress(args[:2]); err == nil || !strings.Contains(err.Error(), "KagentNotConfigured") {
		t.Fatalf("no kagent: %v", err)
	}
	d, err := judgeCleanupWindow(args, 5*time.Minute, 15*time.Minute)
	if err != nil || d != 12*time.Minute {
		t.Fatalf("judgeCleanupWindow = %s, %v", d, err)
	}
	for _, window := range []string{"720h", "1m"} {
		if _, err := judgeCleanupWindow([]string{sessionsCleanupArg + window}, 5*time.Minute, 15*time.Minute); err == nil || !strings.Contains(err.Error(), "--workspaces-cleanup-after") {
			t.Errorf("window %s: %v", window, err)
		}
	}
	if _, err := judgeCleanupWindow(args[:2], 5*time.Minute, 15*time.Minute); err == nil {
		t.Error("no window flag judged fine")
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

func TestWorkspacesTestOptionsSessions(t *testing.T) {
	for name, c := range map[string]struct {
		opts WorkspacesTestOptions
		want string
	}{
		"sessions part":           {WorkspacesTestOptions{Part: WorkspacesPartSessions, Sessions: 10}, ""},
		"default count":           {WorkspacesTestOptions{Part: WorkspacesPartSessions}, ""},
		"too few":                 {WorkspacesTestOptions{Part: WorkspacesPartSessions, Sessions: 2}, "needs 3 or more"},
		"another part":            {WorkspacesTestOptions{Part: WorkspacesPartSync, Sessions: 4}, "--sessions is the sessions part's"},
		"an email":                {WorkspacesTestOptions{Part: WorkspacesPartSessions, Email: accessDefaultSecond}, "takes no [email]"},
		"storage only with part":  {WorkspacesTestOptions{Part: WorkspacesPartSessions, StorageOnly: true}, "proves no storage"},
		"unknown part still says": {WorkspacesTestOptions{Part: "nope"}, "sessions"},
	} {
		err := c.opts.validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: validate = %v, want %q", name, err, c.want)
		}
	}
}
