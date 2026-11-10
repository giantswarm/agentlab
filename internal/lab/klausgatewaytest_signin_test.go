package lab

import (
	"strings"
	"testing"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
)

// The sign-in prompt is the person's ephemeral with Sign in and Not now: not
// another person's, not a Connect prompt without Not now, not one before the
// turn, and the last of several.
func TestSignInPromptIn(t *testing.T) {
	button := func(id string) map[string]any { return map[string]any{slackKeyActionID: id} }
	actions := func(ids ...string) []map[string]any {
		var elements []any
		for _, id := range ids {
			elements = append(elements, button(id))
		}
		return []map[string]any{{slackKeyElements: elements}}
	}
	msgs := []slackMessage{
		{TS: "1", Method: slackPostEphemeral, Recipient: "U1", Blocks: actions(slackActionConnect, slackActionSignInDecline)},
		{TS: "2", Method: slackPostMessage, Text: "Waiting for <@U1> to sign in"},
		{TS: "3", Method: slackPostEphemeral, Recipient: "U2", Blocks: actions(slackActionConnect, slackActionSignInDecline)},
		{TS: "4", Method: slackPostEphemeral, Recipient: "U1", Blocks: actions(slackActionConnect)},
		{TS: "5", Method: slackPostEphemeral, Recipient: "U1", Blocks: actions(slackActionConnect, slackActionSignInDecline)},
	}
	if m, ok := signInPromptIn(msgs, 1, "U1"); !ok || m.TS != "5" {
		t.Errorf("signInPromptIn after 1 = %s, %v; want the prompt 5", m.TS, ok)
	}
	if _, ok := signInPromptIn(msgs[:5], 1, "U2"); !ok {
		t.Error("signInPromptIn for U2 found nothing; want the prompt 3")
	}
	if m, ok := signInPromptIn(msgs[:4], 1, "U1"); ok {
		t.Errorf("signInPromptIn after 1 without 5 = %s; want none: 4 has no Not now, 1 is before the turn", m.TS)
	}
}

// The prompt shows the link's domain in bold and the whole link in code,
// escaped as Slack mrkdwn, as klaus-gateway's signInLinkText writes it.
func TestSignInLinkShown(t *testing.T) {
	link := "https://muster.example:8443/oauth/proxy/start?state=a&b=<1>"
	want := "Opens *muster.example*\n`https://muster.example:8443/oauth/proxy/start?state=a&amp;b=&lt;1&gt;`"
	if got := signInLinkShown(link); got != want {
		t.Errorf("signInLinkShown = %q, want %q", got, want)
	}
}

// Before the sign-in completes, an accepted task still waiting on the request
// it asked in is not parked yet, one working on the retry is, and one that
// ended or asked again is a runtime that did not wait.
func TestRetryParked(t *testing.T) {
	const asksAgain, ended = "asks again", "ended"
	task := func(state a2a.TaskState, message string) *a2a.Task {
		task := &a2a.Task{ID: "t1", Status: a2a.TaskStatus{State: state}}
		if message != "" {
			task.Status.Message = &a2a.Message{ID: message}
		}
		return task
	}
	for _, tc := range []struct {
		name    string
		task    *a2a.Task
		parked  bool
		failure string
	}{
		{"still on the request", task(a2a.TaskStateInputRequired, "m1"), false, ""},
		{"working on the retry", task(a2a.TaskStateWorking, "m2"), true, ""},
		{"submitted", task(a2a.TaskStateSubmitted, ""), true, ""},
		{"a new request", task(a2a.TaskStateInputRequired, "m2"), false, asksAgain},
		{"asks without a message", task(a2a.TaskStateInputRequired, ""), false, asksAgain},
		{"auth required anew", task(a2a.TaskStateAuthRequired, "m3"), false, asksAgain},
		{"completed", task(a2a.TaskStateCompleted, "m2"), false, ended},
		{"a failure", task(a2a.TaskStateFailed, "m2"), false, ended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parked, err := retryParked(tc.task, "m1")
			if parked != tc.parked {
				t.Errorf("retryParked = %t, want %t", parked, tc.parked)
			}
			switch {
			case tc.failure == "" && err != nil:
				t.Errorf("retryParked: %v, want no error", err)
			case tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)):
				t.Errorf("retryParked error = %v, want one saying %q", err, tc.failure)
			}
		})
	}
}

// --hold belongs to the signin part and must leave the sign-in inside
// muster's wait.
func TestCheckSignInHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    KlausGatewayTestOptions
		failure string
	}{
		{"unset", KlausGatewayTestOptions{}, ""},
		{"unset on another part", KlausGatewayTestOptions{Part: KlausGatewayPartRefresh}, ""},
		{"past the Harness MCP timeout", KlausGatewayTestOptions{Part: KlausGatewayPartSignIn, Hold: 45 * time.Second}, ""},
		{"the longest", KlausGatewayTestOptions{Part: KlausGatewayPartSignIn, Hold: signInMaxHold}, ""},
		{"on the whole proof", KlausGatewayTestOptions{Hold: time.Minute}, "applies to --part signin alone"},
		{"negative", KlausGatewayTestOptions{Part: KlausGatewayPartSignIn, Hold: -time.Second}, "positive"},
		{"outlasts muster's wait", KlausGatewayTestOptions{Part: KlausGatewayPartSignIn, Hold: signInMaxHold + time.Second}, "outlasts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSignInHold(tc.opts)
			switch {
			case tc.failure == "" && err != nil:
				t.Errorf("checkSignInHold: %v, want none", err)
			case tc.failure != "" && (err == nil || !strings.Contains(err.Error(), tc.failure)):
				t.Errorf("checkSignInHold = %v, want an error saying %q", err, tc.failure)
			}
		})
	}
}

func TestSignInHoldDefault(t *testing.T) {
	if got := (KlausGatewayTestOptions{}).withDefaults().Hold; got != signInDefaultHold {
		t.Errorf("default hold = %s, want %s", got, signInDefaultHold)
	}
	if got := (KlausGatewayTestOptions{Hold: time.Minute}).withDefaults().Hold; got != time.Minute {
		t.Errorf("hold = %s, want the 1m0s given", got)
	}
}
