package labs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// A lab directory from before the freedesktop folders — a checkout with
// other files in it — moves into the folders: agentlab.yaml to the
// configuration folder, the lab files to the lab's Home, each leaving a link
// at its old path; files that are not the lab's stay. A second Settle finds
// nothing to move.
func TestSettleMovesALegacyLab(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	writeConfig(t, dir, "old")
	for _, d := range []string{"certs", "state/node"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"certs/ca.crt", "state/node/sysctl.conf", "kubeconfig.oidc", "go.mod"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	home, err := Settle("old", dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Home("old")
	cfgFile, _ := ConfigFile("old")
	if home != want {
		t.Fatalf("Settle = %s, want %s", home, want)
	}
	for at, target := range map[string]string{
		filepath.Join(dir, config.File):       cfgFile,
		filepath.Join(dir, "certs"):           filepath.Join(home, "certs"),
		filepath.Join(dir, "state"):           filepath.Join(home, "state"),
		filepath.Join(dir, "kubeconfig.oidc"): filepath.Join(home, "kubeconfig.oidc"),
		filepath.Join(home, config.File):      cfgFile,
	} {
		if dest, err := os.Readlink(at); err != nil || dest != target {
			t.Errorf("%s links to %q (%v), want %s", at, dest, err, target)
		}
	}
	for _, f := range []string{"certs/ca.crt", "state/node/sysctl.conf"} {
		if got, err := os.ReadFile(filepath.Clean(filepath.Join(home, f))); err != nil || string(got) != f {
			t.Errorf("%s in the Home = %q, %v", f, got, err)
		}
		if got, err := os.ReadFile(filepath.Clean(filepath.Join(dir, f))); err != nil || string(got) != f {
			t.Errorf("%s through the old path = %q, %v", f, got, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "go.mod")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("go.mod, not the lab's, moved: %v", err)
	}
	if cfg, err := config.Peek(home); err != nil || cfg.ClusterName != "old" {
		t.Errorf("Peek(Home) = %v, %v", cfg, err)
	}
	if again, err := Settle("old", home); err != nil || again != home {
		t.Errorf("a second Settle = %s, %v", again, err)
	}
}

// A new lab gets its Home and the link to a configuration file that does
// not exist yet; saving through the link writes the configuration folder.
func TestSettleMakesANewLab(t *testing.T) {
	cwd := setup(t)
	home, err := Settle("fresh", cwd)
	if err != nil {
		t.Fatal(err)
	}
	cfgFile, _ := ConfigFile("fresh")
	if err := os.WriteFile(filepath.Join(home, config.File), []byte("clusterName: fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgFile); err != nil {
		t.Fatalf("the write through the link did not reach %s: %v", cfgFile, err)
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("the empty directory a new lab was made from holds %v", entries)
	}
}

// A lab renamed inside the folders moves to the new name's Home and leaves
// no agentlab.yaml behind, so the old name is no lab anymore.
func TestSettleRenamesInsideTheFolders(t *testing.T) {
	cwd := setup(t)
	oldHome, err := Settle("before", cwd)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, oldHome, "after")
	if err := Register("before", oldHome); err != nil {
		t.Fatal(err)
	}
	if _, err := Settle("after", oldHome); err != nil {
		t.Fatal(err)
	}
	if isLab(oldHome) {
		t.Errorf("the old Home %s still holds an agentlab.yaml", oldHome)
	}
}

// Settle refuses to move a lab over files that are already in its Home and
// moves nothing.
func TestSettleRefusesToOverwrite(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	writeConfig(t, dir, "taken")
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o750); err != nil {
		t.Fatal(err)
	}
	home, _ := Home("taken")
	if err := os.MkdirAll(filepath.Join(home, "certs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Settle("taken", dir); err == nil {
		t.Fatal("Settle over an existing Home succeeded")
	}
	if fi, err := os.Lstat(filepath.Join(dir, config.File)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("agentlab.yaml moved although Settle refused: %v", err)
	}
}

// The link Settle leaves in a lab's old directory enters the moved lab in
// every mode, so `up` or `render` run there act on the lab, not on a copy.
func TestResolveFollowsTheLinkOfAMovedLab(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	writeConfig(t, dir, "moved")
	home, err := Settle("moved", dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register("moved", home); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, mode := range []Mode{Enter, Create, Here} {
		got, err := Resolve("", mode, nil)
		if err != nil || got.Dir != home {
			t.Errorf("Resolve(mode %d) = %+v, %v; want the lab in %s", mode, got, err, home)
		}
	}
}

// Registering a moved lab under its Home is no clash with the old directory
// the registry still names: the link there is the lab's own file.
func TestRegisterAMovedLabOverItsOldDirectory(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	writeConfig(t, dir, "moving")
	if err := Register("moving", dir); err != nil {
		t.Fatal(err)
	}
	home, err := Settle("moving", dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Register("moving", home); err != nil {
		t.Fatalf("Register(Home) = %v", err)
	}
	if got, err := List(); err != nil || len(got) != 1 || got[0].Dir != home {
		t.Fatalf("List() = %+v, %v; want the lab in %s only", got, err, home)
	}
}

// A name that is no clusterName never becomes a path: nothing moves.
func TestSettleRefusesANameThatIsNoClusterName(t *testing.T) {
	cwd := setup(t)
	writeConfig(t, cwd, "x")
	for _, name := range []string{"../escape", "a/b", "", "UPPER"} {
		if _, err := Settle(name, cwd); err == nil {
			t.Errorf("Settle(%q) succeeded", name)
		}
	}
	if fi, err := os.Lstat(filepath.Join(cwd, config.File)); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("agentlab.yaml moved: %v", err)
	}
}

// A lab whose agentlab.yaml is a file of its own in its Home — the layout
// before the configuration folder, or a save of an older release over the
// link — settles in place by itself, whatever is in the configuration
// folder: the file moves there byte for byte, an equal copy gives way to
// the link, and a different one is refused with both files untouched.
func TestSettleAdoptsTheAgentlabYAMLOfAHome(t *testing.T) {
	const content = "clusterName: lab\n# a comment and trailing spaces  \n"
	for _, tc := range []struct {
		name     string
		inConfig string // the configuration folder's file, "" none
		inHome   string // the Home's own file, "" the link alone
		refused  bool
	}{
		{name: "old layout: the file in the Home only", inHome: content},
		{name: "new layout: the link in the Home", inConfig: content},
		{name: "both, equal", inConfig: content, inHome: content},
		{name: "both, different", inConfig: content, inHome: "clusterName: lab\nplatform: {}\n", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)
			home, _ := Home("lab")
			cfgFile, _ := ConfigFile("lab")
			at := filepath.Join(home, config.File)
			for _, d := range []string{home, filepath.Dir(cfgFile)} {
				if err := os.MkdirAll(d, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if tc.inConfig != "" {
				if err := os.WriteFile(cfgFile, []byte(tc.inConfig), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.inHome != "" {
				if err := os.WriteFile(at, []byte(tc.inHome), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(cfgFile, at); err != nil {
				t.Fatal(err)
			}

			got, err := Settle("lab", home)
			if tc.refused {
				var conflict *ConfigConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("Settle = %v, want a *ConfigConflictError", err)
				}
				for _, path := range []string{at, cfgFile} {
					if !strings.Contains(err.Error(), path) {
						t.Errorf("the refusal %q does not name %s", err, path)
					}
				}
				if fi, err := os.Lstat(at); err != nil || !fi.Mode().IsRegular() {
					t.Errorf("the Home's file changed although Settle refused: %v", err)
				}
				for path, want := range map[string]string{at: tc.inHome, cfgFile: tc.inConfig} {
					if b, err := os.ReadFile(filepath.Clean(path)); err != nil || string(b) != want {
						t.Errorf("%s = %q, %v; want it untouched", path, b, err)
					}
				}
				return
			}
			if err != nil || got != home {
				t.Fatalf("Settle = %s, %v; want %s", got, err, home)
			}
			if dest, err := os.Readlink(at); err != nil || dest != cfgFile {
				t.Errorf("%s links to %q (%v), want %s", at, dest, err, cfgFile)
			}
			if b, err := os.ReadFile(filepath.Clean(cfgFile)); err != nil || string(b) != content {
				t.Errorf("%s = %q, %v; want the lab's file byte for byte", cfgFile, b, err)
			}
			if again, err := Settle("lab", home); err != nil || again != home {
				t.Errorf("a second Settle = %s, %v", again, err)
			}
		})
	}
}

// sharedFolders points the configuration and the state folder at one
// directory, as on macOS and Windows (xdg.StateHome falls back to
// xdg.ConfigHome): a lab's Home is then its ConfigFile's folder.
func sharedFolders(t *testing.T) (cwd string) {
	t.Helper()
	cwd = setup(t)
	dir := t.TempDir()
	configHome = func() (string, error) { return dir, nil }
	stateHome = func() (string, error) { return dir, nil }
	return cwd
}

// wantConfigFile fails unless the lab's ConfigFile is a regular file
// holding content.
func wantConfigFile(t *testing.T, name, content string) {
	t.Helper()
	cfgFile, _ := ConfigFile(name)
	fi, err := os.Lstat(cfgFile)
	if err != nil || !fi.Mode().IsRegular() {
		dest, _ := os.Readlink(cfgFile)
		t.Fatalf("%s is no regular file (%v, links to %q)", cfgFile, err, dest)
	}
	if b, err := os.ReadFile(filepath.Clean(cfgFile)); err != nil || string(b) != content {
		t.Errorf("%s = %q, %v; want %q", cfgFile, b, err, content)
	}
}

// With the configuration and the state in one folder (macOS), a lab's
// agentlab.yaml is never linked onto itself nor removed as a copy of
// itself: a new lab, an old-layout lab moved in, a second Settle, and a
// self-link an earlier release left behind (#1106).
func TestSettleWithOneFolderForConfigurationAndState(t *testing.T) {
	t.Run("a new lab", func(t *testing.T) {
		cwd := sharedFolders(t)
		home, err := Settle("fresh", cwd)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(home, config.File)); !os.IsNotExist(err) {
			t.Fatalf("a new lab's Home holds an agentlab.yaml before it is saved: %v", err)
		}
		writeConfig(t, home, "fresh")
		wantConfigFile(t, "fresh", "clusterName: fresh\n")
		if again, err := Settle("fresh", home); err != nil || again != home {
			t.Errorf("a second Settle = %s, %v", again, err)
		}
		wantConfigFile(t, "fresh", "clusterName: fresh\n")
	})

	t.Run("an old-layout lab moved in", func(t *testing.T) {
		sharedFolders(t)
		dir := t.TempDir()
		writeConfig(t, dir, "old")
		if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "certs", "ca.crt"), []byte("ca"), 0o600); err != nil {
			t.Fatal(err)
		}
		home, err := Settle("old", dir)
		if err != nil {
			t.Fatal(err)
		}
		wantConfigFile(t, "old", "clusterName: old\n")
		cfgFile, _ := ConfigFile("old")
		if dest, err := os.Readlink(filepath.Join(dir, config.File)); err != nil || dest != cfgFile {
			t.Errorf("the old directory's agentlab.yaml links to %q (%v), want %s", dest, err, cfgFile)
		}
		if cfg, err := config.Peek(dir); err != nil || cfg.ClusterName != "old" {
			t.Errorf("Peek(old directory) = %v, %v", cfg, err)
		}
		if b, err := os.ReadFile(filepath.Join(home, "certs", "ca.crt")); err != nil || string(b) != "ca" {
			t.Errorf("certs/ca.crt in the Home = %q, %v", b, err)
		}
		if again, err := Settle("old", home); err != nil || again != home {
			t.Errorf("a second Settle = %s, %v", again, err)
		}
		wantConfigFile(t, "old", "clusterName: old\n")
	})

	t.Run("a second Settle on a settled lab", func(t *testing.T) {
		sharedFolders(t)
		home, _ := Home("lab")
		if err := os.MkdirAll(home, 0o750); err != nil {
			t.Fatal(err)
		}
		writeConfig(t, home, "lab")
		for range 2 {
			if got, err := Settle("lab", home); err != nil || got != home {
				t.Fatalf("Settle = %s, %v; want %s", got, err, home)
			}
			wantConfigFile(t, "lab", "clusterName: lab\n")
		}
	})

	t.Run("a self-link from an earlier release", func(t *testing.T) {
		sharedFolders(t)
		home, _ := Home("broken")
		cfgFile, _ := ConfigFile("broken")
		if err := os.MkdirAll(home, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cfgFile, cfgFile); err != nil {
			t.Fatal(err)
		}
		if _, err := Settle("broken", home); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(cfgFile); !os.IsNotExist(err) {
			t.Fatalf("the self-link at %s is still there: %v", cfgFile, err)
		}
		writeConfig(t, home, "broken")
		wantConfigFile(t, "broken", "clusterName: broken\n")
	})
}
