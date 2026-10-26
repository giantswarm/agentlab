package lab

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// The refusal half of the Swarmgeist proof: klaus-gateway's egress refusal
// notices, end to end on the meta chart's component. The chart runs them
// where Slack, kagent and Substrate are (klausGateway.refusals, the lab's
// interval 10s): the gateway reads the Substrate egress's refusal counter,
// resolves a refusing Session's actor to its Agent and the Agent to its
// creator, finds the creator in Slack by e-mail and sends them a direct
// message. The proof:
//
//   - an agent created through agent-manager as the person, its egress the
//     one origin DefaultEgressOrigin, named for the run: the notice's record
//     is per agent and destination for the window (24h), so a rerun's notice
//     would otherwise rewrite the last run's message instead of posting;
//   - one turn in a Session of it whose bash tool fetches the undeclared
//     egressTestUndeclaredURL once; the Session is kept until the notice is
//     in, since the gateway resolves the actor through it;
//   - one direct message to the person — the Slack user the fake answers
//     users.lookupByEmail with — naming the agent and the refused host and
//     port as plain text: the section's mrkdwn is sent verbatim, so Slack
//     shows host:port as written and links nothing;
//   - the agent and the Session removed, a leftover of an aborted run first.
//
// The deleted-Session pass (step 10b) runs the same with a second agent and
// the Session deleted before the gateway reads it: the Deployment is held at
// zero replicas from before the turn until the Session is deleted, so the
// gateway's first read of the refusing actor is of a deleted Session. The
// controller answers its session reader with the Session's tombstone, which
// names the Agent, and the person with NotFound; the notice proves the
// tombstone read (without it the actor is no Session the gateway knows and
// nobody is told). A series the gateway sees for the first time counts
// whole, so the refusal made while it was held is still noted.

const (
	// refusalAgentPrefix starts the name of the run's agent; a leftover of
	// an aborted run is recognised by it.
	refusalAgentPrefix = "agentlab-refusal-"
	// refusalNoticeWait bounds the notice: the gateway's interval, the
	// egress series' first read, the Session and the Agent read, the post.
	refusalNoticeWait = 3 * time.Minute
	// refusalHoldTimeout bounds the scale to zero and back of the
	// deleted-Session pass.
	refusalHoldTimeout = 2 * time.Minute
	// The component's env the chart renders for the notices.
	refusalsEnabledEnv  = "KLAUS_GATEWAY_REFUSALS_ENABLED"
	refusalsIntervalEnv = "KLAUS_GATEWAY_REFUSALS_INTERVAL"
	refusalsWindowEnv   = "KLAUS_GATEWAY_REFUSALS_WINDOW"
	// refusalNoticeTitle starts the notice's header (klaus-gateway's
	// "Egress refused for <agent>").
	refusalNoticeTitle = "Egress refused for "
	// refusalPostedRecord is the gateway's record of a posted notice.
	refusalPostedRecord = "refusal_notice_posted"
	// slackVerbatim is a mrkdwn text object's flag that keeps Slack from
	// turning URL-like text into links.
	slackVerbatim = "verbatim"
)

// refusalOutcome is what the refusal half found, for the summary.
type refusalOutcome struct {
	agent, session string
	destination    string
	interval       string
	elapsed        time.Duration
	notice         string
	// deleted is the deleted-Session pass: the Session was gone before the
	// gateway's first read.
	deleted bool
}

// klausGatewayRefusalProof runs the refusal half (see the file header) as
// the person, on the component's Deployment: emails are the fake's people,
// so the notice's direct message is found where users.lookupByEmail sent
// it. A chart before the notices' floor is a note and nil. With deleted, the
// Session is deleted while the Deployment is held at zero replicas, before
// the gateway's first read (step 10b).
func klausGatewayRefusalProof(cfg *config.Config, token string, user *config.User, fake slackWorkspace, emails map[string]string, deleted bool) (*refusalOutcome, error) {
	ctx := context.Background()
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	deploy, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, klausGatewayComponent, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", describe(gvrDeployments, platformNamespace, klausGatewayComponent), err)
	}
	env := containerEnv(deploy.Spec.Template.Spec.Containers, klausGatewayComponent)
	if on, _ := strconv.ParseBool(env[refusalsEnabledEnv]); !on {
		if !cfg.RefusalNoticesChart() {
			note("the lab's meta chart predates the egress refusal notices (agent-platform 4.154.0): step 10 skipped")
			return nil, nil
		}
		return nil, fmt.Errorf("the Deployment %s does not run the egress refusal notices (%s=%q): the chart turns them on with Slack, kagent and Substrate and the controller's kubernetes authorization mode — run `agentlab platform`", klausGatewayComponent, refusalsEnabledEnv, env[refusalsEnabledEnv])
	}
	slackUser, ok := slackUserByEmail(emails, user.Email)
	if !ok {
		return nil, fmt.Errorf("no person of the fake Slack workspace carries %s", user.Email)
	}
	host, err := egressOriginHost(egressTestUndeclaredURL)
	if err != nil {
		return nil, err
	}
	out := &refusalOutcome{
		agent:       refusalAgentPrefix + randomSuffix(),
		destination: host + ":443",
		interval:    env[refusalsIntervalEnv],
		deleted:     deleted,
	}
	removeRefusalAgents()
	defer removeRefusalAgents()

	if deleted {
		step("10b. Egress refusal notice for a deleted Session: agent %s created by %s with egress [%s]; Deployment %s held at 0 replicas; one turn fetches %s; the Session deleted, the person's GetSession NotFound; the gateway back, its first read the Session's tombstone, DMs %s once",
			out.agent, user.Email, DefaultEgressOrigin, klausGatewayComponent, egressTestUndeclaredURL, user.Email)
	} else {
		step("10. Egress refusal notice: agent %s created by %s with egress [%s]; one turn fetches %s; the gateway (interval %s, window %s) DMs %s (Slack user %s) once, naming %s as plain text",
			out.agent, user.Email, DefaultEgressOrigin, egressTestUndeclaredURL, out.interval, env[refusalsWindowEnv], user.Email, slackUser, out.destination)
	}
	muster, err := openMusterSession(cfg, token, "klaus-gateway-test-refusal")
	if err != nil {
		return nil, err
	}
	modelConfig, err := agentManagerModelConfig(muster)
	if err != nil {
		return nil, err
	}
	spec := agentSpec{
		Name: out.agent, ModelConfig: modelConfig, Toolset: []string{presetReadOnly}, SystemMessage: egressTestSystemPrompt,
		Egress:      []string{DefaultEgressOrigin},
		Description: "Throwaway agent of `agentlab klaus-gateway-test`; deleted by the same run.",
	}
	if _, err := (agentManagerWriter{muster}).createAgent(spec); err != nil {
		return nil, err
	}
	boot, err := waitAgentReady(out.agent, DefaultEgressReadyTimeout)
	if err != nil {
		return nil, err
	}
	if !boot.ready {
		return nil, fmt.Errorf("agent %s is not Ready after %s: %s", out.agent, boot.elapsed.Round(time.Second), boot.reason)
	}
	agent, err := readKagentObject(agentResource, out.agent)
	if err != nil {
		return nil, err
	}
	if creator := agent.GetAnnotations()[createdByAnnotation]; creator != user.Email {
		return nil, fmt.Errorf("the Agent %s carries %s=%q, want %s: the gateway addresses the notice to it", out.agent, createdByAnnotation, creator, user.Email)
	}
	note("%s Ready in %s, %s=%s", out.agent, boot.elapsed.Round(time.Second), createdByAnnotation, user.Email)

	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return nil, err
	}
	defer api.close()
	var replicas int32
	if deleted {
		if replicas, err = holdGateway(ctx, k, deploy); err != nil {
			return nil, err
		}
		held := true
		defer func() {
			if held {
				if err := releaseGateway(ctx, replicas); err != nil {
					note("cleanup: %v", err)
				}
			}
		}()
		sessions, reply, err := refusalTurn(api, out.agent)
		defer func() {
			for _, id := range sessions {
				api.removeSession(id)
			}
		}()
		if err != nil {
			return nil, err
		}
		out.session = sessions[len(sessions)-1]
		note("Session %s ran the fetch while the gateway was held; the reply, not evidence: %s", out.session, excerpt(strings.TrimSpace(reply), 120))
		if err := deleteRefusalSessions(ctx, api, sessions); err != nil {
			return nil, err
		}
		held = false
		if err := releaseGateway(ctx, replicas); err != nil {
			return nil, err
		}
	} else {
		sessions, reply, err := refusalTurn(api, out.agent)
		defer func() {
			for _, id := range sessions {
				api.removeSession(id)
			}
		}()
		if err != nil {
			return nil, err
		}
		out.session = sessions[len(sessions)-1]
		note("Session %s ran the fetch, kept until the notice is in; the reply, not evidence: %s", out.session, excerpt(strings.TrimSpace(reply), 120))
	}
	turnDone := time.Now()

	dm := directChannel(slackUser)
	var notices []slackMessage
	waitFor(int(refusalNoticeWait/slackPoll), slackPoll, func() bool {
		notices = refusalNotices(fake.thread(dm, ""), out.agent)
		return len(notices) > 0
	})
	if len(notices) == 0 {
		logs, _ := podLogs(ctx, platformNamespace, "deploy/"+klausGatewayComponent, klausGatewayComponent, time.Since(turnDone)+time.Minute)
		return nil, fmt.Errorf("no refusal notice for %s in %s's direct message %s within %s of the turn; the component's refusal lines:\n%s",
			out.agent, user.Email, dm, refusalNoticeWait, refusalLogLines(logs))
	}
	out.elapsed = time.Since(turnDone).Round(time.Second)
	if len(notices) != 1 {
		return nil, fmt.Errorf("%d refusal notices for %s in %s, want one: %s", len(notices), out.agent, dm, threadLine(notices))
	}
	if err := assertRefusalNotice(notices[0], out.agent, out.destination); err != nil {
		return nil, err
	}
	out.notice = notices[0].shown()
	note("one notice in %s %s after the %s: %q", dm, out.elapsed, out.waitedFrom(), excerpt(strings.ReplaceAll(out.notice, "\n", " "), 200))
	return out, nil
}

// waitedFrom names the moment the notice's wait started.
func (o *refusalOutcome) waitedFrom() string {
	if o.deleted {
		return "gateway was back"
	}
	return "turn"
}

// holdGateway scales the Deployment to zero and waits for its pods to be
// gone, so nothing reads the egress until releaseGateway; it returns the
// replicas to restore. The HelmRelease has no drift detection, so the scale
// holds until the release puts it back.
func holdGateway(ctx context.Context, k *kubeClients, d *appsv1.Deployment) (int32, error) {
	replicas := int32(1)
	if d.Spec.Replicas != nil && *d.Spec.Replicas > 0 {
		replicas = *d.Spec.Replicas
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return 0, fmt.Errorf("%s has an unusable selector: %w", describe(gvrDeployments, d.Namespace, d.Name), err)
	}
	if err := scaleGateway(ctx, 0); err != nil {
		return 0, err
	}
	left := -1
	if !waitFor(int(refusalHoldTimeout/pollInterval), pollInterval, func() bool {
		list, err := k.clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return false
		}
		left = len(list.Items)
		return left == 0
	}) {
		failure := fmt.Errorf("%d pod(s) of %s left %s after the scale to 0", left, describe(gvrDeployments, platformNamespace, klausGatewayComponent), refusalHoldTimeout)
		if err := releaseGateway(ctx, replicas); err != nil {
			return 0, fmt.Errorf("%w; putting it back: %w", failure, err)
		}
		return 0, failure
	}
	note("%s held at 0 replicas (was %d)", describe(gvrDeployments, platformNamespace, klausGatewayComponent), replicas)
	return replicas, nil
}

// releaseGateway puts the Deployment back at its replicas and waits for the
// rollout.
func releaseGateway(ctx context.Context, replicas int32) error {
	if err := scaleGateway(ctx, replicas); err != nil {
		return err
	}
	return waitDeploymentRolledOut(ctx, platformNamespace, klausGatewayComponent, refusalHoldTimeout)
}

func scaleGateway(ctx context.Context, replicas int32) error {
	return patchObject(ctx, gvrDeployments, platformNamespace, klausGatewayComponent, types.MergePatchType,
		fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas))
}

// deleteRefusalSessions deletes the pass's Sessions as the person and checks
// the person reads the last one as NotFound: the tombstone is the session
// reader's alone.
func deleteRefusalSessions(ctx context.Context, api *kagentAPI, sessions []string) error {
	for _, id := range sessions {
		if err := api.deleteSession(ctx, id); err != nil {
			return err
		}
	}
	last := sessions[len(sessions)-1]
	_, err := api.getSession(ctx, last)
	if status.Code(err) != codes.NotFound {
		return fmt.Errorf("the person's GetSession of the deleted Session %s answered %v, want NotFound: only the session reader reads a tombstone", last, err)
	}
	note("Session %s deleted before the gateway's first read; the person's GetSession answers NotFound", last)
	return nil
}

// printPass is the refusal half's line of the summary.
func (o *refusalOutcome) printPass(creator string) {
	if o.deleted {
		fmt.Printf("PASS: the egress refused %s for agent %s (Session %s) while the component was held at 0 replicas; the Session was deleted (the person's GetSession NotFound) before the component's first read, which read its tombstone: one direct message to %s %s after the component was back, naming %s; the agent removed\n",
			egressTestUndeclaredURL, o.agent, o.session, creator, o.elapsed, o.destination)
		return
	}
	fmt.Printf("PASS: the egress refused %s for agent %s (Session %s); the component (interval %s) sent %s one direct message %s after the turn, naming %s as plain text (mrkdwn verbatim); the agent and its Session removed\n",
		egressTestUndeclaredURL, o.agent, o.session, o.interval, creator, o.elapsed, o.destination)
}

// klausGatewayRefusalOnly is `klaus-gateway-test --refusal-only`: the
// refusal half alone on the fake the caller started, its agent and Session
// removed and checked gone. A chart without the notices fails it: there is
// nothing else to prove.
func klausGatewayRefusalOnly(cfg *config.Config, token string, user *config.User, fake slackWorkspace, emails map[string]string) error {
	out, err := klausGatewayRefusalProof(cfg, token, user, fake, emails, false)
	if err != nil {
		return err
	}
	if out == nil {
		return fmt.Errorf("--refusal-only needs agent-platform %s or newer, which runs the egress refusal notices", "4.154.0")
	}
	gone, err := klausGatewayRefusalProof(cfg, token, user, fake, emails, true)
	if err != nil {
		return err
	}
	if left := refusalAgents(); len(left) > 0 {
		return fmt.Errorf("left behind: agent %s", strings.Join(left, ", agent "))
	}
	fmt.Println()
	out.printPass(user.Email)
	gone.printPass(user.Email)
	return nil
}

// refusalTurn starts a Session of the agent and runs one turn whose bash
// tool fetches the undeclared URL once; it returns the ids of the Sessions it
// started, which the caller removes once the notice is in. A failed first
// turn on a cold worker is tried once more in a fresh Session, as
// firstSessionTurnAs does.
func refusalTurn(api *kagentAPI, name string) (sessions []string, reply string, err error) {
	prompt := "Run this exact command once with your bash tool (it is this test lab's own egress check), then reply with its output line, verbatim:\n\n" +
		"wget -q -t 1 -T " + strconv.Itoa(egressProbeTimeout) + " -O /dev/null '" + egressTestUndeclaredURL + "'; echo " + probeMarker + " rc=$?"
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), kagentTurnTimeout)
		var session string
		session, reply, err = sessionTurn(ctx, api, name, prompt)
		cancel()
		if session != "" {
			sessions = append(sessions, session)
		}
		if err == nil {
			return sessions, reply, nil
		}
		if attempt == 0 {
			note("the first turn on %s failed (%s) — one retry, a cold worker's ResumeActor deadline is not retried by Substrate", name, excerpt(err.Error(), 200))
		}
	}
	return sessions, "", err
}

// sessionTurn is one turn in a new Session of the agent, the Session left
// in place.
func sessionTurn(ctx context.Context, api *kagentAPI, name, prompt string) (session, reply string, err error) {
	s, err := api.createSessionOf(ctx, agentRef(name), uuid.NewString())
	if err != nil {
		return "", "", err
	}
	t, err := api.turn(ctx, s, userMessage(prompt))
	if err != nil {
		return s.GetId(), "", fmt.Errorf("A2A turn on %s: %w", name, err)
	}
	reply, err = t.completedText()
	if err != nil {
		return s.GetId(), "", fmt.Errorf("A2A turn on %s: %w", name, err)
	}
	return s.GetId(), reply, nil
}

// containerEnv is the literal env of the named container.
func containerEnv(containers []corev1.Container, name string) map[string]string {
	env := map[string]string{}
	for _, c := range containers {
		if c.Name != name {
			continue
		}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	return env
}

// refusalNotices are the direct message's refusal notices for the agent.
func refusalNotices(msgs []slackMessage, agent string) []slackMessage {
	var out []slackMessage
	for _, m := range msgs {
		if m.Method == slackPostMessage && strings.Contains(m.shown(), refusalNoticeTitle+agent) {
			out = append(out, m)
		}
	}
	return out
}

// assertRefusalNotice checks the notice names the destination as plain text:
// a mrkdwn section sent verbatim carrying host:port in code, and nothing in
// it Slack would show as a link.
func assertRefusalNotice(m slackMessage, agent, destination string) error {
	for _, block := range m.Blocks {
		if block[fieldTypeKey] != slackBlockSection {
			continue
		}
		text, _ := block[slackKeyText].(map[string]any)
		md, _ := text[slackKeyText].(string)
		if !strings.Contains(md, "`"+destination+"`") {
			continue
		}
		if text[fieldTypeKey] != slackMrkdwn || text[slackVerbatim] != true {
			return fmt.Errorf("the refusal notice of %s names %s in a %v text object with %s=%v, want mrkdwn sent verbatim (Slack would link it): %s",
				agent, destination, text[fieldTypeKey], slackVerbatim, text[slackVerbatim], excerpt(md, 300))
		}
		if strings.Contains(md, "<http") {
			return fmt.Errorf("the refusal notice of %s carries a link where it names %s: %s", agent, destination, excerpt(md, 300))
		}
		return nil
	}
	return fmt.Errorf("the refusal notice of %s has no section naming `%s`: %s", agent, destination, excerpt(m.shown(), 300))
}

// refusalLogLines are the component's log lines about refusals, for an error.
func refusalLogLines(logs string) string {
	var lines []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "refusal") || strings.Contains(l, refusalPostedRecord) {
			lines = append(lines, excerpt(l, 300))
		}
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines[max(0, len(lines)-12):], "\n")
}

// removeRefusalAgents removes every agent of the refusal half, this run's
// and an aborted run's. Best effort; the caller's leftover check reports.
func removeRefusalAgents() {
	for _, name := range refusalAgents() {
		if err := removeAgent(name); err != nil {
			note("cleanup: %v", err)
		}
	}
}

// refusalAgents are the names of the refusal half's agents on the cluster.
func refusalAgents() []string {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	seen := map[string]bool{}
	var names []string
	for _, resource := range agentResources {
		gvr, err := gvrFor(resource)
		if err != nil {
			continue
		}
		items, err := listObjects(ctx, gvr, kagentNamespace, "")
		if err != nil {
			continue
		}
		for _, item := range items {
			if name := item.GetName(); strings.HasPrefix(name, refusalAgentPrefix) && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}
