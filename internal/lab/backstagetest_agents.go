package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// The Agent Platform half of `agentlab backstage-test`: the Dev Portal's
// create and chat paths on kagent API v2, driven headlessly the way a person
// drives them, per user — the wizard's Deploy through agent-manager over
// muster as the person (backstagetest_create.go), the agents list through
// the Kubernetes proxy (backstagetest_roster.go), the Sessions pages through
// the agent-platform backend (backstagetest_chat.go), the detail page's
// edit, skills update and delete (backstagetest_edit.go). Everything the
// proof creates — two agents, their sessions — is removed on every path.

// backstageTestHITLAgent is the second agent the proof brings along: the
// same shape written by the lab directly, with its muster binding requiring
// approval, so the session page's confirmation and Stop paths have a tool
// call that pauses. It is also the second release of the chart E7 keeps the
// shared source for.
const backstageTestHITLAgent = backstageTestAgent + "-hitl"

// The chart value that gates every muster tool call behind a human approval
// (spec.tools[].mcp.requireApproval on the binding), and the first chart
// release that carries it.
const (
	musterRequireApprovalKey = "requireApproval"
	agentChartWithApproval   = "1.1.0"
	agentChartCatchUpTimeout = 2 * time.Minute
	fluxReconcileAnnotation  = "reconcile.fluxcd.io/requestedAt"
)

// hitlFixtureWriter writes an agent whose muster binding requires approval:
// the direct writer's HelmRelease with values.muster.requireApproval set. The
// manifest is the direct writer's, re-read and extended — one apply, one
// Helm revision.
type hitlFixtureWriter struct{}

func (hitlFixtureWriter) String() string {
	return "a HelmRelease of the agent chart applied directly, its muster binding requiring approval (muster.requireApproval)"
}

func (hitlFixtureWriter) createAgent(spec agentSpec) (*agentWritten, error) {
	manifest, err := hitlAgentManifest(spec)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	written := &agentWritten{HelmRelease: true}
	if _, _, _, err := agentChartSource(); apierrors.IsNotFound(err) {
		written.OCIRepository = true
		manifest = agentOCIRepositoryManifest() + "---\n" + manifest
	} else if err != nil {
		return nil, err
	}
	if _, err := applyManifests(ctx, []byte(manifest)); err != nil {
		return nil, err
	}
	return written, nil
}

// hitlAgentManifest is the direct writer's HelmRelease for the spec with
// values.muster.requireApproval, which the chart renders as requireApproval
// on the template's binding of the agent's own RemoteMCPServer.
func hitlAgentManifest(spec agentSpec) (string, error) {
	var release map[string]any
	if err := yaml.Unmarshal([]byte(agentHelmReleaseManifest(spec)), &release); err != nil {
		return "", fmt.Errorf("re-reading the direct writer's HelmRelease: %w", err)
	}
	releaseSpec, _ := release["spec"].(map[string]any)
	values, _ := releaseSpec["values"].(map[string]any)
	if values == nil {
		return "", fmt.Errorf("the direct writer's HelmRelease carries no spec.values:\n%s", agentHelmReleaseManifest(spec))
	}
	muster, _ := values["muster"].(map[string]any)
	if muster == nil {
		muster = map[string]any{}
	}
	muster[musterRequireApprovalKey] = true
	values["muster"] = muster
	out, err := yaml.Marshal(release)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ensureAgentChartCarriesApproval makes sure the namespace's shared chart
// source has fetched a release with muster.requireApproval before any agent
// of this run renders: the OCIRepository tracks 1.x and polls on its
// interval, so a release published since is asked for with Flux's reconcile
// request and waited for, bounded. Nothing of the source's spec changes.
func ensureAgentChartCarriesApproval() error {
	version, exists, err := agentChartArtifactVersion()
	if err != nil {
		return err
	}
	if !exists {
		note("no OCIRepository %s in %s yet: the first create makes it, tracking the newest %s release", agentChartOCIRepository, kagentNamespace, agentChartRange)
		return nil
	}
	if !chartVersionBelow(version, agentChartWithApproval) {
		return nil
	}
	note("OCIRepository %s holds chart %s; muster.%s needs %s — requesting a reconcile", agentChartOCIRepository, version, musterRequireApprovalKey, agentChartWithApproval)
	gvr, err := gvrFor(fluxOCIRepositoryResource)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, fluxReconcileAnnotation, time.Now().UTC().Format(time.RFC3339Nano))
	if err := patchObject(ctx, gvr, kagentNamespace, agentChartOCIRepository, types.MergePatchType, []byte(patch)); err != nil {
		return err
	}
	caught := waitFor(int(agentChartCatchUpTimeout/pollInterval), pollInterval, func() bool {
		version, _, err = agentChartArtifactVersion()
		return err == nil && !chartVersionBelow(version, agentChartWithApproval)
	})
	if err != nil {
		return err
	}
	if !caught {
		return fmt.Errorf("OCIRepository %s still holds chart %s after %s; the HITL fixture needs >= %s (muster.%s)", agentChartOCIRepository, version, agentChartCatchUpTimeout, agentChartWithApproval, musterRequireApprovalKey)
	}
	note("OCIRepository %s now holds chart %s", agentChartOCIRepository, version)
	return nil
}

// agentChartArtifactVersion is the chart version the shared OCIRepository
// last fetched (status.artifact.revision, `<version>@sha256:…`), "" while it
// has none, and whether the source exists at all (the first create makes it).
func agentChartArtifactVersion() (version string, exists bool, err error) {
	obj, err := readKagentFluxObject(fluxOCIRepositoryResource, agentChartOCIRepository)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	revision, _, _ := unstructured.NestedString(obj.Object, "status", "artifact", "revision")
	version, _, _ = strings.Cut(revision, "@")
	return version, true, nil
}

// chartVersionBelow reports whether the fetched version is older than the
// floor; an empty or unparsable version counts as below (the source has not
// fetched anything the proof can rely on).
func chartVersionBelow(version, floor string) bool {
	v, err := semver.NewVersion(version)
	if err != nil {
		return true
	}
	f, err := semver.NewVersion(floor)
	if err != nil {
		return true
	}
	return v.LessThan(f)
}

// proveAgentPlatform is the whole Agent Platform proof over the signed-in
// sessions: the create path as the first platform-admin (the wizard needs
// one), the roster as every user, the chat as the admin with every other
// user as the boundary, HITL and Stop on the fixture, the edit path, and
// both agents deleted through agent-manager. Every verdict line is printed
// at the end.
func proveAgentPlatform(cfg *config.Config, sessions []*portalSession) error {
	primary := adminSession(sessions)
	if primary == nil {
		fmt.Printf("agent platform pages skipped: the portal's create path runs as a %s user and none is among %s\n", platformAdminsGroup, sessionEmails(sessions))
		return nil
	}
	viewer := sessionInGroup(sessions, viewerGroup)
	if viewer == nil {
		note("no %s-group user among the sessions: the forbidden-write assertions are skipped", viewerGroup)
	}

	// Leftovers of an aborted run first, and everything this run creates on
	// every exit path.
	cleanup := func() {
		for _, name := range []string{backstageTestAgent, backstageTestHITLAgent, backstageTestAgent + "-viewer"} {
			if !agentExists(name) {
				continue
			}
			note("cleanup: removing %s", name)
			if err := removeAgent(name); err != nil {
				note("cleanup: %v", err)
			}
		}
	}
	cleanup()
	defer cleanup()
	if err := ensureAgentChartCarriesApproval(); err != nil {
		return err
	}

	var verdicts []string
	spec, _, info, created, err := proveCreatePath(primary, viewer)
	verdicts = append(verdicts, created...)
	if err != nil {
		return err
	}

	rostered, err := proveRoster(sessions, spec)
	verdicts = append(verdicts, rostered...)
	if err != nil {
		return err
	}

	agent := portalAgentRef{Namespace: kagentNamespace, Name: spec.Name}
	chatted, err := proveChat(primary, otherSessions(sessions, primary), agent)
	verdicts = append(verdicts, chatted...)
	if err != nil {
		return err
	}

	hitlSpec := agentSpec{
		Name: backstageTestHITLAgent, ModelConfig: spec.ModelConfig, DisplayName: backstageTestDisplayName + " (hitl)", Toolset: spec.Toolset,
		Description:   "Throwaway agent of `agentlab backstage-test` whose muster binding requires approval; deleted by the same run.",
		SystemMessage: skillsTestSystemPrompt,
	}
	writer := hitlFixtureWriter{}
	step("The HITL fixture %s: %s", hitlSpec.Name, writer)
	hitlTemplate, _, err := readyAgent(writer, hitlSpec, agentReadyTimeout)
	if err != nil {
		return err
	}
	if err := assertAgentRender(hitlTemplate, hitlSpec, firstNonEmpty(info.Muster.URL, defaultMusterMCPURL)); err != nil {
		return err
	}
	if approval, err := templateRequiresApproval(hitlSpec.Name, hitlSpec.Name); err != nil {
		return err
	} else if !approval {
		return fmt.Errorf("AgentTemplate %s binds RemoteMCPServer %s without requireApproval — the escape hatch did not reach the binding", hitlSpec.Name, hitlSpec.Name)
	}
	note("Ready on Harness %s; the binding of RemoteMCPServer %s requires approval", kagentHarness, hitlSpec.Name)
	interrupted, err := proveHITLAndStop(primary, portalAgentRef{Namespace: kagentNamespace, Name: hitlSpec.Name})
	verdicts = append(verdicts, interrupted...)
	if err != nil {
		return err
	}

	edited, err := proveEditPath(primary, viewer, spec, hitlSpec.Name)
	verdicts = append(verdicts, edited...)
	if err != nil {
		return err
	}

	fmt.Println()
	for _, v := range verdicts {
		fmt.Println(v)
	}
	return nil
}

// templateRequiresApproval reports whether the AgentTemplate's binding of the
// named MCP server carries spec.tools[].mcp.requireApproval, read off the
// object as the apiserver holds it.
func templateRequiresApproval(template, server string) (bool, error) {
	obj, err := readKagentObject(agentTemplateResource, template)
	if err != nil {
		return false, err
	}
	tools, _, _ := unstructured.NestedSlice(obj.Object, "spec", "tools")
	for _, tool := range tools {
		m, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if bound, _, _ := unstructured.NestedString(m, "mcp", "server", nameKey); bound != server {
			continue
		}
		approval, _, _ := unstructured.NestedBool(m, "mcp", "requireApproval")
		return approval, nil
	}
	return false, nil
}

// sessionEmails lists the sessions' users.
func sessionEmails(sessions []*portalSession) string {
	emails := make([]string, 0, len(sessions))
	for _, ps := range sessions {
		emails = append(emails, ps.user.Email)
	}
	return strings.Join(emails, ", ")
}

// collectStrings walks a decoded JSON tree in document order and returns
// every string value stored under key.
func collectStrings(node any, key string) []string {
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if s, ok := v[key].(string); ok {
				out = append(out, s)
			}
			for _, k := range mapKeys(v) {
				walk(v[k])
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(node)
	return out
}

// mapKeys is the sorted keys of a decoded JSON object.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// stringAt is m[key] when it is a string, "" otherwise.
func stringAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
