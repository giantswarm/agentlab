package config

import (
	"strings"
	"testing"
)

// TestGitHubSecretDefaults: platform.github reads the OAuth client from
// github-oauth-client in the platform namespace unless the config names
// another Secret, and the file carries no Secret fields nobody set.
func TestGitHubSecretDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Platform.GitHub.Enabled {
		t.Fatal("off by default")
	}
	want := SecretRef{Name: DefaultGitHubSecretName, Namespace: DefaultGitHubSecretNamespace}
	if got := cfg.Platform.GitHub.ClientSecret(); got != want {
		t.Fatalf("default client Secret %+v, want %+v", got, want)
	}
	if cfg.Platform.GitHub.Secret != (SecretRef{}) {
		t.Fatalf("the defaults live in the accessor, not in the file: %+v", cfg.Platform.GitHub.Secret)
	}
	const name = "lab-github-client"
	cfg.Platform.GitHub.Enabled = true
	cfg.Platform.GitHub.Secret = SecretRef{Name: name}
	want = SecretRef{Name: name, Namespace: DefaultGitHubSecretNamespace}
	if got := cfg.Platform.GitHub.ClientSecret(); got != want {
		t.Fatalf("a name alone keeps the default namespace: %+v, want %+v", got, want)
	}
	cfg.Platform.GitHub.Secret = SecretRef{Name: name, Namespace: "lab-secrets"}
	if got := cfg.Platform.GitHub.ClientSecret(); got != cfg.Platform.GitHub.Secret {
		t.Fatalf("configured client Secret %+v", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid lab: %v", err)
	}
}

// TestGitHubSecretNamesAreValidated: a name the apiserver would refuse is
// refused by configure, naming the field.
func TestGitHubSecretNamesAreValidated(t *testing.T) {
	for _, tc := range []struct {
		ref  SecretRef
		want string
	}{
		{SecretRef{Name: "Not_A_Name"}, "platform.github.secret.name"},
		{SecretRef{Namespace: "agent platform"}, "platform.github.secret.namespace"},
	} {
		cfg := Default()
		cfg.Platform.GitHub.Secret = tc.ref
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: want the error naming %s, got %v", tc.ref, tc.want, err)
		}
	}
}
