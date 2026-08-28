package lab

import (
	"fmt"
	"os"
)

// AnthropicKeyEnv is where the lab reads the Anthropic API key from at deploy
// time. Unlike the lab's throwaway passwords, this is a real credential: it
// never enters agentlab.yaml or the rendered state/ files — it travels host
// environment -> Kubernetes Secret only, and the consumers (the kagent
// ModelConfig, Backstage's ai-chat) reference the Secret.
const AnthropicKeyEnv = "ANTHROPIC_API_KEY"

// ensureAnthropicSecret creates ns/name holding the Anthropic API key from
// the host environment and reports whether it created it (so callers can roll
// an already-running consumer exactly once). An existing secret is left alone
// — delete it and re-run to rotate. A missing env var is a note, not an
// error: everything boots without the key, the AI features just stay
// unconfigured until the secret exists.
func ensureAnthropicSecret(ns, name string) (created bool, err error) {
	if _, err := outputQuiet("kubectl", "-n", ns, "get", "secret", name); err == nil {
		note("secret %s/%s already exists, leaving it alone (delete it and re-run to rotate)", ns, name)
		return false, nil
	}
	key := os.Getenv(AnthropicKeyEnv)
	if key == "" {
		note("$%s is not set -- skipping secret %s/%s; AI features stay off until it exists:", AnthropicKeyEnv, ns, name)
		note("  export %s=... && re-run, or:", AnthropicKeyEnv)
		note("  kubectl -n %s create secret generic %s --from-literal=%s=...", ns, name, AnthropicKeyEnv)
		return false, nil
	}
	// Applied via stdin so the key never appears in a process's argv.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  %s: %q
`, name, ns, AnthropicKeyEnv, key)
	if err := pipeInto([]byte(manifest), "kubectl", "apply", "-f", "-"); err != nil {
		return false, err
	}
	note("created secret %s/%s from $%s", ns, name, AnthropicKeyEnv)
	return true, nil
}
