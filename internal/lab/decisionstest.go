package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The decisions proof puts decisions to a person and to a team through
// klaus-gateway's POST /decisions, as a ServiceAccount the lab's API server
// vouches for, and answers them the three ways Slack offers — a Choose click,
// the "Answer in my own words" modal, a reply in the message's thread — each
// answer calling a real muster tool as the linked person. It needs no agent:
// the gateway runs on the host as klaus-gateway-test runs it, Slack is the
// in-process fake.

// DecisionsGatewayImageDefault is the first klaus-gateway release that
// serves POST /decisions.
const DecisionsGatewayImageDefault = "gsoci.azurecr.io/giantswarm/klaus-gateway:3.13.0"

// The proof's caller, the tool its answers call, and how long it waits.
const (
	decisionsServiceAccount = "agentlab-decisions"
	decisionsAudience       = "klaus-gateway"
	// decisionsAnswerTool is read-only and ignores the answer's arguments:
	// what the proof shows is that muster ran it as the person.
	decisionsAnswerTool = "core_mcpserver_list"
	// decisionsRefusedTool does not exist, so muster refuses the answer.
	decisionsRefusedTool = "core_agentlab_no_such_tool"
	decisionsWait        = 60 * time.Second
	decisionsTokenTTL    = 20 * time.Minute
	// decisionsNote marks what the proof puts to the gateway as the lab's.
	decisionsNote     = "agentlab"
	decisionsKeyLabel = "label"
)

// The gateway's decision action ids and what its messages say.
const (
	decisionChoose        = "decision_choose"
	decisionOwnWords      = "decision_own_words"
	decisionCallbackID    = "decision_answer"
	decisionAnsweredBy    = "Answered by <@"
	decisionDefaulted     = "the default was applied"
	decisionNotAccepted   = "'s answer was not accepted"
	decisionNotSubmitted  = "'s answer could not be submitted"
	decisionLateRefusal   = "not answered in time"
	decisionOwnWordsReply = "Roll, but only after the 21:00 backup."
	decisionThreadReply   = "Wait for Monday; the backup runs on Sunday."
)

// DecisionsTestOptions tunes the decisions proof.
type DecisionsTestOptions struct {
	// GatewayImage is the klaus-gateway image run on the host's network
	// (default DecisionsGatewayImageDefault); GatewayBinary, a local build,
	// takes precedence — the proof of a branch.
	GatewayImage  string
	GatewayBinary string
	// Port is the host port of the gateway's endpoints; the admin endpoints
	// take Port+1, the fake Slack Web API Port+2. Default 18090.
	Port int
	// RunDir holds the stores, the keys and the gateway's log; empty picks
	// a temporary directory removed at the end.
	RunDir string
}

// DecisionsTest is the headless proof of klaus-gateway's decisions.
func DecisionsTest(cfg *config.Config, email string, opts DecisionsTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled {
		return fmt.Errorf("the platform is off in %s — the answers run through its muster; enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	if opts.GatewayImage == "" {
		opts.GatewayImage = DecisionsGatewayImageDefault
	}
	if opts.Port == 0 {
		opts.Port = 18090
	}
	if err := portsFree(opts.Port, opts.Port+1, opts.Port+2); err != nil {
		return err
	}
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return err
	}
	defer cleanRunDir()

	step("Logging in to Dex as %s", user.Email)
	idToken, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	identity, err := tokenIdentity(idToken, time.Now())
	if err != nil {
		return fmt.Errorf("the id_token of %s: %w", user.Email, err)
	}

	run := strings.ToUpper(randomSuffix())
	person := slackUserPrefix + run + "P"
	channel := "CAGENTLAB" + run
	fake, err := startFakeSlack(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port+2)), map[string]string{person: user.Email})
	if err != nil {
		return err
	}
	defer fake.close()

	keys, err := writeGatewayFiles(runDir)
	if err != nil {
		return err
	}
	step("Linking Slack user %s to %s in the gateway's bolt link store, the id_token as the link's cached token", person, user.Email)
	if err := seedBoltLink(filepath.Join(runDir, gatewayLinksFile), keys.store, person, identity.link(user.Email, idToken)); err != nil {
		return err
	}

	caller := "system:serviceaccount:" + platformNamespace + ":" + decisionsServiceAccount
	step("Creating ServiceAccount %s and a token for it with audience %s: the caller the gateway admits after the API server's TokenReview", caller, decisionsAudience)
	saToken, removeSA, err := decisionsCallerToken()
	if err != nil {
		return err
	}
	defer removeSA()

	kubeconfig, err := filepath.Abs(labKubeconfigPath)
	if err != nil {
		return err
	}
	caFile, err := filepath.Abs(caCertPath)
	if err != nil {
		return err
	}
	// Decisions never reach an agent: the a2a target is the edge itself,
	// never dialled.
	edge, err := edgeHostPort(cfg.AgentgatewayBaseURL())
	if err != nil {
		return err
	}
	gwOpts := KlausGatewayTestOptions{GatewayImage: opts.GatewayImage, GatewayBinary: opts.GatewayBinary, Port: opts.Port}
	gw := newGatewayProcess(gwOpts, runDir, caFile, "grpcs://"+edge, fake.baseURL(), cfg.MusterBaseURL())
	gw.extraArgs = []string{"--reviews-enabled=true", "--reviews-allowed-callers=" + caller}
	gw.kubeconfig = kubeconfig
	gw.trustLabCA = true
	defer func() { _ = gw.stop() }()
	step("Starting klaus-gateway %s on the host with the reviews endpoint, its TokenReview through the lab's API server, Slack on the fake at %s, muster %s", gw.describe(), fake.baseURL(), cfg.MusterBaseURL())
	if err := gw.start(); err != nil {
		return err
	}
	note("ready: %s", gw.version())

	p := &decisionsProof{
		api:    &decisionsAPI{base: gw.baseURL(), token: saToken, client: &http.Client{Timeout: 30 * time.Second}},
		driver: newSlackDriver(gw.baseURL(), keys.signing, fake, channel),
		fake:   fake, person: person, channel: channel,
	}
	since := time.Now()

	step("0. Refusals: no token is 401; an unknown person is 422")
	if err := p.refusals(); err != nil {
		return err
	}

	step("1. A team decision in %s: header, the options with Choose (the recommended one primary), answered by a click as %s", channel, user.Email)
	if err := p.chooseByClick(); err != nil {
		return err
	}
	step("   The answer ran at muster as %s: the forwarded id_token accepted, %s called under that subject", user.Email, decisionsAnswerTool)
	if err := assertMusterAttribution(user.Email, identity.subject, since); err != nil {
		return err
	}

	step("2. A decision for %s by email: a direct message, answered in the modal with an option and own words", user.Email)
	if err := p.ownWordsInModal(user.Email); err != nil {
		return err
	}

	step("3. A team decision answered by a reply in its thread")
	if err := p.replyInThread(); err != nil {
		return err
	}

	step("4. An answer the tool refuses is a status line under the buttons; the decision stays open")
	if err := p.refusedAnswer(); err != nil {
		return err
	}

	step("5. A decision closed as defaulted loses its buttons; a later click is told it was not answered in time")
	if err := p.closedAsDefaulted(); err != nil {
		return err
	}
	note("all decisions proven")
	return nil
}

// decisionsCallerToken creates the proof's ServiceAccount and a token for it
// with the gateway's audience; the returned func deletes the account.
func decisionsCallerToken() (string, func(), error) {
	k, err := labKube()
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sas := k.clientset.CoreV1().ServiceAccounts(platformNamespace)
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: decisionsServiceAccount, Labels: map[string]string{"app.kubernetes.io/managed-by": managedByAgentlabValue}}}
	if _, err := sas.Create(ctx, sa, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
		return "", nil, fmt.Errorf("creating ServiceAccount %s: %w", decisionsServiceAccount, err)
	}
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sas.Delete(ctx, decisionsServiceAccount, metav1.DeleteOptions{})
	}
	expiry := int64(decisionsTokenTTL / time.Second)
	tr, err := sas.CreateToken(ctx, decisionsServiceAccount, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		Audiences:         []string{decisionsAudience},
		ExpirationSeconds: &expiry,
	}}, metav1.CreateOptions{})
	if err != nil {
		remove()
		return "", nil, fmt.Errorf("a token for %s: %w", decisionsServiceAccount, err)
	}
	return tr.Status.Token, remove, nil
}

// decisionsAPI is the gateway's decisions endpoint as the caller uses it.
type decisionsAPI struct {
	base, token string
	client      *http.Client
}

type decisionReceipt struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// call POSTs body to path with bearer (none for ""), and returns the status
// and the response body.
func (a *decisionsAPI) call(path, bearer string, body any) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, a.base+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, out, nil
}

// post puts a decision and wants its 201.
func (a *decisionsAPI) post(body map[string]any) (decisionReceipt, error) {
	status, out, err := a.call("/decisions", a.token, body)
	if err != nil {
		return decisionReceipt{}, err
	}
	if status != http.StatusCreated {
		return decisionReceipt{}, fmt.Errorf("POST /decisions answered HTTP %d: %s", status, excerpt(string(out), 200))
	}
	var r decisionReceipt
	return r, json.Unmarshal(out, &r)
}

// decisionBody is a decision with two options, the second recommended,
// answered with tool; addressee is {"team", "channel"} or {"person"}.
func decisionBody(addressee map[string]any, question, tool string) map[string]any {
	body := map[string]any{
		"note":      decisionsNote,
		"question":  question,
		"statusQuo": "graveler and glean run the release since Tuesday without a restart.",
		"options": []any{
			map[string]any{decisionsKeyLabel: "Roll tonight", "consequence": "The lane clears at 22:00."},
			map[string]any{decisionsKeyLabel: "Wait for Monday", "consequence": "Nothing rolls before Monday."},
		},
		"recommend": 2,
		"due":       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"default":   "Wait for Monday.",
		"askedBy":   "agentlab decisions-test",
		"answer":    map[string]any{"tool": tool, "arguments": map[string]any{"note": decisionsNote}},
	}
	for k, v := range addressee {
		body[k] = v
	}
	return body
}

type decisionsProof struct {
	api             *decisionsAPI
	driver          *slackDriver
	fake            *fakeSlack
	person, channel string
}

func (p *decisionsProof) team() map[string]any {
	return map[string]any{slackKeyTeam: "team-agentlab", slackKeyChannel: p.channel}
}

// message waits until the decision's message satisfies pred.
func (p *decisionsProof) message(r decisionReceipt, what string, pred func(slackMessage) bool) (slackMessage, error) {
	var last slackMessage
	ok := waitFor(int(decisionsWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		for _, m := range p.fake.thread(r.Channel, "") {
			if m.TS == r.TS {
				last = m
				return pred(m)
			}
		}
		return false
	})
	if !ok {
		return last, fmt.Errorf("decision %s: %s did not happen within %s; the message reads: %s", r.ID, what, decisionsWait, excerpt(last.shown(), 400))
	}
	return last, nil
}

func (p *decisionsProof) posted(body map[string]any) (decisionReceipt, slackMessage, error) {
	r, err := p.api.post(body)
	if err != nil {
		return r, slackMessage{}, err
	}
	m, err := p.message(r, "the post", func(slackMessage) bool { return true })
	return r, m, err
}

func (p *decisionsProof) answered(r decisionReceipt, texts ...string) error {
	_, err := p.message(r, "the answer's rewrite", func(m slackMessage) bool {
		shown := m.shown()
		if _, open := m.action(decisionOwnWords); open || !strings.Contains(shown, decisionAnsweredBy+p.person+">") {
			return false
		}
		for _, t := range texts {
			if !strings.Contains(shown, t) {
				return false
			}
		}
		return true
	})
	if err == nil {
		note("%s: answered by %s", r.ID, p.person)
	}
	return err
}

func (p *decisionsProof) refusals() error {
	status, _, err := p.api.call("/decisions", "", decisionBody(p.team(), "Refused without a token?", decisionsAnswerTool))
	if err != nil {
		return err
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("POST /decisions without a token answered HTTP %d, want 401", status)
	}
	status, out, err := p.api.call("/decisions", p.api.token, decisionBody(map[string]any{"person": "nobody@lab.local"}, "Nobody there?", decisionsAnswerTool))
	if err != nil {
		return err
	}
	if status != http.StatusUnprocessableEntity {
		return fmt.Errorf("POST /decisions for a person Slack does not know answered HTTP %d, want 422: %s", status, excerpt(string(out), 200))
	}
	note("401 without a token, 422 for nobody@lab.local")
	return nil
}

func (p *decisionsProof) chooseByClick() error {
	r, m, err := p.posted(decisionBody(p.team(), "Roll the release onto the lab tonight?", decisionsAnswerTool))
	if err != nil {
		return err
	}
	if err := assertDecisionMessage(m); err != nil {
		return err
	}
	if err := p.driver.click(p.person, m, decisionChoose); err != nil {
		return err
	}
	return p.answered(r, "*Roll tonight*")
}

// assertDecisionMessage wants the question as a header, a Choose button per
// option with the recommended one primary, and the own-words button.
func assertDecisionMessage(m slackMessage) error {
	if len(m.Blocks) == 0 || m.Blocks[0][fieldTypeKey] != "header" {
		return fmt.Errorf("the decision's first block is not its question as a header: %s", excerpt(m.shown(), 300))
	}
	var chooses, primary int
	for _, b := range m.Blocks {
		acc, ok := b["accessory"].(map[string]any)
		if !ok || acc[slackKeyActionID] != decisionChoose {
			continue
		}
		chooses++
		if acc["style"] == "primary" {
			primary++
		}
	}
	if chooses != 2 || primary != 1 {
		return fmt.Errorf("the decision carries %d Choose buttons (%d primary), want 2 with the recommended one primary", chooses, primary)
	}
	if _, ok := m.action(decisionOwnWords); !ok {
		return fmt.Errorf("the decision carries no %q button", decisionOwnWords)
	}
	note("header, 2 Choose buttons (the recommended primary), Answer in my own words")
	return nil
}

func (p *decisionsProof) ownWordsInModal(email string) error {
	r, m, err := p.posted(decisionBody(map[string]any{"person": email}, "Roll the release onto the lab tonight, you decide?", decisionsAnswerTool))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(r.Channel, "D") {
		return fmt.Errorf("the decision for %s went to %s, not to a direct message", email, r.Channel)
	}
	note("direct message %s", r.Channel)
	before := len(p.fake.openedViews())
	if err := p.driver.click(p.person, m, decisionOwnWords); err != nil {
		return err
	}
	var view map[string]any
	if !waitFor(int(decisionsWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		views := p.fake.openedViews()
		if len(views) > before {
			view = views[len(views)-1]
		}
		return view != nil
	}) {
		return fmt.Errorf("the own-words click opened no modal within %s", decisionsWait)
	}
	if view["callback_id"] != decisionCallbackID || view["private_metadata"] != r.ID {
		return fmt.Errorf("the modal is %v for %v, want %s for %s", view["callback_id"], view["private_metadata"], decisionCallbackID, r.ID)
	}
	if err := p.driver.submitView(p.person, view, map[string]any{
		"decision_answer_text":   map[string]any{"text": map[string]any{fieldTypeKey: "plain_text_input", slackKeyValue: decisionOwnWordsReply}},
		"decision_answer_choice": map[string]any{"choice": map[string]any{fieldTypeKey: "static_select", "selected_option": map[string]any{slackKeyValue: "1"}}},
	}); err != nil {
		return err
	}
	return p.answered(r, "*Roll tonight* — "+decisionOwnWordsReply)
}

func (p *decisionsProof) replyInThread() error {
	r, _, err := p.posted(decisionBody(p.team(), "Roll the release onto the lab on Monday instead?", decisionsAnswerTool))
	if err != nil {
		return err
	}
	ts := p.fake.nextTS()
	if err := p.driver.event(map[string]any{
		fieldTypeKey: slackKeyMessage, slackKeyUser: p.person, slackKeyChannel: r.Channel, slackKeyChanType: slackKeyChannel,
		slackKeyText: decisionThreadReply, slackKeyTS: ts, slackKeyEventTS: ts, slackKeyThreadTS: r.TS, "parent_user_id": slackFakeBotUser,
	}); err != nil {
		return err
	}
	return p.answered(r, decisionThreadReply)
}

func (p *decisionsProof) refusedAnswer() error {
	r, m, err := p.posted(decisionBody(p.team(), "Answer with a tool that is not there?", decisionsRefusedTool))
	if err != nil {
		return err
	}
	if err := p.driver.click(p.person, m, decisionChoose); err != nil {
		return err
	}
	status, err := p.message(r, "the refusal's status line", func(m slackMessage) bool {
		shown := m.shown()
		_, open := m.action(decisionOwnWords)
		return open && (strings.Contains(shown, "<@"+p.person+">"+decisionNotAccepted) || strings.Contains(shown, "<@"+p.person+">"+decisionNotSubmitted))
	})
	if err != nil {
		return err
	}
	note("open, with: %s", excerpt(status.shown()[strings.LastIndex(status.shown(), "<@"+p.person+">"):], 160))
	return nil
}

func (p *decisionsProof) closedAsDefaulted() error {
	r, open, err := p.posted(decisionBody(p.team(), "Nobody answers this one?", decisionsAnswerTool))
	if err != nil {
		return err
	}
	status, out, err := p.api.call("/decisions/"+url.PathEscape(r.ID)+"/close", p.api.token, map[string]any{"outcome": "defaulted"})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("closing %s answered HTTP %d: %s", r.ID, status, excerpt(string(out), 200))
	}
	if _, err := p.message(r, "the defaulted rewrite", func(m slackMessage) bool {
		_, buttons := m.action(decisionOwnWords)
		return !buttons && strings.Contains(m.shown(), decisionDefaulted)
	}); err != nil {
		return err
	}
	if err := p.driver.click(p.person, open, decisionChoose); err != nil {
		return err
	}
	if !waitFor(int(decisionsWait/(500*time.Millisecond)), 500*time.Millisecond, func() bool {
		for _, m := range p.fake.thread(r.Channel, "") {
			if m.Method == slackPostEphemeral && m.Recipient == p.person && strings.Contains(m.Text, decisionLateRefusal) {
				return true
			}
		}
		return false
	}) {
		return fmt.Errorf("a click on the defaulted decision %s was not told it was not answered in time", r.ID)
	}
	note("%s: defaulted, a late click refused", r.ID)
	return nil
}
