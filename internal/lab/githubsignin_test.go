package lab

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// A client id as GitHub spells one, and two organization logins.
const (
	testSignInClientID = "Iv23liLabClientId"
	testSignInOrg      = "giantswarm"
	testSignInOrgLabs  = "giantswarm-labs"
)

// gitHubSignInLab is a lab with the sign-in on: the client id by value, the
// client secret by its Secret's name, two organizations.
func gitHubSignInLab() *config.Config {
	cfg := config.Default()
	cfg.Platform.GitHubSignIn = config.GitHubSignIn{
		Enabled:  true,
		ClientID: testSignInClientID,
		Orgs:     []string{testSignInOrg, testSignInOrgLabs},
	}
	return cfg
}

// TestDexTemplateRendersTheGitHubConnector pins the connector's shape: the
// App's client id, the client secret as the pod's environment variable
// (expanded by Dex, never a literal), the lab Dex's own callback URL, the
// organizations with team slugs as groups, the variable read from the
// client Secret in the Dex namespace, and the local users still there.
func TestDexTemplateRendersTheGitHubConnector(t *testing.T) {
	cfg := gitHubSignInLab()
	raw, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"connectors:",
		"type: github",
		"id: " + gitHubSignInConnectorID,
		"clientID: " + testSignInClientID,
		"clientSecret: $" + config.GitHubSignInSecretEnv,
		"redirectURI: https://localhost:32000/dex/callback",
		"teamNameField: slug",
		"- name: " + testSignInOrg + "\n",
		"- name: " + testSignInOrgLabs + "\n",
		"name: " + config.GitHubSignInSecretEnv,
		"name: " + config.DefaultGitHubSignInSecretName,
		"key: " + config.GitHubClientSecretKey,
		"enablePasswordDB: true",
		"passwordConnector: local",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Dex missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "github-signin-client-version") {
		t.Errorf("an offline render knows no Secret version:\n%s", out)
	}
	// The render is applied as is: every document decodes, and the Dex
	// config inside the Secret is YAML whose connector references the
	// variable, never a value.
	objs, err := decodeManifests(raw)
	if err != nil {
		t.Fatalf("the rendered manifests do not decode: %v", err)
	}
	var dexConfig string
	for _, obj := range objs {
		if obj.GetKind() == kindSecret && obj.GetName() == "dex-config" {
			dexConfig, _, _ = unstructured.NestedString(obj.Object, "stringData", "config.yaml")
		}
	}
	var parsed struct {
		Connectors []struct {
			Type, ID string
			Config   map[string]any
		}
	}
	if err := yaml.Unmarshal([]byte(dexConfig), &parsed); err != nil {
		t.Fatalf("the Dex config does not parse: %v\n%s", err, dexConfig)
	}
	if len(parsed.Connectors) != 1 || parsed.Connectors[0].Type != "github" || parsed.Connectors[0].ID != gitHubSignInConnectorID {
		t.Fatalf("connectors %+v", parsed.Connectors)
	}
	if got := parsed.Connectors[0].Config["clientSecret"]; got != "$"+config.GitHubSignInSecretEnv {
		t.Fatalf("clientSecret %v, want the variable reference", got)
	}
}

// TestDexTemplateWithoutTheGitHubSignIn: off, the Dex config carries no
// connector and the pod no environment.
func TestDexTemplateWithoutTheGitHubSignIn(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, absent := range []string{"connectors:", "type: github", "env:", config.GitHubSignInSecretEnv, "secretKeyRef"} {
		if strings.Contains(out, absent) {
			t.Errorf("rendered Dex carries %q with the sign-in off:\n%s", absent, out)
		}
	}
}

// TestDexTemplateNamesTheConfiguredSecretAndStampsItsVersion: the pod reads
// the Secret platform.githubSignIn.secret names, and the version ApplyDex
// learned rolls the pod through its template annotation.
func TestDexTemplateNamesTheConfiguredSecretAndStampsItsVersion(t *testing.T) {
	cfg := gitHubSignInLab()
	cfg.Platform.GitHubSignIn.Secret = "lab-github-signin"
	cfg.Platform.GitHubSignIn.Orgs = nil
	raw, err := renderTemplate(cfg, "dex.yaml.tmpl", func(d *tmplData) { d.GitHubSignIn.SecretVersion = "4711" })
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out := string(raw)
	for _, want := range []string{
		"name: lab-github-signin",
		`agentlab.giantswarm.io/github-signin-client-version: "4711"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Dex missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"name: " + config.DefaultGitHubSignInSecretName + "\n", "orgs:"} {
		if strings.Contains(out, absent) {
			t.Errorf("rendered Dex carries %q:\n%s", absent, out)
		}
	}
}

// TestGitHubSignInValuesFollowTheConfig: nil while off; on, the values the
// template renders, with no Secret version before ApplyDex looked.
func TestGitHubSignInValuesFollowTheConfig(t *testing.T) {
	if v := gitHubSignInValuesFor(config.Default()); v != nil {
		t.Fatalf("off by default, got %+v", v)
	}
	cfg := gitHubSignInLab()
	cfg.DexPort = 31000
	v := gitHubSignInValuesFor(cfg)
	if v == nil {
		t.Fatal("on, got nil")
	}
	want := gitHubSignInValues{
		ConnectorID: gitHubSignInConnectorID,
		ClientID:    testSignInClientID,
		SecretName:  config.DefaultGitHubSignInSecretName,
		SecretKey:   config.GitHubClientSecretKey,
		SecretEnv:   config.GitHubSignInSecretEnv,
		RedirectURI: "https://localhost:31000/dex/callback",
		Orgs:        []string{testSignInOrg, testSignInOrgLabs},
	}
	if v.SecretVersion != "" || v.ConnectorID != want.ConnectorID || v.ClientID != want.ClientID ||
		v.SecretName != want.SecretName || v.SecretKey != want.SecretKey || v.SecretEnv != want.SecretEnv ||
		v.RedirectURI != want.RedirectURI || strings.Join(v.Orgs, ",") != strings.Join(want.Orgs, ",") {
		t.Fatalf("values %+v, want %+v", *v, want)
	}
}
