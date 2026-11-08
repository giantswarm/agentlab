package lab

import (
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
)

// The answer to `@bot login`: the Sign in button wins over anything else in
// the thread, another person's ephemeral is not the person's, and the
// confirmation and the transient failure are told apart.

const (
	testPodA = "pod-a"
	testPodB = "pod-b"
)

func TestLoginReplyIn(t *testing.T) {
	signIn := []map[string]any{{slackKeyElements: []any{map[string]any{slackKeyActionID: slackActionSignIn, slackKeyURL: "https://gw/auth/slack/link?u=x"}}}}
	for _, tc := range []struct {
		name string
		msgs []slackMessage
		want loginOutcome
	}{
		{"nothing yet", nil, loginPending},
		{"signed in as", []slackMessage{{Method: slackPostEphemeral, Recipient: "U1", Text: "Signed in as " + testPortalUser + "."}}, loginLinked},
		{"signed in", []slackMessage{{Method: slackPostEphemeral, Recipient: "U1", Text: "Signed in."}}, loginLinked},
		{"another person's confirmation", []slackMessage{{Method: slackPostEphemeral, Recipient: "U2", Text: "Signed in as dev@lab.local."}}, loginPending},
		{"the Sign in button", []slackMessage{
			{Method: slackPostMessage, Text: "Waiting for <@U1> to sign in"},
			{Method: slackPostEphemeral, Recipient: "U1", Blocks: signIn},
		}, loginSignedOut},
		{"the Sign in button after a confirmation", []slackMessage{
			{Method: slackPostEphemeral, Recipient: "U1", Text: "Signed in."},
			{Method: slackPostEphemeral, Recipient: "U1", Blocks: signIn},
		}, loginSignedOut},
		{"another person's Sign in button", []slackMessage{{Method: slackPostEphemeral, Recipient: "U2", Blocks: signIn}}, loginPending},
		{"transient", []slackMessage{{Method: slackPostEphemeral, Recipient: "U1", Text: "Your Giant Swarm sign-in could not be refreshed. Try again in a moment."}}, loginTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := loginReplyIn(tc.msgs, "U1"); got != tc.want {
				t.Errorf("loginReplyIn = %s, want %s", got, tc.want)
			}
		})
	}
}

// The rewrite expires the cached id_token and keeps everything else.
func TestExpiredLink(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	link := &musterlink.Link{Sub: "sub", Email: testPortalUser, RefreshToken: "rt", LinkedAt: now.Add(-time.Hour), IDToken: "id", Expiry: now.Add(time.Hour)}
	got := expiredLink(link, now)
	if !got.Expiry.Before(now) {
		t.Errorf("expiry %s, want before %s", got.Expiry, now)
	}
	if got.Sub != link.Sub || got.Email != link.Email || got.RefreshToken != link.RefreshToken || !got.LinkedAt.Equal(link.LinkedAt) || got.IDToken != link.IDToken {
		t.Errorf("expiredLink changed more than the expiry: %+v", got)
	}
	if !link.Expiry.Equal(now.Add(time.Hour)) {
		t.Errorf("expiredLink changed its argument: %s", link.Expiry)
	}
}

// A round passes only with every pod signed in and a rotated, written link.
func TestRoundVerdict(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	before := &musterlink.Link{RefreshToken: "rt-1", Expiry: now.Add(-time.Minute)}
	rotated := &musterlink.Link{RefreshToken: "rt-2", Expiry: now.Add(time.Hour)}
	linked := map[string]loginOutcome{testPodA: loginLinked, testPodB: loginLinked}
	for _, tc := range []struct {
		name    string
		after   *musterlink.Link
		answers map[string]loginOutcome
		want    string
	}{
		{"pass", rotated, linked, ""},
		{"a pod signed out", rotated, map[string]loginOutcome{testPodA: loginLinked, testPodB: loginSignedOut}, "pod " + testPodB + " answered the Sign in button"},
		{"a pod silent", rotated, map[string]loginOutcome{testPodA: loginPending, testPodB: loginLinked}, "pod " + testPodA + " answered no answer"},
		{"record gone", nil, linked, "gone from the link Secret"},
		{"no refresh token", &musterlink.Link{Expiry: now.Add(time.Hour)}, linked, "holds no refresh token"},
		{"not rotated", &musterlink.Link{RefreshToken: "rt-1", Expiry: now.Add(time.Hour)}, linked, "was not rotated"},
		{"still expired", &musterlink.Link{RefreshToken: "rt-2", Expiry: now.Add(-time.Minute)}, linked, "expired at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := roundVerdict(before, tc.after, tc.answers, now)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("roundVerdict = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("roundVerdict = %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "rt-") {
				t.Errorf("the verdict names a refresh token: %v", err)
			}
		})
	}
}

// Every reuse record mcp-oauth writes is found, nothing else.
func TestReuseDetections(t *testing.T) {
	logs := strings.Join([]string{
		`{"time":"2026-11-06T12:00:00Z","level":"INFO","msg":"tools/call request"}`,
		`{"time":"2026-11-06T12:00:01Z","level":"ERROR","msg":"Refresh token reuse detected - token was rotated but still being used","user_id":"u"}`,
		`{"time":"2026-11-06T12:00:01Z","level":"INFO","msg":"security_audit","audit":{"event_type":"refresh_token_reuse_detected"}}`,
		`{"time":"2026-11-06T12:00:02Z","level":"INFO","msg":"security_audit","audit":{"event_type":"revoked_token_family_reuse_attempt"}}`,
		`{"time":"2026-11-06T12:00:02Z","level":"ERROR","msg":"Attempted use of revoked token family"}`,
		`{"time":"2026-11-06T12:00:03Z","level":"INFO","msg":"token refreshed","rotated":true}`,
	}, "\n")
	if got := reuseDetections(logs); len(got) != 4 {
		t.Fatalf("reuseDetections found %d lines, want 4: %v", len(got), got)
	}
	if got := reuseDetections(`{"msg":"token refreshed"}`); len(got) != 0 {
		t.Errorf("reuseDetections on a clean log = %v", got)
	}
}

// The gateway's failed refresh records for the person, not another's or a
// successful one.
func TestRefreshFailures(t *testing.T) {
	logs := strings.Join([]string{
		`{"level":"WARN","msg":"musterlink: id_token refresh failed","record":"token_refresh","trigger":"turn","slackUser":"UAGENTLABTESTXR","error":"invalid_grant"}`,
		`{"level":"INFO","msg":"musterlink: id_token refreshed","record":"token_refresh","trigger":"turn","slackUser":"UAGENTLABTESTXR"}`,
		`{"level":"WARN","msg":"musterlink: id_token refresh failed","record":"token_refresh","trigger":"turn","slackUser":"UOTHER"}`,
	}, "\n")
	if got := refreshFailures(logs, "UAGENTLABTESTXR"); len(got) != 1 || !strings.Contains(got[0], "invalid_grant") {
		t.Fatalf("refreshFailures = %v, want the one failure of the person", got)
	}
}

// The lab's hostnames on the https port go to the edge's published port;
// everything else is dialled as given.
func TestEdgeDialAddr(t *testing.T) {
	const (
		domain    = "127.0.0.1.nip.io"
		edgeHTTPS = "agentgateway.127.0.0.1.nip.io:443"
		labEdge   = "8443"
	)
	for _, tc := range []struct {
		addr, edgePort, want string
	}{
		{edgeHTTPS, labEdge, "agentgateway.127.0.0.1.nip.io:8443"},
		{"127.0.0.1.nip.io:443", labEdge, "127.0.0.1.nip.io:8443"},
		{edgeHTTPS, "443", edgeHTTPS},
		{"muster.127.0.0.1.nip.io:8443", labEdge, "muster.127.0.0.1.nip.io:8443"},
		{"localhost:5556", labEdge, "localhost:5556"},
		{"example.com:443", labEdge, "example.com:443"},
		{"evil127.0.0.1.nip.io:443", labEdge, "evil127.0.0.1.nip.io:443"},
	} {
		if got := edgeDialAddr(tc.addr, domain, tc.edgePort); got != tc.want {
			t.Errorf("edgeDialAddr(%s, edge %s) = %s, want %s", tc.addr, tc.edgePort, got, tc.want)
		}
	}
	if got := edgeDialAddr("agentgateway.x:443", "", labEdge); got != "agentgateway.x:443" {
		t.Errorf("edgeDialAddr outside a lab = %s", got)
	}
}

// The sign-in ends on the callback's code-free landing, not on the callback
// with a code or an error.
func TestSignedInLanding(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://agentgateway.lab/auth/slack/callback?done=abc":          true,
		"https://agentgateway.lab/auth/slack/callback?code=c&state=s":    false,
		"https://agentgateway.lab/auth/slack/callback?error=access_deny": false,
		"https://agentgateway.lab/auth/slack/link?done=abc":              false,
	} {
		end, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := signedInLanding(end, nil); got != want {
			t.Errorf("signedInLanding(%s) = %v, want %v", raw, got, want)
		}
	}
}

// Every fire runs once, none before all are ready, and each error lands at
// its index.
func TestFireTogether(t *testing.T) {
	errTest := errors.New("fire failed")
	var started atomic.Int32
	errs := fireTogether(3, func(i int) error {
		started.Add(1)
		if i == 1 {
			return errTest
		}
		return nil
	})
	if started.Load() != 3 {
		t.Fatalf("%d fires ran, want 3", started.Load())
	}
	if errs[0] != nil || errs[1] != errTest || errs[2] != nil {
		t.Errorf("errors %v, want only the second", errs)
	}
}
