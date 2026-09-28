package lab

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The infrastructure families.
//
// An installation registers the servers for the management clusters the
// platform runs on as members of muster's families (kubernetes, prometheus,
// capi): agent-platform-mcps renders one MCPServer per cluster and family,
// <cluster>-mcp-<family>, with spec.family so muster exposes the family once
// (x_<family>_<tool> with a management_cluster argument), labelled with the
// cluster it serves and with the tool-group label. The lab registers its own
// cluster the same way: the connectivity chart's mcp-kubernetes as the
// kubernetes member (mcp-kubernetes.mcpServer.managementCluster) and, with
// platform.observability, mcp-prometheus as the prometheus member (an
// agent-platform-mcps entry). There is no family-less registration.
//
// The label. agent-platform.giantswarm.io/tool-group sorts MCP servers into
// the platform's three groups — agent-platform (agent-manager, model-manager,
// muster core), infrastructure (the families) and, for a server without the
// label, Registered servers (whatever an installation or a user registers).
// It is stamped by the chart that ships the server and read by the portal
// (grouping), muster (the shipped `infrastructure` / `agent-platform` toolset
// presets select by it) and kubectl -l. Orientation and preset membership,
// not authorization. The OAuth sign-in fixture (oauthfixture.go) and anything
// registered by hand carry nothing, so the Registered servers group has
// members too.

const (
	// managedByLabel/managedByAgentlabValue mark every CR the lab itself creates
	// (as opposed to the vendored charts).
	managedByLabel         = "app.kubernetes.io/managed-by"
	managedByAgentlabValue = "agentlab"
	// managementClusterLabel is muster's convention for the cluster a family
	// member serves (the instance argument selects the member by its name).
	managementClusterLabel = "muster.giantswarm.io/management-cluster"
	// familyInstanceArg is the required argument every family tool takes.
	familyInstanceArg = "management_cluster"
	// toolGroupLabel and its two values: the Agent Platform's tiering of MCP
	// servers. Absent means Registered servers.
	toolGroupLabel          = "agent-platform.giantswarm.io/tool-group"
	toolGroupInfrastructure = "infrastructure"
	toolGroupAgentPlatform  = "agent-platform"
	// The families the lab's servers are members of.
	familyKubernetes = "kubernetes"
	familyPrometheus = "prometheus"
	// fixtureLabel marks the CRs of a lab test fixture; retiredFixtureValue
	// is the fake fleet earlier releases created, which `agentlab platform`
	// removes.
	fixtureLabel        = "agentlab.giantswarm.io/fixture"
	retiredFixtureValue = "fake-fleet"
)

// familyTool is the name muster exposes a family's tool under.
func familyTool(family, tool string) string { return "x_" + family + "_" + tool }

// familyMember is the lab's member of a family — the value of the instance
// argument selecting the lab: muster offers a family's members by their
// MCPServer names (an installation's gazelle-mcp-kubernetes, the lab's
// <clusterName>-mcp-kubernetes).
func familyMember(cfg *config.Config, family string) string {
	return cfg.ClusterName + "-mcp-" + family
}

// familyArgs adds the instance argument selecting the lab's member to a
// family tool's arguments.
func familyArgs(cfg *config.Config, family string, args map[string]any) map[string]any {
	out := map[string]any{familyInstanceArg: familyMember(cfg, family)}
	maps.Copy(out, args)
	return out
}

// labFamilyMembers maps the lab's own family members to their family.
func labFamilyMembers(cfg *config.Config) map[string]string {
	members := map[string]string{familyMember(cfg, familyKubernetes): familyKubernetes}
	if cfg.Platform.Observability {
		members[familyMember(cfg, familyPrometheus)] = familyPrometheus
	}
	return members
}

// labFamilies lists the families the lab's servers are members of, sorted.
func labFamilies(cfg *config.Config) []string {
	return slices.Sorted(maps.Values(labFamilyMembers(cfg)))
}

// removeRetiredFixture deletes the fake-fleet members an earlier release
// created, so a lab made before the families keeps no MCPServer named after a
// cluster that does not exist. A lab that never had them is one step of
// silence.
func removeRetiredFixture(ctx context.Context) error {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	existing, err := listObjects(ctx, gvr, platformNamespace, fixtureLabel+"="+retiredFixtureValue)
	if err != nil {
		return err
	}
	for _, member := range existing {
		note("removing the retired fake-fleet member %s", member.GetName())
		if err := deleteObject(ctx, gvr, platformNamespace, member.GetName(), fixtureDeleteWait); err != nil {
			return err
		}
	}
	return nil
}

// labelledServer is what the tool-group proof reads off an MCPServer CR.
type labelledServer struct {
	Namespace   string
	Name        string
	Labels      map[string]string
	Family      string
	InstanceArg string
}

func (s labelledServer) key() string { return s.Namespace + "/" + s.Name }

// proveToolGroupLabels is the platform-test step for the families and the
// label: the lab's family members carry the infrastructure label, their
// cluster and the family block; no family-less mcp-kubernetes exists; every
// label value in the cluster is one of the two the contract knows; nothing
// the lab creates itself is labelled and the OAuth fixture is a Registered
// server. Servers the vendored charts label are reported, not judged.
func proveToolGroupLabels(cfg *config.Config) error {
	step("Infrastructure families: %s is the member of %s, %s=%s", cfg.ClusterName, strings.Join(labFamilies(cfg), ", "), toolGroupLabel, toolGroupInfrastructure)
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	all, err := listMCPServers(ctx, platformNamespace, "")
	if err != nil {
		return err
	}
	labelled, err := listMCPServers(ctx, "", toolGroupLabel)
	if err != nil {
		return err
	}
	oauth, err := getMCPServer(ctx, platformNamespace, oauthFixtureServer)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("MCPServer %s is missing — the sign-in fixture `agentlab platform` creates", oauthFixtureServer)
	}
	if err != nil {
		return err
	}
	chartLabelled, err := checkToolGroupLabels(cfg, all, labelled, oauth)
	if err != nil {
		return err
	}
	note("family members: %s (%s=%s)", strings.Join(slices.Sorted(maps.Keys(labFamilyMembers(cfg))), ", "), managementClusterLabel, cfg.ClusterName)
	if len(chartLabelled) > 0 {
		note("labelled by the charts: %s", strings.Join(chartLabelled, ", "))
	}
	note("%s carries no %s (Registered servers)", oauthFixtureServer, toolGroupLabel)
	return nil
}

// checkToolGroupLabels is the pure half of proveToolGroupLabels: all is every
// MCPServer of the platform namespace, labelled every one carrying the
// tool-group label, oauth the OAuth fixture. It returns the labelled servers
// that are neither the lab's family members nor lab-created (the charts'
// own).
func checkToolGroupLabels(cfg *config.Config, all, labelled []labelledServer, oauth labelledServer) ([]string, error) {
	if v, ok := oauth.Labels[toolGroupLabel]; ok {
		return nil, fmt.Errorf("MCPServer %s carries %s=%s — the OAuth fixture must stay unlabelled so the Registered servers group has a member",
			oauth.Name, toolGroupLabel, v)
	}
	members := labFamilyMembers(cfg)
	seen := map[string]bool{}
	for _, s := range all {
		if s.Name == componentMCPKubernetes {
			return nil, fmt.Errorf("MCPServer %s is the family-less mcp-kubernetes — the lab registers it as %s, the %s family's member, only", s.key(), cfg.MCPServerName(), familyKubernetes)
		}
		if s.Labels[fixtureLabel] == retiredFixtureValue {
			return nil, fmt.Errorf("MCPServer %s is a retired fake-fleet member (`agentlab platform` removes it)", s.key())
		}
		family, ok := members[s.Name]
		if !ok {
			continue
		}
		seen[s.Name] = true
		switch {
		case s.Family != family || s.InstanceArg != familyInstanceArg:
			return nil, fmt.Errorf("MCPServer %s declares family %q (instanceArg %q), want %s (%s)", s.key(), s.Family, s.InstanceArg, family, familyInstanceArg)
		case s.Labels[managementClusterLabel] != cfg.ClusterName:
			return nil, fmt.Errorf("MCPServer %s carries %s=%q, want %s", s.key(), managementClusterLabel, s.Labels[managementClusterLabel], cfg.ClusterName)
		case s.Labels[toolGroupLabel] != toolGroupInfrastructure:
			return nil, fmt.Errorf("MCPServer %s carries %s=%q, want %s", s.key(), toolGroupLabel, s.Labels[toolGroupLabel], toolGroupInfrastructure)
		}
	}
	var missing []string
	for name := range members {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("the lab's family members are missing: %s (`agentlab platform` registers them)", strings.Join(missing, ", "))
	}
	var chartLabelled []string
	for _, s := range labelled {
		v := s.Labels[toolGroupLabel]
		if v != toolGroupInfrastructure && v != toolGroupAgentPlatform {
			return nil, fmt.Errorf("MCPServer %s carries %s=%q — the contract knows %s and %s only",
				s.key(), toolGroupLabel, v, toolGroupInfrastructure, toolGroupAgentPlatform)
		}
		if _, isMember := members[s.Name]; isMember && s.Namespace == platformNamespace {
			continue
		}
		if s.Labels[managedByLabel] == managedByAgentlabValue {
			return nil, fmt.Errorf("MCPServer %s is lab-created but carries %s=%s — the platform's servers carry the group from their charts' templates",
				s.key(), toolGroupLabel, v)
		}
		chartLabelled = append(chartLabelled, s.key()+"="+v)
	}
	sort.Strings(chartLabelled)
	return chartLabelled, nil
}

// listMCPServers reads muster's MCPServer CRs (musterMCPServerResource, fully
// qualified: kagent ships its own mcpservers.kagent.dev) in a namespace — or
// in every one with ns "" — matching the label selector.
func listMCPServers(ctx context.Context, ns, labelSelector string) ([]labelledServer, error) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil, err
	}
	items, err := listObjects(ctx, gvr, ns, labelSelector)
	if err != nil {
		return nil, err
	}
	out := make([]labelledServer, 0, len(items))
	for i := range items {
		out = append(out, labelledServerOf(&items[i]))
	}
	return out, nil
}

// getMCPServer reads one of muster's MCPServer CRs; a missing one stays an
// apierrors.IsNotFound.
func getMCPServer(ctx context.Context, ns, name string) (labelledServer, error) {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return labelledServer{}, err
	}
	obj, err := getObject(ctx, gvr, ns, name)
	if err != nil {
		return labelledServer{}, err
	}
	return labelledServerOf(obj), nil
}

func labelledServerOf(obj *unstructured.Unstructured) labelledServer {
	family, _, _ := unstructured.NestedString(obj.Object, "spec", "family", "name")
	instanceArg, _, _ := unstructured.NestedString(obj.Object, "spec", "family", "instanceArg")
	return labelledServer{Namespace: obj.GetNamespace(), Name: obj.GetName(), Labels: obj.GetLabels(), Family: family, InstanceArg: instanceArg}
}
