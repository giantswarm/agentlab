package lab

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The App's public ids as GitHub spells them.
const (
	testWorkspaceAppID    = "123456"
	testWorkspaceClientID = "Iv23liWorkspaceLab"
)

// workspaceGitHubLab is a lab with workspaces on, the provider github and
// the edge on a non-default gateway port, so the port shows in every URL.
func workspaceGitHubLab() *config.Config {
	cfg := config.Default()
	cfg.Platform.GatewayPort = 8443
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Provider = config.WorkspaceProviderGitHub
	return cfg
}

// renderWorkspaceTemplates renders the platform values and the Dex manifest
// offline, the values parsed.
func renderWorkspaceTemplates(t *testing.T, cfg *config.Config, mutate func(*tmplData)) (map[string]any, string, string) {
	t.Helper()
	values, err := renderTemplate(cfg, platformValuesTemplate, mutate)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(values, &parsed); err != nil {
		t.Fatalf("%v\n%s", err, values)
	}
	dex, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	return parsed, string(values), string(dex)
}

// With the provider set, the values forward the base URL with the gateway
// port, whose host is the one the chart's route serves /signin, /connect and
// /callback on; the lab Dex's agent-platform client lists <base>/signin; and
// with the App's ids empty no instance renders (the workspace-manager
// refuses one without them at start-up).
func TestWorkspaceGitHubProviderWiring(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceGitHubLab()
	const base = "https://workspace-manager.127.0.0.1.nip.io:8443"
	if got := cfg.WorkspaceManagerBaseURL(); got != base {
		t.Fatalf("base URL %s, want %s", got, base)
	}
	values, _, dex := renderWorkspaceTemplates(t, cfg, nil)
	wm, _ := values["workspace-manager"].(map[string]any)
	oauth, _ := wm["oauth"].(map[string]any)
	if oauth["baseURL"] != base {
		t.Fatalf("workspace-manager.oauth.baseURL = %v, want %s", oauth["baseURL"], base)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "workspace-manager."+cfg.Platform.Domain {
		t.Errorf("route host %s, want workspace-manager.%s", u.Hostname(), cfg.Platform.Domain)
	}
	if _, ok := wm["providers"]; ok {
		t.Errorf("a provider instance rendered without the App's ids: %v", wm["providers"])
	}
	if !strings.Contains(dex, "- "+base+"/signin\n") {
		t.Errorf("the Dex agent-platform client does not list %s/signin:\n%s", base, dex)
	}
	if cfg.WorkspaceGitHubCallbackURL() != base+"/callback/github" {
		t.Errorf("callback URL %s", cfg.WorkspaceGitHubCallbackURL())
	}
}

// With the App's ids set, the instance names the Secret for the private key
// and the client secret: the values carry references, never a value.
func TestWorkspaceGitHubProviderInstance(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceGitHubLab()
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	values, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	wm, _ := values["workspace-manager"].(map[string]any)
	providers, _ := wm["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("providers %v, want the github instance", wm["providers"])
	}
	p, _ := providers[0].(map[string]any)
	if p["name"] != config.WorkspaceProviderGitHub || p["kind"] != config.WorkspaceProviderGitHub {
		t.Fatalf("instance %v", p)
	}
	v, _ := p["values"].(map[string]any)
	app, _ := v["app"].(map[string]any)
	oauth, _ := v["oauth"].(map[string]any)
	if app["id"] != testWorkspaceAppID || oauth["clientID"] != testWorkspaceClientID {
		t.Errorf("app %v, oauth %v", app, oauth)
	}
	wantRef := func(field string, got any, key string) {
		ref, _ := got.(map[string]any)
		if len(ref) != 2 || ref["name"] != config.WorkspaceGitHubSecretName || ref["key"] != key {
			t.Errorf("%s = %v, want {name: %s, key: %s}", field, got, config.WorkspaceGitHubSecretName, key)
		}
	}
	wantRef("app.privateKey", app["privateKey"], config.WorkspaceGitHubPrivateKeyKey)
	wantRef("oauth.clientSecret", oauth["clientSecret"], config.GitHubClientSecretKey)

	// The install renders what the cluster allows: no instance while the
	// Secret is missing, the instance once it carries both keys.
	stubWorkspacesProbe(t, true)
	values, _, _ = renderWorkspaceTemplates(t, cfg, func(d *tmplData) { d.WorkspaceGitHub = nil })
	wm, _ = values["workspace-manager"].(map[string]any)
	if _, ok := wm["providers"]; ok {
		t.Errorf("an instance rendered with the install's verdict nil: %v", wm["providers"])
	}
}

// Without the provider nothing changes: the values and the Dex manifest are
// byte for byte those of a lab with workspaces and no provider, whatever
// App ids agentlab.yaml carries; with workspaces off the provider is inert.
func TestWorkspaceGitHubProviderUnsetChangesNothing(t *testing.T) {
	stubWorkspacesProbe(t, true)
	plain := config.Default()
	plain.Platform.GatewayPort = 8443
	plain.Platform.Workspaces.Enabled = true
	_, wantValues, wantDex := renderWorkspaceTemplates(t, plain, nil)
	if strings.Contains(wantValues, "workspace-manager:") || strings.Contains(wantDex, "workspace-manager") {
		t.Fatalf("a lab without the provider renders the workspace-manager wiring")
	}

	withIDs := workspaceGitHubLab()
	withIDs.Platform.Workspaces.Provider = ""
	withIDs.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	off := workspaceGitHubLab()
	off.Platform.Workspaces.Enabled = false
	plainOff := config.Default()
	plainOff.Platform.GatewayPort = 8443
	_, wantOffValues, wantOffDex := renderWorkspaceTemplates(t, plainOff, nil)
	for _, tc := range []struct {
		name                string
		cfg                 *config.Config
		wantValues, wantDex string
	}{
		{"App ids without the provider", withIDs, wantValues, wantDex},
		{"the provider with workspaces off", off, wantOffValues, wantOffDex},
	} {
		_, values, dex := renderWorkspaceTemplates(t, tc.cfg, nil)
		if values != tc.wantValues {
			t.Errorf("%s: the values differ", tc.name)
		}
		if dex != tc.wantDex {
			t.Errorf("%s: the Dex manifest differs", tc.name)
		}
	}
}

// workspaceGitHubFor reads which keys the Secret carries: the instance once
// the App's ids and both keys are there, none while either is missing.
func TestWorkspaceGitHubForReadsTheSecretsKeys(t *testing.T) {
	ctx := context.Background()
	cfg := workspaceGitHubLab()
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	secret := func(keys ...string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: config.WorkspaceGitHubSecretName, Namespace: platformNamespace}, Data: map[string][]byte{}}
		for _, k := range keys {
			s.Data[k] = []byte("x")
		}
		return s
	}

	newFakeLab(t)
	if got, err := workspaceGitHubFor(ctx, cfg); err != nil || got != nil {
		t.Errorf("without the Secret: %v, %v; want no instance", got, err)
	}
	newFakeLab(t, secret(config.WorkspaceGitHubPrivateKeyKey))
	if got, err := workspaceGitHubFor(ctx, cfg); err != nil || got != nil {
		t.Errorf("with the client secret missing: %v, %v; want no instance", got, err)
	}
	newFakeLab(t, secret(workspaceGitHubKeys...))
	got, err := workspaceGitHubFor(ctx, cfg)
	if err != nil || got == nil || got.AppID != testWorkspaceAppID || got.Secret != config.WorkspaceGitHubSecretName {
		t.Errorf("with both keys: %+v, %v; want the instance", got, err)
	}
	noIDs := workspaceGitHubLab()
	if got, err := workspaceGitHubFor(ctx, noIDs); err != nil || got != nil {
		t.Errorf("with the App's ids empty: %v, %v; want no instance", got, err)
	}
}
