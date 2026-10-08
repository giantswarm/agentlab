package lab

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// AnthropicKeyEnv is where the lab reads the Anthropic API key from at deploy
// time when agentlab.yaml records no source. Unlike the lab's throwaway
// passwords, this is a real credential: it never enters agentlab.yaml or the
// rendered state/ files — it travels host environment -> Kubernetes Secret
// only, and the consumers (the kagent ModelConfig, Backstage's ai-chat)
// reference the Secret.
const AnthropicKeyEnv = "ANTHROPIC_API_KEY"

// anthropicSecret is the Secret the chart-rendered default ModelConfig
// references (agent-platform-values.yaml.tmpl, providers.anthropic), under
// the key the ADK runtime reads it from.
const (
	anthropicSecret    = "kagent-anthropic" // #nosec G101 -- Secret NAME, not a credential
	anthropicSecretKey = AnthropicKeyEnv
)

// secretTool is the operator's secret tooling a recorded source is handed to:
// beekeeper (github.com/giantswarm/beekeeper), whose `secret copy <ref>
// --to-secret kind-<cluster>/<ns>/<name>/<key>` reads the value in its own
// process and writes it into the lab's Secret through the kind admin
// kubeconfig it holds — the one key, the Secret created when absent. It
// answers the value's length, never the value; agentlab prints that answer.
// The second subprocess of the lab next to the container engine (exec.go),
// run only while agentlab.yaml records a source.
const secretTool = "beekeeper"

// runSecretTool runs the tooling and answers its stdout; the seam the tests
// replace with a fake that records the call.
var runSecretTool = func(args ...string) (string, error) { return outputQuiet(secretTool, args...) }

// secretCopyArgs is the placement of one value: `secret copy <source>
// --to-secret <target>`.
func secretCopyArgs(source, target string) []string {
	return []string{"secret", "copy", source, "--to-secret", target}
}

// secretTarget is the --to-secret form of a key: the kind context, the
// namespace, the Secret and the key.
func secretTarget(cfg *config.Config, ns, name, key string) string {
	return fmt.Sprintf("kind-%s/%s/%s/%s", cfg.ClusterName, ns, name, key)
}

// ensureAnthropicSecret makes sure kagent/kagent-anthropic carries the key
// the default ModelConfig references, so the ModelConfig resolves on every
// lab — a fresh one included — and reports whether it wrote it (so callers
// can roll an already-running consumer exactly once). In order:
//
//   - a source in agentlab.yaml (aiKey.source): the Secret is placed from it
//     through the secret tooling, and its failure fails the run — a lab whose
//     configuration says where the key is never runs on a placeholder. A
//     Secret that already holds a real key is left alone (delete it and
//     re-run to rotate, or place the value directly); the placeholder and a
//     missing key are replaced.
//   - $ANTHROPIC_API_KEY in the host environment: the Secret is created from
//     it, in-process, so the key never appears in a process's argv or in a
//     file; a real key already there is left alone, the placeholder replaced.
//   - neither: the placeholder (placeholderAPIKey, as for an extra model
//     without a key), so the ModelConfig resolves and agent pods boot; every
//     turn on it fails at Anthropic until the key is placed, which the warning
//     says how to do.
func ensureAnthropicSecret(cfg *config.Config) (wrote bool, err error) {
	ctx := context.Background()
	state, err := anthropicSecretState(ctx)
	if err != nil {
		return false, err
	}
	target := secretTarget(cfg, kagentNamespace, anthropicSecret, anthropicSecretKey)
	if source := cfg.AIKey.Source; source != "" {
		if state == anthropicKeyReal {
			note("secret %s/%s already holds a key, leaving it alone (delete it and re-run to place %s again)", kagentNamespace, anthropicSecret, source)
			return false, nil
		}
		args := secretCopyArgs(source, target)
		answer, err := runSecretTool(args...)
		if err != nil {
			return false, fmt.Errorf("secret %s/%s key %s from aiKey.source %s: %w\n  the default ModelConfig %s cannot resolve without it; when %s answers, re-run `agentlab platform`",
				kagentNamespace, anthropicSecret, anthropicSecretKey, source, err, defaultModelConfig, secretTool)
		}
		if state, err = anthropicSecretState(ctx); err != nil {
			return false, err
		}
		if state != anthropicKeyReal {
			return false, fmt.Errorf("secret %s/%s key %s: %s answered %q, but the Secret still lacks a key from aiKey.source %s",
				kagentNamespace, anthropicSecret, anthropicSecretKey, secretTool, strings.TrimSpace(answer), source)
		}
		note("secret %s/%s key %s placed from %s by %s (%s); agentlab never read the value", kagentNamespace, anthropicSecret, anthropicSecretKey, source, secretTool, strings.TrimSpace(answer))
		return true, nil
	}
	if key := os.Getenv(AnthropicKeyEnv); key != "" {
		if state == anthropicKeyReal {
			note("secret %s/%s already holds a key, leaving it alone (delete it and re-run to rotate)", kagentNamespace, anthropicSecret)
			return false, nil
		}
		// Applied in-process, so the key never appears in a process's argv or
		// in a file; server-side apply replaces the placeholder.
		if err := ensureSecret(kagentNamespace, anthropicSecret, corev1.SecretTypeOpaque, map[string][]byte{anthropicSecretKey: []byte(key)}); err != nil {
			return false, err
		}
		note("created secret %s/%s from $%s", kagentNamespace, anthropicSecret, AnthropicKeyEnv)
		return true, nil
	}
	switch state {
	case anthropicKeyReal:
		note("secret %s/%s already holds a key, leaving it alone (delete it and re-run to rotate)", kagentNamespace, anthropicSecret)
		return false, nil
	case anthropicKeyPlaceholder:
		warnAnthropicPlaceholder(cfg, target, "still holds")
		return false, nil
	}
	if err := ensureSecret(kagentNamespace, anthropicSecret, corev1.SecretTypeOpaque, map[string][]byte{anthropicSecretKey: []byte(placeholderAPIKey)}); err != nil {
		return false, err
	}
	warnAnthropicPlaceholder(cfg, target, "holds")
	return true, nil
}

// warnAnthropicPlaceholder is the loud part of the no-key path: what the
// placeholder means and the three ways to the real key.
func warnAnthropicPlaceholder(cfg *config.Config, target, verb string) {
	warn("secret %s/%s %s a placeholder key: ModelConfig %s resolves and agents start, but every turn on it fails at Anthropic until the key is placed:", kagentNamespace, anthropicSecret, verb, defaultModelConfig)
	warn("  %s secret copy op://<vault>/<item>/<field> --to-secret %s   (agentlab never reads the value)", secretTool, target)
	warn("  or record the source so every `agentlab up` restores it: agentlab configure --ai-key-source op://<vault>/<item>/<field>, then `agentlab platform`")
	warn("  or export %s=... and re-run `agentlab platform`", AnthropicKeyEnv)
	if cfg.Backstage.Enabled {
		warn("  Backstage's AI chat answers nothing on the placeholder either")
	}
}

// anthropicKeyState is what kagent/kagent-anthropic carries under the key.
type anthropicKeyState int

const (
	// anthropicKeyMissing: no Secret, or the key absent or empty.
	anthropicKeyMissing anthropicKeyState = iota
	// anthropicKeyPlaceholder: the lab's own placeholder (placeholderAPIKey).
	anthropicKeyPlaceholder
	// anthropicKeyReal: a value that is not the placeholder. Nothing of it is
	// kept, compared beyond that or printed.
	anthropicKeyReal
)

// anthropicSecretState reads the Secret and classifies its key. The one
// comparison is against the lab's public placeholder constant; the value
// stays in the apiserver's answer.
func anthropicSecretState(ctx context.Context) (anthropicKeyState, error) {
	obj, err := getObject(ctx, gvrSecrets, kagentNamespace, anthropicSecret)
	if apierrors.IsNotFound(err) {
		return anthropicKeyMissing, nil
	}
	if err != nil {
		return anthropicKeyMissing, err
	}
	return classifyAnthropicKey(obj), nil
}

func classifyAnthropicKey(secret *unstructured.Unstructured) anthropicKeyState {
	value, err := secretDataValue(secret, anthropicSecretKey)
	if err != nil || len(value) == 0 {
		return anthropicKeyMissing
	}
	if bytes.Equal(value, []byte(placeholderAPIKey)) {
		return anthropicKeyPlaceholder
	}
	return anthropicKeyReal
}

// waitDefaultModelConfigResolved is the proof the Secret was for: the
// chart's default ModelConfig Accepted and, on a line that reports it, its
// Secret reference resolved — the condition an agent's Harness refuses a
// template on otherwise.
func waitDefaultModelConfigResolved() error {
	return waitModelConfigAccepted(defaultModelConfig)
}
