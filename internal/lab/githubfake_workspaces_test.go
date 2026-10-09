package lab

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The workspace fake's test fixture names (templates/github-fixture.yaml).
const (
	wsOrg         = "agentlab-org"
	wsPublic      = wsOrg + "/platform-api"
	wsPrivate     = wsOrg + "/platform-infra"
	wsClientID    = "Iv1.agentlab-workspaces"
	wsDomain      = "127.0.0.1.nip.io"
	wsRedirect    = "http://localhost/callback"
	wsVerifier    = "a-verifier-of-at-least-forty-three-characters-0123456789"
	wsAccept      = "Accept"
	wsJSON        = "application/json"
	wsAccess      = "access_token"
	wsRefresh     = "refresh_token"
	wsGrantType   = "grant_type"
	wsError       = "error"
	wsPushedAt    = "pushed_at"
	wsFullName    = "full_name"
	wsAuthHeader  = "Authorization"
	wsDev         = "dev"
	wsCode        = "code"
	wsChange      = "change"
	wsMain        = "main"
	wsVerifierKey = "code_verifier"
	wsRedirectKey = "redirect_uri"
)

// wsFake is the fake served over TLS from the lab's own credentials, in a
// lab directory of the test's own.
type wsFake struct {
	g      *workspaceGitHub
	url    string
	host   string
	client *http.Client
	caPath string
	appKey *rsa.PrivateKey
	secret string
}

func startWorkspaceFake(t *testing.T) *wsFake {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the workspace fake serves git with git itself; git is not on PATH")
	}
	t.Chdir(t.TempDir())
	if err := GenCerts(wsDomain, false); err != nil {
		t.Fatal(err)
	}
	dir, err := EnsureGitHubFakeCredentials(wsDomain)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := loadGitHubFixture("")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := readRSAPublicKey(filepath.Join(dir, githubFakeAppPublicKey))
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := os.ReadFile(filepath.Join(dir, githubFakeClientSecret)) // #nosec G304 -- the test's own credentials
	g, err := newWorkspaceGitHub(fixture, pub, string(secret), t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, githubFakeTLSCert), filepath.Join(dir, githubFakeTLSKey))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(g.handler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	caPEM, _ := os.ReadFile(caCertPath)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	keyPEM, _ := os.ReadFile(filepath.Join(dir, githubFakeAppKey)) // #nosec G304 -- the test's own credentials
	block, _ := pem.Decode(keyPEM)
	appKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caPath, _ := filepath.Abs(caCertPath)
	return &wsFake{g: g, url: srv.URL, host: strings.TrimPrefix(srv.URL, "https://"), caPath: caPath, appKey: appKey, secret: string(secret),
		client: &http.Client{
			Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}
}

// call sends method path with the credential (a bearer; "" for none) and
// the JSON body, and decodes the answer into out when it is not nil.
func (f *wsFake) call(t *testing.T, method, path, credential string, body any, out any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, f.url+path, reader)
	if credential != "" {
		req.Header.Set(wsAuthHeader, "Bearer "+credential)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decoding: %v", method, path, err)
		}
	}
	return resp
}

// appJWT is a JWT of the App signed by key, issued at iat, living ttl.
func (f *wsFake) appJWT(key *rsa.PrivateKey, iat time.Time, ttl time.Duration) string {
	enc := func(v any) string { raw, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(raw) }
	signing := enc(map[string]string{"alg": "RS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"iss": fmt.Sprint(f.g.fixture.App.ID), "iat": iat.Unix(), "exp": iat.Add(ttl).Unix()})
	digest := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func fullNames(repos []map[string]any) []string {
	var out []string
	for _, r := range repos {
		out = append(out, r[wsFullName].(string))
	}
	return out
}

func TestWorkspaceGitHubAppToken(t *testing.T) {
	f := startWorkspaceFake(t)
	jwt := f.appJWT(f.appKey, time.Now(), 9*time.Minute)

	var app map[string]any
	if resp := f.call(t, http.MethodGet, "/api/v3/app", jwt, nil, &app); resp.StatusCode != http.StatusOK || app["client_id"] != wsClientID {
		t.Fatalf("GET /app = %d %v", resp.StatusCode, app)
	}
	var inst map[string]any
	if resp := f.call(t, http.MethodGet, "/api/v3/orgs/"+wsOrg+"/installation", jwt, nil, &inst); resp.StatusCode != http.StatusOK || inst["id"] != float64(1) {
		t.Fatalf("GET /orgs/%s/installation = %d %v", wsOrg, resp.StatusCode, inst)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/repos/"+wsPrivate+"/installation", jwt, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /repos/%s/installation = %d", wsPrivate, resp.StatusCode)
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, bad := range map[string]string{
		"another key":    f.appJWT(other, time.Now(), 9*time.Minute),
		"expired":        f.appJWT(f.appKey, time.Now().Add(-20*time.Minute), 9*time.Minute),
		"too long-lived": f.appJWT(f.appKey, time.Now(), time.Hour),
	} {
		if resp := f.call(t, http.MethodPost, "/api/v3/app/installations/1/access_tokens", bad, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("an App JWT of %s: access_tokens = %d, want 401", name, resp.StatusCode)
		}
	}

	var tok struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if resp := f.call(t, http.MethodPost, "/api/v3/app/installations/1/access_tokens", jwt, nil, &tok); resp.StatusCode != http.StatusCreated || !strings.HasPrefix(tok.Token, "ghs_") {
		t.Fatalf("access_tokens = %d %+v", resp.StatusCode, tok)
	}
	var listing struct {
		TotalCount   int              `json:"total_count"`
		Repositories []map[string]any `json:"repositories"`
	}
	f.call(t, http.MethodGet, "/api/v3/installation/repositories", tok.Token, nil, &listing)
	if names := fullNames(listing.Repositories); !slices.Contains(names, wsPrivate) || listing.TotalCount != 4 || slices.ContainsFunc(names, func(n string) bool { return !strings.HasPrefix(n, wsOrg+"/") }) {
		t.Fatalf("the installation's repositories = %d %v, want agentlab-org's four, the private one included", listing.TotalCount, names)
	}

	var scoped struct {
		Token string `json:"token"`
	}
	f.call(t, http.MethodPost, "/api/v3/app/installations/1/access_tokens", jwt, map[string]any{"repositories": []string{"platform-api"}}, &scoped)
	f.call(t, http.MethodGet, "/api/v3/installation/repositories", scoped.Token, nil, &listing)
	if names := fullNames(listing.Repositories); !slices.Equal(names, []string{wsPublic}) {
		t.Fatalf("a token limited to platform-api lists %v", names)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/installation/repositories", jwt, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("installation/repositories with the App's JWT = %d, want 401", resp.StatusCode)
	}
}

func TestWorkspaceGitHubListing(t *testing.T) {
	f := startWorkspaceFake(t)
	var repos []map[string]any
	f.call(t, http.MethodGet, "/api/v3/orgs/"+wsOrg+"/repos", "", nil, &repos)
	if names := fullNames(repos); slices.Contains(names, wsPrivate) || len(names) != 3 {
		t.Fatalf("an anonymous listing of %s = %v, want its three public repositories", wsOrg, names)
	}
	byName := map[string]map[string]any{}
	for _, r := range repos {
		byName[r[wsFullName].(string)] = r
	}
	legacy := byName[wsOrg+"/legacy-tool"]
	if legacy["archived"] != true || legacy["language"] != "Python" || legacy["fork"] != false ||
		fmt.Sprint(legacy["topics"]) != "[legacy]" || legacy[wsPushedAt] != "2025-01-10T10:00:00Z" {
		t.Fatalf("legacy-tool's fields = %v", legacy)
	}
	if size := byName[wsOrg+"/monorepo"]["size"].(float64); size < 1024 {
		t.Fatalf("monorepo's size = %v KiB, want its generated MiB", size)
	}

	f.call(t, http.MethodGet, "/api/v3/users/agentlab-oss/repos?type=forks", "", nil, &repos)
	if names := fullNames(repos); !slices.Equal(names, []string{"agentlab-oss/kagent"}) || repos[0]["fork"] != true {
		t.Fatalf("type=forks = %v", names)
	}
	f.call(t, http.MethodGet, "/api/v3/orgs/"+wsOrg+"/repos?sort=pushed", "", nil, &repos)
	if names := fullNames(repos); !slices.Equal(names, []string{wsOrg + "/monorepo", wsPublic, wsOrg + "/legacy-tool"}) {
		t.Fatalf("sort=pushed = %v, want the newest push first", names)
	}
	resp := f.call(t, http.MethodGet, "/api/v3/orgs/"+wsOrg+"/repos?per_page=2", "", nil, &repos)
	if len(repos) != 2 || !strings.Contains(resp.Header.Get("Link"), `page=2`) {
		t.Fatalf("per_page=2 = %d repositories, Link %q", len(repos), resp.Header.Get("Link"))
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/orgs/nobody/repos", "", nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown owner = %d, want 404", resp.StatusCode)
	}
}

// signIn runs the OAuth flow for login and returns the token answer.
func (f *wsFake) signIn(t *testing.T, login string) map[string]any {
	t.Helper()
	code := f.authorize(t, login, wsVerifier)
	return f.redeem(t, url.Values{wsCode: {code}, wsVerifierKey: {wsVerifier}, wsRedirectKey: {wsRedirect}})
}

// authorize asks for a code with the challenge of verifier.
func (f *wsFake) authorize(t *testing.T, login, verifier string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {wsClientID}, wsRedirectKey: {wsRedirect}, "state": {"st"}, "login": {login},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	resp := f.call(t, http.MethodGet, "/login/oauth/authorize?"+q.Encode(), "", nil, nil)
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || loc.Query().Get("state") != "st" || loc.Query().Get(wsCode) == "" {
		t.Fatalf("authorize for %s = %d, Location %q", login, resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc.Query().Get(wsCode)
}

// redeem posts the token request with the client's credentials.
func (f *wsFake) redeem(t *testing.T, form url.Values) map[string]any {
	t.Helper()
	form.Set("client_id", wsClientID)
	form.Set("client_secret", f.secret)
	req, _ := http.NewRequest(http.MethodPost, f.url+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(wsAccept, wsJSON)
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWorkspaceGitHubOAuthWithPKCE(t *testing.T) {
	f := startWorkspaceFake(t)
	noPKCE := url.Values{"client_id": {wsClientID}, wsRedirectKey: {wsRedirect}, "login": {wsDev}}
	if resp := f.call(t, http.MethodGet, "/login/oauth/authorize?"+noPKCE.Encode(), "", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("authorize without PKCE = %d, want 400", resp.StatusCode)
	}
	code := f.authorize(t, wsDev, wsVerifier)
	if out := f.redeem(t, url.Values{wsCode: {code}, wsVerifierKey: {"another-verifier"}}); out[wsError] != "bad_verification_code" {
		t.Fatalf("a wrong verifier = %v, want bad_verification_code", out)
	}
	if out := f.redeem(t, url.Values{wsCode: {code}, wsVerifierKey: {wsVerifier}}); out[wsError] != "bad_verification_code" {
		t.Fatalf("a code redeemed a second time = %v, want bad_verification_code", out)
	}
	tokens := f.signIn(t, wsDev)
	access, _ := tokens[wsAccess].(string)
	if !strings.HasPrefix(access, "ghu_") || !strings.HasPrefix(fmt.Sprint(tokens[wsRefresh]), "ghr_") || tokens["expires_in"] != float64(28800) {
		t.Fatalf("the token answer = %v", tokens)
	}
	var user map[string]any
	if resp := f.call(t, http.MethodGet, "/api/v3/user", access, nil, &user); resp.StatusCode != http.StatusOK || user[githubLogin] != wsDev {
		t.Fatalf("GET /user = %d %v", resp.StatusCode, user)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/user", fmt.Sprint(tokens[wsRefresh]), nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /user with the refresh token = %d, want 401", resp.StatusCode)
	}
	wrongSecret := url.Values{wsCode: {f.authorize(t, wsDev, wsVerifier)}, wsVerifierKey: {wsVerifier}}
	wrongSecret.Set("client_id", wsClientID)
	wrongSecret.Set("client_secret", "wrong")
	req, _ := http.NewRequest(http.MethodPost, f.url+"/login/oauth/access_token?"+wrongSecret.Encode(), nil)
	req.Header.Set(wsAccept, wsJSON)
	resp, _ := f.client.Do(req)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if out[wsError] != "incorrect_client_credentials" {
		t.Fatalf("a wrong client secret = %v", out)
	}
}

func TestWorkspaceGitHubRefreshRotation(t *testing.T) {
	f := startWorkspaceFake(t)
	first := f.signIn(t, wsDev)
	refresh := url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(first[wsRefresh])}}
	second := f.redeem(t, refresh)
	if second[wsAccess] == nil || second[wsRefresh] == first[wsRefresh] {
		t.Fatalf("a refresh = %v, want a new access and refresh token", second)
	}
	if out := f.redeem(t, refresh); out[wsError] != "bad_refresh_token" {
		t.Fatalf("the refresh token's second redemption = %v, want bad_refresh_token", out)
	}
	if out := f.redeem(t, url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(second[wsRefresh])}}); out[wsAccess] == nil {
		t.Fatalf("the rotated refresh token = %v, want a new pair", out)
	}
}

func TestWorkspaceGitHubRevocation(t *testing.T) {
	f := startWorkspaceFake(t)
	tokens := f.signIn(t, wsDev)
	access := tokens[wsAccess].(string)
	revoke := func(user, secret string) int {
		body, _ := json.Marshal(map[string]string{wsAccess: access})
		req, _ := http.NewRequest(http.MethodDelete, f.url+"/api/v3/applications/"+wsClientID+"/grant", strings.NewReader(string(body)))
		req.SetBasicAuth(user, secret)
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if status := revoke(wsClientID, "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("a revocation with a wrong secret = %d, want 401", status)
	}
	if status := revoke(wsClientID, f.secret); status != http.StatusNoContent {
		t.Fatalf("the grant revocation = %d, want 204", status)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/user", access, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /user after the revocation = %d, want 401", resp.StatusCode)
	}
	if out := f.redeem(t, url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(tokens[wsRefresh])}}); out[wsError] != "bad_refresh_token" {
		t.Fatalf("the refresh token after the revocation = %v, want bad_refresh_token", out)
	}
	if last := f.g.requests[len(f.g.requests)-2]; last.Credential != credRevoked || last.User != wsDev {
		t.Fatalf("the request log names the revoked call %+v, want credential revoked of dev", last)
	}
}

func TestWorkspaceGitHubPerUserVisibility(t *testing.T) {
	f := startWorkspaceFake(t)
	admin := f.signIn(t, testLogin)[wsAccess].(string)
	dev := f.signIn(t, wsDev)[wsAccess].(string)
	for _, c := range []struct {
		who, token string
		want       int
	}{{"anonymous", "", http.StatusNotFound}, {wsDev, dev, http.StatusNotFound}, {testLogin, admin, http.StatusOK}, {"a placeholder", "placeholder-token", http.StatusUnauthorized}} {
		if resp := f.call(t, http.MethodGet, "/api/v3/repos/"+wsPrivate, c.token, nil, nil); resp.StatusCode != c.want {
			t.Errorf("GET /repos/%s as %s = %d, want %d", wsPrivate, c.who, resp.StatusCode, c.want)
		}
	}
	var repos []map[string]any
	f.call(t, http.MethodGet, "/api/v3/user/repos", dev, nil, &repos)
	if names := fullNames(repos); slices.Contains(names, wsPrivate) || !slices.Contains(names, wsPublic) {
		t.Fatalf("dev's repositories = %v", names)
	}
	f.call(t, http.MethodGet, "/api/v3/user/repos?visibility=private", admin, nil, &repos)
	if names := fullNames(repos); !slices.Equal(names, []string{wsPrivate}) {
		t.Fatalf("admin's private repositories = %v", names)
	}
	if !slices.ContainsFunc(f.g.requests, func(r githubRequest) bool {
		return r.Credential == credPlaceholder && r.Path == "/api/v3/repos/"+wsPrivate && r.User == ""
	}) {
		t.Fatalf("the request log has no placeholder call: %+v", f.g.requests)
	}
}

// git runs git in dir against the fake, trusting the lab CA, without the
// host's configuration or a credential prompt.
func (f *wsFake) git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	// #nosec G204 -- the test's own git arguments
	cmd := exec.Command("git", append([]string{"-c", "http.sslCAInfo=" + f.caPath, "-c", "commit.gpgsign=false",
		"-c", "user.name=test", "-c", "user.email=test@lab.local"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *wsFake) remote(token, repo string) string {
	if token == "" {
		return f.url + "/" + repo + ".git"
	}
	return "https://x-access-token:" + token + "@" + f.host + "/" + repo + ".git"
}

func TestWorkspaceGitHubPushMovesPushedAt(t *testing.T) {
	f := startWorkspaceFake(t)
	dev := f.signIn(t, wsDev)[wsAccess].(string)
	viewer := f.signIn(t, "viewer")[wsAccess].(string)
	admin := f.signIn(t, testLogin)[wsAccess].(string)
	work := t.TempDir()

	if out, err := f.git(t, work, "clone", "-q", f.remote("", wsPrivate), "anon"); err == nil {
		t.Fatalf("an anonymous clone of %s succeeded: %s", wsPrivate, out)
	}
	if out, err := f.git(t, work, "clone", "-q", f.remote(dev, wsPrivate), "dev-private"); err == nil || !strings.Contains(out, "not found") {
		t.Fatalf("dev's clone of %s = %v: %s, want not found", wsPrivate, err, out)
	}
	if out, err := f.git(t, work, "clone", "-q", f.remote(admin, wsPrivate), "admin-private"); err != nil {
		t.Fatalf("admin's clone of %s: %v: %s", wsPrivate, err, out)
	}
	clone := filepath.Join(work, "clone")
	if out, err := f.git(t, work, "clone", "-q", f.remote("", wsPublic), clone); err != nil {
		t.Fatalf("an anonymous clone of %s: %v: %s", wsPublic, err, out)
	}
	if err := os.WriteFile(filepath.Join(clone, "change.txt"), []byte("change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"checkout", "-qb", "feature"}, {"add", "change.txt"}, {"commit", "-qm", wsChange}} {
		if out, err := f.git(t, clone, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	before := f.g.repos[wsPublic].pushedAt
	if out, err := f.git(t, clone, "push", "-q", f.remote("", wsPublic), "feature"); err == nil {
		t.Fatalf("an anonymous push succeeded: %s", out)
	}
	if out, err := f.git(t, clone, "push", "-q", f.remote(viewer, wsPublic), "feature"); err == nil || !strings.Contains(out, "denied to viewer") {
		t.Fatalf("viewer's push = %v: %s, want the refusal naming viewer", err, out)
	}
	if !f.g.repos[wsPublic].pushedAt.Equal(before) {
		t.Fatal("a refused push moved pushed_at")
	}
	if out, err := f.git(t, clone, "push", "-q", f.remote(dev, wsPublic), "feature"); err != nil {
		t.Fatalf("dev's push: %v: %s", err, out)
	}
	var repo map[string]any
	f.call(t, http.MethodGet, "/api/v3/repos/"+wsPublic, dev, nil, &repo)
	pushed, _ := time.Parse(time.RFC3339, fmt.Sprint(repo[wsPushedAt]))
	if !pushed.After(before) || time.Since(pushed) > time.Minute {
		t.Fatalf("pushed_at after the push = %v, before %v", repo[wsPushedAt], before)
	}

	var pr map[string]any
	pullBody := map[string]string{githubTitle: wsChange, "head": "feature", githubBase: wsMain}
	if resp := f.call(t, http.MethodPost, "/api/v3/repos/"+wsPublic+"/pulls", dev, pullBody, &pr); resp.StatusCode != http.StatusCreated {
		t.Fatalf("opening the pull request = %d %v", resp.StatusCode, pr)
	}
	if resp := f.call(t, http.MethodPost, "/api/v3/repos/"+wsPublic+"/pulls", "", pullBody, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous pull request = %d, want 401", resp.StatusCode)
	}
	if pulls := f.g.repos[wsPublic].pulls; len(pulls) != 1 || pulls[0].Author != wsDev {
		t.Fatalf("the pull requests = %+v, want one by dev", pulls)
	}

	for _, req := range f.g.requests {
		for _, secret := range []string{dev, viewer, admin, f.secret} {
			if strings.Contains(req.Path, secret) {
				t.Fatalf("the request log carries a token: %+v", req)
			}
		}
	}
}
