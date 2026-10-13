package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/lab"
	"github.com/giantswarm/agentlab/internal/labs"
)

// Every command a person can discover belongs to one of the help groups —
// otherwise cobra prints it under "Additional Commands", below the groups,
// which is where a new command would silently end up.
func TestEveryCommandHasAGroup(t *testing.T) {
	root := rootCmd()
	ids := []string{}
	for _, g := range root.Groups() {
		ids = append(ids, g.ID)
	}
	for _, cmd := range root.Commands() {
		if cmd.Hidden {
			continue
		}
		if !slices.Contains(ids, cmd.GroupID) {
			t.Errorf("command %q has group %q, want one of %v (see inGroup in main.go)", cmd.Name(), cmd.GroupID, ids)
		}
	}
}

// The groups a newcomer meets first, and where the everyday commands sit.
func TestGroupOrderAndPlacement(t *testing.T) {
	root := rootCmd()
	var titles []string
	for _, g := range root.Groups() {
		titles = append(titles, g.Title)
	}
	want := []string{"Setup", "Everyday", "Testing", "Cleanup", "Advanced"}
	if !slices.Equal(titles, want) {
		t.Errorf("group titles = %v, want %v (docs/cli.md carries the same order)", titles, want)
	}
	for name, group := range map[string]string{
		"up":    groupSetup,
		"open":  groupEveryday,
		"login": groupEveryday,
		"down":  groupCleanup,
		"test":  groupTesting,
	} {
		cmd := find(root, name)
		if cmd == nil {
			t.Errorf("no %q command", name)
			continue
		}
		if cmd.GroupID != group {
			t.Errorf("%q is in group %q, want %q", name, cmd.GroupID, group)
		}
	}
}

// `backstage` is gone (Backstage deploys with the platform), and `browser`
// stays only as the hidden name `login --browser` had.
func TestRetiredCommands(t *testing.T) {
	root := rootCmd()
	if cmd := find(root, "backstage"); cmd != nil {
		t.Errorf("the retired backstage command is still registered")
	}
	browser := find(root, "browser")
	if browser == nil {
		t.Fatalf("no browser command; it stays as the hidden name of `login --browser`")
	}
	if !browser.Hidden {
		t.Errorf("the browser command should be hidden")
	}
}

// `up` and `platform` carry the pre-answers for the questions they end with.
func TestBootCommandsTakeTheOfferFlags(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"up", "platform"} {
		cmd := find(root, name)
		if cmd == nil {
			t.Fatalf("no %q command", name)
		}
		for _, flag := range []string{"trust", "open"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%q has no --%s flag", name, flag)
			}
		}
	}
}

func find(root *cobra.Command, name string) *cobra.Command {
	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	return nil
}

// A command run in a lab directory the registry does not know registers it:
// a lab from before the registry, or one whose entry is gone, is found from
// any directory again.
func TestEnterLabRegistersTheLabHere(t *testing.T) {
	regDir := t.TempDir()
	prev := labs.Dir
	labs.Dir = func() (string, error) { return regDir, nil }
	t.Cleanup(func() { labs.Dir = prev })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.File), []byte("clusterName: unregistered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if err := enterLab(false); err != nil {
		t.Fatal(err)
	}
	got, err := labs.List()
	if err != nil || len(got) != 1 || got[0].Name != "unregistered" || got[0].Dir != dir {
		t.Fatalf("labs.List() = %+v, %v; want unregistered in %s", got, err, dir)
	}
}

// The help of --workspaces-cleanup-after names the range the sessions part's
// precondition requires, taken from the same constants.
func TestCleanupAfterHelpNamesTheSessionsRange(t *testing.T) {
	flag := find(rootCmd(), "configure").Flags().Lookup("workspaces-cleanup-after")
	if flag == nil {
		t.Fatal("configure has no --workspaces-cleanup-after flag")
	}
	for _, want := range []string{lab.SessionsCleanupWindowRange(), lab.SessionsCleanupWindowExample()} {
		if !strings.Contains(flag.Usage, want) {
			t.Errorf("--workspaces-cleanup-after help %q does not name %q", flag.Usage, want)
		}
	}
}

// Without a terminal `configure` refuses before its form starts, and the
// message names --defaults, the way without one.
func TestConfigureWithoutATerminalNamesDefaults(t *testing.T) {
	prev := formTerminal
	formTerminal = func() bool { return false }
	t.Cleanup(func() { formTerminal = prev })
	t.Setenv("ACCESSIBLE", "")
	dir := t.TempDir()
	t.Chdir(dir)

	cmd := configureCmd()
	cmd.SetArgs(nil)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if !errors.Is(err, errHeadlessForm) {
		t.Fatalf("configure without a terminal = %v, want errHeadlessForm", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "--defaults") || strings.Contains(msg, "TTY") || strings.Contains(msg, "\n") {
		t.Fatalf("message %q: want one line naming --defaults", msg)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("configure wrote %d entries before refusing", len(entries))
	}
}
