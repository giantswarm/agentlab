package lab

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// A platform.valuesFiles entry is an overlay: values merged over the lab's
// render, typically a proof's pin of a component to a version the chart's own
// range does not admit. It changes what the lab runs without changing the
// chart version anything else reports, so `agentlab status` names every one —
// the ones agentlab.yaml lists and the ones the release in place was installed
// with — and `agentlab configure --released` drops them before a lab is handed
// on.

// overlaysMarker introduces the overlays a platform run merged in its
// revision's description (installDescription): the record `agentlab status`
// reads back, so an overlay that left agentlab.yaml but still runs in the
// cluster is named too.
const overlaysMarker = "; overlays: "

// overlaySetsShown bounds the keys `agentlab status` prints per overlay; the
// JSON carries them all.
const overlaySetsShown = 8

// Overlay is one platform.valuesFiles entry as `agentlab status` reports it.
type Overlay struct {
	// File is the entry as agentlab.yaml lists it.
	File string `json:"file"`
	// Sets are the values the file sets, one `<dotted key>=<value>` each in
	// key order: a component's version, versionRange, semverFilter or
	// repository among them.
	Sets []string `json:"sets,omitempty"`
	// Error is why the file could not be read; "" when it was.
	Error string `json:"error,omitempty"`
	// Installed reports that the release in place was installed with it.
	Installed bool `json:"installed"`
}

// readOverlays reads agentlab.yaml's overlays, relative paths from the lab
// directory, and marks the ones the release in place carries.
func readOverlays(cfg *config.Config, dir string, installed *installedChart) []Overlay {
	var applied []string
	if installed != nil {
		applied = installed.Overlays()
	}
	overlays := make([]Overlay, 0, len(cfg.Platform.ValuesFiles))
	for _, file := range cfg.Platform.ValuesFiles {
		o := Overlay{File: file, Installed: slices.Contains(applied, file)}
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, err := os.ReadFile(path) // #nosec G304 -- the overlay agentlab.yaml names
		if err == nil {
			o.Sets, err = overlaySets(data)
		}
		if err != nil {
			o.Error = err.Error()
		}
		overlays = append(overlays, o)
	}
	return overlays
}

// staleOverlays are the overlays the release in place was installed with
// that agentlab.yaml no longer lists: still running, gone at the next
// platform run.
func staleOverlays(cfg *config.Config, installed *installedChart) []string {
	if installed == nil {
		return nil
	}
	var stale []string
	for _, file := range installed.Overlays() {
		if !slices.Contains(cfg.Platform.ValuesFiles, file) {
			stale = append(stale, file)
		}
	}
	return stale
}

// overlaySets flattens a values file into its leaves, `<dotted key>=<value>`
// in key order: a list is one leaf, an empty map one, a null the key's
// deletion.
func overlaySets(data []byte) ([]string, error) {
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	var sets []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch v := v.(type) {
		case map[string]any:
			if len(v) == 0 {
				sets = append(sets, prefix+"={}")
				return
			}
			for k, child := range v {
				walk(joinKey(prefix, k), child)
			}
		case []any:
			sets = append(sets, fmt.Sprintf("%s=[%d items]", prefix, len(v)))
		case nil:
			sets = append(sets, prefix+"=null")
		default:
			sets = append(sets, fmt.Sprintf("%s=%v", prefix, v))
		}
	}
	walk("", values)
	slices.Sort(sets)
	return sets, nil
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// overlaysNote is the suffix installDescription gives a run with overlays.
func overlaysNote(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return overlaysMarker + strings.Join(files, ", ")
}

// Overlays are the platform.valuesFiles entries the revision was installed
// with, as its description records them; none for a run without, or one of an
// agentlab from before the record.
func (c installedChart) Overlays() []string {
	_, files, ok := strings.Cut(c.Description, overlaysMarker)
	if !ok || files == "" {
		return nil
	}
	return strings.Split(files, ", ")
}

// overlayLines are `agentlab status`'s lines on the overlays: each listed one
// with what it sets, and each the release in place carries that agentlab.yaml
// dropped.
func overlayLines(s *Status) []string {
	var lines []string
	for _, o := range s.Overlays {
		line := o.File
		switch {
		case o.Error != "":
			line += " — unreadable: " + o.Error
		case len(o.Sets) == 0:
			line += " — sets nothing"
		default:
			sets := o.Sets
			more := ""
			if len(sets) > overlaySetsShown {
				more = fmt.Sprintf(" and %d more", len(sets)-overlaySetsShown)
				sets = sets[:overlaySetsShown]
			}
			line += " sets " + strings.Join(sets, ", ") + more
		}
		if s.Chart != nil && !o.Installed {
			line += " (not in the release in place: `agentlab platform` applies it)"
		}
		lines = append(lines, line)
	}
	for _, file := range s.StaleOverlays {
		lines = append(lines, file+" is in the release in place but no longer in "+config.File+": `agentlab platform` drops it")
	}
	if len(lines) > 0 {
		lines[len(lines)-1] += "; the component versions differ from the chart's own — `agentlab configure --released` drops every overlay"
	}
	return lines
}

// DropProofState puts agentlab.yaml back on the released platform, the
// restore step before a lab is handed on: the overlays
// (platform.valuesFiles), the dev images, a chart directory, the dev channel
// with the dev build it pinned, and a local guest image all go. It returns
// one line per dropped setting, none when the config had none.
func DropProofState(cfg *config.Config) []string {
	var dropped []string
	p := &cfg.Platform
	for _, file := range p.ValuesFiles {
		dropped = append(dropped, "overlay "+file)
	}
	p.ValuesFiles = nil
	for _, name := range slices.Sorted(maps.Keys(p.DevImages)) {
		dropped = append(dropped, "dev image "+name+" -> "+p.DevImages[name])
	}
	p.DevImages = nil
	if p.ChartPath != "" {
		dropped = append(dropped, "chart directory "+p.ChartPath)
		p.ChartPath = ""
	}
	if p.ChartBranch != "" {
		dropped = append(dropped, "dev channel of branch "+p.ChartBranch)
		p.ChartBranch = ""
		p.ChartPinned = false
	}
	if old := ResetDevBuildPin(cfg); old != "" {
		dropped = append(dropped, "dev build "+old+" in chartVersion (now "+p.ChartVersion+")")
	}
	if p.VMManager.ImageDir != "" {
		dropped = append(dropped, "guest image build "+p.VMManager.ImageDir)
		p.VMManager.ImageDir = ""
	}
	return dropped
}
