package lab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// proFakeClient posts JSON-RPC to a fake pro under test with alice's bearer.
type proFakeClient struct {
	t      *testing.T
	srv    *httptest.Server
	bearer string
}

func newProFakeClient(t *testing.T) *proFakeClient {
	t.Helper()
	srv := httptest.NewServer(newProFake().handler())
	t.Cleanup(srv.Close)
	return &proFakeClient{t: t, srv: srv, bearer: unsignedJWT(map[string]any{"email": "alice@example.com"})}
}

func (c *proFakeClient) post(body string) (*http.Response, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.srv.URL+proFakeMCPPath, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp, parsed
}

// callTool runs tools/call and returns the tool's result.
func (c *proFakeClient) callTool(name string, args map[string]any) map[string]any {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{jsonRPCKey: jsonRPCVersion, "id": 7, "method": "tools/call", "params": map[string]any{nameKey: name, "arguments": args}})
	resp, parsed := c.post(string(body))
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("tools/call %s answered %d", name, resp.StatusCode)
	}
	result, _ := parsed["result"].(map[string]any)
	if result == nil {
		c.t.Fatalf("tools/call %s answered no result: %v", name, parsed)
	}
	return result
}

// payload decodes a successful result's JSON text, which must equal its
// structuredContent.
func payload[T any](t *testing.T, result map[string]any) T {
	t.Helper()
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("the tool failed: %v", result[toolResultContent])
	}
	content, _ := result[toolResultContent].([]any)
	first, _ := content[0].(map[string]any)
	var v T
	if err := json.Unmarshal([]byte(first[toolResultText].(string)), &v); err != nil {
		t.Fatalf("the text is no JSON payload: %v", err)
	}
	var text any
	_ = json.Unmarshal([]byte(first[toolResultText].(string)), &text)
	if !reflect.DeepEqual(result["structuredContent"], text) {
		t.Errorf("structuredContent %v differs from the text %s", result["structuredContent"], first[toolResultText])
	}
	return v
}

// TestProFakeRefusesTokenless: muster's token-less probe gets pro's 401; a
// GET (no server stream) 405.
func TestProFakeRefusesTokenless(t *testing.T) {
	c := newProFakeClient(t)
	c.bearer = ""
	resp, _ := c.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("a token-less initialize answered %d %q, want 401 with a Bearer challenge", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	get, err := http.Get(c.srv.URL + proFakeMCPPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET %s answered %d, want 405", proFakeMCPPath, get.StatusCode)
	}
}

// TestProFakeHandshake: initialize echoes the client's revision and
// declares tools, a notification is accepted without a body, tools/list
// names the read tools muster aggregates as x_pro_*.
func TestProFakeHandshake(t *testing.T) {
	c := newProFakeClient(t)
	_, init := c.post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	result, _ := init["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" || result["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("initialize answered %v", init)
	}
	if resp, _ := c.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Errorf("a notification answered %d, want 202", resp.StatusCode)
	}
	_, list := c.post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, tool := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)[nameKey].(string))
	}
	want := []string{"get_board_schema", "get_issue_details", "get_item_by_issue", "get_parent_issue", proToolListIssues, "list_sub_issues"}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Errorf("tools/list = %v, want %v", names, want)
	}
	if _, unknown := c.post(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`); unknown[rpcErrorKey] == nil {
		t.Errorf("an unknown method answered %v, want a JSON-RPC error", unknown)
	}
}

// TestProFakeListIssues: the filters the roadmap-backend sends select the
// fixture's items as pro's would.
func TestProFakeListIssues(t *testing.T) {
	c := newProFakeClient(t)
	type answer struct {
		Issues     []proBoardItem `json:"issues"`
		TotalCount int            `json:"totalCount"`
		Truncated  bool           `json:"truncated"`
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"whole board", map[string]any{}, fixtureTitles(nil)},
		{"team", map[string]any{argFilters: map[string]any{"Team": proFakeTeam}}, fixtureTitles(teamIs(proFakeTeam))},
		{"status, case-insensitive", map[string]any{argFilters: map[string]any{"Status": "in progress"}}, []string{proFakeEpic, "Phoenix storage migration"}},
		{"without a status", map[string]any{"emptyFields": []any{"Status"}}, []string{"Triage the cluster feedback"}},
		{"closed", map[string]any{argState: "closed"}, []string{"Cluster creation through muster"}},
		{argKeyword, map[string]any{argKeyword: "cluster portal"}, []string{"Cluster templates in the portal"}},
		{"quarter", map[string]any{argKeyword: `quarter:"Q4 2026"`}, nil},
		{argRepository, map[string]any{argRepository: "giantswarm/other"}, nil},
		{"updated since a time", map[string]any{argUpdated: ">=2006-01-02T15:04:05Z"}, fixtureTitles(nil)},
		{"updated on the day", map[string]any{argUpdated: "2026-10-05"}, fixtureTitles(nil)},
		{"updated by the day's end", map[string]any{argUpdated: "<=2026-10-05"}, fixtureTitles(nil)},
		{"updated after the day", map[string]any{argUpdated: ">2026-10-05"}, nil},
		{"updated before the time", map[string]any{argUpdated: "<2026-10-05T08:00:00Z"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{argBoard: proFakeBoard, "limit": 0}
			for k, v := range tc.args {
				args[k] = v
			}
			got := payload[answer](t, c.callTool(proToolListIssues, args))
			if err := sameTitles(tc.name, itemTitles(got.Issues), tc.want); err != nil {
				t.Error(err)
			}
			if got.TotalCount != len(tc.want) || got.Truncated {
				t.Errorf("totalCount %d truncated %v, want %d and false", got.TotalCount, got.Truncated, len(tc.want))
			}
		})
	}
	cut := payload[answer](t, c.callTool(proToolListIssues, map[string]any{argBoard: proFakeBoard, "limit": 2}))
	if len(cut.Issues) != 2 || !cut.Truncated || cut.TotalCount != len(proFakeItems()) {
		t.Errorf("limit 2 answered %d items, truncated %v, total %d", len(cut.Issues), cut.Truncated, cut.TotalCount)
	}
}

// TestProFakeReads: the schema, the item lookups and the epic's sub-issue
// links; another board and the writes are refused as tool errors.
func TestProFakeReads(t *testing.T) {
	c := newProFakeClient(t)
	schema := payload[struct {
		Fields []proBoardField `json:"fields"`
	}](t, c.callTool("get_board_schema", map[string]any{argBoard: proFakeBoard}))
	if len(schema.Fields) != 3 || schema.Fields[0].Name != boardFieldStatus {
		t.Errorf("get_board_schema = %+v", schema.Fields)
	}
	byIssue := payload[struct {
		Item *proBoardItem `json:"item"`
	}](t, c.callTool("get_item_by_issue", map[string]any{argBoard: proFakeBoard, argOwner: proFakeOwner, argRepo: proFakeRepoName, argIssueNumber: 1}))
	if byIssue.Item == nil || byIssue.Item.Title != proFakeEpic {
		t.Errorf("get_item_by_issue #1 = %+v", byIssue.Item)
	}
	details := payload[map[string]any](t, c.callTool("get_issue_details", map[string]any{"itemId": byIssue.Item.ID}))
	if details["body"] == "" || details["title"] != proFakeEpic || details[resultItem] != nil {
		t.Errorf("get_issue_details must return pro's flat detail object: %v", details)
	}
	fields, ok := details["fields"].([]any)
	if !ok || len(fields) != len(byIssue.Item.Fields) {
		t.Fatalf("get_issue_details fields = %v, want pro's field-value array", details["fields"])
	}
	for _, field := range fields {
		value := field.(map[string]any)
		if byIssue.Item.Fields[value["name"].(string)] != value["value"] {
			t.Errorf("get_issue_details field differs from the board item: %v", value)
		}
	}
	repository, ok := details["repository"].(map[string]any)
	if !ok || repository["nameWithOwner"] != proFakeRoadmapRepo {
		t.Errorf("get_issue_details repository = %v, want pro's repository metadata", details["repository"])
	}
	subs := payload[struct {
		Subs []proCompactIssue `json:"sub_issues"`
	}](t, c.callTool("list_sub_issues", map[string]any{argOwner: proFakeOwner, argRepo: proFakeRepoName, argIssueNumber: 1}))
	if len(subs.Subs) != 2 {
		t.Errorf("the epic's sub-issues = %+v, want 2", subs.Subs)
	}
	parent := payload[struct {
		Parent *proCompactIssue `json:"parent"`
	}](t, c.callTool("get_parent_issue", map[string]any{argOwner: proFakeOwner, argRepo: proFakeRepoName, argIssueNumber: 2}))
	if parent.Parent == nil || parent.Parent.Number != 1 {
		t.Errorf("get_parent_issue #2 = %+v, want #1", parent.Parent)
	}
	for _, refused := range []struct {
		tool string
		args map[string]any
	}{
		{proToolListIssues, map[string]any{argBoard: "customer"}},
		{"update_issue_field", map[string]any{argBoard: proFakeBoard}},
		{proToolListIssues, map[string]any{argBoard: proFakeBoard, argUpdated: "last week"}},
	} {
		if r := c.callTool(refused.tool, refused.args); r["isError"] != true {
			t.Errorf("%s %v answered %v, want a tool error", refused.tool, refused.args, r)
		}
	}
}

// TestProFakeRecordsCaller: every call is recorded with the e-mail of the
// bearer it came with.
func TestProFakeRecordsCaller(t *testing.T) {
	c := newProFakeClient(t)
	c.callTool(proToolListIssues, map[string]any{argBoard: proFakeBoard, argFilters: map[string]any{"Team": proFakeTeam}})
	resp, err := http.Get(c.srv.URL + proFakeCallsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var calls []proFakeCall
	if err := json.NewDecoder(resp.Body).Decode(&calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Tool != proToolListIssues || calls[0].Caller != "alice@example.com" {
		t.Errorf("calls = %+v, want one list_issues as alice@example.com", calls)
	}
}

// TestBackstageHive pins backstage.hive: on, the lab's values enable Hive's
// extensions and the overlay wires the roadmap-backend to the board of the
// muster server pro; off (the default), neither.
func TestBackstageHive(t *testing.T) {
	hive := []string{"page:plans", "api:plans", "page:roadmap", "api:roadmap", "page:plans/plans-redirect", "page:plans/magazine"}
	for _, on := range []bool{false, true} {
		cfg := config.Default()
		cfg.Backstage.Hive = on
		raw, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var values struct {
			Backstage struct {
				EnabledExtensions []string `yaml:"enabledExtensions"`
			} `yaml:"backstage"`
		}
		if err := yaml.Unmarshal(raw, &values); err != nil {
			t.Fatalf("values are not YAML: %v", err)
		}
		for _, ext := range hive {
			if slices.Contains(values.Backstage.EnabledExtensions, ext) != on {
				t.Errorf("hive %v: enabledExtensions %v, %s listed: %v", on, values.Backstage.EnabledExtensions, ext, !on)
			}
		}
		roadmap := dig(backstageOverlayAppConfig(t, cfg), proFakeRepoName)
		if !on {
			if roadmap != nil {
				t.Errorf("hive off: the overlay carries roadmap %v", roadmap)
			}
			continue
		}
		m, _ := roadmap.(map[string]any)
		if m[argBoard] != proFakeBoard || dig(m, "muster", "server") != proFakeServer || dig(m, "muster", "installation") != platformRelease {
			t.Errorf("hive on: roadmap = %v, want board %s on muster server %s of installation %s", m, proFakeBoard, proFakeServer, platformRelease)
		}
		if teams, _ := m["teams"].([]any); len(teams) != 1 || teams[0] != proFakeTeam {
			t.Errorf("hive on: roadmap.teams = %v, want [%s]", m["teams"], proFakeTeam)
		}
	}
}
