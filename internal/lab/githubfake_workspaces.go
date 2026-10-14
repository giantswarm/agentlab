package lab

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The GitHub of the workspace proofs, headless: what a workspace manager's
// provider instance pointed at a GitHub Enterprise (API base
// https://<fake>/api/v3, git base https://<fake>) needs end to end, over the
// repositories, owners and users of a fixture (githubfake_fixture.go):
//
//   - the App: its installation per owner and installation tokens, the App's
//     JWT checked against the public key the lab generated; an installation
//     suspended and unsuspended as an organization owner does it, after
//     which the installation's tokens and the App's user tokens act on none
//     of the owner's repositories (public ones stay readable);
//   - repository listings with the fields their filters read (language,
//     topics, archived, fork, pushed_at), per caller's visibility;
//   - the user-to-server OAuth flow: authorize with PKCE consenting at once
//     for the lab user a login hint names, expiring access tokens, refresh
//     tokens that rotate (a second redemption is refused), grant and token
//     revocation, GET /user;
//   - git's smart HTTP served by git itself, Basic auth checked per
//     user and repository, a push moving pushed_at (githubfake_git.go);
//   - pull requests over REST and the GraphQL calls of `gh pr create`
//     (githubfake_pulls.go);
//   - a request log the proofs read: host, method, path, user and the kind
//     of credential, never a token; a JWT bearer the fake did not issue (a
//     Session Actor's, injected by Substrate's egress gateway) with its aud
//     and sub claims;
//   - besides the GitHub Enterprise shape, a github.com shape on the hosts
//     WorkspaceGitHubOptions.APIHosts names: the REST API at the root of
//     api.<git host>, the git host's name in its answers' web URLs.
//
// It is a separate server from the commit proof's fake (githubfake.go): that
// one keeps git's objects in memory for the REST git data API, this one keeps
// real repositories on disk for git itself.

// The fake's read-back of its request log, and the GitHub Enterprise version
// GET /meta reports (gh reads it).
const (
	githubFakeRequestsPath      = "/_fake/requests"
	githubFakeEnterpriseVersion = "3.17.0"
)

// Words of OAuth's requests and answers.
const (
	oauthError = "error"
	oauthScope = "scope"
	httpScheme = "http"
)

// githubPermissions is the key of an installation's and a repository's
// permissions.
const githubPermissions = "permissions"

// githubFakeSuspender is who the fake names as having suspended an
// installation: on GitHub an owner of the organization, here the call with
// the App's JWT that stands in for the owner's click.
const githubFakeSuspender = "lab-owner"

// The kinds of credential a request carries, as the request log names them:
// none; the App's JWT; an installation token; a user's token; the App's
// OAuth client credentials (revocation's Basic auth); one the fake issued
// that has expired or was revoked; and anything else (the sandbox's
// placeholder that the egress gateway did not replace); and a JWT the fake
// did not issue, logged with its aud and sub. The last three are refused
// with GitHub's 401 wherever they are presented: the fake verifies no JWT it
// did not sign.
const (
	credNone         = "none"
	credApp          = "app-jwt"
	credInstallation = "installation-token"
	credUser         = "user-token"
	credClient       = "oauth-client" // #nosec G101 -- the name of a credential kind
	credRevoked      = "revoked-token"
	credPlaceholder  = "placeholder"
	credJWT          = "jwt"
)

// Token lifetimes, GitHub's own.
const (
	githubInstallationTokenTTL = time.Hour
	githubUserTokenTTL         = 8 * time.Hour
	githubRefreshTokenTTL      = 184 * 24 * time.Hour
	githubOAuthCodeTTL         = 10 * time.Minute
	githubAppJWTMaxTTL         = 10 * time.Minute
	githubClockSkew            = time.Minute
)

// workspaceRepo is a fixture repository and what changes on it.
type workspaceRepo struct {
	githubFixtureRepo
	id       int
	sizeKiB  int
	pushedAt time.Time
	pulls    []*workspacePull
	// generated is a repository added at runtime (githubfake_generated.go),
	// the only kind that can be removed again.
	generated bool
}

// githubToken is a token the fake issued: an installation's (Installation,
// limited to Repos when set) or a user's (Login), an access or a refresh
// token.
type githubToken struct {
	kind         string
	login        string
	installation int64
	repos        []string
	refresh      bool
	expires      time.Time
	dead         bool
}

// oauthCode is an authorization code waiting for its redemption.
type oauthCode struct {
	login, redirectURI, challenge string
	expires                       time.Time
}

// githubCaller is who a request is from.
type githubCaller struct {
	kind         string
	login        string
	installation int64
	repos        []string
	// audience and subject are a JWT bearer's aud and sub claims.
	audience []string
	subject  string
}

// githubRequest is one line of the request log.
type githubRequest struct {
	Time       time.Time `json:"time"`
	Host       string    `json:"host,omitempty"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	User       string    `json:"user,omitempty"`
	Credential string    `json:"credential"`
	// Audience and Subject are a JWT bearer's aud and sub, never the token.
	Audience []string `json:"aud,omitempty"`
	Subject  string   `json:"sub,omitempty"`
}

// workspaceGitHub is the workspace proofs' GitHub.
type workspaceGitHub struct {
	fixture      *githubFixture
	appKey       *rsa.PublicKey
	clientSecret string
	git          *githubGit
	now          func() time.Time
	// apiHosts are the hosts that serve the REST API at their root, each
	// api.<git host>, as api.github.com does.
	apiHosts []string
	// tokenStore is the file the users' tokens are kept in across a
	// replacement of the fake (WorkspaceGitHubOptions.TokenStore), "" for
	// none.
	tokenStore string

	// suspended is when each suspended installation was suspended, under
	// its own lock: installationJSON reads it with mu held and without.
	suspendMu sync.Mutex
	suspended map[int64]time.Time

	mu       sync.Mutex
	repos    map[string]*workspaceRepo // owner/name → repository
	tokens   map[string]*githubToken
	codes    map[string]*oauthCode
	requests []githubRequest
}

// WorkspaceGitHubOptions configures `agentlab github-fake --workspaces`.
type WorkspaceGitHubOptions struct {
	Listen string
	// Fixture is the fixture file, "" for the embedded default.
	Fixture string
	// Credentials is the directory EnsureGitHubFakeCredentials generated.
	Credentials string
	// DataDir holds the bare repositories, a temporary directory for "".
	DataDir string
	// LargeRepoMiB overrides the generated size of the fixture's large
	// repositories; 0 keeps the fixture's.
	LargeRepoMiB int
	// APIHosts are host names, each api.<git host>, on which the fake serves
	// the REST API at the root, shaped like github.com's api.github.com.
	APIHosts []string
	// TokenStore is a file the users' access and refresh tokens are kept in:
	// read at the start, rewritten on every issue, rotation and revocation,
	// so a person's sign-in outlives a replacement of the fake as it outlives
	// a restart of GitHub. "" keeps them in memory only.
	TokenStore string
}

// newWorkspaceGitHub seeds the fixture's repositories into dataDir and
// returns the fake, its handler not yet served.
func newWorkspaceGitHub(fixture *githubFixture, appKey *rsa.PublicKey, clientSecret, dataDir string, largeRepoMiB int) (*workspaceGitHub, error) {
	g := &workspaceGitHub{fixture: fixture, appKey: appKey, clientSecret: clientSecret, now: time.Now,
		repos: map[string]*workspaceRepo{}, tokens: map[string]*githubToken{}, codes: map[string]*oauthCode{}, suspended: map[int64]time.Time{}}
	git, err := newGitHubGit(dataDir)
	if err != nil {
		return nil, err
	}
	g.git = git
	for i, r := range fixture.Repositories {
		if r.GeneratedMiB > 0 && largeRepoMiB > 0 {
			r.GeneratedMiB = largeRepoMiB
		}
		size, err := git.seed(r)
		if err != nil {
			return nil, err
		}
		pushed := r.PushedAt
		if pushed.IsZero() {
			pushed = g.now().UTC()
		}
		g.repos[r.fullName()] = &workspaceRepo{githubFixtureRepo: r, id: i + 1, sizeKiB: size, pushedAt: pushed}
	}
	return g, nil
}

// handler is the fake's HTTP surface, every request logged.
func (g *workspaceGitHub) handler() http.Handler {
	api := githubFakeAPIPath
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+githubFakeHealthPath, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET "+githubFakeRequestsPath, g.serveRequests)
	mux.HandleFunc("GET "+githubFakePullsPath, g.servePulls)
	mux.HandleFunc("PUT "+githubFakeReposPath+"/{owner}/{repo}", g.addGeneratedRepo)
	mux.HandleFunc("DELETE "+githubFakeReposPath+"/{owner}/{repo}", g.removeGeneratedRepo)

	mux.HandleFunc("GET /login/oauth/authorize", g.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", g.accessToken)
	mux.HandleFunc("DELETE "+api+"/applications/{client}/grant", g.revoke(true))
	mux.HandleFunc("DELETE "+api+"/applications/{client}/token", g.revoke(false))

	mux.HandleFunc("GET "+api+"/meta", func(w http.ResponseWriter, _ *http.Request) {
		writeGitHubJSON(w, http.StatusOK, map[string]any{"installed_version": githubFakeEnterpriseVersion, "verifiable_password_authentication": false})
	})
	mux.HandleFunc("GET "+api+"/user", g.getUser)
	mux.HandleFunc("GET "+api+"/app", g.appCall(g.getApp))
	mux.HandleFunc("GET "+api+"/app/installations", g.appCall(g.listInstallations))
	mux.HandleFunc("GET "+api+"/orgs/{owner}/installation", g.appCall(g.ownerInstallation))
	mux.HandleFunc("GET "+api+"/users/{owner}/installation", g.appCall(g.ownerInstallation))
	mux.HandleFunc("GET "+api+"/repos/{owner}/{repo}/installation", g.appCall(g.ownerInstallation))
	mux.HandleFunc("POST "+api+"/app/installations/{id}/access_tokens", g.appCall(g.installationToken))
	mux.HandleFunc("PUT "+api+"/app/installations/{id}/suspended", g.appCall(g.suspend(true)))
	mux.HandleFunc("DELETE "+api+"/app/installations/{id}/suspended", g.appCall(g.suspend(false)))

	mux.HandleFunc("GET "+api+"/installation/repositories", g.installationRepositories)
	mux.HandleFunc("GET "+api+"/orgs/{owner}/repos", g.ownerRepositories)
	mux.HandleFunc("GET "+api+"/users/{owner}/repos", g.ownerRepositories)
	mux.HandleFunc("GET "+api+"/user/repos", g.userRepositories)
	mux.HandleFunc("GET "+api+"/repos/{owner}/{repo}", g.getRepository)
	mux.HandleFunc("GET "+api+"/repos/{owner}/{repo}/topics", g.getTopics)
	mux.HandleFunc("PUT "+api+"/repos/{owner}/{repo}/topics", g.replaceTopics)

	mux.HandleFunc("POST "+api+"/repos/{owner}/{repo}/pulls", g.createPull)
	mux.HandleFunc("GET "+api+"/repos/{owner}/{repo}/pulls", g.listPulls)
	mux.HandleFunc("POST /api/graphql", g.graphql)

	// git's smart HTTP: https://<fake>/<owner>/<repo>[.git]/...
	mux.HandleFunc("/", g.serveGit)
	return g.logged(g.apiHostFront(mux))
}

// --- the github.com shape -------------------------------------------------------

type apiBaseKey struct{}

// apiHostFront serves a request to one of the API hosts as the same request
// under /api/v3 of its git host (GraphQL's /graphql under /api/graphql), so
// every handler answers it as GitHub Enterprise's: the web URLs it writes
// name the git host, the API URLs the API host.
func (g *workspaceGitHub) apiHostFront(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostName(r.Host)
		if !slices.Contains(g.apiHosts, host) {
			next.ServeHTTP(w, r)
			return
		}
		inner := r.Clone(context.WithValue(r.Context(), apiBaseKey{}, "https://"+host))
		inner.Host = strings.TrimPrefix(host, "api.")
		if r.URL.Path == "/graphql" {
			inner.URL.Path = "/api/graphql"
		} else {
			inner.URL.Path = githubFakeAPIPath + r.URL.Path
		}
		inner.URL.RawPath = ""
		next.ServeHTTP(w, inner)
	})
}

// hostName is a Host header without its port.
func hostName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// apiBase is the REST API's base URL as the request reached it: its API
// host's root, or /api/v3 of the host it came to.
func apiBase(r *http.Request) string {
	if base, ok := r.Context().Value(apiBaseKey{}).(string); ok {
		return base
	}
	return "https://" + r.Host + githubFakeAPIPath
}

// --- the request log ----------------------------------------------------------

type callerKey struct{}

// statusWriter keeps the status a handler answered.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// logged resolves every request's caller once, hands it to the handler and
// logs the request: the path only, so neither a token nor an OAuth code or
// verifier (they travel in headers, queries and bodies) reaches the log.
func (g *workspaceGitHub) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := g.caller(r)
		sw := &statusWriter{ResponseWriter: w}
		switch {
		case c.invalid():
			badCredentials(sw)
		case c.kind == credInstallation && g.isSuspended(c.installation):
			installationSuspended(sw)
		default:
			next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
		}
		if r.URL.Path == githubFakeHealthPath || strings.HasPrefix(r.URL.Path, "/_fake/") {
			return
		}
		entry := githubRequest{Time: g.now().UTC(), Host: hostName(r.Host), Method: r.Method, Path: r.URL.Path, Status: sw.status, User: c.name(), Credential: c.kind,
			Audience: c.audience, Subject: c.subject}
		g.mu.Lock()
		g.requests = append(g.requests, entry)
		g.mu.Unlock()
		line := fmt.Sprintf("%s %s%s %d user=%s credential=%s", entry.Method, entry.Host, entry.Path, entry.Status, entry.User, entry.Credential)
		if c.kind == credJWT {
			line += fmt.Sprintf(" aud=%s sub=%s", strings.Join(c.audience, ","), c.subject)
		}
		fmt.Println(line)
	})
}

// name is how the log names a caller: a user's login, installation/<id>,
// app/<id>.
func (c githubCaller) name() string {
	switch c.kind {
	case credUser, credRevoked:
		return c.login
	case credInstallation:
		return "installation/" + strconv.FormatInt(c.installation, 10)
	}
	return ""
}

// invalid says whether c presented a credential GitHub refuses.
func (c githubCaller) invalid() bool {
	return c.kind == credPlaceholder || c.kind == credRevoked || c.kind == credJWT
}

func callerOf(r *http.Request) githubCaller {
	c, _ := r.Context().Value(callerKey{}).(githubCaller)
	return c
}

func (g *workspaceGitHub) serveRequests(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	writeGitHubJSON(w, http.StatusOK, append([]githubRequest{}, g.requests...))
}

// --- credentials --------------------------------------------------------------

// presentedToken is the credential of a request: a bearer or `token`
// Authorization, or the password of git's Basic auth.
func presentedToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	for _, scheme := range []string{"Bearer ", "bearer ", "token "} {
		if t, ok := strings.CutPrefix(h, scheme); ok {
			return strings.TrimSpace(t)
		}
	}
	if _, password, ok := r.BasicAuth(); ok {
		return password
	}
	return ""
}

// caller resolves who a request is from.
func (g *workspaceGitHub) caller(r *http.Request) githubCaller {
	token := presentedToken(r)
	if token == "" {
		return githubCaller{kind: credNone}
	}
	if id, _, ok := r.BasicAuth(); ok && id == g.fixture.App.ClientID && g.clientSecretMatches(token) {
		return githubCaller{kind: credClient}
	}
	if g.verifyAppJWT(token) {
		return githubCaller{kind: credApp}
	}
	g.mu.Lock()
	t, ok := g.tokens[token]
	g.mu.Unlock()
	switch {
	case ok && t.refresh:
	case ok && (t.dead || g.now().After(t.expires)):
		return githubCaller{kind: credRevoked, login: t.login}
	case ok:
		return githubCaller{kind: t.kind, login: t.login, installation: t.installation, repos: t.repos}
	}
	if aud, sub, ok := jwtAudienceSubject(token); ok {
		return githubCaller{kind: credJWT, audience: aud, subject: sub}
	}
	return githubCaller{kind: credPlaceholder}
}

// jwtAudienceSubject reads a JWT's aud (a string or a list) and sub claims
// without verifying it; ok false for a token that is no JWT.
func jwtAudienceSubject(token string) (aud []string, sub string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, "", false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
		Sub string          `json:"sub"`
	}
	if decodeJWTPart(parts[0], &header) != nil || header.Alg == "" || decodeJWTPart(parts[1], &claims) != nil {
		return nil, "", false
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil {
		aud = []string{one}
	} else {
		_ = json.Unmarshal(claims.Aud, &aud)
	}
	return aud, claims.Sub, true
}

// verifyAppJWT checks an App JWT as GitHub does: RS256 signed by the App's
// key, issued by the App's id, unexpired, valid for at most ten minutes.
func (g *workspaceGitHub) verifyAppJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || g.appKey == nil {
		return false
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss json.RawMessage `json:"iss"`
		Iat int64           `json:"iat"`
		Exp int64           `json:"exp"`
	}
	if decodeJWTPart(parts[0], &header) != nil || header.Alg != "RS256" || decodeJWTPart(parts[1], &claims) != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(g.appKey, crypto.SHA256, digest[:], sig) != nil {
		return false
	}
	iss := strings.Trim(string(claims.Iss), `"`)
	now := g.now()
	iat, exp := time.Unix(claims.Iat, 0), time.Unix(claims.Exp, 0)
	return iss == strconv.FormatInt(g.fixture.App.ID, 10) &&
		!iat.After(now.Add(githubClockSkew)) && exp.After(now) && exp.Sub(iat) <= githubAppJWTMaxTTL+githubClockSkew
}

// githubAppJWT is the App's credential as GitHub takes it: RS256 signed by
// key, issued by the App's id at iat, valid for ttl.
func githubAppJWT(key *rsa.PrivateKey, appID int64, iat time.Time, ttl time.Duration) (string, error) {
	enc := func(v any) (string, error) {
		raw, err := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw), err
	}
	header, err := enc(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := enc(map[string]any{"iss": strconv.FormatInt(appID, 10), "iat": iat.Unix(), "exp": iat.Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(header + "." + claims))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return header + "." + claims + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func decodeJWTPart(part string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// issue records a new token of prefix (ghs_, ghu_, ghr_) and returns it.
func (g *workspaceGitHub) issue(prefix string, t *githubToken, ttl time.Duration) string {
	t.expires = g.now().Add(ttl)
	token := prefix + randomHex(18)
	g.tokens[token] = t
	return token
}

// storedUserToken is a user's token as the token store keeps it.
type storedUserToken struct {
	Token   string    `json:"token"`
	Login   string    `json:"login"`
	Refresh bool      `json:"refresh,omitempty"`
	Expires time.Time `json:"expires"`
	Dead    bool      `json:"dead,omitempty"`
}

// loadUserTokens reads the users' tokens a former fake kept in the token
// store, an expired one left out; a store not yet written holds none.
func (g *workspaceGitHub) loadUserTokens() (int, error) {
	raw, err := os.ReadFile(g.tokenStore) // #nosec G304 G703 -- the token store the command names
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the token store: %w", err)
	}
	var stored []storedUserToken
	if err := json.Unmarshal(raw, &stored); err != nil {
		return 0, fmt.Errorf("the token store %s: %w", g.tokenStore, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, s := range stored {
		if s.Token == "" || g.now().After(s.Expires) {
			continue
		}
		g.tokens[s.Token] = &githubToken{kind: credUser, login: s.Login, refresh: s.Refresh, expires: s.Expires, dead: s.Dead}
		n++
	}
	return n, nil
}

// saveUserTokens rewrites the token store with the users' unexpired tokens,
// with g.mu held: written beside it and renamed into place, so a fake
// replaced mid-write reads the former store whole. A failed write is logged:
// the sign-in it answers stands, only its survival of a replacement is lost.
func (g *workspaceGitHub) saveUserTokens() {
	if g.tokenStore == "" {
		return
	}
	stored := []storedUserToken{}
	for token, t := range g.tokens {
		if t.kind == credUser && !g.now().After(t.expires) {
			stored = append(stored, storedUserToken{Token: token, Login: t.login, Refresh: t.refresh, Expires: t.expires, Dead: t.dead})
		}
	}
	raw, err := json.Marshal(stored)
	if err == nil {
		tmp := g.tokenStore + ".next"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil { // #nosec G703 -- beside the token store the command names
			err = os.Rename(tmp, g.tokenStore) // #nosec G703 -- the token store the command names
		}
	}
	if err != nil {
		fmt.Printf("keeping the users' tokens in %s: %v\n", g.tokenStore, err)
	}
}

// canRead says whether c reads repo: a public one everyone, a private one
// the users the fixture grants it and the installation on its owner.
func (g *workspaceGitHub) canRead(c githubCaller, repo *workspaceRepo) bool {
	return !repo.Private || g.permission(c, repo) != ""
}

// canWrite says whether c pushes to repo.
func (g *workspaceGitHub) canWrite(c githubCaller, repo *workspaceRepo) bool {
	return g.permission(c, repo) == githubPermWrite
}

// permission is c's explicit permission on repo: an installation token
// writes to every repository of its owner (or of its list), a user what the
// fixture grants.
func (g *workspaceGitHub) permission(c githubCaller, repo *workspaceRepo) string {
	if g.isSuspended(g.installationOf(repo.Owner)) {
		return ""
	}
	switch c.kind {
	case credInstallation:
		if g.installationOf(repo.Owner) == c.installation && (len(c.repos) == 0 || slices.Contains(c.repos, repo.Name)) {
			return githubPermWrite
		}
	case credUser:
		if u, ok := g.fixture.fixtureUser(c.login); ok {
			return u.Repositories[repo.fullName()]
		}
	}
	return ""
}

func (g *workspaceGitHub) installationOf(owner string) int64 {
	for _, o := range g.fixture.Owners {
		if o.Login == owner {
			return o.Installation
		}
	}
	return 0
}

func (g *workspaceGitHub) owner(login string) (githubFixtureOwner, bool) {
	i := slices.IndexFunc(g.fixture.Owners, func(o githubFixtureOwner) bool { return o.Login == login })
	if i < 0 {
		return githubFixtureOwner{}, false
	}
	return g.fixture.Owners[i], true
}

func badCredentials(w http.ResponseWriter) {
	githubFakeError(w, http.StatusUnauthorized, "Bad credentials")
}

// installationSuspended is GitHub's answer to an installation token of a
// suspended installation, and to a new token for one.
func installationSuspended(w http.ResponseWriter) {
	githubFakeError(w, http.StatusForbidden, "This installation has been suspended")
}

// isSuspended says whether installation id is suspended; 0, no
// installation, never is.
func (g *workspaceGitHub) isSuspended(id int64) bool {
	g.suspendMu.Lock()
	defer g.suspendMu.Unlock()
	_, ok := g.suspended[id]
	return ok && id != 0
}

// --- the App ------------------------------------------------------------------

// appCall admits a call with the App's JWT only.
func (g *workspaceGitHub) appCall(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if callerOf(r).kind != credApp {
			githubFakeError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		h(w, r)
	}
}

func (g *workspaceGitHub) getApp(w http.ResponseWriter, r *http.Request) {
	a := g.fixture.App
	writeGitHubJSON(w, http.StatusOK, map[string]any{"id": a.ID, "slug": a.Slug, nameKey: a.Slug, "client_id": a.ClientID,
		githubHTMLURL: "https://" + r.Host + "/apps/" + a.Slug})
}

func (g *workspaceGitHub) installationJSON(o githubFixtureOwner) map[string]any {
	out := map[string]any{
		"id": o.Installation, "app_id": g.fixture.App.ID, "app_slug": g.fixture.App.Slug, "target_type": o.Type,
		"account":              map[string]any{githubLogin: o.Login, fieldTypeKey: o.Type},
		"repository_selection": githubAll,
		githubPermissions:      map[string]string{"contents": githubPermWrite, "pull_requests": githubPermWrite},
		"suspended_at":         nil,
		"suspended_by":         nil,
	}
	g.suspendMu.Lock()
	defer g.suspendMu.Unlock()
	if at, ok := g.suspended[o.Installation]; ok {
		out["suspended_at"] = at.UTC().Format(time.RFC3339)
		out["suspended_by"] = map[string]any{githubLogin: githubFakeSuspender, fieldTypeKey: githubUser}
	}
	return out
}

// suspend is PUT (true) and DELETE (false) of
// /app/installations/{id}/suspended: 204 either way, 404 for an unknown
// installation. Suspending keeps the first suspension's time, as GitHub
// does for a second PUT.
func (g *workspaceGitHub) suspend(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if id == 0 || !slices.ContainsFunc(g.fixture.Owners, func(o githubFixtureOwner) bool { return o.Installation == id }) {
			githubFakeError(w, http.StatusNotFound, "Not Found")
			return
		}
		g.suspendMu.Lock()
		if _, ok := g.suspended[id]; on && !ok {
			g.suspended[id] = g.now()
		} else if !on {
			delete(g.suspended, id)
		}
		g.suspendMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (g *workspaceGitHub) listInstallations(w http.ResponseWriter, _ *http.Request) {
	out := []map[string]any{}
	for _, o := range g.fixture.Owners {
		if o.Installation != 0 {
			out = append(out, g.installationJSON(o))
		}
	}
	writeGitHubJSON(w, http.StatusOK, out)
}

// ownerInstallation is the App's installation on an owner, or on a
// repository's owner; 404 where the App is not installed.
func (g *workspaceGitHub) ownerInstallation(w http.ResponseWriter, r *http.Request) {
	o, ok := g.owner(r.PathValue("owner"))
	if repo := r.PathValue("repo"); repo != "" {
		_, known := g.repos[o.Login+"/"+repo]
		ok = ok && known
	}
	if !ok || o.Installation == 0 {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeGitHubJSON(w, http.StatusOK, g.installationJSON(o))
}

// installationToken is POST /app/installations/{id}/access_tokens, limited
// to the named repositories of the owner when the body names any.
func (g *workspaceGitHub) installationToken(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	i := slices.IndexFunc(g.fixture.Owners, func(o githubFixtureOwner) bool { return o.Installation == id && id != 0 })
	if i < 0 {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	owner := g.fixture.Owners[i]
	if g.isSuspended(id) {
		installationSuspended(w)
		return
	}
	var in struct {
		Repositories []string `json:"repositories"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil && err != io.EOF {
			githubFakeError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, name := range in.Repositories {
		if _, ok := g.repos[owner.Login+"/"+name]; !ok {
			githubFakeError(w, http.StatusUnprocessableEntity, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
	}
	t := &githubToken{kind: credInstallation, installation: id, repos: in.Repositories}
	token := g.issue("ghs_", t, githubInstallationTokenTTL)
	selection := githubAll
	if len(in.Repositories) > 0 {
		selection = "selected"
	}
	writeGitHubJSON(w, http.StatusCreated, map[string]any{
		"token": token, "expires_at": t.expires.UTC().Format(time.RFC3339), "repository_selection": selection,
		githubPermissions: g.installationJSON(owner)[githubPermissions],
	})
}

// --- OAuth --------------------------------------------------------------------

// authorize is GitHub's consent page, consenting at once for the lab user
// the login hint names: PKCE (S256) is required, and the code goes back to
// redirect_uri with the state.
func (g *workspaceGitHub) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	switch {
	case q.Get("client_id") != g.fixture.App.ClientID:
		http.Error(w, "unknown client_id", http.StatusNotFound)
		return
	case err != nil || (redirect.Scheme != "https" && redirect.Scheme != httpScheme) || redirect.Host == "":
		http.Error(w, "redirect_uri is not an absolute http(s) URL", http.StatusBadRequest)
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		http.Error(w, "the fake requires PKCE: code_challenge with code_challenge_method S256", http.StatusBadRequest)
		return
	}
	login := q.Get("login")
	if _, ok := g.fixture.fixtureUser(login); !ok {
		http.Error(w, fmt.Sprintf("no lab user %q: the login parameter names the user who consents", login), http.StatusBadRequest)
		return
	}
	code := randomHex(10)
	g.mu.Lock()
	g.codes[code] = &oauthCode{login: login, redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), expires: g.now().Add(githubOAuthCodeTTL)}
	g.mu.Unlock()
	back := redirect.Query()
	back.Set("code", code)
	if state := q.Get(githubState); state != "" {
		back.Set(githubState, state)
	}
	redirect.RawQuery = back.Encode()
	// #nosec G710 -- an OAuth server redirects to the client's redirect_uri; the fake serves lab clients only
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// accessToken is POST /login/oauth/access_token: an authorization code with
// its PKCE verifier, or a refresh token, for a new access and refresh
// token. A refresh token redeems once; GitHub answers a failure with 200 and
// an error body, and so does the fake.
func (g *workspaceGitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	in := oauthParams(r)
	answer := func(v map[string]any) { writeOAuth(w, r, v) }
	fail := func(code, description string) {
		answer(map[string]any{oauthError: code, "error_description": description})
	}
	if in.Get("client_id") != g.fixture.App.ClientID || !g.clientSecretMatches(in.Get("client_secret")) {
		fail("incorrect_client_credentials", "The client_id and/or client_secret passed are incorrect.")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var login string
	switch {
	case in.Get("grant_type") == "refresh_token":
		t, ok := g.tokens[in.Get("refresh_token")]
		if !ok || !t.refresh || t.dead || g.now().After(t.expires) {
			fail("bad_refresh_token", "The refresh token passed is incorrect or expired.")
			return
		}
		t.dead = true
		login = t.login
	case in.Get("grant_type") == "" || in.Get("grant_type") == "authorization_code":
		c, ok := g.codes[in.Get("code")]
		delete(g.codes, in.Get("code"))
		if !ok || g.now().After(c.expires) || (in.Get("redirect_uri") != "" && in.Get("redirect_uri") != c.redirectURI) {
			fail("bad_verification_code", "The code passed is incorrect or expired.")
			return
		}
		sum := sha256.Sum256([]byte(in.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			fail("bad_verification_code", "The code_verifier does not match the code_challenge.")
			return
		}
		login = c.login
	default:
		fail("unsupported_grant_type", "grant_type is neither authorization_code nor refresh_token.")
		return
	}
	access := g.issue("ghu_", &githubToken{kind: credUser, login: login}, githubUserTokenTTL)
	refresh := g.issue("ghr_", &githubToken{kind: credUser, login: login, refresh: true}, githubRefreshTokenTTL)
	g.saveUserTokens()
	answer(map[string]any{
		"access_token": access, "expires_in": int(githubUserTokenTTL.Seconds()),
		"refresh_token": refresh, "refresh_token_expires_in": int(githubRefreshTokenTTL.Seconds()),
		"token_type": "bearer", oauthScope: "",
	})
}

func (g *workspaceGitHub) clientSecretMatches(secret string) bool {
	return g.clientSecret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(g.clientSecret)) == 1
}

// oauthParams reads the token request's parameters from a form or a JSON
// body, both of which GitHub takes.
func oauthParams(r *http.Request) url.Values {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := url.Values{}
		for k, v := range body {
			out.Set(k, v)
		}
		return out
	}
	_ = r.ParseForm()
	return r.Form
}

// writeOAuth answers in JSON when the client accepts it, form-encoded
// otherwise, as GitHub does.
func writeOAuth(w http.ResponseWriter, r *http.Request, v map[string]any) {
	if strings.Contains(r.Header.Get("Accept"), "json") {
		writeGitHubJSON(w, http.StatusOK, v)
		return
	}
	form := url.Values{}
	for k, val := range v {
		form.Set(k, fmt.Sprint(val))
	}
	w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
	_, _ = io.WriteString(w, form.Encode())
}

// revoke is DELETE /applications/{client_id}/grant (every token of the
// person's grant) or /token (the one access token), with the App's client
// credentials as Basic auth.
func (g *workspaceGitHub) revoke(grant bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if callerOf(r).kind != credClient || r.PathValue("client") != g.fixture.App.ClientID {
			githubFakeError(w, http.StatusUnauthorized, "Requires authentication")
			return
		}
		var in struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		defer g.mu.Unlock()
		t, known := g.tokens[in.AccessToken]
		if !known || t.kind != credUser || t.refresh {
			githubFakeError(w, http.StatusNotFound, "Not Found")
			return
		}
		t.dead = true
		if grant {
			for _, other := range g.tokens {
				if other.kind == credUser && other.login == t.login {
					other.dead = true
				}
			}
		}
		g.saveUserTokens()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (g *workspaceGitHub) getUser(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	u, ok := g.fixture.fixtureUser(c.login)
	if c.kind != credUser || !ok {
		badCredentials(w)
		return
	}
	writeGitHubJSON(w, http.StatusOK, g.userJSON(r, u))
}

func (g *workspaceGitHub) userJSON(r *http.Request, u githubFixtureUser) map[string]any {
	id := slices.IndexFunc(g.fixture.Users, func(o githubFixtureUser) bool { return o.Login == u.Login }) + 1
	return map[string]any{githubLogin: u.Login, "id": id, "node_id": "U_" + u.Login, nameKey: u.Name, "email": u.Email,
		fieldTypeKey: githubUser, githubHTMLURL: "https://" + r.Host + "/" + u.Login}
}

// --- repositories -------------------------------------------------------------

func (g *workspaceGitHub) repoJSON(r *http.Request, c githubCaller, repo *workspaceRepo) map[string]any {
	visibility := "public"
	if repo.Private {
		visibility = githubPrivate
	}
	o, _ := g.owner(repo.Owner)
	base := "https://" + r.Host
	topics := repo.Topics
	if topics == nil {
		topics = []string{}
	}
	perm := g.permission(c, repo)
	return map[string]any{
		"id": repo.id, "node_id": "R_" + strconv.Itoa(repo.id), nameKey: repo.Name, "full_name": repo.fullName(),
		"owner":   map[string]any{githubLogin: repo.Owner, fieldTypeKey: o.Type},
		"private": repo.Private, "visibility": visibility, "fork": repo.Fork, "archived": repo.Archived, "disabled": false,
		"language": repo.Language, "topics": topics, "default_branch": repo.DefaultBranch, "size": repo.sizeKiB,
		"pushed_at": repo.pushedAt.UTC().Format(time.RFC3339), "updated_at": repo.pushedAt.UTC().Format(time.RFC3339),
		githubHTMLURL: base + "/" + repo.fullName(), "clone_url": base + "/" + repo.fullName() + ".git",
		githubURL:         apiBase(r) + "/repos/" + repo.fullName(),
		githubPermissions: map[string]bool{"admin": false, githubPush: perm == githubPermWrite, "pull": g.canRead(c, repo)},
	}
}

// listRepos answers a listing of the repositories keep admits, sorted and
// paged as the query asks (sort full_name, pushed or updated; direction;
// per_page, page with GitHub's Link header).
func (g *workspaceGitHub) listRepos(w http.ResponseWriter, r *http.Request, keep func(*workspaceRepo) bool, wrap func([]map[string]any, int) any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := callerOf(r)
	var repos []*workspaceRepo
	for _, repo := range g.repos {
		if g.canRead(c, repo) && keep(repo) {
			repos = append(repos, repo)
		}
	}
	q := r.URL.Query()
	sortBy, desc := q.Get("sort"), q.Get("direction") == "desc"
	if q.Get("direction") == "" {
		desc = sortBy == "pushed" || sortBy == "updated"
	}
	slices.SortFunc(repos, func(a, b *workspaceRepo) int {
		n := strings.Compare(a.fullName(), b.fullName())
		if sortBy == "pushed" || sortBy == "updated" {
			n = a.pushedAt.Compare(b.pushedAt)
		}
		if desc {
			return -n
		}
		return n
	})
	page, perPage := pageParams(q)
	total := len(repos)
	from, to := min((page-1)*perPage, total), min(page*perPage, total)
	out := []map[string]any{}
	for _, repo := range repos[from:to] {
		out = append(out, g.repoJSON(r, c, repo))
	}
	if to < total {
		next := *r.URL
		nq := next.Query()
		nq.Set("page", strconv.Itoa(page+1))
		next.RawQuery = nq.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, apiBase(r), strings.TrimPrefix(next.RequestURI(), githubFakeAPIPath)))
	}
	writeGitHubJSON(w, http.StatusOK, wrap(out, total))
}

func pageParams(q url.Values) (page, perPage int) {
	page, perPage = 1, 30
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 0 {
		page = n
	}
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil && n > 0 {
		perPage = min(n, 100)
	}
	return page, perPage
}

func plainList(out []map[string]any, _ int) any { return out }

// typeFilter is a listing's `type` (all, public, private, forks, sources).
func typeFilter(t string) func(*workspaceRepo) bool {
	return func(repo *workspaceRepo) bool {
		switch t {
		case "public":
			return !repo.Private
		case githubPrivate:
			return repo.Private
		case "forks":
			return repo.Fork
		case "sources":
			return !repo.Fork
		}
		return true
	}
}

// ownerRepositories is GET /orgs/{owner}/repos and /users/{owner}/repos:
// the owner's repositories the caller reads.
func (g *workspaceGitHub) ownerRepositories(w http.ResponseWriter, r *http.Request) {
	owner := r.PathValue("owner")
	if _, ok := g.owner(owner); !ok {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return
	}
	byType := typeFilter(r.URL.Query().Get("type"))
	g.listRepos(w, r, func(repo *workspaceRepo) bool { return repo.Owner == owner && byType(repo) }, plainList)
}

// userRepositories is GET /user/repos: what the signed-in user reads.
func (g *workspaceGitHub) userRepositories(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.kind != credUser {
		badCredentials(w)
		return
	}
	byType := typeFilter(r.URL.Query().Get("visibility"))
	g.listRepos(w, r, func(repo *workspaceRepo) bool { return g.permission(c, repo) != "" && byType(repo) }, plainList)
}

// installationRepositories is GET /installation/repositories with an
// installation token: its owner's repositories, or the ones it was limited
// to.
func (g *workspaceGitHub) installationRepositories(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.kind != credInstallation {
		badCredentials(w)
		return
	}
	g.listRepos(w, r, func(repo *workspaceRepo) bool { return g.permission(c, repo) != "" }, func(out []map[string]any, total int) any {
		return map[string]any{"total_count": total, repositoriesKey: out}
	})
}

// readableRepo is the repository of the path the caller reads; it answers
// GitHub's 404 (also for a private repository the caller does not read)
// and returns nil otherwise. The caller holds g.mu.
func (g *workspaceGitHub) readableRepo(w http.ResponseWriter, r *http.Request) *workspaceRepo {
	repo, ok := g.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
	if !ok || !g.canRead(callerOf(r), repo) {
		githubFakeError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return repo
}

func (g *workspaceGitHub) getRepository(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if repo := g.readableRepo(w, r); repo != nil {
		writeGitHubJSON(w, http.StatusOK, g.repoJSON(r, callerOf(r), repo))
	}
}

// githubTopics is the body of GitHub's topics endpoints.
type githubTopics struct {
	Names []string `json:"names"`
}

func (g *workspaceGitHub) getTopics(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if repo := g.readableRepo(w, r); repo != nil {
		writeGitHubJSON(w, http.StatusOK, githubTopics{Names: append([]string{}, repo.Topics...)})
	}
}

// replaceTopics is PUT /repos/{owner}/{repo}/topics: the repository's
// topics replaced by the body's names, lower-cased as GitHub keeps them, by
// a caller that writes to it. Like on GitHub, it is no push: pushed_at stays.
func (g *workspaceGitHub) replaceTopics(w http.ResponseWriter, r *http.Request) {
	var in githubTopics
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Names == nil {
		githubFakeError(w, http.StatusUnprocessableEntity, "names is required: an array of topics")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	repo := g.readableRepo(w, r)
	if repo == nil {
		return
	}
	if !g.canWrite(callerOf(r), repo) {
		githubFakeError(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	topics := make([]string, 0, len(in.Names))
	for _, name := range in.Names {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" && !slices.Contains(topics, name) {
			topics = append(topics, name)
		}
	}
	repo.Topics = topics
	writeGitHubJSON(w, http.StatusOK, githubTopics{Names: append([]string{}, topics...)})
}

// --- serving ------------------------------------------------------------------

// ServeWorkspaceGitHub serves the workspace proofs' GitHub over TLS until
// ctx ends: `agentlab github-fake --workspaces`.
func ServeWorkspaceGitHub(ctx context.Context, opts WorkspaceGitHubOptions) error {
	fixture, err := loadGitHubFixture(opts.Fixture)
	if err != nil {
		return err
	}
	if opts.Credentials == "" {
		return fmt.Errorf("--credentials names the directory `agentlab github-fake credentials` generated")
	}
	appKey, err := readRSAPublicKey(filepath.Join(opts.Credentials, githubFakeAppPublicKey))
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(filepath.Join(opts.Credentials, githubFakeClientSecret))
	if err != nil {
		return fmt.Errorf("reading the OAuth client secret: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(opts.Credentials, githubFakeTLSCert), filepath.Join(opts.Credentials, githubFakeTLSKey))
	if err != nil {
		return fmt.Errorf("reading the fake's TLS pair: %w", err)
	}
	dataDir := opts.DataDir
	if dataDir == "" {
		if dataDir, err = os.MkdirTemp("", "github-fake-"); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(dataDir) }()
	}
	g, err := newWorkspaceGitHub(fixture, appKey, strings.TrimSpace(string(secret)), dataDir, opts.LargeRepoMiB)
	if err != nil {
		return err
	}
	g.apiHosts = opts.APIHosts
	if g.tokenStore = opts.TokenStore; g.tokenStore != "" {
		n, err := g.loadUserTokens()
		if err != nil {
			return err
		}
		fmt.Printf("%d users' tokens kept from %s\n", n, g.tokenStore)
	}
	l, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return fmt.Errorf("the fake GitHub cannot listen on %s: %w", opts.Listen, err)
	}
	server := &http.Server{Handler: g.handler(), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	go func() { _ = server.ServeTLS(l, "", "") }()
	fmt.Printf("fake GitHub on https://%s (%d owners, %d repositories, %d users; repositories in %s)\n",
		l.Addr(), len(fixture.Owners), len(fixture.Repositories), len(fixture.Users), dataDir)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func readRSAPublicKey(path string) (*rsa.PublicKey, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the credentials directory the command names
	if err != nil {
		return nil, fmt.Errorf("reading the App's public key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA public key", path)
	}
	return rsaKey, nil
}
