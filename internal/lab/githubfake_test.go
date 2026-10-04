package lab

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fake's test fixtures.
const (
	testLogin   = "admin"
	testEmail   = testLogin + "@lab.local"
	testOldFile = "kagent/old.yaml"
	testFileA   = "d/a.yaml"
	testCreate  = "create"
	testSealed  = "d/secret.yaml"
)

// fakeBearer is a JWT-shaped token carrying the e-mail the fake's login
// follows; the signature is not checked.
func fakeBearer(email string) string {
	claims, _ := json.Marshal(map[string]string{"email": email})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// TestFakeGitHubServesTheCommitRemote drives gitops-commit's GitHub remote —
// the client model-manager's commit mode uses — against the fake: branch,
// commit (a write and a removal), pull request, the read of a file, and the
// read-back the proof asserts on.
func TestFakeGitHubServesTheCommitRemote(t *testing.T) {
	f, err := startFakeGitHub("127.0.0.1:0", "agentlab/gitops", gitopsBranch, map[string][]byte{
		"kagent/kustomization.yaml": []byte("resources: []\n"),
		testOldFile:                 []byte("old\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	base := "http://" + f.listener.Addr().String() + githubFakeAPIPath

	resp, err := http.Get(base + "/user")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /user without a bearer = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+fakeBearer(testEmail))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var user struct{ Login string }
	_ = json.NewDecoder(resp.Body).Decode(&user)
	_ = resp.Body.Close()
	if user.Login != testLogin {
		t.Fatalf("GET /user login = %q, want admin", user.Login)
	}

	for path, want := range map[string]int{"/repos/agentlab/gitops": http.StatusOK, "/repos/agentlab/app-not-installed": http.StatusNotFound} {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+fakeBearer(testEmail))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	gh, err := commit.NewGitHub(fakeBearer(testEmail), commit.WithBaseURL(base))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	repo := commit.Repository{Owner: "agentlab", Name: "gitops"}
	if _, err := gh.ReadFile(ctx, repo, gitopsBranch, "kagent/missing.yaml"); !errors.Is(err, commit.ErrFileNotFound) {
		t.Fatalf("ReadFile of a missing file: %v, want ErrFileNotFound", err)
	}
	raw, err := gh.ReadFile(ctx, repo, gitopsBranch, "kagent/kustomization.yaml")
	if err != nil || string(raw) != "resources: []\n" {
		t.Fatalf("ReadFile = %q, %v", raw, err)
	}
	const branch = "model-manager/wire-qwen3"
	if err := gh.CreateBranch(ctx, repo, branch, gitopsBranch); err != nil {
		t.Fatal(err)
	}
	if err := gh.CreateBranch(ctx, repo, branch, gitopsBranch); err != nil {
		t.Fatalf("CreateBranch of an existing branch: %v", err)
	}
	if err := gh.Commit(ctx, repo, branch, "wire", map[string][]byte{
		"kagent/model-manager/modelconfig.yaml": []byte("kind: ModelConfig\n"),
		testOldFile:                             nil,
	}); err != nil {
		t.Fatal(err)
	}
	pr, err := gh.OpenPullRequest(ctx, repo, branch, gitopsBranch, "feat(model-manager): wire qwen3", "body")
	if err != nil {
		t.Fatal(err)
	}
	again, err := gh.OpenPullRequest(ctx, repo, branch, gitopsBranch, "feat(model-manager): wire qwen3", "body")
	if err != nil || again.Number != pr.Number {
		t.Fatalf("a second OpenPullRequest = #%d, %v; want the open #%d", again.Number, err, pr.Number)
	}
	if !strings.Contains(pr.URL, "/agentlab/gitops/pull/1") {
		t.Errorf("pull request URL %q", pr.URL)
	}
	if raw, err := gh.ReadFile(ctx, repo, branch, "kagent/model-manager/modelconfig.yaml"); err != nil || string(raw) != "kind: ModelConfig\n" {
		t.Errorf("ReadFile on the branch = %q, %v", raw, err)
	}

	views := f.pullViews()
	if len(views) != 1 {
		t.Fatalf("%d pull requests recorded, want 1", len(views))
	}
	v := views[0]
	if v.Author != testLogin || v.Head != branch || v.Base != gitopsBranch {
		t.Errorf("recorded %+v", v)
	}
	if len(v.Files) != 1 || v.Files["kagent/model-manager/modelconfig.yaml"] != "kind: ModelConfig\n" {
		t.Errorf("recorded files %v", v.Files)
	}
	if len(v.Removed) != 1 || v.Removed[0] != testOldFile {
		t.Errorf("recorded removals %v", v.Removed)
	}

	other, _ := commit.NewGitHub(fakeBearer(testEmail), commit.WithBaseURL(base))
	if _, err := other.ReadFile(ctx, commit.Repository{Owner: "someone", Name: "else"}, gitopsBranch, "x"); err == nil {
		t.Error("a repository the fake does not hold answered a read")
	}
	if err := ServeFakeGitHub(ctx, "127.0.0.1:0", "agentlab/gitops", gitopsBranch, []string{"a.yaml"}); err == nil || !strings.Contains(err.Error(), "<path>=<base64 content>") {
		t.Errorf("a file without content: %v", err)
	}
}

// TestSameCommitFiles is the proof's comparison of the dry run with the
// pull request.
func TestSameCommitFiles(t *testing.T) {
	type file = struct {
		Path    string `json:"path"`
		Action  string `json:"action"`
		Content string `json:"content"`
	}
	pr := githubFakePullView{Files: map[string]string{testFileA: "a\n", testSealed: "data: ENC\nsops:\n  age: []\n"}, Removed: []string{"d/old.yaml"}} // #nosec G101 -- a test fixture, no credential
	dry := []file{{testFileA, testCreate, "a\n"}, {testSealed, testCreate, ""}, {"d/old.yaml", "delete", ""}}
	if err := sameCommitFiles(dry, pr); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]file{
		"content differs":     {{testFileA, testCreate, "b\n"}, {testSealed, testCreate, ""}},
		"file missing":        {{testFileA, testCreate, "a\n"}, {testSealed, testCreate, ""}, {"d/c.yaml", testCreate, "c\n"}},
		"unnamed file":        {{testFileA, testCreate, "a\n"}},
		"removal kept":        {{testFileA, testCreate, "a\n"}, {testSealed, testCreate, ""}, {"d/keep.yaml", "delete", ""}},
		"secret in the clear": {{testFileA, testCreate, "a\n"}, {testSealed, testCreate, ""}},
	} {
		view := pr
		if name == "secret in the clear" {
			view = githubFakePullView{Files: map[string]string{testFileA: "a\n", testSealed: "stringData: {}\n"}} // #nosec G101 -- a test fixture's empty Secret, no credential
		}
		if err := sameCommitFiles(bad, view); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestGitOpsHelmRelease is the copy of the platform's model-manager
// HelmRelease: the post-renderers retargeted at the copy's Deployment, the
// chart reference kept, the values pinned to the fake and Dex.
func TestGitOpsHelmRelease(t *testing.T) {
	var platform map[string]any
	require := `{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": {"name": "model-manager"},
	  "spec": {"chartRef": {"kind": "OCIRepository", "name": "model-manager"}, "releaseName": "model-manager",
	    "values": {"backends": ["backend-a"], "networkPolicy": {"enabled": true, "egressCIDRs": ["10.0.0.0/8"]}},
	    "postRenderers": [{"kustomize": {"patches": [{"target": {"kind": "Deployment", "name": "model-manager"}, "patch": "kind: Deployment\nmetadata:\n  name: model-manager\nspec:\n  template:\n    spec:\n      containers:\n        - name: model-manager\n"}]}}]}}`
	if err := json.Unmarshal([]byte(require), &platform); err != nil {
		t.Fatal(err)
	}
	raw, err := gitopsHelmRelease(&config.Config{}, platform, "172.21.0.9")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Metadata struct{ Name string }
		Spec     struct {
			ChartRef      struct{ Name string } `json:"chartRef"`
			ReleaseName   string                `json:"releaseName"`
			Values        map[string]any        `json:"values"`
			PostRenderers []struct {
				Kustomize struct {
					Patches []struct {
						Target struct{ Name string }
						Patch  string
					}
				}
			} `json:"postRenderers"`
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	patch := got.Spec.PostRenderers[0].Kustomize.Patches[0]
	if got.Metadata.Name != gitopsModelManager || got.Spec.ReleaseName != gitopsModelManager || got.Spec.ChartRef.Name != modelManagerMCPServer {
		t.Errorf("names: %s, release %s, chartRef %s", got.Metadata.Name, got.Spec.ReleaseName, got.Spec.ChartRef.Name)
	}
	if patch.Target.Name != gitopsModelManager || !strings.Contains(patch.Patch, "metadata:\n  name: "+gitopsModelManager+"\n") ||
		!strings.Contains(patch.Patch, "- name: "+modelManagerMCPServer+"\n") {
		t.Errorf("post-renderer not retargeted: %+v", patch)
	}
	v := got.Spec.Values
	gh, _ := v["github"].(map[string]any)
	np, _ := v["networkPolicy"].(map[string]any)
	if v["fullnameOverride"] != gitopsModelManager || gh["enabled"] != true || gh["apiURL"] != githubFakeServiceHost+githubFakeAPIPath {
		t.Errorf("values: %v", v)
	}
	if cidrs, _ := np["egressCIDRs"].([]any); len(cidrs) != 2 || cidrs[1] != "172.21.0.9/32" {
		t.Errorf("egress: %v", np)
	}
	if b, _ := v["backends"].([]any); len(b) != 1 {
		t.Errorf("the platform's values are not kept: %v", v)
	}
}
