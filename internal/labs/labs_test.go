package labs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// setup points the registry at a temporary directory and changes into an
// empty working directory, so every test starts with no lab anywhere.
func setup(t *testing.T) (cwd string) {
	t.Helper()
	regDir := t.TempDir()
	prev := Dir
	Dir = func() (string, error) { return regDir, nil }
	t.Cleanup(func() { Dir = prev })
	cwd = t.TempDir()
	t.Chdir(cwd)
	return cwd
}

// newLab creates a lab directory (one holding an agentlab.yaml) and
// registers it under name.
func newLab(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, dir, name)
	if err := Register(name, dir); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}
	return dir
}

// writeConfig writes an agentlab.yaml naming the lab name into dir.
func writeConfig(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.File), []byte("clusterName: "+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOrder(t *testing.T) {
	t.Run("nothing registered: the current directory", func(t *testing.T) {
		setup(t)
		got, err := Resolve("", false, nil)
		if err != nil || got.Dir != "" {
			t.Fatalf("Resolve() = %+v, %v; want the current directory", got, err)
		}
	})
	t.Run("one registered lab off a terminal: refused naming it and --lab", func(t *testing.T) {
		setup(t)
		dir := newLab(t, "one")
		_, err := Resolve("", false, nil)
		if err == nil || !strings.Contains(err.Error(), "one in "+dir) || !strings.Contains(err.Error(), "--lab") {
			t.Fatalf("Resolve() = %v, want a refusal naming one and --lab", err)
		}
	})
	t.Run("one registered lab on a terminal: the picker decides", func(t *testing.T) {
		setup(t)
		dir := newLab(t, "one")
		asked := false
		got, err := Resolve("", false, func(labs []Lab) (Lab, error) { asked = true; return labs[0], nil })
		if err != nil || !asked || got.Dir != dir {
			t.Fatalf("Resolve() = %+v, %v (picker asked: %v); want %s from the picker", got, err, asked, dir)
		}
	})
	t.Run("a local agentlab.yaml of an unregistered lab is this directory", func(t *testing.T) {
		cwd := setup(t)
		newLab(t, "one")
		writeConfig(t, cwd, "two")
		got, err := Resolve("", false, func([]Lab) (Lab, error) { t.Fatal("picker asked"); return Lab{}, nil })
		if err != nil || got.Dir != "" {
			t.Fatalf("Resolve() = %+v, %v; want the current directory", got, err)
		}
	})
	t.Run("a local agentlab.yaml of the lab registered here is this directory", func(t *testing.T) {
		cwd := setup(t)
		newLab(t, "one")
		writeConfig(t, cwd, "two")
		if err := Register("two", cwd); err != nil {
			t.Fatal(err)
		}
		got, err := Resolve("", false, nil)
		if err != nil || got.Dir != "" {
			t.Fatalf("Resolve() = %+v, %v; want the current directory", got, err)
		}
	})
	t.Run("a local agentlab.yaml naming the second lab enters it, never the first", func(t *testing.T) {
		cwd := setup(t)
		newLab(t, "agentlab")
		second := newLab(t, "agentlab-2")
		writeConfig(t, cwd, "agentlab-2")
		got, err := Resolve("", false, func([]Lab) (Lab, error) { t.Fatal("picker asked"); return Lab{}, nil })
		if err != nil || got.Name != "agentlab-2" || got.Dir != second {
			t.Fatalf("Resolve() = %+v, %v; want agentlab-2 in %s", got, err, second)
		}
	})
	t.Run("a local agentlab.yaml without clusterName is refused", func(t *testing.T) {
		cwd := setup(t)
		newLab(t, "agentlab")
		if err := os.WriteFile(filepath.Join(cwd, config.File), []byte("dexPort: 32000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve("", false, nil)
		if err == nil || !strings.Contains(err.Error(), "clusterName") {
			t.Fatalf("Resolve() = %v, want a refusal naming clusterName", err)
		}
	})
	t.Run("create: an empty directory stays the lab", func(t *testing.T) {
		setup(t)
		newLab(t, "one")
		got, err := Resolve("", true, nil)
		if err != nil || got.Dir != "" {
			t.Fatalf("Resolve() = %+v, %v; want the current directory", got, err)
		}
	})
	t.Run("two labs off a terminal: refused naming both and --lab", func(t *testing.T) {
		setup(t)
		a, b := newLab(t, "a"), newLab(t, "b")
		_, err := Resolve("", false, nil)
		if err == nil {
			t.Fatal("Resolve() = nil error, want a refusal")
		}
		for _, want := range []string{"a in " + a, "b in " + b, "--lab"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %q", err, want)
			}
		}
	})
	t.Run("two labs on a terminal: the picker decides", func(t *testing.T) {
		setup(t)
		newLab(t, "a")
		b := newLab(t, "b")
		var offered []Lab
		got, err := Resolve("", false, func(labs []Lab) (Lab, error) { offered = labs; return labs[1], nil })
		if err != nil || got.Dir != b {
			t.Fatalf("Resolve() = %+v, %v; want %s", got, err, b)
		}
		if len(offered) != 2 || offered[0].Name != "a" || offered[1].Name != "b" {
			t.Errorf("picker offered %+v, want a then b", offered)
		}
	})
	t.Run("--lab skips the picker and beats a local agentlab.yaml", func(t *testing.T) {
		cwd := setup(t)
		a := newLab(t, "a")
		newLab(t, "b")
		writeConfig(t, cwd, "b")
		got, err := Resolve("a", false, func([]Lab) (Lab, error) { t.Fatal("picker asked"); return Lab{}, nil })
		if err != nil || got.Dir != a {
			t.Fatalf("Resolve(a) = %+v, %v; want %s", got, err, a)
		}
	})
	t.Run("an unknown --lab is refused naming the registered ones", func(t *testing.T) {
		setup(t)
		a := newLab(t, "a")
		_, err := Resolve("nope", false, nil)
		if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "a in "+a) {
			t.Fatalf("Resolve(nope) = %v, want a refusal naming a", err)
		}
	})
}

func TestPruning(t *testing.T) {
	setup(t)
	gone := newLab(t, "gone")
	kept := newLab(t, "kept")
	if err := os.Remove(filepath.Join(gone, config.File)); err != nil {
		t.Fatal(err)
	}
	labs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(labs) != 1 || labs[0].Dir != kept {
		t.Fatalf("List() = %+v, want only kept", labs)
	}
	// The vanished lab is not offered.
	var offered []Lab
	if _, err := Resolve("", false, func(labs []Lab) (Lab, error) { offered = labs; return labs[0], nil }); err != nil {
		t.Fatal(err)
	}
	if len(offered) != 1 || offered[0].Dir != kept {
		t.Fatalf("picker offered %+v, want only %s", offered, kept)
	}
	// And the name is free again.
	if err := Check("gone", t.TempDir()); err != nil {
		t.Errorf("Check(gone) = %v, want nil once its directory vanished", err)
	}
}

// A read never drops an entry: a lab whose agentlab.yaml is away for a
// moment is listed again once it is back; a Register drops the stale ones.
func TestListKeepsAnEntryWhoseFileReturns(t *testing.T) {
	setup(t)
	dir := newLab(t, "away")
	f := filepath.Join(dir, config.File)
	if err := os.Rename(f, f+".aside"); err != nil {
		t.Fatal(err)
	}
	if labs, err := List(); err != nil || len(labs) != 0 {
		t.Fatalf("List() while away = %+v, %v; want none", labs, err)
	}
	if err := os.Rename(f+".aside", f); err != nil {
		t.Fatal(err)
	}
	if labs, err := List(); err != nil || len(labs) != 1 || labs[0].Dir != dir {
		t.Fatalf("List() once back = %+v, %v; want away in %s", labs, err, dir)
	}

	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	newLab(t, "other")
	reg, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Labs["away"]; ok || len(reg.Labs) != 1 {
		t.Errorf("registry after Register = %v, want only other", reg.Labs)
	}
}

func TestNameCollision(t *testing.T) {
	setup(t)
	a := newLab(t, "agentlab")
	other := t.TempDir()
	for name, fn := range map[string]func(string, string) error{"Check": Check, "Register": Register} {
		err := fn("agentlab", other)
		var taken *TakenError
		if !errors.As(err, &taken) {
			t.Fatalf("%s = %v, want a *TakenError", name, err)
		}
		if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), other) {
			t.Errorf("%s refusal %q does not name both directories", name, err)
		}
	}
	// The same directory re-registers freely.
	if err := Register("agentlab", a); err != nil {
		t.Errorf("Register(same dir) = %v", err)
	}
}

func TestRenameDropsTheOldName(t *testing.T) {
	setup(t)
	dir := newLab(t, "old")
	if err := Register("new", dir); err != nil {
		t.Fatal(err)
	}
	labs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(labs) != 1 || labs[0].Name != "new" {
		t.Fatalf("List() = %+v, want only new", labs)
	}
}
