package lab

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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

// musterServerAuth is a rendered component block's muster.mcpServer.auth.
func musterServerAuth(block map[string]any) map[string]any {
	muster, _ := block["muster"].(map[string]any)
	server, _ := muster["mcpServer"].(map[string]any)
	auth, _ := server["auth"].(map[string]any)
	return auth
}

// With workspaces on, every manager muster forwards the person's token to
// requires the lab's `kubernetes` audience: agent-manager, model-manager and
// the workspace-manager, pinned to a provider App or not.
func TestManagersRequireKubernetesAudience(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceGitHubLab()
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testGrantAppID, ClientID: testGrantClientID}
	grant := workspaceGrantOf(workspaceProviderValues{Name: config.WorkspaceProviderGitHub, Kind: providerKindGitHub, AppID: testGrantAppID, ClientID: testGrantClientID, Secret: config.WorkspaceGitHubSecretName})
	for name, pin := range map[string]*workspaceGrantValues{"pinned": grant, "unpinned": nil} {
		t.Run(name, func(t *testing.T) {
			values, raw, _, _ := renderWorkspaceTemplates(t, cfg, func(d *tmplData) {
				d.WorkspaceGrant = pin
				d.ModelManagerEnabled = true
				d.ModelManagerBackends = []string{config.ModelManagerBackendOllama}
				d.ModelManagerEndpoints = map[string]string{config.ModelManagerBackendOllama: "http://host.agentlab.internal:11434"}
			})
			for _, manager := range []string{agentManagerMCPServer, modelManagerMCPServer, workspaceManagerMCPServer} {
				block, _ := values[manager].(map[string]any)
				got, _ := musterServerAuth(block)["requiredAudiences"].([]any)
				if len(got) != 1 || got[0] != config.KubernetesClientID {
					t.Errorf("%s.muster.mcpServer.auth.requiredAudiences = %v, want [%s]\n%s", manager, got, config.KubernetesClientID, raw)
				}
			}
		})
	}
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

// wantNoManagerSignIn asserts the person's provider sign-in is muster's: the
// lab Dex lists no route of the workspace-manager, and every instance's App
// registers muster's proxy callback alone.
func wantNoManagerSignIn(t *testing.T, cfg *config.Config, dex string) {
	t.Helper()
	if strings.Contains(dex, cfg.WorkspaceManagerBaseURL()) {
		t.Errorf("the lab Dex lists a workspace-manager route:\n%s", dex)
	}
	if got, want := WorkspaceAppCallback(cfg), cfg.MusterBaseURL()+"/oauth/proxy/callback"; got != want {
		t.Errorf("the App's callback URL %q, want muster's %s alone", got, want)
	}
}

// With workspaces on and nothing named, github is the one instance; alone
// or named, the values forward the base URL (the gateway port in it), no
// instance renders while the App's ids are empty (the workspace-manager
// refuses one without them at start-up), the lab Dex lists none of the
// manager's routes and the App's one callback is muster's.
func TestWorkspaceGitHubProviderWiring(t *testing.T) {
	stubWorkspacesProbe(t, true)
	const base = "https://workspace-manager.127.0.0.1.nip.io:8443"
	for _, provider := range []string{"", config.WorkspaceProviderGitHub} {
		cfg := workspaceLab(provider)
		if got := cfg.WorkspaceProviders(); !reflect.DeepEqual(got, []string{config.WorkspaceProviderGitHub}) {
			t.Fatalf("provider %q: instances %v, want github alone", provider, got)
		}
		values, _, dex, _ := renderWorkspaceTemplates(t, cfg, nil)
		wm, _ := workspaceManagerBlock(t, values)
		oauth, _ := wm["oauth"].(map[string]any)
		if oauth["baseURL"] != base {
			t.Fatalf("provider %q: workspace-manager.oauth.baseURL = %v, want %s", provider, oauth["baseURL"], base)
		}
		if _, ok := wm["providers"]; ok {
			t.Errorf("provider %q: an instance rendered without the App's ids: %v", provider, wm["providers"])
		}
		wantNoManagerSignIn(t, cfg, dex)
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

// Both at once: two instances in the workspace-manager's order, github
// first, each with its own Secret.
func TestWorkspaceBothProviders(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab("github, gitlab")
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	cfg.Platform.Workspaces.GitLab = testWorkspaceGitLab(testWorkspaceClientID)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	wm, providers := workspaceManagerBlock(t, values)
	if len(providers) != 2 || providers[0]["name"] != config.WorkspaceProviderGitHub || providers[1]["name"] != config.WorkspaceProviderGitLab {
		t.Fatalf("providers %v, want github then gitlab", wm["providers"])
	}
	for i, want := range []string{config.WorkspaceGitHubSecretName, config.WorkspaceGitLabSecretName} {
		v, _ := providers[i]["values"].(map[string]any)
		oauth, _ := v["oauth"].(map[string]any)
		wantSecretRef(t, providers[i]["name"].(string)+" oauth.clientSecret", oauth["clientSecret"], want, config.GitHubClientSecretKey)
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
	if strings.Contains(wantValues, "workspace-manager:") || strings.Contains(wantDex, "workspace-manager") || strings.Contains(wantCoreDNS, "workspace-manager") {
		t.Fatalf("a lab without workspaces renders the workspace-manager wiring")
	}
	for _, provider := range []string{"", config.WorkspaceProviderGitHub, "github,gitlab"} {
		cfg := workspaceLab(provider)
		cfg.Platform.Workspaces.Enabled = false
		cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
		if cfg.WorkspaceGitHubProvider() || cfg.WorkspaceGitLabProvider() || len(cfg.WorkspaceProviders()) != 0 {
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

// The instances' credentials reach the render as references only: every
// manifest `agentlab render` writes into state/ names the Secrets, and the
// values the install hands the chart reference each instance's Secret.
func TestWorkspaceProvidersRenderSecretReferences(t *testing.T) {
	stubWorkspacesProbe(t, true)
	t.Chdir(t.TempDir())
	cfg := workspaceLab("github,gitlab")
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	cfg.Platform.Workspaces.GitLab = testWorkspaceGitLab(testWorkspaceClientID)
	if err := GenCerts(cfg.Platform.Domain, false); err != nil {
		t.Fatal(err)
	}
	if err := RenderAll(cfg); err != nil {
		t.Fatal(err)
	}
	rendered, err := os.ReadFile(filepath.Join(StateDir, manifests[platformValuesTemplate].out)) // #nosec G304 -- the render's own output
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{config.WorkspaceGitHubSecretName, config.WorkspaceGitLabSecretName} {
		if !strings.Contains(string(rendered), "{name: "+want+", key: ") {
			t.Errorf("the values do not reference the Secret %s:\n%s", want, rendered)
		}
	}
}

// workspaceGitHubFor reads which keys the Secret carries: the instance once
// the App's ids and both keys are there, none while either is missing; the
// install's instance list follows it.
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
	providers, err := workspaceProvidersInstall(ctx, cfg)
	if err != nil || len(providers) != 0 {
		t.Errorf("the install's instances without the Secret: %v, %v; want none", providers, err)
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
	if err != nil || len(providers) != 1 || providers[0].Name != config.WorkspaceProviderGitHub {
		t.Errorf("the install's instances with both keys: %v, %v; want github", providers, err)
	}
	noIDs := workspaceGitHubLab()
	if got, err := workspaceGitHubFor(ctx, noIDs); err != nil || got != nil {
		t.Errorf("with the App's ids empty: %v, %v; want no instance", got, err)
	}
}

// The github instance's Secret from the recorded sources: one placement per
// recorded key into agent-platform/workspace-github, none without a source
// or without the instance, and a placement that fails fails the run.
func TestEnsureWorkspaceGitHubSecret(t *testing.T) {
	cfg := workspaceGitHubLab()
	calls := fakeSecretTool(t, func() error { return nil })
	if err := ensureWorkspaceSecrets(cfg); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("placed without a source: %v", *calls)
	}

	const (
		keySource    = "op://Lab/workspaces-app/private-key"   // #nosec G101 -- a reference, not a credential
		secretSource = "op://Lab/workspaces-app/client-secret" // #nosec G101 -- a reference, not a credential
	)
	cfg.Platform.Workspaces.GitHub = config.WorkspaceGitHub{
		AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID,
		PrivateKeySource: keySource, ClientSecretSource: secretSource,
	}
	if err := ensureWorkspaceSecrets(cfg); err != nil {
		t.Fatal(err)
	}
	target := "kind-" + cfg.ClusterName + "/agent-platform/" + config.WorkspaceGitHubSecretName + "/"
	want := [][]string{
		secretCopyArgs(keySource, target+config.WorkspaceGitHubPrivateKeyKey),
		secretCopyArgs(secretSource, target+config.GitHubClientSecretKey),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("placements %v, want %v", *calls, want)
	}

	fakeSecretTool(t, nil)
	if err := ensureWorkspaceSecrets(cfg); err == nil || !strings.Contains(err.Error(), "privateKeySource") {
		t.Errorf("a failed placement answered %v, want the run failed naming the source", err)
	}

	cfg.Platform.Workspaces.Provider = config.WorkspaceProviderGitLab
	calls = fakeSecretTool(t, func() error { return nil })
	if err := ensureWorkspaceSecrets(cfg); err != nil || len(*calls) != 0 {
		t.Errorf("placed %v (%v) without the github instance", *calls, err)
	}
}

// The gitlab instance names its Secret for the sync token and the client
// secret: references only, its URL and client id as agentlab.yaml has them.
func TestWorkspaceGitLabProviderInstance(t *testing.T) {
	stubWorkspacesProbe(t, true)
	cfg := workspaceLab(config.WorkspaceProviderGitLab)
	cfg.Platform.Workspaces.GitLab = testWorkspaceGitLabAt(testGitLabURL+"/", testWorkspaceClientID)
	values, _, _, _ := renderWorkspaceTemplates(t, cfg, nil)
	wm, providers := workspaceManagerBlock(t, values)
	if len(providers) != 1 || providers[0]["name"] != config.WorkspaceProviderGitLab || providers[0]["kind"] != config.WorkspaceProviderGitLab {
		t.Fatalf("providers %v, want gitlab", wm["providers"])
	}
	v, _ := providers[0]["values"].(map[string]any)
	oauth, _ := v["oauth"].(map[string]any)
	if v["url"] != testGitLabURL || oauth["clientID"] != testWorkspaceClientID {
		t.Errorf("url %v, oauth %v", v["url"], oauth)
	}
	if _, set := v["app"]; set {
		t.Errorf("a GitHub App rendered for the gitlab kind: %v", v["app"])
	}
	wantSecretRef(t, "token", v["token"], config.WorkspaceGitLabSecretName, config.WorkspaceGitLabTokenKey)
	wantSecretRef(t, "oauth.clientSecret", oauth["clientSecret"], config.WorkspaceGitLabSecretName, config.GitHubClientSecretKey)
	checkGolden(t, "gitlab", wm)

	cfg.Platform.Workspaces.GitLab.ClientID = ""
	values, _, _, _ = renderWorkspaceTemplates(t, cfg, nil)
	if _, providers = workspaceManagerBlock(t, values); len(providers) != 0 {
		t.Errorf("providers %v without the client id, want none", providers)
	}
}

// musterReleasable is the rendered muster.muster.credentialProvider.releasable,
// nil when the values name none.
func musterReleasable(values map[string]any) map[string]any {
	muster, _ := values["muster"].(map[string]any)
	inner, _ := muster["muster"].(map[string]any)
	provider, _ := inner["credentialProvider"].(map[string]any)
	releasable, _ := provider["releasable"].(map[string]any)
	return releasable
}

// With no provider instance rendered (the github instance before its App's
// ids, or the install's verdict none), muster's credential provider still
// releases the configured instance (github.com's OAuth issuer before the
// App's id, the App's own after), so the chart's guard of the credential
// provider renders, and the manager gets no grant signing key, which it
// refuses without an instance; once an instance renders, the chart derives
// the releasable from the pin and the values name none.
func TestWorkspaceReleasableWithoutInstance(t *testing.T) {
	stubWorkspacesProbe(t, true)
	values, _, _, _ := renderWorkspaceTemplates(t, workspaceGitHubLab(), nil)
	want := map[string]any{config.WorkspaceProviderGitHub: githubOAuthIssuer}
	if got := musterReleasable(values); !maps.Equal(got, want) {
		t.Errorf("github instance without its App's ids: releasable %v, want %v", got, want)
	}
	if wm, _ := workspaceManagerBlock(t, values); wm["grant"] != nil || wm["tokenExchange"] != nil {
		t.Errorf("no instance: grant %v, tokenExchange %v, want neither (the manager refuses a signing key without an instance)", wm["grant"], wm["tokenExchange"])
	}
	withIDs := workspaceGitHubLab()
	withIDs.Platform.Workspaces.GitHub = config.WorkspaceGitHub{AppID: testWorkspaceAppID, ClientID: testWorkspaceClientID}
	values, _, _, _ = renderWorkspaceTemplates(t, withIDs, func(d *tmplData) { d.WorkspaceProviders = nil })
	want = map[string]any{config.WorkspaceProviderGitHub: "https://github.com/apps/" + testWorkspaceAppID}
	if got := musterReleasable(values); !maps.Equal(got, want) {
		t.Errorf("the install's verdict none: releasable %v, want %v", got, want)
	}

	values, _, _, _ = renderWorkspaceTemplates(t, withIDs, nil)
	if got := musterReleasable(values); got != nil {
		t.Errorf("github instance rendered: releasable %v, want none (the chart derives it from the pin)", got)
	}
	if wm, _ := workspaceManagerBlock(t, values); wm["grant"] == nil {
		t.Error("github instance rendered: no grant signing key")
	}
	off := workspaceGitHubLab()
	off.Platform.Workspaces.Enabled = false
	values, _, _, _ = renderWorkspaceTemplates(t, off, nil)
	if got := musterReleasable(values); got != nil {
		t.Errorf("workspaces off: releasable %v, want none", got)
	}
}
