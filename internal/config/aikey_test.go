package config

import (
	"strings"
	"testing"
)

// TestAIKeySourceIsAReference: aiKey.source takes an op:// field or a SOPS
// path and refuses a value — a key pasted into agentlab.yaml must never pass
// configure; empty is the no-source default and the file carries no aiKey
// then.
func TestAIKeySourceIsAReference(t *testing.T) {
	cfg := Default()
	if cfg.AIKey != (AIKey{}) {
		t.Fatalf("no source by default: %+v", cfg.AIKey)
	}
	for _, ok := range []string{"", "op://Employee/agentlab.anthropic-key/credential", "lab.sops.yaml#stringData.ANTHROPIC_API_KEY", "sops://secrets/lab.sops.yaml#data.key", "/abs/path/to/file.sops.yaml#a.b.c"} {
		cfg.AIKey.Source = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"sk-ant-api03-abcdef", "op://vault/item", "op://vault//field", "op://vault/item/field/extra", "file.sops.yaml", "#path", "file#", "op://vault/it em/field", "a value with spaces"} {
		cfg.AIKey.Source = bad
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "aiKey.source") {
			t.Errorf("%q must be refused naming aiKey.source, got %v", bad, err)
		}
	}
}
