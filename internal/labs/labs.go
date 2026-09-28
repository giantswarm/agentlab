// Package labs is the registry of the labs on this machine: one entry per
// cluster name, pointing at the lab directory that holds its agentlab.yaml,
// certs/ and state/. `configure` and `up` write it; every command that runs
// against a lab resolves the lab through it (Resolve), so a person no longer
// has to `cd` into the lab first. The lab's own layout does not move: the
// registry only points at directories.
package labs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/pkg/project"
)

// file is the registry's name inside Dir.
const file = "labs.yaml"

// Dir is the registry's directory: agentlab's own under the user's config
// directory (~/.config/agentlab on Linux, ~/Library/Application
// Support/agentlab on macOS), the sibling of the update check's cache. A
// variable so tests can point it at a temporary directory.
var Dir = func() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, project.Name), nil
}

// Lab is one registered lab: its cluster name (agentlab.yaml's clusterName,
// the lab's name everywhere) and its absolute directory.
type Lab struct {
	Name string
	Dir  string
}

// registry is the file's shape: cluster name → lab directory.
type registry struct {
	Labs map[string]string `yaml:"labs"`
}

// List returns the registered labs sorted by name. Entries whose directory or
// agentlab.yaml is gone are dropped — neither offered nor listed — and the
// file is rewritten without them (best effort: a registry that cannot be
// written still answers).
func List() ([]Lab, error) {
	reg, err := read()
	if err != nil {
		return nil, err
	}
	var labs []Lab
	pruned := false
	for name, dir := range reg.Labs {
		if !isLab(dir) {
			delete(reg.Labs, name)
			pruned = true
			continue
		}
		labs = append(labs, Lab{Name: name, Dir: dir})
	}
	if pruned {
		_ = write(reg)
	}
	slices.SortFunc(labs, func(a, b Lab) int { return strings.Compare(a.Name, b.Name) })
	return labs, nil
}

// TakenError refuses a second lab of a registered name: the two would
// collide on the kind cluster and its ports.
type TakenError struct {
	Name  string
	Dir   string // the directory asking for the name
	Other string // the directory the name is registered to
}

func (e *TakenError) Error() string {
	return fmt.Sprintf("a lab named %q already lives in %s — its kind cluster and ports would collide with one in %s.\n"+
		"  Use that lab with --lab %s (or from its directory), or give this one another clusterName (`agentlab configure`).",
		e.Name, e.Other, e.Dir, e.Name)
}

// Check refuses a name registered to a different, still existing lab
// directory; it writes nothing. `configure` and the first run call it before
// they save an agentlab.yaml, so a refused name leaves no file behind.
func Check(name, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	reg, err := read()
	if err != nil {
		return err
	}
	return reg.taken(name, dir)
}

// taken is Check's rule on an already read registry.
func (reg *registry) taken(name, dir string) error {
	if other, ok := reg.Labs[name]; ok && other != dir && isLab(other) {
		return &TakenError{Name: name, Dir: dir, Other: other}
	}
	return nil
}

// Register records dir as the lab of that name, dropping any other name the
// directory was registered under (a changed clusterName). A name registered
// to another existing lab directory is refused with a *TakenError.
func Register(name, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	reg, err := read()
	if err != nil {
		return err
	}
	if err := reg.taken(name, dir); err != nil {
		return err
	}
	if reg.Labs[name] == dir && len(namesOf(reg, dir)) == 1 {
		return nil // already recorded: no write on every `up`
	}
	for _, n := range namesOf(reg, dir) {
		delete(reg.Labs, n)
	}
	reg.Labs[name] = dir
	return write(reg)
}

// Picker asks which of several labs a command runs against, on a terminal.
type Picker func(labs []Lab) (Lab, error)

// Resolve decides which lab directory a command runs against, in one order:
//
//  1. --lab <name> (flag), refused naming the registered labs when unknown;
//  2. an agentlab.yaml in the current directory, whatever the registry says;
//  3. unless create: the one registered lab, or on a terminal (pick non-nil)
//     the person's choice among several, or off one a refusal naming them
//     and --lab.
//
// dir is "" for the current directory — also when nothing is registered, so
// the caller's missing-agentlab.yaml path (the first run, or its refusal)
// stays what it is. create is for the commands that make a lab where they
// run (`configure`, `up`): in a directory without agentlab.yaml they create
// one there instead of reaching for another lab.
func Resolve(flag string, create bool, pick Picker) (lab Lab, err error) {
	if flag != "" {
		labs, err := List()
		if err != nil {
			return Lab{}, err
		}
		for _, l := range labs {
			if l.Name == flag {
				return l, nil
			}
		}
		return Lab{}, fmt.Errorf("no lab named %q is registered on this machine (%s); a lab registers itself on `agentlab configure` and `agentlab up` in its directory", flag, names(labs))
	}
	if _, err := os.Stat(config.File); err == nil {
		return Lab{}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Lab{}, err
	}
	if create {
		return Lab{}, nil
	}
	labs, err := List()
	if err != nil {
		return Lab{}, err
	}
	switch {
	case len(labs) == 0:
		return Lab{}, nil
	case len(labs) == 1:
		return labs[0], nil
	case pick != nil:
		return pick(labs)
	default:
		return Lab{}, fmt.Errorf("no %s here, and %d labs are registered on this machine (%s): pick one with --lab <name>, or run the command in its directory", config.File, len(labs), names(labs))
	}
}

// names lists the labs for a refusal: "a (/dir/a), b (/dir/b)", or "none".
func names(labs []Lab) string {
	if len(labs) == 0 {
		return "none registered"
	}
	parts := make([]string, len(labs))
	for i, l := range labs {
		parts[i] = fmt.Sprintf("%s in %s", l.Name, l.Dir)
	}
	return strings.Join(parts, ", ")
}

// namesOf returns the names registered to dir.
func namesOf(reg *registry, dir string) []string {
	var out []string
	for n, d := range reg.Labs {
		if d == dir {
			out = append(out, n)
		}
	}
	return out
}

// isLab reports whether dir still holds a lab: its agentlab.yaml exists.
func isLab(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, config.File))
	return err == nil
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", fmt.Errorf("locating the lab registry: %w", err)
	}
	return filepath.Join(dir, file), nil
}

// read loads the registry; a missing file is an empty one.
func read() (*registry, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	reg := &registry{Labs: map[string]string{}}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(raw, reg); err != nil {
		return nil, fmt.Errorf("parsing the lab registry %s: %w", p, err)
	}
	if reg.Labs == nil {
		reg.Labs = map[string]string{}
	}
	return reg, nil
}

// write replaces the registry in one rename, so a concurrent reader sees the
// old file or the new one, never half of one.
func write(reg *registry) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	out, err := yaml.Marshal(reg)
	if err != nil {
		return err
	}
	header := []byte("# The labs on this machine, written by `agentlab configure` and `agentlab up`.\n")
	tmp, err := os.CreateTemp(filepath.Dir(p), file+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(header, out...)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
