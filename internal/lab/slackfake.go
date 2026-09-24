package lab

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The Slack side of the Swarmgeist proof, headless.
//
// klaus-gateway's only channel is Slack, and the gateway ships what a proof
// of it needs without a workspace: its Web API client takes a base URL
// (`--slack-api-base`, KLAUS_GATEWAY_SLACK_API_BASE), and in events mode it
// takes Events API callbacks and Block Kit clicks signed with the configured
// signing secret. fakeSlack is that Web API for one run: it answers who the
// bot and the people are (auth.test, users.info), acknowledges every other
// call the way Slack does ({"ok":true,slackKeyTS:…}), and keeps what the gateway
// posted, streamed, rewrote and deleted per thread, so the proof reads the
// thread the way a person in the channel would see it. slackDriver is the
// workspace's other half: it signs and posts the messages and the button
// clicks a person would make.
//
// Where the fake runs follows who calls it. A gateway on this host alone (the
// component off) calls an in-process fake on loopback. The meta chart's
// component calls it from a pod, and a pod reaches a service on this host
// only through the host's firewall — a default-deny one drops that traffic —
// so then the fake runs as a container on the kind network, the proof's own
// binary (`agentlab slack-fake`) in the lab's probe image: pods reach it
// container to container, this host through a port published on loopback,
// and the proof reads the recorded threads back over HTTP.

// The fake workspace's names: the team, the bot user the gateway learns from
// auth.test (Slack ids are upper-case alphanumerics; the gateway parses
// mentions as <@U…>), its name, and the prefix of the proof's people.
const (
	slackFakeTeam    = "TAGENTLAB"
	slackFakeBotUser = "UAGENTLABBOT"
	slackFakeBotName = "agentlab-swarmgeist"
	slackUserPrefix  = "UAGENTLABTEST"
	// slackAPIPath is where the fake serves the Web API: the gateway joins
	// the base and the method with a slash and trims nothing.
	slackAPIPath = "/api"
	// slackResponsePath receives what the gateway posts to an interaction's
	// response_url.
	slackResponsePath = "/response"
	// slackHealthPath answers "ok": what the pre-flight from a pod fetches.
	slackHealthPath = "/healthz"
	// slackThreadPath reads one recorded thread back (?channel=&ts=), for
	// the proof when the fake runs in a container.
	slackThreadPath = "/state/thread"
)

// slackWorkspace is the fake Slack of one run as the proof uses it: the Web
// API base a gateway on this host calls, the threads the fake recorded, fresh
// timestamps for the people's messages, and its end.
type slackWorkspace interface {
	baseURL() string
	thread(channel, threadTS string) []slackMessage
	nextTS() string
	close()
}

// The Web API methods the fake keeps a thread of, by name.
const (
	slackPostMessage   = "chat.postMessage"
	slackPostEphemeral = "chat.postEphemeral"
	slackUpdate        = "chat.update"
	slackDelete        = "chat.delete"
	slackStartStream   = "chat.startStream"
	slackAppendStream  = "chat.appendStream"
	slackStopStream    = "chat.stopStream"
	slackAuthTest      = "auth.test"
	slackUsersInfo     = "users.info"
	slackReplies       = "conversations.replies"
)

// The fields of Slack's payloads the fake and the driver read and write, and
// the block and chunk types among their values, named once.
const (
	slackKeyChannel    = "channel"
	slackKeyThreadTS   = "thread_ts"
	slackKeyTS         = "ts"
	slackKeyText       = "text"
	slackKeyBlocks     = "blocks"
	slackKeyChunks     = "chunks"
	slackKeyUser       = "user"
	slackKeyTeamID     = "team_id"
	slackKeyActionID   = "action_id"
	slackKeyElements   = "elements"
	slackKeyValue      = "value"
	slackKeyURL        = "url"
	slackKeyMessage    = "message"
	slackMrkdwn        = "mrkdwn"
	slackMarkdownChunk = "markdown_text"
	slackBlockActions  = "actions"
	slackBlockSection  = "section"
	slackButton        = "button"
)

// The gateway's Block Kit action ids and texts the proof reads.
const (
	slackActionApprove = "hitl_approve"
	slackActionDeny    = "hitl_deny"
	slackActionSignIn  = "obo_sign_in"
	// slackRosterHeading opens the roster `@bot /agent` posts.
	slackRosterHeading = "*Available agents*"
	// slackNotRunnable is the refusal of an agent that exists but cannot
	// start a conversation (the reason follows it).
	slackNotRunnable = "cannot start a conversation right now"
	slackNotStarted  = "I haven't started anything"
	slackStopped     = "⏹ Stopped."
	slackApprovedBy  = "Approved by <@"
	slackDeniedBy    = "Denied by <@"
	// slackStopCommand is the thread reply that stops a running turn.
	slackStopCommand = "/stop"
	// slackAgentCommand selects an agent in a mention; bare, it lists them.
	slackAgentCommand = "/agent"
)

// slackMessage is one message of a fake thread as it stands: posted,
// streamed into, rewritten, or deleted.
type slackMessage struct {
	TS       string
	Channel  string
	ThreadTS string
	Method   string
	// Recipient is the one person an ephemeral message is shown to.
	Recipient string
	// Text is the message's text, or for a streamed message the prose of
	// every markdown chunk in order.
	Text     string
	Blocks   []map[string]any
	Username string
	IconURL  string
	Deleted  bool
}

// streamed reports a message the gateway streamed (chat.startStream).
func (m slackMessage) streamed() bool { return m.Method == slackStartStream }

// action returns the Block Kit button with the action id, if the message
// carries one.
func (m slackMessage) action(id string) (map[string]any, bool) {
	for _, block := range m.Blocks {
		elements, _ := block[slackKeyElements].([]any)
		for _, e := range elements {
			if el, ok := e.(map[string]any); ok && el[slackKeyActionID] == id {
				return el, true
			}
		}
	}
	return nil, false
}

// fakeSlack is the Slack Web API of one proof run.
type fakeSlack struct {
	listener net.Listener
	server   *http.Server
	// emails is what users.info answers per Slack user id.
	emails map[string]string

	mu       sync.Mutex
	seq      int
	messages []*slackMessage
	byTS     map[string]*slackMessage
	calls    map[string]int
	// responses are the bodies posted to response_url.
	responses []string
}

// startFakeSlack serves the fake Web API on addr (host:port; port 0 picks
// one). emails maps the proof's Slack user ids to what users.info answers.
func startFakeSlack(addr string, emails map[string]string) (*fakeSlack, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("the fake Slack Web API cannot listen on %s: %w", addr, err)
	}
	f := &fakeSlack{listener: l, emails: emails, byTS: map[string]*slackMessage{}, calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+slackAPIPath+"/{method}", f.serveAPI)
	mux.HandleFunc("POST "+slackResponsePath, f.serveResponse)
	mux.HandleFunc("GET "+slackHealthPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET "+slackThreadPath, func(w http.ResponseWriter, r *http.Request) {
		writeSlackJSON(w, f.thread(r.URL.Query().Get(slackKeyChannel), r.URL.Query().Get(slackKeyTS)))
	})
	f.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.server.Serve(l) }()
	return f, nil
}

// baseURL is the Web API base as a gateway on the host dials it.
func (f *fakeSlack) baseURL() string { return "http://" + f.listener.Addr().String() + slackAPIPath }

// close stops the fake.
func (f *fakeSlack) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = f.server.Shutdown(ctx)
}

// nextTS is a fresh Slack timestamp: the current second and a run-wide
// sequence, so every message — the gateway's and the proof's own — has a
// distinct, increasing ts near now (the gateway drops stale events when told
// to, and dedups by channel and ts).
func (f *fakeSlack) nextTS() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextTSLocked()
}

func (f *fakeSlack) nextTSLocked() string {
	f.seq++
	return fmt.Sprintf("%d.%06d", time.Now().Unix(), f.seq%1_000_000)
}

// callCount is how many times the gateway called a method.
func (f *fakeSlack) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// thread is the thread's live messages (deleted ones left out) in post
// order: the root's replies the gateway posted, ephemeral ones included.
func (f *fakeSlack) thread(channel, threadTS string) []slackMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []slackMessage
	for _, m := range f.messages {
		if m.Channel == channel && m.ThreadTS == threadTS && !m.Deleted {
			out = append(out, *m)
		}
	}
	return out
}

// waitThread polls the thread until pred holds and returns its messages;
// on the deadline it returns them with false.
func waitThread(ws slackWorkspace, channel, threadTS string, timeout time.Duration, pred func([]slackMessage) bool) ([]slackMessage, bool) {
	var msgs []slackMessage
	ok := waitFor(int(timeout/(250*time.Millisecond))+1, 250*time.Millisecond, func() bool {
		msgs = ws.thread(channel, threadTS)
		return pred(msgs)
	})
	return msgs, ok
}

// ServeFakeSlack serves the fake Slack Web API on addr until ctx ends:
// `agentlab slack-fake`, what the proof runs in a container on the kind
// network. emails are the people's `<slack user id>=<e-mail>` pairs
// users.info answers.
func ServeFakeSlack(ctx context.Context, addr string, emails []string) error {
	people := make(map[string]string, len(emails))
	for _, pair := range emails {
		id, email, ok := strings.Cut(pair, "=")
		if !ok || id == "" {
			return fmt.Errorf("--email %q is not <slack user id>=<e-mail>", pair)
		}
		people[id] = email
	}
	f, err := startFakeSlack(addr, people)
	if err != nil {
		return err
	}
	fmt.Printf("fake Slack Web API on %s (%d people)\n", f.listener.Addr(), len(people))
	<-ctx.Done()
	f.close()
	return nil
}

// --- the fake in a container on the kind network -----------------------------

// The container's own port and the command it runs.
const (
	slackFakeContainerPort = 8080
	slackFakeCommand       = "slack-fake"
	slackFakeBinaryPath    = "/agentlab"
	slackFakeStartWait     = 30 * time.Second
)

// slackFakeContainer is the fake running as a container on the kind network:
// pods dial podIP on slackFakeContainerPort, this host the port published on
// loopback. The people's message timestamps are made here, in the upper half
// of the microsecond field, so they never meet the ones the fake gives the
// gateway's messages.
type slackFakeContainer struct {
	name    string
	hostURL string
	podIP   string
	client  *http.Client

	mu  sync.Mutex
	seq int
}

func (c *slackFakeContainer) baseURL() string { return c.hostURL + slackAPIPath }

func (c *slackFakeContainer) nextTS() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return fmt.Sprintf("%d.%06d", time.Now().Unix(), 500_000+c.seq%500_000)
}

// thread reads the recorded thread back; a failed read is an empty thread,
// which every wait on it outlasts or reports.
func (c *slackFakeContainer) thread(channel, threadTS string) []slackMessage {
	q := url.Values{slackKeyChannel: {channel}, slackKeyTS: {threadTS}}
	resp, err := c.client.Get(c.hostURL + slackThreadPath + "?" + q.Encode())
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var msgs []slackMessage
	if json.NewDecoder(resp.Body).Decode(&msgs) != nil {
		return nil
	}
	return msgs
}

func (c *slackFakeContainer) close() { _ = command(dockerBin, "rm", "-f", c.name).Run() }

// startSlackFakeContainer runs `<binary> slack-fake` in the lab's probe image
// on the kind network, with its port published on loopback, as the caller's
// uid, and waits for its health; a leftover of an aborted run is replaced.
func startSlackFakeContainer(cfg *config.Config, binary string, emails map[string]string) (*slackFakeContainer, error) {
	if err := linuxStaticBinary(binary); err != nil {
		return nil, err
	}
	name := cfg.ClusterName + "-slack-fake"
	_ = command(dockerBin, "rm", "-f", name).Run()
	args := dockerRun(name, kindDockerNetwork, "-d", "--rm",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-p", "127.0.0.1::"+strconv.Itoa(slackFakeContainerPort),
		"-v", binary+":"+slackFakeBinaryPath+":ro",
		probeImage, slackFakeBinaryPath, slackFakeCommand, "--listen", "0.0.0.0:"+strconv.Itoa(slackFakeContainerPort))
	for _, id := range slices.Sorted(maps.Keys(emails)) {
		args = append(args, "--email", id+"="+emails[id])
	}
	if out, err := command(dockerBin, args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("starting the fake Slack Web API container %s: %w: %s", name, err, excerpt(strings.TrimSpace(string(out)), 300))
	}
	c := &slackFakeContainer{name: name, client: &http.Client{Timeout: 10 * time.Second}}
	ip, err := outputQuiet(dockerBin, "inspect", "-f", `{{with index .NetworkSettings.Networks "`+kindDockerNetwork+`"}}{{.IPAddress}}{{end}}`, name)
	published, perr := outputQuiet(dockerBin, "port", name, strconv.Itoa(slackFakeContainerPort)+"/tcp")
	c.podIP = firstIPv4(ip)
	hostPort := publishedLoopback(published)
	if err != nil || perr != nil || c.podIP == "" || hostPort == "" {
		logs, _ := outputQuiet(dockerBin, "logs", name)
		c.close()
		return nil, fmt.Errorf("the fake Slack Web API container %s has no address on network %s (%q) or no published port (%q): %v %v; its log: %s",
			name, kindDockerNetwork, strings.TrimSpace(ip), strings.TrimSpace(published), err, perr, excerpt(logs, 300))
	}
	c.hostURL = "http://" + hostPort
	if !waitFor(int(slackFakeStartWait/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
		resp, err := c.client.Get(c.hostURL + slackHealthPath)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}) {
		logs, _ := outputQuiet(dockerBin, "logs", name)
		c.close()
		return nil, fmt.Errorf("the fake Slack Web API container %s did not answer %s%s within %s; its log: %s", name, c.hostURL, slackHealthPath, slackFakeStartWait, excerpt(logs, 300))
	}
	return c, nil
}

// publishedLoopback is the loopback address `docker port` names for the
// published port.
func publishedLoopback(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if addr := strings.TrimSpace(line); strings.HasPrefix(addr, "127.0.0.1:") {
			return addr
		}
	}
	return ""
}

// linuxStaticBinary refuses a binary the probe image cannot run: one that is
// not a Linux executable (agentlab built for another OS) or that needs a
// dynamic loader (a `go build` with cgo; the image carries no glibc).
func linuxStaticBinary(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("the fake Slack Web API runs %s in a Linux container, and it is not a Linux executable (%v): pass --slack-fake-binary with a linux/%s agentlab", path, err, runtime.GOARCH)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("the fake Slack Web API runs %s in a container on the kind network, and it is dynamically linked: build it with CGO_ENABLED=0 (`make build` does), or pass --slack-fake-binary with a static agentlab", path)
		}
	}
	return nil
}

// serveAPI answers one Web API call. The gateway sends plain posts
// form-encoded and block, markdown and stream calls as JSON; both land in
// one parameter map.
func (f *fakeSlack) serveAPI(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	params, err := slackParams(r)
	if err != nil {
		writeSlackJSON(w, map[string]any{"ok": false, "error": "invalid_arguments"})
		return
	}
	writeSlackJSON(w, f.answer(method, params))
}

// answer records the call and returns what Slack would.
func (f *fakeSlack) answer(method string, params map[string]any) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	ok := map[string]any{"ok": true}
	switch method {
	case slackAuthTest:
		return map[string]any{"ok": true, "user_id": slackFakeBotUser, slackKeyUser: slackFakeBotName, slackKeyTeamID: slackFakeTeam, "team": "agentlab", "bot_id": "BAGENTLAB"}
	case slackUsersInfo:
		id := paramString(params, "user")
		name := strings.ToLower(id)
		if id == slackFakeBotUser {
			name = slackFakeBotName
		}
		return map[string]any{"ok": true, slackKeyUser: map[string]any{
			"id": id, "name": name, slackKeyTeamID: slackFakeTeam,
			"profile": map[string]any{"email": f.emails[id], "display_name": name, "real_name": name},
		}}
	case slackReplies:
		// The proof's threads start with the mention itself, so a thread
		// holds nothing the gateway did not see.
		return map[string]any{"ok": true, "messages": []any{}, "has_more": false, "response_metadata": map[string]any{"next_cursor": ""}}
	case slackPostMessage, slackPostEphemeral, slackStartStream:
		m := &slackMessage{
			TS:        f.nextTSLocked(),
			Channel:   paramString(params, "channel"),
			ThreadTS:  paramString(params, "thread_ts"),
			Method:    method,
			Text:      paramString(params, "text") + chunkText(params[slackKeyChunks]),
			Blocks:    paramBlocks(params[slackKeyBlocks]),
			Username:  paramString(params, "username"),
			IconURL:   paramString(params, "icon_url"),
			Recipient: paramString(params, "user"),
		}
		f.messages = append(f.messages, m)
		f.byTS[m.TS] = m
		ok[slackKeyTS], ok[slackKeyChannel] = m.TS, m.Channel
		if method == slackPostEphemeral {
			ok["message_ts"] = m.TS
		}
		return ok
	case slackAppendStream, slackStopStream:
		if m := f.byTS[paramString(params, "ts")]; m != nil {
			m.Text += chunkText(params[slackKeyChunks])
		}
		ok[slackKeyTS] = paramString(params, "ts")
		return ok
	case slackUpdate:
		if m := f.byTS[paramString(params, "ts")]; m != nil {
			m.Text = paramString(params, "text")
			m.Blocks = paramBlocks(params[slackKeyBlocks])
		}
		ok[slackKeyTS] = paramString(params, "ts")
		return ok
	case slackDelete:
		if m := f.byTS[paramString(params, "ts")]; m != nil {
			m.Deleted = true
		}
		ok[slackKeyTS] = paramString(params, "ts")
		return ok
	}
	ok[slackKeyTS] = f.nextTSLocked()
	return ok
}

// serveResponse keeps what the gateway posts to an interaction's
// response_url (an ephemeral answer to the clicker).
func (f *fakeSlack) serveResponse(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	f.responses = append(f.responses, string(body))
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// slackParams reads a Web API call's parameters, form or JSON.
func slackParams(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		params := map[string]any{}
		if len(bytes.TrimSpace(body)) == 0 {
			return params, nil
		}
		return params, json.Unmarshal(body, &params)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	params := make(map[string]any, len(values))
	for k, v := range values {
		params[k] = v[0]
	}
	return params, nil
}

// paramString is a parameter as a string, whatever the encoding.
func paramString(params map[string]any, key string) string {
	switch v := params[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// paramBlocks is the blocks parameter: a JSON array, or its string form in a
// form-encoded call.
func paramBlocks(v any) []map[string]any {
	var raw []any
	switch b := v.(type) {
	case []any:
		raw = b
	case string:
		if json.Unmarshal([]byte(b), &raw) != nil {
			return nil
		}
	}
	var out []map[string]any
	for _, block := range raw {
		if m, ok := block.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// chunkText is the prose of a stream call's chunks: the markdown ones, in
// order (the task_update chunks are the tool steps, not the answer).
func chunkText(v any) string {
	chunks, _ := v.([]any)
	var b strings.Builder
	for _, c := range chunks {
		if m, ok := c.(map[string]any); ok && m[fieldTypeKey] == slackMarkdownChunk {
			s, _ := m[slackKeyText].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

func writeSlackJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- the workspace's people, as signed requests to the gateway ---------------

// slackDriver posts what the people of the fake workspace do to one gateway:
// messages as Events API callbacks and button clicks as interaction
// payloads, each signed with the gateway's signing secret the way Slack signs
// them.
type slackDriver struct {
	// base is the gateway's public base URL.
	base          string
	signingSecret string
	fake          slackWorkspace
	channel       string
	client        *http.Client
	eventSeq      int
}

func newSlackDriver(base, signingSecret string, fake slackWorkspace, channel string) *slackDriver {
	return &slackDriver{base: base, signingSecret: signingSecret, fake: fake, channel: channel, client: &http.Client{Timeout: 30 * time.Second}}
}

// The gateway's inbound Slack endpoints.
const (
	slackEventsPath       = "/channels/slack/events"
	slackInteractionsPath = "/channels/slack/interactions"
)

// slackSignature is Slack's v0 request signature: HMAC-SHA256 over
// "v0:<timestamp>:<body>" with the signing secret, hex, prefixed v0=.
func slackSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// post sends one signed request and wants the gateway's 200.
func (d *slackDriver) post(path, contentType string, body []byte) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, d.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackSignature(d.signingSecret, ts, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("POST %s answered HTTP %d: %s", path, resp.StatusCode, excerpt(string(b), 200))
	}
	return nil
}

// event posts one event_callback carrying event and returns nothing but the
// gateway's acknowledgement: the turn runs after it.
func (d *slackDriver) event(event map[string]any) error {
	d.eventSeq++
	body, err := json.Marshal(map[string]any{
		fieldTypeKey: "event_callback", slackKeyTeamID: slackFakeTeam, "api_app_id": "AAGENTLAB",
		"event_id": fmt.Sprintf("Ev%s%04d", strings.ToUpper(randomSuffix()), d.eventSeq), "event_time": time.Now().Unix(),
		"event": event,
	})
	if err != nil {
		return err
	}
	return d.post(slackEventsPath, "application/json", body)
}

// mention is a person's `@bot <text>` in the channel: a new thread when
// threadTS is empty, else a reply in that thread. It returns the message's
// ts — the thread's ts for a new thread.
func (d *slackDriver) mention(user, threadTS, text string) (string, error) {
	ts := d.fake.nextTS()
	ev := map[string]any{
		fieldTypeKey: "app_mention", slackKeyUser: user, slackKeyChannel: d.channel, "channel_type": "channel",
		slackKeyText: "<@" + slackFakeBotUser + "> " + text, slackKeyTS: ts, "event_ts": ts,
	}
	if threadTS != "" {
		ev[slackKeyThreadTS] = threadTS
	}
	return ts, d.event(ev)
}

// reply is a person's plain message in a thread, no mention: what the
// gateway serves as a reply in a thread it already carries.
func (d *slackDriver) reply(user, threadTS, text string) (string, error) {
	ts := d.fake.nextTS()
	return ts, d.event(map[string]any{
		fieldTypeKey: slackKeyMessage, slackKeyUser: user, slackKeyChannel: d.channel, "channel_type": "channel",
		slackKeyText: text, slackKeyTS: ts, "event_ts": ts, slackKeyThreadTS: threadTS,
	})
}

// click is a person pressing a button of a message the gateway posted: a
// block_actions payload with the button's action id and value, the
// message's ts and blocks as Slack sends them.
func (d *slackDriver) click(user string, msg slackMessage, actionID string) error {
	button, ok := msg.action(actionID)
	if !ok {
		return fmt.Errorf("message %s carries no %s button", msg.TS, actionID)
	}
	value, _ := button[slackKeyValue].(string)
	payload, err := json.Marshal(map[string]any{
		fieldTypeKey: "block_actions", slackKeyUser: map[string]any{"id": user}, "team": map[string]any{"id": slackFakeTeam},
		slackKeyChannel: map[string]any{"id": msg.Channel},
		"container":     map[string]any{fieldTypeKey: slackKeyMessage, "message_ts": msg.TS, "channel_id": msg.Channel, slackKeyThreadTS: msg.ThreadTS},
		slackKeyMessage: map[string]any{slackKeyTS: msg.TS, slackKeyThreadTS: msg.ThreadTS, slackKeyBlocks: msg.Blocks},
		"actions":       []any{map[string]any{fieldTypeKey: slackButton, slackKeyActionID: actionID, slackKeyValue: value}},
		"trigger_id":    "trigger-" + randomSuffix(),
		"response_url":  d.responseURL(),
	})
	if err != nil {
		return err
	}
	return d.post(slackInteractionsPath, "application/x-www-form-urlencoded", []byte(url.Values{"payload": {string(payload)}}.Encode()))
}

// responseURL is where the gateway answers a click ephemerally: the fake
// keeps it.
func (d *slackDriver) responseURL() string {
	return strings.TrimSuffix(d.fake.baseURL(), slackAPIPath) + slackResponsePath
}

// --- reading a thread ------------------------------------------------------

// hitlCard is an approval card the gateway posted: the message, and the task
// its buttons decide (the value carries the thread and the A2A task id).
type hitlCard struct {
	msg    slackMessage
	taskID string
}

// openCard is the thread's last approval card still carrying its buttons (a
// decision rewrites the card without them).
func openCard(msgs []slackMessage) (hitlCard, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		button, ok := msgs[i].action(slackActionApprove)
		if !ok {
			continue
		}
		var value struct {
			Task string `json:"id"`
		}
		raw, _ := button[slackKeyValue].(string)
		_ = json.Unmarshal([]byte(raw), &value)
		return hitlCard{msg: msgs[i], taskID: value.Task}, true
	}
	return hitlCard{}, false
}

// shown is a message's text and the text of its section and context
// blocks: what a person reads of it.
func (m slackMessage) shown() string {
	parts := []string{m.Text}
	for _, block := range m.Blocks {
		if t, ok := block[slackKeyText].(map[string]any); ok {
			s, _ := t[slackKeyText].(string)
			parts = append(parts, s)
		}
		elements, _ := block[slackKeyElements].([]any)
		for _, e := range elements {
			if el, ok := e.(map[string]any); ok && el[fieldTypeKey] == slackMrkdwn {
				s, _ := el[slackKeyText].(string)
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// findMessage is the first message whose text carries every needle.
func findMessage(msgs []slackMessage, needles ...string) (slackMessage, bool) {
	for _, m := range msgs {
		text := m.shown()
		if !slices.ContainsFunc(needles, func(n string) bool { return !strings.Contains(text, n) }) {
			return m, true
		}
	}
	return slackMessage{}, false
}

// streamedSince is the text of the streamed answers the thread gained after
// its first `after` messages, joined: one turn's answer, which a resumed or
// continued turn may split over several streams.
func streamedSince(msgs []slackMessage, after int) (slackMessage, string) {
	var last slackMessage
	var texts []string
	for _, m := range msgs[min(after, len(msgs)):] {
		if m.streamed() {
			last = m
			texts = append(texts, m.Text)
		}
	}
	return last, strings.Join(texts, "\n")
}

// rosterNames reads the display names of a roster post: its `• *Name*`
// lines.
func rosterNames(text string) []string {
	var names []string
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "• *")
		if !ok {
			continue
		}
		if name, _, ok := strings.Cut(rest, "*"); ok {
			names = append(names, name)
		}
	}
	return names
}

// threadLine words a thread for an error: each message's method and the
// start of its text.
func threadLine(msgs []slackMessage) string {
	if len(msgs) == 0 {
		return "(the gateway posted nothing in the thread)"
	}
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		lines = append(lines, m.Method+" "+strconv.Quote(excerpt(strings.ReplaceAll(m.shown(), "\n", " "), 100)))
	}
	return strings.Join(lines, " | ")
}

// errNoCard reports a paused turn without an approval card in its thread.
var errNoCard = errors.New("no approval card with an Approve button in the thread")
