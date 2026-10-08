package lab

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// The keys the tests seed and expect: a real one, the vault's, the
// variable's — test values, no credential.
const (
	testRealKey  = "sk-ant-real"  // #nosec G101 -- a test value, not a credential
	testVaultKey = "sk-ant-vault" // #nosec G101 -- a test value, not a credential
	testEnvKey   = "sk-ant-env"   // #nosec G101 -- a test value, not a credential
	// testKeySource is the reference the tests record as aiKey.source.
	testKeySource = "op://lab/anthropic/credential"
	// testToolVersion is what the fake tooling reports.
	testToolVersion = "v0.108.3"
)

// anthropicSecretWith is a kagent/kagent-anthropic seed carrying value under
// the key.
func anthropicSecretWith(value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: anthropicSecret, Namespace: kagentNamespace},
		Data: map[string][]byte{anthropicSecretKey: []byte(value)}}
}

// storedAnthropicKey is the key the fake apiserver holds, decoded.
func storedAnthropicKey(t *testing.T, f *fakeLab) string {
	t.Helper()
	encoded, _, _ := unstructured.NestedString(f.stored(t, gvrSecrets, kagentNamespace, anthropicSecret).Object, "data", anthropicSecretKey)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// fakeSecretTool replaces the secret tooling for a test: it records every
// call and runs place against the fake lab, the way beekeeper writes the
// value into the apiserver in its own process.
func fakeSecretTool(t *testing.T, place func() error) *[][]string {
	t.Helper()
	var calls [][]string
	prev := runSecretTool
	runSecretTool = func(args ...string) (string, error) {
		calls = append(calls, args)
		if place == nil {
			return "", errors.New("exit status 78: the shared vault answered nothing within a minute")
		}
		return "42 bytes\n", place()
	}
	t.Cleanup(func() { runSecretTool = prev })
	return &calls
}

// TestEnsureAnthropicSecretPlaceholder: with neither a source nor the
// variable the Secret is created with the placeholder, so the default
// ModelConfig resolves; a re-run leaves it; a real key is left alone too.
func TestEnsureAnthropicSecretPlaceholder(t *testing.T) {
	t.Setenv(AnthropicKeyEnv, "")
	calls := fakeSecretTool(t, nil)
	f := newFakeLab(t)
	cfg := config.Default()
	wrote, err := ensureAnthropicSecret(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || storedAnthropicKey(t, f) != placeholderAPIKey {
		t.Errorf("a fresh lab without a key must get the placeholder: wrote %v, key %q", wrote, storedAnthropicKey(t, f))
	}
	wrote, err = ensureAnthropicSecret(cfg)
	if err != nil || wrote {
		t.Errorf("a re-run must leave the placeholder as it is: wrote %v, %v", wrote, err)
	}
	if len(*calls) != 0 {
		t.Errorf("no source, no tooling call: %v", *calls)
	}

	real := newFakeLab(t, anthropicSecretWith(testRealKey))
	wrote, err = ensureAnthropicSecret(cfg)
	if err != nil || wrote {
		t.Errorf("a real key is left alone: wrote %v, %v", wrote, err)
	}
	if storedAnthropicKey(t, real) != testRealKey {
		t.Errorf("the real key was rewritten")
	}
}

// TestEnsureAnthropicSecretFromEnv: the variable creates the Secret, replaces
// the placeholder, and never overwrites a real key.
func TestEnsureAnthropicSecretFromEnv(t *testing.T) {
	t.Setenv(AnthropicKeyEnv, testEnvKey)
	fakeSecretTool(t, nil)
	cfg := config.Default()
	for _, tc := range []struct {
		name      string
		seed      *corev1.Secret
		wantWrote bool
		wantKey   string
	}{
		{name: "no Secret", wantWrote: true, wantKey: testEnvKey},
		{name: "over the placeholder", seed: anthropicSecretWith(placeholderAPIKey), wantWrote: true, wantKey: testEnvKey},
		{name: "a real key stays", seed: anthropicSecretWith(testRealKey), wantKey: testRealKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fakeLab
			if tc.seed != nil {
				f = newFakeLab(t, tc.seed)
			} else {
				f = newFakeLab(t)
			}
			wrote, err := ensureAnthropicSecret(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if wrote != tc.wantWrote || storedAnthropicKey(t, f) != tc.wantKey {
				t.Errorf("wrote %v, key %q; want wrote %v, key %q", wrote, storedAnthropicKey(t, f), tc.wantWrote, tc.wantKey)
			}
		})
	}
}

// TestEnsureAnthropicSecretFromSource: a recorded source is handed to the
// secret tooling as `secret copy <source> --to-secret
// kind-<cluster>/kagent/kagent-anthropic/ANTHROPIC_API_KEY` — on a fresh lab
// (the restore after down and up) and over the placeholder — and agentlab
// writes nothing itself; a real key already there is left alone without a
// call; the variable is ignored while a source is recorded.
func TestEnsureAnthropicSecretFromSource(t *testing.T) {
	t.Setenv(AnthropicKeyEnv, testEnvKey)
	const source = testKeySource
	cfg := config.Default()
	cfg.ClusterName = "agentlab-2"
	cfg.AIKey.Source = source
	wantArgs := []string{"secret", "copy", source, "--to-secret", "kind-agentlab-2/kagent/kagent-anthropic/ANTHROPIC_API_KEY"}

	for _, tc := range []struct {
		name string
		seed *corev1.Secret
	}{
		{name: "fresh lab"},
		{name: "over the placeholder", seed: anthropicSecretWith(placeholderAPIKey)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fakeLab
			if tc.seed != nil {
				f = newFakeLab(t, tc.seed)
			} else {
				f = newFakeLab(t)
			}
			calls := fakeSecretTool(t, func() error {
				return ensureSecret(kagentNamespace, anthropicSecret, corev1.SecretTypeOpaque, map[string][]byte{anthropicSecretKey: []byte(testVaultKey)})
			})
			wrote, err := ensureAnthropicSecret(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !wrote {
				t.Error("the placement must count as a write")
			}
			if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], wantArgs) {
				t.Errorf("tooling calls %v, want one: %v", *calls, wantArgs)
			}
			if got := storedAnthropicKey(t, f); got != testVaultKey {
				t.Errorf("key %q, want the one the tooling placed (never the variable's)", got)
			}
		})
	}

	t.Run("real key stays", func(t *testing.T) {
		f := newFakeLab(t, anthropicSecretWith(testRealKey))
		calls := fakeSecretTool(t, nil)
		wrote, err := ensureAnthropicSecret(cfg)
		if err != nil || wrote {
			t.Errorf("wrote %v, %v", wrote, err)
		}
		if len(*calls) != 0 || storedAnthropicKey(t, f) != testRealKey {
			t.Errorf("a real key is left alone, the tooling not called: %v", *calls)
		}
	})
}

// TestEnsureAnthropicSecretSourceFailureIsNoPlaceholder: a source the tooling
// cannot place fails the run, naming the Secret and the source, and nothing is
// written — a lab whose configuration says where the key is never runs on a
// placeholder; the same when the tooling answers but the key is not there.
func TestEnsureAnthropicSecretSourceFailureIsNoPlaceholder(t *testing.T) {
	t.Setenv(AnthropicKeyEnv, "")
	cfg := config.Default()
	cfg.AIKey.Source = testKeySource

	f := newFakeLab(t)
	fakeSecretTool(t, nil)
	_, err := ensureAnthropicSecret(cfg)
	for _, want := range []string{"secret kagent/kagent-anthropic key ANTHROPIC_API_KEY", "aiKey.source op://lab/anthropic/credential", "exit status 78", "re-run `agentlab platform`"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want the failure naming %q, got %v", want, err)
		}
	}
	if _, err := f.dyn.Tracker().Get(gvrSecrets, kagentNamespace, anthropicSecret); !apierrors.IsNotFound(err) {
		t.Errorf("no placeholder behind a failed source: %v", err)
	}

	newFakeLab(t)
	fakeSecretTool(t, func() error { return nil })
	_, err = ensureAnthropicSecret(cfg)
	if err == nil || !strings.Contains(err.Error(), "still lacks a key") {
		t.Errorf("an answer without the key is a failure: %v", err)
	}
}

// TestClassifyAnthropicKey: no key, an empty one, the placeholder, a value.
func TestClassifyAnthropicKey(t *testing.T) {
	seed := func(data map[string][]byte) *unstructured.Unstructured {
		raw := map[string]any{}
		for k, v := range data {
			raw[k] = base64.StdEncoding.EncodeToString(v)
		}
		u := &unstructured.Unstructured{Object: map[string]any{fieldMetadata: map[string]any{nameKey: anthropicSecret, fieldNamespace: kagentNamespace}}}
		if data != nil {
			u.Object["data"] = raw
		}
		return u
	}
	for _, tc := range []struct {
		name string
		data map[string][]byte
		want anthropicKeyState
	}{
		{name: "no data", want: anthropicKeyMissing},
		{name: "other key", data: map[string][]byte{"OPENAI_API_KEY": []byte("x")}, want: anthropicKeyMissing},
		{name: "the key empty", data: map[string][]byte{anthropicSecretKey: nil}, want: anthropicKeyMissing},
		{name: "the placeholder", data: map[string][]byte{anthropicSecretKey: []byte(placeholderAPIKey)}, want: anthropicKeyPlaceholder},
		{name: "a value", data: map[string][]byte{anthropicSecretKey: []byte(testRealKey)}, want: anthropicKeyReal},
	} {
		if got := classifyAnthropicKey(seed(tc.data)); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPreflightAsksForTheSecretToolOnlyWithASource: a recorded source adds
// the tooling to the verdict when it is missing, naming the source, the
// Secret and the way out; without a source it is never asked for.
func TestPreflightAsksForTheSecretToolOnlyWithASource(t *testing.T) {
	d := &Discovery{Tools: toolVersions("29.7.2"), KeySource: testKeySource}
	err := d.Preflight()
	for _, want := range []string{"beekeeper is not on PATH", testKeySource, "kagent/kagent-anthropic", "giantswarm/beekeeper", `--ai-key-source ""`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want the refusal naming %q, got %v", want, err)
		}
	}
	d.SecretTool = testToolVersion
	if err := d.Preflight(); err != nil {
		t.Errorf("with the tooling answering: %v", err)
	}
	if err := (&Discovery{Tools: toolVersions("29.7.2")}).Preflight(); err != nil {
		t.Errorf("without a source the tooling is not asked for: %v", err)
	}
}

// TestReportAnthropicKeyLine: the report says where the key comes from —
// the source with the tooling's version, the variable, or the placeholder
// with the command that places the key.
func TestReportAnthropicKeyLine(t *testing.T) {
	cfg := config.Default()
	keyLine := func(d *Discovery) string {
		for _, l := range strings.Split(d.Report(cfg), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "Anthropic key") {
				return l
			}
		}
		return ""
	}
	tools := toolVersions("29.7.2")
	if line := keyLine(&Discovery{Tools: tools, KeySource: testKeySource, SecretTool: testToolVersion}); !strings.Contains(line, "aiKey.source "+testKeySource+" — beekeeper "+testToolVersion+" places it into the Secret kagent/kagent-anthropic") {
		t.Errorf("source line %q", line)
	}
	if line := keyLine(&Discovery{Tools: tools, KeySource: testKeySource}); !strings.Contains(line, "beekeeper is not on PATH") {
		t.Errorf("source without the tooling %q", line)
	}
	if line := keyLine(&Discovery{Tools: tools, AnthropicKey: true}); !strings.Contains(line, "$ANTHROPIC_API_KEY is set") {
		t.Errorf("variable line %q", line)
	}
	line := keyLine(&Discovery{Tools: tools})
	for _, want := range []string{"placeholder", "beekeeper secret copy <ref> --to-secret kind-agentlab/kagent/kagent-anthropic/ANTHROPIC_API_KEY", "--ai-key-source"} {
		if !strings.Contains(line, want) {
			t.Errorf("placeholder line %q lacks %q", line, want)
		}
	}
}

// TestWaitDefaultModelConfigResolved: the wait outlives the controller's
// resync — a ResolvedRefs that turns True on a later look passes, one that
// stays False fails with its message after the looks, a status without the
// condition (the 0.x line) passes at once.
func TestWaitDefaultModelConfigResolved(t *testing.T) {
	prevAccepted, prevResolved, prevLooks := modelConfigAcceptedPoll, modelConfigResolvedPoll, modelConfigResolvedLooks
	modelConfigAcceptedPoll, modelConfigResolvedPoll, modelConfigResolvedLooks = time.Millisecond, time.Millisecond, 5
	t.Cleanup(func() {
		modelConfigAcceptedPoll, modelConfigResolvedPoll, modelConfigResolvedLooks = prevAccepted, prevResolved, prevLooks
	})
	accepted := func() *unstructured.Unstructured {
		return withCondition(customObject(gvkModelConfig, kagentNamespace, defaultModelConfig, nil), "Accepted", conditionTrue, "")
	}
	resolvedRefs := func(mc *unstructured.Unstructured, status, message string) *unstructured.Unstructured {
		conds, _, _ := unstructured.NestedSlice(mc.Object, fieldStatus, "conditions")
		_ = unstructured.SetNestedSlice(mc.Object, append(conds, map[string]any{fieldType: conditionResolvedRefs, fieldStatus: status, fieldMessage: message}), fieldStatus, "conditions")
		return mc
	}

	newFakeLab(t, accepted())
	if err := waitDefaultModelConfigResolved(); err != nil {
		t.Errorf("no ResolvedRefs condition (the 0.x line): %v", err)
	}

	f := newFakeLab(t, resolvedRefs(accepted(), condFalseStatus, "secret kagent-anthropic not found"))
	err := waitDefaultModelConfigResolved()
	if err == nil || !strings.Contains(err.Error(), "ResolvedRefs=False after 5ms: secret kagent-anthropic not found") {
		t.Errorf("a False that stays: %v", err)
	}

	// Stale right after the Secret landed, True on a later look — the
	// controller's resync, played by a reactor on the third read.
	looks := 0
	f.dyn.PrependReactor("get", "modelconfigs", func(clienttesting.Action) (bool, runtime.Object, error) {
		if looks++; looks == 3 {
			_ = f.dyn.Tracker().Update(gvrModelConfigs, resolvedRefs(accepted(), conditionTrue, "All referenced secrets resolved"), kagentNamespace)
		}
		return false, nil, nil
	})
	if err := waitDefaultModelConfigResolved(); err != nil {
		t.Errorf("a False that turns True within the looks: %v", err)
	}
}
