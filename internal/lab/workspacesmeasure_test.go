package lab

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

func TestMeasureStorageUnread(t *testing.T) {
	md := measureStorage{Expansion: &measureExpansion{From: "5Gi", To: "6Gi"}}.markdown()
	for _, want := range []string{"| Standing storage | not available: no run reached", "| Online expansion | not available: the run ended before the volume grew |"} {
		if !strings.Contains(md, want) {
			t.Errorf("an unread storage's Markdown lacks %q:\n%s", want, md)
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
		{"another cluster's git URL in the lab", WorkspacesMeasureOptions{GitURL: "https://git.example.com"}, "--git-url names another cluster's"},
		{"another cluster with a git URL", func() WorkspacesMeasureOptions { o := remote; o.GitURL = "https://git.example.com"; return o }(), ""},
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
		"git":          measureGitScript(syncListRepoB, measureLabBranch, ""),
		"git, remote":  measureGitScript("o/a", "", "https://proxy.example,direct"),
		"storage":      measureStorageScript,
		"owners":       sessionOwnersScript,
		"clear":        measureClearSessionsScript(volumeOwner{65532, 65532}),
		"fetch (turn)": measureFetchCommand(syncMovingRepo, measureLabBranch),
	} {
		// #nosec G204 -- the test's own scripts, parsed and not run
		if out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("the %s script does not parse: %v\n%s", name, err, out)
		}
	}
	git := measureGitScript(syncListRepoB, measureLabBranch, "")
	for _, want := range []string{"switch -q", "status --porcelain", "go build", "--config checkout.workers=0", "rm -rf /workspace/" + measureBuildDir + " 2>/dev/null && break"} {
		if !strings.Contains(git, want) {
			t.Errorf("the git script lacks %q", want)
		}
	}
}

func TestParseFetch(t *testing.T) {
	head := "0123456789abcdef0123456789abcdef01234567"
	reply := probeMarker + " pwd=/workspace\n`" + probeMarker + " fetch=start=1760000000.100000 end=1760000000.512000 rc=0 head=" + head + "`\n"
	seconds, rc, got, err := parseFetch(reply)
	if err != nil || seconds < 0.4119 || seconds > 0.4121 || rc != "0" || got != head {
		t.Fatalf("parseFetch = %v, %q, %q, %v", seconds, rc, got, err)
	}
	for _, coarse := range []string{"start=1760000000.000000000 end=1760000001.000000000", "start=1760000000.N end=1760000000.N", "start=1760000000 end=1760000001"} {
		if _, _, _, err := parseFetch(probeMarker + " fetch=" + coarse + " rc=0 head=" + head); err == nil || !strings.Contains(err.Error(), "no clock below a second") {
			t.Errorf("stamps %s: %v, want no clock below a second", coarse, err)
		}
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
		if i < 0 || fixture.Repositories[i].Files[goModFile] == "" || fixture.Repositories[i].Files[mainGoFile] == "" {
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
	if out, err := exec.Command("git", "-C", dir+"/c", "ls-files", goModFile, mainGoFile).Output(); err != nil || strings.Count(string(out), "\n") != 2 { // #nosec G204 -- the test's clone
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

// A run that ends at the preconditions has no samples: the report is still
// written, every figure not available.
func TestMeasureWriteReportWithoutSamples(t *testing.T) {
	m := &measurement{runDir: t.TempDir(), ws: []*measureWorkspace{
		{name: "agentlab-measure-small", kind: "small", repos: []string{"o/a"}},
		{name: "agentlab-measure-large", kind: "large", repos: []string{"o/b"}},
	}}
	m.report = measureReport{Runs: 5, Error: "workspace agentlab-measure-small is not gone"}
	if err := m.writeReport(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.runDir, measureReportJSON))
	if err != nil {
		t.Fatal(err)
	}
	var got measureReport
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Workspaces) != 2 || got.Error == "" {
		t.Fatalf("the report = %+v, want both workspaces and the error", got)
	}
	for _, w := range got.Workspaces {
		if len(w.Figures) != len(measureFigureDefs) {
			t.Fatalf("%s has %d figures, want %d", w.Name, len(w.Figures), len(measureFigureDefs))
		}
		for _, f := range w.Figures {
			if f.Median != nil || f.NotAvailable == "" {
				t.Errorf("%s %s = %+v, want not available", w.Name, f.Key, f)
			}
		}
	}
	md, err := os.ReadFile(filepath.Join(m.runDir, measureReportMarkdown))
	if err != nil || !strings.Contains(string(md), "The run ended early") {
		t.Fatalf("the Markdown = %q, %v; want the reason the run ended", md, err)
	}
}

// A leftover still deleting is measured around under a fresh name, which
// the report names.
func TestMeasureLeftoverReport(t *testing.T) {
	started := time.Date(2026, 10, 13, 17, 40, 5, 0, time.UTC)
	fresh := measureFreshName("agentlab-measure-small", started)
	if fresh != "agentlab-measure-small-20261013-174005" || len(fresh) > 63 {
		t.Fatalf("the fresh name = %q", fresh)
	}
	r := measureReport{Started: started, Ended: started, Runs: 5, Leftovers: []measureLeftover{{Name: "agentlab-measure-small", MeasuredAs: fresh}}}
	if md := r.markdown(); !strings.Contains(md, "`agentlab-measure-small` of an interrupted run was still deleting") || !strings.Contains(md, "`"+fresh+"`") {
		t.Fatalf("the Markdown does not name the leftover:\n%s", md)
	}
}

// measureTestProxy is a file:// module proxy serving example.com/dep
// v1.0.0, a module the measured repository depends on.
func measureTestProxy(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proxy")
	v := filepath.Join(dir, "example.com", "dep")
	if err := os.MkdirAll(filepath.Join(v, "@v"), 0o750); err != nil {
		t.Fatal(err)
	}
	mod := "module example.com/dep\n\ngo 1.22\n"
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	for name, body := range map[string]string{goModFile: mod, "dep.go": "package dep\n\nfunc Answer() int { return 42 }\n"} {
		f, err := zw.Create("example.com/dep@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"@v/list": []byte("v1.0.0\n"), "@v/v1.0.0.info": []byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`),
		"@v/v1.0.0.mod": []byte(mod), "@v/v1.0.0.zip": zipped.Bytes()} {
		if err := os.WriteFile(filepath.Join(v, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return "file://" + dir
}

// runMeasureBuild runs the git probe's build in a module of the files and
// returns its output and the parsed probe.
func runMeasureBuild(t *testing.T, goproxy string, files map[string]string) (string, measureGitProbe) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	root := t.TempDir()
	mod := filepath.Join(root, "repo")
	for name, body := range files {
		path := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(files) == 0 {
		if err := os.MkdirAll(mod, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	script := "ms() { date +%s%3N; }\n" + measureBuildFunction(goproxy) + "cd " + mod + " && build volume " + filepath.Join(root, "build") + "\n"
	// #nosec G204 -- the test's own script
	cmd := exec.Command(sh, shExitOnError, "-c", script)
	// The checksum database is not asked for the test's module; the probe
	// sets the rest of Go's environment itself.
	cmd.Env = append(os.Environ(), "GOSUMDB=off", "GOENV=off", "GOPROXY=off", "HOME="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the build failed the script: %v\n%s", err, out)
	}
	return string(out), parseGitProbe(string(out))
}

// The test module's files.
const (
	goModFile           = "go.mod"
	mainGoFile          = "main.go"
	measureTestGoMod    = "module example.com/app\n\ngo 1.22\n"
	measureTestGoModDep = measureTestGoMod + "\nrequire example.com/dep v1.0.0\n"
)

const measureTestMain = "package main\n\nimport \"example.com/dep\"\n\nfunc main() { println(dep.Answer()) }\n"

func TestMeasureBuildDownloadsDependencies(t *testing.T) {
	out, p := runMeasureBuild(t, measureTestProxy(t), map[string]string{
		goModFile:  measureTestGoModDep,
		mainGoFile: measureTestMain,
	})
	for _, key := range []string{"download-volume", "build-volume"} {
		if _, why := p.value(key, "o/app"); why != "" {
			t.Errorf("%s not available: %s\n%s", key, why, out)
		}
	}
}

func TestMeasureBuildNotAvailable(t *testing.T) {
	proxy := measureTestProxy(t)
	for name, tc := range map[string]struct {
		files           map[string]string
		goproxy         string
		download, build string
	}{
		// The probe before it downloaded: every dependency refused.
		"the proxy off": {files: map[string]string{goModFile: measureTestGoModDep, mainGoFile: measureTestMain}, goproxy: "off",
			download: "module lookup disabled by GOPROXY=off", build: "the build failed: the download failed"},
		"no go.mod": {files: nil, download: "no Go module at its root", build: "no Go module at its root"},
		"compile fails": {files: map[string]string{goModFile: measureTestGoMod, mainGoFile: "package main\n\nfunc main() { undefinedName() }\n"},
			build: "the build failed: ./main.go:3:15: undefined: undefinedName"},
		"download fails": {files: map[string]string{goModFile: "module example.com/app\n\ngo 1.22\n\nrequire example.com/missing v1.0.0\n", mainGoFile: "package main\n\nimport _ \"example.com/missing\"\n\nfunc main() {}\n"},
			download: "the build failed: ", build: "the build failed: the download failed"},
		"vendored": {files: map[string]string{goModFile: measureTestGoModDep, mainGoFile: measureTestMain,
			"vendor/modules.txt": "# example.com/dep v1.0.0\n## explicit\nexample.com/dep\n", "vendor/example.com/dep/dep.go": "package dep\n\nfunc Answer() int { return 42 }\n"},
			download: "vendors its modules"},
	} {
		t.Run(name, func(t *testing.T) {
			out, p := runMeasureBuild(t, cmpOr(tc.goproxy, proxy), tc.files)
			for key, want := range map[string]string{"download-volume": tc.download, "build-volume": tc.build} {
				_, why := p.value(key, "o/app")
				switch {
				case want == "" && why != "":
					t.Errorf("%s not available: %s\n%s", key, why, out)
				case want != "" && !strings.Contains(why, want):
					t.Errorf("%s: %q, want not available with %q\n%s", key, why, want, out)
				}
			}
		})
	}
}

// The git probe's output of a run whose build failed before the probe
// reported its figures one by one: what it measured is kept.
func TestParseGitProbeKeepsFigures(t *testing.T) {
	out := "measure branch=main workers=0 untracked-cache=true many-files=true\nmeasure checkout-volume=2170\nmeasure status-volume=844\nmeasure session-bytes=1517786\n" +
		"measure download-volume=1200\nmeasure build-volume=fail\nmeasure-why build-volume cmd/serve.go:16:2: undefined: x\nmeasure checkout-local=900\nmeasure build-local=\n"
	p := parseGitProbe(out)
	s := newMeasureSamples()
	for _, f := range measureGitFigures {
		if v, why := p.value(f.key, "o/a"); why != "" {
			s.notAvailable(f.fig, why)
		} else {
			s.add(f.fig, v)
		}
	}
	byKey := map[string]measureFigure{}
	for _, f := range s.figures() {
		byKey[f.Key] = f
	}
	for fig, want := range map[string]float64{figCheckoutVolume: 2.17, figStatusVolume: 0.844, figSessionBytes: 1517786, figDownloadVolume: 1.2, figCheckoutLocal: 0.9} {
		if f := byKey[fig]; len(f.Samples) != 1 || f.Samples[0] != want {
			t.Errorf("%s = %v (%s), want %v", fig, f.Samples, f.NotAvailable, want)
		}
	}
	for fig, want := range map[string]string{figBuildVolume: "the build failed: cmd/serve.go:16:2: undefined: x", figBuildLocal: `the git probe reported build-local=""`,
		figStatusLocal: "the git probe reported no status-local"} {
		if f := byKey[fig]; f.NotAvailable != want {
			t.Errorf("%s not available %q, want %q", fig, f.NotAvailable, want)
		}
	}
}

func TestMeasureReportFailedProbes(t *testing.T) {
	r := measureReport{Target: "the lab agentlab", Runs: 5, Failed: []measureFailedProbe{{Run: 2, Workspace: measureSmallWorkspace, Probe: "git (build-volume)", Reason: "a | b"}}}
	md := r.markdown()
	if want := "| 2 | `" + measureSmallWorkspace + "` | git (build-volume) | a \\| b |"; !strings.Contains(md, want) {
		t.Errorf("the Markdown lacks %q:\n%s", want, md)
	}
	if strings.Contains(md, "ended early") {
		t.Errorf("a failed probe is not the run's end:\n%s", md)
	}
}

func TestWorkspacesMeasureOptionsGoProxy(t *testing.T) {
	if err := (WorkspacesMeasureOptions{GoProxy: "https://proxy.example,direct"}).validate(); err != nil {
		t.Errorf("a GOPROXY list: %v", err)
	}
	for _, bad := range []string{"x' ; rm -rf /", "a b"} {
		if err := (WorkspacesMeasureOptions{GoProxy: bad}).validate(); err == nil {
			t.Errorf("--goproxy %q accepted", bad)
		}
	}
}

// measureTestPushed is the repository the pusher tests push to.
const measureTestPushed = "o/pushed"

// measureTestRepo is a bare repository with one commit on main, served to
// the pusher at a file:// URL.
func measureTestRepo(t *testing.T) (bare, url, head string) {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "origin.git"), filepath.Join(dir, "work")
	run := func(d string, args ...string) string {
		t.Helper()
		out, err := personGit(d, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run(dir, "init", "-q", "--bare", "-b", measureLabBranch, bare)
	run(dir, "init", "-q", "-b", measureLabBranch, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "README.md")
	run(work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "a")
	run(work, "push", "-q", bare, measureLabBranch)
	return bare, "file://" + bare, run(work, "rev-parse", "HEAD")
}

func TestMeasurePusher(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	// The clones go under the state directory, relative to the working
	// directory.
	t.Chdir(t.TempDir())
	if err := os.Mkdir(StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ref := func(bare, branch string) string {
		out, _ := personGit(bare, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
		return out
	}
	t.Run("scratch", func(t *testing.T) {
		bare, url, start := measureTestRepo(t)
		m := &measurement{pushers: map[string]*measurePusher{}}
		pu := &measurePusher{repo: measureTestPushed, branch: "agentlab-workspaces-measure-x", scratch: true, git: personGit}
		if err := m.clonePusher(pu, url); err != nil {
			t.Fatal(err)
		}
		first, err := m.push(measureTestPushed)
		if err != nil {
			t.Fatal(err)
		}
		second, err := m.push(measureTestPushed)
		if err != nil {
			t.Fatal(err)
		}
		if first == second || ref(bare, pu.branch) != second {
			t.Errorf("the scratch branch is at %q after pushes %s and %s, want the second", ref(bare, pu.branch), first, second)
		}
		if ref(bare, measureLabBranch) != start {
			t.Errorf("a scratch push moved main to %s from %s", ref(bare, measureLabBranch), start)
		}
		if err := pu.unpush(); err != nil {
			t.Fatal(err)
		}
		if got := ref(bare, pu.branch); got != "" {
			t.Errorf("the scratch branch is still there at %s", got)
		}
	})
	t.Run("the lab's default branch", func(t *testing.T) {
		bare, url, start := measureTestRepo(t)
		m := &measurement{pushers: map[string]*measurePusher{}}
		pu := &measurePusher{repo: measureTestPushed, branch: measureLabBranch, git: personGit}
		if err := m.clonePusher(pu, url); err != nil {
			t.Fatal(err)
		}
		head, err := m.push(measureTestPushed)
		if err != nil || ref(bare, measureLabBranch) != head {
			t.Fatalf("main at %s after a push of %s: %v", ref(bare, measureLabBranch), head, err)
		}
		if err := pu.unpush(); err != nil {
			t.Fatal(err)
		}
		if ref(bare, measureLabBranch) != start {
			t.Errorf("main at %s after the cleanup, want back at %s", ref(bare, measureLabBranch), start)
		}
	})
	t.Run("no push", func(t *testing.T) {
		pu := &measurePusher{repo: measureTestPushed, branch: "b", scratch: true, git: func(string, ...string) (string, error) {
			t.Fatal("unpush ran git without a push")
			return "", nil
		}}
		if err := pu.unpush(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMeasureReportSessionsLeft(t *testing.T) {
	md := (&measureReport{Runs: 1, SessionsLeft: []string{"s1", "s2"}}).markdown()
	if !strings.Contains(md, "**Sessions left:** the cleanup could not delete `s1`, `s2`") {
		t.Errorf("the Markdown does not name the Sessions left:\n%s", md)
	}
}

// TestMeasureDeleteEmptyAnswer: a delete_workspace whose call_tool answer is
// empty passes once get_workspace answers not found, and fails naming the
// empty answer while get_workspace still answers the workspace.
func TestMeasureDeleteEmptyAnswer(t *testing.T) {
	const name = "measure-empty"
	present := toolResult("workspace "+name, false, map[string]any{"workspace": map[string]any{nameKey: name}})
	notFound := toolResult("workspace "+name+" not found", true, nil)
	script := func(goneAfter int) func(string, int) mcpAnswer {
		return func(tool string, n int) mcpAnswer {
			switch {
			case tool == workspaceManagerToolPrefix+"delete_workspace":
				return callToolAnswer(t, nil)
			case n >= goneAfter:
				return callToolAnswer(t, notFound)
			default:
				return callToolAnswer(t, present)
			}
		}
	}

	m := &measurement{person: scriptedMuster(t, script(1)), timeout: 10 * time.Second}
	answered, err := m.requestDelete(name)
	if err != nil || answered {
		t.Fatalf("requestDelete on an empty answer = %v, %v; want false, nil", answered, err)
	}
	if err := m.awaitDeleted(name, syncVolume{}, answered); err != nil {
		t.Errorf("the workspace gone after an empty answer: %v", err)
	}

	m = &measurement{person: scriptedMuster(t, script(1<<30)), timeout: 100 * time.Millisecond}
	answered, err = m.requestDelete(name)
	if err != nil {
		t.Fatal(err)
	}
	err = m.awaitDeleted(name, syncVolume{}, answered)
	if err == nil {
		t.Fatal("the workspace still present after the bound passed")
	}
	for _, want := range []string{name, "answered nothing", "get_workspace still answers it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	refusing := &measurement{person: scriptedMuster(t, func(string, int) mcpAnswer {
		return callToolAnswer(t, toolResult("permission denied", true, nil))
	}), timeout: time.Second}
	if _, err := refusing.requestDelete(name); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("a refused delete: %v, want its error", err)
	}
}
