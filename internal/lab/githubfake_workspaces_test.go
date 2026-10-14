package lab

import (
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

	"github.com/giantswarm/agentlab/internal/config"
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
	wsBadRefresh  = "bad_refresh_token"
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
	return startWorkspaceFakeKeeping(t, "")
}

// startWorkspaceFakeKeeping starts a fake that keeps the users' tokens in
// tokenStore ("" for memory only), reading what a former fake kept there.
func startWorkspaceFakeKeeping(t *testing.T, tokenStore string) *wsFake {
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
	if g.tokenStore = tokenStore; tokenStore != "" {
		if _, err := g.loadUserTokens(); err != nil {
			t.Fatal(err)
		}
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
	jwt, _ := githubAppJWT(key, f.g.fixture.App.ID, iat, ttl)
	return jwt
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

func TestWorkspaceGitHubReplaceTopics(t *testing.T) {
	f := startWorkspaceFake(t)
	viewer := f.signIn(t, "viewer")[wsAccess].(string)
	dev := f.signIn(t, wsDev)[wsAccess].(string)
	path := "/api/v3/repos/" + wsPublic + "/topics"
	before := f.g.repos[wsPublic].pushedAt

	if resp := f.call(t, http.MethodPut, path, viewer, map[string]any{"names": []string{}}, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer's PUT topics = %d, want 403: viewer pushes nowhere", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodPut, path, dev, map[string]any{}, nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT topics without names = %d, want 422", resp.StatusCode)
	}
	var topics githubTopics
	if resp := f.call(t, http.MethodPut, path, dev, map[string]any{"names": []string{"Go", "go", " sync "}}, &topics); resp.StatusCode != http.StatusOK || !slices.Equal(topics.Names, []string{"go", "sync"}) {
		t.Fatalf("dev's PUT topics = %d %v, want [go sync]", resp.StatusCode, topics.Names)
	}
	var repos []map[string]any
	f.call(t, http.MethodGet, "/api/v3/orgs/"+wsOrg+"/repos", "", nil, &repos)
	for _, r := range repos {
		if r[wsFullName] == wsPublic && fmt.Sprint(r["topics"]) != "[go sync]" {
			t.Fatalf("the listing after the change carries topics %v", r["topics"])
		}
	}
	f.call(t, http.MethodGet, path, "", nil, &topics)
	if !slices.Equal(topics.Names, []string{"go", "sync"}) {
		t.Fatalf("GET topics = %v", topics.Names)
	}
	if !f.g.repos[wsPublic].pushedAt.Equal(before) {
		t.Fatal("a topic change moved pushed_at: it is no push")
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/repos/"+wsPrivate+"/topics", dev, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dev's GET of %s's topics = %d, want 404: dev does not read it", wsPrivate, resp.StatusCode)
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
	if out := f.redeem(t, refresh); out[wsError] != wsBadRefresh {
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
	if out := f.redeem(t, url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(tokens[wsRefresh])}}); out[wsError] != wsBadRefresh {
		t.Fatalf("the refresh token after the revocation = %v, want bad_refresh_token", out)
	}
	if last := f.g.requests[len(f.g.requests)-2]; last.Credential != credRevoked || last.User != wsDev {
		t.Fatalf("the request log names the revoked call %+v, want credential revoked of dev", last)
	}
}

// TestWorkspaceGitHubTokensSurviveReplacement is `agentlab platform`
// replacing the lab's GitHub after a person signed in: a fake without the
// store refuses the person's tokens (the workspace-manager then answers
// connect_required), one with it honours them, a rotated refresh token and a
// revoked grant staying dead.
func TestWorkspaceGitHubTokensSurviveReplacement(t *testing.T) {
	store := filepath.Join(t.TempDir(), "user-tokens.json")
	first := startWorkspaceFakeKeeping(t, store)
	dev := first.signIn(t, wsDev)
	rotated := first.signIn(t, wsDev)
	if next := first.redeem(t, url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(rotated[wsRefresh])}}); next[wsAccess] == nil {
		t.Fatalf("a refresh = %v", next)
	}
	viewer := first.signIn(t, "viewer")[wsAccess].(string)
	body, _ := json.Marshal(map[string]string{wsAccess: viewer})
	req, _ := http.NewRequest(http.MethodDelete, first.url+"/api/v3/applications/"+wsClientID+"/grant", strings.NewReader(string(body)))
	req.SetBasicAuth(wsClientID, first.secret)
	resp, err := first.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("viewer's grant revocation = %d, want 204", resp.StatusCode)
	}

	forgetful := startWorkspaceFake(t)
	if resp := forgetful.call(t, http.MethodGet, "/api/v3/user", dev[wsAccess].(string), nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a replacement without the store: GET /user with dev's token = %d, want 401", resp.StatusCode)
	}

	second := startWorkspaceFakeKeeping(t, store)
	var user map[string]any
	if resp := second.call(t, http.MethodGet, "/api/v3/user", dev[wsAccess].(string), nil, &user); resp.StatusCode != http.StatusOK || user[githubLogin] != wsDev {
		t.Fatalf("the replacement: GET /user with dev's token = %d %v, want 200 dev", resp.StatusCode, user)
	}
	if out := second.redeem(t, url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(dev[wsRefresh])}}); out[wsAccess] == nil {
		t.Fatalf("the replacement: dev's refresh token = %v, want a new pair", out)
	}
	if out := second.redeem(t, url.Values{wsGrantType: {wsRefresh}, wsRefresh: {fmt.Sprint(rotated[wsRefresh])}}); out[wsError] != wsBadRefresh {
		t.Fatalf("the replacement: a refresh token redeemed before = %v, want bad_refresh_token", out)
	}
	if resp := second.call(t, http.MethodGet, "/api/v3/user", viewer, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the replacement: GET /user with viewer's revoked token = %d, want 401", resp.StatusCode)
	}
	if info, err := os.Stat(store); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the token store = %v %v, want mode 0600", info, err)
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

// TestWorkspaceGitHubSuspendedInstallation suspends the App's installation
// on agentlab-org as an organization owner would: the listing names the
// suspension, no new token is issued, the installation's token and the App's
// user tokens act on none of the owner's repositories while the public ones
// stay readable, the installation on agentlab-oss is untouched, and
// unsuspending gives everything back.
func TestWorkspaceGitHubSuspendedInstallation(t *testing.T) {
	f := startWorkspaceFake(t)
	jwt := f.appJWT(f.appKey, time.Now(), 9*time.Minute)
	var tok struct {
		Token string `json:"token"`
	}
	f.call(t, http.MethodPost, "/api/v3/app/installations/1/access_tokens", jwt, nil, &tok)
	admin := f.signIn(t, testLogin)[wsAccess].(string)
	work := t.TempDir()
	if out, err := f.git(t, work, "clone", "-q", f.remote(admin, wsPublic), "api"); err != nil {
		t.Fatalf("clone of %s: %v: %s", wsPublic, err, out)
	}
	if out, err := f.git(t, work, "clone", "-q", f.remote(admin, "agentlab-oss/kagent"), "kagent"); err != nil {
		t.Fatalf("clone of agentlab-oss/kagent: %v: %s", err, out)
	}
	push := func(dir, name string) error {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := f.git(t, filepath.Join(work, dir), "add", "."); err != nil {
			t.Fatalf("add: %v: %s", err, out)
		}
		if out, err := f.git(t, filepath.Join(work, dir), "commit", "-q", "-m", name); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
		_, err := f.git(t, filepath.Join(work, dir), "push", "-q", "origin", "HEAD:"+wsMain)
		return err
	}

	if resp := f.call(t, http.MethodPut, "/api/v3/app/installations/1/suspended", admin, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a suspension with a user's token = %d, want 401", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodPut, "/api/v3/app/installations/99/suspended", jwt, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("suspending an unknown installation = %d, want 404", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodPut, "/api/v3/app/installations/1/suspended", jwt, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("suspend = %d, want 204", resp.StatusCode)
	}

	var installations []map[string]any
	f.call(t, http.MethodGet, "/api/v3/app/installations", jwt, nil, &installations)
	for _, i := range installations {
		suspended := i["suspended_at"] != nil
		if want := i["id"] == float64(1); suspended != want {
			t.Errorf("installation %v: suspended_at = %v, want set %v", i["id"], i["suspended_at"], want)
		}
	}
	if resp := f.call(t, http.MethodPost, "/api/v3/app/installations/1/access_tokens", jwt, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a token of the suspended installation = %d, want 403", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/installation/repositories", tok.Token, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the suspended installation's token = %d, want 403", resp.StatusCode)
	}
	if err := push("api", "while-suspended"); err == nil {
		t.Fatalf("the admin's user token pushed to %s while the installation on its owner is suspended", wsPublic)
	}
	if out, err := f.git(t, work, "clone", "-q", f.remote(admin, wsPrivate), "infra"); err == nil {
		t.Fatalf("the admin's user token cloned the private %s while suspended: %s", wsPrivate, out)
	}
	if out, err := f.git(t, work, "clone", "-q", f.remote("", wsPublic), "api-anon"); err != nil {
		t.Fatalf("the public %s is no longer readable while suspended: %v: %s", wsPublic, err, out)
	}
	if err := push("kagent", "other-owner"); err != nil {
		t.Fatalf("a push to agentlab-oss, whose installation is not suspended: %v", err)
	}

	if resp := f.call(t, http.MethodDelete, "/api/v3/app/installations/1/suspended", jwt, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unsuspend = %d, want 204", resp.StatusCode)
	}
	if resp := f.call(t, http.MethodGet, "/api/v3/installation/repositories", tok.Token, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the installation's token after unsuspending = %d, want 200", resp.StatusCode)
	}
	if _, err := f.git(t, filepath.Join(work, "api"), "push", "-q", "origin", "HEAD:"+wsMain); err != nil {
		t.Fatalf("the admin's push after unsuspending: %v", err)
	}
}

// unsignedJWT is a JWT with the claims and no valid signature: what the fake
// sees of a token it did not issue.
func unsignedJWT(claims map[string]any) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "ES256", "typ": "JWT"}) + "." + enc(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("signature"))
}

// On an API host the fake serves the REST API at the root, shaped like
// api.github.com: the answers' web URLs name the git host, their API URLs
// the API host. A JWT it did not issue is refused and logged with its aud
// and sub, under the host and the path as they came.
func TestWorkspaceGitHubAPIHost(t *testing.T) {
	f := startWorkspaceFake(t)
	apiHost, gitHost := config.GitHubFakeBearerAPIHost(wsDomain), config.GitHubFakeBearerHost(wsDomain)
	f.g.apiHosts = []string{apiHost}
	call := func(path, credential string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, f.url+path, nil)
		req.Host = apiHost
		if credential != "" {
			req.Header.Set(wsAuthHeader, "Bearer "+credential)
		}
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if out != nil && resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}
	var app map[string]any
	if status := call("/app", f.appJWT(f.appKey, time.Now(), 9*time.Minute), &app); status != http.StatusOK || app["client_id"] != wsClientID {
		t.Fatalf("GET /app on %s = %d %v", apiHost, status, app)
	}
	var repos []map[string]any
	if status := call("/orgs/"+wsOrg+"/repos?per_page=1", "", &repos); status != http.StatusOK || len(repos) != 1 {
		t.Fatalf("GET /orgs/%s/repos on %s = %d %v", wsOrg, apiHost, status, repos)
	}
	name := repos[0][wsFullName].(string)
	if got, want := repos[0]["html_url"], "https://"+gitHost+"/"+name; got != want {
		t.Errorf("html_url = %v, want %s", got, want)
	}
	if got, want := repos[0]["url"], "https://"+apiHost+"/repos/"+name; got != want {
		t.Errorf("url = %v, want %s", got, want)
	}

	const sub = "actor/kagent/session-s1"
	if status := call("/agentlab/caller-credential/t1", unsignedJWT(map[string]any{"aud": []string{config.WorkspaceProviderFakeBearer}, claimSubject: sub}), nil); status != http.StatusUnauthorized {
		t.Errorf("a JWT the fake did not issue = %d, want 401", status)
	}
	if status := call("/agentlab/caller-credential/t2", unsignedJWT(map[string]any{"aud": "one", claimSubject: sub}), nil); status != http.StatusUnauthorized {
		t.Errorf("a JWT with a string aud = %d, want 401", status)
	}
	if status := call("/agentlab/caller-credential/t3", "agentlab-placeholder", nil); status != http.StatusUnauthorized {
		t.Errorf("the placeholder = %d, want 401", status)
	}
	f.g.mu.Lock()
	reqs := append([]githubRequest{}, f.g.requests...)
	f.g.mu.Unlock()
	for path, want := range map[string]githubRequest{
		"/agentlab/caller-credential/t1": {Credential: credJWT, Audience: []string{config.WorkspaceProviderFakeBearer}, Subject: sub},
		"/agentlab/caller-credential/t2": {Credential: credJWT, Audience: []string{"one"}, Subject: sub},
		"/agentlab/caller-credential/t3": {Credential: credPlaceholder},
	} {
		got := requestsTo(reqs, apiHost, path)
		if len(got) != 1 || got[0].Credential != want.Credential || !slices.Equal(got[0].Audience, want.Audience) || got[0].Subject != want.Subject {
			t.Errorf("the log of %s%s = %+v, want credential %s aud %v sub %q", apiHost, path, got, want.Credential, want.Audience, want.Subject)
		}
	}
	// The git host's API stays under /api/v3, its log under its own name.
	if resp := f.call(t, http.MethodGet, "/api/v3/meta", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/v3/meta on the test host = %d", resp.StatusCode)
	}
}
