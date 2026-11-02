package lab

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/labs"
)

// purgeLab makes a settled, registered lab with a CA under folders of the
// test's own and enters its Home, the cluster and the trust stores replaced
// by recorders of the steps Purge takes.
func purgeLab(t *testing.T, name string) (home string, steps *[]string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.File), []byte("clusterName: "+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := labs.Settle(name, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := labs.Register(name, home); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "certs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, caCertPath), []byte("ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)

	steps = &[]string{}
	prevCluster, prevTrust := purgeCluster, purgeTrust
	t.Cleanup(func() { purgeCluster, purgeTrust = prevCluster, prevTrust })
	purgeCluster = func(*config.Config) error {
		*steps = append(*steps, "cluster")
		return nil
	}
	purgeTrust = func() error {
		if _, err := os.Stat(caCertPath); err != nil {
			t.Errorf("the CA is gone before its removal from the trust stores: %v", err)
		}
		*steps = append(*steps, "trust")
		return nil
	}
	return home, steps
}

func registered(t *testing.T, name string) bool {
	t.Helper()
	ls, err := labs.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ls {
		if l.Name == name {
			return true
		}
	}
	return false
}

func TestPurge(t *testing.T) {
	t.Run("removes the cluster, then the CA, then the lab", func(t *testing.T) {
		home, steps := purgeLab(t, "purged")
		cfgFile, err := labs.ConfigFile("purged")
		if err != nil {
			t.Fatal(err)
		}

		if err := Purge(&config.Config{ClusterName: "purged"}); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		if got := *steps; len(got) != 2 || got[0] != "cluster" || got[1] != "trust" {
			t.Errorf("steps = %v, want [cluster trust]", got)
		}
		for _, p := range []string{home, filepath.Dir(cfgFile)} {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s is still there: %v", p, err)
			}
		}
		if registered(t, "purged") {
			t.Error("the registry still names purged")
		}
	})
	t.Run("refused outside its Home before anything changes", func(t *testing.T) {
		const cluster = "elsewhere"
		home, steps := purgeLab(t, cluster)
		t.Chdir(t.TempDir())

		var outside *labs.OutsideHomeError
		if err := Purge(&config.Config{ClusterName: cluster}); !errors.As(err, &outside) {
			t.Fatalf("Purge = %v, want *labs.OutsideHomeError", err)
		}
		if len(*steps) != 0 {
			t.Errorf("steps = %v, want none", *steps)
		}
		if _, err := os.Stat(filepath.Join(home, caCertPath)); err != nil {
			t.Errorf("the lab was touched: %v", err)
		}
	})
	t.Run("a failed trust store removal keeps the lab", func(t *testing.T) {
		const cluster = "kept"
		home, _ := purgeLab(t, cluster)
		purgeTrust = func() error { return errors.New("sudo: a password is required") }

		if err := Purge(&config.Config{ClusterName: cluster}); err == nil {
			t.Fatal("Purge succeeded with the trust stores failing")
		}
		if _, err := os.Stat(filepath.Join(home, caCertPath)); err != nil {
			t.Errorf("the lab's CA is gone: %v", err)
		}
		if !registered(t, cluster) {
			t.Error("the registry no longer names kept")
		}
	})
}
