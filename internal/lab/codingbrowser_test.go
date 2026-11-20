package lab

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

func TestJudgeBrowserProof(t *testing.T) {
	readings := codingProbe{}
	for _, key := range []string{"fetch", "fetch-dot", "redirect", "redirect-dot", browserWorkerCase, "worker-dot", "suite", "suite-dot"} {
		readings["browser-"+key] = "net::ERR_NAME_NOT_RESOLVED"
	}
	readings["browser-preflight"] = "blocked"
	readings["browser-anonymous"] = "401"
	readings["browser-extra-header"] = "400"
	readings["browser-memory"] = "bounded"
	require.NoError(t, judgeBrowserProof(readings))
	for key := range readings {
		t.Run(key, func(t *testing.T) {
			missing := maps.Clone(readings)
			delete(missing, key)
			require.Error(t, judgeBrowserProof(missing))
			wrong := maps.Clone(readings)
			wrong[key] = "403"
			require.Error(t, judgeBrowserProof(wrong))
		})
	}
}

func TestBrowserMusterTarget(t *testing.T) {
	policy := testBrowserPolicy()
	target, err := browserMusterTarget(policy)
	require.NoError(t, err)
	require.Equal(t, "http://muster.agent-platform.svc.cluster.local:8090/mcp", target)
	policy.Rules[0].Http.Effects.ReplaceHeaders[0].ActorJwt = nil
	_, err = browserMusterTarget(policy)
	require.ErrorContains(t, err, "actor-JWT")
	_, err = browserMusterTarget(nil)
	require.Error(t, err)
}

func testBrowserPolicy() *ateapi.EgressPolicy {
	return &ateapi.EgressPolicy{Rules: []*ateapi.EgressRule{{Http: &ateapi.HTTPRule{
		Hostnames: []string{"muster.agent-platform.svc.cluster.local"},
		Ports:     &ateapi.Ports{Numbers: []int32{8090}},
		Effects:   &ateapi.HttpRuleEffects{ReplaceHeaders: []*ateapi.CredentialHeader{{Header: "Authorization", ActorJwt: &ateapi.ActorJWTSource{}}}},
	}}}}
}

type browserPolicyClient struct {
	ateapi.ControlClient
	policies []*ateapi.EgressPolicy
	failure  error
	reads    int
}

func (c *browserPolicyClient) GetActorEgressPolicy(ctx context.Context, _ *ateapi.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapi.EgressPolicy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.failure != nil {
		return nil, c.failure
	}
	policy := c.policies[min(c.reads, len(c.policies)-1)]
	c.reads++
	return policy, nil
}

func TestBrowserPolicyOpensDuringTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &browserPolicyClient{policies: []*ateapi.EgressPolicy{nil, testBrowserPolicy()}}
		require.NoError(t, waitBrowserPolicy(t.Context(), client, "browser-actor", defaultMusterMCPURL, make(chan struct{})))
		require.Equal(t, 2, client.reads)
	})
}

func TestBrowserPolicyRefusesMissingOrWrongBinding(t *testing.T) {
	done := make(chan struct{})
	close(done)
	client := &browserPolicyClient{policies: []*ateapi.EgressPolicy{testBrowserPolicy()}}
	require.ErrorContains(t, waitBrowserPolicy(t.Context(), client, "browser-actor", defaultMusterMCPURL, done), "turn ended")
	require.Zero(t, client.reads)
	require.ErrorContains(t, waitBrowserPolicy(t.Context(), client, "browser-actor", "http://muster.other.svc:8090/mcp", make(chan struct{})), "live actor-JWT rule")
}

func TestBrowserPolicyReadFailures(t *testing.T) {
	failure := errors.New("policy unavailable")
	client := &browserPolicyClient{failure: failure}
	require.ErrorIs(t, waitBrowserPolicy(t.Context(), client, "browser-actor", defaultMusterMCPURL, make(chan struct{})), failure)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, waitBrowserPolicy(ctx, client, "browser-actor", defaultMusterMCPURL, make(chan struct{})), context.Canceled)
}

func TestBrowserPolicyDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		client := &browserPolicyClient{policies: []*ateapi.EgressPolicy{nil}}
		require.ErrorIs(t, waitBrowserPolicy(ctx, client, "browser-actor", defaultMusterMCPURL, make(chan struct{})), context.DeadlineExceeded)
		require.Positive(t, client.reads)
	})
}

func TestBrowserSessionTarget(t *testing.T) {
	session := &apiv1alpha1.Session{Providers: []*apiv1alpha1.SessionProvider{{Audience: componentMuster, Scheme: "Bearer", Hostname: "muster.other.svc"}}}
	target, err := browserSessionTarget(session)
	require.NoError(t, err)
	require.Equal(t, "http://muster.other.svc:8090/mcp", target)
	_, err = browserSessionTarget(&apiv1alpha1.Session{})
	require.Error(t, err)
}

func TestJudgeBrowserAttachment(t *testing.T) {
	readings := codingProbe{"browser-person": "person", "browser-comment": "https://github.com/owner/repo/pull/1#issuecomment-1", "browser-upload": "attached", "browser-close": browserClosed}
	require.NoError(t, judgeBrowserAttachment(readings))
	for key := range readings {
		missing := maps.Clone(readings)
		delete(missing, key)
		require.Error(t, judgeBrowserAttachment(missing))
	}
	readings["browser-comment"] = "https://example.org/#issuecomment-1"
	require.Error(t, judgeBrowserAttachment(readings))
}

func TestBrowserAttachmentCleansBranchWhenPRCreationFails(t *testing.T) {
	directory := t.TempDir()
	origin := filepath.Join(directory, "origin")
	clone := filepath.Join(directory, "o", "r")
	environment := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@lab.local", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@lab.local", "GH_TOKEN=ghu_"+credentialPlaceholder)
	git := func(arguments ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", arguments...) // #nosec G204 -- test-owned git arguments
		command.Env = environment
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return string(output)
	}
	git("init", "-q", "-b", "main", origin)
	git("-C", origin, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "seed")
	git("clone", "-q", origin, clone)
	stub := filepath.Join(directory, "gh")
	require.NoError(t, os.WriteFile(stub, []byte(`#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == 'pr comment --help' ]]; then
  echo --attach
  exit 0
fi
git diff --quiet origin/HEAD HEAD && exit 99
exit 42
`), 0o600))
	require.NoError(t, os.Chmod(stub, 0o700)) // #nosec G302 -- executable stub requires owner execute permission

	command := exec.CommandContext(t.Context(), "sh", "-c", strings.Replace(browserAttachmentCommand("o/r", "browser-proof"), "gh=/opt/kagent/tools/github/bin/gh", "gh='"+stub+"'", 1)) // #nosec G204 -- fixed proof script and test-owned executable
	command.Dir = directory
	command.Env = environment
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	require.ErrorAs(t, err, &exitError, "%s", output)
	require.Equal(t, 42, exitError.ExitCode(), "%s", output)
	require.Empty(t, git("ls-remote", origin, "refs/heads/browser-proof"))
}
