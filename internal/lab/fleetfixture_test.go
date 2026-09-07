package lab

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// fixtureCR is the shape of one rendered MCPServer the tests decode into.
type fixtureCR struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Type      string `yaml:"type"`
		URL       string `yaml:"url"`
		AutoStart bool   `yaml:"autoStart"`
		Family    struct {
			Name        string `yaml:"name"`
			InstanceArg string `yaml:"instanceArg"`
		} `yaml:"family"`
		Auth struct {
			Type         string `yaml:"type"`
			ForwardToken bool   `yaml:"forwardToken"`
		} `yaml:"auth"`
	} `yaml:"spec"`
}

func decodeCRs(t *testing.T, raw []byte) []fixtureCR {
	t.Helper()
	var out []fixtureCR
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var cr fixtureCR
		err := dec.Decode(&cr)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding rendered fixture: %v\n%s", err, raw)
		}
		if cr.Kind == "" {
			continue // the leading comment-only document
		}
		out = append(out, cr)
	}
}

// TestFleetFixtureTemplate pins the fake fleet to what its consumers read:
// one MCPServer per family per fake cluster in the platform namespace, the
// family block with muster's management_cluster argument, the
// management-cluster label naming the member's cluster, the type label the
// fleet charts stamp, the tool-group label with the infrastructure value,
// the lab's fixture markers — and, like the OAuth fixture, muster's own
// protected endpoint as target with an oauth auth block without SSO.
func TestFleetFixtureTemplate(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "fleet-fixture.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	crs := decodeCRs(t, raw)
	want := len(fleetFamilies) * len(fleetFixtureClusters)
	if len(crs) != want {
		t.Fatalf("want %d MCPServers (%d families × %d clusters), got %d:\n%s", want, len(fleetFamilies), len(fleetFixtureClusters), len(crs), raw)
	}
	if got := strings.Count(string(raw), "kind: MCPServer"); got != want {
		t.Errorf("want %d `kind: MCPServer`, got %d", want, got)
	}
	var names []string
	for _, cr := range crs {
		names = append(names, cr.Metadata.Name)
		fam, ok := familyByName(cr.Spec.Family.Name)
		if !ok {
			t.Errorf("%s: family %q is not one of the fleet families", cr.Metadata.Name, cr.Spec.Family.Name)
			continue
		}
		cluster := cr.Metadata.Labels[managementClusterLabel]
		if !slices.Contains(fleetFixtureClusters, cluster) {
			t.Errorf("%s: %s=%q is not a fake cluster", cr.Metadata.Name, managementClusterLabel, cluster)
		}
		checks := map[string][2]string{
			"apiVersion":              {cr.APIVersion, "muster.giantswarm.io/v1alpha1"},
			"kind":                    {cr.Kind, "MCPServer"},
			"name":                    {cr.Metadata.Name, fam.Name + "-" + cluster},
			"namespace":               {cr.Metadata.Namespace, platformNamespace},
			managedByLabel:            {cr.Metadata.Labels[managedByLabel], managedByAgentlabValue},
			fleetFixtureLabel:         {cr.Metadata.Labels[fleetFixtureLabel], fleetFixtureValue},
			"muster type label":       {cr.Metadata.Labels["muster.giantswarm.io/type"], fam.Type},
			toolGroupLabel:            {cr.Metadata.Labels[toolGroupLabel], toolGroupInfrastructure},
			"spec.type":               {cr.Spec.Type, "streamable-http"},
			"spec.url":                {cr.Spec.URL, oauthFixtureURL},
			"spec.family.instanceArg": {cr.Spec.Family.InstanceArg, familyInstanceArg},
			"spec.auth.type":          {cr.Spec.Auth.Type, "oauth"},
		}
		for what, gw := range checks {
			if gw[0] != gw[1] {
				t.Errorf("%s: %s = %q, want %q", cr.Metadata.Name, what, gw[0], gw[1])
			}
		}
		if !cr.Spec.AutoStart {
			t.Errorf("%s: autoStart must be true", cr.Metadata.Name)
		}
		if cr.Spec.Auth.ForwardToken {
			t.Errorf("%s: forwardToken must be off — the member is a manual-login oauth server, never SSO", cr.Metadata.Name)
		}
		if cr.Metadata.Annotations["agentlab.giantswarm.io/purpose"] == "" {
			t.Errorf("%s: the purpose annotation is missing", cr.Metadata.Name)
		}
	}
	slices.Sort(names)
	if got := fleetFixtureNames(); !slices.Equal(names, got) {
		t.Errorf("rendered names %v differ from fleetFixtureNames() %v", names, got)
	}
	if strings.Contains(string(raw), "tokenExchange") {
		t.Errorf("the fixture must not be an SSO server (token exchange):\n%s", raw)
	}
}

func familyByName(name string) (fleetFamily, bool) {
	for _, f := range fleetFamilies {
		if f.Name == name {
			return f, true
		}
	}
	return fleetFamily{}, false
}

// TestOAuthFixtureCarriesNoToolGroup: the OAuth fixture is the lab's
// Registered server — the one group the label marks by absence.
func TestOAuthFixtureCarriesNoToolGroup(t *testing.T) {
	raw, err := renderTemplate(config.Default(), "oauth-fixture.yaml.tmpl", nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(raw), toolGroupLabel) {
		t.Errorf("the OAuth fixture must not carry %s:\n%s", toolGroupLabel, raw)
	}
}

// TestCheckToolGroupLabels covers the platform-test judgement: exactly the
// fixture among the lab's own CRs, chart-labelled servers reported, the
// contract's two values only, the OAuth fixture unlabelled.
func TestCheckToolGroupLabels(t *testing.T) {
	member := func(name string) labelledServer {
		return labelledServer{Namespace: platformNamespace, Name: name, Labels: map[string]string{
			managedByLabel: managedByAgentlabValue, fleetFixtureLabel: fleetFixtureValue, toolGroupLabel: toolGroupInfrastructure}}
	}
	fixture := func() []labelledServer {
		var out []labelledServer
		for _, name := range fleetFixtureNames() {
			out = append(out, member(name))
		}
		return out
	}
	oauth := labelledServer{Namespace: platformNamespace, Name: oauthFixtureServer,
		Labels: map[string]string{managedByLabel: managedByAgentlabValue, fleetFixtureLabel: "oauth-sign-in"}}
	helm := func(name, group string) labelledServer {
		return labelledServer{Namespace: platformNamespace, Name: name, Labels: map[string]string{managedByLabel: "Helm", toolGroupLabel: group}}
	}

	t.Run("exactly the fixture", func(t *testing.T) {
		chart, err := checkToolGroupLabels(fixture(), oauth)
		if err != nil || len(chart) != 0 {
			t.Fatalf("want ok and no chart-labelled servers, got %v, %v", chart, err)
		}
	})
	t.Run("chart-labelled servers are reported, not judged", func(t *testing.T) {
		all := append(fixture(), helm("mcp-kubernetes", toolGroupInfrastructure), helm("agent-manager", toolGroupAgentPlatform))
		chart, err := checkToolGroupLabels(all, oauth)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{platformNamespace + "/agent-manager=agent-platform", platformNamespace + "/mcp-kubernetes=infrastructure"}
		if !slices.Equal(chart, want) {
			t.Fatalf("want %v, got %v", want, chart)
		}
	})
	t.Run("a missing member fails", func(t *testing.T) {
		all := fixture()[1:]
		if _, err := checkToolGroupLabels(all, oauth); err == nil || !strings.Contains(err.Error(), fleetFixtureNames()[0]) {
			t.Fatalf("want the missing member named, got %v", err)
		}
	})
	t.Run("a member with the wrong value fails", func(t *testing.T) {
		all := fixture()
		all[0].Labels[toolGroupLabel] = toolGroupAgentPlatform
		if _, err := checkToolGroupLabels(all, oauth); err == nil || !strings.Contains(err.Error(), "want "+toolGroupInfrastructure) {
			t.Fatalf("want the wrong value refused, got %v", err)
		}
	})
	t.Run("an unknown value fails", func(t *testing.T) {
		all := append(fixture(), helm("pro", "other"))
		if _, err := checkToolGroupLabels(all, oauth); err == nil || !strings.Contains(err.Error(), `"other"`) {
			t.Fatalf("want the unknown value refused, got %v", err)
		}
	})
	t.Run("another lab-created labelled server fails", func(t *testing.T) {
		stray := member("stray")
		delete(stray.Labels, fleetFixtureLabel)
		if _, err := checkToolGroupLabels(append(fixture(), stray), oauth); err == nil || !strings.Contains(err.Error(), "stray") {
			t.Fatalf("want the stray lab server refused, got %v", err)
		}
	})
	t.Run("a labelled OAuth fixture fails", func(t *testing.T) {
		labelledOAuth := oauth
		labelledOAuth.Labels = map[string]string{managedByLabel: managedByAgentlabValue, toolGroupLabel: toolGroupInfrastructure}
		if _, err := checkToolGroupLabels(fixture(), labelledOAuth); err == nil || !strings.Contains(err.Error(), oauthFixtureServer) {
			t.Fatalf("want the labelled OAuth fixture refused, got %v", err)
		}
	})
}
