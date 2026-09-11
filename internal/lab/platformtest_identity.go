package lab

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/giantswarm/agentlab/internal/config"
)

// The kagent controller's caller identity has two layers on the 4.x line
// (the chart's docs/authentication.md, "The kagent controller route"): the
// agentgateway JWT policy on the GRPCRoute verifies the bearer against the
// lab Dex — Strict: a call without one is refused at the edge — and sets
// x-user-id from the verified email claim, replacing whatever the client
// sent; the controller re-derives the caller from the same bearer
// (trusted-proxy). So a client that forges the identity header is still the
// person its token names — at the controller, and at muster and agent-manager
// too, which read nothing but the bearer. This is the proof of that: user
// story 21 of the plan (a spoofed header must not impersonate).

// forgedIdentity is the identity a forging client claims in the header.
const forgedIdentity = "attacker@lab.local"

// proveControllerIdentity drives SystemService/GetCurrentUser through the
// edge over native gRPC: without a token (refused at the edge — the JWT
// policy's Unauthenticated), then with the person's token and a forged
// x-user-id (the claims name the person). It needs the connectivity chart's
// GRPCRoute for the controller, the one the proofs' turns take.
func proveControllerIdentity(cfg *config.Config, user *config.User, token string) error {
	route, err := readControllerRoute()
	if err != nil {
		return fmt.Errorf("the controller identity proof needs the controller's route on the edge: %w", err)
	}
	for _, line := range route.lines() {
		note("%s", line)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	step("Calling the kagent controller through the edge WITHOUT a token — expecting the JWT policy's refusal")
	anonymous, err := dialKagentAPI(cfg, "")
	if err != nil {
		return err
	}
	defer anonymous.close()
	anonymous.extra = metadata.Pairs(userIDHeader, forgedIdentity)
	claims, err := anonymous.currentUser(ctx)
	switch {
	case err == nil:
		return fmt.Errorf("the controller answered GetCurrentUser WITHOUT a token (claims %v): the JWT policy on GRPCRoute %s is not enforcing — check `kubectl -n %s get agentgatewaypolicy %s`", claims, kagentControllerRoute, platformNamespace, kagentControllerJWTPolicy)
	case isUnauthenticated(err):
		note("refused at the edge: %s", excerpt(err.Error(), 120))
	default:
		return fmt.Errorf("wanted the edge's refusal (gRPC %s) for a call without a token, got: %w", codes.Unauthenticated, err)
	}

	step("Calling GetCurrentUser as %s with a forged %s (%s) — expecting the token's identity", user.Email, userIDHeader, forgedIdentity)
	forging, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer forging.close()
	forging.extra = metadata.Pairs(userIDHeader, forgedIdentity)
	claims, err = forging.currentUser(ctx)
	if err != nil {
		return fmt.Errorf("GetCurrentUser as %s: %w", user.Email, err)
	}
	email, _ := claims["email"].(string)
	switch email {
	case user.Email:
		note("the controller attributes the call to %s (the verified email claim; the forged header was replaced)", email)
	case forgedIdentity:
		return fmt.Errorf("the controller attributed the call to the FORGED %s %s: the edge did not replace the header (the policy's transformation) or the controller trusts the header over the bearer", userIDHeader, email)
	default:
		return fmt.Errorf("GetCurrentUser answered email %q, want %s (claims %v)", email, user.Email, claims)
	}
	return nil
}

// identityProofAgent is the agent the viewer's refused create names; the
// refusal leaves nothing behind, and the proof asserts that too.
const identityProofAgent = "agentlab-platform-test-identity"

// proveAgentManagerIdentity drives agent-manager as the viewer with the
// identity header forged to the platform admin: agent-manager writes as the
// bearer's person (downstream OAuth), so the apiserver refuses the create as
// User "oidc:<viewer>" — the header bought nothing, no AgentTemplate exists.
func proveAgentManagerIdentity(cfg *config.Config) error {
	admin := cfg.FindUserInGroup("platform-admins")
	viewer := cfg.FindUserInGroup("viewers")
	if admin == nil || viewer == nil {
		note("skipping the agent-manager identity proof: %s needs one platform-admins and one viewers user", config.File)
		return nil
	}
	toolPrefix := "x_" + agentManagerMCPServer + "_"
	step("%screate_agent as %s with x-user-id forged to %s — expecting the apiserver's Forbidden for User \"oidc:%s\"", toolPrefix, viewer.Email, admin.Email, viewer.Email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		viewer.Email, viewer.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	session, err := openMusterSession(cfg, token, "platform-test-identity")
	if err != nil {
		return err
	}
	session.setHeader(userIDHeader, admin.Email)
	text, err := session.callServerTool(toolPrefix+"create_agent", map[string]any{
		nameKey: identityProofAgent, modelConfigKey: defaultModelConfig, toolsetKey: []string{agentsTestToolset},
	})
	switch {
	case err == nil:
		return fmt.Errorf("%s created an agent through agent-manager with a forged x-user-id — agent-manager is not acting as the caller: %.200s", viewer.Email, text)
	case !strings.Contains(strings.ToLower(err.Error()), "forbidden"):
		return fmt.Errorf("%s: wanted the apiserver's Forbidden, got: %w", viewer.Email, err)
	case !strings.Contains(err.Error(), `User "oidc:`+viewer.Email+`"`):
		return fmt.Errorf("%s: Forbidden, but not under the viewer's own name — the apiserver saw someone else (the forged header?): %w", viewer.Email, err)
	}
	note("%s: %s", viewer.Email, excerpt(err.Error(), 200))
	if agentTemplateExists(identityProofAgent) {
		return fmt.Errorf("AgentTemplate %s exists although the create was refused", identityProofAgent)
	}
	return nil
}
