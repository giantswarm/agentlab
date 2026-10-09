package lab

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fixture serves its repository only with the credential: a shallow
// fetch of the pinned commit (what the Go ADK runs) fails without it and
// with the placeholder, succeeds with it, and the record carries the kind
// of each request's Authorization, never the value.
func TestSkillHostRequiresTheCredential(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("the skill host fixture needs git")
	}
	credential, err := newSkillHostCredential()
	if err != nil {
		t.Fatal(err)
	}
	h, err := newSkillHost(t.TempDir(), "Basic "+credential)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	source := server.URL + "/" + skillHostRepository

	for _, test := range []struct {
		name, header, kind string
		fetches            bool
	}{
		{name: "without Authorization", kind: authNone},
		{name: "placeholder", header: "Basic " + credentialPlaceholder, kind: authPlaceholder},
		{name: "credential", header: "Basic " + credential, kind: authSecret, fetches: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(h.requests)
			dir := t.TempDir()
			gitIn(t, dir, "init", "--quiet")
			args := []string{"fetch", "--quiet", "--depth", "1", source, h.commit}
			if test.header != "" {
				args = append([]string{"-c", "http.extraHeader=" + authorizationHeader + ": " + test.header}, args...)
			}
			out, err := gitCommand(dir, args...).CombinedOutput()
			if test.fetches != (err == nil) {
				t.Fatalf("git fetch with Authorization %s: err %v, want fetched %v: %s", test.name, err, test.fetches, out)
			}
			if test.fetches {
				gitIn(t, dir, "checkout", "--quiet", "--detach", "FETCH_HEAD")
				skill, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(skillHostSkillPath), "SKILL.md")) // #nosec G304 -- the test's own checkout
				if err != nil || !strings.Contains(string(skill), skillHostFact) {
					t.Fatalf("the fetched SKILL.md: %v %q", err, skill)
				}
			}
			recorded := h.requests[before:]
			if len(recorded) == 0 {
				t.Fatal("nothing recorded")
			}
			for _, r := range recorded {
				if r.Authorization != test.kind {
					t.Errorf("%s: Authorization recorded as %s, want %s", r, r.Authorization, test.kind)
				}
				if (len(r.SecretIn) > 0) != (test.kind == authSecret) || (len(r.PlaceholderIn) > 0) != (test.kind == authPlaceholder) {
					t.Errorf("%s: headers with the Secret %v, with the placeholder %v", r, r.SecretIn, r.PlaceholderIn)
				}
				if strings.Contains(r.String(), credential) {
					t.Errorf("the record words the Secret value: %s", r)
				}
			}
		})
	}
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...) // #nosec G204 -- the test's own git commands
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + os.TempDir(), "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "PATH=" + os.Getenv("PATH")}
	return cmd
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := gitCommand(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestGoldenFetchVerdict(t *testing.T) {
	got, err := goldenFetchVerdict([]skillHostRequest{{Seq: 1, Authorization: authSecret}, {Seq: 2, Authorization: authSecret}})
	if err != nil || !strings.HasPrefix(got, goldenCredentialSent) {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := goldenFetchVerdict([]skillHostRequest{{Seq: 1, Authorization: authNone}}); err == nil {
		t.Fatal("a golden boot without the credential passed")
	}
	if _, err := goldenFetchVerdict(nil); err == nil {
		t.Fatal("an empty record passed")
	}
}

func TestSessionRequestVerdict(t *testing.T) {
	const infoRefs = "/" + skillHostRepository + "/info/refs"
	bare := skillHostRequest{Seq: 3, Method: http.MethodGet, Path: infoRefs, Authorization: authNone}
	placeholder := skillHostRequest{Seq: 4, Method: http.MethodGet, Path: infoRefs, Authorization: authPlaceholder, PlaceholderIn: []string{authorizationHeader}}
	replaced := skillHostRequest{Seq: 4, Method: http.MethodGet, Path: infoRefs, Authorization: authSecret, SecretIn: []string{authorizationHeader}}
	elsewhere := skillHostRequest{Seq: 5, Method: http.MethodGet, Path: infoRefs, Authorization: authNone, SecretIn: []string{"X-Forwarded-Authorization"}}

	if got, finding, err := sessionRequestVerdict([]skillHostRequest{bare, placeholder}); err != nil || finding != nil || !strings.HasPrefix(got, sessionNoCredential) {
		t.Fatalf("the new line: %q %v %v", got, finding, err)
	}
	if _, finding, err := sessionRequestVerdict([]skillHostRequest{bare, replaced}); err != nil || finding == nil || finding.Seq != 4 {
		t.Fatalf("the previous line: finding %v, err %v", finding, err)
	}
	if _, finding, _ := sessionRequestVerdict([]skillHostRequest{bare, placeholder, elsewhere}); finding == nil || finding.Seq != 5 {
		t.Fatalf("the Secret in another header: finding %v", finding)
	}
	if _, _, err := sessionRequestVerdict([]skillHostRequest{bare}); err == nil {
		t.Fatal("a record without the placeholder request passed")
	}
}

func TestParseSandboxProbe(t *testing.T) {
	reply := "```\nAGENTLAB_PROBE bare=128\nAGENTLAB_PROBE placeholder=0\nAGENTLAB_PROBE error= \n```"
	p, err := parseSandboxProbe(reply)
	if err != nil || p.bare != 128 || p.placeholder != 0 || p.err != "" {
		t.Fatalf("got %+v, %v", p, err)
	}
	p, err = parseSandboxProbe("AGENTLAB_PROBE bare=128 AGENTLAB_PROBE placeholder=128\nAGENTLAB_PROBE error=fatal: Authentication failed for 'https://github.com/o/r/'")
	if err != nil || p.placeholder != 128 || !gitAuthRefusal(p.err) {
		t.Fatalf("got %+v, %v", p, err)
	}
	if _, err := parseSandboxProbe("I cannot run commands."); err == nil {
		t.Fatal("a reply without the markers parsed")
	}
}

func TestHostProbeVerdict(t *testing.T) {
	const source = "https://github.com/o/r"
	refused := sandboxProbe{bare: 128, placeholder: 128, err: "fatal: Authentication failed for 'https://github.com/o/r/'"}
	if got, finding, err := hostProbeVerdict(source, true, refused); err != nil || finding || !strings.HasPrefix(got, sessionNoCredential) {
		t.Fatalf("refused: %q %v %v", got, finding, err)
	}
	malformed := sandboxProbe{bare: 128, placeholder: 128, err: "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 400"}
	if got, finding, err := hostProbeVerdict(source, true, malformed); err != nil || finding || !strings.HasPrefix(got, sessionNoCredential) {
		t.Fatalf("GitHub's 400 to the placeholder: %q %v %v", got, finding, err)
	}
	if _, finding, err := hostProbeVerdict(source, true, sandboxProbe{bare: 128, placeholder: 0}); err != nil || !finding {
		t.Fatalf("accepted: finding %v, err %v", finding, err)
	}
	if _, _, err := hostProbeVerdict(source, true, sandboxProbe{bare: 128, placeholder: 128, err: "CONNECT tunnel failed, response 403"}); err == nil {
		t.Fatal("a denied request judged")
	}
	if got, finding, err := hostProbeVerdict(source, false, sandboxProbe{bare: 0, placeholder: 128, err: "fatal: Authentication failed"}); err != nil || finding || !strings.Contains(got, "github.com") {
		t.Fatalf("public: %q %v %v", got, finding, err)
	}
}

func TestSkillHostRouteManifest(t *testing.T) {
	cfg := config.Default()
	got := skillHostRouteManifest(cfg)
	for _, want := range []string{"- " + skillHostPrefix + "." + cfg.Platform.Domain, "value: /" + skillHostRepository, "name: " + skillHostService} {
		if !strings.Contains(got, want) {
			t.Errorf("the route lacks %q:\n%s", want, got)
		}
	}
}
