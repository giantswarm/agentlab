package lab

import (
	"errors"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// The releases and builds the tests step to.
const (
	testStepRC    = "1.8.1-rc.3"
	testAteAPIDev = "ateapi:dev"
	testMusterDev = "muster:dev"
)

// TestSubstrateStepOverlay: the overlay pins both Substrate components to
// the exact release, which the meta chart turns into the control plane's
// chart and the WorkerPool's worker image alike.
func TestSubstrateStepOverlay(t *testing.T) {
	raw, err := substrateStepOverlay(testStepRC)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Components map[string]struct {
			VersionRange string `yaml:"versionRange"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("the overlay is no YAML: %v\n%s", err, raw)
	}
	if len(values.Components) != 2 {
		t.Errorf("the overlay names %d components, want substrate and substrate-crds alone:\n%s", len(values.Components), raw)
	}
	for _, component := range []string{substrateRelease, substrateCRDsRelease} {
		if got := values.Components[component].VersionRange; got != testStepRC {
			t.Errorf("components.%s.versionRange = %q, want the exact release", component, got)
		}
	}
}

// TestSubstrateStepConfig: the step's configuration adds the overlay after
// the lab's own values files and the step's dev images over the lab's,
// validated, and leaves the lab's configuration untouched; an unknown dev
// image target is refused.
func TestSubstrateStepConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.ValuesFiles = []string{"mine.yaml"}
	cfg.Platform.DevImages = map[string]string{componentMuster: testMusterDev}

	stepped, err := substrateStepConfig(cfg, "1.8.0", "state/step.yaml", map[string]string{config.DevImageSubstrateAPI: testAteAPIDev})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stepped.Platform.ValuesFiles, ","); got != "mine.yaml,state/step.yaml" {
		t.Errorf("stepped values files = %s, want the lab's, then the overlay", got)
	}
	if stepped.Platform.DevImages[componentMuster] != testMusterDev || stepped.Platform.DevImages[config.DevImageSubstrateAPI] != testAteAPIDev {
		t.Errorf("stepped dev images = %v, want the lab's and the step's", stepped.Platform.DevImages)
	}
	if len(cfg.Platform.ValuesFiles) != 1 || len(cfg.Platform.DevImages) != 1 {
		t.Errorf("the lab's configuration changed: values files %v, dev images %v", cfg.Platform.ValuesFiles, cfg.Platform.DevImages)
	}

	if _, err := substrateStepConfig(cfg, "1.8.0", "state/step.yaml", map[string]string{"ateapi": testAteAPIDev}); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("an unknown dev image target must be refused, got %v", err)
	}
	cfg.Platform.DevImages = nil
	if stepped, err := substrateStepConfig(cfg, "1.8.0", "state/step.yaml", nil); err != nil || len(stepped.Platform.DevImages) != 0 {
		t.Errorf("no dev images on either side: %v, %v", stepped.Platform.DevImages, err)
	}
}

// TestStepFrom: the proof starts on an older Substrate release than it
// steps to, compared in full — a minor step, a patch step and a prerelease
// of the next patch are steps; the same release, a newer one and what is no
// version are refused.
func TestStepFrom(t *testing.T) {
	minor, patch, rc := semver.MustParse("1.8.0"), semver.MustParse("1.8.2"), semver.MustParse("1.8.2-rc.5")
	for _, tc := range []struct {
		before string
		to     *semver.Version
		ok     bool
	}{
		{"1.7.0", minor, true}, // a minor step
		{"1.7.3", minor, true},
		{"0.9.0", minor, true},
		{"1.8.1", patch, true},      // a patch step
		{"1.8.2-rc.4", patch, true}, // a prerelease to its release
		{"1.8.1", rc, true},         // a prerelease of the next patch
		{"1.8.2-rc.4", rc, true},    // the prerelease before it
		{"1.8.0", minor, false},     // the same release
		{"1.8.2-rc.5", rc, false},
		{"1.8.1-rc.3", minor, false}, // a newer one
		{"1.8.2", rc, false},
		{"1.8.3", patch, false},
		{"1.9.0", minor, false},
		{"2.0.0", minor, false},
		{"latest", minor, false}, // no version
	} {
		err := stepFrom(tc.before, tc.to)
		if (err == nil) != tc.ok {
			t.Errorf("stepFrom(%s, %s) = %v, want ok %v", tc.before, tc.to, err, tc.ok)
		}
	}
	if err := stepFrom("1.8.0", minor); err == nil || !strings.Contains(err.Error(), "agentlab platform") {
		t.Errorf("the refusal must name the way back to the chart's Substrate, got %v", err)
	}
}

// TestCurrentSnapshotLayout: the layout a release writes is PerVolume from
// the first release that records one (1.8.1-rc.4) and none recorded before
// it, so the released line's step 1.8.1 -> 1.8.2 keeps the layout and its
// goldens, while 1.8.0 -> 1.8.1 (and 1.7.x -> 1.8.x) takes them again; from
// 1.17.5-rc.2 on it is the gVisor nightly's layout 3, so a step onto it takes
// every golden again before the proof's new conversation starts.
func TestCurrentSnapshotLayout(t *testing.T) {
	v := func(major, minor, patch uint64, pre string) *semver.Version {
		return semver.New(major, minor, patch, pre, "")
	}
	for _, tc := range []struct {
		release *semver.Version
		want    int32
	}{
		{v(1, 7, 0, ""), snapshotLayoutUnrecorded},
		{v(1, 8, 0, ""), snapshotLayoutUnrecorded},
		{v(1, 8, 1, "rc.3"), snapshotLayoutUnrecorded},
		{v(1, 8, 1, "rc.4"), snapshotLayoutPerVolume},
		{v(1, 8, 1, ""), snapshotLayoutPerVolume},
		{v(1, 8, 2, "rc.5"), snapshotLayoutPerVolume},
		{v(1, 8, 2, ""), snapshotLayoutPerVolume},
		{v(1, 9, 0, ""), snapshotLayoutPerVolume},
		{v(1, 17, 4, ""), snapshotLayoutPerVolume},
		{v(1, 17, 5, "rc.1"), snapshotLayoutPerVolume},
		{v(1, 17, 5, "rc.2"), snapshotLayoutGVisorNightly20260909},
		{v(1, 17, 5, "rc.3"), snapshotLayoutGVisorNightly20260909},
		{v(1, 17, 5, ""), snapshotLayoutGVisorNightly20260909},
		{v(1, 18, 0, ""), snapshotLayoutGVisorNightly20260909},
	} {
		if got := currentSnapshotLayout(tc.release); got != tc.want {
			t.Errorf("currentSnapshotLayout(%s) = %d, want %d", tc.release, got, tc.want)
		}
	}
	if currentSnapshotLayout(v(1, 8, 1, "")) != currentSnapshotLayout(v(1, 8, 2, "")) {
		t.Error("the patch step 1.8.1 -> 1.8.2 keeps the layout: its goldens are kept")
	}
	if currentSnapshotLayout(v(1, 8, 0, "")) == currentSnapshotLayout(v(1, 8, 1, "")) {
		t.Error("the step to 1.8.1 changes the recorded layout: its goldens are taken again")
	}
	if currentSnapshotLayout(v(1, 17, 4, "")) == currentSnapshotLayout(v(1, 17, 5, "rc.2")) {
		t.Error("the step 1.17.4 -> 1.17.5-rc.2 changes the layout (the gVisor nightly's checkpoint image): its goldens are taken again before a new conversation")
	}
}

// TestImageTag: the release is the atelet's tag, a digest after it ignored;
// an image without a version tag cannot be placed.
func TestImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"gsoci.azurecr.io/giantswarm/substrate/atelet:1.7.0":                                  "1.7.0",
		"gsoci.azurecr.io/giantswarm/substrate/atelet:1.8.1-rc.3":                             "1.8.1-rc.3",
		"localhost:5000/substrate/atelet:1.8.0@sha256:" + strings.Repeat("a", 64):             "1.8.0",
		"gsoci.azurecr.io/giantswarm/substrate/atelet:1.7.1-r588f3d76t20261011143316hcebc6f9": "1.7.1-r588f3d76t20261011143316hcebc6f9",
	} {
		got, err := imageTag(image)
		if err != nil || got != want {
			t.Errorf("imageTag(%s) = %q, %v, want %s", image, got, err, want)
		}
	}
	for _, image := range []string{"gsoci.azurecr.io/giantswarm/substrate/atelet", "localhost:5000/atelet:dev"} {
		if _, err := imageTag(image); err == nil {
			t.Errorf("imageTag(%s) must fail", image)
		}
	}
}

// TestWorkersOff: a worker on another image, one not Ready and one
// terminating are each named; Ready workers on the image are none; no
// worker at all is one entry.
// stepPod is a pod of one container on the image, Ready or not.
func stepPod(name, img string, ready bool) unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{nameKey: name},
		"spec":     map[string]any{podContainersKey: []any{map[string]any{nameKey: "worker", imageKey: img}}},
		crStatus:   map[string]any{crConditions: []any{map[string]any{"type": conditionReady, crStatus: status}}},
	}}
}

func TestWorkersOff(t *testing.T) {
	const image = "gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.8.0"
	if off := workersOff([]unstructured.Unstructured{stepPod("w1", image, true), stepPod("w2", image, true)}, image); len(off) != 0 {
		t.Errorf("Ready workers on the image: %v, want none", off)
	}
	terminating := stepPod("w4", image, true)
	now := metav1.Now()
	terminating.SetDeletionTimestamp(&now)
	off := workersOff([]unstructured.Unstructured{
		stepPod("w1", image, true),
		stepPod("w2", "gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.7.0", true),
		stepPod("w3", image, false),
		terminating,
	}, image)
	want := []string{"w2 runs gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.7.0", "w3 is not Ready", "w4 is terminating"}
	if strings.Join(off, "|") != strings.Join(want, "|") {
		t.Errorf("workersOff = %q, want %q", off, want)
	}
	if off := workersOff(nil, image); len(off) != 1 || !strings.Contains(off[0], "no worker pod") {
		t.Errorf("no workers = %v, want one entry saying so", off)
	}
}

// TestTemplateOfRevision: the agent's ActorTemplate of a revision is told
// apart from its others by the revision's first twelve characters; without
// a revision only a single template answers.
func TestTemplateOfRevision(t *testing.T) {
	old := substrateTemplate{namespace: kagentNamespace, name: substrateStepAgent + "-0123456789ab", golden: "golden tag kagent/a"}
	current := substrateTemplate{namespace: kagentNamespace, name: substrateStepAgent + "-ba9876543210", golden: "golden tag kagent/b"}
	got, err := templateOfRevision([]substrateTemplate{old, current}, substrateStepAgent, "BA9876543210ffff")
	if err != nil || got.name != current.name {
		t.Errorf("templateOfRevision = %v, %v, want %s", got, err, current.name)
	}
	if _, err := templateOfRevision([]substrateTemplate{old}, substrateStepAgent, "ba9876543210"); err == nil {
		t.Error("a revision without its template must fail")
	}
	if got, err := templateOfRevision([]substrateTemplate{old}, substrateStepAgent, ""); err != nil || got.name != old.name {
		t.Errorf("one template, no revision = %v, %v, want it", got, err)
	}
	if _, err := templateOfRevision([]substrateTemplate{old, current}, substrateStepAgent, ""); err == nil {
		t.Error("two templates and no revision must fail")
	}
}

// TestRestoreVerdict: Substrate 1.8.0's refusal of a snapshot in the layout
// before the per-volume one is named as the cause; other evidence is quoted
// as it is.
func TestRestoreVerdict(t *testing.T) {
	refused := restoreVerdict([]string{
		"session s1 FAILED: RuntimeError resume failed",
		"actor agentlab-substrate-step-x: CRASHED, crash: FailedPrecondition: snapshot written before the per-volume layout; re-snapshot from a running actor or reset the golden snapshot",
	})
	if !strings.HasPrefix(refused, "the restore refusal:") || !strings.Contains(refused, "CRASHED") {
		t.Errorf("the refusal must be named with its line, got %q", refused)
	}
	other := restoreVerdict([]string{"session s1 FAILED: Timeout", "actor x: RUNNING"})
	if other != "session s1 FAILED: Timeout; actor x: RUNNING" {
		t.Errorf("other evidence = %q, want it quoted", other)
	}
	if got := restoreVerdict(nil); !strings.Contains(got, "no failure recorded") {
		t.Errorf("no evidence = %q", got)
	}
}

// TestResumedReplyProves: the conversation is the first turn's codeword, the
// volume is the marker the file holds; each is judged on its own.
func TestResumedReplyProves(t *testing.T) {
	for _, tc := range []struct {
		reply                string
		conversation, volume bool
	}{
		{"step-1a2b3c4d substrate-step 0011223344556677", true, true},
		{"step-1a2b3c4d cat: /data/agentlab-substrate-step/marker: No such file or directory", true, false},
		{"I do not know a codeword. substrate-step 0011223344556677", false, true},
		{"", false, false},
	} {
		conversation, volume := resumedReplyProves(tc.reply, "step-1a2b3c4d", "substrate-step 0011223344556677")
		if conversation != tc.conversation || volume != tc.volume {
			t.Errorf("resumedReplyProves(%q) = %v, %v, want %v, %v", tc.reply, conversation, volume, tc.conversation, tc.volume)
		}
	}
}

// TestSubstrateStepPrompts: the file is written to the durable directory and
// read back from it; the marker reaches the shell quoted.
func TestSubstrateStepPrompts(t *testing.T) {
	write := writeMarkerPrompt("substrate-step 00ff")
	for _, want := range []string{"mkdir -p /data/agentlab-substrate-step", "printf '%s\\n' 'substrate-step 00ff' > /data/agentlab-substrate-step/marker", "cat /data/agentlab-substrate-step/marker"} {
		if !strings.Contains(write, want) {
			t.Errorf("the write prompt lacks %q:\n%s", want, write)
		}
	}
	if !strings.Contains(resumePrompt, "cat "+substrateStepFile) {
		t.Errorf("the resume prompt must read %s back: %s", substrateStepFile, resumePrompt)
	}
	if !strings.Contains(codewordPrompt("step-ab"), "step-ab") {
		t.Error("the first turn carries the codeword")
	}
}

// TestSubstrateStepTemplate: the proof's agent is an AgentTemplate labelled
// as agentlab's on the given ModelConfig, with a skill, and the Agent pairing
// it with the platform Harness.
func TestSubstrateStepTemplate(t *testing.T) {
	manifest := substrateStepTemplate(defaultModelConfig)
	objs, err := decodeManifests([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].GetKind() != "AgentTemplate" || objs[1].GetKind() != kindAgent {
		t.Fatalf("want an AgentTemplate and an Agent, got %d objects:\n%s", len(objs), manifest)
	}
	for _, obj := range objs {
		if obj.GetName() != substrateStepAgent || obj.GetNamespace() != kagentNamespace || obj.GetLabels()[managedByLabel] != managedByAgentlabValue {
			t.Errorf("%s %s/%s labels %v: want %s/%s labelled as agentlab's", obj.GetKind(), obj.GetNamespace(), obj.GetName(), obj.GetLabels(), kagentNamespace, substrateStepAgent)
		}
	}
	if got, _, _ := unstructured.NestedString(objs[0].Object, "spec", "modelConfig", "name"); got != defaultModelConfig {
		t.Errorf("modelConfig = %q", got)
	}
	skills, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "skills")
	if len(skills) != 1 {
		t.Errorf("the template must carry one skill, the Go ADK's condition for its bash tool: %v", skills)
	}
	if got, _, _ := unstructured.NestedString(objs[1].Object, "spec", "harnessRef", "name"); got != kagentHarness {
		t.Errorf("harnessRef = %q, want %s", got, kagentHarness)
	}
}

// TestDevImagesNote words the step's dev images in a stable order.
func TestDevImagesNote(t *testing.T) {
	if got := devImagesNote(nil); got != "" {
		t.Errorf("no dev images = %q", got)
	}
	got := devImagesNote(map[string]string{config.DevImageSubstrateAPI: testAteAPIDev, componentMuster: testMusterDev})
	if got != ", with the dev images ate-api-server=ateapi:dev, muster=muster:dev" {
		t.Errorf("devImagesNote = %q", got)
	}
}

// substrateAPIRender is the part of the substrate chart's render the
// ate-api-server target reads.
const substrateAPIRender = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ate-api-server
  namespace: ate-system
spec:
  template:
    spec:
      containers:
      - name: ate-api-server
        image: gsoci.azurecr.io/giantswarm/substrate/ateapi:1.8.1-rc.3
`

// TestSubstrateAPIDevImage: the ate-api-server target replaces the image the
// substrate chart renders on its Deployment and rides the substrate
// release's postRenderers, which the values template renders as a component
// block of its own.
func TestSubstrateAPIDevImage(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.DevImages = map[string]string{config.DevImageSubstrateAPI: "ateapi:retake"}
	names, err := resolveDevImageNames(cfg, map[string]string{substrateRelease: substrateAPIRender})
	if err != nil {
		t.Fatal(err)
	}
	if got := names[config.DevImageSubstrateAPI]; got != "gsoci.azurecr.io/giantswarm/substrate/ateapi" {
		t.Errorf("resolved %q, want the chart's ateapi image", got)
	}
	postRenderers, err := componentPostRenderers(cfg, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	patch := postRenderers[substrateRelease]
	for _, want := range []string{"name: gsoci.azurecr.io/giantswarm/substrate/ateapi", "newName: localhost/ateapi", "newTag: retake", "name: ate-api-server"} {
		if !strings.Contains(patch, want) {
			t.Errorf("the substrate postRenderers lack %q:\n%s", want, patch)
		}
	}
	data := &tmplData{Config: cfg, PostRenderers: postRenderers}
	if _, ok := data.ExtraPostRenderers()[substrateRelease]; !ok {
		t.Errorf("the substrate component has no roster block: its postRenderers must render as an extra one")
	}
}

// TestActorLine words an actor's state without the enum prefix, with the
// crash message when there is one.
func TestActorLine(t *testing.T) {
	crashed := &ateapi.Actor{
		Metadata: &ateapi.ResourceMetadata{Name: "session-1"},
		Status:   &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_CRASHED, Crash: &ateapi.ActorCrash{Message: "snapshot " + legacyLayoutRefusal}},
	}
	if got := actorLine(crashed); got != "actor session-1: CRASHED, crash: snapshot "+legacyLayoutRefusal {
		t.Errorf("actorLine = %q", got)
	}
	if got := restoreVerdict([]string{actorLine(crashed)}); !strings.HasPrefix(got, "the restore refusal:") {
		t.Errorf("a crashed actor's line must read as the restore refusal, got %q", got)
	}
	running := &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Name: "session-2"}, Status: &ateapi.ActorStatus{State: ateapi.ActorState_ACTOR_STATE_RUNNING}}
	if got := actorLine(running); got != "actor session-2: RUNNING" {
		t.Errorf("actorLine = %q", got)
	}
}

// TestRustfsServiceAccountPatch: the patch removes the pod spec's
// ServiceAccount and its deprecated alias, each only where present.
func TestRustfsServiceAccountPatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec map[string]any
		want string
	}{
		{"both", map[string]any{podServiceAccountNameKey: rustfsDeployment, podServiceAccountKey: rustfsDeployment},
			`[{"op":"remove","path":"/spec/template/spec/serviceAccountName"},{"op":"remove","path":"/spec/template/spec/serviceAccount"}]`},
		{"the alias alone", map[string]any{podServiceAccountKey: rustfsDeployment},
			`[{"op":"remove","path":"/spec/template/spec/serviceAccount"}]`},
		{"neither", map[string]any{podContainersKey: []any{}}, ""},
		{"no spec", nil, ""},
	} {
		if got := string(rustfsServiceAccountPatch(tc.spec)); got != tc.want {
			t.Errorf("%s: patch %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The leftovers the cleanup tests read: an earlier run's actor and the
// proof agent's template.
const (
	testEarlierActor     = "c7eba5fd"
	testEarlierActorLine = "actor c7eba5fd: DELETING"
	testStepTemplate     = "agentlab-substrate-step-00aef75d6578"
)

// TestSplitLeftovers: what an earlier run left, by name, is told apart from
// this run's own; a failed read is always this run's.
func TestSplitLeftovers(t *testing.T) {
	earlier := leftoverNames([]stepLeftover{
		{name: testEarlierActor, line: testEarlierActorLine},
		{line: "ListActors: unavailable"},
	})
	if len(earlier) != 1 || !earlier[testEarlierActor] {
		t.Fatalf("earlier names %v, want c7eba5fd alone", earlier)
	}
	own, inherited := splitLeftovers([]stepLeftover{
		{name: testEarlierActor, line: testEarlierActorLine},
		{name: "a1b2c3d4", line: "actor a1b2c3d4: SUSPENDED"},
		{line: "ListActors: unavailable"},
	}, earlier)
	if strings.Join(inherited, "|") != testEarlierActorLine {
		t.Errorf("inherited %q, want the earlier run's actor", inherited)
	}
	if strings.Join(own, "|") != "actor a1b2c3d4: SUSPENDED|ListActors: unavailable" {
		t.Errorf("own %q, want this run's actor and the failed read", own)
	}
}

// TestFootprintLeftovers: kagent's view of the agent becomes named
// leftovers, a failed read one without a name.
func TestFootprintLeftovers(t *testing.T) {
	left := footprintLeftovers(substrateFootprint{
		templates: []substrateTemplate{{namespace: kagentNamespace, name: testStepTemplate, phase: "Ready"}},
		actors:    []substrateActor{{id: testEarlierActor, templateName: testStepTemplate, state: "DELETING"}},
	})
	if len(left) != 2 || left[0].name != testStepTemplate || left[1].name != testEarlierActor {
		t.Fatalf("leftovers %+v, want the template and the actor by name", left)
	}
	if !strings.Contains(left[1].line, "DELETING") {
		t.Errorf("actor line %q does not word its state", left[1].line)
	}
	if left := footprintLeftovers(substrateFootprint{err: errors.New("unavailable")}); len(left) != 1 || left[0].name != "" {
		t.Errorf("a failed read: %+v, want one unnamed leftover", left)
	}
}
