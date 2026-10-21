package lab

import (
	"net/url"
	"strings"
	"testing"
)

// refusalNoticeBlocks is a refusal notice as klaus-gateway posts it: the
// header, the reason with the destination in code, the context; verbatim
// says how the reason's text object is sent.
func refusalNoticeBlocks(agent, destination string, verbatim bool) []any {
	reason := map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "`" + destination + "` is not one of the hosts *" + agent + "* may reach.\n1 request in 1 Session."}
	if verbatim {
		reason[slackVerbatim] = true
	}
	return []any{
		map[string]any{fieldTypeKey: slackBlockHeader, slackKeyText: map[string]any{fieldTypeKey: slackPlainText, slackKeyText: refusalNoticeTitle + agent}},
		map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: reason},
		map[string]any{fieldTypeKey: slackBlockContext, slackKeyElements: []any{map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "You get this because you created *" + agent + "*."}}},
	}
}

// TestFakeSlackRefusalNotice: users.lookupByEmail answers the lowest id of
// the people sharing an address, an unknown address users_not_found; the
// notice posted to that user lands in their direct message with its blocks
// as sent, verbatim flag included, and the proof's readers find exactly it.
func TestFakeSlackRefusalNotice(t *testing.T) {
	const email = "person@lab.local"
	emails := map[string]string{"UTESTP": email, "UTESTC": email, "UTESTS": "stranger@lab.local"}
	f := startTestFake(t, emails)

	got := callFake(t, f, slackUsersByEmail, url.Values{"email": {email}}, nil)
	user, _ := got[slackKeyUser].(map[string]any)
	if got["ok"] != true || user["id"] != "UTESTC" {
		t.Fatalf("users.lookupByEmail = %v, want UTESTC, the lowest id carrying %s", got, email)
	}
	if id, ok := slackUserByEmail(emails, email); !ok || id != "UTESTC" {
		t.Fatalf("slackUserByEmail = %q %v, want the id the fake answers", id, ok)
	}
	if got := callFake(t, f, slackUsersByEmail, url.Values{"email": {"nobody@lab.local"}}, nil); got["ok"] != false || got["error"] != "users_not_found" {
		t.Fatalf("users.lookupByEmail of an unknown address = %v", got)
	}

	const agent, destination = "agentlab-refusal-x1", "proxy.golang.org:443"
	posted := callFake(t, f, slackPostMessage, nil, map[string]any{
		slackKeyChannel: "UTESTC", slackKeyText: refusalNoticeTitle + agent + ": proxy.golang.org", slackKeyBlocks: refusalNoticeBlocks(agent, destination, true),
	})
	if posted[slackKeyChannel] != "DTESTC" {
		t.Fatalf("a post to UTESTC answered channel %v, want its direct message DTESTC", posted[slackKeyChannel])
	}
	callFake(t, f, slackPostMessage, nil, map[string]any{slackKeyChannel: "UTESTC", slackKeyText: "something else"})

	notices := refusalNotices(f.thread(directChannel("UTESTC"), ""), agent)
	if len(notices) != 1 {
		t.Fatalf("refusal notices in DTESTC = %d, want 1: %s", len(notices), threadLine(f.thread("DTESTC", "")))
	}
	if err := assertRefusalNotice(notices[0], agent, destination); err != nil {
		t.Fatal(err)
	}
	if len(refusalNotices(f.thread("DTESTC", ""), "agentlab-refusal-other")) != 0 {
		t.Fatal("a notice of another agent must not count")
	}
}

// TestAssertRefusalNotice: a destination Slack would link — the mrkdwn not
// verbatim, or already a link — fails naming it, as does a notice without
// the destination.
func TestAssertRefusalNotice(t *testing.T) {
	const agent, destination = "agentlab-refusal-x1", "proxy.golang.org:443"
	notice := func(blocks []any) slackMessage {
		return slackMessage{Method: slackPostMessage, Blocks: paramBlocks(blocks)}
	}
	if err := assertRefusalNotice(notice(refusalNoticeBlocks(agent, destination, true)), agent, destination); err != nil {
		t.Fatalf("the verbatim notice: %v", err)
	}
	for name, tc := range map[string]struct {
		blocks []any
		want   string
	}{
		"not verbatim": {refusalNoticeBlocks(agent, destination, false), "verbatim"},
		"linked": {[]any{map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{
			fieldTypeKey: slackMrkdwn, slackVerbatim: true, slackKeyText: "`" + destination + "` <http://" + destination + ">",
		}}}, "link"},
		"other host": {refusalNoticeBlocks(agent, "example.org:443", true), "no section"},
	} {
		err := assertRefusalNotice(notice(tc.blocks), agent, destination)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", name, err, tc.want)
		}
	}
}
