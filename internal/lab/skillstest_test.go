package lab

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/kagentpb"
)

// fieldReason is a condition's reason key.
const fieldReason = "reason"

// condAccepted is the Harness's admission condition.
const condAccepted = "Accepted"

// bootTemplate seeds an AgentTemplate with the controller's status for the Go
// ADK Harness the way a golden boot leaves it: Accepted, ResolvedRefs and
// Compatible True, Ready as given.
func bootTemplate(name, readyStatus, reason, message string) *unstructured.Unstructured {
	template := customObject(gvkAgentTemplate, kagentNamespace, name, map[string]string{harnessLabel: kagentHarness})
	_ = unstructured.SetNestedSlice(template.Object, []any{map[string]any{
		"harness":         kagentHarness,
		"desiredRevision": testRevision,
		fieldConditions: []any{
			map[string]any{fieldType: condAccepted, fieldStatus: conditionTrue, fieldReason: condAccepted, fieldMessage: "Harness admission selector matches the AgentTemplate"},
			map[string]any{fieldType: "ResolvedRefs", fieldStatus: conditionTrue, fieldReason: "Resolved", fieldMessage: "All runtime references resolved"},
			map[string]any{fieldType: "Compatible", fieldStatus: conditionTrue, fieldReason: "Compatible", fieldMessage: "Resolved configuration is compatible with the Harness"},
			map[string]any{fieldType: condReady, fieldStatus: readyStatus, fieldReason: reason, fieldMessage: message},
		},
	}}, fieldStatus, "harnesses")
	return template
}

// TestSkillsAgentTemplate: the proof's template is a v1alpha3 AgentTemplate on
// the Go ADK Harness binding the shared muster server, with the one git skill
// pinned to the fixture's full commit and selected by its directory; the
// control carries no skills at all.
func TestSkillsAgentTemplate(t *testing.T) {
	fixture, err := SkillsFixture{}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(skillsAgentTemplate(skillsTestAgent, defaultModelConfig, &fixture, kagentAdmission)), &obj); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: obj}
	if u.GetAPIVersion() != agentTemplateAPIVersion || u.GetKind() != "AgentTemplate" || u.GetNamespace() != kagentNamespace ||
		u.GetLabels()[harnessLabel] != kagentHarness || u.GetLabels()[managedByLabel] != managedByAgentlabValue {
		t.Errorf("template head:\n%s", skillsAgentTemplate(skillsTestAgent, defaultModelConfig, &fixture, kagentAdmission))
	}
	template, err := agentTemplateFrom(u)
	if err != nil {
		t.Fatal(err)
	}
	if template.mcpServer() != componentMuster || template.Spec.ModelConfig.Name != defaultModelConfig {
		t.Errorf("binding %q, model %v", template.mcpServer(), template.Spec.ModelConfig)
	}
	skills, _, _ := unstructured.NestedSlice(obj, "spec", "skills")
	if len(skills) != 1 {
		t.Fatalf("skills = %v", skills)
	}
	skill, _ := skills[0].(map[string]any)
	name, _, _ := unstructured.NestedString(skill, nameKey)
	url, _, _ := unstructured.NestedString(skill, "source", "git", "url")
	commit, _, _ := unstructured.NestedString(skill, "source", "git", "commit")
	path, _, _ := unstructured.NestedString(skill, "source", "path")
	if name != skillsTestSkill || url != skillsTestRepo || commit != skillsTestCommit || path != skillsTestSkill {
		t.Errorf("skill = %v", skill)
	}
	if len(commit) != 40 {
		t.Errorf("the fixture commit is not a full SHA: %q", commit)
	}

	var control map[string]any
	if err := yaml.Unmarshal([]byte(skillsAgentTemplate(skillsTestControlAgent, defaultModelConfig, nil, kagentAdmission)), &control); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(control, "spec", "skills"); found {
		t.Error("the control carries skills")
	}
	if description, _, _ := unstructured.NestedString(control, "spec", "description"); !strings.Contains(description, "control") {
		t.Errorf("the control's description does not say so: %q", description)
	}
}

// kagentAdmission is kagent's default admission label, what the tests
// render unless they test the Harness's own; platformHarnessLabel is the
// connectivity chart's; the fixture names below are the test's.
var kagentAdmission = skillsTemplateShape{admission: map[string]string{harnessLabel: kagentHarness}, musterTools: true}

const (
	platformHarnessLabel = "agent-platform.giantswarm.io/harness"
	exampleRepo          = "https://example.com/x"
	gitAuthObject        = "skills-git-auth"
)

// TestSkillsAgentTemplateAdmissionLabels: the template carries the labels the
// Harness admits — the platform's own label here — and not kagent's default
// when the Harness does not select on it; agentlab's managed-by label stays.
func TestSkillsAgentTemplateAdmissionLabels(t *testing.T) {
	fixture, _ := SkillsFixture{}.resolve()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(skillsAgentTemplate(skillsTestAgent, defaultModelConfig, &fixture, skillsTemplateShape{admission: map[string]string{platformHarnessLabel: kagentHarness}})), &obj); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: obj}
	labels := u.GetLabels()
	if labels[platformHarnessLabel] != kagentHarness || labels[managedByLabel] != managedByAgentlabValue {
		t.Errorf("labels = %v", labels)
	}
	if _, has := labels[harnessLabel]; has {
		t.Errorf("kagent's default label rendered although the Harness does not select on it: %v", labels)
	}
	if _, found, _ := unstructured.NestedSlice(obj, "spec", "tools"); found {
		t.Errorf("tools rendered although the platform has no shared muster server: %v", obj["spec"])
	}
}

// TestSkillsFixturePrivate: a fixture of the caller's renders its repository,
// commit and directory (the last element the skill's name) and, when it names
// a Secret, the source's credentialRef on the token key — nothing else about
// the template changes; the public fixture renders no credentialRef.
func TestSkillsFixturePrivate(t *testing.T) {
	fixture, err := SkillsFixture{
		Repo: "https://github.com/acme/private-skills", Commit: strings.Repeat("c", 40), Skill: "skills/runbooks",
		Question: "which flag renders a recipe?", Expect: "--render", CredentialSecret: gitAuthObject,
	}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if fixture.name() != "runbooks" {
		t.Errorf("name = %q", fixture.name())
	}
	if !strings.Contains(fixture.prompt(), "named runbooks") || !strings.Contains(fixture.prompt(), "which flag renders a recipe?") {
		t.Errorf("prompt = %q", fixture.prompt())
	}
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(skillsAgentTemplate(skillsTestAgent, defaultModelConfig, &fixture, kagentAdmission)), &obj); err != nil {
		t.Fatal(err)
	}
	skills, _, _ := unstructured.NestedSlice(obj, "spec", "skills")
	if len(skills) != 1 {
		t.Fatalf("skills = %v", skills)
	}
	skill, _ := skills[0].(map[string]any)
	name, _, _ := unstructured.NestedString(skill, nameKey)
	url, _, _ := unstructured.NestedString(skill, "source", "git", "url")
	commit, _, _ := unstructured.NestedString(skill, "source", "git", "commit")
	path, _, _ := unstructured.NestedString(skill, "source", "path")
	refName, _, _ := unstructured.NestedString(skill, "source", "git", "credentialRef", nameKey)
	key, _, _ := unstructured.NestedString(skill, "source", "git", "credentialRef", "key")
	if name != "runbooks" || url != fixture.Repo || commit != fixture.Commit || path != "skills/runbooks" || refName != gitAuthObject || key != skillsCredentialKey {
		t.Errorf("skill = %v", skill)
	}

	public, _ := SkillsFixture{}.resolve()
	var anonymous map[string]any
	if err := yaml.Unmarshal([]byte(skillsAgentTemplate(skillsTestAgent, defaultModelConfig, &public, kagentAdmission)), &anonymous); err != nil {
		t.Fatal(err)
	}
	publicSkills, _, _ := unstructured.NestedSlice(anonymous, "spec", "skills")
	if _, found, _ := unstructured.NestedMap(publicSkills[0].(map[string]any), "source", "git", "credentialRef"); found {
		t.Error("the public fixture renders a credentialRef")
	}
}

// TestSkillsFixtureResolve: the zero value is the public fixture; a fixture
// of the caller's needs every field, a full commit and an http(s) URL, and a
// Secret only on https.
func TestSkillsFixtureResolve(t *testing.T) {
	public, err := SkillsFixture{}.resolve()
	if err != nil || public.Repo != skillsTestRepo || public.Commit != skillsTestCommit || public.Skill != skillsTestSkill || public.Question != skillsTestQuestion || public.Expect != skillsTestFact {
		t.Errorf("public fixture = %+v, %v", public, err)
	}
	full := strings.Repeat("a", 40)
	cases := map[string]struct {
		fixture SkillsFixture
		want    string
	}{
		"missing fields": {SkillsFixture{Repo: exampleRepo}, "missing: --skill-commit, --skill-path, --skill-question, --skill-expect"},
		"short commit":   {SkillsFixture{Repo: exampleRepo, Commit: "abc123", Skill: "s", Question: "q", Expect: "e"}, "full 40- or 64-hex"},
		"not a url":      {SkillsFixture{Repo: "git@example.com:x", Commit: full, Skill: "s", Question: "q", Expect: "e"}, "http(s) git URL"},
		"bad path":       {SkillsFixture{Repo: exampleRepo, Commit: full, Skill: "../s", Question: "q", Expect: "e"}, "--skill-path"},
		"secret on http": {SkillsFixture{Repo: "http://example.com/x", Commit: full, Skill: "s", Question: "q", Expect: "e", CredentialSecret: "t"}, "https://"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tc.fixture.resolve()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("resolve() = %v, want %q", err, tc.want)
			}
		})
	}
	ok, err := SkillsFixture{Repo: exampleRepo, Commit: full, Skill: "/dir/skill/", Question: "q", Expect: "e"}.resolve()
	if err != nil || ok.Skill != "dir/skill" || ok.name() != "skill" {
		t.Errorf("resolve() = %+v, %v", ok, err)
	}
}

// TestTerminalHarnessFailure: the golden boot is waited through while Ready
// is False for ActorTemplatePending; Substrate's error (ActorTemplateFailed),
// a conflict, or an earlier stage False end the wait, quoting the condition.
func TestTerminalHarnessFailure(t *testing.T) {
	pending := &harnessStatus{Conditions: []templateCondition{
		{Type: condAccepted, Status: conditionTrue, Reason: condAccepted},
		{Type: condReady, Status: condFalseStatus, Reason: "ActorTemplatePending", Message: "waiting for the ActorTemplate golden snapshot"},
	}}
	if text, terminal := terminalHarnessFailure(pending); terminal {
		t.Errorf("pending is terminal: %s", text)
	}
	failed := &harnessStatus{Conditions: []templateCondition{
		{Type: condReady, Status: condFalseStatus, Reason: "ActorTemplateFailed", Message: "golden actor exited: git fetch: Broken pipe"},
	}}
	if text, terminal := terminalHarnessFailure(failed); !terminal || !strings.Contains(text, "Ready=False ActorTemplateFailed: golden actor exited") {
		t.Errorf("failed = %q %v", text, terminal)
	}
	blocked := &harnessStatus{Conditions: []templateCondition{
		{Type: "ResolvedRefs", Status: condFalseStatus, Reason: "ModelConfigNotFound", Message: "ModelConfig nope not found"},
		{Type: condReady, Status: condFalseStatus, Reason: "Blocked", Message: "blocked by ResolvedRefs"},
	}}
	if text, terminal := terminalHarnessFailure(blocked); !terminal || !strings.HasPrefix(text, "ResolvedRefs=False ModelConfigNotFound") {
		t.Errorf("blocked = %q %v", text, terminal)
	}
	if text, terminal := terminalHarnessFailure(&harnessStatus{}); terminal || text != "" {
		t.Errorf("no conditions = %q %v", text, terminal)
	}
}

// TestWaitGoldenBoot: a template Ready on the Harness returns ready with the
// time it took; one failed for good returns before the timeout, terminal; one
// still pending runs into the timeout, neither.
func TestWaitGoldenBoot(t *testing.T) {
	newFakeLab(t,
		bootTemplate(testReadyName, conditionTrue, "Ready", "ActorTemplate golden snapshot is ready"),
		bootTemplate("failed", condFalseStatus, "ActorTemplateFailed", "golden actor exited"),
		bootTemplate("pending", condFalseStatus, "ActorTemplatePending", "waiting for the ActorTemplate golden snapshot"),
	)
	boot, err := waitGoldenBoot(testReadyName, kagentHarness, goldenBootPoll)
	if err != nil || !boot.ready || boot.terminal || boot.template.Metadata.Name != testReadyName {
		t.Errorf("ready: %+v %v", boot, err)
	}
	boot, err = waitGoldenBoot("failed", kagentHarness, goldenBootPoll)
	if err != nil || boot.ready || !boot.terminal {
		t.Errorf("failed: %+v %v", boot, err)
	}
	if text, _ := terminalHarnessFailure(boot.template.harness(kagentHarness)); !strings.Contains(text, "ActorTemplateFailed") {
		t.Errorf("the failed template's condition: %q", text)
	}
	boot, err = waitGoldenBoot("pending", kagentHarness, goldenBootPoll)
	if err != nil || boot.ready || boot.terminal || boot.elapsed < goldenBootPoll {
		t.Errorf("pending: %+v %v", boot, err)
	}
	if _, err := waitGoldenBoot("absent", kagentHarness, goldenBootPoll); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Errorf("a missing template: %v", err)
	}
}

// TestFootprintOf: Substrate's state is filtered down to the template's own
// ActorTemplates (named <template>-<harness>-<revision>) and the actors booted
// from them, other templates' left out; the worded lines name the phase, the
// snapshot and the pinned worker; an ate-api error is carried.
func TestFootprintOf(t *testing.T) {
	status := &kagentpb.GetSubstrateStatusResponse{
		Enabled:     true,
		WorkerPools: []*kagentpb.SubstrateWorkerPool{{Namespace: kagentNamespace, Name: "kagent-default", Replicas: 2, AteomImage: "ateom:v0"}},
		ActorTemplates: []*kagentpb.SubstrateActorTemplate{
			{Namespace: kagentNamespace, Name: skillsTestAgent + "-kagent-3bd7156d4194", Phase: "Pending", HarnessName: kagentHarness},
			{Namespace: kagentNamespace, Name: skillsTestAgent + "-control-kagent-0a0a0a0a0a0a", Phase: conditionReady, GoldenSnapshot: "s3://ate-snapshots/kagent/x"},
			{Namespace: kagentNamespace, Name: "agentlab-toolset-ro-kagent-111111111111", Phase: conditionReady},
		},
		Actors: []*kagentpb.SubstrateActor{
			{ActorId: "01a0aaaa", ActorTemplateNamespace: kagentNamespace, ActorTemplateName: skillsTestAgent + "-kagent-3bd7156d4194", Status: "Resuming", AteomPodNamespace: kagentNamespace, AteomPodName: "kagent-default-abc", AteomPodIp: "10.0.0.7"},
			{ActorId: "01a0bbbb", ActorTemplateNamespace: kagentNamespace, ActorTemplateName: "agentlab-toolset-ro-kagent-111111111111", Status: "Paused"},
		},
	}
	f := footprintOf(status, skillsTestAgent, kagentHarness)
	if f.err != nil || len(f.templates) != 1 || len(f.actors) != 1 || f.empty() {
		t.Fatalf("footprint = %+v", f)
	}
	lines := strings.Join(f.lines(), "\n")
	for _, want := range []string{"WorkerPool kagent/kagent-default: 2 workers", skillsTestAgent + "-kagent-3bd7156d4194: phase Pending, golden snapshot none", "actor 01a0aaaa", "Resuming, pinned to worker pod kagent/kagent-default-abc (10.0.0.7)"} {
		if !strings.Contains(lines, want) {
			t.Errorf("lines lack %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "toolset-ro") || strings.Contains(lines, "-control-") {
		t.Errorf("another template's footprint leaked:\n%s", lines)
	}
	control := footprintOf(status, skillsTestControlAgent, kagentHarness)
	if len(control.templates) != 1 || len(control.actors) != 0 || !strings.Contains(strings.Join(control.lines(), "\n"), "phase Ready, golden snapshot s3://ate-snapshots/kagent/x") {
		t.Errorf("control footprint = %+v", control)
	}
	if none := footprintOf(status, "nobody", kagentHarness); !none.empty() || !strings.Contains(strings.Join(none.lines(), "\n"), "holds no ActorTemplate") {
		t.Errorf("an absent template: %+v %v", none, none.lines())
	}
	broken := footprintOf(&kagentpb.GetSubstrateStatusResponse{AteApiError: "dial ate-api: refused"}, skillsTestAgent, kagentHarness)
	if broken.err == nil || broken.empty() || !strings.Contains(broken.lines()[0], "dial ate-api: refused") {
		t.Errorf("an ate-api error: %+v %v", broken, broken.lines())
	}
	long := strings.Repeat("a", 60)
	if prefix := actorTemplatePrefix(long, kagentHarness); len(prefix) != actorTemplateNameSize+1 || !strings.HasSuffix(prefix, "-") {
		t.Errorf("a long name's prefix = %q", prefix)
	}
	if prefix := actorTemplatePrefix("My_Agent", kagentHarness); prefix != "my-agent-kagent-" {
		t.Errorf("prefix = %q", prefix)
	}
}

// TestSkillReplyProves: the answer must name the skill and carry the fact
// from its text, case-insensitively; each miss says which.
func TestSkillReplyProves(t *testing.T) {
	if err := skillReplyProves("Skills: Agent-Self-Awareness. The bridge is klaus-gateway.", skillsTestSkill, skillsTestFact); err != nil {
		t.Error(err)
	}
	if err := skillReplyProves("I have no skills configured.", skillsTestSkill, skillsTestFact); err == nil || !strings.Contains(err.Error(), "does not name the skill") {
		t.Errorf("no skill: %v", err)
	}
	if err := skillReplyProves("agent-self-awareness; the application is Slack itself.", skillsTestSkill, skillsTestFact); err == nil || !strings.Contains(err.Error(), "not \"klaus-gateway\"") {
		t.Errorf("no fact: %v", err)
	}
}

// TestGrepLines: matching is case-insensitive over any pattern, bounded to
// the last n lines; the tail helper drops empty lines.
func TestGrepLines(t *testing.T) {
	const gateLine = "actor 01a0 not RUNNING"
	text := "a first\nEGRESS denied for 01a0\nnoise\n\n" + gateLine + "\nlast"
	if got := grepLines(text, 10, "", "egress", "01a0"); len(got) != 2 || got[0] != "EGRESS denied for 01a0" {
		t.Errorf("grep = %q", got)
	}
	if got := grepLines(text, 1, "", "01a0"); len(got) != 1 || got[0] != gateLine {
		t.Errorf("bounded grep = %q", got)
	}
	if got := grepLines(text, 10, "", ""); len(got) != 0 {
		t.Errorf("an empty pattern matched %q", got)
	}
	if got := grepLines(text, 10, "actor", "denied", "running"); len(got) != 1 || got[0] != gateLine {
		t.Errorf("must + any = %q", got)
	}
	if got := lastLines(text, 2); len(got) != 2 || got[0] != gateLine || got[1] != "last" {
		t.Errorf("tail = %q", got)
	}
	if got := indentLines([]string{"x"}); got[0] != "  x" {
		t.Errorf("indentLines = %q", got)
	}
}

// TestLineFactsWording: the facts read as five lines and one summary, an
// unreadable fact worded as such.
func TestLineFactsWording(t *testing.T) {
	facts := lineFacts{metaChart: "3.22.1-dev (branch poc)", kagentChart: "0.11.0-dev", kagentCRDsChart: "0.11.0-dev", substrateChart: "0.0.27-dev", controllerVersion: "0.11.0-dev (c231bd6)", controllerImage: "ghcr.io/x/controller@sha256:1", harnessImage: "ghcr.io/x/golang-adk@sha256:2", workerPool: "kagent-default", propagatesToken: true}
	lines := facts.lines()
	if len(lines) != 5 || !strings.Contains(lines[4], propagateIdentityEnv+"=true") || !strings.Contains(lines[0], "3.22.1-dev (branch poc)") {
		t.Errorf("lines = %q", lines)
	}
	if s := facts.summary(); !strings.Contains(s, "kagent 0.11.0-dev") || !strings.Contains(s, "Substrate 0.0.27-dev") {
		t.Errorf("summary = %q", s)
	}
	if got := unreadable(nil); got != "not found" {
		t.Errorf("unreadable(nil) = %q", got)
	}
	if got := unreadable(errors.New("boom")); !strings.HasPrefix(got, "unreadable (boom)") {
		t.Errorf("unreadable(err) = %q", got)
	}
	if warningsNote(nil) != "" || !strings.Contains(warningsNote([]string{"w"}), "[w]") {
		t.Error("warningsNote")
	}
	if SkillsTestReadyTimeout < 5*time.Minute {
		t.Error("the golden boot's default wait is too short for a cold lab")
	}
}
