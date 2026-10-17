package lab

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

func TestWriteCommandsParse(t *testing.T) {
	for name, command := range map[string]string{
		"write":               writeCommand("github-bearer.example", writeRepo, "agent/agentlab-write-x"),
		"without a workspace": writeBareCommand("github-bearer.example", writeRepo, "agent/agentlab-write-x-bare"),
	} {
		// #nosec G204 -- the test's own command, only parsed
		if out, err := exec.Command("sh", "-n", "-c", command).CombinedOutput(); err != nil {
			t.Errorf("%s: sh -n: %v: %s\n%s", name, err, out, command)
		}
	}
}

func TestParseWriteTurn(t *testing.T) {
	reply := probeMarker + " clone=./agentlab-org/platform-api\n" + probeMarker + " push=0:\n" + probeMarker + " pr=0:https://github-bearer.example/agentlab-org/platform-api/pull/1\n" +
		probeMarker + " read-only=128:remote:_Permission_to_agentlab-org/handbook.git_denied_to_admin.\n" + probeMarker + " uninstalled=128:x\n" +
		probeMarker + " gh-token=set\n" + probeMarker + " token-env=\n" + probeMarker + " token-files=`/workspace/x /data/y`\n"
	v, err := parseWriteTurn(reply, "clone", "push", "pr", "read-only", "uninstalled", "token-env", "token-files")
	if err != nil {
		t.Fatal(err)
	}
	if v.push != "0:" || v.readOnly == "" || v.ghToken != "set" || len(v.env) != 0 || strings.Join(v.files, ",") != "/workspace/x,/data/y" {
		t.Fatalf("parsed %+v", v)
	}
	if _, err := parseWriteTurn("no markers", "push"); err == nil {
		t.Fatal("a reply without markers parsed")
	}
}

func TestJudgeWrite(t *testing.T) {
	const host, login, branch = "github-bearer.example", testLogin, "agent/x"
	good := []githubRequest{
		{Host: host, Method: http.MethodPost, Path: "/" + writeRepo + ".git/" + gitReceivePack, Status: 200, User: login, Credential: credUser},
		{Host: host, Method: http.MethodGet, Path: "/" + writeReadOnlyRepo + ".git/info/refs", Status: 403, User: login, Credential: credUser},
		{Host: host, Method: http.MethodGet, Path: "/" + writeUninstalledRepo + ".git/info/refs", Status: 403, User: login, Credential: credUser},
	}
	pulls := []workspacePull{{Number: 1, Head: branch, Author: login}}
	v := writeValues{clone: "./" + writeRepo, push: "0:", pr: "0:url", readOnly: "128:denied", uninstalled: "128:denied"}
	if _, err := judgeWrite(v, good, pulls, host, login, branch); err != nil {
		t.Fatal(err)
	}
	withV := func(f func(*writeValues)) writeValues { c := v; f(&c); return c }
	for name, tc := range map[string]struct {
		v      writeValues
		logged []githubRequest
		pulls  []workspacePull
		want   string
	}{
		"no clone":             {withV(func(c *writeValues) { c.clone = writeNoClone }), good, pulls, "no clone"},
		"a token in a file":    {withV(func(c *writeValues) { c.files = []string{"/workspace/x"} }), good, pulls, "a token in the sandbox"},
		"a token in the env":   {withV(func(c *writeValues) { c.env = []string{"GH_TOKEN"} }), good, pulls, "a token in the sandbox"},
		"push refused":         {withV(func(c *writeValues) { c.push = "128:401" }), good, pulls, "git push"},
		"push unauthenticated": {v, append([]githubRequest{{Host: host, Path: "/" + writeRepo + ".git/" + gitReceivePack, Status: 401, Credential: credNone}}, good[1:]...), pulls, "egress gateway set no credential"},
		"no pull request":      {v, good, nil, "no pull request"},
		"someone else's pull":  {v, good, []workspacePull{{Number: 1, Head: branch, Author: "dev"}}, "someone else's credential"},
		"read-only pushed":     {withV(func(c *writeValues) { c.readOnly = "0:" }), good, pulls, writeReadOnlyRepo},
		"uninstalled no 403":   {v, good[:2], pulls, writeUninstalledRepo},
	} {
		_, err := judgeWrite(tc.v, tc.logged, tc.pulls, host, login, branch)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestJudgeBare(t *testing.T) {
	if err := judgeBare(writeValues{push: "128:auth"}, []githubRequest{{Credential: credNone, Status: 401}}, "s"); err != nil {
		t.Error(err)
	}
	if err := judgeBare(writeValues{push: "128:x"}, []githubRequest{{Credential: credUser, User: testLogin}}, "s"); err == nil {
		t.Error("a user token without a workspace passed")
	}
	if err := judgeBare(writeValues{push: "0:"}, nil, "s"); err == nil {
		t.Error("a push without a workspace passed")
	}
}
