package lab

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
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

// The golden renders of the workspace-manager block, one per provider
// configuration (testdata/workspace-providers): what the platform chart is
// handed. AGENTLAB_UPDATE_GOLDEN=1 writes them from the render.
const (
	workspaceGoldenDir = "testdata/workspace-providers"
	updateGoldenEnv    = "AGENTLAB_UPDATE_GOLDEN"
)

// workspaceLab is a lab with workspaces on, the provider instances provider
// names ("" for the default) and the edge on a non-default gateway port, so
// the port shows in every URL.
func workspaceLab(provider string) *config.Config {
	cfg := config.Default()
	cfg.Platform.GatewayPort = 8443
	cfg.Platform.Workspaces.Enabled = true
	cfg.Platform.Workspaces.Provider = provider
	return cfg
}

// workspaceGitHubLab is a lab with the github instance alone.
func workspaceGitHubLab() *config.Config { return workspaceLab(config.WorkspaceProviderGitHub) }

// renderWorkspaceTemplates renders the platform values, the Dex manifest
// and the CoreDNS Corefile offline, the values parsed.
func renderWorkspaceTemplates(t *testing.T, cfg *config.Config, mutate func(*tmplData)) (values map[string]any, raw, dex, coredns string) {
	t.Helper()
	out, err := renderTemplate(cfg, platformValuesTemplate, mutate)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	dexOut, err := renderTemplate(cfg, "dex.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	corednsOut, err := renderTemplate(cfg, "coredns.yaml.tmpl", func(d *tmplData) { d.LabHostIP = "172.21.0.1" })
	if err != nil {
		t.Fatal(err)
	}
	return values, string(out), string(dexOut), string(corednsOut)
}

// workspaceManagerBlock is the rendered workspace-manager block, and its
// provider instances.
func workspaceManagerBlock(t *testing.T, values map[string]any) (map[string]any, []map[string]any) {
	t.Helper()
	wm, _ := values["workspace-manager"].(map[string]any)
	if wm == nil {
		t.Fatalf("no workspace-manager block rendered: %v", values)
	}
	raw, _ := wm["providers"].([]any)
	providers := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		m, _ := p.(map[string]any)
		providers = append(providers, m)
	}
	return wm, providers
}

// wantSecretRef checks a credential renders as the Secret reference {name,
// key} and nothing else.
func wantSecretRef(t *testing.T, field string, got any, secret, key string) {
	t.Helper()
	ref, _ := got.(map[string]any)
	if len(ref) != 2 || ref["name"] != secret || ref["key"] != key {
		t.Errorf("%s = %v, want {name: %s, key: %s}", field, got, secret, key)
	}
}

// checkGolden compares the rendered workspace-manager block with its golden
// file, or writes the file with AGENTLAB_UPDATE_GOLDEN set.
func checkGolden(t *testing.T, name string, block map[string]any) {
	t.Helper()
	got, err := yaml.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspaceGoldenDir, name+".yaml")
	if os.Getenv(updateGoldenEnv) != "" {
		if err := os.MkdirAll(workspaceGoldenDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) // #nosec G304 -- the test's own golden file
	if err != nil {
		t.Fatalf("%v (%s=1 writes it)", err, updateGoldenEnv)
	}
	if string(got) != string(want) {
		t.Errorf("the rendered workspace-manager block differs from %s (%s=1 rewrites it):\n--- got\n%s--- want\n%s", path, updateGoldenEnv, got, want)
	}
}

// With workspaces on and nothing named, the fake is the one instance: the
// lab's GitHub at its URL (the API under /api/v3 is the kind's derivation),
// the embedded fixture's App and client, each credential a reference to
// the lab's Secret; the manager trusts the lab CA for it; CoreDNS sends its
// name to its Service; the base URL carries the gateway port and the lab
// Dex's agent-platform client lists <base>/signin.
func TestWorkspaceFakeProviderDefault(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab("")
	const base = "https://workspace-manager.127.0.0.1.nip.io:8443"
	if got := cfg.WorkspaceManagerBaseURL(); got != base {
		t.Fatalf("base URL %s, want %s", got, base)
	}
	if got, want := cfg.GitHubFakeURL(), "https://github.127.0.0.1.nip.io"; got != want {
		t.Fatalf("the fake's URL %s, want %s (port-free: pods reach its Service on 443)", got, want)
	}
	values, _, dex, coredns := renderWorkspaceTemplates(t, cfg, nil)
	wm, providers := workspaceManagerBlock(t, values)
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
	if len(providers) != 1 || providers[0]["name"] != config.WorkspaceProviderFake || providers[0]["kind"] != config.WorkspaceProviderGitHub {
		t.Fatalf("providers %v, want the fake instance of the github kind", wm["providers"])
	}
	fixture, err := loadGitHubFixture("")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := providers[0]["values"].(map[string]any)
	app, _ := v["app"].(map[string]any)
	appOAuth, _ := v["oauth"].(map[string]any)
	if v["url"] != cfg.GitHubFakeURL() {
		t.Errorf("url = %v, want %s", v["url"], cfg.GitHubFakeURL())
	}
	if _, set := v["apiURL"]; set {
		t.Errorf("apiURL set to %v: the kind derives <url>/api/v3 itself", v["apiURL"])
	}
	if app["id"] != "1" || appOAuth["clientID"] != fixture.App.ClientID {
		t.Errorf("app %v, oauth %v: want the embedded fixture's App %d and client %s", app, appOAuth, fixture.App.ID, fixture.App.ClientID)
	}
	wantSecretRef(t, "app.privateKey", app["privateKey"], config.WorkspaceFakeSecretName, config.WorkspaceGitHubPrivateKeyKey)
	wantSecretRef(t, "oauth.clientSecret", appOAuth["clientSecret"], config.WorkspaceFakeSecretName, config.GitHubClientSecretKey)
	env, _ := wm["extraEnv"].([]any)
	if len(env) != 1 {
		t.Fatalf("extraEnv %v, want the one entry that names the lab CA", wm["extraEnv"])
	}
	if e, _ := env[0].(map[string]any); e["name"] != "SSL_CERT_FILE" || e["value"] != "/etc/workspace-manager/idp-ca/ca.crt" {
		t.Errorf("extraEnv %v, want SSL_CERT_FILE naming the mounted Dex CA (the lab CA)", e)
	}
	if !strings.Contains(coredns, `name regex ^github\.127\.0\.0\.1\.nip\.io\. `+labGitHubService+".agent-platform.svc.cluster.local") {
		t.Errorf("CoreDNS does not send %s to the fake's Service:\n%s", config.GitHubFakeHost(cfg.Platform.Domain), coredns)
	}
	if !strings.Contains(dex, "- "+base+"/signin\n") {
		t.Errorf("the Dex agent-platform client does not list %s/signin:\n%s", base, dex)
	}
	if got := cfg.WorkspaceCallbackURL(config.WorkspaceProviderFake); got != base+"/callback/fake" {
		t.Errorf("callback URL %s", got)
	}
	checkGolden(t, "fake", wm)
}

// With the github instance alone, the values forward the base URL, no
// instance renders while the App's ids are empty (the workspace-manager
// refuses one without them at start-up), nothing names the lab CA and
// CoreDNS leaves the fake's name to the wildcard.
func TestWorkspaceGitHubProviderWiring(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceGitHubLab()
	const base = "https://workspace-manager.127.0.0.1.nip.io:8443"
	values, _, dex, coredns := renderWorkspaceTemplates(t, cfg, nil)
	wm, _ := workspaceManagerBlock(t, values)
	oauth, _ := wm["oauth"].(map[string]any)
	if oauth["baseURL"] != base {
		t.Fatalf("workspace-manager.oauth.baseURL = %v, want %s", oauth["baseURL"], base)
	}
	if _, ok := wm["providers"]; ok {
		t.Errorf("a provider instance rendered without the App's ids: %v", wm["providers"])
	}
	if _, ok := wm["extraEnv"]; ok {
		t.Errorf("the lab CA named without the fake: %v", wm["extraEnv"])
	}
	if strings.Contains(coredns, labGitHubService) {
		t.Errorf("CoreDNS sends the fake's name to its Service without the fake:\n%s", coredns)
	}
	if !strings.Contains(dex, "- "+base+"/signin\n") {
		t.Errorf("the Dex agent-platform client does not list %s/signin:\n%s", base, dex)
	}
	if cfg.WorkspaceGitHubCallbackURL() != base+"/callback/github" {
		t.Errorf("callback URL %s", cfg.WorkspaceGitHubCallbackURL())
	}
}

// With the App's ids set, the github instance names the Secret for the
// private key and the client secret: the values carry references, never a
// value; and the install renders what the cluster allows.
func TestWorkspaceGitHubProviderInstance(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceGitHubLab()
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	wm, providers := workspaceManagerBlock(t, values)
	if len(providers) != 1 {
		t.Fatalf("providers %v, want the github instance", wm["providers"])
	}
	p := providers[0]
	if p["name"] != config.WorkspaceProviderGitHub || p["kind"] != config.WorkspaceProviderGitHub {
		t.Fatalf("instance %v", p)
	}
	v, _ := p["values"].(map[string]any)
	app, _ := v["app"].(map[string]any)
	oauth, _ := v["oauth"].(map[string]any)
	if _, set := v["url"]; set {
		t.Errorf("url set to %v: github.com is the kind's default", v["url"])
	}
	if app["id"] != testWorkspaceAppID || oauth["clientID"] != testWorkspaceClientID {
		t.Errorf("app %v, oauth %v", app, oauth)
	}
	wantSecretRef(t, "app.privateKey", app["privateKey"], config.WorkspaceGitHubSecretName, config.WorkspaceGitHubPrivateKeyKey)
	wantSecretRef(t, "oauth.clientSecret", oauth["clientSecret"], config.WorkspaceGitHubSecretName, config.GitHubClientSecretKey)
	checkGolden(t, "github", wm)

	// The install renders what the cluster allows: no instance while the
	// Secret is missing, the instance once it carries both keys.
	values, _, _, _ = renderWorkspaceTemplates(t, cfg, func(d *tmplData) { d.WorkspaceProviders = nil })
	wm, _ = workspaceManagerBlock(t, values)
	if _, ok := wm["providers"]; ok {
		t.Errorf("an instance rendered with the install's verdict none: %v", wm["providers"])
	}
}

// Both at once: two instances in the workspace-manager's order, the fake
// first, each with its own Secret, the lab CA named for the fake.
func TestWorkspaceBothProviders(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab("github, fake")
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	values, _, _, coredns := renderWorkspaceTemplates(t, cfg, nil)
	wm, providers := workspaceManagerBlock(t, values)
	if len(providers) != 2 || providers[0]["name"] != config.WorkspaceProviderFake || providers[1]["name"] != config.WorkspaceProviderGitHub {
		t.Fatalf("providers %v, want fake then github", wm["providers"])
	}
	for i, want := range []string{config.WorkspaceFakeSecretName, config.WorkspaceGitHubSecretName} {
		v, _ := providers[i]["values"].(map[string]any)
		app, _ := v["app"].(map[string]any)
		wantSecretRef(t, providers[i]["name"].(string)+" app.privateKey", app["privateKey"], want, config.WorkspaceGitHubPrivateKeyKey)
	}
	if _, ok := wm["extraEnv"]; !ok {
		t.Error("the lab CA is not named with the fake among the instances")
	}
	if !strings.Contains(coredns, labGitHubService) {
		t.Error("CoreDNS does not send the fake's name to its Service with the fake among the instances")
	}
	checkGolden(t, "both", wm)
}

// Without the switch nothing changes: the values, the Dex manifest and the
// Corefile are byte for byte those of a lab without workspaces, whatever
// provider and App ids agentlab.yaml carries.
func TestWorkspaceProviderInertWithWorkspacesOff(t *testing.T) {
	stubWorkspacesProbe(t, true)
	plain := config.Default()
	plain.Platform.GatewayPort = 8443
	_, wantValues, wantDex, wantCoreDNS := renderWorkspaceTemplates(t, plain, nil)
	if strings.Contains(wantValues, "workspace-manager:") || strings.Contains(wantDex, "workspace-manager") || strings.Contains(wantCoreDNS, labGitHubService) {
		t.Fatalf("a lab without workspaces renders the workspace-manager wiring")
	}
	for _, provider := range []string{"", config.WorkspaceProviderGitHub, "fake,github"} {
		cfg := workspaceLab(provider)
		cfg.Platform.Workspaces.Enabled = false
		cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
		if cfg.WorkspaceFakeProvider() || cfg.WorkspaceGitHubProvider() || len(cfg.WorkspaceProviders()) != 0 {
			t.Errorf("provider %q counts with workspaces off", provider)
		}
		_, values, dex, coredns := renderWorkspaceTemplates(t, cfg, nil)
		if values != wantValues {
			t.Errorf("provider %q with workspaces off: the values differ", provider)
		}
		if dex != wantDex {
			t.Errorf("provider %q with workspaces off: the Dex manifest differs", provider)
		}
		if coredns != wantCoreDNS {
			t.Errorf("provider %q with workspaces off: the Corefile differs", provider)
		}
	}
}

// No secret value reaches a rendered file: with the fake's credentials
// generated (the App's private key, the client secret), every manifest
// `agentlab render` writes into state/ and the values the install hands the
// chart reference the Secrets and carry neither.
func TestWorkspaceProvidersRenderNoSecretValue(t *testing.T) {
	stubWorkspacesProbe(t, true)
	t.Chdir(t.TempDir())
	cfg := workspaceLab("fake,github")
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	if err := GenCerts(cfg.Platform.Domain, false); err != nil {
		t.Fatal(err)
	}
	dir, err := EnsureGitHubFakeCredentials(cfg.Platform.Domain)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join(dir, githubFakeClientSecret)) // #nosec G304 -- the test's own credentials
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, githubFakeAppKey)) // #nosec G304 -- the test's own credentials
	if err != nil {
		t.Fatal(err)
	}
	keyLines := strings.Split(strings.TrimSpace(string(key)), "\n")
	if len(keyLines) < 3 {
		t.Fatalf("the App's key is not a PEM block:\n%s", key)
	}
	values := []string{strings.TrimSpace(string(secret)), keyLines[1], "PRIVATE KEY"}
	if err := RenderAll(cfg); err != nil {
		t.Fatal(err)
	}
	var files []string
	if err := filepath.WalkDir(StateDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("agentlab render wrote nothing into %s", StateDir)
	}
	for _, path := range files {
		raw, err := os.ReadFile(path) // #nosec G304 -- the render's own output
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range values {
			if strings.Contains(string(raw), v) {
				t.Errorf("%s carries a secret value (%.12s…)", path, v)
			}
		}
	}
	rendered, err := os.ReadFile(filepath.Join(StateDir, manifests[platformValuesTemplate].out)) // #nosec G304 -- the render's own output
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{config.WorkspaceFakeSecretName, config.WorkspaceGitHubSecretName} {
		if !strings.Contains(string(rendered), "{name: "+want+", key: ") {
			t.Errorf("the values do not reference the Secret %s:\n%s", want, rendered)
		}
	}
}

// workspaceGitHubFor reads which keys the Secret carries: the instance once
// the App's ids and both keys are there, none while either is missing; and
// the install's instance list carries the fake regardless.
func TestWorkspaceGitHubForReadsTheSecretsKeys(t *testing.T) {
	ctx := context.Background()
	cfg := workspaceLab("fake,github")
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
	providers, err := workspaceProvidersInstall(ctx, cfg)
	if err != nil || len(providers) != 1 || providers[0].Name != config.WorkspaceProviderFake {
		t.Errorf("the install's instances without the Secret: %v, %v; want the fake alone", providers, err)
	}
	newFakeLab(t, secret(config.WorkspaceGitHubPrivateKeyKey))
	if got, err := workspaceGitHubFor(ctx, cfg); err != nil || got != nil {
		t.Errorf("with the client secret missing: %v, %v; want no instance", got, err)
	}
	newFakeLab(t, secret(workspaceGitHubKeys...))
	got, err := workspaceGitHubFor(ctx, cfg)
	if err != nil || got == nil || got.AppID != testWorkspaceAppID || got.Secret != config.WorkspaceGitHubSecretName || got.URL != "" {
		t.Errorf("with both keys: %+v, %v; want the instance", got, err)
	}
	providers, err = workspaceProvidersInstall(ctx, cfg)
	if err != nil || len(providers) != 2 || providers[1].Name != config.WorkspaceProviderGitHub {
		t.Errorf("the install's instances with both keys: %v, %v; want the fake and github", providers, err)
	}
	noIDs := workspaceGitHubLab()
	if got, err := workspaceGitHubFor(ctx, noIDs); err != nil || got != nil {
		t.Errorf("with the App's ids empty: %v, %v; want no instance", got, err)
	}
}
