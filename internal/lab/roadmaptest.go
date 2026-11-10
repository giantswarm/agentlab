package lab

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The roadmap board proof (docs/backstage.md "The roadmap board fixture"):
// the portal's roadmap-backend reads a board through muster as the person,
// against the fake pro (profake.go) in place of GitHub. The fake serves a
// fixed board; the proof reads it through muster's x_pro_list_issues, then
// through the backend's routes, and reads back from the fake that every call
// came with the person's own token. The Hive board's rendering is the
// portal's own e2e spec, which reads the same routes while `--keep` leaves
// the fixture in place.

// roadmapTeamParam is the roadmap-backend's team query parameter.
const roadmapTeamParam = "team"

// RoadmapTestOptions are roadmap-test's flags.
type RoadmapTestOptions struct {
	// Email is the lab user the proof reads as (default the admin).
	Email string
	// Keep leaves the fake pro, its Service and the MCPServer in place after
	// a passing run, for the portal's Hive spec and a person in the portal;
	// the next run without it removes them.
	Keep bool
}

// RoadmapTest runs the proof.
func RoadmapTest(cfg *config.Config, opts RoadmapTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled {
		return fmt.Errorf("the platform is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	user := cfg.AdminUser()
	if opts.Email != "" {
		user = cfg.FindUser(opts.Email)
	}
	if user == nil {
		return fmt.Errorf("no lab user %q in %s", opts.Email, config.File)
	}
	p := &roadmapProof{cfg: cfg, opts: opts, user: user, stages: newProofStages()}
	err := p.run()
	p.cleanup(err == nil && opts.Keep)
	return p.stages.close(err)
}

// roadmapProof is one run's state; the remove funcs undo what it set up.
type roadmapProof struct {
	cfg    *config.Config
	opts   RoadmapTestOptions
	user   *config.User
	stages *proofStages
	fake   *proFakeContainer
	remove []func()
}

// cleanup removes the fixture, last made first, unless keep says to leave it.
func (p *roadmapProof) cleanup(keep bool) {
	if keep {
		note("--keep: the fake pro (container %s), Service %s/%s and MCPServer %s stay; the next roadmap-test without --keep removes them",
			p.fake.name, platformNamespace, proFakeService, proFakeServer)
		return
	}
	for _, remove := range slices.Backward(p.remove) {
		remove()
	}
}

func (p *roadmapProof) run() error {
	if err := p.fixture(); err != nil {
		return err
	}
	if err := p.muster(); err != nil {
		return err
	}
	return p.backend()
}

// fixture starts the fake, points the Service at it and registers it with
// muster as pro, the person's token forwarded.
func (p *roadmapProof) fixture() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	p.stages.begin("fixture", "Starting the fake pro (board %s) on the kind network and registering it with muster as MCPServer %s", proFakeBoard, proFakeServer)
	if p.fake, err = startProFakeContainer(p.cfg, self); err != nil {
		return err
	}
	p.remove = append(p.remove, p.fake.close)
	healthURL := fmt.Sprintf("http://%s.%s.svc.cluster.local%s", proFakeService, platformNamespace, proFakeHealthPath)
	removeService, err := fakeServiceForPods(p.cfg, proFakeService, "the fake pro", p.fake.podIP, fakeContainerPort, fakeServicePort, healthURL)
	if err != nil {
		return err
	}
	p.remove = append(p.remove, removeService)
	removeServer, err := registerProFake()
	if err != nil {
		return err
	}
	p.remove = append(p.remove, removeServer)
	return waitMCPServerReachable(proFakeServer)
}

// proFakeURL is the fake's MCP endpoint as muster dials it.
var proFakeURL = fmt.Sprintf("http://%s.%s.svc.cluster.local%s", proFakeService, platformNamespace, proFakeMCPPath)

// registerProFake registers the fake with muster as pro, served per session
// with the caller's token forwarded (how pro reads GitHub as the person);
// the returned func removes the MCPServer.
func registerProFake() (func(), error) {
	manifest := fmt.Sprintf(`apiVersion: muster.giantswarm.io/v1alpha1
kind: MCPServer
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: %s
    agentlab.giantswarm.io/fixture: roadmap-board
spec:
  description: >-
    Lab fixture: agentlab's fake pro, a fixed board %q for the portal's
    roadmap-backend (agentlab roadmap-test). Not GitHub.
  type: streamable-http
  url: %s
  autoStart: true
  auth:
    type: oauth
    forwardToken: true
`, proFakeServer, platformNamespace, managedByAgentlabValue, proFakeBoard, proFakeURL)
	if _, err := applyManifests(context.Background(), []byte(manifest)); err != nil {
		return nil, err
	}
	return func() {
		gvr, err := gvrFor(musterMCPServerResource)
		if err == nil {
			if err := deleteObject(context.Background(), gvr, platformNamespace, proFakeServer, fixtureDeleteWait); err != nil {
				note("cleanup: MCPServer %s: %v", proFakeServer, err)
			}
		}
	}, nil
}

// muster reads the board through muster as the person: the whole board, a
// team's items, and the calls the fake recorded carry the person's e-mail.
func (p *roadmapProof) muster() error {
	p.stages.begin("muster", "x_%s_list_issues through muster as %s: the fixture board, whole and by team, every call with %s's own token", proFakeServer, p.user.Email, p.user.Email)
	token, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, p.user.Email, p.user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	s, err := openMusterSession(p.cfg, token, "agentlab-roadmap-test")
	if err != nil {
		return err
	}
	list := func(filters map[string]any) ([]string, error) {
		args := map[string]any{argBoard: proFakeBoard, argFilters: filters, "limit": 0}
		var text string
		var last error
		// The first call of a session connects muster to the server.
		if !waitFor(20, 3*time.Second, func() bool {
			text, last = s.callServerTool("x_"+proFakeServer+"_list_issues", args)
			return last == nil
		}) {
			return nil, fmt.Errorf("muster answered no x_%s_list_issues within a minute: %w", proFakeServer, last)
		}
		var answer struct {
			Issues []proBoardItem `json:"issues"`
		}
		if err := json.Unmarshal([]byte(text), &answer); err != nil {
			return nil, fmt.Errorf("x_%s_list_issues answered no board: %w: %s", proFakeServer, err, excerpt(text, 300))
		}
		return itemTitles(answer.Issues), nil
	}
	whole, err := list(map[string]any{})
	if err != nil {
		return err
	}
	if err := sameTitles("x_pro_list_issues of the whole board", whole, fixtureTitles(nil)); err != nil {
		return err
	}
	note("the whole board: %s", strings.Join(whole, ", "))
	team, err := list(map[string]any{boardFieldTeam: proFakeTeam})
	if err != nil {
		return err
	}
	if err := sameTitles("x_pro_list_issues of team "+proFakeTeam, team, fixtureTitles(teamIs(proFakeTeam))); err != nil {
		return err
	}
	note("team %s: %d items, %s's left out", proFakeTeam, len(team), proFakeOtherTeam)
	return p.callsAs("a list_issues of team "+proFakeTeam, func(c proFakeCall) bool {
		filters, _ := c.Arguments[argFilters].(map[string]any)
		return c.Tool == proToolListIssues && filters[boardFieldTeam] == proFakeTeam
	})
}

// backend reads the board through the portal's roadmap-backend as the
// person: the connection, the schema, a team's column, the column without a
// status and the overview.
func (p *roadmapProof) backend() error {
	if !p.cfg.Backstage.Enabled || !p.cfg.Backstage.Hive {
		p.stages.begin("backend", "The portal's roadmap-backend reads the board as %s", p.user.Email)
		p.stages.skip("backstage.enabled and backstage.hive are not both on in %s (`agentlab configure --hive`, then `agentlab platform`): the portal serves no roadmap routes", config.File)
		return nil
	}
	p.stages.begin("backend", "The portal's roadmap-backend reads the board through muster as %s: /connection, /schema, a team's items, the items without a status, /overview", p.user.Email)
	ps, err := backstageLogin(p.cfg, p.user)
	if err != nil {
		return err
	}
	get := func(path string, v any) error {
		var status int
		var raw []byte
		var err error
		// A portal rolled onto backstage.hive a moment ago may still start.
		if !waitFor(20, 3*time.Second, func() bool {
			status, raw, err = ps.request(http.MethodGet, "/api/roadmap"+path, nil, ps.musterHeaders())
			return err == nil && status != http.StatusNotFound && status != http.StatusServiceUnavailable && status < 500
		}) && err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("GET /api/roadmap%s answered %d: %s", path, status, excerpt(strings.TrimSpace(string(raw)), 400))
		}
		if err := json.Unmarshal(raw, v); err != nil {
			return fmt.Errorf("GET /api/roadmap%s: %w: %s", path, err, excerpt(string(raw), 300))
		}
		return nil
	}

	var connection struct {
		Connected bool   `json:"connected"`
		Message   string `json:"message"`
	}
	if err := get("/connection", &connection); err != nil {
		return err
	}
	if !connection.Connected {
		return fmt.Errorf("/api/roadmap/connection says %s is not connected to %s: %s", p.user.Email, proFakeServer, connection.Message)
	}

	var schema struct {
		Board        string          `json:"board"`
		DefaultTeams []string        `json:"defaultTeams"`
		Fields       []proBoardField `json:"fields"`
	}
	if err := get("/schema", &schema); err != nil {
		return err
	}
	if schema.Board != proFakeBoard || !slices.Contains(schema.DefaultTeams, proFakeTeam) || len(schema.Fields) != len(proFakeSchema) {
		return fmt.Errorf("/api/roadmap/schema answered board %q, default teams %v and %d fields, want %q, %s and the fixture's %d",
			schema.Board, schema.DefaultTeams, len(schema.Fields), proFakeBoard, proFakeTeam, len(proFakeSchema))
	}

	type items struct {
		Items []proBoardItem `json:"items"`
	}
	team := url.Values{roadmapTeamParam: {proFakeTeam}}
	// The backend keeps each person's reads for minutes, across restarts:
	// the item reads carry an updated qualifier of this run's own, which
	// every fixture item matches, so they reach the fake instead of a read
	// an earlier run left (the overview's and the schema's may not).
	runUpdated := ">=" + time.Now().UTC().AddDate(-20, 0, 0).Format(time.RFC3339)
	teamItems := url.Values{roadmapTeamParam: {proFakeTeam}, argUpdated: {runUpdated}}
	var column items
	if err := get("/items?"+teamItems.Encode(), &column); err != nil {
		return err
	}
	if err := sameTitles("/api/roadmap/items of team "+proFakeTeam, itemTitles(column.Items), fixtureTitles(teamIs(proFakeTeam))); err != nil {
		return err
	}
	noStatus := url.Values{roadmapTeamParam: {proFakeTeam}, "empty": {"status"}, argUpdated: {runUpdated}}
	var unset items
	if err := get("/items?"+noStatus.Encode(), &unset); err != nil {
		return err
	}
	wantUnset := fixtureTitles(func(i proBoardItem) bool { return teamIs(proFakeTeam)(i) && i.Fields[boardFieldStatus] == "" })
	if err := sameTitles("/api/roadmap/items of team "+proFakeTeam+" without a status", itemTitles(unset.Items), wantUnset); err != nil {
		return err
	}

	var overview struct {
		Total    int            `json:"total"`
		ByStatus map[string]int `json:"byStatus"`
	}
	if err := get("/overview?"+team.Encode(), &overview); err != nil {
		return err
	}
	want := map[string]int{}
	for _, i := range proFakeItems() {
		if teamIs(proFakeTeam)(i) {
			want[cmp.Or(i.Fields[boardFieldStatus], "No status")]++
		}
	}
	if overview.Total != len(column.Items) || !maps.Equal(overview.ByStatus, want) {
		return fmt.Errorf("/api/roadmap/overview of team %s answered %d items by status %v, want %d by %v", proFakeTeam, overview.Total, overview.ByStatus, len(column.Items), want)
	}
	note("team %s: %d items (%s), %v by status; without a status: %s", proFakeTeam, len(column.Items), strings.Join(itemTitles(column.Items), ", "), overview.ByStatus, strings.Join(itemTitles(unset.Items), ", "))
	// The backend's own calls of this run: muster's stage sends no updated.
	if err := p.callsAs("the backend's list_issues of this run's items without a status", func(c proFakeCall) bool {
		empty, _ := c.Arguments["emptyFields"].([]any)
		return c.Tool == proToolListIssues && c.Arguments[argUpdated] == runUpdated && slices.Contains(empty, any(boardFieldStatus))
	}); err != nil {
		return err
	}
	return p.callsAs("the backend's list_issues of this run's team items", func(c proFakeCall) bool {
		filters, _ := c.Arguments[argFilters].(map[string]any)
		return c.Tool == proToolListIssues && c.Arguments[argUpdated] == runUpdated && filters[boardFieldTeam] == proFakeTeam
	})
}

// callsAs reads back that the fake answered a call match accepts, what
// names it, and that every call it answered came with the person's own token.
func (p *roadmapProof) callsAs(what string, match func(proFakeCall) bool) error {
	calls, err := p.fake.calls()
	if err != nil {
		return err
	}
	found := false
	for _, c := range calls {
		if c.Caller != p.user.Email {
			return fmt.Errorf("the fake pro answered %s for %q, not %s: muster did not forward the person's token", c.Tool, c.Caller, p.user.Email)
		}
		found = found || match(c)
	}
	if !found {
		return fmt.Errorf("the fake pro recorded no %s among its %d calls", what, len(calls))
	}
	note("the fake pro answered %s; its %d calls, each as %s", what, len(calls), p.user.Email)
	return nil
}

func teamIs(team string) func(proBoardItem) bool {
	return func(i proBoardItem) bool { return i.Fields[boardFieldTeam] == team }
}

// fixtureTitles are the titles of the fixture items keep accepts, all for nil.
func fixtureTitles(keep func(proBoardItem) bool) []string {
	var titles []string
	for _, i := range proFakeItems() {
		if keep == nil || keep(i) {
			titles = append(titles, i.Title)
		}
	}
	return titles
}

func itemTitles(items []proBoardItem) []string {
	titles := make([]string, 0, len(items))
	for _, i := range items {
		titles = append(titles, i.Title)
	}
	return titles
}

// sameTitles compares two title lists as sets, naming what is missing and
// what is extra.
func sameTitles(what string, got, want []string) error {
	var missing, extra []string
	for _, t := range want {
		if !slices.Contains(got, t) {
			missing = append(missing, t)
		}
	}
	for _, t := range got {
		if !slices.Contains(want, t) {
			extra = append(extra, t)
		}
	}
	if len(missing) > 0 || len(extra) > 0 || len(got) != len(want) {
		return fmt.Errorf("%s answered %d items %v, want the fixture's %d %v (missing %v, extra %v)", what, len(got), got, len(want), want, missing, extra)
	}
	return nil
}
