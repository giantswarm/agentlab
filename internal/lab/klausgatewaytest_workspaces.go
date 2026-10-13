package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The workspaces steps of the Swarmgeist proof, behind `klaus-gateway-test --workspaces`:
// the workspace a Slack thread's Session works in (docs/klaus-gateway.md
// "Workspaces per thread"). The gateway restarts on its stores with the
// workspace-manager's tools behind muster (--workspace-tools-prefix) and an
// Agent on the kagent Harness that may reach the lab's GitHub as its default,
// and the person and a colleague of the Organization are connected to the
// provider instance fake. On two workspaces — one over public repositories,
// one holding a private repository only the person reads (where the App of
// the lab's GitHub is installed on its owner) — the proof drives:
//
//   - a new thread asked once: the prompt offers the person's workspaces, the
//     pick starts a Session on the workspace's volume whose first turn lists
//     a clone of every repository in its session directory; two more turns
//     and a restart ask nothing and stay on that Session;
//   - No workspace: a Session in an empty working directory, no mirrors;
//   - the slash command's agent picker: its Workspace select preset to No
//     workspace, a submission with the workspace starting the Session in it
//     with no prompt in the thread; reopened, the preset is that workspace
//     with its last-used hint, and a change of agent changes the preset;
//   - the colleague picking the private workspace: refused naming the
//     repository, and no Session in kagent or agent-manager;
//   - the colleague allowed into the person's thread on the private
//     workspace: refused privately naming the repository, no turn reaching
//     the agent.

const (
	// gatewayWSAgent is the stage's Agent, the gateway's default while the
	// stage runs: on the kagent Harness, whose Sessions take collaborators,
	// with the lab's GitHub as its egress.
	gatewayWSAgent = klausGatewayTestAgent + "-ws"
	// gatewayWSPublic and gatewayWSPrivate are the stage's workspaces.
	gatewayWSPublic  = "agentlab-gateway-ws"
	gatewayWSPrivate = "agentlab-gateway-ws-private"
	// klausGatewayWorkspacesSince is the first klaus-gateway with the
	// workspace prompt, the picker's Workspace select and the collaborator's
	// workspace check.
	klausGatewayWorkspacesSince = "4.6.0-rc.1"
	// gatewayWSPwdCommand prints the working directory alone: the turns that
	// follow the first.
	gatewayWSPwdCommand   = `echo "` + probeMarker + ` pwd=$(pwd)"`
	gatewayWSProbePurpose = "the command only lists your working directory and its git clones."
	// recordWorkspaceChosen is the gateway's record of a prompt's choice.
	recordWorkspaceChosen = "workspace_chosen"
)

// gatewayWSPublicRepos are the public workspace's repositories, read by
// everyone; gatewayWSPrivateRepos the private workspace's: one the colleague
// reads, one only the person does (github-fixture.yaml).
var (
	gatewayWSPublicRepos  = []string{syncMovingRepo, syncListRepoB}
	gatewayWSPrivateRepos = []string{syncListRepoA, syncPrivateRepo}
)

// The gateway's Block Kit ids and words of the workspace prompt, the picker's
// Workspace select and the refusals the stage reads.
const (
	slackWorkspaceSelect = "workspace_select"
	slackWorkspaceNone   = "workspace_none"
	slackWorkspaceAsk    = "Which workspace should this conversation work in?"
	slackWorkspaceChosen = "Working in workspace *"
	slackWorkspaceNoneIn = "Working without a workspace"
	// slackWorkspaceRefused opens the refusal of a workspace the chooser
	// cannot read, slackCollaboratorRefused the collaborator's.
	slackWorkspaceRefused    = "You cannot start in workspace *"
	slackCollaboratorRefused = "which holds repositories you cannot read"
	slackAccessAllow         = "access_allow"
	// The picker's Workspace input: its block_id prefix (".<revision>"
	// follows), action, No workspace value and last-used hint.
	slackPickerWorkspaceBlock  = "ask_agent_workspace"
	slackPickerWorkspaceAction = "workspace"
	slackPickerWorkspaceNone   = "*none*"
	slackPickerLastUsed        = "Last used with"
)

// gatewayWorkspacesOutcome is what the stage proved, for the PASS lines.
type gatewayWorkspacesOutcome struct {
	session, noneSession, modalSession string
	repos                              []string
	volume                             string
	presets                            []string
	refusal, collaborator              string
	refusalTraces                      accessTraces
	privateSkipped                     string
}

// gatewayWorkspaces is the stage's state.
type gatewayWorkspaces struct {
	cfg     *config.Config
	timeout time.Duration
	sp      *syncProof
	p       *slackProof
	gw      *gatewayProcess
	people  slackPeople
	// person is the proof's user, api their kagent API; colleague the
	// second member of the Organization, who does not read the private
	// repository.
	person, colleague *workspacePerson
	api, colleagueAPI *kagentAPI
	public, private   workspaceVolume
	withPrivate       bool
	out               gatewayWorkspacesOutcome
	// cleaned is set once cleanup ran: the deferred call after the step's
	// own is a no-op.
	cleaned bool
}

// klausGatewayWorkspacesPreflight fails fast, before the proof signs anyone
// in, when the lab cannot run the workspaces steps: no workspace-manager behind muster, no
// provider instance fake, no Session admission, no colleague in the
// Organization.
func klausGatewayWorkspacesPreflight(cfg *config.Config, email string) error {
	if reason := workspaceManagerSkip(cfg); reason != "" {
		return fmt.Errorf("--workspaces needs the workspace-manager: %s (`agentlab configure --workspaces`, then `agentlab platform`)", reason)
	}
	if !cfg.WorkspaceFakeProvider() {
		return fmt.Errorf("--workspaces defines its workspaces on the lab's GitHub, the provider instance %s, which platform.workspaces.provider %q does not configure: `agentlab configure --workspaces-provider %s` (or fake,github), then `agentlab platform`",
			config.WorkspaceProviderFake, cfg.Platform.Workspaces.Provider, config.WorkspaceProviderFake)
	}
	if workspaceAdmissionValuesFor(cfg, true) == nil {
		return fmt.Errorf("--workspaces starts Sessions on a workspace volume, and this lab has no Session admission (platform.agents is off)")
	}
	for _, e := range []string{email, accessDefaultSecond} {
		u := cfg.FindUser(e)
		if u == nil {
			return fmt.Errorf("--workspaces needs the user %s in %s", e, config.File)
		}
		if !slices.ContainsFunc(config.WorkspaceOrganizationGroups, u.HasGroup) {
			return fmt.Errorf("--workspaces needs %s in the Organization %s (groups %s)", e, config.WorkspaceOrganization, strings.Join(config.WorkspaceOrganizationGroups, ", "))
		}
	}
	if strings.EqualFold(email, accessDefaultSecond) {
		return fmt.Errorf("--workspaces runs as a person beside the colleague %s: name another user", accessDefaultSecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	registered, err := objectExists(ctx, mcpServers, platformNamespace, workspaceManagerMCPServer)
	if err != nil {
		return err
	}
	if !registered {
		return fmt.Errorf("--workspaces needs the workspace-manager behind muster: no MCPServer %s in %s (`agentlab platform` registers it)", workspaceManagerMCPServer, platformNamespace)
	}
	return nil
}

// run is the workspaces steps against the running gateway, which it leaves running with
// the workspaces on; cleanup removes what it made.
func (ws *gatewayWorkspaces) run(user *config.User, keys gatewayKeys, linksPath string) (*gatewayWorkspacesOutcome, error) {
	cfg := ws.cfg
	step("W. Workspaces: %s connected to the provider instance %s, Agent %s on Harness %s (the gateway's default for this step), workspace %s over %s",
		user.Email, config.WorkspaceProviderFake, gatewayWSAgent, kagentHarness, gatewayWSPublic, strings.Join(gatewayWSPublicRepos, ", "))
	ws.sp = newSyncProof(cfg, ws.timeout, gatewayWSPublic, gatewayWSPrivate)
	if err := ws.sp.preconditions(user.Email); err != nil {
		return nil, err
	}
	token, err := ws.sp.gh.installationToken(syncOrg)
	switch {
	case isNoInstallation(err):
		ws.out.privateSkipped = fmt.Sprintf("the App of the lab's GitHub is not installed on %s, so no workspace holds a repository %s cannot read", syncOrg, accessDefaultSecond)
		note("%s: the refusal and the collaborator's steps are skipped", ws.out.privateSkipped)
	case err != nil:
		return nil, err
	default:
		ws.withPrivate, ws.sp.tokens[syncOrg] = true, token
	}
	if ws.person, err = openWorkspacePerson(cfg, user); err != nil {
		return nil, err
	}
	hops, err := connectWorkspaceProvider(cfg, ws.person, config.WorkspaceProviderFake)
	if err != nil {
		return nil, err
	}
	note("%s connected to %s: %s", user.Email, config.WorkspaceProviderFake, hops)
	if _, err := applyManifests(context.Background(), []byte(sessionsAgentManifest(gatewayWSAgent, kagentHarness, cfg.GitHubFakeURL()))); err != nil {
		return nil, fmt.Errorf("applying the Agent %s: %w", gatewayWSAgent, err)
	}
	boot, err := waitAgentReadyOn(gatewayWSAgent, kagentHarness, ws.timeout)
	if err != nil {
		return nil, err
	}
	if !boot.ready {
		return nil, boot.failure(gatewayWSAgent, ws.timeout)
	}
	note("the Agent %s Ready on Harness %s in %s", gatewayWSAgent, kagentHarness, boot.elapsed.Round(time.Second))
	if ws.public, err = ws.sp.createNamed(gatewayWSPublic, gatewayWSPublicRepos); err != nil {
		return nil, err
	}
	ws.out.volume, ws.out.repos = ws.public.Handle, gatewayWSPublicRepos

	var colleague *config.User
	var colleagueToken string
	if ws.withPrivate {
		step("W. The private workspace %s over %s, and the colleague %s connected to %s", gatewayWSPrivate, strings.Join(gatewayWSPrivateRepos, ", "), accessDefaultSecond, config.WorkspaceProviderFake)
		if ws.private, err = ws.sp.createNamed(gatewayWSPrivate, gatewayWSPrivateRepos); err != nil {
			return nil, err
		}
		colleague = cfg.FindUser(accessDefaultSecond)
		if ws.colleague, err = openWorkspacePerson(cfg, colleague); err != nil {
			return nil, err
		}
		if hops, err = connectWorkspaceProvider(cfg, ws.colleague, config.WorkspaceProviderFake); err != nil {
			return nil, err
		}
		note("%s connected to %s: %s", colleague.Email, config.WorkspaceProviderFake, hops)
		if colleagueToken, err = passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, colleague.Email, colleague.Password, musterLoginScopes); err != nil {
			return nil, err
		}
		if ws.colleagueAPI, err = dialKagentAPI(cfg, colleagueToken); err != nil {
			return nil, err
		}
	}

	step("W. Restarting klaus-gateway on its stores with the workspace-manager's tools (--workspace-tools-prefix=%s) and %s as its default agent", workspaceManagerToolPrefix, gatewayWSAgent)
	if err := ws.gw.stop(); err != nil {
		return nil, err
	}
	if colleague != nil {
		identity, err := tokenIdentity(colleagueToken, time.Now())
		if err != nil {
			return nil, fmt.Errorf("the id_token of %s: %w", colleague.Email, err)
		}
		if err := seedBoltLink(linksPath, keys.store, ws.people.colleague, identity.link(colleague.Email, colleagueToken)); err != nil {
			return nil, err
		}
		note("Slack user %s linked to %s", ws.people.colleague, colleague.Email)
	}
	ws.gw.extraArgs = append(ws.gw.extraArgs, "--workspace-tools-prefix="+workspaceManagerToolPrefix)
	ws.gw.defaultAgent = gatewayWSAgent
	if err := ws.gw.start(); err != nil {
		return nil, fmt.Errorf("restarting the gateway with workspaces: %w", err)
	}
	if v := ws.gw.version(); !gatewayAtLeast(v, klausGatewayWorkspacesSince) {
		return nil, fmt.Errorf("%s predates klaus-gateway %s, the first with the workspace prompt, the picker's Workspace select and the collaborator's workspace check: --gateway-image names a newer one", v, klausGatewayWorkspacesSince)
	}
	note("ready: %s", ws.gw.version())

	steps := []func() error{ws.prompted, ws.noWorkspace, ws.picker}
	if ws.withPrivate {
		steps = append(steps, ws.refused, ws.collaborator)
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return nil, err
		}
	}
	return &ws.out, nil
}

// prompted is Wa to Wc: a new thread asked once, the pick starting the
// Session in the workspace, two more turns and a restart asking nothing.
func (ws *gatewayWorkspaces) prompted() error {
	step("Wa. A new thread is asked once: the prompt offers %s; picking it starts a Session on its volume %s whose first turn lists a clone of every repository in its session directory", gatewayWSPublic, ws.public.Handle)
	t := &slackThread{user: ws.people.person}
	prompt, err := ws.ask(t, probePrompt(sessionsResumeCommand, ""))
	if err != nil {
		return err
	}
	offered := promptWorkspaces(prompt)
	if _, ok := offered[gatewayWSPublic]; !ok {
		return fmt.Errorf("the workspace prompt offers %v, not %s", slices.Sorted(maps.Keys(offered)), gatewayWSPublic)
	}
	before := len(ws.p.fake.thread(ws.p.channel, t.ts))
	if err := ws.p.driver.pick(t.user, prompt, slackWorkspaceSelect, offered[gatewayWSPublic]); err != nil {
		return err
	}
	turn, err := ws.p.awaitTurn(t, before)
	if err != nil {
		return fmt.Errorf("the thread's first turn after the pick: %w", err)
	}
	session, err := ws.onVolume(t, ws.public, gatewayWSPublic)
	if err != nil {
		return err
	}
	if err := ws.listed(t, turn, session); err != nil {
		return err
	}
	if err := ws.chosenOnce(t, slackWorkspaceChosen+gatewayWSPublic+"*"); err != nil {
		return err
	}
	ws.out.session = session.GetId()
	note("the prompt offered %s; the pick bound the thread to Session %s on volume %s, which lists %s in /workspace and holds its directory %s/%s on the volume",
		strings.Join(slices.Sorted(maps.Keys(offered)), ", "), session.GetId(), ws.public.Handle, strings.Join(gatewayWSPublicRepos, ", "), workspacesTestSessions, session.GetId())

	step("Wb. Two more turns in the thread ask nothing and reuse Session %s", session.GetId())
	for i := range 2 {
		if err := ws.reused(t, session.GetId()); err != nil {
			return fmt.Errorf("turn %d after the first: %w", i+2, err)
		}
	}
	note("both turns ran on %s in /workspace, no prompt posted again", session.GetId())

	step("Wc. A restart on the stores keeps the choice: the next turn asks nothing and stays on %s", session.GetId())
	if err := ws.gw.stop(); err != nil {
		return err
	}
	if err := ws.gw.start(); err != nil {
		return fmt.Errorf("restarting the gateway: %w", err)
	}
	if err := ws.reused(t, session.GetId()); err != nil {
		return fmt.Errorf("the turn after the restart: %w", err)
	}
	note("after the restart the turn ran on %s in /workspace, no prompt", session.GetId())
	return nil
}

// noWorkspace is Wd: No workspace starts a Session in an empty directory.
func (ws *gatewayWorkspaces) noWorkspace() error {
	step("Wd. No workspace: a new thread's No workspace button starts a Session with no volume, in an empty working directory and no mirrors")
	t := &slackThread{user: ws.people.person}
	prompt, err := ws.ask(t, probePrompt(sessionsEmptyCommand, ""))
	if err != nil {
		return err
	}
	before := len(ws.p.fake.thread(ws.p.channel, t.ts))
	if err := ws.p.driver.click(t.user, prompt, slackWorkspaceNone); err != nil {
		return err
	}
	turn, err := ws.p.awaitTurn(t, before)
	if err != nil {
		return fmt.Errorf("the turn after No workspace: %w", err)
	}
	session, err := ws.p.boundSession(ws.api, t)
	if err != nil {
		return err
	}
	if vs := session.GetVolumeSource(); vs != nil {
		return fmt.Errorf("the Session %s started with No workspace carries volume %q", session.GetId(), vs.GetVolume().GetVolumeHandle())
	}
	v, err := ws.probed(t, turn, parseSessionsTurn)
	if err != nil {
		return err
	}
	if v.entries != "" || v.mirrors != probeAbsent {
		return fmt.Errorf("the Session %s with No workspace works in %s holding [%s], mirrors %s: want an empty directory and no mirrors", session.GetId(), v.pwd, v.entries, v.mirrors)
	}
	if err := ws.chosenOnce(t, slackWorkspaceNoneIn); err != nil {
		return err
	}
	ws.out.noneSession = session.GetId()
	note("Session %s, no volume, works in an empty %s with no mirrors", session.GetId(), v.pwd)
	return nil
}

// picker is We: the slash command's agent picker and its Workspace select.
func (ws *gatewayWorkspaces) picker() error {
	step("We. The slash command's agent picker: its Workspace select preset to No workspace for %s; a submission with %s starts the Session in it and posts no prompt; reopened, the preset is %s, and a change of agent changes it", gatewayWSAgent, gatewayWSPublic, gatewayWSPublic)
	user := ws.people.person
	view, err := ws.openPicker(user)
	if err != nil {
		return err
	}
	ref, otherRef, err := pickerRefs(view)
	if err != nil {
		return err
	}
	if err := ws.preset(view, slackPickerWorkspaceNone, false, "opened for "+gatewayWSAgent); err != nil {
		return err
	}
	block, _, _, _ := pickerWorkspace(view)
	roots := len(ws.p.fake.thread(ws.p.channel, ""))
	if err := ws.p.driver.submitView(user, view, map[string]any{
		slackPickerAgentBlock:    map[string]any{slackPickerAgentAction: map[string]any{fieldTypeKey: slackStaticSelect, slackSelectedOption: map[string]any{slackKeyValue: ref}}},
		slackPickerQuestionBlock: map[string]any{slackPickerQuestionAction: map[string]any{fieldTypeKey: slackPlainTextInput, slackKeyValue: probePrompt(sessionsResumeCommand, "")}},
		block:                    map[string]any{slackPickerWorkspaceAction: map[string]any{fieldTypeKey: slackStaticSelect, slackSelectedOption: map[string]any{slackKeyValue: gatewayWSPublic}}},
	}); err != nil {
		return err
	}
	var echo slackMessage
	if _, ok := waitThread(ws.p.fake, ws.p.channel, "", klausGatewayReplyWait, func(msgs []slackMessage) bool {
		if len(msgs) > roots {
			echo = msgs[len(msgs)-1]
		}
		return echo.TS != ""
	}); !ok {
		return fmt.Errorf("the picker's submission posted no conversation root within %s", klausGatewayReplyWait)
	}
	t := &slackThread{ts: echo.TS, user: user}
	turn, err := ws.p.awaitTurn(t, 0)
	if err != nil {
		return fmt.Errorf("the picker's conversation: %w", err)
	}
	session, err := ws.onVolume(t, ws.public, gatewayWSPublic)
	if err != nil {
		return err
	}
	if err := ws.listed(t, turn, session); err != nil {
		return err
	}
	if asked := workspacePrompts(ws.p.fake.thread(ws.p.channel, t.ts)); asked > 0 {
		return fmt.Errorf("the picker's conversation was asked for its workspace %d time(s): the submission's choice did not start the Session", asked)
	}
	ws.out.modalSession = session.GetId()
	ws.out.presets = append(ws.out.presets, "No workspace on the first open")
	note("the submission started Session %s on volume %s with no prompt in its thread; its first turn lists %s", session.GetId(), ws.public.Handle, strings.Join(gatewayWSPublicRepos, ", "))

	view, err = ws.openPicker(user)
	if err != nil {
		return err
	}
	if err := ws.preset(view, gatewayWSPublic, true, "reopened for "+gatewayWSAgent); err != nil {
		return err
	}
	for _, change := range []struct {
		ref, want string
		lastUsed  bool
	}{{otherRef, slackPickerWorkspaceNone, false}, {ref, gatewayWSPublic, true}} {
		if view, err = ws.changeAgent(user, view, change.ref); err != nil {
			return err
		}
		if err := ws.preset(view, change.want, change.lastUsed, "after the change of agent to "+change.ref); err != nil {
			return err
		}
	}
	ws.out.presets = append(ws.out.presets, gatewayWSPublic+" (last used) on reopening", "No workspace after a change to "+otherRef, gatewayWSPublic+" again after the change back")
	note("presets: %s", strings.Join(ws.out.presets, "; "))
	return nil
}

// refused is Wf: the colleague's pick of the private workspace refused.
func (ws *gatewayWorkspaces) refused() error {
	step("Wf. %s picks %s in a new thread: refused naming %s, and no Session of %s exists for them in kagent or agent-manager", accessDefaultSecond, gatewayWSPrivate, syncPrivateRepo, gatewayWSAgent)
	t := &slackThread{user: ws.people.colleague}
	prompt, err := ws.ask(t, klausGatewayWordPrompt)
	if err != nil {
		return err
	}
	offered := promptWorkspaces(prompt)
	if _, ok := offered[gatewayWSPrivate]; !ok {
		return fmt.Errorf("the workspace prompt offers %s %v, not %s", accessDefaultSecond, slices.Sorted(maps.Keys(offered)), gatewayWSPrivate)
	}
	before, err := sessionTraces(ws.colleague, ws.colleagueAPI, ws.api, gatewayWSAgent)
	if err != nil {
		return err
	}
	shown := len(ws.p.fake.thread(ws.p.channel, t.ts))
	if err := ws.p.driver.pick(t.user, prompt, slackWorkspaceSelect, offered[gatewayWSPrivate]); err != nil {
		return err
	}
	refusal, err := ws.privately(t.ts, t.user, shown, slackWorkspaceRefused+gatewayWSPrivate+"*")
	if err != nil {
		return err
	}
	if err := namesOnly(refusal, syncPrivateRepo, syncListRepoA); err != nil {
		return err
	}
	time.Sleep(accessTraceSettle)
	after, err := sessionTraces(ws.colleague, ws.colleagueAPI, ws.api, gatewayWSAgent)
	if err != nil {
		return err
	}
	if err := judgeNothingNew(before, after); err != nil {
		return fmt.Errorf("after the refusal of %s: %w", accessDefaultSecond, err)
	}
	if bound := ws.p.records(recordBound, t.ts); len(bound) > 0 {
		return fmt.Errorf("the refused thread %s is bound to Session %s", t.ts, bound[0].Session)
	}
	ws.out.refusal, ws.out.refusalTraces = excerpt(refusal, 200), after
	note("refused privately: %q; kagent lists %d Session(s) of %s for %s, agent-manager's list_sessions %d, Substrate %d actors — none new, the thread bound to nothing",
		excerpt(refusal, 160), len(after.kagent), gatewayWSAgent, accessDefaultSecond, len(after.manager), len(after.actors))
	return nil
}

// collaborator is Wg: the colleague allowed into a thread on the private
// workspace, refused privately, no turn reaching the agent.
func (ws *gatewayWorkspaces) collaborator() error {
	step("Wg. %s allows %s into a thread on %s: the colleague's turn is refused privately naming %s, and no turn reaches the agent", ws.person.user.Email, accessDefaultSecond, gatewayWSPrivate, syncPrivateRepo)
	t := &slackThread{user: ws.people.person}
	prompt, err := ws.ask(t, klausGatewayWordPrompt)
	if err != nil {
		return err
	}
	offered := promptWorkspaces(prompt)
	before := len(ws.p.fake.thread(ws.p.channel, t.ts))
	if err := ws.p.driver.pick(t.user, prompt, slackWorkspaceSelect, offered[gatewayWSPrivate]); err != nil {
		return err
	}
	turn, err := ws.p.awaitTurn(t, before)
	if err != nil {
		return fmt.Errorf("the turn on %s: %w", gatewayWSPrivate, err)
	}
	if err := assertSlackTurnSaid(turn, klausGatewayWord); err != nil {
		return fmt.Errorf("the turn on %s: %w", gatewayWSPrivate, err)
	}
	session, err := ws.onVolume(t, ws.private, gatewayWSPrivate)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	tasks, err := ws.api.taskIDs(ctx, session)
	cancel()
	if err != nil {
		return err
	}

	shown := len(ws.p.fake.thread(ws.p.channel, t.ts))
	if _, err := ws.p.driver.mention(ws.people.colleague, t.ts, klausGatewayWordPrompt); err != nil {
		return err
	}
	consent, err := ws.privatelyMsg(t.ts, ws.people.person, shown, func(m slackMessage) bool { _, ok := m.action(slackAccessAllow); return ok })
	if err != nil {
		return fmt.Errorf("%s was not asked to allow %s: %w", ws.person.user.Email, accessDefaultSecond, err)
	}
	shown = len(ws.p.fake.thread(ws.p.channel, t.ts))
	if err := ws.p.driver.click(ws.people.person, consent, slackAccessAllow); err != nil {
		return err
	}
	refusal, err := ws.privately(t.ts, ws.people.colleague, shown, slackCollaboratorRefused)
	if err != nil {
		return err
	}
	if err := namesOnly(refusal, syncPrivateRepo, syncListRepoA); err != nil {
		return err
	}
	time.Sleep(accessTraceSettle)
	for _, d := range ws.p.records(recordDispatch, t.ts) {
		if d.SlackUser == ws.people.colleague {
			return fmt.Errorf("a turn of %s reached the agent in thread %s (turn_dispatch, task %s)", accessDefaultSecond, t.ts, d.TaskID)
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), kubeReadTimeout)
	after, err := ws.api.taskIDs(ctx, session)
	cancel()
	if err != nil {
		return err
	}
	if len(after) != len(tasks) {
		return fmt.Errorf("the Session %s holds %d task(s) after the colleague's refused turn, %d before", session.GetId(), len(after), len(tasks))
	}
	ws.out.collaborator = excerpt(refusal, 200)
	note("allowed, then refused privately: %q; no turn_dispatch of %s, Session %s still at %d task(s)", excerpt(refusal, 160), ws.people.colleague, session.GetId(), len(after))
	return nil
}

// ask opens a thread with text and returns the workspace prompt it gets:
// before the prompt, no Session is bound and no turn dispatched.
func (ws *gatewayWorkspaces) ask(t *slackThread, text string) (slackMessage, error) {
	ts, err := ws.p.driver.mention(t.user, "", text)
	if err != nil {
		return slackMessage{}, err
	}
	t.ts = ts
	var prompt slackMessage
	msgs, ok := waitThread(ws.p.fake, ws.p.channel, ts, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		for _, m := range msgs {
			if _, found := m.action(slackWorkspaceSelect); found {
				prompt = m
				return true
			}
		}
		return false
	})
	if !ok {
		return slackMessage{}, fmt.Errorf("the new thread of %s was not asked for its workspace within %s; the thread shows: %s", t.user, klausGatewayReplyWait, threadLine(msgs))
	}
	if !strings.Contains(prompt.shown(), slackWorkspaceAsk) || prompt.Method != slackPostMessage {
		return slackMessage{}, fmt.Errorf("the workspace prompt is a %s reading %q, want a thread post asking %q", prompt.Method, excerpt(prompt.shown(), 200), slackWorkspaceAsk)
	}
	if n := len(ws.p.records(recordDispatch, ts)) + len(ws.p.records(recordBound, ts)); n > 0 {
		return slackMessage{}, fmt.Errorf("thread %s ran a turn before its workspace was chosen", ts)
	}
	return prompt, nil
}

// onVolume is the thread's Session, which must be on the workspace's volume.
func (ws *gatewayWorkspaces) onVolume(t *slackThread, volume workspaceVolume, workspace string) (*apiv1alpha1.Session, error) {
	session, err := ws.p.boundSession(ws.api, t)
	if err != nil {
		return nil, err
	}
	if got := session.GetVolumeSource().GetVolume().GetVolumeHandle(); got != volume.Handle {
		return nil, fmt.Errorf("the Session %s of thread %s is on volume %q, want %s's %s", session.GetId(), t.ts, got, workspace, volume.Handle)
	}
	return session, nil
}

// listed checks the first turn of a Session on the public workspace:
// /workspace holding exactly a clone of every repository, and the Session's
// directory on the volume, its setup record naming the same clones.
func (ws *gatewayWorkspaces) listed(t *slackThread, turn *slackTurn, session *apiv1alpha1.Session) error {
	repos := slices.Sorted(slices.Values(gatewayWSPublicRepos))
	v, err := ws.probed(t, turn, parseSessionsTurn)
	if err != nil {
		return err
	}
	if got := slices.Sorted(maps.Keys(v.heads)); v.pwd != workspacesTestMountPath || !slices.Equal(got, repos) {
		return fmt.Errorf("the Session %s works in %s with the clones %v, want %s with %v", session.GetId(), v.pwd, got, workspacesTestMountPath, repos)
	}
	out, err := ws.sp.onVolume(gatewayWSPublic, "gateway-ws-dirs", sessionsDirectoriesScript)
	if err != nil {
		return err
	}
	dirs, err := parseSessionDirectories(out)
	if err != nil {
		return err
	}
	d, ok := dirs[session.GetId()]
	switch {
	case !ok:
		return fmt.Errorf("the volume of %s holds no directory %s/%s for the Session (it holds %v)", gatewayWSPublic, workspacesTestSessions, session.GetId(), slices.Sorted(maps.Keys(dirs)))
	case d.state == nil:
		return fmt.Errorf("the session directory %s/%s holds no setup record %s", workspacesTestSessions, session.GetId(), workspaceStateFile)
	case !slices.Equal(slices.Sorted(slices.Values(d.state.Cloned)), repos):
		return fmt.Errorf("the session directory %s/%s records the clones %v, want %v", workspacesTestSessions, session.GetId(), d.state.Cloned, repos)
	}
	return nil
}

// probed parses a probe turn's streamed answer, asking once more in the
// thread when the agent relayed no marker line.
func (ws *gatewayWorkspaces) probed(t *slackThread, turn *slackTurn, parse func(string) (sessionsTurnValues, error)) (sessionsTurnValues, error) {
	if turn.record.Outcome != outcomeCompleted {
		return sessionsTurnValues{}, fmt.Errorf("the probe turn did not complete: %s", turnFailure(turn))
	}
	v, err := parse(turn.answer)
	if err == nil {
		return v, nil
	}
	note("the agent relayed no probe line (%s); asking once more in the thread", excerpt(err.Error(), 160))
	again, err := ws.p.say(t, probeFollowUp(gatewayWSProbePurpose))
	if err != nil {
		return sessionsTurnValues{}, err
	}
	return parse(again.answer)
}

// reused runs a turn in the thread that must ask nothing and stay on the
// Session, in /workspace.
func (ws *gatewayWorkspaces) reused(t *slackThread, session string) error {
	turn, err := ws.p.say(t, probePrompt(gatewayWSPwdCommand, ""))
	if err != nil {
		return err
	}
	v, err := ws.probed(t, turn, parseSessionsTurn)
	if err != nil {
		return err
	}
	if v.pwd != workspacesTestMountPath {
		return fmt.Errorf("the turn ran in %s, not %s", v.pwd, workspacesTestMountPath)
	}
	bound := ws.p.records(recordBound, t.ts)
	if len(bound) != 1 || bound[0].Session != session {
		return fmt.Errorf("thread %s has %d session_bound record(s), want one for %s", t.ts, len(bound), session)
	}
	if asked := workspacePrompts(ws.p.fake.thread(ws.p.channel, t.ts)); asked > 0 {
		return fmt.Errorf("thread %s was asked for its workspace again", t.ts)
	}
	return nil
}

// chosenOnce checks the thread's prompt was answered once: one
// workspace_chosen record, the prompt rewritten to the choice, no open
// prompt left.
func (ws *gatewayWorkspaces) chosenOnce(t *slackThread, notice string) error {
	if chosen := ws.p.records(recordWorkspaceChosen, t.ts); len(chosen) != 1 {
		return fmt.Errorf("thread %s has %d %s record(s), want one", t.ts, len(chosen), recordWorkspaceChosen)
	}
	msgs := ws.p.fake.thread(ws.p.channel, t.ts)
	if _, ok := findMessage(msgs, notice); !ok {
		return fmt.Errorf("the prompt of thread %s was not rewritten to %q: %s", t.ts, notice, threadLine(msgs))
	}
	if asked := workspacePrompts(msgs); asked > 0 {
		return fmt.Errorf("thread %s still shows %d open workspace prompt(s)", t.ts, asked)
	}
	return nil
}

// privately waits for a message shown to user alone in the thread after its
// first shown messages, carrying needle, and returns its text.
func (ws *gatewayWorkspaces) privately(thread, user string, shown int, needle string) (string, error) {
	m, err := ws.privatelyMsg(thread, user, shown, func(m slackMessage) bool { return strings.Contains(m.shown(), needle) })
	if err != nil {
		return "", fmt.Errorf("%s got no private answer carrying %q: %w", user, needle, err)
	}
	// The text is the blocks' fallback, the same words: one of them.
	return cmp.Or(m.Text, m.shown()), nil
}

// privatelyMsg waits for an ephemeral message to user in the thread after its
// first shown messages that match holds for.
func (ws *gatewayWorkspaces) privatelyMsg(thread, user string, shown int, match func(slackMessage) bool) (slackMessage, error) {
	var found slackMessage
	msgs, ok := waitThread(ws.p.fake, ws.p.channel, thread, klausGatewayReplyWait, func(msgs []slackMessage) bool {
		for _, m := range msgs[min(shown, len(msgs)):] {
			if m.Method == slackPostEphemeral && m.Recipient == user && match(m) {
				found = m
				return true
			}
		}
		return false
	})
	if !ok {
		return slackMessage{}, fmt.Errorf("nothing within %s; the thread shows: %s", klausGatewayReplyWait, threadLine(msgs))
	}
	return found, nil
}

// openPicker types the slash command as user and returns the agent picker it
// opens, with its Workspace select: a picker opened without it (the listing
// missed the trigger's budget on a cold path) is opened once more.
func (ws *gatewayWorkspaces) openPicker(user string) (map[string]any, error) {
	var view map[string]any
	for attempt := range 2 {
		before := len(ws.p.fake.openedViews())
		if err := ws.p.driver.slashCommand(user, ""); err != nil {
			return nil, err
		}
		view = nil
		if !waitFor(int(klausGatewayReplyWait/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
			if views := ws.p.fake.openedViews(); len(views) > before {
				view = views[len(views)-1]
			}
			return view != nil
		}) {
			return nil, fmt.Errorf("%s opened no picker within %s", slackCommand, klausGatewayReplyWait)
		}
		if view[slackKeyCallbackID] != slackPickerCallback {
			return nil, fmt.Errorf("%s opened modal %v, not the agent picker %s", slackCommand, view[slackKeyCallbackID], slackPickerCallback)
		}
		if _, _, _, ok := pickerWorkspace(view); ok {
			return view, nil
		}
		if attempt == 0 {
			note("the picker opened without its Workspace select (the listing missed the trigger's budget); opening it again")
		}
	}
	return nil, fmt.Errorf("the agent picker opened twice without a Workspace select: the gateway did not list %s's workspaces in time", user)
}

// changeAgent picks ref in the open picker's agent select and returns the
// view as the gateway redrew it.
func (ws *gatewayWorkspaces) changeAgent(user string, view map[string]any, ref string) (map[string]any, error) {
	before := len(ws.p.fake.openedViews())
	if err := ws.p.driver.pickInView(user, view, slackPickerAgentBlock, slackPickerAgentAction, ref); err != nil {
		return nil, err
	}
	var redrawn map[string]any
	if !waitFor(int(klausGatewayReplyWait/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
		if views := ws.p.fake.openedViews(); len(views) > before {
			redrawn = views[len(views)-1]
		}
		return redrawn != nil
	}) {
		return nil, fmt.Errorf("the change of agent to %s did not redraw the picker within %s", ref, klausGatewayReplyWait)
	}
	return redrawn, nil
}

// preset checks the picker's Workspace select opens on want, with the
// last-used hint exactly when lastUsed.
func (ws *gatewayWorkspaces) preset(view map[string]any, want string, lastUsed bool, when string) error {
	_, got, hint, _ := pickerWorkspace(view)
	if got != want || strings.Contains(hint, slackPickerLastUsed) != lastUsed {
		return fmt.Errorf("the picker's Workspace select %s opens on %q with hint %q, want %q (last-used hint %t)", when, got, hint, want, lastUsed)
	}
	return nil
}

// cleanup removes what the stage made, best effort past the first failure:
// the Agent's Sessions of both people, the Agent, the session directories,
// the workspaces, the moving repository's head, the provider sign-ins.
func (ws *gatewayWorkspaces) cleanup() error {
	if ws.cleaned {
		return nil
	}
	ws.cleaned = true
	var errs []error
	for _, api := range []*kagentAPI{ws.api, ws.colleagueAPI} {
		if api == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		sessions, err := api.listSessionsOf(ctx, agentRef(gatewayWSAgent))
		cancel()
		errs = append(errs, err)
		for _, s := range sessions {
			api.removeSession(s.GetId())
		}
	}
	if ws.colleagueAPI != nil {
		ws.colleagueAPI.close()
	}
	if agentExists(gatewayWSAgent) {
		errs = append(errs, removeAgent(gatewayWSAgent))
	}
	if ws.sp != nil && ws.sp.member != nil {
		for _, name := range ws.sp.names {
			if gone, err := ws.sp.gone(name); err == nil && !gone {
				if _, err := ws.sp.volume(name); err == nil {
					_, err := ws.sp.onVolume(name, "gateway-ws-clear", accessClearSessionsScript)
					errs = append(errs, err)
				}
			}
		}
	}
	if ws.sp != nil {
		errs = append(errs, ws.sp.restore())
	}
	for _, p := range []*workspacePerson{ws.person, ws.colleague} {
		if p != nil {
			p.disconnect(config.WorkspaceProviderFake)
		}
	}
	return errors.Join(errs...)
}

// workspacePrompts counts the thread's open workspace prompts: messages that
// still carry the workspace select.
func workspacePrompts(msgs []slackMessage) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.action(slackWorkspaceSelect); ok {
			n++
		}
	}
	return n
}

// promptWorkspaces are the workspaces a prompt's select offers, each with
// its option's value (the thread and the workspace, as the gateway encodes
// them).
func promptWorkspaces(prompt slackMessage) map[string]string {
	out := map[string]string{}
	sel, ok := prompt.action(slackWorkspaceSelect)
	if !ok {
		return out
	}
	for _, opt := range selectOptions(sel) {
		raw, _ := opt[slackKeyValue].(string)
		var v struct {
			Workspace string `json:"w"`
		}
		if json.Unmarshal([]byte(raw), &v) == nil && v.Workspace != "" {
			out[v.Workspace] = raw
		}
	}
	return out
}

// selectOptions are a static select's options, flat or in option groups.
func selectOptions(element map[string]any) []map[string]any {
	var out []map[string]any
	collect := func(list any) {
		items, _ := list.([]any)
		for _, o := range items {
			if opt, ok := o.(map[string]any); ok {
				out = append(out, opt)
			}
		}
	}
	collect(element[slackKeyOptions])
	groups, _ := element["option_groups"].([]any)
	for _, g := range groups {
		if group, ok := g.(map[string]any); ok {
			collect(group[slackKeyOptions])
		}
	}
	return out
}

// pickerWorkspace reads the picker's Workspace input: its block_id, the value
// its select opens on and its hint; ok is false when the picker has none.
func pickerWorkspace(view map[string]any) (block, initial, hint string, ok bool) {
	blocks, _ := view[slackKeyBlocks].([]any)
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		id, _ := m[slackKeyBlockID].(string)
		if !strings.HasPrefix(id, slackPickerWorkspaceBlock+".") {
			continue
		}
		element, _ := m[slackKeyElement].(map[string]any)
		if opt, _ := element["initial_option"].(map[string]any); opt != nil {
			initial, _ = opt[slackKeyValue].(string)
		}
		if h, _ := m["hint"].(map[string]any); h != nil {
			hint, _ = h[slackKeyText].(string)
		}
		return id, initial, hint, true
	}
	return "", "", "", false
}

// pickerRefs are the picker's agent refs of the stage's Agent and of the
// fixture's.
func pickerRefs(view map[string]any) (ws, other string, err error) {
	offered := pickerOptions(view)
	for _, ref := range offered {
		switch {
		case strings.HasSuffix(ref, gatewayWSAgent):
			ws = ref
		case strings.HasSuffix(ref, klausGatewayTestAgent):
			other = ref
		}
	}
	if ws == "" || other == "" {
		return "", "", fmt.Errorf("the picker offers %s; wanted %s and %s", strings.Join(offered, ", "), gatewayWSAgent, klausGatewayTestAgent)
	}
	return ws, other, nil
}

// namesOnly checks a refusal names the unreadable repository and not the one
// the person reads.
func namesOnly(refusal, unreadable, readable string) error {
	if !strings.Contains(refusal, "`"+unreadable+"`") || strings.Contains(refusal, readable) {
		return fmt.Errorf("the refusal %q does not name %s alone (it must not name %s, which the person reads)", excerpt(refusal, 300), unreadable, readable)
	}
	return nil
}

// gatewayWorkspacesPASS words the stage's PASS lines.
func gatewayWorkspacesPASS(o *gatewayWorkspacesOutcome) []string {
	lines := []string{
		fmt.Sprintf("a new thread was asked once for its workspace; picking %s started Session %s on volume %s, whose first turn listed %s in its session directory; two more turns and a restart asked nothing and stayed on it",
			gatewayWSPublic, o.session, o.volume, strings.Join(o.repos, ", ")),
		fmt.Sprintf("No workspace started Session %s with no volume, in an empty working directory with no mirrors", o.noneSession),
		fmt.Sprintf("the slash command's picker carried the Workspace select (%s); its submission with %s started Session %s in it and posted no prompt",
			strings.Join(o.presets, "; "), gatewayWSPublic, o.modalSession),
	}
	if o.privateSkipped != "" {
		return append(lines, "the refusal and the collaborator's steps were skipped: "+o.privateSkipped)
	}
	return append(lines,
		fmt.Sprintf("%s picking %s was refused naming %s (%q); kagent listed %d Session(s) of %s for them, agent-manager's list_sessions %d — none new, no Session bound",
			accessDefaultSecond, gatewayWSPrivate, syncPrivateRepo, o.refusal, len(o.refusalTraces.kagent), gatewayWSAgent, len(o.refusalTraces.manager)),
		fmt.Sprintf("%s, allowed into a thread on %s, was refused privately (%q) and no turn of theirs reached the agent", accessDefaultSecond, gatewayWSPrivate, o.collaborator),
	)
}

// gatewayWSLeftovers lists what the stage's cleanup did not remove.
func gatewayWSLeftovers(ws *gatewayWorkspaces) []string {
	var left []string
	if agentExists(gatewayWSAgent) {
		left = append(left, agentResource+" "+gatewayWSAgent)
	}
	if ws.sp != nil && ws.sp.member != nil {
		for _, name := range ws.sp.names {
			if gone, err := ws.sp.gone(name); err == nil && !gone {
				left = append(left, "workspace "+name)
			}
		}
	}
	return left
}
