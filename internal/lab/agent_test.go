package lab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The fixtures of the agent helper's tests: a spec with everything set, and
// the names of the shapes waitAgentReady is asked about.
const (
	testAgentName   = "probe"
	testSkillURL    = "https://github.com/giantswarm/agent-skills"
	testSkillName   = "runbooks"
	testCommit      = "0123456789abcdef0123456789abcdef01234567"
	testHeadCommit  = "89abcdef0123456789abcdef0123456789abcdef"
	testMusterURL   = "http://muster.agent-platform.svc.cluster.local:8090/mcp"
	testIconURL     = "https://avatars.example.test/probe.svg"
	testDescription = "a probe"
	testDisplay     = "Probe"
	testNoHarness   = "nobody"
	helmReleaseKind = "HelmRelease"
	fieldText       = "text"
)

func testSpec() agentSpec {
	return agentSpec{
		Name: testAgentName, ModelConfig: defaultModelConfig, DisplayName: testDisplay, Description: testDescription,
		SystemMessage: "Reply with pong.", IconURL: testIconURL, Toolset: []string{presetReadOnly, workflowIncidentTriage},
		Skills: []agentSkill{{Name: testSkillName, Path: testSkillName, Git: &gitSkill{URL: testSkillURL, Commit: testCommit}}},
	}
}

// TestCreateAgentArgs: create_agent's arguments are the spec's set fields
// only — the toolset as given, the skills as the list agent-manager takes —
// and a spec for another Harness is refused (agent-manager places every agent
// on the platform Harness).
func TestCreateAgentArgs(t *testing.T) {
	spec := testSpec()
	args, err := createAgentArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		nameKey: testAgentName, modelConfigKey: defaultModelConfig, displayNameKey: testDisplay, descriptionKey: testDescription,
		systemMessageKey: "Reply with pong.", iconURLKey: testIconURL, toolsetKey: spec.Toolset, skillsKey: spec.Skills,
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
	bare, err := createAgentArgs(agentSpec{Name: testAgentName, ModelConfig: defaultModelConfig})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bare, map[string]any{nameKey: testAgentName, modelConfigKey: defaultModelConfig}) {
		t.Errorf("a bare spec composes %v", bare)
	}
	if _, err := createAgentArgs(agentSpec{Name: testAgentName, ModelConfig: defaultModelConfig, Harness: "claude"}); err == nil || !strings.Contains(err.Error(), "direct HelmRelease writer") {
		t.Errorf("another Harness through agent-manager: %v", err)
	}
	if _, err := createAgentArgs(agentSpec{Name: testAgentName, ModelConfig: defaultModelConfig, Harness: kagentHarness}); err != nil {
		t.Errorf("the platform Harness named explicitly: %v", err)
	}
}

// TestAgentManifests: the direct writer composes agent-manager's two
// objects — the shared OCIRepository of the chart at 1.x and the agent's
// HelmRelease as the tenant ServiceAccount on the chart values, skills pinned
// without a ref, the Harness the template is placed on; a spec without a
// toolset renders no toolset key.
func TestAgentManifests(t *testing.T) {
	var source map[string]any
	if err := yaml.Unmarshal([]byte(agentOCIRepositoryManifest()), &source); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: source}
	url, _, _ := unstructured.NestedString(source, "spec", "url")
	semver, _, _ := unstructured.NestedString(source, "spec", "ref", "semver")
	if u.GetAPIVersion() != fluxOCIRepositoryAPIVersion || u.GetKind() != kindOCIRepository || u.GetName() != agentChartOCIRepository || u.GetNamespace() != kagentNamespace || url != agentChartURL || semver != agentChartRange {
		t.Errorf("OCIRepository:\n%s", agentOCIRepositoryManifest())
	}

	spec := testSpec()
	spec.Skills[0].Git.Ref = "main"
	var release map[string]any
	if err := yaml.Unmarshal([]byte(agentHelmReleaseManifest(spec)), &release); err != nil {
		t.Fatalf("%v\n%s", err, agentHelmReleaseManifest(spec))
	}
	u = &unstructured.Unstructured{Object: release}
	sa, _, _ := unstructured.NestedString(release, "spec", "serviceAccountName")
	chartRef, _, _ := unstructured.NestedStringMap(release, "spec", "chartRef")
	if u.GetAPIVersion() != fluxHelmReleaseAPIVersion || u.GetKind() != helmReleaseKind || u.GetName() != testAgentName || u.GetNamespace() != kagentNamespace ||
		u.GetLabels()[managedByLabel] != managedByAgentlabValue || sa != kagentFluxServiceAccount ||
		!reflect.DeepEqual(chartRef, map[string]string{fieldKind: kindOCIRepository, nameKey: agentChartOCIRepository, fieldNamespace: kagentNamespace}) {
		t.Errorf("HelmRelease head:\n%s", agentHelmReleaseManifest(spec))
	}
	values, _, _ := unstructured.NestedMap(release, "spec", "values")
	toolset, _, _ := unstructured.NestedStringSlice(values, toolsetKey)
	harness, _, _ := unstructured.NestedString(values, "agent", "harness")
	displayName, _, _ := unstructured.NestedString(values, "agent", displayNameKey)
	iconURL, _, _ := unstructured.NestedString(values, "agent", iconURLKey)
	modelConfig, _, _ := unstructured.NestedString(values, modelConfigKey, nameKey)
	skills, _, _ := unstructured.NestedSlice(values, skillsKey)
	if !reflect.DeepEqual(toolset, spec.Toolset) || harness != kagentHarness || displayName != testDisplay || iconURL != testIconURL || modelConfig != defaultModelConfig || len(skills) != 1 {
		t.Errorf("values = %v", values)
	}
	skill, _ := skills[0].(map[string]any)
	if commit, _, _ := unstructured.NestedString(skill, "git", "commit"); commit != testCommit {
		t.Errorf("skill = %v", skill)
	}
	if _, hasRef, _ := unstructured.NestedString(skill, "git", "ref"); hasRef {
		t.Errorf("the chart takes no ref, got %v", skill)
	}
	if got := skillCommits(values); !reflect.DeepEqual(got, map[string]string{testSkillName: testCommit}) {
		t.Errorf("skillCommits = %v", got)
	}

	unscoped := agentValues(agentSpec{Name: testAgentName, ModelConfig: defaultModelConfig, Harness: testNoHarness})
	if _, declared := unscoped[toolsetKey]; declared {
		t.Errorf("a spec without a toolset renders one: %v", unscoped)
	}
	if harness, _, _ := unstructured.NestedString(unscoped, "agent", "harness"); harness != testNoHarness {
		t.Errorf("agent.harness = %q, want the spec's", harness)
	}
	if _, hasSkills := unscoped[skillsKey]; hasSkills {
		t.Errorf("a spec without skills renders some: %v", unscoped)
	}
}

// helmRelease seeds an agent's HelmRelease with values (as the apiserver
// stores them: JSON types) and one Ready condition ("" for none).
func helmRelease(name string, values map[string]any, ready, reason, message string) *unstructured.Unstructured {
	hr := customObject(fluxHelmReleaseGVK, kagentNamespace, name, map[string]string{managedByLabel: agentManagerFieldManager})
	_ = unstructured.SetNestedField(hr.Object, kagentFluxServiceAccount, "spec", "serviceAccountName")
	_ = unstructured.SetNestedField(hr.Object, map[string]any{fieldKind: kindOCIRepository, nameKey: agentChartOCIRepository, fieldNamespace: kagentNamespace}, "spec", "chartRef")
	if values != nil {
		raw, _ := json.Marshal(values)
		var stored map[string]any
		_ = json.Unmarshal(raw, &stored)
		_ = unstructured.SetNestedField(hr.Object, stored, "spec", "values")
	}
	if ready != "" {
		_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{fieldType: condReady, fieldStatus: ready, fieldReason: reason, fieldMessage: message}}, fieldStatus, "conditions")
	}
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"chartVersion": "1.0.5+cb7655f077c0", fieldStatus: "deployed"}}, fieldStatus, "history")
	_ = unstructured.SetNestedSlice(hr.Object, []any{map[string]any{managerField: agentManagerFieldManager, operationField: "Apply"}, map[string]any{managerField: "helm-controller", operationField: "Update", "subresource": fieldStatus}}, "metadata", "managedFields")
	return hr
}

// TestReadAgentRelease: the release is read the way the proofs assert it —
// who wrote it, the chart it renders from, values.toolset (declared or not,
// refused when not a string list), the skills' pins, one string value, the
// deployed chart version, the Ready condition — and a missing one is the
// apiserver's NotFound.
func TestReadAgentRelease(t *testing.T) {
	values := agentValues(testSpec())
	odd := helmRelease("odd", map[string]any{toolsetKey: presetReadOnly}, "", "", "")
	newFakeLab(t,
		helmRelease(testAgentName, values, conditionTrue, "InstallSucceeded", "Helm install succeeded"),
		helmRelease("unscoped", agentValues(agentSpec{Name: "unscoped", ModelConfig: defaultModelConfig}), condFalse, helmInstallFailed, retriesExhausted),
		odd,
	)
	release, err := readAgentRelease(testAgentName)
	if err != nil {
		t.Fatal(err)
	}
	toolset, declared, err := release.toolset()
	if err != nil || !declared || !reflect.DeepEqual(toolset, []string{presetReadOnly, workflowIncidentTriage}) {
		t.Errorf("toolset = %v %v %v", toolset, declared, err)
	}
	if !reflect.DeepEqual(release.managers, []string{agentManagerFieldManager, "helm-controller"}) || release.Spec.ChartRef.Name != agentChartOCIRepository || release.Spec.ServiceAccountName != kagentFluxServiceAccount {
		t.Errorf("release head = %+v managers %v", release.Spec, release.managers)
	}
	if got := release.skillCommits(); !reflect.DeepEqual(got, map[string]string{testSkillName: testCommit}) {
		t.Errorf("skillCommits = %v", got)
	}
	if release.value("agent", "harness") != kagentHarness || release.value("agent", "nope") != "" || release.chartVersion() != "1.0.5+cb7655f077c0" {
		t.Errorf("value/chartVersion: %q %q", release.value("agent", "harness"), release.chartVersion())
	}
	if status, reason, _ := release.ready(); status != conditionTrue || reason != "InstallSucceeded" {
		t.Errorf("ready = %q %q", status, reason)
	}
	unscoped, err := readAgentRelease("unscoped")
	if err != nil {
		t.Fatal(err)
	}
	if toolset, declared, err := unscoped.toolset(); err != nil || declared || toolset != nil {
		t.Errorf("an unscoped release: %v %v %v", toolset, declared, err)
	}
	if status, reason, message := unscoped.ready(); status != condFalse || reason != helmInstallFailed || message != retriesExhausted {
		t.Errorf("a failed release: %q %q %q", status, reason, message)
	}
	oddRelease, err := readAgentRelease("odd")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := oddRelease.toolset(); err == nil || !strings.Contains(err.Error(), "not a string list") {
		t.Errorf("a scalar toolset: %v", err)
	}
	if _, err := readAgentRelease("absent"); !apierrors.IsNotFound(err) {
		t.Errorf("a missing release: %v", err)
	}
	if url, semver, _, err := agentChartSource(); err == nil || url != "" || semver != "" {
		t.Errorf("no OCIRepository seeded, got %q %q %v", url, semver, err)
	}
}

// unadmittedTemplate seeds a template kagent has reported on (observed
// generation caught up) with no Harness admitting it.
func unadmittedTemplate(name string) *unstructured.Unstructured {
	template := customObject(gvkAgentTemplate, kagentNamespace, name, map[string]string{harnessLabel: testNoHarness})
	template.SetGeneration(1)
	_ = unstructured.SetNestedField(template.Object, int64(1), fieldStatus, "observedGeneration")
	_ = unstructured.SetNestedSlice(template.Object, []any{}, fieldStatus, "harnesses")
	return template
}

// TestWaitAgentReady: the one wait ends Ready on the platform Harness's
// desired revision; terminal — with the reason — for a template no Harness
// admits, one another Harness admits, a Harness condition the controller will
// not retry, and a release Helm gave up on; and keeps waiting (naming what it
// saw last) while a revision compiles, while the release has not rendered
// yet, and while nothing is there.
func TestWaitAgentReady(t *testing.T) {
	// A new revision compiling: Ready still True on the last successful
	// revision, the desired one ahead of it.
	compiling := readyTemplate("compiling", kagentHarness, conditionTrue, "ActorTemplate golden snapshot is ready")
	harnesses, _, _ := unstructured.NestedSlice(compiling.Object, fieldStatus, "harnesses")
	harnesses[0].(map[string]any)[fieldDesiredRevision] = "abc123"
	_ = unstructured.SetNestedSlice(compiling.Object, harnesses, fieldStatus, "harnesses")
	blocked := bootTemplate("blocked", condFalse, readyReasonPending, "waiting")
	_ = unstructured.SetNestedSlice(blocked.Object, []any{map[string]any{
		fieldHarness: kagentHarness, fieldDesiredRevision: testRevision,
		fieldConditions: []any{
			map[string]any{fieldType: conditionResolvedRefs, fieldStatus: condFalse, fieldReason: "ModelConfigNotFound", fieldMessage: `ModelConfig "nope" not found`},
			map[string]any{fieldType: condReady, fieldStatus: condFalse, fieldReason: readyReasonPending, fieldMessage: "waiting"},
		},
	}}, fieldStatus, "harnesses")
	newFakeLab(t,
		readyTemplate(testReadyName, kagentHarness, conditionTrue, "ActorTemplate golden snapshot is ready"),
		helmRelease(testReadyName, agentValues(testSpec()), conditionTrue, "InstallSucceeded", ""),
		unadmittedTemplate("unadmitted"),
		readyTemplate("elsewhere", "claude", conditionTrue, "ready"),
		bootTemplate("pending", condFalse, readyReasonPending, "waiting for the ActorTemplate golden snapshot"),
		compiling,
		blocked,
		helmRelease("refused", nil, condFalse, helmInstallFailed, "values don't meet the specifications of the schema(s)"),
		helmRelease("rendering", nil, conditionTrue, "InstallSucceeded", ""),
	)
	r, err := waitAgentReady(testReadyName, 4*time.Second)
	if err != nil || !r.ready || r.terminal || r.template == nil || r.release == nil {
		t.Fatalf("a ready agent: %+v %v", r, err)
	}
	for _, tc := range []struct {
		name     string
		terminal bool
		reason   string
	}{
		{"unadmitted", true, "no Harness admits"},
		{"elsewhere", true, "admitted by claude only"},
		{"blocked", true, "ResolvedRefs=False ModelConfigNotFound"},
		{"refused", true, "InstallFailed"},
		{"pending", false, "Ready=\"False\""},
		{"compiling", false, "revision abc123 compiling"},
		{"rendering", false, "not rendered yet"},
		{"absent", false, "neither a HelmRelease nor an AgentTemplate"},
	} {
		r, err := waitAgentReady(tc.name, pollInterval)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.ready || r.terminal != tc.terminal || !strings.Contains(r.reason, tc.reason) {
			t.Errorf("%s: ready=%v terminal=%v (want %v) reason %q (want %q)", tc.name, r.ready, r.terminal, tc.terminal, r.reason, tc.reason)
		}
		failure := r.failure(tc.name, pollInterval)
		if !strings.Contains(failure.Error(), tc.reason) || !strings.Contains(failure.Error(), "kubectl -n "+kagentNamespace) {
			t.Errorf("%s: the failure does not carry the reason and the hint: %v", tc.name, failure)
		}
		if tc.terminal != strings.Contains(failure.Error(), "failed for good") {
			t.Errorf("%s: the failure's verdict does not match terminal=%v: %v", tc.name, tc.terminal, failure)
		}
	}
}

// TestRemoveAgent: the cleanup takes the HelmRelease, the AgentTemplate and
// the RemoteMCPServer of the agent's name alike — present ones go, a missing
// agent is no failure — and agentExists sees any of the three.
func TestRemoveAgent(t *testing.T) {
	f := newFakeLab(t,
		helmRelease(testAgentName, agentValues(testSpec()), conditionTrue, "", ""),
		agentTemplateBinding(testAgentName, testAgentName),
		agentServer(testAgentName, presetReadOnly),
		agentServer("orphan", ""),
	)
	if !agentExists(testAgentName) || !agentExists("orphan") || agentExists("absent") {
		t.Fatal("agentExists disagrees with the store")
	}
	if err := removeAgent(testAgentName); err != nil {
		t.Fatal(err)
	}
	if err := removeAgent("absent"); err != nil {
		t.Errorf("a missing agent: %v", err)
	}
	if _, err := f.dyn.Tracker().Get(fluxHelmReleaseGVR, kagentNamespace, testAgentName); !apierrors.IsNotFound(err) {
		t.Errorf("HelmRelease still there: %v", err)
	}
	if _, err := f.dyn.Tracker().Get(gvrAgentTemplates, kagentNamespace, testAgentName); !apierrors.IsNotFound(err) {
		t.Errorf("AgentTemplate still there: %v", err)
	}
	if _, err := f.dyn.Tracker().Get(gvrRemoteMCPServers, kagentNamespace, testAgentName); !apierrors.IsNotFound(err) {
		t.Errorf("RemoteMCPServer still there: %v", err)
	}
	if agentExists(testAgentName) {
		t.Error("agentExists still true after the remove")
	}
	if err := waitAgentRemoved(testAgentName); err != nil {
		t.Error(err)
	}
}

// renderedTemplate seeds what the chart renders for a spec: the template
// with the Harness label, the provenance label, the annotations, the model,
// the prompt and the binding of the agent's own server.
func renderedTemplate(spec agentSpec, server string) *unstructured.Unstructured {
	template := agentTemplateBinding(spec.Name, server)
	template.SetLabels(map[string]string{harnessLabel: spec.harnessValue(), fluxHelmReleaseNameLabel: spec.Name})
	template.SetAnnotations(map[string]string{displayNameAnnotation: spec.DisplayName, iconURLAnnotation: spec.IconURL})
	_ = unstructured.SetNestedField(template.Object, spec.ModelConfig, "spec", modelConfigKey, nameKey)
	_ = unstructured.SetNestedField(template.Object, spec.SystemMessage, "spec", "systemPrompt")
	return template
}

// TestAssertAgentRender: the render passes for a bound agent whose server
// points at muster with the header and discovery off, and for a chat-only
// agent with neither; it fails naming the Harness label, a missing header, a
// header on an unscoped release, and an Authorization header.
func TestAssertAgentRender(t *testing.T) {
	spec := testSpec()
	chatOnly := agentSpec{Name: "chat", ModelConfig: defaultModelConfig, DisplayName: "Chat", Toolset: []string{presetNone}}
	unscoped := agentSpec{Name: "unscoped", ModelConfig: defaultModelConfig}
	server := agentServer(spec.Name, strings.Join(spec.Toolset, ","))
	authz := agentServer("authz", presetReadOnly)
	_ = unstructured.SetNestedSlice(authz.Object, []any{map[string]any{nameKey: toolsetHeader, fieldValue: presetReadOnly}, map[string]any{nameKey: "Authorization", fieldValue: "Bearer static"}}, "spec", "headersFrom")
	mislabelled := renderedTemplate(spec, spec.Name)
	mislabelled.SetName("mislabelled")
	mislabelled.SetLabels(map[string]string{"kagent.dev/harness": kagentHarness, fluxHelmReleaseNameLabel: "mislabelled"})
	newFakeLab(t,
		renderedTemplate(spec, spec.Name), server,
		renderedTemplate(chatOnly, ""),
		renderedTemplate(unscoped, unscoped.Name), agentServer(unscoped.Name, presetReadOnly),
		renderedTemplate(agentSpec{Name: "authz", ModelConfig: defaultModelConfig, Toolset: []string{presetReadOnly}}, "authz"), authz,
		mislabelled,
	)
	read := func(name string) *agentTemplate {
		t.Helper()
		template, err := readAgentTemplate(name)
		if err != nil {
			t.Fatal(err)
		}
		return template
	}
	if err := assertAgentRender(read(spec.Name), spec, testMusterURL); err != nil {
		t.Errorf("a bound agent: %v", err)
	}
	if err := assertAgentRender(read(chatOnly.Name), chatOnly, testMusterURL); err != nil {
		t.Errorf("a chat-only agent: %v", err)
	}
	if err := assertAgentRender(read(unscoped.Name), unscoped, testMusterURL); err == nil || !strings.Contains(err.Error(), "although the release declares no toolset") {
		t.Errorf("a header on an unscoped release: %v", err)
	}
	if err := assertAgentRender(read("authz"), agentSpec{Name: "authz", ModelConfig: defaultModelConfig, Toolset: []string{presetReadOnly}}, testMusterURL); err == nil || !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("an Authorization header: %v", err)
	}
	mis := spec
	mis.Name = "mislabelled"
	if err := assertAgentRender(read("mislabelled"), mis, testMusterURL); err == nil || !strings.Contains(err.Error(), harnessLabel) {
		t.Errorf("the wrong admission label: %v", err)
	}
	if err := assertAgentRender(read(spec.Name), spec, "http://elsewhere/mcp"); err == nil || !strings.Contains(err.Error(), "points at") {
		t.Errorf("another muster URL: %v", err)
	}
}

// TestHelmReleaseWriter: the direct writer applies the OCIRepository once
// (the first agent of the namespace creates it, the next reuses it) and one
// HelmRelease per agent, and refuses a git skill without a commit.
func TestHelmReleaseWriter(t *testing.T) {
	f := newFakeLab(t)
	written, err := helmReleaseWriter{}.createAgent(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if !written.HelmRelease || !written.OCIRepository {
		t.Errorf("first agent: %+v, want both created", written)
	}
	source := f.stored(t, fluxOCIRepositoryGVR, kagentNamespace, agentChartOCIRepository)
	if semver, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "semver"); semver != agentChartRange {
		t.Errorf("OCIRepository tracks %q", semver)
	}
	release := f.stored(t, fluxHelmReleaseGVR, kagentNamespace, testAgentName)
	if toolset, _, _ := unstructured.NestedStringSlice(release.Object, "spec", "values", toolsetKey); !reflect.DeepEqual(toolset, testSpec().Toolset) {
		t.Errorf("HelmRelease values.toolset = %v", toolset)
	}
	second := agentSpec{Name: "second", ModelConfig: defaultModelConfig, Toolset: []string{presetNone}}
	written, err = helmReleaseWriter{}.createAgent(second)
	if err != nil {
		t.Fatal(err)
	}
	if !written.HelmRelease || written.OCIRepository {
		t.Errorf("second agent: %+v, want the OCIRepository reused", written)
	}
	unpinned := testSpec()
	unpinned.Skills[0].Git = &gitSkill{URL: testSkillURL, Ref: "main"}
	if _, err := (helmReleaseWriter{}).createAgent(unpinned); err == nil || !strings.Contains(err.Error(), "names no commit") {
		t.Errorf("an unpinned skill: %v", err)
	}
	if got := (helmReleaseWriter{}).String(); !strings.Contains(got, "HelmRelease") {
		t.Errorf("String() = %q", got)
	}
}

// fakeMuster answers muster's call_tool for one agent-manager tool: it
// records the tool's arguments and hands back the given envelope text as the
// tool's result.
func fakeMuster(t *testing.T, tool string, result string, isError bool) (*httptest.Server, *map[string]any) {
	t.Helper()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			ID     int `json:"id"`
			Params struct {
				Name      string `json:"name"`
				Arguments struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &rpc); err != nil || rpc.Params.Name != "call_tool" || rpc.Params.Arguments.Name != tool {
			t.Errorf("unexpected muster request: %s", body)
		}
		captured = rpc.Params.Arguments.Arguments
		envelope, _ := json.Marshal(map[string]any{"isError": isError, "content": []map[string]any{{fieldType: fieldText, fieldText: result}}})
		answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": map[string]any{"content": []map[string]any{{fieldType: fieldText, fieldText: string(envelope)}}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

// TestAgentManagerWriter: the platform path calls x_agent-manager_create_agent
// through muster's call_tool with the spec's arguments and reads the report
// back (who the write ran as, which objects were created); agent-manager's
// refusal is the error.
func TestAgentManagerWriter(t *testing.T) {
	report, _ := json.Marshal(map[string]any{"requestedBy": testDevUser, "created": map[string]bool{"helmRelease": true, "ociRepository": false}})
	srv, captured := fakeMuster(t, agentManagerToolPrefix+"create_agent", string(report), false)
	writer := agentManagerWriter{&musterSession{client: srv.Client(), url: srv.URL, token: testToken}}
	written, err := writer.createAgent(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if written.RequestedBy != testDevUser || !written.HelmRelease || written.OCIRepository {
		t.Errorf("written = %+v", written)
	}
	want, _ := createAgentArgs(testSpec())
	if !reflect.DeepEqual(jsonValue(t, *captured), jsonValue(t, want)) {
		t.Errorf("arguments = %v, want %v", *captured, want)
	}
	if !strings.Contains(writer.String(), "create_agent") {
		t.Errorf("String() = %q", writer)
	}
	refusing, _ := fakeMuster(t, agentManagerToolPrefix+"create_agent", "invalid_request: toolset is required", true)
	if _, err := (agentManagerWriter{&musterSession{client: refusing.Client(), url: refusing.URL, token: testToken}}).createAgent(testSpec()); err == nil || !strings.Contains(err.Error(), "toolset is required") {
		t.Errorf("a refusal: %v", err)
	}
	if _, err := writer.createAgent(agentSpec{Name: testAgentName, Harness: "claude"}); err == nil {
		t.Error("another Harness must be refused before any call")
	}
}

// jsonValue is v as JSON decodes it — the shape on the wire, whatever Go
// types composed it.
func jsonValue(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
