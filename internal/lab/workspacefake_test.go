package lab

import (
	"os"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestCheckLabGitHubBinary: the up-front check judges this binary exactly as
// the lab's GitHub container start would (the test binary is whatever the
// build made it, so the two verdicts are compared, not pinned), names the
// instance so the refusal reads with agentlab.yaml, and judges nothing while
// the instance is not configured.
func TestCheckLabGitHubBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := workspaceLab("")
	if !cfg.WorkspaceFakeProvider() {
		t.Fatal("the default lab has no provider instance fake")
	}
	got, want := checkLabGitHubBinary(cfg), linuxStaticBinary(exe, labGitHubWhat, "")
	if (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
		t.Errorf("checkLabGitHubBinary = %v, the container start says %v", got, want)
	}
	if got != nil && !strings.Contains(got.Error(), "provider instance "+config.WorkspaceProviderFake) {
		t.Errorf("the refusal does not name the instance: %v", got)
	}
	off := workspaceLab("")
	off.Platform.Workspaces.Enabled = false
	for name, cfg := range map[string]*config.Config{"the github instance alone": workspaceGitHubLab(), "workspaces off": off} {
		if err := checkLabGitHubBinary(cfg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestGitHubFakeEdgeManifest: the edge serves the authorization endpoint
// alone, on the instance's host and fake-bearer's git host where that is
// configured, to the fake's Service over TLS verified against the lab CA.
func TestGitHubFakeEdgeManifest(t *testing.T) {
	const host = "github.lab.test"
	for _, tc := range []struct {
		providers string
		hosts     []string
	}{
		{"", []string{host}},
		{config.WorkspaceProviderFake + "," + config.WorkspaceProviderFakeBearer, []string{host, "github-bearer.lab.test"}},
	} {
		cfg := workspaceLab(tc.providers)
		cfg.Platform.Domain = "lab.test"
		raw, err := githubFakeEdgeManifest(cfg, []byte("CA PEM"))
		if err != nil {
			t.Fatal(err)
		}
		objs, err := decodeManifests(raw)
		if err != nil || len(objs) != 3 {
			t.Fatalf("%q: %d objects, %v", tc.providers, len(objs), err)
		}
		byKind := map[string]map[string]any{}
		for _, o := range objs {
			if o.GetNamespace() != platformNamespace || o.GetName() != labGitHubEdge {
				t.Errorf("%s %s/%s", o.GetKind(), o.GetNamespace(), o.GetName())
			}
			byKind[o.GetKind()] = o.Object
		}
		if got, _, _ := unstructured.NestedString(byKind["ConfigMap"], "data", caCertKey); got != "CA PEM" {
			t.Errorf("the CA ConfigMap carries %q", got)
		}
		if got, _, _ := unstructured.NestedString(byKind["BackendTLSPolicy"], "spec", "validation", "hostname"); got != host {
			t.Errorf("the BackendTLSPolicy verifies %q", got)
		}
		hosts, _, _ := unstructured.NestedStringSlice(byKind["HTTPRoute"], "spec", "hostnames")
		if !slices.Equal(hosts, tc.hosts) {
			t.Errorf("%q: the route's hosts = %v, want %v", tc.providers, hosts, tc.hosts)
		}
		rules, _, _ := unstructured.NestedSlice(byKind["HTTPRoute"], "spec", "rules")
		if len(rules) != 1 {
			t.Fatalf("the route's rules = %v", rules)
		}
		rule, _ := rules[0].(map[string]any)
		matches, _, _ := unstructured.NestedSlice(rule, "matches")
		path, _ := matches[0].(map[string]any)["path"].(map[string]any)
		if len(matches) != 1 || path["type"] != "Exact" || path["value"] != githubAuthorizePath {
			t.Errorf("the route matches %v, want the authorization endpoint alone", matches)
		}
	}
}
