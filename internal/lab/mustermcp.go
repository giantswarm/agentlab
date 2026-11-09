package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/agentlab/internal/config"
)

// musterMCP is an MCP client of the lab muster as a person on mcp-go's
// client, pinned to one protocol revision and declaring what the person can
// answer. It is the client for the proofs that judge what muster answers a
// client of a given MCP generation: the input_required result of 2026-07-28,
// the -32042 error of 2025-11-25, a plain result for a client without URL
// elicitation (MCP specification, client/elicitation).
type musterMCP struct {
	client *mcpclient.Client
}

// musterMCPOptions are a client's generation: its name, the protocol
// revision it asks for and whether it declares URL elicitation.
type musterMCPOptions struct {
	name           string
	protocol       string
	urlElicitation bool
}

// musterMCPTimeout bounds one exchange with muster.
const musterMCPTimeout = time.Minute

// dialMusterMCP connects to muster's /mcp with the person's token. On
// 2026-07-28 the declared capabilities travel in every request's _meta; on
// 2025-11-25 they are declared at initialize. A muster that does not speak
// the asked revision is answered on the newest one it does: protocol says
// which one is in effect.
func dialMusterMCP(ctx context.Context, cfg *config.Config, token string, o musterMCPOptions) (*musterMCP, error) {
	httpClient, err := labHTTPClient(musterMCPTimeout)
	if err != nil {
		return nil, err
	}
	return dialMCPAt(ctx, strings.TrimSuffix(cfg.MusterBaseURL(), "/")+"/mcp", httpClient, token, o)
}

// dialMCPAt is dialMusterMCP on the MCP endpoint with httpClient.
func dialMCPAt(ctx context.Context, endpoint string, httpClient *http.Client, token string, o musterMCPOptions) (*musterMCP, error) {
	t, err := transport.NewStreamableHTTP(endpoint,
		transport.WithHTTPBasicClient(httpClient),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}),
	)
	if err != nil {
		return nil, fmt.Errorf("muster MCP transport: %w", err)
	}
	var capabilities mcp.ClientCapabilities
	if o.urlElicitation {
		capabilities.Elicitation = &mcp.ElicitationCapability{URL: &struct{}{}}
	}
	c := mcpclient.NewClient(t,
		mcpclient.WithManualInputRoundTrips(),
		mcpclient.WithClientCapabilities(capabilities),
		mcpclient.WithProtocolVersion(o.protocol),
	)
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("muster MCP start as %s: %w", o.name, err)
	}
	initialize := mcp.InitializeRequest{}
	initialize.Params.ClientInfo = mcp.Implementation{Name: o.name, Version: "1"}
	if _, err := c.Initialize(ctx, initialize); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("muster MCP initialize as %s on %s: %w", o.name, o.protocol, err)
	}
	return &musterMCP{client: c}, nil
}

// protocol is the revision in effect.
func (m *musterMCP) protocol() string { return m.client.ProtocolVersion() }

func (m *musterMCP) close() { _ = m.client.Close() }

// musterAnswer is muster's answer to one call_tool: an input_required result
// (2026-07-28), the URL elicitations of a -32042 error (2025-11-25), or the
// target tool's result.
type musterAnswer struct {
	input        mcp.InputRequests
	requestState string
	urlRequired  []mcp.ElicitationParams
	result       *musterToolResult
}

// call runs tool with args through muster's call_tool, the way every
// aggregated tool is reached, answering a previous input_required result
// with roundTrip when it is set.
func (m *musterMCP) call(ctx context.Context, tool string, args map[string]any, roundTrip mcp.MultiRoundTripParams) (*musterAnswer, error) {
	if args == nil {
		args = map[string]any{}
	}
	request := mcp.CallToolRequest{}
	request.Params.Name = musterMetaCallTool
	request.Params.Arguments = map[string]any{nameKey: tool, argumentsKey: args}
	request.Params.MultiRoundTripParams = roundTrip
	res, err := m.client.CallTool(ctx, request)
	var required mcp.URLElicitationRequiredError
	switch {
	case errors.As(err, &required):
		return &musterAnswer{urlRequired: required.Elicitations}, nil
	case err != nil:
		return nil, fmt.Errorf("muster call_tool %s: %w", tool, err)
	case res.NeedsInput():
		return &musterAnswer{input: res.InputRequests, requestState: res.RequestState}, nil
	}
	var texts []string
	for _, c := range res.Content {
		if text, ok := mcp.AsTextContent(c); ok {
			texts = append(texts, text.Text)
		}
	}
	result, err := parseMusterToolResult(strings.Join(texts, "\n"), res.IsError)
	if err != nil {
		return nil, fmt.Errorf("muster call_tool %s: %w", tool, err)
	}
	return &musterAnswer{result: result}, nil
}

// urlElicitations are the answer's URL mode elicitations, from either
// generation's shape.
func (a *musterAnswer) urlElicitations() []mcp.ElicitationParams {
	out := append([]mcp.ElicitationParams(nil), a.urlRequired...)
	for _, request := range a.input {
		if request.Method == mcp.MethodElicitationCreate && request.Elicitation != nil && request.Elicitation.Mode == mcp.ElicitationModeURL {
			out = append(out, *request.Elicitation)
		}
	}
	return out
}

// musterToolResult is the target tool's result as call_tool serialises it
// into its text: every content block with its type, structuredContent as
// sent, and the tool's isError.
type musterToolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URI  string `json:"uri"`
		Name string `json:"name"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

// parseMusterToolResult reads call_tool's text. A refusal by muster itself
// (an unknown tool, one outside the caller's toolset) is plain text with
// call_tool's isError: it comes back as an error result of that text.
func parseMusterToolResult(text string, isError bool) (*musterToolResult, error) {
	var r musterToolResult
	if err := json.Unmarshal([]byte(text), &r); err == nil && len(r.Content) > 0 {
		if string(r.StructuredContent) == "null" {
			r.StructuredContent = nil
		}
		return &r, nil
	}
	if isError && strings.TrimSpace(text) != "" {
		r = musterToolResult{IsError: true}
		r.Content = append(r.Content, struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URI  string `json:"uri"`
			Name string `json:"name"`
		}{Type: toolResultText, Text: text})
		return &r, nil
	}
	if strings.TrimSpace(text) == "" {
		return nil, errEmptyToolAnswer
	}
	return nil, fmt.Errorf("unexpected call_tool payload shape: %s", excerpt(text, 300))
}

// text is the result's first text block, where a tool's JSON answer is.
func (r *musterToolResult) text() string {
	for _, c := range r.Content {
		if c.Type == toolResultText {
			return c.Text
		}
	}
	return ""
}

// resourceLinks are the URIs of the result's resource_link blocks.
func (r *musterToolResult) resourceLinks() []string {
	var out []string
	for _, c := range r.Content {
		if c.Type == "resource_link" && c.URI != "" {
			out = append(out, c.URI)
		}
	}
	return out
}
