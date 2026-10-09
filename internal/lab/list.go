package lab

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/labs"
)

// ListedLab is one lab as `agentlab list` shows it, and its -o json shape.
type ListedLab struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	// Current marks the lab of the current directory.
	Current bool `json:"current"`
	// State is the kind cluster's: running, exited (a node container a `down`
	// that lost its race with docker left behind, node.go), paused, not
	// created, or unknown while docker does not answer.
	State string `json:"state"`
	// Components are the enabled parts of the lab, the platform with the
	// chart it installs.
	Components []string `json:"components"`
	// URLs are the lab's entry points by name: portal, muster.
	URLs map[string]string `json:"urls,omitempty"`
	// CA is trusted, untrusted, or none (no lab CA minted yet).
	CA string `json:"ca"`
	// Error is set when the lab's agentlab.yaml cannot be read; the other
	// fields past Dir are empty then.
	Error string `json:"error,omitempty"`
	// Unregistered marks a kind cluster no registered lab names: its lab
	// directory is unknown (Dir empty), so only its State is known.
	Unregistered bool `json:"unregistered,omitempty"`
}

// The cluster states `list` reports besides the containers' own.
const (
	stateNotCreated = "not created"
	stateUnknown    = "unknown"
)

// The CA column's values.
const (
	caTrusted   = "trusted"
	caUntrusted = "untrusted"
	caNone      = "none"
)

// labClusterState is the kind cluster's state from its node containers —
// docker alone, so `list` works while the apiserver is down. A variable so
// tests can stand in for docker.
var labClusterState = func(cluster string) string {
	cs, err := clusterContainers(cluster)
	switch {
	case err != nil:
		return stateUnknown
	case len(cs) == 0:
		return stateNotCreated
	}
	for _, c := range cs {
		if c.state == stateRunning {
			return stateRunning
		}
	}
	return cs[0].state
}

// listKindClusters names the kind clusters on this machine, running or not
// (kindClusters). A variable so tests can stand in for docker.
var listKindClusters = kindClusters

// labCAState is whether the lab CA in dir is in the system trust store. A
// variable so tests can stand in for the trust store.
var labCAState = func(dir string) string {
	cert := readCertFile(filepath.Join(dir, caCertPath))
	switch {
	case cert == nil:
		return caNone
	case certTrustedBySystem(cert):
		return caTrusted
	default:
		return caUntrusted
	}
}

// ListLabs describes every registered lab and every kind cluster no
// registered lab names (Unregistered: a lab whose registry entry or directory
// is gone still holds its cluster, ports and memory), sorted by name; here is
// the current directory (absolute), whose lab is marked. Nothing here needs
// a cluster to answer.
func ListLabs(registered []labs.Lab, here string) []ListedLab {
	out := make([]ListedLab, 0, len(registered))
	known := map[string]bool{}
	for _, l := range registered {
		known[l.Name] = true
		ll := ListedLab{Name: l.Name, Dir: l.Dir, Current: l.Dir == here}
		cfg, err := config.Peek(l.Dir)
		if err != nil {
			ll.Error = err.Error()
			out = append(out, ll)
			continue
		}
		known[cfg.ClusterName] = true
		ll.State = labClusterState(cfg.ClusterName)
		ll.Components = listComponents(cfg)
		ll.URLs = listURLs(cfg)
		ll.CA = labCAState(l.Dir)
		out = append(out, ll)
	}
	// Without docker the registered labs still list, each State "unknown".
	clusters, _ := listKindClusters()
	for _, c := range clusters {
		if !known[c] {
			out = append(out, ListedLab{Name: c, State: labClusterState(c), Unregistered: true})
		}
	}
	slices.SortFunc(out, func(a, b ListedLab) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// listComponents names what the lab runs: the bare kind+Dex sandbox, or the
// platform with its chart and the parts on top.
func listComponents(cfg *config.Config) []string {
	if !cfg.Platform.Enabled {
		return []string{"kind + Dex only"}
	}
	chart := "agent-platform " + cfg.Platform.ChartVersion
	if cfg.Platform.ChartPath != "" {
		chart = "chart " + cfg.Platform.ChartPath
	}
	out := []string{"platform (" + chart + ")"}
	if cfg.Platform.Agents {
		out = append(out, "agents")
	}
	if cfg.Platform.Observability {
		out = append(out, "observability")
	}
	if cfg.Backstage.Enabled {
		out = append(out, "Backstage")
	}
	return out
}

// listURLs are the entry points a person opens or wires a client to, with
// the edge's port suffix when it is not on 443.
func listURLs(cfg *config.Config) map[string]string {
	if !cfg.Platform.Enabled {
		return nil
	}
	urls := map[string]string{"muster": cfg.MusterBaseURL() + "/mcp"}
	if cfg.Backstage.Enabled {
		urls["portal"] = cfg.BackstageBaseURL()
	}
	return urls
}

// PrintLabs writes the list for a person, one block per lab, or as JSON.
func PrintLabs(w io.Writer, listed []ListedLab, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(listed)
	}
	var b strings.Builder
	if len(listed) == 0 {
		b.WriteString("No labs on this machine yet. `agentlab up` in an empty directory creates one.\n")
	}
	current := false
	for i, l := range listed {
		if i > 0 {
			b.WriteString("\n")
		}
		mark := " "
		if l.Current {
			mark, current = "*", true
		}
		if l.Unregistered {
			fmt.Fprintf(&b, "%s %s  %s  lab directory unknown\n", mark, l.Name, l.State)
			b.WriteString("    registry    no registered lab names this kind cluster; any agentlab command in its directory (`agentlab pods`) registers it again\n")
			continue
		}
		if l.Error != "" {
			fmt.Fprintf(&b, "%s %s  %s\n    error       %s\n", mark, l.Name, l.Dir, l.Error)
			continue
		}
		fmt.Fprintf(&b, "%s %s  %s  %s\n", mark, l.Name, l.State, l.Dir)
		fmt.Fprintf(&b, "    components  %s\n", strings.Join(l.Components, ", "))
		for _, name := range []string{"portal", "muster"} {
			if u, ok := l.URLs[name]; ok {
				fmt.Fprintf(&b, "    %-11s %s\n", name, u)
			}
		}
		fmt.Fprintf(&b, "    lab CA      %s\n", caNote(l.CA))
	}
	if current {
		b.WriteString("\n* the lab in the current directory\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func caNote(state string) string {
	switch state {
	case caTrusted:
		return "trusted on this machine"
	case caUntrusted:
		return "not trusted — browsers warn; `agentlab trust` (in the lab, or with --lab)"
	default:
		return "not minted yet (`agentlab up` creates it)"
	}
}
