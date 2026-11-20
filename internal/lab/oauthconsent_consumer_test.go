package lab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestConsentConsumerTargets(t *testing.T) {
	for _, testCase := range []struct {
		value string
		valid bool
	}{
		{"agent-manager=https://agent-manager.lab.example", true},
		{"mcp-template=https://template.lab.example:8443/", true},
		{"unknown=https://mcp.example", false},
		{"agent-manager=http://mcp.example", false},
		{"agent-manager=https://person:password@mcp.example", false},
		{"agent-manager=https://mcp.example/path", false},
		{"agent-manager=https://mcp.example?query=value", false},
		{"agent-manager=https://mcp.example?", false},
		{"agent-manager=https://mcp.example#fragment", false},
		{consentAgentManager, false},
	} {
		t.Run(testCase.value, func(t *testing.T) {
			targets, err := consentConsumerTargets([]string{testCase.value})
			if !testCase.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.NotEmpty(t, targets[0].base)
		})
	}
}

func TestConsentUnregisteredRedirectRequiresLocalRefusal(t *testing.T) {
	for _, testCase := range []struct {
		name, location string
		status         int
		valid          bool
	}{
		{name: "local error", status: http.StatusBadRequest, valid: true},
		{name: "consent page", status: http.StatusOK},
		{name: "redirect", status: http.StatusFound, location: "https://attacker.invalid/unregistered"},
		{name: "error with location", status: http.StatusBadRequest, location: "https://attacker.invalid/unregistered"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, "https://attacker.invalid/unregistered", request.URL.Query().Get(oauthRedirectURI))
				if testCase.location != "" {
					writer.Header().Set("Location", testCase.location)
				}
				writer.WriteHeader(testCase.status)
				require.NoError(t, json.NewEncoder(writer).Encode(map[string]string{rpcErrorKey: "invalid_request"}))
			}))
			t.Cleanup(server.Close)
			browser, err := consentBrowserOn(http.DefaultTransport)
			require.NoError(t, err)
			flow := newConsentProofFlow(browser, &consentMetadata{Authorization: server.URL}, server.URL)
			err = flow.checkUnregisteredRedirect(t.Context())
			if testCase.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestConsentDCRRegistrationDeclaresWebApplication(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/oauth/register", request.URL.Path)
		require.Equal(t, "application/json", request.Header.Get("Content-Type"))
		var registration struct {
			ApplicationType string   `json:"application_type"`
			RedirectURIs    []string `json:"redirect_uris"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&registration))
		require.Equal(t, "web", registration.ApplicationType)
		require.Equal(t, []string{consentProofRedirect}, registration.RedirectURIs)
		writer.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	flow := newConsentProofFlow(server.Client(), &consentMetadata{Registration: server.URL + "/oauth/register"}, server.URL)
	require.ErrorContains(t, flow.checkDCRCompatibility(t.Context(), nil, server.URL), "DCR returned HTTP 403")
}

func TestConsentDCRGrantUsesRegisteredIdentity(t *testing.T) {
	flow := newConsentProofFlow(http.DefaultClient, &consentMetadata{Authorization: testConsentIssuer + "/oauth/authorize"}, testConsentIssuer)
	flow.clientID = "registered-public-client"
	endpoint, err := url.Parse(flow.authorizationURL("consent"))
	require.NoError(t, err)
	require.Equal(t, flow.clientID, endpoint.Query().Get(oauthClientID))
	require.Equal(t, consentProofRedirect, endpoint.Query().Get(oauthRedirectURI))

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.NoError(t, request.ParseForm())
		require.Equal(t, flow.clientID, request.Form.Get(oauthClientID))
		require.Equal(t, flow.verifier, request.Form.Get("code_verifier"))
		// #nosec G117 -- fixed OAuth protocol fixture values.
		require.NoError(t, json.NewEncoder(writer).Encode(struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}{AccessToken: "issued-access", RefreshToken: "issued-refresh"}))
	}))
	t.Cleanup(server.Close)
	flow.metadata.Token = server.URL
	_, err = flow.exchange(t.Context(), testConsentCode)
	require.NoError(t, err)
}

func TestConsumerMCPReadSupportsStatelessProtectedResources(t *testing.T) {
	var mutex sync.Mutex
	methods := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "" {
			writer.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example/.well-known/oauth-protected-resource"`)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer issued-access", request.Header.Get("Authorization"))
		if request.Method == http.MethodDelete {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		var message struct {
			ID     mcp.RequestId `json:"id"`
			Method string        `json:"method"`
		}
		require.NoError(t, json.NewDecoder(request.Body).Decode(&message))
		mutex.Lock()
		methods = append(methods, message.Method)
		mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		var result any
		switch message.Method {
		case "initialize":
			result = mcp.InitializeResult{
				ProtocolVersion: "2025-11-25", ServerInfo: mcp.Implementation{Name: "fixture", Version: "1"},
				Capabilities: mcp.ServerCapabilities{Tools: &struct {
					ListChanged bool `json:"listChanged,omitempty"`
				}{}},
			}
		case "notifications/initialized":
			writer.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = mcp.ListToolsResult{Tools: []mcp.Tool{mcp.NewTool("consumer-proof-read")}}
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		require.NoError(t, json.NewEncoder(writer).Encode(mcp.JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: message.ID, Result: result}))
	}))
	t.Cleanup(server.Close)
	require.NoError(t, consumerMCPChallenge(t.Context(), server.Client(), server.URL))
	require.NoError(t, consumerMCPRead(t.Context(), server.Client(), server.URL, "issued-access"))
	mutex.Lock()
	defer mutex.Unlock()
	require.Contains(t, methods, "initialize")
	require.Contains(t, methods, "tools/list")
}
