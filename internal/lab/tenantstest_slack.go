package lab

import (
	"context"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The Slack half of tenants-test (giantswarm/agentlab#166): klaus-gateway on
// the host (klaus-gateway-test's host shape) with both tenant users linked,
// each asking the controller as themselves (giantswarm/klaus-gateway#354).
const (
	// slackNotPermitted starts the gateway's refusal of an agent the person
	// may not use; slackNothingSent is its promise that nothing reached it.
	slackNotPermitted = "You cannot use"
	slackNothingSent  = "Nothing was sent to it"
	// slackReplayFailed is the gateway's note on a held message it could not
	// run after the thread's person allowed its sender.
	slackReplayFailed = "could not be picked up again"
	// slackPlatformTenant is the platform tenant's label on a roster row.
	slackPlatformTenant = "platform"
	// slackAccessAllow is the button of the gateway's access prompt that
	// lets a collaborator into the thread.
	slackAccessAllow = "access_allow"
	// tenantsTraceSettle lets the gateway's log and the controller catch up
	// with a refusal before the proof reads them.
	tenantsTraceSettle = 3 * time.Second
	// tenantsSlackGatewayHint names the gateway release the half needs.
	tenantsSlackGatewayHint = "klaus-gateway with giantswarm/klaus-gateway#486 (4.10.0 or later: `--gateway-image gsoci.azurecr.io/giantswarm/klaus-gateway:4.10.0-rc.1` until 4.10.0 is released)"
)

// rosterRows are a roster post's rows as ref -> the row's text: each Select
// button's value, with the text of the section it is the accessory of.
func rosterRows(m slackMessage) map[string]string {
	rows := map[string]string{}
	for _, block := range m.Blocks {
		acc, ok := block["accessory"].(map[string]any)
		if !ok || acc[slackKeyActionID] != slackActionAgentSelect {
			continue
		}
		if ref, _ := acc[slackKeyValue].(string); ref != "" {
			rows[ref] = blockText(block)
		}
	}
	return rows
}

// organizationTenant is the gateway's label of an org-<name> namespace's
// tenant.
func organizationTenant(ns string) string {
	return "organization " + strings.TrimPrefix(ns, "org-")
}

// rosterProblem words what is wrong with a person's roster, "" when nothing
// is: their organization's agent labelled with its tenant, the platform
// tenant's agent labelled platform, and no agent of any other namespace.
func rosterProblem(rows map[string]string, ownNS, platformAgent string) string {
	ownRef := ownNS + "/" + tenantsTestAgent
	var problems []string
	if text, ok := rows[ownRef]; !ok {
		problems = append(problems, "no row of "+ownRef)
	} else if !strings.Contains(text, organizationTenant(ownNS)) {
		problems = append(problems, fmt.Sprintf("the row of %s does not name its tenant %q: %q", ownRef, organizationTenant(ownNS), text))
	}
	if text, ok := rows[platformAgent]; !ok {
		problems = append(problems, "no row of the platform tenant's "+platformAgent)
	} else if !strings.Contains(text, "· "+slackPlatformTenant) {
		problems = append(problems, fmt.Sprintf("the row of %s does not name the tenant %q: %q", platformAgent, slackPlatformTenant, text))
	}
	for _, ref := range slices.Sorted(maps.Keys(rows)) {
		if ns, _, ok := strings.Cut(ref, "/"); ok && ns != ownNS && ns != kagentNamespace {
			problems = append(problems, "a row of "+ref+", another tenant's")
		}
	}
	return strings.Join(problems, "; ")
}

// refusalProblem words what is wrong with the gateway's refusal of another
// tenant's agent, "" when nothing is: it names the tenant and says nothing
// was sent.
func refusalProblem(text, otherNS string) string {
	switch {
	case !strings.Contains(text, slackNotPermitted):
		return fmt.Sprintf("it is not the refusal %q", slackNotPermitted)
	case !strings.Contains(text, organizationTenant(otherNS)):
		return fmt.Sprintf("it does not name the tenant %q", organizationTenant(otherNS))
	case !strings.Contains(text, slackNothingSent):
		return fmt.Sprintf("it does not say %q", slackNothingSent)
	}
	return ""
}

// tenantsSlack is what the Slack half works with.
type tenantsSlack struct {
	cfg        *config.Config
	opts       KlausGatewayTestOptions
	namespaces [2]string
	users      [2]*config.User
	admin      *kagentAPI
}

// run is the Slack half, steps S1 to S4.
func (s *tenantsSlack) run() ([]string, error) {
	opts := s.opts
	opts.Port = gatewayTestPort(s.cfg)
	opts = opts.withDefaults()
	if err := portsFree(opts.Port, opts.Port+1, opts.Port+2); err != nil {
		return nil, err
	}
	image, err := resolveGatewayImage(s.cfg, opts.GatewayImage, opts.GatewayBinary)
	if err != nil {
		return nil, err
	}
	opts.GatewayImage = image
	runDir, cleanRunDir, err := klausGatewayRunDir(opts.RunDir)
	if err != nil {
		return nil, err
	}
	defer cleanRunDir()
	keys, err := writeGatewayFiles(runDir)
	if err != nil {
		return nil, err
	}

	run := strings.ToUpper(randomSuffix())
	var slackUsers [2]string
	var apis [2]*kagentAPI
	emails := map[string]string{}
	for i, u := range s.users {
		slackUsers[i] = slackUserPrefix + run + strconv.Itoa(i)
		emails[slackUsers[i]] = u.Email
		token, err := passwordGrant(s.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, u.Email, u.Password, musterLoginScopes)
		if err != nil {
			return nil, err
		}
		identity, err := tokenIdentity(token, time.Now())
		if err != nil {
			return nil, fmt.Errorf("the id_token of %s: %w", u.Email, err)
		}
		if err := seedBoltLink(filepath.Join(runDir, gatewayLinksFile), keys.store, slackUsers[i], identity.link(u.Email, token)); err != nil {
			return nil, err
		}
		if apis[i], err = dialKagentAPI(s.cfg, token); err != nil {
			return nil, err
		}
		defer apis[i].close()
	}
	fake, err := startFakeSlack(net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port+2)), emails)
	if err != nil {
		return nil, err
	}
	defer fake.close()
	edge, err := edgeHostPort(s.cfg.AgentgatewayBaseURL())
	if err != nil {
		return nil, err
	}
	caFile, err := filepath.Abs(trustBundleFile(s.cfg))
	if err != nil {
		return nil, err
	}
	gw := newGatewayProcess(opts, runDir, caFile, "grpcs://"+edge, fake.baseURL(), s.cfg.MusterBaseURL())
	if opts.GatewayBinary == "" && runtime.GOOS == goosDarwin {
		if err := s.bridged(gw, edge, fake.baseURL()); err != nil {
			return nil, err
		}
	}
	gw.container = gatewayContainerName(s.cfg)
	// A thread's Session starts through agent-manager's start_session, a
	// muster call the gateway makes as the person.
	gw.trustLabCA = true
	gw.defaultAgent = tenantsTestPlatformAgent
	defer func() { _ = gw.stop() }()
	step("S. klaus-gateway %s against the edge at %s, Slack on the fake at %s; %s and %s linked as Slack users %s and %s",
		gw.describe(), gw.target, gw.slackAPI, s.users[0].Email, s.users[1].Email, slackUsers[0], slackUsers[1])
	if err := gw.start(); err != nil {
		return nil, err
	}
	note("ready: %s", gw.version())
	channel := "CAGENTLAB" + run
	p := &slackProof{
		driver: newSlackDriver(gw.baseURL(), keys.signing, fake, channel),
		fake:   fake, channel: channel,
		logs: func() (string, error) { return gw.logs(), nil },
	}
	var out []string

	step("S1. `@bot %s` as %s and as %s at the same time: each roster lists their organization's agent and the platform tenant's, each row naming its tenant, nothing of the other organization", slackAgentsWord, s.users[0].Email, s.users[1].Email)
	type rosterResult struct {
		msg slackMessage
		err error
	}
	results := make(chan struct {
		i int
		rosterResult
	}, 2)
	for i := range s.users {
		go func(i int) {
			msg, err := p.roster(slackUsers[i])
			results <- struct {
				i int
				rosterResult
			}{i, rosterResult{msg, err}}
		}(i)
	}
	var rosters [2]slackMessage
	for range s.users {
		r := <-results
		if r.err != nil {
			return nil, fmt.Errorf("S1: %s's roster: %w", s.users[r.i].Email, r.err)
		}
		rosters[r.i] = r.msg
	}
	for i := range s.users {
		rows := rosterRows(rosters[i])
		if problem := rosterProblem(rows, s.namespaces[i], tenantsTestPlatformAgent); problem != "" {
			return nil, fmt.Errorf("S1: %s's roster (%s): %s — the gateway needs %s", s.users[i].Email, strings.Join(slices.Sorted(maps.Keys(rows)), ", "), problem, tenantsSlackGatewayHint)
		}
		note("%s: %s", s.users[i].Email, strings.Join(slices.Sorted(maps.Keys(rows)), ", "))
	}
	again, err := p.roster(slackUsers[0])
	if err != nil {
		return nil, err
	}
	if a, b := slices.Sorted(maps.Keys(rosterRows(again))), slices.Sorted(maps.Keys(rosterRows(rosters[0]))); !slices.Equal(a, b) {
		return nil, fmt.Errorf("S1: %s's roster changed after %s's: %s, before %s", s.users[0].Email, s.users[1].Email, strings.Join(a, ", "), strings.Join(b, ", "))
	}
	out = append(out, fmt.Sprintf("PASS: in Slack %s and %s, asking at the same time, get their own rosters: each their organization's agent (labelled %q, %q) and the platform tenant's (%q), nothing of the other's", s.users[0].Email, s.users[1].Email, organizationTenant(s.namespaces[0]), organizationTenant(s.namespaces[1]), slackPlatformTenant))

	otherRef := s.namespaces[1] + "/" + tenantsTestAgent
	otherAgent := &apiv1alpha1.ResourceReference{Namespace: s.namespaces[1], Name: tenantsTestAgent}
	step("S2. %s submits the agent picker with %s, the other organization's agent: refused with a note naming its tenant, and nothing reaches it", s.users[0].Email, otherRef)
	ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
	defer cancel()
	before, err := s.admin.allCreatorsSessions(ctx, otherAgent)
	if err != nil {
		return nil, err
	}
	since := time.Now()
	refusal, err := p.pickRefused(slackUsers[0], rosters[0], s.namespaces[0]+"/"+tenantsTestAgent, otherRef, klausGatewayWordPrompt)
	if err != nil {
		return nil, fmt.Errorf("S2: %w", err)
	}
	if problem := refusalProblem(refusal, s.namespaces[1]); problem != "" {
		return nil, fmt.Errorf("S2: the answer to %s's pick of %s: %s: %q", s.users[0].Email, otherRef, problem, excerpt(refusal, 300))
	}
	if err := s.nothingReached(ctx, p, otherAgent, before, since, slackUsers[0], true); err != nil {
		return nil, fmt.Errorf("S2: %w", err)
	}
	out = append(out, fmt.Sprintf("PASS: %s's pick of %s in Slack is refused (%q) and nothing reaches it", s.users[0].Email, otherRef, excerpt(refusal, 120)))

	step("S3. %s starts a conversation with %s from the picker in their roster's thread: the answer streams", s.users[1].Email, otherRef)
	thread := &slackThread{ts: rosters[1].ThreadTS, user: slackUsers[1]}
	turn, err := p.firstTurnBy(func(t *slackThread, text string) (*slackTurn, error) {
		return p.pickStarted(t, rosters[1], otherRef, text)
	}, thread, klausGatewayWordPrompt)
	if err != nil {
		return nil, fmt.Errorf("S3: %w", err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return nil, fmt.Errorf("S3: %w", err)
	}
	session, err := p.boundSession(apis[1], thread)
	if err != nil {
		return nil, fmt.Errorf("S3: %w", err)
	}
	note("answered %q; thread bound to Session %s", excerpt(turn.answer, 60), session.GetId())

	step("S4. %s, a collaborator without access, mentions the bot in %s's thread: refused with a note naming the tenant, and no turn reaches the agent", s.users[0].Email, s.users[1].Email)
	tasks, err := apis[1].taskIDs(ctx, session)
	if err != nil {
		return nil, err
	}
	before, err = s.admin.allCreatorsSessions(ctx, otherAgent)
	if err != nil {
		return nil, err
	}
	since = time.Now()
	refusal, resent, err := p.collaboratorRefused(thread, slackUsers[0], klausGatewayWordPrompt)
	if err != nil {
		return nil, fmt.Errorf("S4: %w", err)
	}
	if problem := refusalProblem(refusal, s.namespaces[1]); problem != "" {
		return nil, fmt.Errorf("S4: the answer to %s in %s's thread: %s: %q", s.users[0].Email, s.users[1].Email, problem, excerpt(refusal, 300))
	}
	if err := s.nothingReached(ctx, p, otherAgent, before, since, slackUsers[0], false); err != nil {
		return nil, fmt.Errorf("S4: %w", err)
	}
	after, err := apis[1].taskIDs(ctx, session)
	if err != nil {
		return nil, err
	}
	if len(after) != len(tasks) {
		return nil, fmt.Errorf("S4: Session %s holds %d task(s) after the collaborator's refused turn, %d before", session.GetId(), len(after), len(tasks))
	}
	how := ""
	if resent {
		how = fmt.Sprintf(" (after the Allow, the held message's replay got the gateway's generic %q note; the refusal came on the message sent again)", slackReplayFailed)
	}
	out = append(out, fmt.Sprintf("PASS: %s's turn in %s's thread on %s is refused (%q)%s; the Session keeps its %d task(s)", s.users[0].Email, s.users[1].Email, otherRef, excerpt(refusal, 120), how, len(after)))
	apis[1].removeSession(session.GetId())
	return out, nil
}

// nothingReached checks a refusal reached nothing: no new Session of the
// agent (every creator's, read as the admin) — and, where the gateway itself
// refuses (gatewayRefuses: a pick it validates against the person's roster),
// no turn_dispatch of the person in its log and no credential read for the
// agent's atespace since. A collaborator's turn is dispatched and the
// controller refuses it (the surface asks, the controller decides), and the
// gateway may resume the thread's suspended Session first, which reads the
// credentials without a turn: there the Session's tasks are the evidence.
func (s *tenantsSlack) nothingReached(ctx context.Context, p *slackProof, agent *apiv1alpha1.ResourceReference, before []*apiv1alpha1.Session, since time.Time, slackUser string, gatewayRefuses bool) error {
	time.Sleep(tenantsTraceSettle)
	logs, _ := p.logs()
	for _, d := range gatewayRecords(logs, recordDispatch) {
		if gatewayRefuses && d.SlackUser == slackUser {
			return fmt.Errorf("a turn of %s was dispatched to agent %s (turn_dispatch in thread %s)", slackUser, d.Agent, d.thread())
		}
	}
	after, err := s.admin.allCreatorsSessions(ctx, agent)
	if err != nil {
		return err
	}
	if len(after) != len(before) {
		return fmt.Errorf("%s holds %d Session(s) after the refusal, %d before", agentTenant(agent), len(after), len(before))
	}
	if !gatewayRefuses {
		return nil
	}
	read, err := tenantCredentialReads(agent.GetNamespace(), since)
	if err != nil {
		return err
	}
	if len(read) > 0 {
		return fmt.Errorf("the credential provider read %v for atespace %s: a turn reached the model", read, agent.GetNamespace())
	}
	return nil
}

// openPickerFrom presses a roster row's Select button and returns the agent
// picker it opens.
func (p *slackProof) openPickerFrom(user string, roster slackMessage, ref string) (map[string]any, error) {
	before := len(p.fake.openedViews())
	if err := p.driver.press(user, roster, slackActionAgentSelect, ref); err != nil {
		return nil, err
	}
	var picker map[string]any
	if !waitFor(int(klausGatewayReplyWait/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
		if views := p.fake.openedViews(); len(views) > before {
			picker = views[len(views)-1]
		}
		return picker != nil
	}) {
		return nil, fmt.Errorf("the Select button of %s opened no picker within %s", ref, klausGatewayReplyWait)
	}
	if picker["callback_id"] != slackPickerCallback {
		return nil, fmt.Errorf("the Select button of %s opened modal %v, not the agent picker %s", ref, picker["callback_id"], slackPickerCallback)
	}
	return picker, nil
}

// submitPicker submits the picker with the agent ref selected and the
// question.
func (p *slackProof) submitPicker(user string, picker map[string]any, ref, question string) error {
	return p.driver.submitView(user, picker, map[string]any{
		slackPickerAgentBlock:    map[string]any{slackPickerAgentAction: map[string]any{fieldTypeKey: slackStaticSelect, slackSelectedOption: map[string]any{slackKeyValue: ref}}},
		slackPickerQuestionBlock: map[string]any{slackPickerQuestionAction: map[string]any{fieldTypeKey: slackPlainTextInput, slackKeyValue: question}},
	})
}

// pickRefused opens the picker from the person's own row and submits it
// with another tenant's agent — one the picker does not offer them, a
// crafted submission — and returns the answer, which must be the refusal.
func (p *slackProof) pickRefused(user string, roster slackMessage, ownRef, otherRef, question string) (string, error) {
	picker, err := p.openPickerFrom(user, roster, ownRef)
	if err != nil {
		return "", err
	}
	if offered := pickerOptions(picker); slices.Contains(offered, otherRef) {
		return "", fmt.Errorf("the picker offers %s the other tenant's %s (%s)", user, otherRef, strings.Join(offered, ", "))
	}
	shown := len(p.fake.thread(p.channel, roster.ThreadTS))
	if err := p.submitPicker(user, picker, otherRef, question); err != nil {
		return "", err
	}
	msgs, ok := waitThread(p.fake, p.channel, roster.ThreadTS, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		_, found := findMessage(msgs[min(shown, len(msgs)):], slackNotPermitted)
		return found
	})
	if !ok {
		return "", fmt.Errorf("the pick of %s got no refusal within %s; the thread shows: %s", otherRef, klausGatewayReplyWait, threadLine(msgs))
	}
	msg, _ := findMessage(msgs[min(shown, len(msgs)):], slackNotPermitted)
	if msg.Method == slackPostEphemeral && msg.Recipient != user {
		return "", fmt.Errorf("the refusal went to %s, not to %s", msg.Recipient, user)
	}
	return msg.shown(), nil
}

// pickStarted opens the picker from the person's own row and submits it
// with that agent and the question: the conversation starts in the roster's
// thread. It waits for the turn's end.
func (p *slackProof) pickStarted(t *slackThread, roster slackMessage, ref, question string) (*slackTurn, error) {
	picker, err := p.openPickerFrom(t.user, roster, ref)
	if err != nil {
		return nil, err
	}
	before := len(p.fake.thread(p.channel, t.ts))
	if err := p.submitPicker(t.user, picker, ref, question); err != nil {
		return nil, err
	}
	return p.awaitTurn(t, before)
}

// collaboratorRefused posts the collaborator's mention in the thread; when
// the gateway asks the thread's person to allow them, the person allows it.
// It returns the refusal the collaborator gets, and whether they had to send
// again after the held message's replay failed.
func (p *slackProof) collaboratorRefused(t *slackThread, collaborator, text string) (string, bool, error) {
	shown := len(p.fake.thread(p.channel, t.ts))
	if _, err := p.driver.mention(collaborator, t.ts, text); err != nil {
		return "", false, err
	}
	var refusal, consent *slackMessage
	var resent bool
	msgs, ok := waitThread(p.fake, p.channel, t.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		for i := min(shown, len(msgs)); i < len(msgs); i++ {
			m := msgs[i]
			if strings.Contains(m.shown(), slackNotPermitted) {
				refusal = &m
				return true
			}
			if _, ok := m.action(slackAccessAllow); ok && m.Recipient == t.user {
				consent = &m
				return true
			}
		}
		return false
	})
	if !ok {
		return "", false, fmt.Errorf("the collaborator's mention got neither a refusal nor an access prompt to %s within %s; the thread shows: %s", t.user, klausGatewayReplyWait, threadLine(msgs))
	}
	if consent != nil {
		note("the gateway asks %s to allow %s; allowed", t.user, collaborator)
		shown = len(p.fake.thread(p.channel, t.ts))
		if err := p.driver.click(t.user, *consent, slackAccessAllow); err != nil {
			return "", false, err
		}
		var replayed bool
		msgs, ok = waitThread(p.fake, p.channel, t.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
			if m, found := findMessage(msgs[min(shown, len(msgs)):], slackNotPermitted); found {
				refusal = &m
				return true
			}
			_, replayed = findMessage(msgs[min(shown, len(msgs)):], slackReplayFailed)
			return replayed
		})
		if !ok {
			return "", false, fmt.Errorf("the allowed collaborator got no refusal within %s; the thread shows: %s", klausGatewayReplyWait, threadLine(msgs))
		}
		if refusal == nil {
			// klaus-gateway 4.10.0-rc.1 answers the held message's replay
			// after the Allow with its generic replay note, not the refusal
			// (postReplayFailureNote knows only the share refusal): the
			// collaborator sends again, as the note asks.
			note("the held message's replay got %q, not the refusal (a klaus-gateway follow-up); %s sends again", slackReplayFailed, collaborator)
			resent = true
			shown = len(p.fake.thread(p.channel, t.ts))
			if _, err := p.driver.mention(collaborator, t.ts, text); err != nil {
				return "", false, err
			}
			msgs, ok = waitThread(p.fake, p.channel, t.ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
				m, found := findMessage(msgs[min(shown, len(msgs)):], slackNotPermitted)
				if found {
					refusal = &m
				}
				return found
			})
			if !ok {
				return "", false, fmt.Errorf("the allowed collaborator's second message got no refusal within %s; the thread shows: %s", klausGatewayReplyWait, threadLine(msgs))
			}
		}
	}
	if refusal.Method == slackPostEphemeral && refusal.Recipient != collaborator {
		return "", false, fmt.Errorf("the refusal went to %s, not to the collaborator %s", refusal.Recipient, collaborator)
	}
	return refusal.shown(), resent, nil
}

// bridged runs the gateway's image on the kind network instead of the
// host's: Docker Desktop gives macOS no route into a host-network container
// (its host networking is off by default), so the proof could not reach the
// gateway's ports. The ports are published on loopback, the edge and muster
// are the node's NodePort under their public names (TLS verifies as usual),
// and the fake Slack Web API on this host is host.docker.internal.
func (s *tenantsSlack) bridged(gw *gatewayProcess, edge, slackAPI string) error {
	node := s.cfg.ControlPlaneNode()
	nodeIP, err := outputQuiet(dockerBin, "inspect", "-f", `{{with index .NetworkSettings.Networks "`+kindDockerNetwork+`"}}{{.IPAddress}}{{end}}`, node)
	if err != nil || strings.TrimSpace(nodeIP) == "" {
		return fmt.Errorf("the address of node %s on network %s: %v", node, kindDockerNetwork, err)
	}
	nodeIP = strings.TrimSpace(nodeIP)
	edgeHost, _, _ := net.SplitHostPort(edge)
	muster, err := url.Parse(s.cfg.MusterBaseURL())
	if err != nil {
		return err
	}
	nodePort := strconv.Itoa(config.GatewayNodePort)
	gw.target = "grpcs://" + net.JoinHostPort(edgeHost, nodePort)
	muster.Host = net.JoinHostPort(muster.Hostname(), nodePort)
	gw.musterURL = muster.String()
	gw.slackAPI = strings.Replace(slackAPI, "127.0.0.1", "host.docker.internal", 1)
	gw.network = kindDockerNetwork
	port := gw.opts.Port
	for _, p := range []int{port, port + 1} {
		gw.publish = append(gw.publish, fmt.Sprintf("127.0.0.1:%d:%d", p, p))
	}
	gw.addHosts = []string{edgeHost + ":" + nodeIP, muster.Hostname() + ":" + nodeIP}
	gw.extraArgs = append(gw.extraArgs,
		"--listen-address="+net.JoinHostPort("0.0.0.0", strconv.Itoa(port)),
		"--admin-address="+net.JoinHostPort("0.0.0.0", strconv.Itoa(port+1)))
	note("the gateway on network %s: edge and muster at node %s:%s, its ports published on 127.0.0.1, the fake Slack Web API at %s", kindDockerNetwork, nodeIP, nodePort, gw.slackAPI)
	return nil
}
