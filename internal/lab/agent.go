package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

// An agent on the platform — the Generic agent chart 1.x contract as the
// proofs read and write it.
//
// One agent is one Flux HelmRelease of the `agent` chart in the kagent
// namespace, rendering from the namespace's shared OCIRepository `agent`
// that tracks the chart at 1.x. The platform chart's bundled engine renders
// it as the tenant ServiceAccount kagent-flux into one kagent.dev/v1alpha3
// AgentTemplate named after the release — labelled
// agent-platform.giantswarm.io/harness=<agent.harness> for the platform
// Harness's admission selector, annotated ui.giantswarm.io/display-name and
// ui.giantswarm.io/icon-url — and, unless the toolset is exactly
// [preset:none], one RemoteMCPServer named after the agent that points at
// muster and carries the toolset as the X-Muster-Toolset header; the
// template binds it as its tools. The HelmRelease's `values.toolset` is the
// stable anchor of the toolset; skills are `{name, git: {url, commit}, path}`
// or `{name, oci}` entries pinned at write time. Readiness is the platform
// Harness's entry in the template's status.harnesses[]: Ready once the golden
// snapshot exists.
//
// Two writers put the same two Flux objects there: agent-manager as the
// signed-in person (the platform path — muster's x_agent-manager_create_agent,
// from an MCP session or through the portal's muster backend), and a
// HelmRelease the lab applies itself (the operator's kubectl path). One
// helper (readyAgent) knows both and when the agent is ready; every proof
// uses it. docs/agents.md.

const (
	// agentChartOCIRepository is the shared OCIRepository of the chart in the
	// namespace (kindOCIRepository); agentChartURL and agentChartRange what it
	// tracks.
	kindOCIRepository       = "OCIRepository"
	agentChartOCIRepository = "agent"
	agentChartURL           = "oci://gsoci.azurecr.io/charts/giantswarm/agent"
	agentChartRange         = "1.x"
	// harnessLabel selects the Harness that admits an AgentTemplate
	// (allowedAgentTemplates.selector.matchLabels): the chart renders it from
	// agent.harness, the connectivity chart's Harness matches its own name.
	harnessLabel = "agent-platform.giantswarm.io/harness"
	// displayNameAnnotation and iconURLAnnotation carry the agent's friendly
	// name and avatar on the AgentTemplate (agent.displayName, agent.iconUrl).
	displayNameAnnotation = "ui.giantswarm.io/display-name"
	iconURLAnnotation     = "ui.giantswarm.io/icon-url"
	// discoveryLabel is the chart's opt-out of the controller's tool
	// discovery on the agent's RemoteMCPServer (muster is an OAuth resource
	// server; the agent resolves its tools at run time as the person).
	discoveryLabel         = "kagent.dev/discovery"
	discoveryDisabledValue = "disabled"
	// fluxHelmReleaseNameLabel is the provenance label helm-controller stamps
	// on every object a release renders.
	fluxHelmReleaseNameLabel = "helm.toolkit.fluxcd.io/name"
	// kagentFluxServiceAccount is the tenant identity every agent HelmRelease
	// runs as: the connectivity chart renders the ServiceAccount and its
	// RoleBinding from kagent.fluxServiceAccountName, and names it into
	// agent-manager's flux.helmReleaseServiceAccount. The engine is
	// multitenant: a HelmRelease that names no ServiceAccount runs as the
	// namespace's `default`, which holds no RBAC, and never renders.
	kagentFluxServiceAccount = "kagent-flux"
	// agentManagerFieldManager is the field manager agent-manager writes the
	// HelmRelease and the OCIRepository with (agents.FieldManager).
	agentManagerFieldManager = "agent-manager"
	// agentManagerToolPrefix prefixes agent-manager's tools as muster
	// aggregates them.
	agentManagerToolPrefix = "x_" + agentManagerMCPServer + "_"
	// The Flux API versions the direct writer's manifests name (the reads
	// resolve through discovery, gvrFor).
	fluxHelmReleaseAPIVersion   = "helm.toolkit.fluxcd.io/v2"
	fluxOCIRepositoryAPIVersion = "source.toolkit.fluxcd.io/v1"
	// helmReleaseInterval and ociRepositoryInterval are the Flux intervals
	// the direct writer composes — agent-manager's defaults.
	helmReleaseInterval   = "10m"
	ociRepositoryInterval = "30m"
	// agentReadyTimeout bounds an agent's readiness by default: the release
	// reconciled, the template compiled, the golden snapshot taken (≈ 10 s
	// with the runtime image cached, minutes cold or with a skill to fetch).
	agentReadyTimeout = 4 * time.Minute
	// agentGoneTimeout bounds a delete: helm-controller's uninstall and the
	// controller letting go of the template.
	agentGoneTimeout = time.Minute
)

// The agent-manager tool arguments and report fields the proofs share, next
// to the ones in proofs.go.
const (
	iconURLKey       = "iconUrl"
	skillsKey        = "skills"
	refreshSkillsKey = "refreshSkills"
	// verdictFailed is get_agent_status's verdict for an agent the platform
	// will not get to run on its own.
	verdictFailed = "failed"
)

// The condition types kagent reports per Harness on an AgentTemplate (next
// to conditionReady and conditionResolvedRefs), the Ready reason it writes
// while the golden snapshot is pending, and the HelmRelease reasons
// helm-controller gives up with.
const (
	conditionAccepted   = "Accepted"
	conditionCompatible = "Compatible"
	readyReasonPending  = "ActorTemplatePending"
	helmInstallFailed   = "InstallFailed"
	helmUpgradeFailed   = "UpgradeFailed"
)

// agentSpec is what every proof's agent is made of — the create_agent
// arguments and the chart values alike.
type agentSpec struct {
	// Name is the DNS-1123 technical name: the HelmRelease, the AgentTemplate
	// and the RemoteMCPServer.
	Name string
	// ModelConfig names an existing kagent ModelConfig of the namespace.
	ModelConfig string
	// DisplayName, Description, SystemMessage and IconURL are the agent's
	// own; empty keeps the chart's default.
	DisplayName, Description, SystemMessage, IconURL string
	// Toolset is the selector list muster resolves per request. nil declares
	// none: implicit full access on the release (the direct writer only —
	// agent-manager refuses a create without one).
	Toolset []string
	// Skills the agent mounts, pinned (agent-manager pins a ref for the
	// platform path; the direct writer takes a commit).
	Skills []agentSkill
	// Harness is the value of the admission label (agent.harness); "" is the
	// platform Harness. Another name places the template on no Harness — the
	// direct writer's way to a template nothing admits.
	Harness string
}

// agentSkill is one skills[] entry: a name, a directory and exactly one
// source, git or OCI. The JSON shape is agent-manager's argument and, minus
// Ref, the chart value.
type agentSkill struct {
	Name string    `json:"name,omitempty"`
	Path string    `json:"path,omitempty"`
	Git  *gitSkill `json:"git,omitempty"`
	OCI  string    `json:"oci,omitempty"`
}

// gitSkill is a skill in a git repository: the repository and a Ref
// agent-manager resolves to its head commit, or the Commit the pin already is
// (the chart takes commits only).
type gitSkill struct {
	URL    string `json:"url"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// harnessValue is the admission label value the spec renders.
func (s agentSpec) harnessValue() string {
	if s.Harness == "" {
		return kagentHarness
	}
	return s.Harness
}

// agentWritten is what a writer reports back: who the write ran as and
// which of the two Flux objects it created — the OCIRepository is shared per
// namespace and reused when it exists.
type agentWritten struct {
	RequestedBy   string
	HelmRelease   bool
	OCIRepository bool
}

// agentWriter is one way an agent's HelmRelease reaches the cluster.
type agentWriter interface {
	// createAgent writes the agent's HelmRelease, and the namespace's shared
	// OCIRepository of the chart when it is missing.
	createAgent(spec agentSpec) (*agentWritten, error)
	// String names the path for the steps.
	String() string
}

// agentManagerWriter is the platform path: agent-manager's create_agent as
// the person whose muster session this is.
type agentManagerWriter struct {
	session *musterSession
}

func (w agentManagerWriter) String() string {
	return "agent-manager (" + agentManagerToolPrefix + "create_agent through muster)"
}

func (w agentManagerWriter) createAgent(spec agentSpec) (*agentWritten, error) {
	args, err := createAgentArgs(spec)
	if err != nil {
		return nil, err
	}
	var created createAgentReport
	if err := w.session.callServerJSON(agentManagerToolPrefix+"create_agent", args, &created); err != nil {
		return nil, err
	}
	return created.written(), nil
}

// portalAgentManagerWriter is the portal's path to the same tool: the muster
// plugin's backend calls x_agent-manager_create_agent with the portal
// session's own forwarded Dex id_token (POST /api/muster/call), the way the
// Dev Portal's create flow does.
type portalAgentManagerWriter struct {
	ps *portalSession
}

func (w portalAgentManagerWriter) String() string {
	return "the portal (POST /api/muster/call " + agentManagerToolPrefix + "create_agent as " + w.ps.user.Email + ")"
}

func (w portalAgentManagerWriter) createAgent(spec agentSpec) (*agentWritten, error) {
	args, err := createAgentArgs(spec)
	if err != nil {
		return nil, err
	}
	status, raw, err := w.ps.musterPost("/call"+installationQuery, map[string]any{nameKey: agentManagerToolPrefix + "create_agent", argumentsKey: args})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("POST /api/muster/call %screate_agent answered %d: %.300s", agentManagerToolPrefix, status, raw)
	}
	var envelope toolEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("POST /api/muster/call: not a tool result: %w\n%.300s", err, raw)
	}
	text := ""
	if len(envelope.Content) > 0 {
		text = envelope.Content[0].Text
	}
	if envelope.IsError {
		return nil, fmt.Errorf("%screate_agent through the portal: %s", agentManagerToolPrefix, excerpt(text, 300))
	}
	var created createAgentReport
	if err := json.Unmarshal([]byte(text), &created); err != nil {
		return nil, fmt.Errorf("%screate_agent through the portal: payload is not the expected JSON: %w\n%.300s", agentManagerToolPrefix, err, text)
	}
	return created.written(), nil
}

// createAgentReport is create_agent's result as the proofs read it.
type createAgentReport struct {
	RequestedBy string `json:"requestedBy"`
	Created     struct {
		HelmRelease   bool `json:"helmRelease"`
		OCIRepository bool `json:"ociRepository"`
	} `json:"created"`
}

func (r createAgentReport) written() *agentWritten {
	return &agentWritten{RequestedBy: r.RequestedBy, HelmRelease: r.Created.HelmRelease, OCIRepository: r.Created.OCIRepository}
}

// createAgentArgs composes create_agent's arguments from a spec: only what
// is set, so the chart's defaults apply to the rest. agent-manager places
// every agent on the platform Harness and requires a toolset.
func createAgentArgs(spec agentSpec) (map[string]any, error) {
	if spec.Harness != "" && spec.Harness != kagentHarness {
		return nil, fmt.Errorf("agent %s: agent-manager places every agent on the platform Harness %s; a template for Harness %q is the direct HelmRelease writer's", spec.Name, kagentHarness, spec.Harness)
	}
	args := map[string]any{nameKey: spec.Name, modelConfigKey: spec.ModelConfig}
	for key, value := range map[string]string{
		displayNameKey: spec.DisplayName, descriptionKey: spec.Description, systemMessageKey: spec.SystemMessage, iconURLKey: spec.IconURL,
	} {
		if value != "" {
			args[key] = value
		}
	}
	if spec.Toolset != nil {
		args[toolsetKey] = spec.Toolset
	}
	if len(spec.Skills) > 0 {
		args[skillsKey] = spec.Skills
	}
	return args, nil
}

// helmReleaseWriter is the operator's path: the OCIRepository of the chart
// (when the namespace has none yet) and the agent's HelmRelease, applied by
// the lab as the same two objects agent-manager composes.
type helmReleaseWriter struct{}

func (helmReleaseWriter) String() string { return "a HelmRelease of the agent chart applied directly" }

func (helmReleaseWriter) createAgent(spec agentSpec) (*agentWritten, error) {
	for _, skill := range spec.Skills {
		if skill.Git != nil && skill.Git.Commit == "" {
			return nil, fmt.Errorf("agent %s: skill %s names no commit; the chart takes pinned skills only (agent-manager resolves a ref)", spec.Name, skill.Name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	written := &agentWritten{HelmRelease: true}
	ociGVR, err := gvrFor(fluxOCIRepositoryResource)
	if err != nil {
		return nil, err
	}
	exists, err := objectExists(ctx, ociGVR, kagentNamespace, agentChartOCIRepository)
	if err != nil {
		return nil, err
	}
	manifests := agentHelmReleaseManifest(spec)
	if !exists {
		written.OCIRepository = true
		manifests = agentOCIRepositoryManifest() + "---\n" + manifests
	}
	if _, err := applyManifests(ctx, []byte(manifests)); err != nil {
		return nil, err
	}
	return written, nil
}

// agentOCIRepositoryManifest is the namespace's shared chart source, as
// agent-manager composes it.
func agentOCIRepositoryManifest() string {
	return fmt.Sprintf(`apiVersion: %s
kind: OCIRepository
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  interval: %s
  url: %s
  ref:
    semver: %q
`, fluxOCIRepositoryAPIVersion, agentChartOCIRepository, kagentNamespace, managedByLabel, managedByAgentlabValue, ociRepositoryInterval, agentChartURL, agentChartRange)
}

// agentHelmReleaseManifest is the agent's HelmRelease: the chart by its
// OCIRepository, the tenant ServiceAccount, and the values the spec sets.
func agentHelmReleaseManifest(spec agentSpec) string {
	values, _ := json.Marshal(agentValues(spec))
	return fmt.Sprintf(`apiVersion: %s
kind: HelmRelease
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  interval: %s
  serviceAccountName: %s
  chartRef:
    kind: OCIRepository
    name: %s
    namespace: %s
  values: %s
`, fluxHelmReleaseAPIVersion, spec.Name, kagentNamespace, managedByLabel, managedByAgentlabValue, helmReleaseInterval, kagentFluxServiceAccount, agentChartOCIRepository, kagentNamespace, values)
}

// agentValues is the chart 1.x values of a spec: only what is set, plus the
// Harness the template is placed on.
func agentValues(spec agentSpec) map[string]any {
	agent := map[string]any{nameKey: spec.Name, "harness": spec.harnessValue()}
	for key, value := range map[string]string{
		displayNameKey: spec.DisplayName, descriptionKey: spec.Description, systemMessageKey: spec.SystemMessage, iconURLKey: spec.IconURL,
	} {
		if value != "" {
			agent[key] = value
		}
	}
	values := map[string]any{"agent": agent, modelConfigKey: map[string]any{nameKey: spec.ModelConfig}}
	if spec.Toolset != nil {
		values[toolsetKey] = spec.Toolset
	}
	if len(spec.Skills) > 0 {
		skills := make([]agentSkill, len(spec.Skills))
		for i, skill := range spec.Skills {
			skills[i] = skill
			if skill.Git != nil {
				pinned := *skill.Git
				pinned.Ref = ""
				skills[i].Git = &pinned
			}
		}
		values[skillsKey] = skills
	}
	return values
}

// agentRelease is the part of an agent's HelmRelease the proofs read.
type agentRelease struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ServiceAccountName string `json:"serviceAccountName"`
		ChartRef           struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"chartRef"`
		Values map[string]any `json:"values"`
	} `json:"spec"`
	Status struct {
		Conditions            []templateCondition `json:"conditions"`
		LastAttemptedRevision string              `json:"lastAttemptedRevision"`
		History               []struct {
			ChartVersion string `json:"chartVersion"`
			Status       string `json:"status"`
		} `json:"history"`
	} `json:"status"`
	// managers are the field managers on the release — who wrote it.
	managers []string
}

// readAgentRelease reads an agent's HelmRelease; a missing one is the
// apiserver's NotFound.
func readAgentRelease(name string) (*agentRelease, error) {
	obj, err := readKagentFluxObject(fluxHelmReleaseResource, name)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	var r agentRelease
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing HelmRelease %s: %w", name, err)
	}
	for _, entry := range obj.GetManagedFields() {
		r.managers = append(r.managers, entry.Manager)
	}
	return &r, nil
}

// readKagentFluxObject reads one Flux object of the kagent namespace by
// resource argument.
func readKagentFluxObject(resourceArg, name string) (*unstructured.Unstructured, error) {
	gvr, err := gvrFor(resourceArg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	return getObject(ctx, gvr, kagentNamespace, name)
}

// toolset is the release's `values.toolset` — the stable anchor of the
// agent's toolset — and whether the release declares one at all.
func (r *agentRelease) toolset() ([]string, bool, error) {
	raw, found := r.Spec.Values[toolsetKey]
	if !found || raw == nil {
		return nil, false, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, true, fmt.Errorf("HelmRelease %s values.toolset is not a string list: %v", r.Metadata.Name, raw)
	}
	toolset := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, true, fmt.Errorf("HelmRelease %s values.toolset is not a string list: %v", r.Metadata.Name, raw)
		}
		toolset = append(toolset, s)
	}
	return toolset, true, nil
}

// value is one string leaf of the release's values by path, "" when unset.
func (r *agentRelease) value(path ...string) string {
	s, _, _ := unstructured.NestedString(r.Spec.Values, path...)
	return s
}

// skillCommits are the release's git skills by name, each with its pinned
// commit.
func (r *agentRelease) skillCommits() map[string]string { return skillCommits(r.Spec.Values) }

// skillCommits reads a chart values map's git skills by name, each with its
// pinned commit.
func skillCommits(values map[string]any) map[string]string {
	commits := map[string]string{}
	skills, _, _ := unstructured.NestedSlice(values, skillsKey)
	for _, s := range skills {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, nameKey)
		commit, _, _ := unstructured.NestedString(m, "git", "commit")
		commits[name] = commit
	}
	return commits
}

// chartVersion is the chart version the release last deployed (the newest
// history entry), else the revision it last attempted.
func (r *agentRelease) chartVersion() string {
	if len(r.Status.History) > 0 && r.Status.History[0].ChartVersion != "" {
		return r.Status.History[0].ChartVersion
	}
	return r.Status.LastAttemptedRevision
}

// ready is the release's Ready condition, "" while unreported.
func (r *agentRelease) ready() (status string, reason string, message string) {
	for _, c := range r.Status.Conditions {
		if c.Type == conditionReady {
			return c.Status, c.Reason, c.Message
		}
	}
	return "", "", ""
}

// agentChartSource is what the proofs read off the namespace's shared
// OCIRepository of the chart: its URL and the semver range it tracks.
func agentChartSource() (url, semver string, managers []string, err error) {
	obj, err := readKagentFluxObject(fluxOCIRepositoryResource, agentChartOCIRepository)
	if err != nil {
		return "", "", nil, err
	}
	url, _, _ = unstructured.NestedString(obj.Object, "spec", "url")
	semver, _, _ = unstructured.NestedString(obj.Object, "spec", "ref", "semver")
	for _, entry := range obj.GetManagedFields() {
		managers = append(managers, entry.Manager)
	}
	return url, semver, managers, nil
}

// agentReadiness is how waiting on an agent ended: Ready on the platform
// Harness, a failure the platform will not get past on its own (terminal:
// the release's render refused, a Harness condition False for good, no
// Harness admitting the template), or the timeout — with the last reads and
// the reason, worded, and the time it took.
type agentReadiness struct {
	template *agentTemplate
	release  *agentRelease
	ready    bool
	terminal bool
	reason   string
	elapsed  time.Duration
}

// waitAgentReady is the one wait every proof shares: it polls an agent's
// HelmRelease (when it has one) and its AgentTemplate until the platform
// Harness reports Ready on the desired revision — the golden snapshot — or
// until a terminal failure, or until the timeout. A read that keeps failing
// (not NotFound) is the error; a template that is not there yet is waited
// for.
func waitAgentReady(name string, timeout time.Duration) (agentReadiness, error) {
	started := time.Now()
	var r agentReadiness
	var lastErr error
	waitFor(int(timeout/pollInterval), pollInterval, func() bool {
		r, lastErr = readAgentReadiness(name)
		return lastErr != nil || r.ready || r.terminal
	})
	r.elapsed = time.Since(started)
	if lastErr != nil {
		return r, fmt.Errorf("agent %s: %w", name, lastErr)
	}
	return r, nil
}

// readAgentReadiness is one reading of an agent's state (waitAgentReady's
// probe): the release first, then the template and its Harness entry.
func readAgentReadiness(name string) (agentReadiness, error) {
	var r agentReadiness
	release, err := readAgentRelease(name)
	switch {
	case err == nil:
		r.release = release
		if status, reason, message := release.ready(); status == condFalseStatus && (reason == helmInstallFailed || reason == helmUpgradeFailed) {
			r.terminal, r.reason = true, fmt.Sprintf("HelmRelease %s: Ready=False %s: %s", name, reason, message)
			return r, nil
		} else if status != conditionTrue {
			r.reason = fmt.Sprintf("HelmRelease %s: Ready=%q %s %s", name, status, reason, excerpt(message, 160))
		}
	case !apierrors.IsNotFound(err):
		return r, err
	}
	template, err := readAgentTemplate(name)
	switch {
	case apierrors.IsNotFound(err):
		if r.release == nil {
			r.reason = "neither a HelmRelease nor an AgentTemplate " + name + " yet"
		} else if r.reason == "" {
			r.reason = fmt.Sprintf("HelmRelease %s is Ready but the AgentTemplate is not rendered yet", name)
		}
		return r, nil
	case err != nil:
		return r, err
	}
	r.template = template
	h := template.harness(kagentHarness)
	if h == nil {
		switch {
		case len(template.Status.Harnesses) > 0:
			r.terminal, r.reason = true, fmt.Sprintf("AgentTemplate %s is admitted by %s only, not by the platform Harness %s", name, strings.Join(template.harnessNames(), ", "), kagentHarness)
		case template.Status.ObservedGeneration >= template.Metadata.Generation && template.Status.ObservedGeneration > 0:
			r.terminal, r.reason = true, fmt.Sprintf("no Harness admits AgentTemplate %s (labels %v; the platform Harness %s admits %s=%s)", name, template.Metadata.Labels, kagentHarness, harnessLabel, kagentHarness)
		default:
			r.reason = fmt.Sprintf("kagent has not reported on AgentTemplate %s yet", name)
		}
		return r, nil
	}
	if text, terminal := terminalHarnessFailure(h); terminal {
		r.terminal, r.reason = true, fmt.Sprintf("AgentTemplate %s on Harness %s: %s", name, kagentHarness, text)
		return r, nil
	}
	if status, _ := h.condition(conditionReady); status == conditionTrue && h.DesiredRevision == h.LatestSuccessfulRevision {
		r.ready = true
		return r, nil
	}
	status, message := h.condition(conditionReady)
	r.reason = fmt.Sprintf("AgentTemplate %s on Harness %s: Ready=%q %s (revision %.12s compiling%s)", name, kagentHarness, status, excerpt(message, 160), h.DesiredRevision, warningsNote(h.Warnings))
	return r, nil
}

// terminalHarnessFailure reads a Harness's conditions for a failure the
// controller will not retry: ResolvedRefs or Compatible False, or Ready False
// for a reason other than the golden snapshot still pending
// (ActorTemplatePending). ActorTemplateFailed carries Substrate's own error
// for the boot, ActorTemplateConflict an immutable-template clash. The text
// is the condition as the evidence quotes it.
func terminalHarnessFailure(h *harnessStatus) (string, bool) {
	for _, c := range h.Conditions {
		if c.Status != condFalseStatus {
			continue
		}
		switch c.Type {
		case conditionResolvedRefs, conditionCompatible:
			return c.String(), true
		case conditionReady:
			if c.Reason != readyReasonPending {
				return c.String(), true
			}
		}
	}
	return "", false
}

// readyAgent is the one helper behind every proof's agent: the spec written
// by the writer, then waited for until it is Ready on the platform Harness.
// The caller removes the agent (removeAgent) on every path. The error of a
// failed or timed-out boot carries the reason and where to look.
func readyAgent(w agentWriter, spec agentSpec, timeout time.Duration) (*agentTemplate, *agentWritten, error) {
	written, err := w.createAgent(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("creating agent %s through %s: %w", spec.Name, w, err)
	}
	r, err := waitAgentReady(spec.Name, timeout)
	if err != nil {
		return nil, written, err
	}
	if !r.ready {
		return nil, written, r.failure(spec.Name, timeout)
	}
	return r.template, written, nil
}

// failure words a readiness that did not end Ready: what was seen last and
// how to look further.
func (r agentReadiness) failure(name string, timeout time.Duration) error {
	verdict := fmt.Sprintf("never became Ready on Harness %s within %s", kagentHarness, timeout)
	if r.terminal {
		verdict = fmt.Sprintf("failed for good after %s", r.elapsed.Round(time.Second))
	}
	return fmt.Errorf("agent %s %s: %s;\ncheck `kubectl -n %s get %s,%s,%s %s -o yaml`", name, verdict, r.reason, kagentNamespace, fluxHelmReleaseResource, agentTemplateResource, remoteMCPServerResource, name)
}

// agentResources are the three resources an agent is on the cluster: its
// HelmRelease and the two objects the release renders.
var agentResources = []string{fluxHelmReleaseResource, agentTemplateResource, remoteMCPServerResource}

// removeAgent deletes everything an agent is on the cluster and waits for
// it to be gone, bounded: the HelmRelease (helm-controller uninstalls the
// render), then whatever is left directly — a bare AgentTemplate and the
// RemoteMCPServer of the agent's name — ignoring what is not there. For the
// cleanup paths and the leftovers of aborted runs; the error says what stays.
func removeAgent(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentGoneTimeout+kubeReadTimeout)
	defer cancel()
	for i, resource := range agentResources {
		gvr, err := gvrFor(resource)
		if err != nil {
			return err
		}
		// The release first, waited for: its uninstall takes the render along.
		waitGone := time.Duration(0)
		if i == 0 {
			waitGone = agentGoneTimeout
		}
		if err := deleteObject(ctx, gvr, kagentNamespace, name, waitGone); err != nil {
			return err
		}
	}
	return waitAgentRemoved(name)
}

// waitAgentRemoved waits, bounded, for nothing of the agent to be left on the
// cluster: no HelmRelease, no AgentTemplate, no RemoteMCPServer of its name.
func waitAgentRemoved(name string) error {
	gvrs := make([]schema.GroupVersionResource, 0, len(agentResources))
	for _, resource := range agentResources {
		gvr, err := gvrFor(resource)
		if err != nil {
			return err
		}
		gvrs = append(gvrs, gvr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentGoneTimeout+kubeReadTimeout)
	defer cancel()
	var left []string
	gone := waitFor(int(agentGoneTimeout/pollInterval), pollInterval, func() bool {
		left = left[:0]
		for i, gvr := range gvrs {
			if exists, err := objectExists(ctx, gvr, kagentNamespace, name); err != nil || exists {
				left = append(left, agentResources[i])
			}
		}
		return len(left) == 0
	})
	if !gone {
		return fmt.Errorf("agent %s: %s still there %s after the delete", name, strings.Join(left, " and "), agentGoneTimeout)
	}
	return nil
}

// agentExists reports whether anything of the agent is on the cluster: its
// HelmRelease, its AgentTemplate or its RemoteMCPServer.
func agentExists(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	for _, resource := range agentResources {
		gvr, err := gvrFor(resource)
		if err != nil {
			continue
		}
		if exists, _ := objectExists(ctx, gvr, kagentNamespace, name); exists {
			return true
		}
	}
	return false
}

// firstTurnAs is one turn on an agent that has just become Ready, as the
// person whose token is given: the first resume of a cold worker can run
// into Substrate's un-retried ResumeActor deadline and wedge that instance,
// so a failed first turn is said and tried once more — never more, and never
// silently.
func firstTurnAs(cfg *config.Config, name, token, prompt string) (string, error) {
	reply, err := agentTurnAs(cfg, name, token, prompt)
	if err == nil {
		return reply, nil
	}
	note("the first turn on %s failed (%s) — one retry, a cold worker's ResumeActor deadline is not retried by Substrate", name, excerpt(err.Error(), 200))
	return agentTurnAs(cfg, name, token, prompt)
}

// toolsetHeaderOf is the X-Muster-Toolset the agent's runtime sends, read
// off the RemoteMCPServer the template binds: the header's value and true,
// or "" and false for a server without the header (a release that declares
// no toolset: implicit full access). An error when the template binds no
// MCP server at all (the chat-only shape, preset:none) or the bound server
// cannot be read.
func toolsetHeaderOf(t *agentTemplate) (string, bool, error) {
	server := t.mcpServer()
	if server == "" {
		return "", false, fmt.Errorf("no MCP server binding (spec.tools)")
	}
	rms, err := readKagentObject(remoteMCPServerResource, server)
	if err != nil {
		return "", false, fmt.Errorf("the bound RemoteMCPServer %s: %w", server, err)
	}
	return remoteMCPServerHeader(rms, toolsetHeader)
}

// remoteMCPServerHeader is the value of one spec.headersFrom entry of a
// RemoteMCPServer — its literal value, or the Secret key it names, decoded —
// and whether the entry is there.
func remoteMCPServerHeader(rms *unstructured.Unstructured, name string) (string, bool, error) {
	headers, _, _ := unstructured.NestedSlice(rms.Object, "spec", "headersFrom")
	for _, h := range headers {
		m, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if got, _, _ := unstructured.NestedString(m, nameKey); got != name {
			continue
		}
		if value, found, _ := unstructured.NestedString(m, "value"); found {
			return value, true, nil
		}
		kind, _, _ := unstructured.NestedString(m, "valueFrom", fieldTypeKey)
		secret, _, _ := unstructured.NestedString(m, "valueFrom", nameKey)
		key, _, _ := unstructured.NestedString(m, "valueFrom", "key")
		if kind != kindSecret {
			return "", true, fmt.Errorf("RemoteMCPServer %s takes %s from a %s (%s/%s), not a value the proof can read", rms.GetName(), name, kind, secret, key)
		}
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		value, err := secretDataKey(ctx, rms.GetNamespace(), secret, key)
		if err != nil {
			return "", true, fmt.Errorf("RemoteMCPServer %s takes %s from Secret %s/%s: %w", rms.GetName(), name, secret, key, err)
		}
		return string(value), true, nil
	}
	return "", false, nil
}

// headerNames lists the names of a RemoteMCPServer's headersFrom, sorted.
func headerNames(rms *unstructured.Unstructured) []string {
	headers, _, _ := unstructured.NestedSlice(rms.Object, "spec", "headersFrom")
	var names []string
	for _, h := range headers {
		if m, ok := h.(map[string]any); ok {
			if n, _, _ := unstructured.NestedString(m, nameKey); n != "" {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}

// fieldTypeKey is the `type` key of a headersFrom valueFrom (Secret | ConfigMap).
const fieldTypeKey = "type"
