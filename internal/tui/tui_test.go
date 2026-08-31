package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/giantswarm/agentplatform-kind/internal/config"
	"github.com/giantswarm/agentplatform-kind/internal/lab"
)

func key(s string) tea.KeyMsg {
	if s == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func drive(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	mm, _ := m.Update(msg)
	return mm.(model)
}

// TestDashboardFlow walks the model through the main interactions: status
// rendering, the users toggle, the down confirmation, and the single-action
// guard, with the subprocess runner stubbed out.
func TestDashboardFlow(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Enabled = true
	// Off on purpose (the default is on): the "disabled" status-row rendering
	// is part of what this walk asserts.
	cfg.Backstage.Enabled = false

	var launched [][]string
	m := newModel(cfg)
	m.runner = func(label string, args ...string) tea.Cmd {
		launched = append(launched, args)
		return func() tea.Msg {
			return actionStartedMsg{name: label, ch: make(chan actionEvent, 1), cancel: func() {}}
		}
	}

	m = drive(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m = drive(t, m, statusMsg{cfg: cfg, status: lab.Status{ClusterUp: true, DexUp: true, MusterUp: true}})

	view := m.View()
	for _, want := range []string{"agentlab", "kind cluster", "dex", "up", "disabled", "claude mcp add"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "admin@lab.local") {
		t.Fatalf("users shown before toggling")
	}

	m = drive(t, m, key("u"))
	if view := m.View(); !strings.Contains(view, "admin@lab.local") || !strings.Contains(view, "platform-admins") {
		t.Errorf("users panel missing after toggle:\n%s", view)
	}

	// Down needs a confirmation; n backs out without launching anything.
	m = drive(t, m, key("d"))
	if view := m.View(); !strings.Contains(view, "destroy the kind cluster") {
		t.Errorf("no confirmation prompt:\n%s", view)
	}
	m = drive(t, m, key("n"))
	if len(launched) != 0 {
		t.Fatalf("declined confirmation still launched %v", launched)
	}

	// d then y launches `agentlab down`.
	m = drive(t, m, key("d"))
	mm, cmd := m.Update(key("y"))
	m = mm.(model)
	if len(launched) != 1 || launched[0][0] != "down" {
		t.Fatalf("launched = %v, want [[down]]", launched)
	}
	m = drive(t, m, cmd()) // actionStartedMsg
	if m.action != "down" {
		t.Fatalf("action = %q, want down", m.action)
	}
	if view := m.View(); !strings.Contains(view, "running") {
		t.Errorf("running action not shown:\n%s", view)
	}

	// A second action while one runs is refused with a hint.
	m = drive(t, m, key("s"))
	if len(launched) != 1 {
		t.Fatalf("second action launched while busy: %v", launched)
	}
	if m.flash == "" {
		t.Errorf("no busy hint set")
	}

	// Output lines stream into the scrollback; done clears the action.
	m = drive(t, m, actionEvent{line: "Lab destroyed."})
	if view := m.View(); !strings.Contains(view, "Lab destroyed.") {
		t.Errorf("output line missing:\n%s", view)
	}
	m = drive(t, m, actionEvent{done: true})
	if m.action != "" {
		t.Fatalf("action not cleared after done")
	}
	if view := m.View(); !strings.Contains(view, "✓ down finished") {
		t.Errorf("finish marker missing:\n%s", view)
	}
}

// TestSubmenus checks the open-URL and logs submenus dispatch and back out.
func TestSubmenus(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Enabled = false // exercise the disabled-component paths
	var launched [][]string
	m := newModel(cfg)
	m.runner = func(label string, args ...string) tea.Cmd {
		launched = append(launched, args)
		return func() tea.Msg { return actionFailedMsg{} }
	}
	m = drive(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})

	m = drive(t, m, key("o"))
	if view := m.View(); !strings.Contains(view, "open in browser") {
		t.Errorf("open submenu missing:\n%s", view)
	}
	m = drive(t, m, key("m")) // platform disabled: flash, no crash
	if m.mode != modeMain || !strings.Contains(m.flash, "disabled") {
		t.Errorf("disabled muster open: mode=%v flash=%q", m.mode, m.flash)
	}

	m = drive(t, m, key("l"))
	m = drive(t, m, key("esc"))
	if m.mode != modeMain || len(launched) != 0 {
		t.Fatalf("esc did not back out of logs menu (launched %v)", launched)
	}
	m = drive(t, m, key("l"))
	m, _ = mustModel(m.Update(key("b")))
	if len(launched) != 1 || strings.Join(launched[0], " ") != "logs backstage" {
		t.Fatalf("launched = %v, want [[logs backstage]]", launched)
	}
}

func mustModel(mm tea.Model, _ tea.Cmd) (model, bool) {
	m, ok := mm.(model)
	return m, ok
}

// TestCancelUnblocksWait reproduces canceling `logs`: the action subprocess
// spawns its own child (kubectl logs -f) that inherits the output pipe.
// Cancel must kill the whole process group — killing only the direct child
// leaves the grandchild holding the pipe and the done event never arrives.
func TestCancelUnblocksWait(t *testing.T) {
	selfExe = func() (string, error) { return "/bin/sh", nil }
	defer func() { selfExe = os.Executable }()

	// The grandchild must exist before the cancel (as kubectl does when the
	// user hits x), so the script reports readiness through the pipe.
	msg := startAction("logs", "-c", "sleep 300 & echo ready; sleep 300")()
	started, ok := msg.(actionStartedMsg)
	if !ok {
		t.Fatalf("startAction returned %T: %v", msg, msg)
	}

	deadline := time.After(10 * time.Second)
	canceled := false
	for {
		select {
		case ev := <-started.ch:
			if ev.line == "ready" && !canceled {
				canceled = true
				started.cancel()
			}
			if !ev.done {
				continue
			}
			if !canceled {
				t.Fatalf("subprocess exited before the test canceled it (err: %v)", ev.err)
			}
			if !ev.canceled {
				t.Errorf("done event not marked canceled (err: %v)", ev.err)
			}
			return
		case <-deadline:
			t.Fatal("done event never arrived after cancel; the process group kill is broken")
		}
	}
}
