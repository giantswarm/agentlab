package lab

import (
	"net/url"
	"strings"
	"testing"
)

// refusalNoticeBlocks is a refusal notice as klaus-gateway posts it before
// 4.16.20: the header, the reason with the destination in code, the context;
// verbatim says how the reason's text object is sent.
func refusalNoticeBlocks(agent, destination string, verbatim bool) []any {
	reason := map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "`" + destination + "` is not one of the hosts *" + agent + "* may reach.\n1 request in 1 Session."}
	if verbatim {
		reason[slackVerbatim] = true
	}
	return []any{
		map[string]any{fieldTypeKey: slackBlockHeader, slackKeyText: map[string]any{fieldTypeKey: slackPlainText, slackKeyText: refusalNoticeTitleEgress + agent}},
		map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: reason},
		map[string]any{fieldTypeKey: slackBlockContext, slackKeyElements: []any{map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "You get this because you created *" + agent + "*."}}},
	}
}

// refusalNoticeBlocksOutgoing is a refusal notice as klaus-gateway posts it
// from 4.16.20 on: the header names the host, the section the agent, the
// context's technical details the destination in code.
func refusalNoticeBlocksOutgoing(agent, destination string, verbatim bool) []any {
	host, _, _ := strings.Cut(destination, ":")
	details := map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "Technical details: destination `" + destination + " (https)`, origin `https://" + host + "`"}
	if verbatim {
		details[slackVerbatim] = true
	}
	return []any{
		map[string]any{fieldTypeKey: slackBlockHeader, slackKeyText: map[string]any{fieldTypeKey: slackPlainText, slackKeyText: refusalNoticeTitleOutgoing + host + " blocked"}},
		map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{fieldTypeKey: slackMrkdwn, slackVerbatim: true,
			slackKeyText: agent + " is not allowed to reach this address · 1 attempt in 1 Session\nYou can allow it: add `https://" + host + "` to " + agent + "’s Extra egress origins."}},
		map[string]any{fieldTypeKey: slackBlockContext, slackKeyElements: []any{
			details,
			map[string]any{fieldTypeKey: slackMrkdwn, slackVerbatim: true, slackKeyText: "You get this because you created *" + agent + "*."},
		}},
	}
}

// TestFakeSlackRefusalNotice: users.lookupByEmail answers the lowest id of
// the people sharing an address, an unknown address users_not_found; the
// notice posted to that user lands in their direct message with its blocks
// as sent, verbatim flag included, and the proof's readers find exactly it.
func TestFakeSlackRefusalNotice(t *testing.T) {
	const email, lowest = "person@lab.local", "UTESTC"
	emails := map[string]string{"UTESTP": email, lowest: email, "UTESTS": "stranger@lab.local"}
	f := startTestFake(t, emails)

	got := callFake(t, f, slackUsersByEmail, url.Values{emailClaim: {email}}, nil)
	user, _ := got[slackKeyUser].(map[string]any)
	if got["ok"] != true || user["id"] != lowest {
		t.Fatalf("users.lookupByEmail = %v, want UTESTC, the lowest id carrying %s", got, email)
	}
	if id, ok := slackUserByEmail(emails, email); !ok || id != lowest {
		t.Fatalf("slackUserByEmail = %q %v, want the id the fake answers", id, ok)
	}
	if got := callFake(t, f, slackUsersByEmail, url.Values{emailClaim: {"nobody@lab.local"}}, nil); got["ok"] != false || got["error"] != "users_not_found" {
		t.Fatalf("users.lookupByEmail of an unknown address = %v", got)
	}

	const agent, destination = "agentlab-refusal-x1", "proxy.golang.org:443"
	posted := callFake(t, f, slackPostMessage, nil, map[string]any{
		slackKeyChannel: lowest, slackKeyText: refusalNoticeTitleEgress + agent + ": proxy.golang.org", slackKeyBlocks: refusalNoticeBlocks(agent, destination, true),
	})
	if posted[slackKeyChannel] != "DTESTC" {
		t.Fatalf("a post to UTESTC answered channel %v, want its direct message DTESTC", posted[slackKeyChannel])
	}
	callFake(t, f, slackPostMessage, nil, map[string]any{slackKeyChannel: lowest, slackKeyText: "something else"})

	notices := refusalNotices(f.thread(directChannel(lowest), ""), agent)
	if len(notices) != 1 {
		t.Fatalf("refusal notices in DTESTC = %d, want 1: %s", len(notices), threadLine(f.thread("DTESTC", "")))
	}
	if err := assertRefusalNotice(notices[0], agent, destination); err != nil {
		t.Fatal(err)
	}
	if len(refusalNotices(f.thread("DTESTC", ""), "agentlab-refusal-other")) != 0 {
		t.Fatal("a notice of another agent must not count")
	}

	const agentNow = "agentlab-refusal-x2"
	callFake(t, f, slackPostMessage, nil, map[string]any{
		slackKeyChannel: lowest, slackKeyText: refusalNoticeTitleOutgoing + "proxy.golang.org blocked (" + agentNow + ")", slackKeyBlocks: refusalNoticeBlocksOutgoing(agentNow, destination, true),
	})
	notices = refusalNotices(f.thread(directChannel(lowest), ""), agentNow)
	if len(notices) != 1 {
		t.Fatalf("refusal notices of %s in DTESTC = %d, want 1: %s", agentNow, len(notices), threadLine(f.thread("DTESTC", "")))
	}
	if err := assertRefusalNotice(notices[0], agentNow, destination); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{agent: 1, agentNow: 1, "agentlab-refusal-x": 0, "agentlab-refusal-other": 0} {
		if got := len(refusalNotices(f.thread("DTESTC", ""), name)); got != want {
			t.Errorf("refusal notices of %s with both posted = %d, want %d: one agent's name inside another's is no match", name, got, want)
		}
	}
}

// TestAssertRefusalNotice: a destination Slack would link — the mrkdwn not
// verbatim, or already a link — fails naming it, as does a notice without
// the destination; both wordings of the notice pass.
func TestAssertRefusalNotice(t *testing.T) {
	const agent, destination = "agentlab-refusal-x1", "proxy.golang.org:443"
	notice := func(blocks []any) slackMessage {
		return slackMessage{Method: slackPostMessage, Blocks: paramBlocks(blocks)}
	}
	if err := assertRefusalNotice(notice(refusalNoticeBlocks(agent, destination, true)), agent, destination); err != nil {
		t.Fatalf("the verbatim notice: %v", err)
	}
	outgoingWith := func(change func([]any)) []any {
		blocks := refusalNoticeBlocksOutgoing(agent, destination, true)
		change(blocks)
		return blocks
	}
	if err := assertRefusalNotice(notice(refusalNoticeBlocksOutgoing(agent, destination, true)), agent, destination); err != nil {
		t.Fatalf("the verbatim notice of 4.16.20: %v", err)
	}
	for name, tc := range map[string]struct {
		blocks []any
		want   string
	}{
		"not verbatim":         {refusalNoticeBlocks(agent, destination, false), slackVerbatim},
		"details not verbatim": {refusalNoticeBlocksOutgoing(agent, destination, false), slackVerbatim},
		"section not verbatim": {outgoingWith(func(blocks []any) {
			blocks[1].(map[string]any)[slackKeyText].(map[string]any)[slackVerbatim] = false
		}), slackVerbatim},
		"section linked": {outgoingWith(func(blocks []any) {
			text := blocks[1].(map[string]any)[slackKeyText].(map[string]any)
			text[slackKeyText] = text[slackKeyText].(string) + " <https://proxy.golang.org>"
		}), "link"},
		"raw TCP details": {outgoingWith(func(blocks []any) {
			el := blocks[2].(map[string]any)[slackKeyElements].([]any)[0].(map[string]any)
			el[slackKeyText] = "Technical details: destination `" + destination + " (tcp)`"
		}), "no context"},
		"destination in the section only": {append(outgoingWith(func([]any) {})[:2], refusalNoticeBlocks(agent, destination, true)[1]), "no context"},
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
