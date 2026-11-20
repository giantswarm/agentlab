package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/agentlab/internal/config"
)

type consentConsumerTarget struct {
	name, base string
}

const (
	consentAgentManager         = "agent-manager"
	consentPublicClientAuthNone = "none"
)

func consentConsumerTargets(values []string) ([]consentConsumerTarget, error) {
	names := []string{
		consentAgentManager, clusterManager, "mcp-capi", config.DevImageMCPPrometheus,
		"model-manager", "skill-manager", "vm-manager", "workspace-manager",
		"mcp-observability-platform", "mcp-template", "mcp-timescale",
	}
	targets := make([]consentConsumerTarget, 0, len(values))
	for _, value := range values {
		name, base, found := strings.Cut(value, "=")
		if !found || !slices.Contains(names, name) {
			return nil, errors.New("consumer must be a supported name=HTTPS-base-URL")
		}
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme != schemeHTTPS || parsed.Hostname() == "" || parsed.User != nil ||
			parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
			(parsed.Path != "" && parsed.Path != "/") {
			return nil, fmt.Errorf("consumer %s needs an HTTPS origin without credentials, path, query or fragment", name)
		}
		targets = append(targets, consentConsumerTarget{name: name, base: strings.TrimSuffix(base, "/")})
	}
	return targets, nil
}

func proveConsumerConsent(ctx context.Context, configuration *config.Config, user *config.User, targets []consentConsumerTarget, stages *proofStages) error {
	for _, target := range targets {
		if err := proveOneConsumerConsent(ctx, configuration, user, target, stages); err != nil {
			return fmt.Errorf("%s: %w", target.name, err)
		}
	}
	return nil
}

func proveOneConsumerConsent(ctx context.Context, configuration *config.Config, user *config.User, target consentConsumerTarget, stages *proofStages) error {
	stages.begin(target.name+" discovery", "Discovering the consumer's own issuer through HTTPS")
	browser, err := newConsentBrowser()
	if err != nil {
		return err
	}
	metadata, resource, err := discoverConsentIssuer(ctx, browser, target.base)
	if err != nil {
		return err
	}
	flow := newConsentProofFlow(browser, metadata, resource)
	flow.providerOrigin = consentOrigin(configuration.Issuer())
	stages.begin(target.name+" resource challenge", "Refusing anonymous MCP access with an OAuth metadata challenge")
	if err := consumerMCPChallenge(ctx, browser, target.base); err != nil {
		return err
	}
	stages.begin(target.name+" CIMD consent", "Fetching public HTTPS metadata, requiring consent and rejecting private metadata hosts")
	if err := flow.checkConsentGate(ctx); err != nil {
		return err
	}
	stages.begin(target.name+" S256 redemption", "Completing the lab identity-provider login and redeeming the public client's code")
	code, err := flow.authenticate(ctx, user)
	if err != nil {
		return err
	}
	accessToken, err := flow.exchange(ctx, code)
	if err != nil {
		return err
	}
	stages.begin(target.name+" protected resource", "Initializing MCP and listing the consumer's tools with its issued access token")
	if err := consumerMCPRead(ctx, browser, target.base, accessToken); err != nil {
		return err
	}
	stages.begin(target.name+" refresh", "Rotating the refresh token and listing tools with the refreshed access token")
	accessToken, err = flow.rotateRefresh(ctx)
	if err != nil {
		return err
	}
	if err := consumerMCPRead(ctx, browser, target.base, accessToken); err != nil {
		return err
	}
	stages.begin(target.name+" redirect rejection", "Refusing an unregistered redirect URI without redirecting to it")
	if err := flow.checkUnregisteredRedirect(ctx); err != nil {
		return err
	}
	stages.begin(target.name+" PKCE rejection", "Refusing redemption with an incorrect S256 verifier")
	if err := flow.checkPKCE(ctx, user); err != nil {
		return err
	}
	stages.begin(target.name+" DCR compatibility", "Registering a public compatibility client and using its authorization-code grant")
	return flow.checkDCRCompatibility(ctx, user, target.base)
}

func consumerMCPRead(ctx context.Context, browser *http.Client, base, accessToken string) error {
	connection, err := dialMCPAt(ctx, base+"/mcp", browser, accessToken, musterMCPOptions{
		name: "oauth-consent-test", protocol: "2025-11-25",
	})
	if err != nil {
		return err
	}
	defer func() { _ = connection.client.Close() }()
	tools, err := connection.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return err
	}
	if len(tools.Tools) == 0 {
		return fmt.Errorf("protected tools/list returned no consumer tools")
	}
	return nil
}

func consumerMCPChallenge(ctx context.Context, browser *http.Client, base string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"oauth-consent-test","version":"1"}}}`))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := browser.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(response.Header.Get("WWW-Authenticate"), "resource_metadata=") {
		return fmt.Errorf("anonymous MCP initialize did not receive a 401 OAuth metadata challenge")
	}
	return nil
}

func (flow *consentProofFlow) checkUnregisteredRedirect(ctx context.Context) error {
	endpoint, err := url.Parse(flow.authorizationURL(""))
	if err != nil {
		return err
	}
	query := endpoint.Query()
	query.Set(oauthRedirectURI, "https://attacker.invalid/unregistered")
	endpoint.RawQuery = query.Encode()
	reply, err := consentRequest(ctx, flow.browser, http.MethodGet, endpoint.String(), nil, "")
	if err := expectConsentStatus(reply, err, http.StatusBadRequest); err != nil {
		return err
	}
	var refusal struct {
		Code string `json:"error"`
	}
	if err := json.Unmarshal(reply.body, &refusal); err != nil {
		return fmt.Errorf("unregistered redirect refusal was not an OAuth error: %w", err)
	}
	if reply.header.Get("Location") != "" || refusal.Code != "invalid_request" {
		return fmt.Errorf("unregistered redirect URI did not receive a local invalid_request refusal")
	}
	return nil
}

func (flow *consentProofFlow) checkDCRCompatibility(ctx context.Context, user *config.User, base string) error {
	if flow.metadata.Registration != base+"/oauth/register" {
		return fmt.Errorf("DCR compatibility proof requires registration enabled and advertised on this lab consumer")
	}
	document, err := json.Marshal(map[string]any{
		"client_name": "Agentlab DCR compatibility proof", "redirect_uris": []string{consentProofRedirect},
		"token_endpoint_auth_method": consentPublicClientAuthNone, "grant_types": []string{oauthAuthorizationCodeGrant, oauthRefreshTokenGrant},
		"response_types": []string{oauthCodeParam},
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, flow.metadata.Registration, bytes.NewReader(document))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := flow.browser.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("DCR returned HTTP %d; enable public registration only on the proof deployment", response.StatusCode)
	}
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&registration); err != nil {
		return err
	}
	if registration.ClientID == "" {
		return fmt.Errorf("DCR response has no client_id")
	}
	dynamic := newConsentProofFlow(flow.browser, flow.metadata, flow.resource)
	dynamic.clientID = registration.ClientID
	dynamic.providerOrigin = flow.providerOrigin
	_, form, err := dynamic.page(ctx, "consent")
	if err != nil {
		return err
	}
	approved, err := dynamic.answer(ctx, form, consentOrigin(dynamic.metadata.Authorization))
	if err := expectConsentStatus(approved, err, http.StatusFound); err != nil {
		return err
	}
	dynamic.upstream = approved.header.Get("Location")
	code, err := dynamic.authenticate(ctx, user)
	if err != nil {
		return err
	}
	accessToken, err := dynamic.exchange(ctx, code)
	if err != nil {
		return err
	}
	return consumerMCPRead(ctx, flow.browser, base, accessToken)
}
