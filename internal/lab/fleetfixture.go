package lab

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fake-fleet fixture.
//
// A real installation federates many management clusters: agent-platform-mcps
// renders one MCPServer per cluster and per infrastructure family
// (kubernetes, capi, prometheus), each declaring spec.family so muster exposes
// the family once (x_kubernetes_<tool> with a management_cluster argument)
// and labelled with the cluster it serves. The lab has one cluster and its
// bundled mcp-kubernetes declares no family, so nothing in the lab looks like
// a fleet — yet the portal's MCP servers page, its dashboard's fleet coverage
// and the agent Tools step group servers by family and by the tool-group
// label. This fixture fakes the fleet: fleetFixtureClusters × fleetFamilies
// MCPServers in the platform namespace, each with the family block, the
// management-cluster label and the tool-group label the fleet charts stamp.
//
// The label. agent-platform.giantswarm.io/tool-group sorts MCP servers into
// the platform's three groups — agent-platform (agent-manager, model-manager,
// muster core), infrastructure (the kubernetes/capi/prometheus families) and,
// for a server without the label, Registered servers (whatever an
// installation or a user registers). It is stamped by the chart that ships
// the server and read by the portal (grouping), muster (the shipped
// `infrastructure` / `agent-platform` toolset presets select by it) and
// kubectl -l. Orientation and preset membership, not authorization. The
// fixture carries `infrastructure`; the OAuth sign-in fixture
// (oauthfixture.go) and anything registered by hand carry nothing, so the
// Registered servers group has members too. The charts the lab vendors
// (agent-manager, model-manager, the umbrella's mcp-kubernetes) bring their
// own labels once bumped to the releases that stamp them.
//
// Where the members point. Like the OAuth fixture, at muster's own protected
// /mcp with auth.type oauth: every member reads Auth Required — the state an
// unconnected forwardToken fleet member shows on a real installation — at
// zero cost. Pointing them at the lab's single mcp-kubernetes instead would
// make muster open one connection per member per user session at session
// start; a dozen such members rate-limited mcp-kubernetes (429) and took the
// real mcp-kubernetes connection down with them. The fixture exists to be
// grouped, listed and selected, not to be called.

const (
	// fleetFixtureLabel marks the fixture's CRs for cleanup and for the
	// proofs; its value is the fixture's name.
	fleetFixtureLabel = "agentlab.giantswarm.io/fixture"
	fleetFixtureValue = "fake-fleet"
	// managedByLabel/managedByAgentlabValue mark every CR the lab itself creates
	// (as opposed to the vendored charts).
	managedByLabel         = "app.kubernetes.io/managed-by"
	managedByAgentlabValue = "agentlab"
	// managementClusterLabel is muster's convention for the cluster a family
	// member serves — the value the family's instanceArg selects.
	managementClusterLabel = "muster.giantswarm.io/management-cluster"
	// familyInstanceArg is the required argument every family tool takes.
	familyInstanceArg = "management_cluster"
	// toolGroupLabel and its two values: the Agent Platform's tiering of MCP
	// servers. Absent means Registered servers.
	toolGroupLabel          = "agent-platform.giantswarm.io/tool-group"
	toolGroupInfrastructure = "infrastructure"
	toolGroupAgentPlatform  = "agent-platform"
)

// fleetFixtureClusters are the fake management clusters. Two is a fleet for
// every consumer (family rows with more than one cell, coverage per cluster)
// at the smallest footprint.
var fleetFixtureClusters = []string{"lab-01", "lab-02"}

// fleetFamily is one infrastructure family the fleet charts ship, with the
// muster.giantswarm.io/type the charts stamp on its members.
type fleetFamily struct {
	Name string
	Type string
}

// fleetFamilies mirrors agent-platform-mcps' muster.families.
var fleetFamilies = []fleetFamily{
	{Name: "kubernetes", Type: "mcp-kubernetes"},
	{Name: "capi", Type: "mcp-capi"},
	{Name: "prometheus", Type: "mcp-observability"},
}

// fleetFixtureServer is one member the template renders.
type fleetFixtureServer struct {
	Name    string
	Family  string
	Type    string
	Cluster string
}

// fleetFixtureServers lists every member, family-major, in render order.
func fleetFixtureServers() []fleetFixtureServer {
	out := make([]fleetFixtureServer, 0, len(fleetFamilies)*len(fleetFixtureClusters))
	for _, f := range fleetFamilies {
		for _, c := range fleetFixtureClusters {
			out = append(out, fleetFixtureServer{Name: f.Name + "-" + c, Family: f.Name, Type: f.Type, Cluster: c})
		}
	}
	return out
}

// fleetFixtureNames is the member names, sorted.
func fleetFixtureNames() []string {
	names := make([]string, 0, len(fleetFamilies)*len(fleetFixtureClusters))
	for _, s := range fleetFixtureServers() {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

// ensureFleetFixture applies the fixture, removes members of an earlier
// shape (a cluster or family dropped from the lists above — the label
// selector must keep listing exactly the fixture) and waits until muster
// reports every member Auth Required. After the umbrella install for the
// same reason as the OAuth fixture: the MCPServer CRD ships with muster.
func ensureFleetFixture(cfg *config.Config) error {
	names := fleetFixtureNames()
	step("Creating the fake-fleet fixture (%d MCPServers: %s × %s)", len(names),
		strings.Join(familyNames(), "/"), strings.Join(fleetFixtureClusters, ", "))
	_, path, err := renderManifest(cfg, "fleet-fixture.yaml.tmpl")
	if err != nil {
		return err
	}
	if err := runQuiet("kubectl", "apply", "-f", path); err != nil {
		return err
	}
	existing, err := outputQuiet("kubectl", "-n", platformNamespace, "get", "mcpservers.muster.giantswarm.io",
		"-l", fleetFixtureLabel+"="+fleetFixtureValue, "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(existing) {
		if !slices.Contains(names, name) {
			note("removing stale fixture member %s", name)
			if err := runQuiet("kubectl", "-n", platformNamespace, "delete", "mcpservers.muster.giantswarm.io", name); err != nil {
				return err
			}
		}
	}
	for _, name := range names {
		if err := waitMCPServerState(name, mcpServerStateAuthRequired); err != nil {
			return err
		}
	}
	return nil
}

func familyNames() []string {
	names := make([]string, 0, len(fleetFamilies))
	for _, f := range fleetFamilies {
		names = append(names, f.Name)
	}
	return names
}

// labelledServer is what the tool-group proof reads off every MCPServer CR
// that carries the label, plus the OAuth fixture.
type labelledServer struct {
	Namespace string
	Name      string
	Labels    map[string]string
}

func (s labelledServer) key() string { return s.Namespace + "/" + s.Name }

// proveToolGroupLabels is the platform-test step for the label: kubectl -l
// with the infrastructure value lists every fake-fleet member and, of the
// lab's own CRs, nothing else; every value in the cluster is one of the two
// the contract knows; the OAuth fixture is unlabelled (a Registered server).
// Servers the vendored charts label (Helm-managed) are reported, not judged:
// they appear as the charts bump to the releases that stamp the label.
func proveToolGroupLabels() error {
	step("Tool-group label: %s=%s selects the fake-fleet fixture", toolGroupLabel, toolGroupInfrastructure)
	labelled, err := listMCPServers("-A", "-l", toolGroupLabel)
	if err != nil {
		return err
	}
	oauth, err := listMCPServers("-n", platformNamespace, "--field-selector", "metadata.name="+oauthFixtureServer)
	if err != nil {
		return err
	}
	if len(oauth) != 1 {
		return fmt.Errorf("MCPServer %s is missing — the sign-in fixture `agentlab platform` creates", oauthFixtureServer)
	}
	chartLabelled, err := checkToolGroupLabels(labelled, oauth[0])
	if err != nil {
		return err
	}
	note("%s=%s: %s", toolGroupLabel, toolGroupInfrastructure, strings.Join(fleetFixtureNames(), ", "))
	if len(chartLabelled) > 0 {
		note("labelled by the vendored charts: %s", strings.Join(chartLabelled, ", "))
	}
	note("%s carries no %s (Registered servers)", oauthFixtureServer, toolGroupLabel)
	return nil
}

// checkToolGroupLabels is the pure half of proveToolGroupLabels: labelled is
// every MCPServer carrying the tool-group label, oauth the OAuth fixture. It
// returns the labelled servers the lab did not create (the charts' own).
func checkToolGroupLabels(labelled []labelledServer, oauth labelledServer) ([]string, error) {
	if v, ok := oauth.Labels[toolGroupLabel]; ok {
		return nil, fmt.Errorf("MCPServer %s carries %s=%s — the OAuth fixture must stay unlabelled so the Registered servers group has a member",
			oauth.Name, toolGroupLabel, v)
	}
	want := map[string]bool{}
	for _, name := range fleetFixtureNames() {
		want[platformNamespace+"/"+name] = false
	}
	var chartLabelled []string
	for _, s := range labelled {
		v := s.Labels[toolGroupLabel]
		if v != toolGroupInfrastructure && v != toolGroupAgentPlatform {
			return nil, fmt.Errorf("MCPServer %s carries %s=%q — the contract knows %s and %s only",
				s.key(), toolGroupLabel, v, toolGroupInfrastructure, toolGroupAgentPlatform)
		}
		if _, isFixture := want[s.key()]; isFixture {
			if v != toolGroupInfrastructure {
				return nil, fmt.Errorf("fake-fleet member %s carries %s=%s, want %s", s.key(), toolGroupLabel, v, toolGroupInfrastructure)
			}
			want[s.key()] = true
			continue
		}
		if s.Labels[managedByLabel] == managedByAgentlabValue {
			return nil, fmt.Errorf("MCPServer %s is lab-created but carries %s=%s — only the fake-fleet fixture is labelled",
				s.key(), toolGroupLabel, v)
		}
		chartLabelled = append(chartLabelled, s.key()+"="+v)
	}
	var missing []string
	for key, seen := range want {
		if !seen {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("fake-fleet members without %s=%s: %s (`agentlab platform` creates them)",
			toolGroupLabel, toolGroupInfrastructure, strings.Join(missing, ", "))
	}
	sort.Strings(chartLabelled)
	return chartLabelled, nil
}

// listMCPServers reads muster MCPServer CRs (fully qualified: kagent ships
// its own mcpservers.kagent.dev) with the given kubectl selectors.
func listMCPServers(selectors ...string) ([]labelledServer, error) {
	args := append([]string{"get", "mcpservers.muster.giantswarm.io", "-o", "json"}, selectors...)
	raw, err := outputQuiet("kubectl", args...)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string            `json:"namespace"`
				Name      string            `json:"name"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("parsing MCPServer list: %w", err)
	}
	out := make([]labelledServer, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, labelledServer{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Labels: it.Metadata.Labels})
	}
	return out, nil
}
