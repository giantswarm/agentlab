package config

import (
	"strings"
	"testing"
)

// A client id as GitHub spells one, and an organization login.
const (
	testSignInClientID = "Iv23liLabClientId"
	testSignInOrg      = "giantswarm"
	testSignInSecret   = "lab-github-signin" // #nosec G101 -- a Secret NAME, not a credential
)

// TestGitHubSignInDefaults: off by default, the client Secret resolved to the
// default name in the Dex namespace, the callback the lab Dex's own.
func TestGitHubSignInDefaults(t *testing.T) {
	cfg := Default()
	if cfg.GitHubSignInEnabled() {
		t.Fatal("off by default")
	}
	want := SecretRef{Name: DefaultGitHubSignInSecretName, Namespace: GitHubSignInSecretNamespace}
	if got := cfg.Platform.GitHubSignIn.ClientSecret(); got != want {
		t.Fatalf("default client Secret %+v, want %+v", got, want)
	}
	if cfg.Platform.GitHubSignIn.Secret != "" {
		t.Fatalf("the default lives in the accessor, not in the file: %q", cfg.Platform.GitHubSignIn.Secret)
	}
	if got, want := cfg.GitHubSignInCallbackURL(), "https://localhost:32000/dex/callback"; got != want {
		t.Fatalf("callback %q, want %q", got, want)
	}
	cfg.Platform.GitHubSignIn = GitHubSignIn{Enabled: true, ClientID: testSignInClientID, Secret: testSignInSecret}
	want = SecretRef{Name: testSignInSecret, Namespace: GitHubSignInSecretNamespace}
	if got := cfg.Platform.GitHubSignIn.ClientSecret(); got != want {
		t.Fatalf("configured client Secret %+v, want %+v", got, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid lab: %v", err)
	}
}

// TestGitHubSignInIsValidated: a client id while on, no whitespace in it, a
// Secret name the apiserver accepts, organization logins GitHub accepts.
func TestGitHubSignInIsValidated(t *testing.T) {
	for _, tc := range []struct {
		block GitHubSignIn
		want  string
	}{
		{GitHubSignIn{Enabled: true}, "clientId is required"},
		{GitHubSignIn{Enabled: true, ClientID: "Iv23li Lab"}, "whitespace"},
		{GitHubSignIn{ClientID: testSignInClientID, Secret: "Not_A_Name"}, "secret"},
		{GitHubSignIn{Enabled: true, ClientID: testSignInClientID, Orgs: []string{"giant swarm"}}, "orgs"},
		{GitHubSignIn{Enabled: true, ClientID: testSignInClientID, Orgs: []string{"-giantswarm"}}, "orgs"},
	} {
		cfg := Default()
		cfg.Platform.GitHubSignIn = tc.block
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "platform.githubSignIn") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: want the error naming platform.githubSignIn and %q, got %v", tc.block, tc.want, err)
		}
	}
	// Off, a client id may stay in the file for the next time.
	cfg := Default()
	cfg.Platform.GitHubSignIn = GitHubSignIn{ClientID: testSignInClientID, Orgs: []string{testSignInOrg}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("off with a client id kept: %v", err)
	}
}

// TestGitHubSignInDecodesStrictly: the block reads from agentlab.yaml under
// its keys, and a key the schema does not know is refused like any other.
func TestGitHubSignInDecodesStrictly(t *testing.T) {
	cfg := Default()
	raw := []byte("platform:\n  githubSignIn:\n    enabled: true\n    clientId: Iv23liLabClientId\n    secret: lab-github-signin\n    orgs: [giantswarm]\n")
	if err := decodeStrict(raw, cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := cfg.Platform.GitHubSignIn
	if !got.Enabled || got.ClientID != testSignInClientID || got.Secret != testSignInSecret || strings.Join(got.Orgs, ",") != "giantswarm" {
		t.Fatalf("decoded %+v", got)
	}
	err := decodeStrict([]byte("platform:\n  githubSignIn:\n    clientSecret: literal\n"), Default())
	if err == nil || !strings.Contains(err.Error(), "clientSecret") {
		t.Fatalf("a literal client secret must be refused as an unknown field, got %v", err)
	}
}
