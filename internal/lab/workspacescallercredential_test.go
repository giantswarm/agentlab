package lab

import (
	"strings"
	"testing"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

func TestJudgeCallerCredential(t *testing.T) {
	const aud, session, wrongClaims = "fake-bearer", "s1", "want aud fake-bearer"
	jwt := func(aud []string, sub string) githubRequest {
		return githubRequest{Credential: credJWT, Audience: aud, Subject: sub, Status: 401}
	}
	for name, tc := range map[string]struct {
		logged []githubRequest
		want   string
	}{
		"the Actor's JWT":       {[]githubRequest{jwt([]string{aud}, "atespaces:kagent:actors:session-s1")}, ""},
		"nothing reached it":    {nil, "never reached the lab's GitHub"},
		"the placeholder":       {[]githubRequest{{Credential: credPlaceholder}}, "the credential placeholder, not the Session Actor's JWT"},
		"another audience":      {[]githubRequest{jwt([]string{"fake"}, "atespaces:kagent:actors:session-s1")}, wrongClaims},
		"another Session":       {[]githubRequest{jwt([]string{aud}, "atespaces:kagent:actors:session-s2")}, wrongClaims},
		"a subject of no Actor": {[]githubRequest{jwt([]string{aud}, "session-s1")}, wrongClaims},
	} {
		_, err := judgeCallerCredential(tc.logged, "401", aud, session)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if err := judgeNoCallerCredential([]githubRequest{{Credential: credPlaceholder}}, session); err != nil {
		t.Errorf("a placeholder without a workspace: %v", err)
	}
	if err := judgeNoCallerCredential(nil, session); err != nil {
		t.Errorf("nothing without a workspace: %v", err)
	}
	if err := judgeNoCallerCredential([]githubRequest{jwt([]string{aud}, "x")}, session); err == nil {
		t.Error("a JWT without a workspace passed")
	}
}

func TestJudgeSessionProviders(t *testing.T) {
	const api, aud = "api.github-bearer.example", "fake-bearer"
	git := &apiv1alpha1.SessionProvider{Hostname: "github-bearer.example", Scheme: "Basic", Audience: aud, Username: "x-access-token"}
	if err := judgeSessionProviders([]*apiv1alpha1.SessionProvider{git, {Hostname: api, Scheme: "Bearer", Audience: aud}}, api, aud); err != nil {
		t.Error(err)
	}
	if err := judgeSessionProviders([]*apiv1alpha1.SessionProvider{git}, api, aud); err == nil || !strings.Contains(err.Error(), "no Bearer API host") {
		t.Errorf("the git host alone: %v", err)
	}
	if err := judgeSessionProviders([]*apiv1alpha1.SessionProvider{{Hostname: api, Scheme: "Basic", Audience: aud}}, api, aud); err == nil {
		t.Error("a Basic API host passed")
	}
}

func TestParseCallerCredentialTurn(t *testing.T) {
	v, err := parseCallerCredentialTurn("AGENTLAB_PROBE pwd=/workspace/s\nAGENTLAB_PROBE api-status=401\nAGENTLAB_PROBE jwt-env=\nAGENTLAB_PROBE jwt-files=/data/x /workspace/y \n")
	if err != nil || v.status != "401" || len(v.env) != 0 || strings.Join(v.files, ",") != "/data/x,/workspace/y" {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := parseCallerCredentialTurn("I won't run that"); err == nil {
		t.Error("a reply without the lines parsed")
	}
	cmd := callerCredentialCommand("https://api.example", "tok")
	for _, want := range []string{"'https://api.example/agentlab/caller-credential/tok'", "Authorization: Bearer agentlab-placeholder", "/data /workspace"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command lacks %q: %s", want, cmd)
		}
	}
}
