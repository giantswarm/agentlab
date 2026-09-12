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

// The lab's own shape: the fake fleet (labelled infrastructure, two members
// per family), the chart-shipped managers (agent-platform), the bundled
// mcp-kubernetes (infrastructure), an unlabelled prometheus and the OAuth
// fixture (Registered servers).
func labServers() []mcpServerCR {
	var out []mcpServerCR
	for _, s := range fleetFixtureServers() {
		out = append(out, fakeServer(s.Name, s.Family, toolGroupInfrastructure))
	}
	out = append(out,
		fakeServer("model-manager", "", toolGroupAgentPlatform),
		fakeServer(agentManagerMCPServer, "", toolGroupAgentPlatform),
		fakeServer(componentMCPKubernetes, "", toolGroupInfrastructure),
		fakeServer("mcp-prometheus", "", ""),
		fakeServer(oauthFixtureServer, "", ""),
	)
	return out
}

func TestPartitionServersGroupsByLabelAndFamily(t *testing.T) {
	groups := partitionServers(labServers())
	want := map[string][]string{
		groupAgentPlatform:  {agentManagerMCPServer, "model-manager"},
		groupInfrastructure: {"capi", "kubernetes", "prometheus", componentMCPKubernetes},
		groupRegistered:     {oauthFixtureServer, "mcp-prometheus"},
	}
	for _, g := range toolGroupOrder {
		if got := rowNames(groups[g]); !reflect.DeepEqual(got, want[g]) {
			t.Errorf("%s rows = %v, want %v", g, got, want[g])
		}
	}
	if err := assertServerGroups(labServers(), groups, true); err != nil {
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
	want := []string{"capi", "kubernetes", "prometheus", agentManagerMCPServer, oauthFixtureServer, componentMCPKubernetes, "mcp-prometheus", "model-manager"}
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
	if err := assertServerGroups(servers, partitionServers(servers), true); err == nil {
		t.Fatal("a labelled OAuth fixture must fail the Registered servers assertion")
	}
}

// singleClusterServers is the default lab's shape: labServers() without the
// fake fleet — no family server at all.
func singleClusterServers() []mcpServerCR {
	var out []mcpServerCR
	for _, s := range labServers() {
		if s.family() == "" {
			out = append(out, s)
		}
	}
	return out
}

// TestAssertServerGroupsFollowsFakeFleet: the judgement reads
// platform.fakeFleet — the fleet shape passes only with the key on, the
// single-cluster shape only with it off; each shape under the other key's
// assertion names the fixture.
func TestAssertServerGroupsFollowsFakeFleet(t *testing.T) {
	fleet, single := labServers(), singleClusterServers()
	if err := assertServerGroups(single, partitionServers(single), false); err != nil {
		t.Errorf("the single-cluster shape with the fake fleet off: %v", err)
	}
	if got := rowNames(partitionServers(single)[groupInfrastructure]); !reflect.DeepEqual(got, []string{componentMCPKubernetes}) {
		t.Errorf("Infrastructure rows without the fleet = %v, want the bundled mcp-kubernetes only", got)
	}
	if err := assertServerGroups(fleet, partitionServers(fleet), false); err == nil || !strings.Contains(err.Error(), "platform.fakeFleet is off") {
		t.Errorf("fleet members with the fake fleet off must fail naming the key, got %v", err)
	}
	if err := assertServerGroups(single, partitionServers(single), true); err == nil || !strings.Contains(err.Error(), "fake-fleet family") {
		t.Errorf("no fleet with the fake fleet on must fail naming the missing family, got %v", err)
	}
}
