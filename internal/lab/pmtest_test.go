package lab

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// bearerFor is a Dex-shaped bearer the fake reads the login from: a JWT
// whose claims carry the e-mail; the fake verifies no signature.
func bearerFor(email string) string {
	claims, _ := json.Marshal(struct {
		Email string `json:"email"`
	}{email})
	return "Bearer eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func registryGet(t *testing.T, srv *httptest.Server, path, bearer, accept string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// The registry serves the fixture as the manager reads it: GET /user only
// with a bearer, the recursive tree at HEAD, each blob raw by sha, another
// repository 404 and recorded as a miss.
func TestRegistryGitHubServesTheFixture(t *testing.T) {
	f, err := loadRegistryFixture()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newRegistryGitHub(f).handler())
	defer srv.Close()
	bearer := bearerFor(testLogin + "@lab.local")

	if code, _ := registryGet(t, srv, githubFakeAPIPath+"/user", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /user without a bearer = %d, want 401", code)
	}
	code, body := registryGet(t, srv, githubFakeAPIPath+"/user", bearer, "")
	var user struct{ Login string }
	if code != http.StatusOK || json.Unmarshal(body, &user) != nil || user.Login != testLogin {
		t.Fatalf("GET /user = %d %s, want admin", code, body)
	}

	code, body = registryGet(t, srv, githubFakeAPIPath+"/repos/"+pmRegistryRepo+"/git/trees/HEAD?recursive=1", bearer, "")
	var tree struct {
		Tree []struct{ Path, SHA, Type string }
	}
	if code != http.StatusOK || json.Unmarshal(body, &tree) != nil || len(tree.Tree) != 1 || tree.Tree[0].Path != "catalog/installations.yaml" {
		t.Fatalf("the registry's tree = %d %s", code, body)
	}
	code, body = registryGet(t, srv, githubFakeAPIPath+"/repos/"+pmRegistryRepo+"/git/blobs/"+tree.Tree[0].SHA, bearer, "application/vnd.github.v3.raw")
	if code != http.StatusOK || string(body) != f.Repositories[pmRegistryRepo]["catalog/installations.yaml"] {
		t.Fatalf("the catalog's raw blob = %d %.200s", code, body)
	}

	if code, _ := registryGet(t, srv, githubFakeAPIPath+"/repos/agentlab/unknown/git/trees/HEAD", bearer, ""); code != http.StatusNotFound {
		t.Fatalf("an unknown repository = %d, want 404", code)
	}
	_, body = registryGet(t, srv, registryFakeMissesPath, "", "")
	if !strings.Contains(string(body), "/repos/agentlab/unknown/git/trees/HEAD") {
		t.Fatalf("the misses do not record the unknown repository: %s", body)
	}
}

// The fixture's installation is the manager's workspaces case: its
// agent-platform values on record turn workspaces on, and the collection
// pins a dex-app that takes the extra redirect URIs.
func TestPlatformManagerFixtureTurnsWorkspacesOn(t *testing.T) {
	f, err := loadRegistryFixture()
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Workspaces struct{ Enabled bool }
	}
	if err := yaml.Unmarshal([]byte(f.Repositories["agentlab/labco-configs"][pmValuesPatch]), &values); err != nil || !values.Workspaces.Enabled {
		t.Fatalf("%s does not turn workspaces on: %v", pmValuesPatch, err)
	}
	if !strings.Contains(f.Repositories["agentlab/labco-management-clusters"]["management-clusters/"+pmInstallation+"/collections/kustomization.yaml"], "value: 3.3.0") {
		t.Fatal("the collection does not pin dex-app 3.3.0")
	}
}

// A dry run giantswarm-platform-manager answered for the fixture
// (testdata/platform-manager/dry-run.json, recorded from the release and
// trimmed to what the proof reads) passes; one without the sign-in URI, or
// with the workspace-manager values dropped, fails naming it.
func TestPMDryRunAssertsTheWorkspacesChanges(t *testing.T) {
	raw, err := os.ReadFile("testdata/platform-manager/dry-run.json")
	if err != nil {
		t.Fatal(err)
	}
	decode := func(s string) pmDryRun {
		var p pmDryRun
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plan := decode(string(raw))
	if err := plan.assertWorkspaces(); err != nil {
		t.Fatal(err)
	}
	if lines := plan.proofLines(); len(lines) != 4 || !strings.Contains(lines[2], pmSignInURI) {
		t.Fatalf("proof lines: %v", lines)
	}

	noURI := decode(strings.ReplaceAll(string(raw), pmSignInURI, "https://elsewhere.example/signin"))
	if err := noURI.assertWorkspaces(); err == nil || !strings.Contains(err.Error(), pmSignInURI) {
		t.Fatalf("a plan without the sign-in URI: %v", err)
	}
	dropped := decode(strings.ReplaceAll(string(raw), "workspace-manager:\\n", "other:\\n"))
	if err := dropped.assertWorkspaces(); err == nil || !strings.Contains(err.Error(), "workspace-manager") {
		t.Fatalf("a plan without the workspace-manager values: %v", err)
	}
}

// The release pins the chart at the configured version, the fixture as the
// GitHub and the registry, and the lab Dex as the authorization server.
func TestPlatformManagerManifests(t *testing.T) {
	cfg := &config.Config{}
	cfg.Platform.PlatformManager.Version = "v0.70.0-rc.1"
	out, err := platformManagerManifests(cfg, cfg.PlatformManagerVersion())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if _, err := decodeManifests(out); err != nil {
		t.Fatalf("the manifests do not decode: %v\n%s", err, s)
	}
	for _, want := range []string{`semver: "0.70.0-rc.1"`, `"apiURL":"` + pmGitHubHost + githubFakeAPIPath + `"`, `"repository":"` + pmRegistryRepo + `"`,
		`"hub":"` + pmInstallation + `"`, `"issuer":"` + pmAppIssuer + `"`, "serviceAccountName: agent-platform-flux"} {
		if !strings.Contains(s, want) {
			t.Errorf("the manifests lack %s:\n%s", want, s)
		}
	}
	if v := (&config.Config{}).PlatformManagerVersion(); v != config.DefaultPlatformManagerVersion {
		t.Errorf("the default version = %q", v)
	}
}
