package lab

import (
	"context"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The skills proof: the golden boot under Substrate's egress gate.
//
// Under kagent API v2 every agent is a Substrate actor compiled into a golden
// snapshot: the controller writes an ActorTemplate per revision, Substrate
// boots one actor from it (the golden boot), waits for its readyz and
// snapshots it, and every turn resumes from that snapshot. The Go ADK harness
// materialises the template's skills when it starts — a git skill is
// `git fetch --depth 1 origin <commit>` of a full commit — before it serves
// readyz, and Substrate's atenet denies egress to an actor that is not
// RUNNING. Whether a template with a git skill boots at all is therefore the
// question every skill-carrying agent of the fleet hangs on. This proof asks
// it on the lab: an AgentTemplate on the platform's Go ADK Harness with one
// skill pinned to a full commit of a public repository, the Ready condition
// on the Harness, and on success one turn as the signed-in person that only
// the materialised skill can answer. A negative outcome is a finding, not a
// broken proof: the evidence it prints — the template's conditions and
// warnings, Substrate's actor and worker state as the controller reports it,
// the controller's, atenet's and the worker's log lines, the versions of the
// line — is what the line's upstream issue needs, and the same template
// without the skill is booted as the control. docs/platform.md "The skills
// proof (the golden boot)".

// Names of what the proof creates; both are deleted by the same run, and a
// leftover of an aborted run is removed first.
const (
	skillsTestAgent = "agentlab-skills-test"
	// skillsTestControlAgent is the same template without the skill, booted
	// only after a failed golden boot to show the skill is the difference.
	skillsTestControlAgent = skillsTestAgent + "-control"
)

// The default fixture: giantswarm/agent-skills, the public repository most of
// the fleet's skills come from, at a full commit; the skill whose SKILL.md
// names a fact the model cannot know otherwise (skillsTestFact), which the
// turn asks for without saying it. A SkillsFixture of the caller's puts
// another repository in its place — a private one, with the Secret whose
// token its host takes.
const (
	skillsTestRepo         = "https://github.com/giantswarm/agent-skills"
	skillsTestCommit       = "cb1fb768bbbbcaa035b884a99ad308b14f846468"
	skillsTestSkill        = "agent-self-awareness"
	skillsTestFact         = "klaus-gateway"
	skillsTestQuestion     = "which application bridges kagent with Slack?"
	skillsTestSystemPrompt = "You are a terse assistant of the agentlab skills proof. Use your skills when asked about them. Answer in one short line."
	// skillsCredentialKey is the Secret key the runtime reads a source's token
	// from (skills[].source.git.credentialRef.key).
	skillsCredentialKey = "token"
)

// SkillsFixture is the skill the proof boots and asks about: a git repository
// at a full commit, the skill's directory within it (its last element is the
// skill's name), the question the turn asks and the answer only the skill's
// text has — and, for a private repository, the Secret in the kagent
// namespace whose token key the runtime presents to the repository's host
// (skills[].source.git.credentialRef). The zero value is the public fixture.
type SkillsFixture struct {
	Repo, Commit, Skill, Question, Expect string
	// CredentialSecret names the Secret (key skillsCredentialKey); "" fetches
	// anonymously. The proof never reads the Secret.
	CredentialSecret string
}

var fullCommitID = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// resolve returns the public fixture for the zero value and checks a fixture
// of the caller's: every field but the Secret, an http(s) URL, a full commit.
func (f SkillsFixture) resolve() (SkillsFixture, error) {
	if f.Repo == "" && f.Commit == "" && f.Skill == "" && f.Question == "" && f.Expect == "" {
		f.Repo, f.Commit, f.Skill, f.Question, f.Expect = skillsTestRepo, skillsTestCommit, skillsTestSkill, skillsTestQuestion, skillsTestFact
		return f, nil
	}
	var missing []string
	for _, field := range []struct{ flag, value string }{
		{"--skill-repo", f.Repo}, {"--skill-commit", f.Commit}, {"--skill-path", f.Skill}, {"--skill-question", f.Question}, {"--skill-expect", f.Expect},
	} {
		if field.value == "" {
			missing = append(missing, field.flag)
		}
	}
	if len(missing) > 0 {
		return f, fmt.Errorf("a fixture of your own takes every one of --skill-repo, --skill-commit, --skill-path, --skill-question and --skill-expect; missing: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(f.Repo, "https://") && !strings.HasPrefix(f.Repo, "http://") {
		return f, fmt.Errorf("--skill-repo %q: an http(s) git URL", f.Repo)
	}
	if !fullCommitID.MatchString(f.Commit) {
		return f, fmt.Errorf("--skill-commit %q: a full 40- or 64-hex commit id (the CRD refuses anything else)", f.Commit)
	}
	f.Skill = strings.Trim(f.Skill, "/")
	if f.Skill == "" || f.Skill == "." || strings.Contains(f.Skill, "..") {
		return f, fmt.Errorf("--skill-path %q: the skill's directory within the repository", f.Skill)
	}
	if f.CredentialSecret != "" && !strings.HasPrefix(f.Repo, "https://") {
		return f, fmt.Errorf("--skill-secret needs an https:// repository (the CRD refuses a credentialRef on an http URL)")
	}
	return f, nil
}

// skillsAgentTemplateCRD is the CRD the fixture's credentialRef must be served by.
const skillsAgentTemplateCRD = "agenttemplates.kagent.dev"

// requireCredentialRefServed checks the served AgentTemplate CRD carries
// skills[].source.git.credentialRef: a kagent line without it prunes the
// field at admission and fetches anonymously, which a private fixture would
// report as a boot failure for the wrong reason.
func requireCredentialRefServed() error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	crd, err := getObject(ctx, gvrCRDs, "", skillsAgentTemplateCRD)
	if err != nil {
		return fmt.Errorf("read the AgentTemplate CRD: %w", err)
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		version, _ := v.(map[string]any)
		if name, _, _ := unstructured.NestedString(version, nameKey); name != path.Base(agentTemplateAPIVersion) {
			continue
		}
		if _, found, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "skills", "items", "properties", "source", "properties", "git", "properties", "credentialRef"); found {
			return nil
		}
	}
	return fmt.Errorf("the served AgentTemplate CRD (%s) has no skills[].source.git.credentialRef: the kagent line under test predates the per-source credential, a private fixture cannot be fetched", skillsAgentTemplateCRD)
}

// name is the skill's name: the last element of its directory.
func (f SkillsFixture) name() string { return path.Base(f.Skill) }

// prompt is the turn: list the skills, load the fixture's, answer its
// question from the text only.
func (f SkillsFixture) prompt() string {
	return "First list the names of every skill available to you. Then load the skill named " + f.name() +
		" and answer from its text only: " + f.Question + " Reply in one line: the skill names, then the answer."
}

// credentialNote words the fixture's credential for the steps, "" when none.
func (f SkillsFixture) credentialNote() string {
	if f.CredentialSecret == "" {
		return ""
	}
	return fmt.Sprintf(" with credentialRef {name: %s, key: %s}", f.CredentialSecret, skillsCredentialKey)
}

// SkillsTestReadyTimeout bounds the golden boot by default: the runtime image
// into Substrate's layer cache, the actor's start, the skill fetch, the
// snapshot — minutes on a cold lab, and a boot that is being denied egress
// retries forever, so the wait has to end somewhere.
const SkillsTestReadyTimeout = 10 * time.Minute

// The waits of the proof: the golden boot is polled slowly (it takes minutes
// either way), the control boot runs on the same runtime image and is bounded
// tighter, Substrate is given a while to let go of a deleted template.
const (
	goldenBootPoll          = 5 * time.Second
	skillsControlTimeout    = 5 * time.Minute
	substrateReleaseWait    = 2 * time.Minute
	substrateRecoveryWait   = time.Minute
	skillsEvidenceLogSince  = 20 * time.Minute
	skillsEvidenceLogLines  = 12
	skillsEvidenceTailLines = 3
)

// The words a failed golden boot leaves in the logs: the gate's, and the
// actor's own about its skill fetch and its exit.
var (
	egressGateWords = []string{"denied", "not running", "RUNNING", "reject", "forbid", "CONNECT"}
	actorBootWords  = []string{"fatal", "materializ", "readyz", "unable to", "exit", "level\":\"error", "\"error\":", "authentication", "401", "credential"}
)

// Resources of kagent API v2 the proof reads beyond the AgentTemplate, and
// Substrate's worker-pool label.
const (
	harnessResource       = "harnesses.kagent.dev"
	kagentControllerName  = "kagent-controller"
	propagateIdentityEnv  = "KAGENT_PROPAGATE_TOKEN"
	substrateWorkerLabel  = "ate.dev/worker-pool"
	atenetPodPrefix       = "atenet"
	actorTemplateNameSize = 50
)

// SkillsTestOptions tunes the proof.
type SkillsTestOptions struct {
	// ModelConfig is the kagent ModelConfig the throwaway agent runs on
	// (default: default-model-config, the Anthropic one the lab renders).
	ModelConfig string
	// ReadyTimeout bounds the golden boot (default SkillsTestReadyTimeout).
	ReadyTimeout time.Duration
	// Fixture is the skill to boot and ask about; the zero value is the
	// public fixture, a private repository's names its Secret.
	Fixture SkillsFixture
}

// SkillsTest is the headless proof that a Go ADK AgentTemplate with a git skill
// pinned to a full commit boots under Substrate and uses the skill in a turn
// as the signed-in user; on a failed golden boot it prints the evidence. The
// template is written below the agent chart on purpose — a raw AgentTemplate,
// so a private fixture's credentialRef and the control boot stay possible;
// the same skill through agent-manager and the chart is agents-test's.
func SkillsTest(cfg *config.Config, email string, opts SkillsTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	if opts.ModelConfig == "" {
		opts.ModelConfig = defaultModelConfig
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = SkillsTestReadyTimeout
	}
	fixture, err := opts.Fixture.resolve()
	if err != nil {
		return err
	}
	opts.Fixture = fixture

	step("Logging in to Dex as %s", user.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer api.close()

	step("The line under test")
	facts := skillsLineFacts(cfg, api)
	for _, line := range facts.lines() {
		note("%s", line)
	}
	if !facts.propagatesToken {
		note("Harness %s does not set %s=true: muster would see the agent, not the person, on the turn's tool calls", kagentHarness, propagateIdentityEnv)
	}

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path; what Substrate would not let go of is said.
	cleanup := func() {
		for _, left := range skillsCleanup(api) {
			note("cleanup: still in Substrate: %s", left)
		}
	}
	cleanup()
	defer cleanup()

	shape := readSkillsTemplateShape()
	step("Applying AgentTemplate %s on Harness %s: skill %s from %s @ %.12s%s, %s", skillsTestAgent, kagentHarness, fixture.name(), fixture.Repo, fixture.Commit, fixture.credentialNote(), shape.toolsNote())
	if fixture.CredentialSecret != "" {
		if err := requireCredentialRefServed(); err != nil {
			return err
		}
	}
	if _, err := applyManifests(context.Background(), []byte(skillsAgentTemplate(skillsTestAgent, opts.ModelConfig, &fixture, shape))); err != nil {
		return err
	}
	note("accepted by the apiserver (the CRD validates the full commit and the relative path); labelled %v as the Harness admits", shape.admission)

	step("Waiting up to %s for Ready on Harness %s — the golden boot: the actor starts, fetches the skill, serves readyz, is snapshotted", opts.ReadyTimeout, kagentHarness)
	boot, err := waitAgentReady(skillsTestAgent, opts.ReadyTimeout)
	if err != nil {
		return err
	}
	if !boot.ready {
		return skillsGoldenBootFailed(cfg, api, opts, boot, facts, shape)
	}
	harness := boot.template.harness(kagentHarness)
	note("Ready after %s: revision %.12s%s", boot.elapsed.Round(time.Second), harness.LatestSuccessfulRevision, warningsNote(harness.Warnings))
	footprint := skillsFootprint(api, skillsTestAgent)
	for _, line := range footprint.lines() {
		note("%s", line)
	}

	step("One A2A turn through the edge as %s: the agent names its skills and answers from the skill's text", user.Email)
	reply, err := firstTurnAs(cfg, skillsTestAgent, token, fixture.prompt())
	if err != nil {
		return err
	}
	if err := skillReplyProves(reply, fixture.name(), fixture.Expect); err != nil {
		return fmt.Errorf("the skill was not in effect on the turn: %w", err)
	}
	note("answered: %s", excerpt(reply, 200))

	step("Deleting %s — the template, Substrate's ActorTemplate and its actor go", skillsTestAgent)
	leftovers := skillsCleanup(api)
	if len(leftovers) > 0 {
		return fmt.Errorf("the deleted template left this in Substrate: %s", strings.Join(leftovers, "; "))
	}
	note("nothing left in the kagent namespace or in Substrate")

	fmt.Println()
	fmt.Printf("PASS: AgentTemplate %s with the git skill %s (%s @ %.12s%s) reached Ready on Harness %s in %s — the Go ADK fetched the skill during the golden boot under Substrate's egress gate (%s)\n",
		skillsTestAgent, fixture.name(), fixture.Repo, fixture.Commit, fixture.credentialNote(), kagentHarness, boot.elapsed.Round(time.Second), facts.summary())
	fmt.Printf("PASS: one turn through the edge as %s named the skill and answered %q from its text; the Harness re-emits the person's bearer on tool calls (%s=true), so muster attributes any tool call of the turn to %s\n",
		user.Email, fixture.Expect, propagateIdentityEnv, user.Email)
	fmt.Printf("PASS: nothing left behind — the AgentTemplate, its AgentInstance, Substrate's ActorTemplate and actor are gone\n")
	return nil
}

// skillsTemplateShape is what the lab's platform dictates about the proof's
// templates: the labels the Harness admits, and whether a shared muster
// RemoteMCPServer exists to bind as the template's tools (the platform
// renders one per agent from the agent chart, none shared, so the proof's
// template carries no tools — the skill is what it proves; an older
// connectivity chart that renders a shared one gets it bound).
type skillsTemplateShape struct {
	admission   map[string]string
	musterTools bool
}

// readSkillsTemplateShape reads the shape off the lab.
func readSkillsTemplateShape() skillsTemplateShape {
	shape := skillsTemplateShape{admission: harnessAdmissionLabels()}
	if _, err := readKagentObject(remoteMCPServerResource, componentMuster); err == nil {
		shape.musterTools = true
	}
	return shape
}

// toolsNote words the template's tools for the steps.
func (s skillsTemplateShape) toolsNote() string {
	if s.musterTools {
		return "tools from " + componentMuster
	}
	return "no tools (the platform renders no shared " + componentMuster + " RemoteMCPServer; the skill is what the template carries)"
}

// harnessAdmissionLabels are the labels the platform Harness admits
// (spec.allowedAgentTemplates.selector.matchLabels): the proof's templates
// carry them, so the Harness picks them up whichever label the platform
// chose — the connectivity chart's agent-platform.giantswarm.io/harness:
// <name>, or another. A Harness that cannot be read leaves the chart's.
func harnessAdmissionLabels() map[string]string {
	fallback := map[string]string{harnessLabel: kagentHarness}
	h, err := readKagentObject(harnessResource, kagentHarness)
	if err != nil {
		return fallback
	}
	labels, found, _ := unstructured.NestedStringMap(h.Object, "spec", "allowedAgentTemplates", "selector", "matchLabels")
	if !found || len(labels) == 0 {
		return fallback
	}
	return labels
}

// skillsAgentTemplate is the proof's AgentTemplate: the Go ADK Harness, the
// ModelConfig, the terse prompt, the shared muster server as its tools where
// the platform renders one (the fleet's shape: skills and tools), labelled
// as agentlab's and as the Harness admits — and, given a fixture (nil is the
// control), one git skill pinned to its full commit, selected by its
// directory within the repository, with the fixture's credentialRef when it
// names a Secret.
func skillsAgentTemplate(name, modelConfig string, fixture *SkillsFixture, shape skillsTemplateShape) string {
	labels := []string{managedByLabel + ": " + managedByAgentlabValue}
	for _, key := range slices.Sorted(maps.Keys(shape.admission)) {
		labels = append(labels, key+": "+shape.admission[key])
	}
	tools := ""
	if shape.musterTools {
		tools = fmt.Sprintf(`  tools:
    - mcp:
        server:
          kind: %s
          name: %s
`, remoteMCPServerKind, componentMuster)
	}
	skills := ""
	description := "Throwaway agent of `agentlab skills-test` (the control: the same template without the skill); deleted by the same run."
	if fixture != nil {
		description = "Throwaway agent of `agentlab skills-test`: one git skill pinned to a full commit; deleted by the same run."
		credential := ""
		if fixture.CredentialSecret != "" {
			credential = fmt.Sprintf(`          credentialRef:
            name: %s
            key: %s
`, fixture.CredentialSecret, skillsCredentialKey)
		}
		skills = fmt.Sprintf(`  skills:
    - name: %s
      source:
        git:
          url: %s
          commit: %s
%s        path: %s
`, fixture.name(), fixture.Repo, fixture.Commit, credential, fixture.Skill)
	}
	return fmt.Sprintf(`apiVersion: %s
kind: AgentTemplate
metadata:
  name: %s
  namespace: %s
  labels:
    %s
spec:
  description: %q
  modelConfig:
    name: %s
  systemPrompt: %q
%s%s`, agentTemplateAPIVersion, name, kagentNamespace, strings.Join(labels, "\n    "), description, modelConfig, skillsTestSystemPrompt, tools, skills)
}

// skillsGoldenBootFailed is the negative outcome: the evidence, the control
// boot, and the FAIL verdict that carries the versions — the finding for the
// line's upstream issue.
func skillsGoldenBootFailed(cfg *config.Config, api *kagentAPI, opts SkillsTestOptions, boot agentReadiness, facts lineFacts, shape skillsTemplateShape) error {
	verdict := fmt.Sprintf("never became Ready within %s (%s)", opts.ReadyTimeout, boot.reason)
	if boot.terminal {
		verdict = "failed for good after " + boot.elapsed.Round(time.Second).String() + ": " + boot.reason
	}
	step("The golden boot %s — collecting the evidence", verdict)
	harness := &harnessStatus{Harness: kagentHarness}
	if boot.template != nil {
		if h := boot.template.harness(kagentHarness); h != nil {
			harness = h
		}
	}
	evidence := goldenBootEvidence(api, skillsTestAgent, harness, facts)
	for _, line := range evidence {
		note("%s", line)
	}

	step("The control: the same template without the skill, up to %s", skillsControlTimeout)
	var control string
	if _, err := applyManifests(context.Background(), []byte(skillsAgentTemplate(skillsTestControlAgent, opts.ModelConfig, nil, shape))); err != nil {
		control = "could not be applied: " + err.Error()
	} else if boot, err := waitAgentReady(skillsTestControlAgent, skillsControlTimeout); err != nil {
		control = "could not be read: " + err.Error()
	} else if boot.ready {
		control = fmt.Sprintf("Ready in %s — the skill is the difference", boot.elapsed.Round(time.Second))
	} else {
		control = fmt.Sprintf("not Ready either (%s) — the lab's Harness does not boot at all right now, the skill is not the difference", boot.reason)
	}
	note("%s", control)

	fmt.Println()
	fmt.Printf("FAIL: AgentTemplate %s with the git skill %s (%s @ %.12s%s) %s on Harness %s (%s)\n",
		skillsTestAgent, opts.Fixture.name(), opts.Fixture.Repo, opts.Fixture.Commit, opts.Fixture.credentialNote(), verdict, kagentHarness, facts.summary())
	fmt.Printf("FAIL: the control without the skill: %s\n", control)
	return fmt.Errorf("the golden boot of a template with a git skill %s on Harness %s; the evidence above is the finding (docs/platform.md \"The skills proof (the golden boot)\")", verdict, kagentHarness)
}

// goldenBootEvidence is what a failed golden boot leaves to read, as lines:
// the Harness's conditions and warnings, Substrate's footprint of the
// template (ActorTemplates, actors, their worker pods) as the controller
// reports it, and the log lines of the controller, of atenet (the egress
// gate) and of the pool's worker pods about the template's actors.
func goldenBootEvidence(api *kagentAPI, name string, harness *harnessStatus, facts lineFacts) []string {
	var lines []string
	lines = append(lines, "Harness "+harness.Harness+" conditions:")
	for _, c := range harness.Conditions {
		lines = append(lines, "  "+c.String())
	}
	lines = append(lines, fmt.Sprintf("  desired revision %s, latest successful %q%s", harness.DesiredRevision, harness.LatestSuccessfulRevision, warningsNote(harness.Warnings)))

	footprint := skillsFootprint(api, name)
	lines = append(lines, footprint.lines()...)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lines = append(lines, logEvidence(ctx, kagentControllerName, kagentNamespace, "deploy/"+kagentControllerName, "", "", name)...)
	// atenet's pods run the egress proxy and its gate as sidecars
	// (agentgateway + ext-proc); every container is read, for the actors'
	// ids and the gate's words.
	patterns := append([]string(nil), egressGateWords...)
	for _, actor := range footprint.actors {
		patterns = append(patterns, actor.GetActorId())
	}
	for _, pod := range podsNamed(ctx, substrateNamespace, atenetPodPrefix) {
		for _, container := range podContainers(ctx, substrateNamespace, pod) {
			lines = append(lines, logEvidence(ctx, pod+"/"+container, substrateNamespace, "pod/"+pod, container, "", patterns...)...)
		}
	}
	// The actor's own output: Substrate's worker pods write it as JSON lines
	// labelled with the ActorTemplate (ate.template.name) and the actor, so
	// every worker of the pool is read for the template's name — the actor
	// that failed may have run on any of them, and be gone by now — and
	// within those lines for the words of a boot that went wrong.
	for _, pod := range podsLabelled(ctx, kagentNamespace, substrateWorkerLabel+"="+facts.workerPool) {
		lines = append(lines, logEvidence(ctx, "worker "+pod, kagentNamespace, "pod/"+pod, "", actorTemplatePrefix(name, harness.Harness), actorBootWords...)...)
	}
	return lines
}

// logEvidence is the last lines of a target's recent logs (of the named
// container, or the pod's default) that carry must (when given) and any of
// the patterns, case-insensitively, titled; when none does, it says so and
// gives a short tail instead, so a changed log vocabulary still leaves a
// trace.
func logEvidence(ctx context.Context, title, ns, target, container, must string, patterns ...string) []string {
	logs, err := podLogs(ctx, ns, target, container, skillsEvidenceLogSince)
	if err != nil {
		return []string{fmt.Sprintf("%s logs: %v", title, err)}
	}
	matched := grepLines(logs, skillsEvidenceLogLines, must, patterns...)
	wanted := fmt.Sprintf("%v", patterns)
	if must != "" {
		wanted = fmt.Sprintf("%q and %v", must, patterns)
	}
	if len(matched) == 0 {
		tail := lastLines(logs, skillsEvidenceTailLines)
		if len(tail) == 0 {
			return []string{fmt.Sprintf("%s logs (last %s): empty", title, skillsEvidenceLogSince)}
		}
		return append([]string{fmt.Sprintf("%s logs (last %s): no line carries %s; the tail:", title, skillsEvidenceLogSince, wanted)}, indentLines(tail)...)
	}
	return append([]string{fmt.Sprintf("%s logs (last %s), lines carrying %s:", title, skillsEvidenceLogSince, wanted)}, indentLines(matched)...)
}

// grepLines is the last max lines of text that contain must (when given)
// and any of the patterns, case-insensitively; an empty pattern matches
// nothing.
func grepLines(text string, maxLines int, must string, patterns ...string) []string {
	var matched []string
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		if must != "" && !strings.Contains(lower, strings.ToLower(must)) {
			continue
		}
		for _, p := range patterns {
			if p != "" && strings.Contains(lower, strings.ToLower(p)) {
				matched = append(matched, line)
				break
			}
		}
	}
	if len(matched) > maxLines {
		matched = matched[len(matched)-maxLines:]
	}
	return matched
}

// lastLines is the last n non-empty lines of text.
func lastLines(text string, n int) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func indentLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = "  " + line
	}
	return out
}

// podsLabelled lists the pods of a namespace matching a label selector,
// sorted; the WorkerPool's workers (ate.dev/worker-pool=<pool>).
func podsLabelled(ctx context.Context, ns, selector string) []string {
	pods, err := listObjects(ctx, gvrPods, ns, selector)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.GetName())
	}
	slices.Sort(names)
	return names
}

// podContainers names a pod's containers, in order.
func podContainers(ctx context.Context, ns, pod string) []string {
	obj, err := getObject(ctx, gvrPods, ns, pod)
	if err != nil {
		return nil
	}
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "containers")
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		m, _ := c.(map[string]any)
		if name, _, _ := unstructured.NestedString(m, nameKey); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// podsNamed lists the pods of a namespace whose name starts with prefix,
// sorted; the atenet pods, whose labels the lab does not pin.
func podsNamed(ctx context.Context, ns, prefix string) []string {
	pods, err := listObjects(ctx, gvrPods, ns, "")
	if err != nil {
		return nil
	}
	var names []string
	for _, pod := range pods {
		if strings.HasPrefix(pod.GetName(), prefix) {
			names = append(names, pod.GetName())
		}
	}
	slices.Sort(names)
	return names
}

// substrateFootprint is what Substrate holds for one AgentTemplate on the
// Harness, as the controller's GetSubstrateStatus reports it: the
// ActorTemplates written for its revisions (kagent names them
// <template>-<harness>-<revision>), the actors booted from them with their
// state and worker pod. err is the status call's failure, when it failed.
type substrateFootprint struct {
	templates []*apiv1alpha1.SubstrateActorTemplate
	actors    []*apiv1alpha1.SubstrateActor
	pools     []*apiv1alpha1.SubstrateWorkerPool
	err       error
}

// skillsFootprint reads the template's footprint in Substrate through the
// controller's API.
func skillsFootprint(api *kagentAPI, name string) substrateFootprint {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	status, err := api.substrateStatus(ctx, kagentNamespace)
	if err != nil {
		return substrateFootprint{err: err}
	}
	return footprintOf(status, name, kagentHarness)
}

// footprintOf filters a GetSubstrateStatus answer down to one template's.
func footprintOf(status *apiv1alpha1.GetSubstrateStatusResponse, agentTemplate, harness string) substrateFootprint {
	prefix := actorTemplatePrefix(agentTemplate, harness)
	f := substrateFootprint{pools: status.GetWorkerPools()}
	if status.GetAteApiError() != "" {
		f.err = fmt.Errorf("the controller could not list ate-api's state: %s", status.GetAteApiError())
	}
	for _, t := range status.GetActorTemplates() {
		if t.GetNamespace() == kagentNamespace && strings.HasPrefix(t.GetName(), prefix) {
			f.templates = append(f.templates, t)
		}
	}
	for _, a := range status.GetActors() {
		if a.GetActorTemplateNamespace() == kagentNamespace && strings.HasPrefix(a.GetActorTemplateName(), prefix) {
			f.actors = append(f.actors, a)
		}
	}
	return f
}

// actorTemplatePrefix is how kagent names the ActorTemplates of an
// AgentTemplate on a Harness, up to the revision: `<template>-<harness>`
// lower-cased, cut to 50 characters, then `-<12 hex of the revision>`.
func actorTemplatePrefix(agentTemplate, harness string) string {
	base := strings.ToLower(strings.ReplaceAll(agentTemplate+"-"+harness, "_", "-"))
	if len(base) > actorTemplateNameSize {
		base = strings.TrimRight(base[:actorTemplateNameSize], "-")
	}
	return base + "-"
}

// empty reports whether Substrate holds nothing of the template.
func (f substrateFootprint) empty() bool {
	return f.err == nil && len(f.templates) == 0 && len(f.actors) == 0
}

// lines words the footprint for the notes and the evidence.
func (f substrateFootprint) lines() []string {
	if f.err != nil {
		return []string{"Substrate state through the controller: " + f.err.Error()}
	}
	var lines []string
	for _, p := range f.pools {
		lines = append(lines, fmt.Sprintf("WorkerPool %s/%s: %d workers (%s)", p.GetNamespace(), p.GetName(), p.GetReplicas(), p.GetAteomImage()))
	}
	if len(f.templates) == 0 && len(f.actors) == 0 {
		return append(lines, "Substrate holds no ActorTemplate and no actor of the template")
	}
	for _, t := range f.templates {
		snapshot := t.GetGoldenSnapshot()
		if snapshot == "" {
			snapshot = "none"
		}
		lines = append(lines, fmt.Sprintf("ActorTemplate %s/%s: phase %s, golden snapshot %s, golden actor %s", t.GetNamespace(), t.GetName(), t.GetPhase(), snapshot, t.GetGoldenActorId()))
	}
	for _, a := range f.actors {
		worker := "no worker"
		if a.GetAteomPodName() != "" {
			worker = fmt.Sprintf("pinned to worker pod %s/%s (%s)", a.GetAteomPodNamespace(), a.GetAteomPodName(), a.GetAteomPodIp())
		}
		lines = append(lines, fmt.Sprintf("actor %s of %s: %s, %s", a.GetActorId(), a.GetActorTemplateName(), a.GetStatus(), worker))
	}
	return lines
}

// skillsCleanup removes everything the proof creates and waits for Substrate
// to let go of it: the AgentTemplates (and any carrier), then their
// ActorTemplates and actors, which kagent's revision garbage collector
// deletes once nothing references them. An actor still pinned to a worker
// after that is freed the documented way — its worker pod deleted, the
// WorkerPool replaces it — and whatever is still there is returned, worded,
// for the verdict.
func skillsCleanup(api *kagentAPI) []string {
	names := []string{skillsTestAgent, skillsTestControlAgent}
	removed := false
	for _, name := range names {
		if !agentExists(name) {
			continue
		}
		removed = true
		if err := removeAgent(name); err != nil {
			note("cleanup: %v", err)
		}
	}
	footprints := func() (all []substrateFootprint, clean bool) {
		clean = true
		for _, name := range names {
			f := skillsFootprint(api, name)
			all = append(all, f)
			if !f.empty() {
				clean = false
			}
		}
		return all, clean
	}
	var all []substrateFootprint
	var clean bool
	if waitFor(int(substrateReleaseWait/goldenBootPoll), goldenBootPoll, func() bool { all, clean = footprints(); return clean }) {
		if removed {
			note("cleanup: %s* removed, Substrate holds nothing of them", skillsTestAgent)
		}
		return nil
	}
	// The recovery: a worker pinned to an actor of a deleted template.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, f := range all {
		for _, a := range f.actors {
			if a.GetAteomPodName() == "" {
				continue
			}
			note("cleanup: actor %s (%s) is still pinned to worker pod %s/%s — deleting the pod, the WorkerPool replaces it", a.GetActorId(), a.GetStatus(), a.GetAteomPodNamespace(), a.GetAteomPodName())
			if err := deleteObject(ctx, gvrPods, a.GetAteomPodNamespace(), a.GetAteomPodName(), 0); err != nil {
				note("cleanup: %v", err)
			}
		}
	}
	if waitFor(int(substrateRecoveryWait/goldenBootPoll), goldenBootPoll, func() bool { all, clean = footprints(); return clean }) {
		note("cleanup: Substrate let go of %s* after the worker pods were replaced", skillsTestAgent)
		return nil
	}
	var leftovers []string
	for _, f := range all {
		if !f.empty() {
			leftovers = append(leftovers, f.lines()...)
		}
	}
	return leftovers
}

// skillReplyProves judges the turn's answer: it names the skill and carries
// the fact only the skill's text has.
func skillReplyProves(reply, skill, fact string) error {
	lower := strings.ToLower(reply)
	switch {
	case !strings.Contains(lower, strings.ToLower(skill)):
		return fmt.Errorf("the answer does not name the skill %s (the runtime did not list it — /skills empty?): %s", skill, excerpt(reply, 300))
	case !strings.Contains(lower, strings.ToLower(fact)):
		return fmt.Errorf("the answer names the skill but not %q from its text (the skill was listed but not loadable?): %s", fact, excerpt(reply, 300))
	}
	return nil
}

// warningsNote words a Harness's compile warnings for a note, "" when none.
func warningsNote(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	return fmt.Sprintf(", warnings %v", warnings)
}

// lineFacts is the line the outcome was measured on: the meta chart, the
// kagent and kagent-crds charts and the Substrate chart as installed, the
// controller's build, the Harness's runtime image and pool.
type lineFacts struct {
	metaChart, kagentChart, kagentCRDsChart, substrateChart string
	controllerVersion, controllerImage                      string
	harnessImage, workerPool                                string
	propagatesToken                                         bool
}

// skillsLineFacts reads the facts, each best effort: a fact the lab cannot
// read is reported as such, never a failed proof.
func skillsLineFacts(cfg *config.Config, api *kagentAPI) lineFacts {
	facts := lineFacts{metaChart: cfg.Platform.ChartVersion}
	if cfg.Platform.ChartBranch != "" {
		facts.metaChart += " (branch " + cfg.Platform.ChartBranch + ")"
	}
	facts.kagentChart = fluxHelmReleaseVersion(componentKagent)
	facts.kagentCRDsChart = fluxHelmReleaseVersion(componentKagent + "-crds")
	if v, err := helmReleaseVersion(substrateNamespace, substrateRelease); err == nil && v != "" {
		facts.substrateChart = v
	} else {
		facts.substrateChart = unreadable(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if v, err := api.version(ctx); err == nil {
		facts.controllerVersion = v.GetKagentVersion() + " (" + v.GetGitCommit() + ")"
	} else {
		facts.controllerVersion = unreadable(err)
	}
	if d, err := getObject(ctx, gvrDeployments, kagentNamespace, kagentControllerName); err == nil {
		facts.controllerImage = firstContainerImage(d)
	} else {
		facts.controllerImage = unreadable(err)
	}
	if h, err := readKagentObject(harnessResource, kagentHarness); err == nil {
		facts.harnessImage, _, _ = unstructured.NestedString(h.Object, "spec", "workload", "image")
		facts.workerPool, _, _ = unstructured.NestedString(h.Object, "spec", "substrate", "workerPoolRef", nameKey)
		facts.propagatesToken = harnessEnvTrue(h, propagateIdentityEnv)
	} else {
		facts.harnessImage = unreadable(err)
	}
	return facts
}

// unreadable words a fact the lab could not read.
func unreadable(err error) string {
	if err == nil {
		return "not found"
	}
	return "unreadable (" + excerpt(err.Error(), 80) + ")"
}

// lines words the facts for the notes.
func (f lineFacts) lines() []string {
	return []string{
		"agent-platform " + f.metaChart,
		"kagent chart " + f.kagentChart + ", kagent-crds " + f.kagentCRDsChart,
		"kagent-controller " + f.controllerVersion + ", image " + f.controllerImage,
		"Substrate chart " + f.substrateChart,
		"Harness " + kagentHarness + ": image " + f.harnessImage + ", WorkerPool " + f.workerPool + fmt.Sprintf(", %s=%v", propagateIdentityEnv, f.propagatesToken),
	}
}

// summary is the one-line form for the verdicts.
func (f lineFacts) summary() string {
	return fmt.Sprintf("agent-platform %s, kagent %s, Substrate %s, Go ADK image %s", f.metaChart, f.kagentChart, f.substrateChart, f.harnessImage)
}

// fluxHelmReleaseVersion is the chart version a platform component's Flux
// HelmRelease last applied (status.history[0].chartVersion), else the one it
// last attempted, else what the read ran into.
func fluxHelmReleaseVersion(name string) string {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return unreadable(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	hr, err := getObject(ctx, gvr, platformNamespace, name)
	if err != nil {
		return unreadable(err)
	}
	if history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history"); len(history) > 0 {
		if snapshot, ok := history[0].(map[string]any); ok {
			if v, _, _ := unstructured.NestedString(snapshot, "chartVersion"); v != "" {
				return v
			}
		}
	}
	if v, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision"); v != "" {
		return v + " (attempted)"
	}
	return "no revision applied yet"
}

// firstContainerImage is the image of a Deployment's first container.
func firstContainerImage(d *unstructured.Unstructured) string {
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return "no container"
	}
	c, _ := containers[0].(map[string]any)
	image, _, _ := unstructured.NestedString(c, "image")
	return image
}

// harnessEnvTrue reports whether a Harness sets the literal env var to "true".
func harnessEnvTrue(h *unstructured.Unstructured, name string) bool {
	env, _, _ := unstructured.NestedSlice(h.Object, "spec", "env")
	for _, e := range env {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if n, _, _ := unstructured.NestedString(m, nameKey); n != name {
			continue
		}
		v, _, _ := unstructured.NestedString(m, "value")
		return v == "true"
	}
	return false
}
