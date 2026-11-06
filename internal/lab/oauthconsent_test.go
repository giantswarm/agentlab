package lab

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

const (
	testConsentIssuer = "https://muster.example"
	testConsentState  = "expected-state"
	testConsentCode   = "authorization-code-fixture"
)

func TestConsentFormRejectsForeignActionsAndMissingProtection(t *testing.T) {
	const endpoint = "https://muster.example/oauth/authorize"
	for _, test := range []struct {
		name, action, policy string
		wantError            bool
	}{
		{"same origin", endpoint, "frame-ancestors 'none'", false},
		{"foreign action", "https://attacker.example/collect", "frame-ancestors 'none'", true},
		{"missing framing protection", endpoint, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reply := &consentReply{status: http.StatusOK, header: http.Header{"Content-Security-Policy": {test.policy}},
				body: []byte(`<form method="post" action="` + test.action + `"><input name="consent_id" value="transaction"></form>`)}
			form, err := consentForm(reply, endpoint)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "transaction", form.Get("consent_id"))
			require.Equal(t, consentApprove, form.Get("decision"))
		})
	}
}

func TestConsentWalkDoesNotSubmitCredentialsToAnotherProvider(t *testing.T) {
	requests := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`<form method="post"><input name="login"><input name="password"></form>`))
	}))
	t.Cleanup(foreign.Close)
	browser, err := consentBrowserOn(http.DefaultTransport)
	require.NoError(t, err)
	flow := newConsentProofFlow(browser, &consentMetadata{Issuer: testConsentIssuer}, testConsentIssuer)
	flow.providerOrigin = "https://dex.example"
	flow.upstream = foreign.URL + "/auth/local"
	_, err = flow.authenticate(t.Context(), &config.User{Email: "person@example.com", Password: "fixture"})
	require.ErrorContains(t, err, "configured identity provider")
	require.Zero(t, requests)
}

func TestConsentWalkChecksIssuerAndClientState(t *testing.T) {
	browser, err := consentBrowserOn(http.DefaultTransport)
	require.NoError(t, err)
	flow := newConsentProofFlow(browser, &consentMetadata{Issuer: testConsentIssuer}, testConsentIssuer)
	flow.state = testConsentState
	for _, test := range []struct {
		state, issuer, code string
		valid               bool
	}{
		{testConsentState, testConsentIssuer, testConsentCode, true},
		{"other-state", testConsentIssuer, testConsentCode, false},
		{testConsentState, "https://attacker.example", testConsentCode, false},
		{testConsentState, testConsentIssuer, "", false},
	} {
		t.Run(test.state+test.issuer+test.code, func(t *testing.T) {
			query := url.Values{oauthStateParam: {test.state}, "iss": {test.issuer}, oauthCodeParam: {test.code}}
			code, err := flow.followProvider(t.Context(), &config.User{}, consentProofRedirect+"?"+query.Encode())
			if !test.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.code, code)
		})
	}
}

func TestKubernetesOAuthRouteUsesPublishedPortAndExactPrivateHosts(t *testing.T) {
	configuration := config.Default()
	configuration.Platform.GatewayPort = 8443
	configuration.Platform.Domain = "lab.example"
	raw, err := renderTemplate(configuration, "agent-platform-values.yaml.tmpl", nil)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &values))
	kubernetes := values[componentMCPKubernetes].(map[string]any)
	oauth := kubernetes["mcpKubernetes"].(map[string]any)["oauth"].(map[string]any)
	require.Equal(t, "https://mcp-kubernetes.lab.example:8443", oauth["baseURL"])
	for _, name := range []string{"cimd", "redirectURISecurity"} {
		block := oauth[name].(map[string]any)
		require.Equal(t, []any{"muster.lab.example"}, block["privateIPHosts"])
		require.NotEqual(t, true, block["allowPrivateIPs"])
		require.NotEqual(t, true, block["allowPrivateIPRedirectURIs"])
	}
	require.NotContains(t, kubernetes, "gatewayAPI")
	require.False(t, strings.Contains(string(raw), "consentCookieKey"))
	require.False(t, strings.Contains(string(raw), "trustedClients"))
}
