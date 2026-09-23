package lab

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// callFake posts one Web API call to the fake the way the gateway does:
// params form-encoded, or body as JSON.
func callFake(t *testing.T, f *fakeSlack, method string, params url.Values, body map[string]any) map[string]any {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		raw, _ := json.Marshal(body)
		req, err = http.NewRequest(http.MethodPost, f.baseURL()+"/"+method, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	} else {
		req, err = http.NewRequest(http.MethodPost, f.baseURL()+"/"+method, strings.NewReader(params.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func startTestFake(t *testing.T, emails map[string]string) *fakeSlack {
	t.Helper()
	f, err := startFakeSlack("127.0.0.1:0", emails)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.close)
	return f
}

// TestFakeSlackThread: auth.test names the bot, users.info the person's
// e-mail; a form post, a JSON stream (start, append, stop) with the agent's
// name and icon, an ephemeral, a rewrite and a delete land in the thread as
// a person would see it, in order, every message with a distinct ts.
func TestFakeSlackThread(t *testing.T) {
	f := startTestFake(t, map[string]string{"UP": testUser})
	resp, err := http.Get("http://" + f.listener.Addr().String() + slackHealthPath)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET %s = %d %q", slackHealthPath, resp.StatusCode, body)
	}
	_ = resp.Body.Close()
	if who := callFake(t, f, slackAuthTest, url.Values{}, nil); who["user_id"] != slackFakeBotUser || who[slackKeyTeamID] != slackFakeTeam {
		t.Errorf("auth.test = %v", who)
	}
	info := callFake(t, f, slackUsersInfo, url.Values{slackKeyUser: {"UP"}}, nil)
	profile, _ := info[slackKeyUser].(map[string]any)["profile"].(map[string]any)
	if profile["email"] != testUser {
		t.Errorf("users.info = %v", info)
	}

	root := f.nextTS()
	notice := callFake(t, f, slackPostMessage, url.Values{slackKeyChannel: {"C1"}, slackKeyThreadTS: {root}, slackKeyText: {"_thinking…_"}}, nil)
	stream := callFake(t, f, slackStartStream, nil, map[string]any{slackKeyChannel: "C1", slackKeyThreadTS: root, "username": "Agent", "icon_url": "https://i/x.png",
		slackKeyChunks: []any{map[string]any{fieldTypeKey: "task_update", "id": "step-1", "title": "list"}, map[string]any{fieldTypeKey: slackMarkdownChunk, slackKeyText: "po"}}})
	callFake(t, f, slackAppendStream, nil, map[string]any{slackKeyChannel: "C1", slackKeyTS: stream[slackKeyTS], slackKeyChunks: []any{map[string]any{fieldTypeKey: slackMarkdownChunk, slackKeyText: "n"}}})
	callFake(t, f, slackStopStream, nil, map[string]any{slackKeyChannel: "C1", slackKeyTS: stream[slackKeyTS], slackKeyChunks: []any{map[string]any{fieldTypeKey: slackMarkdownChunk, slackKeyText: "g"}}})
	eph := callFake(t, f, slackPostEphemeral, nil, map[string]any{slackKeyChannel: "C1", slackKeyThreadTS: root, slackKeyUser: "UP", slackKeyText: "sign in",
		slackKeyBlocks: []any{map[string]any{fieldTypeKey: slackBlockActions, slackKeyElements: []any{map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionSignIn, slackKeyURL: "http://gw/auth/slack/link?u=1"}}}}})
	card := callFake(t, f, slackPostMessage, nil, map[string]any{slackKeyChannel: "C1", slackKeyThreadTS: root, slackKeyText: testCardTitle,
		slackKeyBlocks: []any{map[string]any{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "*Approval required* · list"}}}})
	callFake(t, f, slackUpdate, nil, map[string]any{slackKeyChannel: "C1", slackKeyTS: card[slackKeyTS], slackKeyText: testApproved,
		slackKeyBlocks: []any{map[string]any{fieldTypeKey: "context", slackKeyElements: []any{map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: testApproved}}}}})
	callFake(t, f, slackDelete, url.Values{slackKeyChannel: {"C1"}, slackKeyTS: {notice[slackKeyTS].(string)}}, nil)

	msgs := f.thread("C1", root)
	if len(msgs) != 3 {
		t.Fatalf("thread = %s", threadLine(msgs))
	}
	if !msgs[0].streamed() || msgs[0].Text != klausGatewayWord || msgs[0].Username != "Agent" || msgs[0].IconURL != "https://i/x.png" {
		t.Errorf("stream = %+v", msgs[0])
	}
	if msgs[1].Recipient != "UP" || msgs[1].TS != eph["message_ts"] {
		t.Errorf("ephemeral = %+v (answer %v)", msgs[1], eph)
	}
	if _, ok := msgs[1].action(slackActionSignIn); !ok {
		t.Error("the ephemeral's button is not found")
	}
	if !strings.Contains(msgs[2].shown(), testApproved) || len(msgs[2].Blocks) != 1 {
		t.Errorf("rewritten card = %q", msgs[2].shown())
	}
	seen := map[string]bool{root: true}
	for _, m := range msgs {
		if seen[m.TS] {
			t.Errorf("ts %s reused", m.TS)
		}
		seen[m.TS] = true
	}
	if f.callCount(slackPostMessage) != 2 || f.callCount(slackStartStream) != 1 {
		t.Errorf("calls: postMessage %d, startStream %d", f.callCount(slackPostMessage), f.callCount(slackStartStream))
	}
	if stream, answer := streamedSince(msgs, 0); answer != klausGatewayWord || stream.TS != msgs[0].TS {
		t.Errorf("streamedSince = %q", answer)
	}
	if _, answer := streamedSince(msgs, 1); answer != "" {
		t.Errorf("streamedSince after the stream = %q", answer)
	}
}

// capturedRequest is one request a test server received.
type capturedRequest struct {
	path, contentType, timestamp, signature string
	body                                    []byte
}

// TestSlackDriver: every request carries Slack's v0 signature over its
// timestamp and body; a mention is an app_mention event with the bot's
// <@…> and the thread when it has one, a reply a plain threaded message, a
// click a form-encoded block_actions payload carrying the button's value, the
// card's ts and blocks.
func TestSlackDriver(t *testing.T) {
	var got []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, capturedRequest{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body})
	}))
	defer srv.Close()
	f := startTestFake(t, nil)
	d := newSlackDriver(srv.URL, "sekrit", f, "C1")

	root, err := d.mention("UP", "", slackAgentCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.mention("UP", root, "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reply("UP", root, slackStopCommand); err != nil {
		t.Fatal(err)
	}
	card := slackMessage{TS: "9.1", Channel: "C1", ThreadTS: root, Blocks: []map[string]any{{fieldTypeKey: slackBlockActions, slackKeyElements: []any{
		map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionApprove, slackKeyValue: `{"t":"` + root + `","id":"task-1"}`},
	}}}}
	if err := d.click("UP", card, slackActionApprove); err != nil {
		t.Fatal(err)
	}
	if err := d.click("UP", card, slackActionDeny); err == nil {
		t.Error("a button the card does not carry was clicked")
	}
	if len(got) != 4 {
		t.Fatalf("%d requests", len(got))
	}
	for _, r := range got {
		mac := hmac.New(sha256.New, []byte("sekrit"))
		mac.Write([]byte("v0:" + r.timestamp + ":" + string(r.body)))
		if r.signature != "v0="+hex.EncodeToString(mac.Sum(nil)) {
			t.Errorf("%s: signature %q", r.path, r.signature)
		}
		if ts, _ := strconv.ParseInt(r.timestamp, 10, 64); time.Since(time.Unix(ts, 0)) > time.Minute {
			t.Errorf("%s: timestamp %s", r.path, r.timestamp)
		}
	}
	event := func(i int) map[string]any {
		var cb struct {
			Type    string         `json:"type"`
			EventID string         `json:"event_id"`
			Event   map[string]any `json:"event"`
		}
		if err := json.Unmarshal(got[i].body, &cb); err != nil || got[i].path != slackEventsPath || cb.Type != "event_callback" || cb.EventID == "" {
			t.Fatalf("request %d: %s %s", i, got[i].path, got[i].body)
		}
		return cb.Event
	}
	if e := event(0); e[fieldTypeKey] != "app_mention" || e[slackKeyText] != "<@"+slackFakeBotUser+"> /agent" || e[slackKeyTS] != root || e[slackKeyThreadTS] != nil || e[slackKeyUser] != "UP" || e[slackKeyChannel] != "C1" {
		t.Errorf("root mention = %v", e)
	}
	if e := event(1); e[fieldTypeKey] != "app_mention" || e[slackKeyThreadTS] != root || e[slackKeyTS] == root {
		t.Errorf("threaded mention = %v", e)
	}
	if e := event(2); e[fieldTypeKey] != slackKeyMessage || e[slackKeyText] != slackStopCommand || e[slackKeyThreadTS] != root {
		t.Errorf("reply = %v", e)
	}
	form, err := url.ParseQuery(string(got[3].body))
	if err != nil || got[3].path != slackInteractionsPath || got[3].contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("click: %s %s %v", got[3].path, got[3].contentType, err)
	}
	var click struct {
		Type      string `json:"type"`
		User      struct{ ID string }
		Channel   struct{ ID string }
		Container struct {
			MessageTS string `json:"message_ts"`
		}
		Message struct {
			Blocks []map[string]any `json:"blocks"`
		}
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		}
		ResponseURL string `json:"response_url"`
	}
	if err := json.Unmarshal([]byte(form.Get("payload")), &click); err != nil {
		t.Fatal(err)
	}
	if click.Type != "block_actions" || click.User.ID != "UP" || click.Channel.ID != "C1" || click.Container.MessageTS != "9.1" ||
		len(click.Actions) != 1 || click.Actions[0].ActionID != slackActionApprove || !strings.Contains(click.Actions[0].Value, "task-1") ||
		!reflect.DeepEqual(click.Message.Blocks, card.Blocks) || !strings.HasSuffix(click.ResponseURL, slackResponsePath) {
		t.Errorf("click = %+v", click)
	}
}

// TestSlackReaders: the open card is the last one with buttons and names its
// task; the roster's bullet lines are its display names; findMessage needs
// every needle.
func TestSlackReaders(t *testing.T) {
	button := func(task string) []map[string]any {
		return []map[string]any{{fieldTypeKey: slackBlockActions, slackKeyElements: []any{map[string]any{fieldTypeKey: slackButton, slackKeyActionID: slackActionApprove, slackKeyValue: `{"t":"1.1","id":"` + task + `"}`}}}}
	}
	msgs := []slackMessage{
		{TS: "1", Text: testCardTitle, Blocks: button("task-old")},
		{TS: "2", Text: "text"},
		{TS: "3", Text: testCardTitle, Blocks: button("task-1")},
		{TS: "4", Text: testApproved, Blocks: []map[string]any{{fieldTypeKey: slackBlockSection, slackKeyText: map[string]any{fieldTypeKey: slackMrkdwn, slackKeyText: "*Approval required* · list"}}}},
	}
	card, ok := openCard(msgs)
	if !ok || card.msg.TS != "3" || card.taskID != "task-1" {
		t.Errorf("openCard = %+v %v", card, ok)
	}
	if _, ok := openCard(msgs[3:]); ok {
		t.Error("a decided card has no buttons left")
	}
	roster := slackRosterHeading + " — start a new conversation with `/agent \"<name>\" <question>`:\n• *agentlab Swarmgeist proof* — Throwaway\n• *Other*"
	if names := rosterNames(roster); !reflect.DeepEqual(names, []string{"agentlab Swarmgeist proof", "Other"}) {
		t.Errorf("rosterNames = %q", names)
	}
	if err := assertSlackRoster([]string{klausGatewayTestDisplay}); err != nil {
		t.Error(err)
	}
	if err := assertSlackRoster([]string{klausGatewayTestDisplay, klausGatewayTestUnadmittedDisplay}); err == nil {
		t.Error("the unadmitted template in the roster")
	}
	if err := assertSlackRoster([]string{"Other"}); err == nil {
		t.Error("a roster without the fixture")
	}
	if m, ok := findMessage(msgs, "Approved by", "list"); !ok || m.TS != "4" {
		t.Errorf("findMessage = %+v %v", m, ok)
	}
	if _, ok := findMessage(msgs, "Approved by", "absent"); ok {
		t.Error("findMessage matched a missing needle")
	}
	if line := threadLine(nil); !strings.Contains(line, "nothing") {
		t.Errorf("threadLine(nil) = %q", line)
	}
}

// TestSlackFakeReadBack: the proof's view of a fake in a container — the
// recorded thread read back over the fake's state API equals the in-process
// view, and its own timestamps stay clear of the fake's.
func TestSlackFakeReadBack(t *testing.T) {
	f := startTestFake(t, nil)
	root := f.nextTS()
	callFake(t, f, slackPostMessage, url.Values{slackKeyChannel: {"C1"}, slackKeyThreadTS: {root}, slackKeyText: {"hello"}}, nil)
	c := &slackFakeContainer{hostURL: "http://" + f.listener.Addr().String(), client: http.DefaultClient}
	if got, want := c.thread("C1", root), f.thread("C1", root); !reflect.DeepEqual(got, want) || len(got) != 1 {
		t.Errorf("read back %+v, the fake holds %+v", got, want)
	}
	if got := c.thread("C1", "no-such-thread"); len(got) != 0 {
		t.Errorf("an unknown thread read back %+v", got)
	}
	if c.baseURL() != f.baseURL() {
		t.Errorf("baseURL = %s, want %s", c.baseURL(), f.baseURL())
	}
	ts := c.nextTS()
	if _, frac, _ := strings.Cut(ts, "."); frac < "500000" {
		t.Errorf("the proof's ts %s is in the fake's half", ts)
	}
	if (&slackFakeContainer{hostURL: "http://127.0.0.1:1", client: &http.Client{Timeout: time.Second}}).thread("C1", root) != nil {
		t.Error("an unreachable fake reads as an empty thread")
	}
}

// TestSlackFakeContainerParts: `docker port`'s loopback line is the host
// address; a file that is no Linux executable is refused with the flag to
// pass; `slack-fake` refuses a person that is not <id>=<e-mail>.
func TestSlackFakeContainerParts(t *testing.T) {
	if got := publishedLoopback("0.0.0.0:1234\n127.0.0.1:32768\n"); got != "127.0.0.1:32768" {
		t.Errorf("publishedLoopback = %q", got)
	}
	if got := publishedLoopback("[::]:32768\n"); got != "" {
		t.Errorf("publishedLoopback without loopback = %q", got)
	}
	script := filepath.Join(t.TempDir(), "agentlab")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := linuxStaticBinary(script); err == nil || !strings.Contains(err.Error(), "--slack-fake-binary") {
		t.Errorf("a script: %v", err)
	}
	if err := ServeFakeSlack(t.Context(), "127.0.0.1:0", []string{"UP"}); err == nil || !strings.Contains(err.Error(), "<slack user id>=<e-mail>") {
		t.Errorf("a person without an e-mail: %v", err)
	}
}
