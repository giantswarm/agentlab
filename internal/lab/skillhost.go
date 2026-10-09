package lab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The skill host fixture of `skills-test --skill-fixture`: a private git host
// inside the lab, so the proof can tell what the golden boot and a Session's
// sandbox send to a credentialed skill's host without a token of a real forge.
//
// The fixture is this binary (`agentlab skill-host`) in a container on the
// kind network, in the Harness's own runtime image, which carries git: it
// serves one repository over git's smart HTTP protocol (upload-pack in
// stateless RPC mode, what `git http-backend` runs), answers 401 to any
// request without the expected Authorization, as a private forge does, and
// records each request: method, path and the kind of credential its headers
// carry — never a value. The proof puts it behind the lab's edge as
// skillhost.<domain> (a path-scoped HTTPRoute), whose certificate the lab CA
// signs and Substrate's egress gateway trusts (substrate.atenetEgress.
// upstreamTrust), so the gateway binds the source's credential to that host
// as it would to a forge's. The Secret holding the credential is generated
// and created by the proof, handed to the fixture through the container's
// environment, and never printed: the record says `secret`, not the value.

// What the fixture serves and how the proof names it.
const (
	skillHostCommand        = "skill-host"
	skillHostRepositoryName = "agentlab/skill"
	skillHostRepository     = skillHostRepositoryName + ".git"
	skillHostSkillPath      = "skills/agentlab-skill-host"
	skillHostFact           = "periwinkle-lighthouse"
	skillHostQuestion       = "what is the codeword of the skill host fixture?"
	// skillHostAuthorizationEnv carries the Authorization the fixture
	// requires ("Basic <base64 of user:token>") into its container.
	skillHostAuthorizationEnv = "AGENTLAB_SKILL_HOST_AUTHORIZATION"
	skillHostHealthPath       = "/healthz"
	skillHostCommitPath       = "/_fixture/commit"
	skillHostRequestsPath     = "/_fixture/requests"
	// skillHostPrefix is the hostname's first label under platform.domain.
	skillHostPrefix = "skillhost"
	// skillHostService and skillHostRoute front the container in the
	// platform namespace; skillHostSecret is the source's credentialRef.
	skillHostService = "agentlab-skill-host"
	skillHostRoute   = "agentlab-skill-host"
	skillHostSecret  = "agentlab-skills-test-credential"
	// httpRouteResource is the Gateway API route the edge serves the
	// fixture's hostname by.
	httpRouteResource = "httproutes.gateway.networking.k8s.io"
	// credentialPlaceholder is the inert value kagent's runtimes send where
	// a header is bound, and the egress gateway replaces where it binds one.
	credentialPlaceholder = "kagent-credential-injected" // #nosec G101 -- kagent's inert placeholder, the opposite of a credential
	// skillHostUser is the Basic user of the generated credential, the one
	// GitHub's App tokens use.
	skillHostUser = "x-access-token"
	// authorizationHeader is the header kagent binds a git source's
	// credential to.
	authorizationHeader = "Authorization"
)

// The credential kinds a request's Authorization is recorded as.
const (
	authNone        = "none"
	authSecret      = "secret"
	authPlaceholder = "placeholder"
	authOther       = "other"
)

// skillHostRequest is one recorded request: its order, method and path, the
// kind of its Authorization, and the headers carrying the Secret value or
// the placeholder anywhere (names only).
type skillHostRequest struct {
	Seq           int      `json:"seq"`
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Authorization string   `json:"authorization"`
	SecretIn      []string `json:"secretIn,omitempty"`
	PlaceholderIn []string `json:"placeholderIn,omitempty"`
}

func (r skillHostRequest) String() string {
	s := fmt.Sprintf("request %d (%s %s): Authorization %s", r.Seq, r.Method, r.Path, r.Authorization)
	if len(r.SecretIn) > 0 {
		s += ", the Secret value in " + strings.Join(r.SecretIn, ", ")
	}
	return s
}

// skillHost is the fixture's server: the repository's directory and commit,
// the Authorization it requires, the record.
type skillHost struct {
	git                               *githubGit
	commit, authorization, credential string

	mu       sync.Mutex
	requests []skillHostRequest
}

// ServeSkillHost runs the fixture on addr until ctx ends: the repository is
// created on start, the required Authorization read from the environment.
func ServeSkillHost(ctx context.Context, addr string) error {
	authorization := os.Getenv(skillHostAuthorizationEnv)
	if !strings.HasPrefix(authorization, "Basic ") || len(authorization) == len("Basic ") {
		return fmt.Errorf("%s must hold the Authorization the fixture requires (Basic <base64>)", skillHostAuthorizationEnv)
	}
	root, err := os.MkdirTemp("", "skill-host-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	h, err := newSkillHost(root, authorization)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	fmt.Printf("skill host fixture on %s: %s @ %s\n", listener.Addr(), skillHostRepository, h.commit)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newSkillHost creates the repository under root — one commit with the
// fixture's SKILL.md, a bare copy that serves any full commit a client wants
// — and the handler requiring authorization. git runs as the workspace
// GitHub fake runs it (githubfake_git.go).
func newSkillHost(root, authorization string) (*skillHost, error) {
	g, err := newGitHubGit(root)
	if err != nil {
		return nil, err
	}
	work := filepath.Join(root, "work")
	skillDir := filepath.Join(work, filepath.FromSlash(skillHostSkillPath))
	if err := os.MkdirAll(skillDir, 0o750); err != nil {
		return nil, err
	}
	skill := "---\nname: " + path.Base(skillHostSkillPath) + "\ndescription: The skill of agentlab's skill host fixture; read it when asked about the fixture.\n---\n\n" +
		"The codeword of the skill host fixture is " + skillHostFact + ".\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o600); err != nil {
		return nil, err
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "."},
		{"-c", "user.name=agentlab", "-c", "user.email=agentlab@lab.local", gitCommit, "--quiet", "--message", "The skill host fixture"},
	} {
		if _, err := g.run(work, args...); err != nil {
			return nil, err
		}
	}
	commit, err := g.run(work, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	bare := g.path(skillHostRepositoryName)
	if _, err := g.run(root, "clone", "--quiet", "--bare", work, bare); err != nil {
		return nil, err
	}
	// The runtime fetches its pinned commit by id, not by ref.
	if _, err := g.run(bare, "config", "uploadpack.allowAnySHA1InWant", "true"); err != nil {
		return nil, err
	}
	return &skillHost{git: g, commit: commit, authorization: authorization, credential: strings.TrimPrefix(authorization, "Basic ")}, nil
}

func (h *skillHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case skillHostHealthPath:
		_, _ = io.WriteString(w, "ok")
		return
	case skillHostCommitPath:
		_, _ = io.WriteString(w, h.commit)
		return
	case skillHostRequestsPath:
		h.mu.Lock()
		requests := slices.Clone(h.requests)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(requests)
		return
	}
	h.record(r)
	if r.Header.Get(authorizationHeader) != h.authorization {
		w.Header().Set("WWW-Authenticate", `Basic realm="agentlab skill host"`)
		http.Error(w, "credential required", http.StatusUnauthorized)
		return
	}
	// Fetches only: the fixture takes no push.
	fullName, service, advertise, ok := gitService(r)
	if !ok || fullName != skillHostRepositoryName || service != gitUploadPack {
		http.NotFound(w, r)
		return
	}
	if err := h.git.serveService(w, r, fullName, service, advertise); err != nil {
		note("skill host: %s %s: %v", r.Method, r.URL.Path, err)
	}
}

// record keeps the request without any header value: the kind of its
// Authorization and the names of the headers that carry the credential or
// the placeholder.
func (h *skillHost) record(r *http.Request) {
	entry := skillHostRequest{Method: r.Method, Path: r.URL.Path, Authorization: h.authorizationKind(r.Header.Values(authorizationHeader))}
	for _, name := range slices.Sorted(maps.Keys(r.Header)) {
		for _, value := range r.Header[name] {
			if strings.Contains(value, h.credential) {
				entry.SecretIn = append(entry.SecretIn, name)
				break
			}
		}
		for _, value := range r.Header[name] {
			if strings.Contains(value, credentialPlaceholder) {
				entry.PlaceholderIn = append(entry.PlaceholderIn, name)
				break
			}
		}
	}
	h.mu.Lock()
	entry.Seq = len(h.requests) + 1
	h.requests = append(h.requests, entry)
	h.mu.Unlock()
}

func (h *skillHost) authorizationKind(values []string) string {
	switch {
	case len(values) == 0:
		return authNone
	case len(values) == 1 && values[0] == h.authorization:
		return authSecret
	case len(values) == 1 && values[0] == "Basic "+credentialPlaceholder:
		return authPlaceholder
	}
	return authOther
}

// --- the proof's side ---------------------------------------------------------

// skillHostFixture is the running fixture as the proof holds it: the
// container, the source URL the template names, the commit, and what to
// remove when the run ends.
type skillHostFixture struct {
	container *fakeContainer
	url       string
	commit    string
	cleanups  []func()
}

// close removes everything the fixture put in place, last first.
func (f *skillHostFixture) close() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
	f.cleanups = nil
}

// skillHostURL is the source URL of the fixture's repository as every pod
// dials it: through the edge on 443 (the CoreDNS rewrite), whatever port the
// edge has on this host.
func skillHostURL(cfg *config.Config) string {
	return "https://" + skillHostPrefix + "." + cfg.Platform.Domain + "/" + skillHostRepository
}

// newSkillHostCredential is a fresh credential in the shape kagent binds a
// git source's: the base64 of "<user>:<token>", which follows the Basic
// scheme.
func newSkillHostCredential() (string, error) {
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(skillHostUser + ":" + hex.EncodeToString(token))), nil
}

// startSkillHost puts the fixture in place: the credential Secret in the
// kagent namespace, the container on the kind network in image, the Service
// pods reach it through, the edge's HTTPRoute for skillhost.<domain>, and the
// route answering 401 from this host. Whatever was placed is removed on a
// failure; on success the caller closes the fixture.
func startSkillHost(cfg *config.Config, binary, image string) (_ *skillHostFixture, err error) {
	f := &skillHostFixture{url: skillHostURL(cfg)}
	defer func() {
		if err != nil {
			f.close()
		}
	}()
	credential, err := newSkillHostCredential()
	if err != nil {
		return nil, err
	}
	removeSecret, err := createSkillHostSecret(credential)
	if err != nil {
		return nil, err
	}
	f.cleanups = append(f.cleanups, removeSecret)

	if len(hostPullImages([]string{image})) == 0 {
		return nil, fmt.Errorf("the skill host fixture runs in the Harness's runtime image %s (it carries git), and docker cannot pull it", image)
	}
	f.container, err = startFakeContainer(cfg, binary, fakeContainerSpec{
		what: "the skill host fixture", suffix: skillHostCommand, command: skillHostCommand,
		binaryFlag: "--skill-host-binary", healthPath: skillHostHealthPath, image: image,
		env: []string{skillHostAuthorizationEnv + "=Basic " + credential},
	})
	if err != nil {
		return nil, err
	}
	f.cleanups = append(f.cleanups, f.container.close)
	if f.commit, err = f.read(skillHostCommitPath); err != nil {
		return nil, err
	}

	removeService, err := fakeServiceForPods(cfg, skillHostService, "the skill host fixture", f.container.podIP, fakeContainerPort,
		"http://"+skillHostService+"."+platformNamespace+".svc.cluster.local"+skillHostHealthPath)
	if err != nil {
		return nil, err
	}
	f.cleanups = append(f.cleanups, removeService)

	removeRoute, err := applySkillHostRoute(cfg)
	if err != nil {
		return nil, err
	}
	f.cleanups = append(f.cleanups, removeRoute)
	return f, nil
}

// createSkillHostSecret creates the source's credential Secret (key
// skillsCredentialKey) in the kagent namespace, a leftover replaced; the
// returned func deletes it.
func createSkillHostSecret(credential string) (func(), error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	secrets := k.clientset.CoreV1().Secrets(kagentNamespace)
	remove := func() {
		if err := secrets.Delete(context.Background(), skillHostSecret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			note("cleanup: Secret %s/%s: %v", kagentNamespace, skillHostSecret, err)
		}
	}
	remove()
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	if _, err := secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: skillHostSecret, Namespace: kagentNamespace, Labels: map[string]string{managedByLabel: managedByAgentlabValue}},
		Data:       map[string][]byte{skillsCredentialKey: []byte(credential)},
	}, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating the credential Secret %s/%s: %w", kagentNamespace, skillHostSecret, err)
	}
	return remove, nil
}

// skillHostRouteManifest is the edge's route to the fixture: the hostname
// skillhost.<domain>, the repository's path only — the record and the
// commit stay reachable from this host alone.
func skillHostRouteManifest(cfg *config.Config) string {
	return fmt.Sprintf(`apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    %[3]s: %[4]s
spec:
  parentRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: agentgateway
      namespace: %[2]s
      sectionName: https
  hostnames:
    - %[5]s.%[6]s
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /%[7]s
      backendRefs:
        - name: %[8]s
          port: 80
`, skillHostRoute, platformNamespace, managedByLabel, managedByAgentlabValue, skillHostPrefix, cfg.Platform.Domain, skillHostRepository, skillHostService)
}

// skillHostRouteReady bounds the edge's programming of the route.
const skillHostRouteReady = time.Minute

// applySkillHostRoute applies the route and waits until the fixture answers
// its 401 through the edge from this host; the returned func deletes it.
func applySkillHostRoute(cfg *config.Config) (func(), error) {
	gvr, err := gvrFor(httpRouteResource)
	if err != nil {
		return nil, err
	}
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
		defer cancel()
		if err := deleteObject(ctx, gvr, platformNamespace, skillHostRoute, 0); err != nil && !apierrors.IsNotFound(err) {
			note("cleanup: HTTPRoute %s/%s: %v", platformNamespace, skillHostRoute, err)
		}
	}
	remove()
	if _, err := applyManifests(context.Background(), []byte(skillHostRouteManifest(cfg))); err != nil {
		return nil, err
	}
	client, err := labHTTPClient(10 * time.Second)
	if err != nil {
		remove()
		return nil, err
	}
	probe := cfg.GatewayURL(skillHostPrefix) + "/" + skillHostRepository + "/info/refs?service=git-upload-pack"
	var last string
	if waitFor(int(skillHostRouteReady/(2*time.Second)), 2*time.Second, func() bool {
		resp, err := client.Get(probe)
		if err != nil {
			last = err.Error()
			return false
		}
		_ = resp.Body.Close()
		last = resp.Status
		return resp.StatusCode == http.StatusUnauthorized && strings.Contains(resp.Header.Get("WWW-Authenticate"), "agentlab skill host")
	}) {
		note("the edge routes %s to the fixture: 401 without the credential", probe)
		return remove, nil
	}
	remove()
	return nil, fmt.Errorf("the edge did not route %s to the skill host fixture within %s (last answer: %s)", probe, skillHostRouteReady, last)
}

// read is one of the fixture's own endpoints, from this host.
func (f *skillHostFixture) read(path string) (string, error) {
	resp, err := f.container.client.Get(f.container.hostURL + path)
	if err != nil {
		return "", fmt.Errorf("reading the skill host fixture's %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reading the skill host fixture's %s: %s %v", path, resp.Status, err)
	}
	return strings.TrimSpace(string(body)), nil
}

// requests is the fixture's record so far.
func (f *skillHostFixture) requests() ([]skillHostRequest, error) {
	body, err := f.read(skillHostRequestsPath)
	if err != nil {
		return nil, err
	}
	var out []skillHostRequest
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("the skill host fixture's record: %w", err)
	}
	return out, nil
}

// The verdict lines of the fixture run.
const (
	goldenCredentialSent     = "golden fetch: credential sent" // #nosec G101 -- a verdict line, no credential
	sessionNoCredential      = "session request: no credential"
	sessionCredentialFinding = "session request: credential sent"
)

// goldenFetchVerdict judges the requests recorded before the Agent was
// Ready: the golden boot's fetch reached the fixture with the Secret value.
func goldenFetchVerdict(golden []skillHostRequest) (string, error) {
	sent := 0
	for _, r := range golden {
		if r.Authorization == authSecret {
			sent++
		}
	}
	if sent == 0 {
		return "", fmt.Errorf("no request of the golden boot reached the fixture with the Secret value (%d recorded: %s)", len(golden), requestKinds(golden))
	}
	return fmt.Sprintf("%s (%d of %d requests before Ready carried the Secret value)", goldenCredentialSent, sent, len(golden)), nil
}

// sessionRequestVerdict judges the requests recorded from Ready on, the
// Session's: none carries the Secret value in any header, and the sandbox's
// request with the placeholder arrived with the placeholder, not replaced.
// The finding is the first request that carried the credential; a record
// without the placeholder request cannot tell, and says why.
func sessionRequestVerdict(session []skillHostRequest) (verdict string, finding *skillHostRequest, err error) {
	placeholders := 0
	for i, r := range session {
		if r.Authorization == authSecret || len(r.SecretIn) > 0 {
			return "", &session[i], nil
		}
		if r.Authorization == authPlaceholder {
			placeholders++
		}
	}
	if placeholders == 0 {
		return "", nil, fmt.Errorf("the sandbox's request with the placeholder never reached the fixture (%d session requests recorded: %s) — denied by the egress gateway, or the turn did not run it", len(session), requestKinds(session))
	}
	return fmt.Sprintf("%s (%d session requests, %d with the placeholder unreplaced, none with the Secret value)", sessionNoCredential, len(session), placeholders), nil, nil
}

// requestKinds words a record's Authorization kinds, in order.
func requestKinds(requests []skillHostRequest) string {
	if len(requests) == 0 {
		return "none"
	}
	kinds := make([]string, len(requests))
	for i, r := range requests {
		kinds[i] = r.Method + " " + r.Path + " " + r.Authorization
	}
	return strings.Join(kinds, "; ")
}
