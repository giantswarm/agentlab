package lab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/kagentpb"
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

// grpcUnauthenticated is the gRPC status of a refused credential.
const grpcUnauthenticated = 16

// proveControllerIdentity drives SystemService/GetCurrentUser through the
// edge: without a token (refused at the edge — HTTP 401, or gRPC
// Unauthenticated when the data plane answers in gRPC), then with the
// person's token and a forged x-user-id (the claims name the person). Only on
// a lab whose chart routes the controller by gRPC service (the GRPCRoute of
// the 4.x connectivity chart); the 3.x prefixed HTTPRoute carries no policy.
func proveControllerIdentity(cfg *config.Config, user *config.User, token string) error {
	if kagentRoutePrefixServed() != "" {
		note("skipping the controller identity proof: no GRPCRoute %s in %s (the 3.x route carries no JWT policy)", kagentGRPCRouteName, platformNamespace)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	step("Calling the kagent controller through the edge WITHOUT a token — expecting the JWT policy's refusal")
	anonymous, err := newKagentAPI(cfg, forgedIdentity, "")
	if err != nil {
		return err
	}
	var claims kagentpb.GetCurrentUserResponse
	err = anonymous.call(ctx, systemService, "GetCurrentUser", &kagentpb.GetCurrentUserRequest{}, &claims, nil)
	switch {
	case err == nil:
		return fmt.Errorf("the controller answered GetCurrentUser WITHOUT a token (claims %v): the JWT policy on GRPCRoute %s is not enforcing — check `kubectl -n %s get agentgatewaypolicy kagent-controller-jwt`", claims.GetClaims().AsMap(), kagentGRPCRouteName, platformNamespace)
	case refusedAtTheEdge(err):
		note("refused at the edge: %s", excerpt(err.Error(), 120))
	default:
		return fmt.Errorf("wanted the edge's refusal (HTTP %d or gRPC status %d) for a call without a token, got: %w", http.StatusUnauthorized, grpcUnauthenticated, err)
	}

	step("Calling GetCurrentUser as %s with a forged x-user-id (%s) — expecting the token's identity", user.Email, forgedIdentity)
	forging, err := newKagentAPI(cfg, forgedIdentity, token)
	if err != nil {
		return err
	}
	if err := forging.call(ctx, systemService, "GetCurrentUser", &kagentpb.GetCurrentUserRequest{}, &claims, nil); err != nil {
		return fmt.Errorf("GetCurrentUser as %s: %w", user.Email, err)
	}
	email, _ := claims.GetClaims().AsMap()["email"].(string)
	switch email {
	case user.Email:
		note("the controller attributes the call to %s (the verified email claim; the forged header was replaced)", email)
	case forgedIdentity:
		return fmt.Errorf("the controller attributed the call to the FORGED x-user-id %s: the edge did not replace the header (the policy's transformation) or the controller trusts the header over the bearer", forgedIdentity)
	default:
		return fmt.Errorf("GetCurrentUser answered email %q, want %s (claims %v)", email, user.Email, claims.GetClaims().AsMap())
	}
	return nil
}

// refusedAtTheEdge recognises the JWT policy's refusal of a call: HTTP 401
// from the data plane, or gRPC Unauthenticated in a trailers-only answer.
func refusedAtTheEdge(err error) bool {
	var status *grpcStatus
	if errors.As(err, &status) {
		return status.code == grpcUnauthenticated
	}
	return strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", http.StatusUnauthorized))
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
