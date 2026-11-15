package lab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// testSourceValue is what the fake readers answer: a test value whose
// absence from every error and output line the tests check.
const testSourceValue = "s3cr3t-value-from-the-source" // #nosec G101 -- a test value, not a credential

// fakeReader puts an executable name on a PATH of its own that records its
// arguments, one per line, in a file and runs body; it answers that file.
func fakeReader(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	args := filepath.Join(dir, name+".args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + args + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { // #nosec G306 -- a test's fake CLI
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return args
}

// readArgs is what a fake reader was called with.
func readArgs(t *testing.T, file string) []string {
	t.Helper()
	raw, err := os.ReadFile(file) // #nosec G304 -- the test's own file
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// TestSecretReader: every reference form config.ValidateSecretSource takes
// has its reader, and the parse keeps its parts.
func TestSecretReader(t *testing.T) {
	for _, tc := range []struct {
		source, reader string
		want           secretRef
	}{
		{"op://Employee/agentlab.anthropic-key/credential", opBin, secretRef{op: "op://Employee/agentlab.anthropic-key/credential"}},
		{"/lab/agents.sops.yaml#stringData.private-key", sopsBin, secretRef{file: "/lab/agents.sops.yaml", path: "stringData.private-key"}},
		{"sops://lab.sops.yaml#a.b", sopsBin, secretRef{file: "lab.sops.yaml", path: "a.b"}},
		{"k8s://kind-agentlab/kagent/kagent-anthropic/" + AnthropicKeyEnv, "", secretRef{kubeContext: "kind-agentlab", ns: kagentNamespace, secret: anthropicSecret, key: AnthropicKeyEnv}},
		{"k8s://ctx/ns/creds/config.yaml#auth.token", "", secretRef{kubeContext: "ctx", ns: "ns", secret: "creds", key: "config.yaml", path: "auth.token"}},
	} {
		if got := secretReader(tc.source); got != tc.reader {
			t.Errorf("%s: reader %q, want %q", tc.source, got, tc.reader)
		}
		got, err := parseSecretSource(tc.source)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.source, got, err, tc.want)
		}
	}
	if _, err := parseSecretSource("sk-ant-pasted-value"); err == nil {
		t.Error("a value pasted as a source must be refused")
	}
}

// TestValueAtPath: mappings by key (a key holding dots unquoted), lists by
// index, scalars as text; a path that reaches nothing or stops at a mapping
// fails naming the path, never a value.
func TestValueAtPath(t *testing.T) {
	doc := []byte(`{"stringData":{"private-key":"PEM","credentials.json":{"token":"t0k"},"port":8443,"on":false,"list":["zero","one"]},"top":"` + testSourceValue + `"}`)
	for path, want := range map[string]string{
		"stringData.private-key":            "PEM",
		"stringData.credentials.json.token": "t0k",
		"stringData.port":                   "8443",
		"stringData.on":                     "false",
		"stringData.list.1":                 "one",
		"top":                               testSourceValue,
	} {
		if got, err := valueAtPath(doc, path); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", path, got, err, want)
		}
	}
	for path, want := range map[string]string{
		"stringData.missing":       `no key "missing" under stringData`,
		"stringData":               "names a mapping",
		"top.deeper":               "top holds one value",
		"stringData.list.7":        "is no index of the list under stringData.list (2 items)",
		"stringData.private-key.x": "holds one value",
	} {
		_, err := valueAtPath(doc, path)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
		if err != nil && strings.Contains(err.Error(), testSourceValue) {
			t.Errorf("%s: the error carries a value: %v", path, err)
		}
	}
}

// TestReadOpSource: an op:// source is `op read --no-newline <ref>`, its
// stdout the value as it is; a failing op is an error carrying what op said
// on stderr and never what it wrote on stdout.
func TestReadOpSource(t *testing.T) {
	const ref = "op://Employee/agentlab.anthropic-key/credential"
	args := fakeReader(t, opBin, "printf '%s' '"+testSourceValue+"'")
	got, err := readSecretSource(t.Context(), ref)
	if err != nil || string(got) != testSourceValue {
		t.Fatalf("%q, %v; want the value", got, err)
	}
	if want := []string{"read", "--no-newline", ref}; !reflect.DeepEqual(readArgs(t, args), want) {
		t.Errorf("op called with %q, want %q", readArgs(t, args), want)
	}

	fakeReader(t, opBin, "printf '%s' '"+testSourceValue+"'; echo '[ERROR] account is not signed in' >&2; exit 1")
	_, err = readSecretSource(t.Context(), ref)
	if err == nil || !strings.Contains(err.Error(), "account is not signed in") || !strings.Contains(err.Error(), ref) {
		t.Errorf("err %v, want op's stderr and the reference", err)
	}
	if err != nil && strings.Contains(err.Error(), testSourceValue) {
		t.Errorf("the error carries op's stdout: %v", err)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := readSecretSource(t.Context(), ref); err == nil || !strings.Contains(err.Error(), "op is not on PATH") || !strings.Contains(err.Error(), secretReaderInstall[opBin]) {
		t.Errorf("err %v, want op missing with where it comes from", err)
	}
}

// TestReadSOPSSource: a SOPS source decrypts the file to JSON through sops
// and walks the path in this process.
func TestReadSOPSSource(t *testing.T) {
	const file = "/lab/agents.sops.yaml"
	args := fakeReader(t, sopsBin, `printf '%s' '{"stringData":{"private-key":"`+testSourceValue+`","client-secret":"other"}}'`)
	got, err := readSecretSource(t.Context(), file+"#stringData.private-key")
	if err != nil || string(got) != testSourceValue {
		t.Fatalf("%q, %v; want the value", got, err)
	}
	if want := []string{"--decrypt", "--output-type", "json", file}; !reflect.DeepEqual(readArgs(t, args), want) {
		t.Errorf("sops called with %q, want %q", readArgs(t, args), want)
	}
	_, err = readSecretSource(t.Context(), "sops://"+file+"#stringData.missing")
	if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), `no key "missing"`) || strings.Contains(err.Error(), testSourceValue) {
		t.Errorf("err %v, want the file and the missing key, no value", err)
	}
}

// TestReadKubeSource: a k8s:// source is the key of a Secret the named
// context reaches, or one value of the document the key holds.
func TestReadKubeSource(t *testing.T) {
	var asked []string
	prev := kubeSourceSecretData
	kubeSourceSecretData = func(_ context.Context, kubeContext, ns, name string) (map[string][]byte, error) {
		asked = append(asked, kubeContext+"/"+ns+"/"+name)
		if name != "agentlab-anthropic" {
			return nil, errors.New(`secrets "` + name + `" not found`)
		}
		return map[string][]byte{AnthropicKeyEnv: []byte(testSourceValue), "config.yaml": []byte("auth:\n  token: t0k\n")}, nil
	}
	t.Cleanup(func() { kubeSourceSecretData = prev })

	for source, want := range map[string]string{
		"k8s://kind-agentlab/agent-platform/agentlab-anthropic/" + AnthropicKeyEnv:     testSourceValue,
		"k8s://kind-agentlab/agent-platform/agentlab-anthropic/config.yaml#auth.token": "t0k",
	} {
		if got, err := readSecretSource(t.Context(), source); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v; want %q", source, got, err, want)
		}
	}
	if want := "kind-agentlab/agent-platform/agentlab-anthropic"; len(asked) == 0 || asked[0] != want {
		t.Errorf("asked %q, want %s", asked, want)
	}
	if _, err := readSecretSource(t.Context(), "k8s://kind-agentlab/agent-platform/agentlab-anthropic/OTHER"); err == nil || !strings.Contains(err.Error(), "has no key OTHER") {
		t.Errorf("a missing key: %v", err)
	}
	if _, err := readSecretSource(t.Context(), "k8s://kind-agentlab/agent-platform/agentlab-anthropic/"+AnthropicKeyEnv+"#a.b"); err == nil || strings.Contains(err.Error(), testSourceValue) {
		t.Errorf("a path into a plain value fails without quoting it: %v", err)
	}
}

// TestWriteSecretKey: a placed key lands next to the keys another writer
// applied, and in a Secret of its own when there is none.
func TestWriteSecretKey(t *testing.T) {
	newFakeLab(t)
	ctx := t.Context()
	if err := ensureSecret(platformNamespace, gitHubAppSecret, corev1.SecretTypeOpaque, map[string][]byte{gitHubAppSecretAppID: []byte("123")}); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretKey(ctx, platformNamespace, gitHubAppSecret, gitHubAppSecretPrivateKey, []byte(testSourceValue)); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{gitHubAppSecretAppID: "123", gitHubAppSecretPrivateKey: testSourceValue} {
		if got, err := secretDataKey(ctx, platformNamespace, gitHubAppSecret, key); err != nil || string(got) != want {
			t.Errorf("key %s = %q (%v), want %q", key, got, err, want)
		}
	}
	if err := writeSecretKey(ctx, platformNamespace, "fresh", "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if got, err := secretDataKey(ctx, platformNamespace, "fresh", "k"); err != nil || string(got) != "v" {
		t.Errorf("a fresh Secret's key = %q (%v)", got, err)
	}
}

// TestPlaceSecretRefusesBeforeTheCluster: a malformed target or a value
// pasted as the reference is refused before anything is read or written.
func TestPlaceSecretRefusesBeforeTheCluster(t *testing.T) {
	reads := fakeSecretSources(t, map[string]string{testKeySource: testSourceValue})
	cfg := config.Default()
	secret := kagentNamespace + "/" + anthropicSecret
	for _, tc := range []struct{ source, target, want string }{
		{testKeySource, secret, "<namespace>/<secret>/<key>"},
		{testKeySource, kagentNamespace + "//" + AnthropicKeyEnv, "<namespace>/<secret>/<key>"},
		{"sk-ant-pasted", secret + "/" + AnthropicKeyEnv, "a reference, never a value"},
	} {
		if err := PlaceSecret(cfg, tc.source, tc.target); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: %v, want %q", tc.source, tc.target, err, tc.want)
		}
	}
	if len(*reads) != 0 {
		t.Errorf("read %v before refusing", *reads)
	}
}

// TestRecordedSecretSources: discover lists every source up and platform
// place, with its Secret, and none of a feature that is off.
func TestRecordedSecretSources(t *testing.T) {
	cfg := gitHubTokenTestConfig()
	cfg.AIKey.Source = testKeySource
	const keySource, clientSecretSource = "op://lab/app/private-key", "op://lab/app/client-secret" // #nosec G101 -- references, not credentials
	cfg.GitHubApp = config.GitHubApp{AppID: "1", InstallationID: "2", PrivateKeySource: keySource, ClientID: "Iv1", ClientSecretSource: clientSecretSource}
	got := recordedSecretSources(cfg)
	want := []SecretSource{
		{"aiKey.source", testKeySource, kagentNamespace + "/" + anthropicSecret},
		{"githubApp.privateKeySource", keySource, platformNamespace + "/" + gitHubAppSecret},
		{"githubApp.clientSecretSource", clientSecretSource, platformNamespace + "/" + portalGitHubAppSecret},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sources %+v, want %+v", got, want)
	}
	if got := recordedSecretSources(config.Default()); len(got) != 0 {
		t.Errorf("the default lab records %+v, want none", got)
	}
}
