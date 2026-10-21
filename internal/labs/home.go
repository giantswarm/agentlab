package labs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/xdg"
)

// labsDir is the folder of every lab inside agentlab's configuration and
// state folders.
const labsDir = "labs"

// Seams the tests point at temporary directories.
var (
	configHome = xdg.ConfigHome
	stateHome  = xdg.StateHome
)

// Home is the lab's directory under agentlab's state folder
// ($XDG_STATE_HOME/agentlab/labs/<name>): its certs/ (the lab CA), state/
// (kubeconfigs, rendered manifests, the kind node's mounted files) and the
// rest of what the lab writes. Every command runs in it, so the lab's
// relative paths hold. Its agentlab.yaml is a link to ConfigFile.
func Home(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := stateHome()
	if err != nil {
		return "", fmt.Errorf("locating agentlab's state folder: %w", err)
	}
	return filepath.Join(dir, labsDir, name), nil
}

// ConfigFile is the lab's agentlab.yaml under agentlab's configuration folder
// ($XDG_CONFIG_HOME/agentlab/labs/<name>/agentlab.yaml).
func ConfigFile(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := configHome()
	if err != nil {
		return "", fmt.Errorf("locating agentlab's configuration folder: %w", err)
	}
	return filepath.Join(dir, labsDir, name, config.File), nil
}

// validName refuses a lab name that is no clusterName — one read from an
// unvalidated agentlab.yaml could otherwise name a path outside the folders.
func validName(name string) error {
	if err := config.ValidateClusterName(name); err != nil {
		return fmt.Errorf("lab name %q: %w", name, err)
	}
	return nil
}

// moved are the lab files a lab directory from before the freedesktop
// folders holds besides its agentlab.yaml, all of them state: the lab CA and
// certificates, the rendered state, the OIDC kubeconfig and token `login`
// writes.
var moved = []string{"certs", "state", "kubeconfig.oidc", ".token"}

// CrossDeviceError is Settle's refusal to move a lab between file systems: a
// rename there is a copy, which a running lab's kind node mounts would not
// follow. Nothing was moved; the lab stays where it is.
type CrossDeviceError struct {
	Dir, Home string
}

func (e *CrossDeviceError) Error() string {
	return fmt.Sprintf("the lab in %s stays there: %s is on another file system, and a running lab's node mounts would not follow a copy", e.Dir, e.Home)
}

// Settle puts the lab of that name into the freedesktop folders and returns
// the directory the command runs in, Home(name). dir is where the lab lives
// now: Home itself (nothing to do but keep the link), a lab directory from
// before the folders (a checkout, a dedicated directory) or a directory with
// no lab yet, for a new one.
//
// A lab from elsewhere is moved, not copied: its agentlab.yaml to
// ConfigFile, certs/, state/ and the other lab files into Home, each by a
// rename, so nothing is lost and no file is written twice. Each moved file
// leaves a link at its old path: the kind node of a running lab mounts
// certs/ and state/node by their old absolute paths and resolves them again
// on a restart, and an older agentlab run in the old directory finds its
// lab there. A move across file systems is refused before anything moves
// (*CrossDeviceError); a failure halfway moves back what had moved. An
// agentlab.yaml that is a file of its own in Home moves to ConfigFile, a
// link in its place (adoptConfig).
func Settle(name, dir string) (string, error) {
	home, err := Home(name)
	if err != nil {
		return "", err
	}
	cfgFile, err := ConfigFile(name)
	if err != nil {
		return "", err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		// Links there need a privilege a person rarely has; a lab on
		// Windows (in practice WSL, which is Linux) stays where it is.
		return dir, nil
	}
	if err := os.MkdirAll(home, 0o750); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(cfgFile), 0o750); err != nil {
		return "", err
	}
	if dir != home {
		// A lab renamed inside the folders leaves no agentlab.yaml link
		// behind: the old name would still look like a lab.
		legacy := !strings.HasPrefix(dir, filepath.Dir(home)+string(filepath.Separator))
		if err := move(dir, home, cfgFile, legacy); err != nil {
			return "", err
		}
	}
	if err := adoptConfig(filepath.Join(home, config.File), cfgFile); err != nil {
		return "", err
	}
	return home, link(cfgFile, filepath.Join(home, config.File))
}

// ConfigConflictError is Settle's refusal to choose between two different
// agentlab.yaml files of one lab: a file of its own in the lab's Home and
// the one in the configuration folder. Both stay as they are.
type ConfigConflictError struct {
	Name, Home, Config, Newer string
}

func (e *ConfigConflictError) Error() string {
	return fmt.Sprintf("lab %s has two different agentlab.yaml files, %s and %s (the newer: %s); keep the one you want at %s and move the other aside, then the lab's link is made",
		e.Name, e.Home, e.Config, e.Newer, e.Config)
}

// adoptConfig gives the link to cfgFile its place at at, the agentlab.yaml
// of the lab's Home, when a file of its own is there: a lab settled in its
// Home before its agentlab.yaml had the configuration folder, or one an
// older release saved over the link. With no cfgFile yet the file moves
// there by a rename, its content byte for byte; a copy equal to cfgFile
// byte for byte goes, the link takes its place. One that differs is
// refused (*ConfigConflictError) and nothing changes: neither is the lab's
// configuration for certain.
func adoptConfig(at, cfgFile string) error {
	if fi, err := os.Lstat(at); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	if !exists(cfgFile) {
		return os.Rename(at, cfgFile)
	}
	inHome, err := os.ReadFile(filepath.Clean(at))
	if err != nil {
		return err
	}
	inConfig, err := os.ReadFile(filepath.Clean(cfgFile))
	if err != nil {
		return err
	}
	if !bytes.Equal(inHome, inConfig) {
		return &ConfigConflictError{Name: filepath.Base(filepath.Dir(at)), Home: at, Config: cfgFile, Newer: newer(at, cfgFile)}
	}
	return os.Remove(at)
}

// newer is whichever of the two files was modified last.
func newer(a, b string) string {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA == nil && errB == nil && fb.ModTime().After(fa.ModTime()) {
		return b
	}
	return a
}

// move carries the lab files of dir into home and its agentlab.yaml to
// cfgFile, leaving links behind — for agentlab.yaml only when configLink.
func move(dir, home, cfgFile string, configLink bool) error {
	type step struct{ from, to string }
	var steps []step
	if exists(filepath.Join(dir, config.File)) {
		steps = append(steps, step{filepath.Join(dir, config.File), cfgFile})
	}
	for _, name := range moved {
		if exists(filepath.Join(dir, name)) {
			steps = append(steps, step{filepath.Join(dir, name), filepath.Join(home, name)})
		}
	}
	for _, s := range steps {
		if exists(s.to) {
			return fmt.Errorf("moving the lab in %s to %s: %s exists already; move one of the two aside", dir, home, s.to)
		}
	}
	var done []step
	undo := func() {
		for i := len(done) - 1; i >= 0; i-- {
			_ = os.Rename(done[i].to, done[i].from)
		}
	}
	for _, s := range steps {
		if err := os.Rename(s.from, s.to); err != nil {
			undo()
			if errors.Is(err, syscall.EXDEV) {
				return &CrossDeviceError{Dir: dir, Home: home}
			}
			return fmt.Errorf("moving the lab in %s to %s: %w", dir, home, err)
		}
		done = append(done, s)
	}
	for _, s := range done {
		if s.to == cfgFile && !configLink {
			continue
		}
		if err := os.Symlink(s.to, s.from); err != nil {
			return fmt.Errorf("moved the lab in %s to %s, but the link %s is missing: %w", dir, home, s.from, err)
		}
	}
	return nil
}

// link makes at a symbolic link to target, unless it is one already.
func link(target, at string) error {
	if dest, err := os.Readlink(at); err == nil && dest == target {
		return nil
	}
	if _, err := os.Lstat(at); err == nil {
		return fmt.Errorf("%s is in the way of the link to %s; move it aside", at, target)
	}
	return os.Symlink(target, at)
}

// exists reports whether something, a dangling link included, is at path.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
