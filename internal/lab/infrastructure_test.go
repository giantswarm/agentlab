package lab

import (
	"context"
	"maps"
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

// member is a family member the way the charts render it.
func member(cfg *config.Config, name, family string) labelledServer {
	return labelledServer{Namespace: platformNamespace, Name: name, Family: family, InstanceArg: familyInstanceArg, Labels: map[string]string{
		managedByLabel: helmManaged, managementClusterLabel: cfg.ClusterName, toolGroupLabel: toolGroupInfrastructure}}
}

// labMembers is the lab's family members of cfg.
func labMembers(cfg *config.Config) []labelledServer {
	var out []labelledServer
	for _, name := range slices.Sorted(maps.Keys(labFamilyMembers(cfg))) {
		out = append(out, member(cfg, name, labFamilyMembers(cfg)[name]))
	}
	return out
}

// TestLabFamilyMembers: mcp-kubernetes is always the kubernetes member,
// mcp-prometheus the prometheus member with observability on, both named
// the way agent-platform-mcps names a management cluster's.
func TestLabFamilyMembers(t *testing.T) {
	cfg := config.Default()
	cfg.ClusterName = "agentlab-2"
	cfg.Platform.Observability = true
	want := map[string]string{"agentlab-2-mcp-kubernetes": familyKubernetes, "agentlab-2-mcp-prometheus": familyPrometheus}
	if got := labFamilyMembers(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
	cfg.Platform.Observability = false
	if got := labFamilies(cfg); !slices.Equal(got, []string{familyKubernetes}) {
		t.Errorf("families without observability = %v, want kubernetes", got)
	}
	if got := familyArgs(cfg, familyKubernetes, map[string]any{"resourceType": "pods"}); !reflect.DeepEqual(got, map[string]any{familyInstanceArg: "agentlab-2-mcp-kubernetes", "resourceType": "pods"}) {
		t.Errorf("familyArgs = %v", got)
	}
	if got := familyTool(familyKubernetes, "list"); got != "x_kubernetes_list" {
		t.Errorf("familyTool = %s", got)
	}
}

// TestPlatformValuesRegisterTheFamilies: the lab's values make the bundled
// mcp-kubernetes the kubernetes member of its cluster and register
// mcp-prometheus as the prometheus member (no name: the chart's
// <cluster>-mcp-prometheus); the 3.x line keeps its singleton.
func TestPlatformValuesRegisterTheFamilies(t *testing.T) {
	t.Setenv(GitHubTokenEnv, "")
	cfg := config.Default()
	cfg.Platform.Observability = true
	out, err := renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		MCPKubernetes struct {
			MCPServer map[string]any `yaml:"mcpServer"`
		} `yaml:"mcp-kubernetes"`
		MCPS struct {
			MCPServers []map[string]any `yaml:"mcpServers"`
		} `yaml:"agent-platform-mcps"`
	}
	if err := yaml.Unmarshal(out, &values); err != nil {
		t.Fatal(err)
	}
	if got := values.MCPKubernetes.MCPServer; !reflect.DeepEqual(got, map[string]any{"managementCluster": cfg.ClusterName}) {
		t.Errorf("mcp-kubernetes.mcpServer = %v, want managementCluster %s", got, cfg.ClusterName)
	}
	if len(values.MCPS.MCPServers) != 1 {
		t.Fatalf("agent-platform-mcps.mcpServers = %v, want the prometheus member", values.MCPS.MCPServers)
	}
	prom := values.MCPS.MCPServers[0]
	if _, named := prom["name"]; named || prom["cluster"] != cfg.ClusterName || prom["group"] != familyPrometheus {
		t.Errorf("the prometheus entry = %v, want cluster %s, group %s and the chart's name", prom, cfg.ClusterName, familyPrometheus)
	}
}

// TestDemoWorkflowCallsTheFamily: the demo workflow's steps call the
// kubernetes family's tool on the lab's cluster.
func TestDemoWorkflowCallsTheFamily(t *testing.T) {
	cfg := config.Default()
	out, err := renderTemplate(cfg, "demo-workflow.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Spec struct {
			Steps []struct {
				Tool string         `yaml:"tool"`
				Args map[string]any `yaml:"args"`
			} `yaml:"steps"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Spec.Steps) == 0 {
		t.Fatal("no steps")
	}
	for _, s := range wf.Spec.Steps {
		if s.Tool != "x_kubernetes_list" || s.Args[familyInstanceArg] != cfg.MCPServerName() {
			t.Errorf("step %s %v, want x_kubernetes_list with %s=%s", s.Tool, s.Args, familyInstanceArg, cfg.MCPServerName())
		}
	}
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

// TestCheckToolGroupLabels covers the platform-test judgement: the lab's
// family members complete and shaped as an installation's, no family-less
// mcp-kubernetes, no retired fixture member, chart-labelled servers
// reported, the contract's two values only, the OAuth fixture unlabelled.
func TestCheckToolGroupLabels(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Observability = true
	oauth := labelledServer{Namespace: platformNamespace, Name: oauthFixtureServer,
		Labels: map[string]string{managedByLabel: managedByAgentlabValue, fixtureLabel: oauthFixtureMarker}}
	helm := func(name, group string) labelledServer {
		return labelledServer{Namespace: platformNamespace, Name: name, Labels: map[string]string{managedByLabel: helmManaged, toolGroupLabel: group}}
	}
	check := func(all []labelledServer) ([]string, error) {
		var labelled []labelledServer
		for _, s := range all {
			if _, ok := s.Labels[toolGroupLabel]; ok {
				labelled = append(labelled, s)
			}
		}
		return checkToolGroupLabels(cfg, append(all, oauth), labelled, oauth)
	}
	mustFail := func(t *testing.T, all []labelledServer, want string) {
		t.Helper()
		if _, err := check(all); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want an error naming %q, got %v", want, err)
		}
	}

	t.Run("the lab's members, the charts' labels reported", func(t *testing.T) {
		chart, err := check(append(labMembers(cfg), helm(agentManagerMCPServer, toolGroupAgentPlatform)))
		if err != nil || !slices.Equal(chart, []string{platformNamespace + "/" + agentManagerMCPServer + "=agent-platform"}) {
			t.Fatalf("want ok with agent-manager reported, got %v, %v", chart, err)
		}
	})
	t.Run("a missing member fails", func(t *testing.T) {
		mustFail(t, labMembers(cfg)[1:], labMembers(cfg)[0].Name)
	})
	t.Run("the family-less mcp-kubernetes fails", func(t *testing.T) {
		mustFail(t, append(labMembers(cfg), helm(componentMCPKubernetes, toolGroupInfrastructure)), "family-less")
	})
	t.Run("a retired fake-fleet member fails", func(t *testing.T) {
		stale := member(cfg, "kubernetes-lab-01", familyKubernetes)
		stale.Labels[fixtureLabel] = retiredFixtureValue
		mustFail(t, append(labMembers(cfg), stale), "retired fake-fleet member")
	})
	t.Run("a member without its family block fails", func(t *testing.T) {
		all := labMembers(cfg)
		all[0].Family = ""
		mustFail(t, all, "declares family")
	})
	t.Run("a member of another cluster fails", func(t *testing.T) {
		all := labMembers(cfg)
		all[0].Labels[managementClusterLabel] = "elsewhere"
		mustFail(t, all, managementClusterLabel)
	})
	t.Run("a member without the infrastructure label fails", func(t *testing.T) {
		all := labMembers(cfg)
		delete(all[0].Labels, toolGroupLabel)
		mustFail(t, all, "want "+toolGroupInfrastructure)
	})
	t.Run("an unknown value fails", func(t *testing.T) {
		mustFail(t, append(labMembers(cfg), helm("pro", "other")), `"other"`)
	})
	t.Run("a lab-created labelled server fails", func(t *testing.T) {
		stray := labelledServer{Namespace: platformNamespace, Name: vmManagerMCPServer, Labels: map[string]string{
			managedByLabel: managedByAgentlabValue, toolGroupLabel: toolGroupAgentPlatform}}
		mustFail(t, append(labMembers(cfg), stray), vmManagerMCPServer)
	})
	t.Run("a labelled OAuth fixture fails", func(t *testing.T) {
		labelledOAuth := oauth
		labelledOAuth.Labels = map[string]string{managedByLabel: managedByAgentlabValue, toolGroupLabel: toolGroupInfrastructure}
		if _, err := checkToolGroupLabels(cfg, labMembers(cfg), labMembers(cfg), labelledOAuth); err == nil || !strings.Contains(err.Error(), oauthFixtureServer) {
			t.Fatalf("want the labelled OAuth fixture refused, got %v", err)
		}
	})
}

// memberObject is a family member as the apiserver returns it.
func memberObject(cfg *config.Config, name, family string) *unstructured.Unstructured {
	obj := customObject(musterMCPServerGVK, platformNamespace, name, map[string]string{
		managedByLabel: helmManaged, managementClusterLabel: cfg.ClusterName, toolGroupLabel: toolGroupInfrastructure})
	obj.Object["spec"] = map[string]any{"family": map[string]any{nameKey: family, "instanceArg": familyInstanceArg}}
	return obj
}

// labOnCluster seeds what `agentlab platform` leaves on a lab: the family
// members, the OAuth fixture and — in another namespace — a server of the
// other group; retired adds two fake-fleet members of an earlier release.
func labOnCluster(cfg *config.Config, retired bool) []runtime.Object {
	var objs []runtime.Object
	for name, family := range labFamilyMembers(cfg) {
		objs = append(objs, memberObject(cfg, name, family))
	}
	if retired {
		for _, name := range []string{"kubernetes-lab-01", "prometheus-lab-02"} {
			objs = append(objs, customObject(musterMCPServerGVK, platformNamespace, name, map[string]string{
				managedByLabel: managedByAgentlabValue, fixtureLabel: retiredFixtureValue, toolGroupLabel: toolGroupInfrastructure}))
		}
	}
	return append(objs,
		customObject(musterMCPServerGVK, platformNamespace, oauthFixtureServer, map[string]string{managedByLabel: managedByAgentlabValue, fixtureLabel: oauthFixtureMarker}),
		customObject(musterMCPServerGVK, "elsewhere", "pro", map[string]string{managedByLabel: helmManaged, toolGroupLabel: toolGroupAgentPlatform}),
	)
}

// TestProveToolGroupLabelsOnCluster: the platform-test step passes on what
// `agentlab platform` leaves behind, fails once the OAuth fixture is
// missing, and the map the presets proof reads carries the families' label
// under the family's name, the way muster reports a family tool.
func TestProveToolGroupLabelsOnCluster(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.Observability = true
	f := newFakeLab(t, labOnCluster(cfg, false)...)
	if err := proveToolGroupLabels(cfg); err != nil {
		t.Errorf("on a complete lab: %v", err)
	}
	groups, err := mcpServerToolGroups()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{oauthFixtureServer: "", cfg.MCPServerName(): toolGroupInfrastructure, cfg.PrometheusMCPServerName(): toolGroupInfrastructure,
		familyKubernetes: toolGroupInfrastructure, familyPrometheus: toolGroupInfrastructure}
	if !reflect.DeepEqual(groups, want) {
		t.Errorf("tool groups = %v, want %v", groups, want)
	}
	if err := f.dyn.Tracker().Delete(musterMCPServerGVR, platformNamespace, oauthFixtureServer); err != nil {
		t.Fatal(err)
	}
	if err := proveToolGroupLabels(cfg); err == nil || !strings.Contains(err.Error(), oauthFixtureServer+" is missing") {
		t.Errorf("without the OAuth fixture: %v", err)
	}
}

// TestRemoveRetiredFixture: `agentlab platform` removes exactly the retired
// fake-fleet members — the family members and the OAuth fixture stay — and a
// lab without them is a no-op; the proof then passes.
func TestRemoveRetiredFixture(t *testing.T) {
	cfg := config.Default()
	newFakeLab(t, labOnCluster(cfg, true)...)
	if err := proveToolGroupLabels(cfg); err == nil || !strings.Contains(err.Error(), "retired fake-fleet member") {
		t.Fatalf("a lab with retired members must fail the proof, got %v", err)
	}
	ctx := context.Background()
	for round := 1; round <= 2; round++ {
		if err := removeRetiredFixture(ctx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		left, err := listMCPServers(ctx, platformNamespace, fixtureLabel+"="+retiredFixtureValue)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("round %d: %d retired members remain", round, len(left))
		}
	}
	for _, name := range []string{oauthFixtureServer, cfg.MCPServerName()} {
		if _, err := getMCPServer(ctx, platformNamespace, name); err != nil {
			t.Errorf("%s must survive the removal: %v", name, err)
		}
	}
	if err := proveToolGroupLabels(cfg); err != nil {
		t.Errorf("after the removal: %v", err)
	}
	if _, err := getMCPServer(ctx, platformNamespace, "absent"); !apierrors.IsNotFound(err) {
		t.Errorf("a missing server: %v, want the apiserver's NotFound", err)
	}
}

// TestLabelledServerOf reads namespace, name, labels and the family block off
// the object as the apiserver returns it.
func TestLabelledServerOf(t *testing.T) {
	cfg := config.Default()
	got := labelledServerOf(memberObject(cfg, "x", familyKubernetes))
	if got.key() != platformNamespace+"/x" || got.Family != familyKubernetes || got.InstanceArg != familyInstanceArg || got.Labels[toolGroupLabel] != toolGroupInfrastructure {
		t.Errorf("labelledServerOf = %+v", got)
	}
	bare := labelledServerOf(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{nameKey: "bare"}}})
	if bare.Name != "bare" || bare.Namespace != "" || bare.Family != "" || bare.Labels[toolGroupLabel] != "" {
		t.Errorf("a bare object = %+v", bare)
	}
}
