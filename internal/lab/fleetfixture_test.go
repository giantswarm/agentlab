package lab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agentlab/internal/config"
)

// The fixture-label value of the OAuth fixture and the managed-by value of a
// chart-shipped server, as the tests seed them.
const (
	oauthFixtureMarker = "oauth-sign-in"
	helmManaged        = "Helm"
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
			fieldNamespace:            {cr.Metadata.Namespace, platformNamespace},
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
		Labels: map[string]string{managedByLabel: managedByAgentlabValue, fleetFixtureLabel: oauthFixtureMarker}}
	helm := func(name, group string) labelledServer {
		return labelledServer{Namespace: platformNamespace, Name: name, Labels: map[string]string{managedByLabel: helmManaged, toolGroupLabel: group}}
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

// fakeFleetOnCluster seeds what `agentlab platform` leaves on a lab: every
// fake-fleet member, the OAuth fixture, a chart-labelled server, and — in
// another namespace — a server of the other group.
func fakeFleetOnCluster() []runtime.Object {
	var objs []runtime.Object
	for _, name := range fleetFixtureNames() {
		objs = append(objs, customObject(musterMCPServerGVK, platformNamespace, name, map[string]string{
			managedByLabel: managedByAgentlabValue, fleetFixtureLabel: fleetFixtureValue, toolGroupLabel: toolGroupInfrastructure}))
	}
	objs = append(objs,
		customObject(musterMCPServerGVK, platformNamespace, oauthFixtureServer, map[string]string{managedByLabel: managedByAgentlabValue, fleetFixtureLabel: oauthFixtureMarker}),
		customObject(musterMCPServerGVK, platformNamespace, componentMCPKubernetes, map[string]string{managedByLabel: helmManaged, toolGroupLabel: toolGroupInfrastructure}),
		customObject(musterMCPServerGVK, "elsewhere", "pro", map[string]string{managedByLabel: helmManaged, toolGroupLabel: toolGroupAgentPlatform}),
	)
	return objs
}

// TestListMCPServersByLabel: the tool-group listing is a label selector
// across every namespace, the fixture's a selector in the platform
// namespace; a single server reads by name and a missing one is the
// apiserver's NotFound.
func TestListMCPServersByLabel(t *testing.T) {
	newFakeLab(t, fakeFleetOnCluster()...)
	ctx := context.Background()
	labelled, err := listMCPServers(ctx, "", toolGroupLabel)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range labelled {
		keys = append(keys, s.key())
	}
	slices.Sort(keys)
	want := []string{"elsewhere/pro", platformNamespace + "/" + componentMCPKubernetes}
	for _, name := range fleetFixtureNames() {
		want = append(want, platformNamespace+"/"+name)
	}
	slices.Sort(want)
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("labelled = %v, want %v", keys, want)
	}
	members, err := listMCPServers(ctx, platformNamespace, fleetFixtureLabel+"="+fleetFixtureValue)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != len(fleetFixtureNames()) {
		t.Errorf("the fixture selector lists %d servers, want %d", len(members), len(fleetFixtureNames()))
	}
	oauth, err := getMCPServer(ctx, platformNamespace, oauthFixtureServer)
	if err != nil || oauth.Labels[fleetFixtureLabel] != oauthFixtureMarker {
		t.Errorf("getMCPServer = %+v, %v", oauth, err)
	}
	if _, err := getMCPServer(ctx, platformNamespace, "absent"); !apierrors.IsNotFound(err) {
		t.Errorf("a missing server: %v, want the apiserver's NotFound", err)
	}
}

// TestProveToolGroupLabelsOnCluster: the platform-test step passes on what
// `agentlab platform` leaves behind (the chart-labelled server reported, not
// judged), fails once the OAuth fixture is missing, and the per-server map
// the presets proof reads carries "" for the unlabelled fixture.
func TestProveToolGroupLabelsOnCluster(t *testing.T) {
	f := newFakeLab(t, fakeFleetOnCluster()...)
	if err := proveToolGroupLabels(); err != nil {
		t.Errorf("on a complete lab: %v", err)
	}
	groups, err := mcpServerToolGroups()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{oauthFixtureServer: "", componentMCPKubernetes: toolGroupInfrastructure}
	for _, name := range fleetFixtureNames() {
		want[name] = toolGroupInfrastructure
	}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("tool groups = %v, want %v", groups, want)
	}
	if err := f.dyn.Tracker().Delete(musterMCPServerGVR, platformNamespace, oauthFixtureServer); err != nil {
		t.Fatal(err)
	}
	if err := proveToolGroupLabels(); err == nil || !strings.Contains(err.Error(), oauthFixtureServer+" is missing") {
		t.Errorf("without the OAuth fixture: %v", err)
	}
}

// TestLabelledServerOf reads namespace, name and labels off the object as the
// apiserver returns it; no labels is an empty map's worth of lookups.
func TestLabelledServerOf(t *testing.T) {
	got := labelledServerOf(customObject(musterMCPServerGVK, platformNamespace, "x", map[string]string{toolGroupLabel: toolGroupAgentPlatform}))
	if got.key() != platformNamespace+"/x" || got.Labels[toolGroupLabel] != toolGroupAgentPlatform {
		t.Errorf("labelledServerOf = %+v", got)
	}
	bare := labelledServerOf(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{nameKey: "bare"}}})
	if bare.Name != "bare" || bare.Namespace != "" || bare.Labels[toolGroupLabel] != "" {
		t.Errorf("a bare object = %+v", bare)
	}
}
