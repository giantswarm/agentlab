package lab

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fixture pins Dex as its authorization server with the platform client,
// and ships the Secret that client is read from.
func TestOAuthFixtureRenderPinsDex(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, "oauth-fixture.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"authorizationServer:",
		"issuer: " + cfg.MusterBaseURL(),
		"authorizationEndpoint: " + cfg.Issuer() + "/auth",
		"tokenEndpoint: " + cfg.Issuer() + "/token",
		"name: " + oauthFixtureServer + "-client",
		"scopes: openid profile email offline_access",
		"kind: Secret",
		"client-id: " + config.AgentPlatformClientID,
		"client-secret: " + config.AgentPlatformClientSecret,
		"forwardToken: false",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered fixture lacks %q:\n%s", want, s)
		}
	}
}

// Dex lists muster's proxy callback on the platform client, next to the
// server-role callback and Backstage's handler.
func TestDexRenderRegistersProxyCallback(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		cfg.MusterBaseURL() + oauthProxyCallbackPath,
		cfg.MusterBaseURL() + "/oauth/callback",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered Dex config lacks the redirect URI %q", want)
		}
	}
}

func TestToolNamesInReply(t *testing.T) {
	cases := []struct {
		reply string
		want  []string
	}{
		{"x_mcp-kubernetes_list\nx_mcp-kubernetes_get\nworkflow_lab-cluster-overview", []string{"workflow_lab-cluster-overview", "x_mcp-kubernetes_get", "x_mcp-kubernetes_list"}},
		{"Here are the tools:\n- `x_mcp-kubernetes_list`\n- `core_workflow_list`", []string{"core_workflow_list", "x_mcp-kubernetes_list"}},
		{"x_a_b, x_a_b, x_c_d", []string{"x_a_b", "x_c_d"}},
		{"NO_TOOLS", nil},
		{"I have no tools available.", nil},
	}
	for _, tc := range cases {
		if got := toolNamesInReply(tc.reply); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toolNamesInReply(%q) = %v, want %v", tc.reply, got, tc.want)
		}
	}
}

// preset:read-only is the readOnlyHint predicate over the catalogue, kind
// included: an annotated core read belongs to it, an unannotated core write,
// a workflow without the derived hint and an un-annotated server tool do not.
func TestReadOnlySetMismatch(t *testing.T) {
	yes, no := true, false
	ro := &toolAnnotations{ReadOnlyHint: &yes}
	rw := &toolAnnotations{ReadOnlyHint: &no}
	kindTool, kindWorkflow, kindCore := "tool", "workflow", "core"
	list, writer, query, mutating := "x_k8s_list", "x_manager_delete", "workflow_query", "workflow_mutating"
	coreRead, coreWrite := "core_config_get", "core_workflow_delete"
	catalogue := []toolInfo{
		{Name: list, Kind: kindTool, Annotations: ro},
		{Name: writer, Kind: kindTool, Annotations: rw},
		{Name: "x_legacy_probe", Kind: kindTool},
		{Name: query, Kind: kindWorkflow, Annotations: ro},
		{Name: mutating, Kind: kindWorkflow},
		{Name: coreRead, Kind: kindCore, Annotations: ro},
		{Name: coreWrite, Kind: kindCore, Annotations: rw},
		{Name: "core_unannotated", Kind: kindCore},
	}
	var resolved []toolInfo
	for _, tool := range catalogue {
		if tool.Annotations.readOnly() {
			resolved = append(resolved, tool)
		}
	}
	if missing, extra := readOnlySetMismatch(catalogue, resolved); len(missing)+len(extra) != 0 {
		t.Fatalf("the annotated tools themselves mismatch: missing %v, extra %v", missing, extra)
	}
	if got, want := (&filterToolsResponse{Tools: resolved}).names(), []string{coreRead, query, list}; !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved %v, want %v", got, want)
	}

	// The pre-5.13 rule (kind core never in the preset) would leave the
	// annotated read out; a preset carrying a write has an extra.
	missing, extra := readOnlySetMismatch(catalogue, []toolInfo{catalogue[0], catalogue[3], catalogue[6]})
	if !reflect.DeepEqual(missing, []string{coreRead}) || !reflect.DeepEqual(extra, []string{coreWrite}) {
		t.Errorf("missing %v, extra %v; want [%s] and [%s]", missing, extra, coreRead, coreWrite)
	}

	// A catalogue whose core tools carry no annotations puts none in the
	// preset — an empty share of core tools is then not a mismatch.
	bare := []toolInfo{catalogue[0], catalogue[3], {Name: coreRead, Kind: kindCore}, {Name: coreWrite, Kind: kindCore}}
	if missing, extra := readOnlySetMismatch(bare, bare[:2]); len(missing)+len(extra) != 0 {
		t.Errorf("bare core tools: missing %v, extra %v", missing, extra)
	}
}

// What the read-only agent reports is judged against the resolved set: a
// read-only core tool is fine, a catalogue tool outside the set is the
// missing header, a name the catalogue never had is noise.
func TestNamesOutside(t *testing.T) {
	coreRead, coreWrite, mutating, list, made := "core_config_get", "core_workflow_delete", "workflow_mutating", "x_k8s_list", "x_made_up"
	toolset := []string{coreRead, "workflow_query", list}
	catalogue := append([]string{coreWrite, mutating, "x_manager_delete"}, toolset...)
	outside, unknown := namesOutside([]string{coreRead, coreWrite, list, made, mutating}, toolset, catalogue)
	if !reflect.DeepEqual(outside, []string{coreWrite, mutating}) {
		t.Errorf("outside = %v", outside)
	}
	if !reflect.DeepEqual(unknown, []string{made}) {
		t.Errorf("unknown = %v", unknown)
	}
	if outside, unknown := namesOutside(toolset, toolset, catalogue); len(outside)+len(unknown) != 0 {
		t.Errorf("the toolset itself: outside %v, unknown %v", outside, unknown)
	}
}

// The composed manifest is the composer's shape: OCIRepository first, then
// the HelmRelease with the top-level toolset value.
func TestComposeAgentManifest(t *testing.T) {
	m := composeAgentManifest("probe", "default-model-config", []string{presetReadOnly, "workflow:incident-triage"})
	docs := strings.Split(m, "\n---\n")
	if len(docs) != 2 || !strings.Contains(docs[0], "kind: OCIRepository") || !strings.Contains(docs[1], "kind: HelmRelease") {
		t.Fatalf("wanted OCIRepository then HelmRelease, got:\n%s", m)
	}
	for _, want := range []string{
		"url: oci://gsoci.azurecr.io/charts/giantswarm/agent",
		"semver: x.x.x",
		"name: probe\n  namespace: kagent",
		`toolset: ["preset:read-only", "workflow:incident-triage"]`,
		"modelConfig:\n      name: default-model-config",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
}

func TestSessionIDInChallenge(t *testing.T) {
	state := base64.RawURLEncoding.EncodeToString([]byte(`{"session_id":"ext-0123abcd","user_id":"u","server_name":"lab-oauth-fixture"}`))
	got := sessionIDInChallenge("https://muster.127.0.0.1.nip.io/oauth/proxy/start?state=" + state)
	if got != "ext-0123abcd" {
		t.Errorf("sessionIDInChallenge = %q", got)
	}
	if got := sessionIDInChallenge("https://muster.127.0.0.1.nip.io/oauth/proxy/start"); got != "" {
		t.Errorf("no state must give no session id, got %q", got)
	}
}

// A streamed MCP answer may carry a notification frame before the response;
// the response frame is the one that counts.
func TestParseMCPResponsePicksTheResponseFrame(t *testing.T) {
	raw := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n")
	res, err := parseMCPResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if innerText(res) != "ok" {
		t.Fatalf("picked the wrong frame: %v", res)
	}
	errRaw := []byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\ndata: {\"jsonrpc\":\"2.0\",\"id\":3,\"error\":{\"code\":-1,\"message\":\"boom\"}}\n")
	res, err = parseMCPResponse(errRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res["error"]; !ok {
		t.Fatalf("error frame not picked: %v", res)
	}
	if _, err := parseMCPResponse([]byte("nothing here")); err == nil {
		t.Fatal("garbage must fail")
	}
}
