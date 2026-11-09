package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fake pro: a pro-compatible MCP server serving a board fixture, which
// muster aggregates as the MCPServer `pro` so the portal's roadmap-backend
// reads a board through muster as the signed-in person, the way it reads
// GitHub's on an installation (docs/backstage.md "The roadmap board fixture").
// It answers pro's board reads with pro's shapes (list_issues,
// get_board_schema, get_item_by_issue, get_issue_details, list_sub_issues,
// get_parent_issue) and refuses every write: the fixture is the same board
// on every run. Every call is recorded with the e-mail of the bearer muster
// forwarded, so a proof reads back that the call ran as the person. The
// bearer is read, never verified: muster verified it before forwarding.
//
// It runs as a container on the kind network (fakecontainer.go), the
// agentlab binary's hidden `pro-fake` command, and speaks MCP's
// Streamable HTTP transport in its stateless JSON form: every POST is one
// JSON-RPC request answered with one JSON response, no session id, no SSE.
const (
	// proFakeServer is the MCPServer muster aggregates the fake under, the
	// name an installation registers pro by: its tools are x_pro_<tool>.
	proFakeServer = "pro"
	// proFakeService is the selector-less Service in front of the fake's
	// container.
	proFakeService = "agentlab-pro"
	// proFakeCommand is the hidden command the container runs.
	proFakeCommand = "pro-fake"
	// proFakeBoard is the one board the fixture serves, the key the portal's
	// roadmap.board names.
	proFakeBoard = "roadmap"
	// The fake's paths: MCP, its health (answering "ok" for the preflight
	// pod) and the record of the calls it answered.
	proFakeMCPPath    = "/mcp"
	proFakeHealthPath = "/healthz"
	proFakeCallsPath  = "/calls"
	// The fixture's teams: the portal's default team and another one, which
	// a team filter leaves out.
	proFakeTeam      = "Bumblebee🐝"
	proFakeOtherTeam = "Phoenix"
)

// The board fields the fixture's items carry, by pro's names.
const (
	boardFieldStatus = "Status"
	boardFieldKind   = "Kind"
	boardFieldTeam   = "Team"
)

// proBoardField is one field of get_board_schema's answer.
type proBoardField struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
}

// proBoardItem is a board item the way pro's list_issues returns it.
type proBoardItem struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Number    int               `json:"number"`
	URL       string            `json:"url"`
	Repo      string            `json:"repo"`
	Private   bool              `json:"private"`
	State     string            `json:"state"`
	CreatedAt string            `json:"createdAt"`
	UpdatedAt string            `json:"updatedAt"`
	Assignees []string          `json:"assignees"`
	Labels    []string          `json:"labels"`
	Fields    map[string]string `json:"fields"`
	// Body and Parent are the fixture's own: get_issue_details answers the
	// body, the sub-issue tools the parent link; list_issues leaves both out.
	Body   string `json:"-"`
	Parent int    `json:"-"`
}

// proCompactIssue is an issue the way pro's sub-issue tools answer it.
type proCompactIssue struct {
	ID         int      `json:"id"`
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	Repository string   `json:"repository"`
	Assignees  []string `json:"assignees"`
}

// proFakeSchema is the fixture board's schema.
var proFakeSchema = []proBoardField{
	{Name: boardFieldStatus, Type: fieldSingleSelect, Options: []string{"Inbox", "Backlog", "Up Next", "In Progress", "Validation", "Done"}},
	{Name: boardFieldKind, Type: fieldSingleSelect, Options: []string{"Epic", "Story", "Task"}},
	{Name: boardFieldTeam, Type: fieldSingleSelect, Options: []string{proFakeTeam, proFakeOtherTeam}},
}

// The repository the fixture's issues live in.
const (
	proFakeOwner       = "giantswarm"
	proFakeRepoName    = "roadmap"
	proFakeRoadmapRepo = proFakeOwner + "/" + proFakeRepoName
)

// The tool arguments, answer keys and field type the fake shares with pro.
const (
	proToolListIssues = "list_issues"
	argBoard          = "board"
	argFilters        = "filters"
	argKeyword        = "keyword"
	argState          = "state"
	argRepository     = "repository"
	argUpdated        = "updated"
	argOwner          = "owner"
	argRepo           = "repo"
	argIssueNumber    = "issue_number"
	resultItem        = "item"
	fieldSingleSelect = "singleSelect"
	jsonRPCKey        = "jsonrpc"
	jsonRPCVersion    = "2.0"
	rpcErrorKey       = "error"
)

// proFakeEpic is the fixture's epic, the item the portal's Hive spec looks
// for by title.
const proFakeEpic = "Self-service clusters"

// proFakeItems is the fixture board: an epic in progress with two
// sub-issues (one up next, one done), an item without a status, and another
// team's item. Titles are unique, so a proof finds each by its title.
func proFakeItems() []proBoardItem {
	const at = "2026-10-05T08:00:00Z"
	item := func(n int, title, status, kind, team, state string, parent int) proBoardItem {
		fields := map[string]string{boardFieldKind: kind, boardFieldTeam: team}
		if status != "" {
			fields[boardFieldStatus] = status
		}
		return proBoardItem{
			ID: fmt.Sprintf("PVTI_agentlab_%d", n), Title: title, Number: n,
			URL:  fmt.Sprintf("https://github.com/%s/issues/%d", proFakeRoadmapRepo, n),
			Repo: proFakeRoadmapRepo, State: state, CreatedAt: at, UpdatedAt: at,
			Assignees: []string{}, Labels: []string{}, Fields: fields,
			Body:   "A board fixture item of agentlab's fake pro; no such issue exists.",
			Parent: parent,
		}
	}
	return []proBoardItem{
		item(1, proFakeEpic, "In Progress", "Epic", proFakeTeam, "OPEN", 0),
		item(2, "Cluster templates in the portal", "Up Next", "Story", proFakeTeam, "OPEN", 1),
		item(3, "Cluster creation through muster", "Done", "Story", proFakeTeam, "CLOSED", 1),
		item(4, "Triage the cluster feedback", "", "Task", proFakeTeam, "OPEN", 0),
		item(5, "Phoenix storage migration", "In Progress", "Story", proFakeOtherTeam, "OPEN", 0),
	}
}

// proFakeCall is one tools/call the fake answered: the tool, its arguments
// and the e-mail claim of the bearer it came with.
type proFakeCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Caller    string         `json:"caller"`
}

// proFake is the fake's state: the fixture and the calls it answered.
type proFake struct {
	items []proBoardItem

	mu    sync.Mutex
	calls []proFakeCall
}

func newProFake() *proFake { return &proFake{items: proFakeItems()} }

// ServeFakePro serves the fake pro on addr until ctx ends.
func ServeFakePro(ctx context.Context, addr string) error {
	f := newProFake()
	srv := &http.Server{Addr: addr, Handler: f.handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("fake pro on %s (board %s, %d items)\n", ln.Addr(), proFakeBoard, len(f.items))
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (f *proFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(proFakeHealthPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc(proFakeCallsPath, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		calls := slices.Clone(f.calls)
		f.mu.Unlock()
		writeProJSON(w, http.StatusOK, calls)
	})
	mux.HandleFunc(proFakeMCPPath, f.serveMCP)
	return mux
}

// jsonRPCRequest is one JSON-RPC request or notification (no id).
type jsonRPCRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// The JSON-RPC error codes the fake answers.
const (
	jsonRPCParseError     = -32700
	jsonRPCMethodNotFound = -32601
	jsonRPCInvalidParams  = -32602
)

// proFakeProtocolVersion is the MCP revision the fake answers when the
// client names none.
const proFakeProtocolVersion = "2025-06-18"

// serveMCP answers one Streamable HTTP POST. Without a bearer the answer is
// 401, as pro's is: muster forwards the person's token on every call
// (auth.forwardToken), so a token-less request is muster's probe.
func (f *proFake) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-to-client stream and no session to end.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "the fake pro answers POST only", http.StatusMethodNotAllowed)
		return
	}
	caller, ok := bearerEmail(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agentlab-pro-fake"`)
		http.Error(w, "a bearer token is required", http.StatusUnauthorized)
		return
	}
	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProJSON(w, http.StatusOK, rpcError(nil, jsonRPCParseError, "not a JSON-RPC request: "+err.Error()))
		return
	}
	if len(req.ID) == 0 {
		// A notification (notifications/initialized and the like).
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = proFakeProtocolVersion
		}
		writeProJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{nameKey: "agentlab-pro-fake", "version": "1"},
		}))
	case "ping":
		writeProJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{}))
	case "tools/list":
		writeProJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{"tools": proFakeTools()}))
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			writeProJSON(w, http.StatusOK, rpcError(req.ID, jsonRPCInvalidParams, "tools/call needs a tool name"))
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, proFakeCall{Tool: params.Name, Arguments: params.Arguments, Caller: caller})
		f.mu.Unlock()
		writeProJSON(w, http.StatusOK, rpcResult(req.ID, proToolResult(f.call(params.Name, params.Arguments))))
	default:
		writeProJSON(w, http.StatusOK, rpcError(req.ID, jsonRPCMethodNotFound, "the fake pro has no method "+req.Method))
	}
}

// bearerEmail reads the e-mail claim of the bearer JWT, unverified, and
// whether a bearer came at all; a bearer without one is "unknown".
func bearerEmail(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", false
	}
	claims, err := decodeJWTClaims(strings.TrimSpace(token))
	if err != nil {
		return "unknown", true
	}
	if email, _ := claims["email"].(string); email != "" {
		return email, true
	}
	return "unknown", true
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{jsonRPCKey: jsonRPCVersion, "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{jsonRPCKey: jsonRPCVersion, "id": id, rpcErrorKey: map[string]any{"code": code, "message": message}}
}

// proToolResult wraps a tool's payload, or its error, as MCP's CallToolResult:
// the payload as JSON text and as structuredContent, as pro answers.
func proToolResult(payload any, err error) map[string]any {
	if err != nil {
		return map[string]any{"isError": true, toolResultContent: []map[string]any{{fieldTypeKey: toolResultText, toolResultText: err.Error()}}}
	}
	raw, _ := json.Marshal(payload)
	return map[string]any{
		toolResultContent:   []map[string]any{{fieldTypeKey: toolResultText, toolResultText: string(raw)}},
		"structuredContent": payload,
	}
}

func writeProJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// proFakeTool is one tool of tools/list.
type proFakeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

// proFakeTools are the read tools the fake serves, with the arguments the
// roadmap-backend sends.
func proFakeTools() []proFakeTool {
	str := map[string]any{fieldTypeKey: "string"}
	num := map[string]any{fieldTypeKey: "number"}
	tool := func(name, description string, required []string, props map[string]any) proFakeTool {
		return proFakeTool{Name: name, Description: description + " (agentlab's fake pro: a fixture board)",
			InputSchema: map[string]any{fieldTypeKey: "object", "properties": props, "required": required},
			Annotations: map[string]any{"readOnlyHint": true}}
	}
	issue := map[string]any{argOwner: str, argRepo: str, argIssueNumber: num}
	return []proFakeTool{
		tool(proToolListIssues, "List the items of a board, filtered by field values", []string{argBoard}, map[string]any{
			argBoard: str, argFilters: map[string]any{fieldTypeKey: "object", "additionalProperties": str},
			"emptyFields": map[string]any{fieldTypeKey: "array", "items": str}, "limit": num,
			argKeyword: str, argState: str, "assignee": str, argRepository: str, argUpdated: str,
		}),
		tool("get_board_schema", "The fields of a board and their options", []string{argBoard}, map[string]any{argBoard: str}),
		tool("get_item_by_issue", "The board item of an issue", []string{argBoard, argOwner, argRepo, argIssueNumber},
			map[string]any{argBoard: str, argOwner: str, argRepo: str, argIssueNumber: num}),
		tool("get_issue_details", "A board item with its issue's body", []string{"itemId"}, map[string]any{"itemId": str}),
		tool("list_sub_issues", "The sub-issues of an issue", []string{argOwner, argRepo, argIssueNumber},
			map[string]any{argOwner: str, argRepo: str, argIssueNumber: num, "per_page": num}),
		tool("get_parent_issue", "The parent issue of an issue", []string{argOwner, argRepo, argIssueNumber}, issue),
	}
}

// call answers one tool call.
func (f *proFake) call(tool string, args map[string]any) (any, error) {
	switch tool {
	case proToolListIssues:
		if err := requireBoard(args); err != nil {
			return nil, err
		}
		return f.listIssues(args)
	case "get_board_schema":
		if err := requireBoard(args); err != nil {
			return nil, err
		}
		return map[string]any{"fields": proFakeSchema}, nil
	case "get_item_by_issue":
		if err := requireBoard(args); err != nil {
			return nil, err
		}
		item := f.byIssue(args)
		if item == nil {
			return map[string]any{resultItem: nil}, nil
		}
		return map[string]any{resultItem: item}, nil
	case "get_issue_details":
		id, _ := args["itemId"].(string)
		for _, item := range f.items {
			if item.ID == id {
				return map[string]any{resultItem: item, "body": item.Body}, nil
			}
		}
		return nil, fmt.Errorf("item %q not found on board %s", id, proFakeBoard)
	case "list_sub_issues":
		parent := f.byIssue(args)
		subs := []proCompactIssue{}
		if parent != nil {
			for _, item := range f.items {
				if item.Parent == parent.Number {
					subs = append(subs, compact(item))
				}
			}
		}
		return map[string]any{"sub_issues": subs}, nil
	case "get_parent_issue":
		child := f.byIssue(args)
		if child != nil && child.Parent != 0 {
			for _, item := range f.items {
				if item.Number == child.Parent {
					return map[string]any{"parent": compact(item)}, nil
				}
			}
		}
		return map[string]any{"parent": nil}, nil
	default:
		return nil, fmt.Errorf("the fake pro serves the board reads only; %s is not one of them (%s)", tool,
			strings.Join(slices.Sorted(maps.Keys(proFakeToolNames())), ", "))
	}
}

func proFakeToolNames() map[string]bool {
	names := map[string]bool{}
	for _, t := range proFakeTools() {
		names[t.Name] = true
	}
	return names
}

func requireBoard(args map[string]any) error {
	if board, _ := args[argBoard].(string); board != proFakeBoard {
		return fmt.Errorf("board %q not found: the fake pro serves the board %q only", board, proFakeBoard)
	}
	return nil
}

func compact(item proBoardItem) proCompactIssue {
	return proCompactIssue{ID: 1000 + item.Number, Number: item.Number, Title: item.Title, URL: item.URL,
		State: strings.ToLower(item.State), Repository: item.Repo, Assignees: item.Assignees}
}

// byIssue is the fixture item of the owner/repo/issue_number the arguments
// name, nil for an issue the board does not carry.
func (f *proFake) byIssue(args map[string]any) *proBoardItem {
	owner, _ := args[argOwner].(string)
	repo, _ := args[argRepo].(string)
	number, _ := args[argIssueNumber].(float64)
	for i := range f.items {
		if f.items[i].Repo == owner+"/"+repo && f.items[i].Number == int(number) {
			return &f.items[i]
		}
	}
	return nil
}

// listIssues filters the fixture the way pro's list_issues does for the
// arguments the roadmap-backend sends: field values (case-insensitive), the
// fields that must be empty, the state, an assignee, the repository and the
// keyword's words in the title (a quarter:"…" term matches no fixture item,
// which has no quarter), the updated qualifier (updatedRange). limit 0 reads every item; a limit below the count
// cuts the list and says so.
func (f *proFake) listIssues(args map[string]any) (map[string]any, error) {
	filters, _ := args[argFilters].(map[string]any)
	var empty []string
	if list, ok := args["emptyFields"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				empty = append(empty, s)
			}
		}
	}
	state, _ := args[argState].(string)
	assignee, _ := args["assignee"].(string)
	repository, _ := args[argRepository].(string)
	keyword, _ := args[argKeyword].(string)
	updated, _ := args[argUpdated].(string)
	since, until, err := updatedRange(updated)
	if err != nil {
		return nil, err
	}
	matched := []proBoardItem{}
	for _, item := range f.items {
		if itemMatches(item, filters, empty, state, assignee, repository, keyword) && updatedWithin(item, since, until) {
			matched = append(matched, item)
		}
	}
	total := len(matched)
	truncated := false
	if limit, _ := args["limit"].(float64); limit > 0 && int(limit) < total {
		matched, truncated = matched[:int(limit)], true
	}
	return map[string]any{"issues": matched, "totalCount": total, "hidden": 0, "truncated": truncated}, nil
}

// updatedRange reads list_issues' updated qualifier the way GitHub's search
// reads one: >=, >, <= or < before a date (2006-01-02) or a time (RFC 3339),
// or a bare date for that day; "" is no bound. It answers the half-open
// range [since, until) an item's updatedAt must fall in, a zero end being
// open.
func updatedRange(q string) (since, until time.Time, err error) {
	if q == "" {
		return since, until, nil
	}
	parse := func(v string) (time.Time, bool, error) {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, false, nil
		}
		t, err := time.Parse(time.DateOnly, v)
		return t, true, err
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		v, ok := strings.CutPrefix(q, op)
		if !ok {
			continue
		}
		t, day, err := parse(v)
		if err != nil {
			break
		}
		// A date's > and <= take the whole day.
		if day && (op == ">" || op == "<=") {
			t = t.AddDate(0, 0, 1)
		} else if op == ">" || op == "<=" {
			t = t.Add(time.Nanosecond)
		}
		if op[0] == '>' {
			return t, until, nil
		}
		return since, t, nil
	}
	if t, day, err := parse(q); err == nil && day {
		return t, t.AddDate(0, 0, 1), nil
	}
	return since, until, fmt.Errorf("updated %q is no date qualifier: >=, >, <= or < before a date (2006-01-02) or an RFC 3339 time, or a bare date", q)
}

// updatedWithin: the item's updatedAt is in [since, until).
func updatedWithin(item proBoardItem, since, until time.Time) bool {
	at, err := time.Parse(time.RFC3339, item.UpdatedAt)
	if err != nil {
		return false
	}
	return (since.IsZero() || !at.Before(since)) && (until.IsZero() || at.Before(until))
}

func itemMatches(item proBoardItem, filters map[string]any, empty []string, state, assignee, repository, keyword string) bool {
	for field, want := range filters {
		w, _ := want.(string)
		if !strings.EqualFold(item.Fields[field], w) {
			return false
		}
	}
	for _, field := range empty {
		if item.Fields[field] != "" {
			return false
		}
	}
	if state != "" && !strings.EqualFold(item.State, state) {
		return false
	}
	if assignee != "" && !slices.Contains(item.Assignees, assignee) {
		return false
	}
	if repository != "" && !strings.EqualFold(item.Repo, repository) {
		return false
	}
	return keywordMatches(item.Title, keyword)
}

// keywordMatches: every word of the keyword is in the title; a quarter:"…"
// term never matches (the fixture has no Quarter field).
func keywordMatches(title, keyword string) bool {
	title = strings.ToLower(title)
	rest := keyword
	for rest != "" {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if strings.HasPrefix(rest, `quarter:"`) {
			return false
		}
		var word string
		word, rest, _ = strings.Cut(rest, " ")
		if !strings.Contains(title, strings.ToLower(word)) {
			return false
		}
	}
	return true
}

// --- the fake in a container on the kind network -----------------------------

// proFakeContainer is the fake running as a container on the kind network.
type proFakeContainer struct{ *fakeContainer }

// startProFakeContainer runs `<binary> pro-fake` on the kind network.
func startProFakeContainer(cfg *config.Config, binary string) (*proFakeContainer, error) {
	c, err := startFakeContainer(cfg, binary, fakeContainerSpec{
		what: "the fake pro", suffix: proFakeCommand, command: proFakeCommand, healthPath: proFakeHealthPath,
	})
	if err != nil {
		return nil, err
	}
	return &proFakeContainer{fakeContainer: c}, nil
}

// calls reads back the calls the fake answered.
func (c *proFakeContainer) calls() ([]proFakeCall, error) {
	resp, err := c.client.Get(c.hostURL + proFakeCallsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the fake pro answered %s with %s", proFakeCallsPath, resp.Status)
	}
	var calls []proFakeCall
	if err := json.NewDecoder(resp.Body).Decode(&calls); err != nil {
		return nil, fmt.Errorf("the fake pro's %s: %w", proFakeCallsPath, err)
	}
	return calls, nil
}
