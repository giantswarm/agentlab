package lab

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	consentProofClientID = "https://claude.ai/oauth/mcp-oauth-client-metadata"
	consentProofRedirect = "https://claude.ai/api/mcp/auth_callback"
	consentProofScopes   = "openid email profile groups audience:server:client_id:kubernetes"
	consentScopeParam    = "scope"
	consentLoginParam    = "login"
	consentApprove       = "approve"
)

type consentMetadata struct {
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	CIMD          bool     `json:"client_id_metadata_document_supported"`
	PKCEMethods   []string `json:"code_challenge_methods_supported"`
}

// OAuthConsentTest exercises the authorization-code flow against both lab
// issuers, stopping at the registered client redirect without fetching it.
func OAuthConsentTest(ctx context.Context, configuration *config.Config, email string) error {
	if err := useClusterKubeconfig(configuration); err != nil {
		return err
	}
	user := configuration.FindUser(email)
	if user == nil {
		return fmt.Errorf("no lab user %q", email)
	}
	stages := newProofStages()
	err := proveOAuthConsent(ctx, configuration, user, stages)
	return stages.close(err)
}

func proveOAuthConsent(ctx context.Context, configuration *config.Config, user *config.User, stages *proofStages) error {
	stages.begin("consent defaults", "Checking both deployments have no consent or trust overrides")
	if err := consentDeploymentDefaults(ctx); err != nil {
		return err
	}
	for _, target := range []struct{ name, base string }{
		{componentMuster, configuration.MusterBaseURL()},
		{componentMCPKubernetes, configuration.GatewayURL(componentMCPKubernetes)},
	} {
		stages.begin(target.name+" discovery", "Discovering %s through the HTTPS edge", target.name)
		browser, err := newConsentBrowser()
		if err != nil {
			return err
		}
		metadata, resource, err := discoverConsentIssuer(ctx, browser, target.base)
		if err != nil {
			return err
		}
		stages.begin(target.name+" default consent", "Checking first-use consent, denial, CSRF and replay for %s", target.name)
		flow := newConsentProofFlow(browser, metadata, resource)
		flow.providerOrigin = consentOrigin(configuration.Issuer())
		if err := flow.checkConsentGate(ctx); err != nil {
			return err
		}
		stages.begin(target.name+" browser login", "Completing CIMD authorization code + S256 PKCE as %s", user.Email)
		code, err := flow.authenticate(ctx, user)
		if err != nil {
			return err
		}
		accessToken, err := flow.exchange(ctx, code)
		if err != nil {
			return err
		}
		stages.begin(target.name+" MCP request", "Using the issued token for an MCP initialize and a Kubernetes namespace read")
		if err := consentMCPRead(ctx, configuration, user, target.base, accessToken, target.name == componentMuster); err != nil {
			return err
		}
		stages.begin(target.name+" remembered grant", "Checking remembered consent and prompt=consent for %s", target.name)
		if err := flow.checkRemembered(ctx); err != nil {
			return err
		}
		stages.begin(target.name+" replica replacement", "Replacing both %s processes before completing a pending browser callback", target.name)
		if err := flow.checkReplicaReplacement(ctx, configuration, user, target.name, target.base, accessToken); err != nil {
			return err
		}
	}
	return nil
}

func discoverConsentIssuer(ctx context.Context, browser *http.Client, base string) (*consentMetadata, string, error) {
	var protected struct {
		Resource string   `json:"resource"`
		Issuers  []string `json:"authorization_servers"`
	}
	if err := consentGetJSON(ctx, browser, base+"/.well-known/oauth-protected-resource", &protected); err != nil {
		return nil, "", err
	}
	if (protected.Resource != base && protected.Resource != base+"/mcp") || len(protected.Issuers) != 1 || protected.Issuers[0] != base {
		return nil, "", fmt.Errorf("protected resource metadata does not bind %s to its own issuer", base)
	}
	var metadata consentMetadata
	if err := consentGetJSON(ctx, browser, base+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return nil, "", err
	}
	if metadata.Issuer != base || metadata.Authorization != base+"/oauth/authorize" || metadata.Token != base+"/oauth/token" || !metadata.CIMD {
		return nil, "", fmt.Errorf("authorization metadata does not advertise this issuer's endpoints and CIMD")
	}
	return &metadata, protected.Resource, nil
}

func consentGetJSON(ctx context.Context, browser *http.Client, target string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := browser.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("metadata returned HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
}

type consentProofFlow struct {
	browser                   *http.Client
	metadata                  *consentMetadata
	resource, verifier, state string
	upstream                  string
	providerOrigin            string
}

func newConsentProofFlow(browser *http.Client, metadata *consentMetadata, resource string) *consentProofFlow {
	return &consentProofFlow{browser: browser, metadata: metadata, resource: resource}
}

func (flow *consentProofFlow) authorizationURL(prompt string) string {
	flow.verifier = rand.Text() + rand.Text()
	flow.state = rand.Text()
	challenge := sha256.Sum256([]byte(flow.verifier))
	query := url.Values{
		"client_id": {consentProofClientID}, "redirect_uri": {consentProofRedirect}, "response_type": {oauthCodeParam},
		oauthStateParam: {flow.state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"}, consentScopeParam: {consentProofScopes}, "resource": {flow.resource},
	}
	if prompt != "" {
		query.Set("prompt", prompt)
	}
	return flow.metadata.Authorization + "?" + query.Encode()
}

func (flow *consentProofFlow) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {consentProofClientID}, "redirect_uri": {consentProofRedirect},
		oauthCodeParam: {code}, "code_verifier": {flow.verifier}, "resource": {flow.resource},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, flow.metadata.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := flow.browser.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("code exchange returned HTTP %d", response.StatusCode)
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&tokens); err != nil {
		return "", err
	}
	if tokens.AccessToken == "" {
		return "", fmt.Errorf("code exchange returned no access token")
	}
	return tokens.AccessToken, nil
}

func consentMCPRead(ctx context.Context, configuration *config.Config, user *config.User, base, accessToken string, aggregated bool) error {
	session, err := openMusterSessionAt(base, accessToken, "oauth-consent-test")
	if err != nil {
		return err
	}
	arguments := map[string]any{resourceTypeKey: resourceNamespaces}
	tool := "list"
	if aggregated {
		arguments = map[string]any{nameKey: kubernetesTool(configuration, "list"), argumentsKey: kubernetesArgs(configuration, arguments)}
		tool = musterMetaCallTool
	}
	result, err := session.callTool(tool, arguments)
	if err != nil {
		return err
	}
	text := innerText(result)
	if aggregated && !strings.Contains(text, platformNamespace) {
		challenge, err := signInChallenge(configuration, session, configuration.MCPServerName())
		if err != nil {
			return err
		}
		if err := completeConsentBrokerSignIn(ctx, configuration, user, challenge.authURL); err != nil {
			return err
		}
		result, err = session.callTool(tool, arguments)
		if err != nil {
			return err
		}
		text = innerText(result)
	}
	if strings.Contains(text, `"isError":true`) || !strings.Contains(text, platformNamespace) {
		return fmt.Errorf("namespace read did not include %s: %.500s", platformNamespace, text)
	}
	return nil
}

func newConsentBrowser() (*http.Client, error) {
	client, err := labHTTPClient(30 * time.Second)
	if err != nil {
		return nil, err
	}
	return consentBrowserOn(client.Transport)
}

func labMusterAccessToken(ctx context.Context, configuration *config.Config, user *config.User) (string, error) {
	browser, err := newConsentBrowser()
	if err != nil {
		return "", err
	}
	metadata, resource, err := discoverConsentIssuer(ctx, browser, configuration.MusterBaseURL())
	if err != nil {
		return "", err
	}
	flow := newConsentProofFlow(browser, metadata, resource)
	flow.providerOrigin = consentOrigin(configuration.Issuer())
	_, form, err := flow.page(ctx, "")
	if err != nil {
		return "", err
	}
	approved, err := flow.answer(ctx, form, consentOrigin(metadata.Authorization))
	if err := expectConsentStatus(approved, err, http.StatusFound); err != nil {
		return "", err
	}
	flow.upstream = approved.header.Get("Location")
	code, err := flow.authenticate(ctx, user)
	if err != nil {
		return "", err
	}
	return flow.exchange(ctx, code)
}
