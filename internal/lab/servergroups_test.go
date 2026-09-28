package lab

import (
	"reflect"
	"strings"
	"testing"
)

func fakeServer(name, family, group string) mcpServerCR {
	var s mcpServerCR
	s.Metadata.Name = name
	s.Metadata.Namespace = platformNamespace
	s.Metadata.Labels = map[string]string{}
	if group != "" {
		s.Metadata.Labels[toolGroupLabel] = group
	}
	if family != "" {
		s.Spec.Family = &struct {
			Name string `json:"name"`
		}{Name: family}
	}
	return s
}

// The lab's own shape: its cluster as the kubernetes and prometheus
// families' member (labelled infrastructure), the chart-shipped managers
// (agent-platform) and the OAuth fixture (Registered servers).
func labServers() []mcpServerCR {
	return []mcpServerCR{
		fakeServer("agentlab-mcp-kubernetes", familyKubernetes, toolGroupInfrastructure),
		fakeServer("agentlab-mcp-prometheus", familyPrometheus, toolGroupInfrastructure),
		fakeServer("model-manager", "", toolGroupAgentPlatform),
		fakeServer(agentManagerMCPServer, "", toolGroupAgentPlatform),
		fakeServer(oauthFixtureServer, "", ""),
	}
}

var labFamilyRows = []string{familyKubernetes, familyPrometheus}

func TestPartitionServersGroupsByLabelAndFamily(t *testing.T) {
	groups := partitionServers(labServers())
	want := map[string][]string{
		groupAgentPlatform:  {agentManagerMCPServer, "model-manager"},
		groupInfrastructure: labFamilyRows,
		groupRegistered:     {oauthFixtureServer},
	}
	for _, g := range toolGroupOrder {
		if got := rowNames(groups[g]); !reflect.DeepEqual(got, want[g]) {
			t.Errorf("%s rows = %v, want %v", g, got, want[g])
		}
	}
	if err := assertServerGroups(labServers(), groups, labFamilyRows); err != nil {
		t.Fatalf("assertServerGroups: %v", err)
	}
}

func TestPartitionServersFallbackWithoutLabels(t *testing.T) {
	groups := partitionServers(stripToolGroupLabels(labServers()))
	if len(groups) != len(toolGroupOrder) {
		t.Fatalf("every group must be present, got %d", len(groups))
	}
	if n := len(groups[groupAgentPlatform]) + len(groups[groupInfrastructure]); n != 0 {
		t.Errorf("unlabelled servers landed outside Registered servers: %d rows", n)
	}
	want := []string{familyKubernetes, familyPrometheus, agentManagerMCPServer, oauthFixtureServer, "model-manager"}
	if got := rowNames(groups[groupRegistered]); !reflect.DeepEqual(got, want) {
		t.Errorf("Registered rows = %v, want %v", got, want)
	}
}

func TestFamilyToolGroupVotes(t *testing.T) {
	cases := []struct {
		name    string
		members []mcpServerCR
		want    string
	}{
		{"unlabelled family", []mcpServerCR{fakeServer("a-1", "a", ""), fakeServer("a-2", "a", "")}, groupRegistered},
		{"rollout in flight: one labelled member decides", []mcpServerCR{fakeServer("a-1", "a", ""), fakeServer("a-2", "a", toolGroupInfrastructure)}, groupInfrastructure},
		{"majority wins", []mcpServerCR{fakeServer("a-1", "a", toolGroupAgentPlatform), fakeServer("a-2", "a", toolGroupInfrastructure), fakeServer("a-3", "a", toolGroupInfrastructure)}, groupInfrastructure},
		{"tie breaks by display order", []mcpServerCR{fakeServer("a-1", "a", toolGroupInfrastructure), fakeServer("a-2", "a", toolGroupAgentPlatform)}, groupAgentPlatform},
		{"unknown label value is no label", []mcpServerCR{fakeServer("a-1", "a", "other")}, groupRegistered},
	}
	for _, tc := range cases {
		if got := familyToolGroup(tc.members); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestAssertServerGroupsCatchesMisplacedFixture(t *testing.T) {
	servers := labServers()
	// The OAuth fixture wrongly labelled: the fixture contract says unlabelled.
	for i := range servers {
		if servers[i].name() == oauthFixtureServer {
			servers[i].Metadata.Labels[toolGroupLabel] = toolGroupAgentPlatform
		}
	}
	if err := assertServerGroups(servers, partitionServers(servers), labFamilyRows); err == nil {
		t.Fatal("a labelled OAuth fixture must fail the Registered servers assertion")
	}
}

// TestAssertServerGroupsWantsTheFamilies: a lab without one of its families
// fails naming it, and a family-less mcp-kubernetes row fails wherever it
// sits.
func TestAssertServerGroupsWantsTheFamilies(t *testing.T) {
	servers := labServers()
	if err := assertServerGroups(servers[1:], partitionServers(servers[1:]), labFamilyRows); err == nil || !strings.Contains(err.Error(), "the kubernetes family") {
		t.Errorf("a missing family must fail naming it, got %v", err)
	}
	withSingleton := append(labServers(), fakeServer(componentMCPKubernetes, "", toolGroupInfrastructure))
	if err := assertServerGroups(withSingleton, partitionServers(withSingleton), labFamilyRows); err == nil || !strings.Contains(err.Error(), "family-less") {
		t.Errorf("a family-less mcp-kubernetes must fail, got %v", err)
	}
}
