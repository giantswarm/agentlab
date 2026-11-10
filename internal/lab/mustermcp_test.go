package lab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

const fakeFormURL = "https://portal.example/credentials?operation=op1"

// fakeCredentialMuster answers call_tool as muster passes agent-manager's credential
// answer through: input_required to a 2026-07-28 client declaring URL
// elicitation, -32042 to a 2025-11-25 one, the tool's result otherwise.
func fakeCredentialMuster(t *testing.T) string {
	t.Helper()
	srv := mcpserver.NewMCPServer("fake-muster", "1", mcpserver.WithToolCapabilities(false))
	elicitation := mcp.ElicitationParams{Mode: mcp.ElicitationModeURL, Message: "[agent-manager] Enter the value in the credential UI.", URL: fakeFormURL, ElicitationID: "e1"}
	srv.AddTool(mcp.NewTool("call_tool"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if declaresURLElicitation(ctx) {
			if mcp.IsModernProtocol(mcpserver.RequestProtocolVersion(ctx)) {
				return mcpserver.NewInputRequestBuilder("state").Elicit("credential_e1", elicitation).ToolResult(), nil
			}
			return nil, mcp.URLElicitationRequiredError{Elicitations: []mcp.ElicitationParams{elicitation}}
		}
		return mcp.NewToolResultText(`{"isError":false,"content":[{"type":"text","text":"{\"pending\":true}"},{"type":"resource_link","uri":"` + fakeFormURL + `","name":"form"}],"structuredContent":{"pending":true}}`), nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func declaresURLElicitation(ctx context.Context) bool {
	var capabilities *mcp.ClientCapabilities
	if info := mcpserver.RequestProtocolInfoFromContext(ctx); info != nil && info.Modern {
		capabilities = info.ClientCapabilities
	} else if session, ok := mcpserver.ClientSessionFromContext(ctx).(mcpserver.SessionWithClientInfo); ok {
		declared := session.GetClientCapabilities()
		capabilities = &declared
	}
	return capabilities != nil && capabilities.Elicitation != nil && capabilities.Elicitation.URL != nil
}

func TestMusterMCPClientGenerations(t *testing.T) {
	endpoint := fakeCredentialMuster(t)
	for _, tc := range []struct {
		name     string
		options  musterMCPOptions
		protocol string
		check    func(t *testing.T, a *musterAnswer)
	}{
		{
			name:     "2026-07-28 with URL elicitation",
			options:  musterMCPOptions{name: "modern", protocol: mcp.ProtocolVersion20260728, urlElicitation: true},
			protocol: mcp.ProtocolVersion20260728,
			check: func(t *testing.T, a *musterAnswer) {
				require.NotNil(t, a.input)
				require.Equal(t, "state", a.requestState)
				e, err := oneURLElicitation(a)
				require.NoError(t, err)
				require.True(t, attributedTo(e.Message, agentManagerMCPServer))
				require.Equal(t, fakeFormURL, e.URL)
			},
		},
		{
			name:     "2025-11-25 with URL elicitation",
			options:  musterMCPOptions{name: "legacy", protocol: mcp.ProtocolVersion20251125, urlElicitation: true},
			protocol: mcp.ProtocolVersion20251125,
			check: func(t *testing.T, a *musterAnswer) {
				require.Nil(t, a.input)
				require.Len(t, a.urlRequired, 1)
				require.Equal(t, fakeFormURL, a.urlRequired[0].URL)
			},
		},
		{
			name:     "no URL elicitation",
			options:  musterMCPOptions{name: "no-elicitation", protocol: mcp.ProtocolVersion20251125},
			protocol: mcp.ProtocolVersion20251125,
			check: func(t *testing.T, a *musterAnswer) {
				require.Empty(t, a.urlElicitations())
				require.NotNil(t, a.result)
				require.JSONEq(t, `{"pending":true}`, string(a.result.StructuredContent))
				require.Equal(t, []string{fakeFormURL}, a.result.resourceLinks())
				require.JSONEq(t, `{"pending":true}`, a.result.text())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := dialMCPAt(t.Context(), endpoint, http.DefaultClient, "token", tc.options)
			require.NoError(t, err)
			defer m.close()
			require.Equal(t, tc.protocol, m.protocol())
			a, err := m.call(t.Context(), agentManagerToolPrefix+"set_organization_model", nil, mcp.MultiRoundTripParams{})
			require.NoError(t, err)
			tc.check(t, a)
		})
	}
}

func TestParseMusterToolResult(t *testing.T) {
	r, err := parseMusterToolResult(`{"isError":true,"content":[{"type":"text","text":"forbidden"}],"structuredContent":null}`, false)
	require.NoError(t, err)
	require.True(t, r.IsError)
	require.Equal(t, "forbidden", r.text())
	require.Nil(t, r.StructuredContent)

	r, err = parseMusterToolResult("tool x_nope not found", true)
	require.NoError(t, err)
	require.True(t, r.IsError)
	require.Equal(t, "tool x_nope not found", r.text())

	_, err = parseMusterToolResult("", false)
	require.ErrorIs(t, err, errEmptyToolAnswer)

	_, err = parseMusterToolResult("not json", false)
	require.ErrorContains(t, err, "unexpected call_tool payload shape")
}

func TestMusterMCPRecordsElicitationComplete(t *testing.T) {
	srv := mcpserver.NewMCPServer("fake-muster", "1", mcpserver.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("call_tool"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := srv.SendNotificationToClient(ctx, string(mcp.MethodNotificationElicitationComplete), map[string]any{"elicitationId": "e1"}); err != nil {
			return nil, err
		}
		if err := srv.SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "x"}); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText(`{"isError":false,"content":[{"type":"text","text":"{}"}]}`), nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)

	m, err := dialMCPAt(t.Context(), ts.URL+"/mcp", http.DefaultClient, "token", musterMCPOptions{name: "legacy", protocol: mcp.ProtocolVersion20251125, urlElicitation: true, listen: true})
	require.NoError(t, err)
	defer m.close()
	_, err = m.call(t.Context(), "x_agent-manager_set_organization_model", nil, mcp.MultiRoundTripParams{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(m.completedElicitations()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"e1"}, m.completedElicitations())
}

func TestMusterAnswerAnswered(t *testing.T) {
	a := &musterAnswer{input: mcp.InputRequests{"e1": {Method: mcp.MethodElicitationCreate}}, requestState: "s"}
	for _, action := range []mcp.ElicitationResponseAction{mcp.ElicitationResponseActionDecline, mcp.ElicitationResponseActionCancel} {
		rt := a.answered(action)
		require.Equal(t, "s", rt.RequestState)
		raw, err := json.Marshal(rt.InputResponses)
		require.NoError(t, err)
		require.JSONEq(t, `{"e1":{"action":"`+string(action)+`"}}`, string(raw))
	}
}
