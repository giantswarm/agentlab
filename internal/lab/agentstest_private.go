package lab

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// agentsTestPrivate is the agent of agents-test's private-skill case: a
// HelmRelease of the agent chart whose skill comes from the lab's own
// private git host (skillhost.go), fetched with the chart's
// skillsGitAuthSecretRef.
const agentsTestPrivate = agentsTestAgent + "-private"

// AgentsTestOptions tunes agents-test.
type AgentsTestOptions struct {
	// SkillHostBinary is the static Linux agentlab the private-skill case's
	// skill host fixture runs in its container (default: this binary).
	SkillHostBinary string
}

// proveAgentsTestPrivateSkill is agents-test's private-skill case: the
// skill host fixture in place (a private git host answering 401 to any
// request without the credential of a Secret the case creates), a
// HelmRelease of the agent chart applied directly whose skill comes from it
// with skillsGitAuthSecretRef naming that Secret. The render's skill carries
// credentialRef {name, key: token}; the golden boots (the Harness serves
// readyz, the Agent is Ready); and the fixture recorded the golden boot's
// clone carrying the Secret value — Substrate's egress gateway replaced the
// materializer's placeholder Authorization. A line whose materializer sends
// no Authorization (Substrate 1.5's egress replaces a header, never adds
// one) fails the golden boot with git's "could not read Username", and the
// case fails naming the fixture's record. agent-manager is not the writer:
// it resolves github.com skills only, against the caller's GitHub login.
// Returns the PASS line; leaves nothing behind.
func proveAgentsTestPrivateSkill(cfg *config.Config, modelConfig string, opts AgentsTestOptions) (string, error) {
	binary := opts.SkillHostBinary
	if binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locating this binary for the skill host fixture's container: %w (pass --skill-host-binary)", err)
		}
		binary = exe
	}
	image, err := harnessRuntimeImage()
	if err != nil {
		return "", err
	}
	if err := requireCredentialRefServed(); err != nil {
		return "", err
	}

	step("A golden whose skill comes from a private repository: the skill host fixture as %s, its credential the Secret %s/%s, in the Harness's runtime image %s", skillHostURL(cfg), kagentNamespace, skillHostSecret, image)
	host, err := startSkillHost(cfg, binary, image)
	if err != nil {
		return "", err
	}
	defer host.close()
	note("serving %s @ %.12s: 401 to any request without the Secret's credential, each request recorded with the kind of its Authorization (never a value)", host.url, host.commit)

	cleanup := func() {
		if !agentExists(agentsTestPrivate) {
			return
		}
		note("cleanup: removing %s", agentsTestPrivate)
		if err := removeAgent(agentsTestPrivate); err != nil {
			note("cleanup: %v", err)
		}
	}
	cleanup()
	defer cleanup()

	skillName := skillHostSkillPath[strings.LastIndex(skillHostSkillPath, "/")+1:]
	spec := agentSpec{
		Name: agentsTestPrivate, ModelConfig: modelConfig, Toolset: []string{presetNone},
		Description:   "Throwaway agent of `agentlab agents-test` with a skill from a private repository; deleted by the same run.",
		Skills:        []agentSkill{{Name: skillName, Path: skillHostSkillPath, Git: &gitSkill{URL: host.url, Commit: host.commit}}},
		GitAuthSecret: skillHostSecret,
	}
	step("HelmRelease %s applied directly: skill %s from %s @ %.12s, %s.name %s", agentsTestPrivate, skillName, host.url, host.commit, skillsGitAuthSecretRefKey, skillHostSecret)
	if _, err := (helmReleaseWriter{}).createAgent(spec); err != nil {
		return "", err
	}
	template, _, err := waitAgentRender(agentsTestPrivate)
	if err != nil {
		return "", err
	}
	skill := template.skill(skillName)
	if skill == nil || skill.Source.Git == nil || skill.Source.Git.URL != host.url || skill.Source.Git.Commit != host.commit {
		return "", fmt.Errorf("AgentTemplate %s carries skill %s as %+v, wanted {git: {%s, %s}}", agentsTestPrivate, skillName, skill, host.url, host.commit)
	}
	if ref := skill.Source.Git.CredentialRef; ref == nil || ref.Name != skillHostSecret || ref.Key != skillsCredentialKey {
		return "", fmt.Errorf("AgentTemplate %s's skill %s carries credentialRef %+v, wanted {name: %s, key: %s} from %s", agentsTestPrivate, skillName, ref, skillHostSecret, skillsCredentialKey, skillsGitAuthSecretRefKey)
	}
	note("the render's skill carries credentialRef {name: %s, key: %s}", skillHostSecret, skillsCredentialKey)

	step("Waiting for %s to be Ready on Harness %s — the golden boot clones the private skill through Substrate's egress gateway", agentsTestPrivate, kagentHarness)
	readiness, err := waitAgentReady(agentsTestPrivate, agentsTestReadyTimeout)
	if err != nil {
		return "", err
	}
	requests, recordErr := host.requests()
	if !readiness.ready {
		record := "unread: " + fmt.Sprint(recordErr)
		if recordErr == nil {
			record = requestKinds(requests)
		}
		return "", fmt.Errorf("the golden of a skill from a private repository did not boot — the fixture recorded %s (a clone without the credential is the materializer sending no Authorization for the gateway to replace): %w", record, readiness.failure(agentsTestPrivate, agentsTestReadyTimeout))
	}
	if recordErr != nil {
		return "", recordErr
	}
	golden, err := goldenFetchVerdict(requests)
	if err != nil {
		return "", fmt.Errorf("%s reached Ready, but %w", agentsTestPrivate, err)
	}
	note("Ready after %s: revision %.12s; %s", readiness.elapsed.Round(time.Second), readiness.agent.Status.LatestSuccessfulRevision, golden)

	step("Removing %s and the fixture", agentsTestPrivate)
	if err := removeAgent(agentsTestPrivate); err != nil {
		return "", err
	}
	return fmt.Sprintf("PASS: a golden whose skill comes from a private repository: HelmRelease %s with %s.name %s rendered credentialRef {name: %s, key: %s} and reached Ready on Harness %s in %s — %s",
		agentsTestPrivate, skillsGitAuthSecretRefKey, skillHostSecret, skillHostSecret, skillsCredentialKey, kagentHarness, readiness.elapsed.Round(time.Second), golden), nil
}

// harnessRuntimeImage is the platform Harness's runtime image, which carries
// git for the skill host fixture.
func harnessRuntimeImage() (string, error) {
	h, err := readKagentObject(harnessResource, kagentHarness)
	if err != nil {
		return "", fmt.Errorf("reading Harness %s for its runtime image: %w", kagentHarness, err)
	}
	image := nestedString(h.Object, "spec", "workload", "image")
	if image == "" {
		return "", fmt.Errorf("Harness %s names no spec.workload.image", kagentHarness)
	}
	return image, nil
}
