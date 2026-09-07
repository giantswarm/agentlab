package lab

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The portal's MCP servers page groups servers by the tool-group label into
// three sections — Agent Platform, Infrastructure, Registered servers — and
// collapses the members of a family (spec.family.name) into one row. This is
// the same arithmetic the released plugin runs (plugins/muster
// serverGrouping.ts: partitionServers, familyToolGroup), ported so the lab
// can assert what the page shows from the data the page reads: the MCPServer
// CRs, fetched through Backstage's Kubernetes proxy with the user's own
// token. The label contract (D8): agent-platform | infrastructure; absent (or
// unknown) = Registered servers. Orientation, not authorization.

// Tool groups in display order, and their titles on the page.
const (
	groupAgentPlatform  = "agent-platform"
	groupInfrastructure = "infrastructure"
	groupRegistered     = "registered"
)

var toolGroupOrder = []string{groupAgentPlatform, groupInfrastructure, groupRegistered}

var toolGroupTitles = map[string]string{
	groupAgentPlatform:  "Agent Platform",
	groupInfrastructure: "Infrastructure",
	groupRegistered:     "Registered servers",
}

// mcpServerCR is the part of an MCPServer resource the grouping reads.
type mcpServerCR struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Family *struct {
			Name string `json:"name"`
		} `json:"family"`
	} `json:"spec"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func (s mcpServerCR) name() string { return s.Metadata.Name }

func (s mcpServerCR) family() string {
	if s.Spec.Family == nil {
		return ""
	}
	return s.Spec.Family.Name
}

// toolGroup is MCPServer.getToolGroup(): the declared group, or "" for a
// server without (or with an unknown) label.
func (s mcpServerCR) toolGroup() string {
	switch v := s.Metadata.Labels[toolGroupLabel]; v {
	case groupAgentPlatform, groupInfrastructure:
		return v
	}
	return ""
}

// toolGroupKey is MCPServer.getToolGroupKey(): the group the server is
// rendered under.
func (s mcpServerCR) toolGroupKey() string {
	if g := s.toolGroup(); g != "" {
		return g
	}
	return groupRegistered
}

// serverRow is one row of the page: a family (one row for all its members)
// or a singular server.
type serverRow struct {
	Family  string
	Servers []mcpServerCR
}

// rowName is the family name for a family row, the server name otherwise.
func (r serverRow) rowName() string {
	if r.Family != "" {
		return r.Family
	}
	return r.Servers[0].name()
}

// familyToolGroup is the plugin's familyToolGroup: the labelled members
// decide (most common declared group, ties broken by display order); a family
// no member has labelled is a Registered server.
func familyToolGroup(members []mcpServerCR) string {
	votes := map[string]int{}
	for _, m := range members {
		if g := m.toolGroup(); g != "" {
			votes[g]++
		}
	}
	if len(votes) == 0 {
		return groupRegistered
	}
	best := ""
	for _, g := range toolGroupOrder {
		if n, ok := votes[g]; ok && (best == "" || n > votes[best]) {
			best = g
		}
	}
	return best
}

// partitionServers is the plugin's partitionServers: every group present (in
// display order, possibly empty), family rows first (alphabetical), then
// singular servers (alphabetical).
func partitionServers(servers []mcpServerCR) map[string][]serverRow {
	families := map[string][]mcpServerCR{}
	var singular []mcpServerCR
	for _, s := range servers {
		if f := s.family(); f != "" {
			families[f] = append(families[f], s)
		} else {
			singular = append(singular, s)
		}
	}
	out := map[string][]serverRow{}
	for _, g := range toolGroupOrder {
		out[g] = []serverRow{}
	}
	familyNamesSorted := make([]string, 0, len(families))
	for f := range families {
		familyNamesSorted = append(familyNamesSorted, f)
	}
	sort.Strings(familyNamesSorted)
	for _, f := range familyNamesSorted {
		g := familyToolGroup(families[f])
		out[g] = append(out[g], serverRow{Family: f, Servers: families[f]})
	}
	sort.Slice(singular, func(i, j int) bool { return singular[i].name() < singular[j].name() })
	for _, s := range singular {
		g := s.toolGroupKey()
		out[g] = append(out[g], serverRow{Servers: []mcpServerCR{s}})
	}
	return out
}

// rowNames lists a group's rows by name, in display order.
func rowNames(rows []serverRow) []string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.rowName())
	}
	return names
}

// stripToolGroupLabels returns the servers without the tool-group label —
// what an installation whose charts stamp no labels looks like.
func stripToolGroupLabels(servers []mcpServerCR) []mcpServerCR {
	out := make([]mcpServerCR, 0, len(servers))
	for _, s := range servers {
		c := s
		c.Metadata.Labels = map[string]string{}
		for k, v := range s.Metadata.Labels {
			if k != toolGroupLabel {
				c.Metadata.Labels[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

// decodeMCPServerList parses a Kubernetes list of MCPServer resources.
func decodeMCPServerList(raw []byte) ([]mcpServerCR, error) {
	var list struct {
		Items []mcpServerCR `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("parsing the MCPServer list: %w", err)
	}
	return list.Items, nil
}

// describeGroups renders the partition the way the page lays it out, for the
// proof's output.
func describeGroups(groups map[string][]serverRow) string {
	var b strings.Builder
	for _, g := range toolGroupOrder {
		names := rowNames(groups[g])
		fmt.Fprintf(&b, "    %-19s %d rows: %s\n", toolGroupTitles[g], len(names), strings.Join(names, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

// assertServerGroups judges the partition against the lab's own fixtures —
// the fake-fleet families under Infrastructure, the OAuth fixture under
// Registered servers — and reports the chart-shipped servers: those whose
// chart already stamps the label must sit under the group it names, the
// rest under Registered servers (the fallback). Every server the CR list
// carries must be in exactly one group.
func assertServerGroups(servers []mcpServerCR, groups map[string][]serverRow) error {
	infra := rowNames(groups[groupInfrastructure])
	for _, f := range familyNames() {
		if !slices.Contains(infra, f) {
			return fmt.Errorf("the fake-fleet family %s is not under %s (rows: %v)", f, toolGroupTitles[groupInfrastructure], infra)
		}
	}
	registered := rowNames(groups[groupRegistered])
	if !slices.Contains(registered, oauthFixtureServer) {
		return fmt.Errorf("%s (unlabelled) is not under %s (rows: %v)", oauthFixtureServer, toolGroupTitles[groupRegistered], registered)
	}
	seen := map[string]int{}
	for _, g := range toolGroupOrder {
		for _, row := range groups[g] {
			for _, s := range row.Servers {
				seen[s.name()]++
			}
		}
	}
	for _, s := range servers {
		if seen[s.name()] != 1 {
			return fmt.Errorf("server %s appears in %d groups, wanted exactly one", s.name(), seen[s.name()])
		}
		want := s.toolGroupKey()
		if s.family() != "" {
			continue // families are judged by their members' vote above
		}
		if !slices.Contains(rowNames(groups[want]), s.name()) {
			return fmt.Errorf("server %s carries %s=%q but is not under %s", s.name(), toolGroupLabel, s.Metadata.Labels[toolGroupLabel], toolGroupTitles[want])
		}
	}
	return nil
}
