package lab

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The beekeeper subscriptions proof runs beekeeper serve as
// beekeeper-central-test does (on the host, over the lab cluster's beekeeper
// resources, registered in the lab's muster with the person's token
// forwarded) and has two lab users subscribe through muster with MCP
// 2026-07-28's subscriptions/listen, each to their own mailbox. A message
// into one person's mailbox reaches that person's stream and never the
// other's; a subscription to the other person's mailbox is refused; the
// streams end when the subscribers hang up.

// BeekeeperSubscriptionsVersionDefault is a beekeeper release whose serve
// offers its state as subscribable resources.
const BeekeeperSubscriptionsVersionDefault = "v0.149.3"

const (
	subscriptionsProtocol = "2026-07-28"
	subscriptionsHost     = "agentlab"
	subscriptionsMailbox  = "beekeeper://mailbox/"
	notifAcknowledged     = "notifications/subscriptions/acknowledged"
	notifResourceUpdated  = "notifications/resources/updated"
	metaSubscriptionID    = "io.modelcontextprotocol/subscriptionId"
	subscriptionsWait     = 30 * time.Second
)

// BeekeeperSubscriptionsTestOptions tunes the beekeeper subscriptions proof.
type BeekeeperSubscriptionsTestOptions struct {
	// Version is the beekeeper release whose image and CRDs run (default
	// BeekeeperSubscriptionsVersionDefault); Binary, a local static linux
	// build, runs as serve instead.
	Version string
	Binary  string
	// Port is beekeeper serve's in the node's network, Port+1 its
	// Postgres'. Default 18095.
	Port   int
	RunDir string
}

// BeekeeperSubscriptionsTest is the headless proof of resource
// subscriptions relayed per user by muster.
func BeekeeperSubscriptionsTest(cfg *config.Config, opts BeekeeperSubscriptionsTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled {
		return fmt.Errorf("the platform is off in %s — the subscribers reach serve through its muster; enable it and run `agentlab platform` first", config.File)
	}
	ana, pia, group, err := beekeeperUsers(cfg)
	if err != nil {
		return err
	}
	opts.Version = cmp.Or(opts.Version, BeekeeperSubscriptionsVersionDefault)
	opts.Port = cmp.Or(opts.Port, 18095)
	servePort, pgPort := opts.Port, opts.Port+1
	if err := portsFree(servePort, pgPort); err != nil {
		return err
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	tokens := map[string]string{}
	for _, u := range []*config.User{ana, pia} {
		step("Logging in to Dex as %s", u.Email)
		tok, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
		if err != nil {
			return err
		}
		tokens[u.Email] = tok
	}

	node := cfg.ControlPlaneNode()
	nodeIP, err := outputQuiet(dockerBin, "inspect", "-f", `{{with index .NetworkSettings.Networks "`+kindDockerNetwork+`"}}{{.IPAddress}}{{end}}`, node)
	if err != nil || strings.TrimSpace(nodeIP) == "" {
		return fmt.Errorf("the address of node %s on network %s: %v", node, kindDockerNetwork, err)
	}
	nodeIP = strings.TrimSpace(nodeIP)
	inNode := "container:" + node
	run := strings.ToUpper(randomSuffix())

	if _, err := nodeKubeconfig(runDir); err != nil {
		return err
	}
	caFile, err := filepath.Abs(trustBundleFile(cfg))
	if err != nil {
		return err
	}
	if err := copyFile(caFile, filepath.Join(runDir, beekeeperCAFile)); err != nil {
		return err
	}

	step("Applying beekeeper %s's CRDs and the team namespace %s", opts.Version, beekeeperNamespace)
	removeCRDs, err := applyBeekeeperCRDs(opts.Version)
	if err != nil {
		return err
	}
	defer removeCRDs()
	removeNS, err := ensureBeekeeperNamespace()
	if err != nil {
		return err
	}
	defer removeNS()

	step("Starting Postgres for serve's mailboxes in node %s's network on :%d (%s)", node, pgPort, beekeeperPostgresImage)
	stopPG, err := startBeekeeperPostgres(run, inNode, pgPort)
	if err != nil {
		return err
	}
	defer stopPG()
	serveCfg := fmt.Sprintf("serve:\n  issuer: %q\n  clientIDs: [%q, %q]\n  organization: %q\n  teams: {%q: %q}\n",
		cfg.Issuer(), config.AgentPlatformClientID, config.KubernetesClientID, group, group, beekeeperTeam)
	image := beekeeperImage + ":" + strings.TrimPrefix(opts.Version, "v")
	step("Starting beekeeper serve (%s) in node %s's network on :%d", cmp.Or(opts.Binary, image), node, servePort)
	stopServe, err := startBeekeeperServe(image, opts.Binary, run, inNode, runDir, serveCfg, nodeIP, servePort, pgPort)
	if err != nil {
		return err
	}
	defer stopServe()
	url := "http://127.0.0.1:" + strconv.Itoa(servePort) + "/mcp"
	step("Registering MCPServer %s at %s in muster, the person's token forwarded", beekeeperServer, url)
	removeServer, err := registerBeekeeperServer(url)
	if err != nil {
		return err
	}
	defer removeServer()

	sessions := map[string]*musterSession{}
	for _, u := range []*config.User{ana, pia} {
		s, err := openMusterSession(cfg, tokens[u.Email], "agentlab-beekeeper-subscriptions-"+strings.Split(u.Email, "@")[0])
		if err != nil {
			return err
		}
		if err := waitBeekeeperTools(s); err != nil {
			return err
		}
		sessions[u.Email] = s
	}

	endpoint := cfg.MusterBaseURL() + "/mcp"
	step("1. %s and %s each subscribe to their own mailbox through muster (subscriptions/listen, MCP %s)", ana.Email, pia.Email, subscriptionsProtocol)
	anaStream, err := openSubscription(endpoint, tokens[ana.Email], 1, subscriptionsMailbox+ana.Email)
	if err != nil {
		return fmt.Errorf("%s's subscription: %w", ana.Email, err)
	}
	defer anaStream.close()
	piaStream, err := openSubscription(endpoint, tokens[pia.Email], 2, subscriptionsMailbox+pia.Email)
	if err != nil {
		return fmt.Errorf("%s's subscription: %w", pia.Email, err)
	}
	defer piaStream.close()
	note("both subscriptions acknowledged")

	agents := map[string]string{ana.Email: "ana-agent-" + strings.ToLower(run), pia.Email: "pia-agent-" + strings.ToLower(run)}
	for email, agent := range agents {
		if _, err := sessions[email].callServerTool(beekeeperRegister, map[string]any{"agent": agent, "host": subscriptionsHost}); err != nil {
			return err
		}
	}
	send := func(email, id string) error {
		_, err := sessions[email].callServerTool(beekeeperSend, map[string]any{
			"to":      "local:" + subscriptionsHost + "/" + agents[email],
			"message": map[string]any{"messageId": id, "role": "user", "parts": []any{map[string]any{"kind": "text", "text": "agentlab subscriptions proof"}}},
		})
		return err
	}

	step("2. A message into %s's mailbox reaches %s's stream only", ana.Email, ana.Email)
	if err := send(ana.Email, "agentlab-sub-"+run+"-1"); err != nil {
		return err
	}
	if err := anaStream.expectUpdate(subscriptionsMailbox + ana.Email); err != nil {
		return err
	}
	step("3. A message into %s's mailbox reaches %s's stream, first and alone: %s's update never reached it", pia.Email, pia.Email, ana.Email)
	if err := send(pia.Email, "agentlab-sub-"+run+"-2"); err != nil {
		return err
	}
	if err := piaStream.expectUpdate(subscriptionsMailbox + pia.Email); err != nil {
		return err
	}
	if err := send(ana.Email, "agentlab-sub-"+run+"-3"); err != nil {
		return err
	}
	if err := anaStream.expectUpdate(subscriptionsMailbox + ana.Email); err != nil {
		return fmt.Errorf("%w (%s's stream must carry only %s's mailbox)", err, ana.Email, ana.Email)
	}
	note("each person's stream carried their own mailbox only")

	step("4. %s's subscription to %s's mailbox is refused", pia.Email, ana.Email)
	if s, err := openSubscription(endpoint, tokens[pia.Email], 3, subscriptionsMailbox+ana.Email); err == nil {
		s.close()
		return fmt.Errorf("%s subscribed to %s's mailbox", pia.Email, ana.Email)
	} else {
		note("refused: %s", excerpt(err.Error(), 200))
	}
	note("resource subscriptions relayed per user through muster")
	return nil
}

// subscription is one open subscriptions/listen stream on muster.
type subscription struct {
	id     int
	cancel context.CancelFunc
	events chan subscriptionEvent
}

type subscriptionEvent struct {
	method string
	uri    string
	subID  any
	err    error
}

// openSubscription opens a subscriptions/listen stream as the token's
// person for the URIs and returns once muster acknowledged it; a refusal is
// the error.
func openSubscription(endpoint, token string, id int, uris ...string) (*subscription, error) {
	client, err := labHTTPClient(0)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "subscriptions/listen",
		"params": map[string]any{
			"notifications": map[string]any{"resourceSubscriptions": uris},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    subscriptionsProtocol,
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "agentlab-beekeeper-subscriptions", "version": "1"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", subscriptionsProtocol)
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &subscription{id: id, cancel: cancel, events: make(chan subscriptionEvent, 32)}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, excerpt(strings.TrimSpace(string(raw)), 400))
	}
	go s.read(resp.Body)
	ev, err := s.next()
	if err != nil {
		s.close()
		return nil, err
	}
	if ev.method != notifAcknowledged {
		s.close()
		return nil, fmt.Errorf("the stream opened with %s, not %s", cmp.Or(ev.method, "a response"), notifAcknowledged)
	}
	return s, nil
}

// read turns the SSE stream into events until it ends.
func (s *subscription) read(body io.ReadCloser) {
	defer func() { _ = body.Close() }()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				URI  string         `json:"uri"`
				Meta map[string]any `json:"_meta"`
			} `json:"params"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &msg); err != nil {
			continue
		}
		switch {
		case msg.Error != nil:
			s.events <- subscriptionEvent{err: fmt.Errorf("refused: %s", msg.Error.Message)}
		case msg.Method != "":
			s.events <- subscriptionEvent{method: msg.Method, uri: msg.Params.URI, subID: msg.Params.Meta[metaSubscriptionID]}
		default:
			s.events <- subscriptionEvent{err: fmt.Errorf("the stream closed: %s", excerpt(string(msg.Result), 200))}
		}
	}
	close(s.events)
}

func (s *subscription) next() (subscriptionEvent, error) {
	select {
	case ev, ok := <-s.events:
		if !ok {
			return ev, fmt.Errorf("the stream ended")
		}
		return ev, ev.err
	case <-time.After(subscriptionsWait):
		return subscriptionEvent{}, fmt.Errorf("nothing on the stream within %s", subscriptionsWait)
	}
}

// expectUpdate takes the stream's next event and requires it to be the
// update of uri, carrying the stream's own subscription ID.
func (s *subscription) expectUpdate(uri string) error {
	ev, err := s.next()
	if err != nil {
		return err
	}
	if ev.method != notifResourceUpdated || ev.uri != uri {
		return fmt.Errorf("the stream's next notification is %s %s, not %s %s", ev.method, ev.uri, notifResourceUpdated, uri)
	}
	if fmt.Sprint(ev.subID) != strconv.Itoa(s.id) {
		return fmt.Errorf("the update of %s carries subscription %v, not %d", uri, ev.subID, s.id)
	}
	note("%s on subscription %d", uri, s.id)
	return nil
}

func (s *subscription) close() { s.cancel() }
