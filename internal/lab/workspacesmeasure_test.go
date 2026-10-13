package lab

import (
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// wsGeneratedMiB is the generated repository request's size field.
const wsGeneratedMiB = "generatedMiB"

func TestMeasureStats(t *testing.T) {
	for _, tc := range []struct {
		samples          []float64
		median, p90, max float64
	}{
		{[]float64{3}, 3, 3, 3},
		{[]float64{4, 1, 3, 2}, 2.5, 4, 4},
		{[]float64{5, 1, 4, 2, 3}, 3, 5, 5},
		{[]float64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, 5.5, 9, 10},
	} {
		median, p90, maxV, ok := measureStats(tc.samples)
		if !ok || median != tc.median || p90 != tc.p90 || maxV != tc.max {
			t.Errorf("measureStats(%v) = %v, %v, %v, %v; want %v, %v, %v", tc.samples, median, p90, maxV, ok, tc.median, tc.p90, tc.max)
		}
	}
	if _, _, _, ok := measureStats(nil); ok {
		t.Error("measureStats(nil) has statistics")
	}
}

func TestJudgeMirror(t *testing.T) {
	within := judgeMirror("o/a", 100, 120*1024)
	if within.Factor == nil || *within.Factor != 1.2 || !*within.WithinEstimate {
		t.Errorf("a mirror 1.2 × the provider's size = %+v, want factor 1.2 within the estimate", within)
	}
	above := judgeMirror("o/b", 100, 200*1024)
	if *above.WithinEstimate {
		t.Errorf("a mirror 2 × the provider's size = %+v, want above the estimate", above)
	}
	if none := judgeMirror("o/c", 0, 4096); none.Factor != nil {
		t.Errorf("a repository without a provider size = %+v, want no factor", none)
	}
}

func TestJudgeHeadroom(t *testing.T) {
	gib := measureGiB
	capacity := 5 * gib
	small := judgeHeadroom(gib/2, &capacity, "")
	if small.WantBytes != 4*gib || !*small.Held {
		t.Errorf("half a GiB on 5 GiB = %+v, want 4 GiB wanted and held", small)
	}
	big := judgeHeadroom(20*gib, &capacity, "")
	if big.WantBytes != 10*gib || *big.Held || *big.FreeBytes != -15*gib {
		t.Errorf("20 GiB on 5 GiB = %+v, want 10 GiB wanted, not held", big)
	}
	elastic := judgeHeadroom(gib, nil, "elastic")
	if elastic.Held != nil || elastic.NotAvailable != "elastic" {
		t.Errorf("no capacity = %+v, want not available", elastic)
	}
}

func TestMeasureSamplesFigures(t *testing.T) {
	s := newMeasureSamples()
	start := time.Unix(100, 0)
	s.since(figFirstSync, start, start.Add(1500*time.Millisecond))
	s.add(figFirstSync, 2.5)
	s.notAvailable(figChangedCycle, "no push")
	s.notAvailable(figChangedCycle, "a later reason")
	figures := s.figures()
	if len(figures) != len(measureFigureDefs) {
		t.Fatalf("%d figures, want one per definition (%d)", len(figures), len(measureFigureDefs))
	}
	byKey := map[string]measureFigure{}
	for _, f := range figures {
		if f.From == "" || f.To == "" {
			t.Errorf("figure %s does not say what it measured from and to", f.Key)
		}
		byKey[f.Key] = f
	}
	if f := byKey[figFirstSync]; *f.Median != 2 || *f.Max != 2.5 || f.NotAvailable != "" {
		t.Errorf("the first sync = %+v, want median 2, max 2.5", f)
	}
	if f := byKey[figChangedCycle]; f.NotAvailable != "no push" || f.Median != nil {
		t.Errorf("the changed cycle = %+v, want not available for the first reason", f)
	}
	if f := byKey[figBuildLocal]; f.NotAvailable != "no run reached it" {
		t.Errorf("a figure no run reached = %+v", f)
	}
}

func TestMeasureReportMarkdown(t *testing.T) {
	s := newMeasureSamples()
	s.add(figCheckoutVolume, 0.25)
	s.add(figSessionBytes, 3*1024*1024)
	s.notAvailable(figBuildVolume, "kagent | defines no Go module")
	capacity := 5 * measureGiB
	requested, resized := 0.8, 2.25
	r := measureReport{Target: "the lab agentlab", Runs: 1, WorkspaceManager: "0.5.1", Harness: claudeHarness,
		Started: time.Unix(0, 0), Ended: time.Unix(60, 0),
		Workspaces: []measureWorkspaceReport{{Name: measureSmallWorkspace, Kind: measureSmall, Repositories: measureLabSmallRepos, Pushed: syncMovingRepo, Worked: syncListRepoB,
			Figures: s.figures(),
			Storage: measureStorage{StorageClass: workspacesStorageClass, Driver: workspacesCSIDriver, ProvisionsSize: true, CapacityBytes: &capacity,
				MirrorsBytes: 2048, SessionsBytes: 4096,
				Mirrors:   []measureMirror{judgeMirror(syncMovingRepo, 10, 30*1024)},
				Headroom:  judgeHeadroom(6144, &capacity, ""),
				Expansion: &measureExpansion{From: "5Gi", To: "6Gi", RequestedSeconds: &requested, ResizedSeconds: &resized}}}},
	}
	md := r.markdown()
	for _, want := range []string{
		"## Workspace measurements: the lab agentlab",
		"workspace-manager 0.5.1",
		"| First checkout, volume | `git switch <default branch>` in the Session's clone, no working tree yet → the working tree checked out, on the workspace volume | 250 ms | 250 ms | 250 ms | 1 |",
		"| Session directory size |", "3.0 MiB",
		`not available: kagent \| defines no Go module`,
		"| Capacity (the claim's status) | 5.0 GiB |",
		"factor 3.00, above W8's 1.5",
		"wanted 4.0 GiB: held",
		"| Online expansion 5Gi → 6Gi | the manager raised the claim 800 ms after update_workspace, the driver resized it 2.25 s later |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the Markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestWorkspacesMeasureOptionsValidate(t *testing.T) {
	remote := WorkspacesMeasureOptions{KubeContext: "ctx", MusterURL: "https://muster.example.com", Organization: "org", Provider: config.WorkspaceProviderGitHub,
		SmallRepos: []string{"o/a", "o/b"}, LargeRepo: "o/big"}
	withAgent := remote
	withAgent.Agent, withAgent.AgentgatewayURL = "kagent/coder", "https://agentgateway.example.com"
	for _, tc := range []struct {
		name string
		opts WorkspacesMeasureOptions
		want string
	}{
		{"the lab", WorkspacesMeasureOptions{}, ""},
		{"the lab as dev with a large repository", WorkspacesMeasureOptions{Email: testDevUser, LargeRepoMiB: 512, Runs: 3}, ""},
		{"negative runs", WorkspacesMeasureOptions{Runs: -1}, "--runs"},
		{"a large repository too large", WorkspacesMeasureOptions{LargeRepoMiB: githubFakeMaxGeneratedMiB + 1}, "--large-repo-mib"},
		{"another cluster's flag in the lab", WorkspacesMeasureOptions{LargeRepo: "o/big"}, "--large-repo names another cluster's"},
		{"another cluster", remote, ""},
		{"another cluster with an Agent", withAgent, ""},
		{"a half target", WorkspacesMeasureOptions{KubeContext: "ctx"}, "--muster-url"},
		{"the lab's email on another cluster", func() WorkspacesMeasureOptions { o := remote; o.Email = testDevUser; return o }(), "[email] is the lab's"},
		{"a repository without an owner", func() WorkspacesMeasureOptions { o := remote; o.LargeRepo = "big"; return o }(), "owner/name"},
		{"an Agent without the agentgateway", func() WorkspacesMeasureOptions { o := remote; o.Agent = "kagent/coder"; return o }(), "go together"},
		{"an Agent without a namespace", func() WorkspacesMeasureOptions { o := withAgent; o.Agent = "coder"; return o }(), "namespace/name"},
	} {
		err := tc.opts.validate()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: validate = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestMeasureScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, script := range map[string]string{
		"git":          measureGitScript(syncListRepoB, measureLabBranch),
		"git, remote":  measureGitScript("o/a", ""),
		"storage":      measureStorageScript,
		"fetch (turn)": measureFetchCommand(syncMovingRepo, measureLabBranch),
	} {
		// #nosec G204 -- the test's own scripts, parsed and not run
		if out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("the %s script does not parse: %v\n%s", name, err, out)
		}
	}
	git := measureGitScript(syncListRepoB, measureLabBranch)
	for _, want := range []string{"switch -q", "status --porcelain", "go build", "--config checkout.workers=0", "rm -rf /workspace/" + measureBuildDir} {
		if !strings.Contains(git, want) {
			t.Errorf("the git script lacks %q", want)
		}
	}
}

func TestParseFetch(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	reply := probeMarker + " pwd=/workspace\n`" + probeMarker + " fetch=ms=412 rc=0 head=" + head + "`\n"
	ms, rc, got, err := parseFetch(reply)
	if err != nil || ms != 412 || rc != "0" || got != head {
		t.Fatalf("parseFetch = %v, %q, %q, %v", ms, rc, got, err)
	}
	if _, _, _, err := parseFetch(probeMarker + " pwd=/workspace\n"); err == nil {
		t.Fatal("a reply without the fetch line parsed")
	}
}

func TestMeasureLabFixtureBuilds(t *testing.T) {
	fixture, err := loadGitHubFixture("")
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{measureLabLargeRepo, syncListRepoB} {
		i := slices.IndexFunc(fixture.Repositories, func(r githubFixtureRepo) bool { return r.fullName() == repo })
		if i < 0 || fixture.Repositories[i].Files["go.mod"] == "" || fixture.Repositories[i].Files["main.go"] == "" {
			t.Errorf("the fixture's %s carries no Go module for the measurement's build", repo)
		}
	}
	if !slices.Contains(measureLabSmallRepos, syncMovingRepo) || !slices.Contains(measureLabSmallRepos, syncListRepoB) {
		t.Errorf("the small workspace %v holds neither the pushed %s nor the built %s", measureLabSmallRepos, syncMovingRepo, syncListRepoB)
	}
}

func TestWorkspaceGitHubGeneratedRepo(t *testing.T) {
	f := startWorkspaceFake(t)
	path := githubFakeReposPath + "/" + wsOrg + "/monorepo-2mib"
	if resp := f.call(t, http.MethodPut, path, "", map[string]any{wsGeneratedMiB: 0}, nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("a generated repository of 0 MiB = %d, want 422", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodPut, githubFakeReposPath+"/nobody/x", "", map[string]any{wsGeneratedMiB: 1}, nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown owner = %d, want 422", resp.StatusCode)
	}
	var created struct {
		Size int64 `json:"size"`
	}
	if resp := f.call(t, http.MethodPut, path, "", map[string]any{wsGeneratedMiB: 2, "language": "Go"}, &created); resp.StatusCode != http.StatusCreated || created.Size < 2048 {
		t.Fatalf("adding 2 MiB = %d, size %d KiB", resp.StatusCode, created.Size)
	}
	if resp := f.call(t, http.MethodPut, path, "", map[string]any{wsGeneratedMiB: 2}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the same request again = %d, want 200", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodPut, path, "", map[string]any{wsGeneratedMiB: 3}, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("another size under the same name = %d, want 409", resp.StatusCode)
	}
	var repo map[string]any
	f.call(t, http.MethodGet, githubFakeAPIPath+"/repos/"+wsOrg+"/monorepo-2mib", "", nil, &repo)
	if repo[wsFullName] != wsOrg+"/monorepo-2mib" || repo["language"] != "Go" || repo["private"] != false {
		t.Fatalf("the generated repository = %v", repo)
	}
	dir := t.TempDir()
	// #nosec G204 -- git against the test's own fake
	if out, err := exec.Command("git", "-c", "http.sslCAInfo="+f.caPath, "clone", "-q", f.url+"/"+wsOrg+"/monorepo-2mib.git", dir+"/c").CombinedOutput(); err != nil {
		t.Fatalf("cloning the generated repository: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir+"/c", "ls-files", "go.mod", "main.go").Output(); err != nil || strings.Count(string(out), "\n") != 2 { // #nosec G204 -- the test's clone
		t.Fatalf("the generated repository's Go module = %q, %v", out, err)
	}
	if resp := f.call(t, http.MethodDelete, githubFakeReposPath+"/"+wsPublic, "", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("removing the fixture's %s = %d, want 409", wsPublic, resp.StatusCode)
	}
	if resp := f.call(t, http.MethodDelete, path, "", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("removing the generated repository = %d, want 204", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodGet, githubFakeAPIPath+"/repos/"+wsOrg+"/monorepo-2mib", "", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the removed repository answers %d, want 404", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodDelete, path, "", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removing it again = %d, want 404", resp.StatusCode)
	}
}
