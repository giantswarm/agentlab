package labs

import (
	"os"
	"path/filepath"
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
