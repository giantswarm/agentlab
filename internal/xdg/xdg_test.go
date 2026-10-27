package xdg

import (
	"path/filepath"
	"runtime"
	"testing"
)

// The variables name the folders when absolute; a relative one is ignored,
// as the specification says, and the defaults under HOME apply.
func TestFolders(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the defaults asserted are Linux's")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_BIN_HOME"} {
		t.Setenv(v, "relative/"+v)
	}
	for name, tc := range map[string]struct {
		got  func() (string, error)
		want string
	}{
		"config": {ConfigHome, filepath.Join(home, ".config", "agentlab")},
		"state":  {StateHome, filepath.Join(home, ".local", "state", "agentlab")},
		"cache":  {CacheHome, filepath.Join(home, ".cache", "agentlab")},
		"bin":    {BinHome, filepath.Join(home, ".local", "bin")},
	} {
		if got, err := tc.got(); err != nil || got != tc.want {
			t.Errorf("%s = %s, %v; want %s", name, got, err, tc.want)
		}
	}
	t.Setenv("XDG_STATE_HOME", "/x/state")
	t.Setenv("XDG_BIN_HOME", "/x/bin")
	if got, _ := StateHome(); got != "/x/state/agentlab" {
		t.Errorf("StateHome = %s", got)
	}
	if got, _ := BinHome(); got != "/x/bin" {
		t.Errorf("BinHome = %s", got)
	}
}
