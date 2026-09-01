package forms

import (
	"io"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

const keyDelay = 30 * time.Millisecond

// TestRunTUIDrive drives the real TUI form (not accessible mode) with a
// scripted keystroke stream: accept the cluster defaults, keep the default
// users, keep the preselected components (platform AND Backstage — the
// canonical lab), and accept their defaults.
func TestRunTUIDrive(t *testing.T) {
	testInput = newPacedReader(keyDelay,
		"\r", "\r", "\r", // group 1: cluster name, dex port, dex image
		"\r", // group 2: customize users? -> keep as is
		"\r", // group 3: platform + backstage preselected; submit as is
		// platform group: muster port, aps ref, agents confirm, agents ui
		// port, observability confirm, claude model, extra models confirm
		"\r", "\r", "\r", "\r", "\r", "\r", "\r",
		"\r", // backstage group: port
	)
	testOutput = io.Discard
	defer func() { testInput, testOutput = nil, nil }()

	cfg := config.Default()
	if err := Run(cfg, false); err != nil {
		t.Fatalf("form run: %v", err)
	}
	if cfg.ClusterName != "agentlab" || cfg.DexPort != 32000 {
		t.Errorf("defaults not kept: cluster=%q dexPort=%d", cfg.ClusterName, cfg.DexPort)
	}
	if !cfg.Platform.Enabled {
		t.Errorf("platform not enabled (should be preselected from the default config)")
	}
	if !cfg.Platform.Agents {
		t.Errorf("agents not enabled (the confirm should keep the default)")
	}
	if !cfg.Platform.Observability {
		t.Errorf("observability not enabled (the confirm should keep the default)")
	}
	if !cfg.Backstage.Enabled {
		t.Errorf("backstage not enabled by multiselect")
	}
	if len(cfg.Users) != 3 {
		t.Errorf("users changed: %d", len(cfg.Users))
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("resulting config invalid: %v", err)
	}
}

// TestRunTUIDriveEdit changes the cluster name and dex port through the form:
// ctrl+u clears an input's pre-filled value before typing a replacement.
func TestRunTUIDriveEdit(t *testing.T) {
	testInput = newPacedReader(keyDelay,
		"\x15", "renamed", "\r", // clear + retype cluster name
		"\x15", "31000", "\r", // clear + retype dex port
		"\r", // dex image: keep
		"\r", // users: keep as is
		// components: deselect the preselected platform, arrow down,
		// deselect the preselected backstage -> none
		" ", "\x1b[B", " ", "\r",
	)
	testOutput = io.Discard
	defer func() { testInput, testOutput = nil, nil }()

	cfg := config.Default()
	if err := Run(cfg, false); err != nil {
		t.Fatalf("form run: %v", err)
	}
	if cfg.ClusterName != "renamed" {
		t.Errorf("cluster name not edited: %q", cfg.ClusterName)
	}
	if cfg.DexPort != 31000 {
		t.Errorf("dex port not edited: %d", cfg.DexPort)
	}
	if cfg.Platform.Enabled || cfg.Backstage.Enabled {
		t.Errorf("components unexpectedly enabled")
	}
}

// The user-editing loop runs one form per user; driving several sequential
// bubbletea programs from one scripted reader loses keystrokes between
// programs (each program's final blocked read eats one), so that flow is
// covered interactively. Its pure logic is testable directly:
func TestOrderGroups(t *testing.T) {
	got := orderGroups([]string{"viewers", "platform-admins"})
	if len(got) != 2 || got[0] != "platform-admins" || got[1] != "viewers" {
		t.Errorf("orderGroups = %v, want canonical order", got)
	}
}
