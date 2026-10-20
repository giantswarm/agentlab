// Package xdg locates agentlab's folders the way the freedesktop XDG Base
// Directory specification says every program on the machine does: the
// configuration under $XDG_CONFIG_HOME, the state (labs, CAs, kubeconfigs)
// under $XDG_STATE_HOME, the cache under $XDG_CACHE_HOME and the binary in
// ~/.local/bin. Each variable counts only when it holds an absolute path, as
// the specification says; otherwise its default applies. macOS and Windows
// keep their own convention for configuration and cache (os.UserConfigDir,
// os.UserCacheDir) and put the state next to the configuration.
package xdg

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/giantswarm/agentlab/pkg/project"
)

// ConfigHome is agentlab's configuration folder: $XDG_CONFIG_HOME/agentlab,
// ~/.config/agentlab by default (~/Library/Application Support/agentlab on
// macOS).
func ConfigHome() (string, error) {
	return folder("XDG_CONFIG_HOME", ".config", os.UserConfigDir)
}

// StateHome is agentlab's state folder: $XDG_STATE_HOME/agentlab,
// ~/.local/state/agentlab by default (on macOS and Windows, which have no
// state folder, the configuration folder's).
func StateHome() (string, error) {
	if dir := absEnv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, project.Name), nil
	}
	if !freedesktop() {
		return ConfigHome()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", project.Name), nil
}

// CacheHome is agentlab's cache folder: $XDG_CACHE_HOME/agentlab,
// ~/.cache/agentlab by default (~/Library/Caches/agentlab on macOS).
func CacheHome() (string, error) {
	return folder("XDG_CACHE_HOME", ".cache", os.UserCacheDir)
}

// folder is agentlab's folder under the base the variable names, or under
// ~/<fallback>; on macOS and Windows under the base their own convention
// names (native).
func folder(variable, fallback string, native func() (string, error)) (string, error) {
	if !freedesktop() {
		dir, err := native()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, project.Name), nil
	}
	if dir := absEnv(variable); dir != "" {
		return filepath.Join(dir, project.Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback, project.Name), nil
}

// freedesktop reports whether this OS follows the XDG specification: every
// one but macOS and Windows.
func freedesktop() bool {
	return runtime.GOOS != "darwin" && runtime.GOOS != "ios" && runtime.GOOS != "windows"
}

// BinHome is the folder of the user's own executables, where self-update
// installs agentlab: $XDG_BIN_HOME, ~/.local/bin by default.
func BinHome() (string, error) {
	if dir := absEnv("XDG_BIN_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// absEnv is the variable's value when it is an absolute path, else "".
func absEnv(name string) string {
	if v := os.Getenv(name); filepath.IsAbs(v) {
		return v
	}
	return ""
}
