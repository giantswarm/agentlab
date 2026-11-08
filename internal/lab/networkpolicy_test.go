package lab

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// testNodeIP is a control-plane node's address on the kind network.
const testNodeIP = "172.21.0.3"

// TestNetworkPolicyValidation pins what platform.networkPolicy needs: muster
// on the pod network, whose pod labels the policies admit.
func TestNetworkPolicyValidation(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.NetworkPolicy = true
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "platform.networkPolicy requires platform.musterPodNetwork") {
		t.Fatalf("Validate() with muster on the host network = %v, want the musterPodNetwork refusal", err)
	}
	cfg.Platform.MusterPodNetwork = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with muster on the pod network = %v, want nil", err)
	}
}

// networkPolicyValues is the part of the values the switch renders.
type networkPolicyValues struct {
	NetworkPolicy struct {
		Enabled    bool   `yaml:"enabled"`
		Flavor     string `yaml:"flavor"`
		Kubernetes *struct {
			APIServerCIDR      string   `yaml:"apiServerCIDR"`
			WorldExcludedCIDRs []string `yaml:"worldExcludedCIDRs"`
		} `yaml:"kubernetes"`
	} `yaml:"networkPolicy"`
	Muster struct {
		NetworkPolicy struct {
			Enabled    bool   `yaml:"enabled"`
			Flavor     string `yaml:"flavor"`
			Kubernetes *struct {
				APIServerCIDR      string           `yaml:"apiServerCIDR"`
				WorldExcludedCIDRs []string         `yaml:"worldExcludedCIDRs"`
				AdditionalEgress   []map[string]any `yaml:"additionalEgress"`
			} `yaml:"kubernetes"`
		} `yaml:"networkPolicy"`
	} `yaml:"muster"`
}

// TestNetworkPolicyValues pins the values of platform.networkPolicy: off,
// both charts' policies off; on, both in the kubernetes flavour with the
// node's address as the apiserver and muster's two lab peers (the lab Dex,
// muster itself); on before the node exists, the chart's apiServerCIDR left
// in place (`agentlab platform` refuses to install without the address).
func TestNetworkPolicyValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		on     bool
		nodeIP string
	}{
		{name: "off, the default"},
		{name: "on", on: true, nodeIP: testNodeIP},
		{name: "on, no node yet", on: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.NetworkPolicy = tc.on
			cfg.Platform.MusterPodNetwork = tc.on
			out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.NodeIP = tc.nodeIP })
			if err != nil {
				t.Fatal(err)
			}
			var v networkPolicyValues
			if err := yaml.Unmarshal(out, &v); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			np, mp := v.NetworkPolicy, v.Muster.NetworkPolicy
			if np.Enabled != tc.on || mp.Enabled != tc.on {
				t.Fatalf("networkPolicy.enabled = %v, muster.networkPolicy.enabled = %v, want %v", np.Enabled, mp.Enabled, tc.on)
			}
			if !tc.on {
				return
			}
			if np.Flavor != familyKubernetes || mp.Flavor != familyKubernetes {
				t.Errorf("flavors = %q, %q, want kubernetes", np.Flavor, mp.Flavor)
			}
			want := ""
			if tc.nodeIP != "" {
				want = tc.nodeIP + "/32"
			}
			var gotConn, gotMuster string
			if np.Kubernetes != nil {
				gotConn = np.Kubernetes.APIServerCIDR
			}
			if mp.Kubernetes != nil {
				gotMuster = mp.Kubernetes.APIServerCIDR
			}
			if gotConn != want || gotMuster != want {
				t.Errorf("apiServerCIDR = %q (connectivity), %q (muster), want %q", gotConn, gotMuster, want)
			}
			if np.Kubernetes == nil || !slices.Equal(np.Kubernetes.WorldExcludedCIDRs, labWorldExcludedCIDRs) || mp.Kubernetes == nil || !slices.Equal(mp.Kubernetes.WorldExcludedCIDRs, labWorldExcludedCIDRs) {
				t.Errorf("worldExcludedCIDRs: want %v on both charts, got %+v, %+v", labWorldExcludedCIDRs, np.Kubernetes, mp.Kubernetes)
			}
			if slices.Contains(labWorldExcludedCIDRs, "10.0.0.0/8") || !slices.Contains(labWorldExcludedCIDRs, "10.244.0.0/16") {
				t.Errorf("labWorldExcludedCIDRs = %v: want 10.0.0.0/8 narrowed to the cluster's subnets", labWorldExcludedCIDRs)
			}
			if mp.Kubernetes == nil || len(mp.Kubernetes.AdditionalEgress) != 2 {
				t.Fatalf("muster.networkPolicy.kubernetes.additionalEgress: want the lab Dex and muster itself, got %+v", mp.Kubernetes)
			}
			peers, _ := yaml.Marshal(mp.Kubernetes.AdditionalEgress)
			for _, want := range []string{"kubernetes.io/metadata.name: dex", "port: 5556", "app.kubernetes.io/name: muster", "port: 8090"} {
				if !strings.Contains(string(peers), want) {
					t.Errorf("muster's lab peers lack %q:\n%s", want, peers)
				}
			}
		})
	}
}

// TestDataplanePeersPatch pins patch 10: with the policies on, the
// connectivity chart's data-plane policy gains the egress rules to the lab
// Prometheus (observability) and the kagent controller (agents), each only
// with its backend; nothing with the policies off.
func TestDataplanePeersPatch(t *testing.T) {
	for _, tc := range []struct {
		name               string
		on, obs, agents    bool
		prometheus, kagent bool
	}{
		{name: "policies off", obs: true, agents: true},
		{name: "on, both backends", on: true, obs: true, agents: true, prometheus: true, kagent: true},
		{name: "on, agents only", on: true, agents: true, kagent: true},
		{name: "on, no backend", on: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.NetworkPolicy, cfg.Platform.Observability, cfg.Platform.Agents = tc.on, tc.obs, tc.agents
			rendered, err := componentPostRenderers(cfg, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var list []postRenderer
			if err := yaml.Unmarshal([]byte(rendered[config.ConnectivityChartName]), &list); err != nil {
				t.Fatal(err)
			}
			var patch string
			for _, pr := range list {
				for _, p := range pr.Kustomize.Patches {
					if p.Target[kindKey] == kindNetworkPolicy && p.Target[nameKey] == dataplanePolicy {
						patch = string(p.Patch)
					}
				}
			}
			if (patch != "") != (tc.prometheus || tc.kagent) {
				t.Fatalf("patch on %s present = %v, want %v:\n%s", dataplanePolicy, patch != "", tc.prometheus || tc.kagent, patch)
			}
			if patch == "" {
				return
			}
			var ops []struct {
				Op    string `yaml:"op"`
				Path  string `yaml:"path"`
				Value struct {
					To []struct {
						NamespaceSelector struct {
							MatchLabels map[string]string `yaml:"matchLabels"`
						} `yaml:"namespaceSelector"`
						PodSelector struct {
							MatchLabels map[string]string `yaml:"matchLabels"`
						} `yaml:"podSelector"`
					} `yaml:"to"`
					Ports []struct {
						Port int `yaml:"port"`
					} `yaml:"ports"`
				} `yaml:"value"`
			}
			if err := yaml.Unmarshal([]byte(patch), &ops); err != nil {
				t.Fatalf("%v\n%s", err, patch)
			}
			got := map[string]bool{}
			for _, op := range ops {
				if op.Op != "add" || op.Path != "/spec/egress/-" || len(op.Value.To) != 1 || len(op.Value.Ports) != 1 {
					t.Fatalf("not one appended egress rule: %+v", op)
				}
				ns := op.Value.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
				name := op.Value.To[0].PodSelector.MatchLabels["app.kubernetes.io/name"]
				switch {
				case ns == observabilityNamespace && name == "prometheus" && op.Value.Ports[0].Port == prometheusPort:
					got["prometheus"] = true
				case ns == componentKagent && maps.Equal(op.Value.To[0].PodSelector.MatchLabels, kagentControllerSelector) && op.Value.Ports[0].Port == kagentControllerPort:
					got[componentKagent] = true
				default:
					t.Errorf("unexpected rule: %+v", op.Value)
				}
			}
			if got["prometheus"] != tc.prometheus || got[componentKagent] != tc.kagent {
				t.Errorf("rules = %v, want prometheus %v, kagent %v", got, tc.prometheus, tc.kagent)
			}
		})
	}
}

// TestNetpolProbeVerdict pins the denial judgement: the control must answer,
// the backend must not.
func TestNetpolProbeVerdict(t *testing.T) {
	const answered = "  HTTP/1.1 200 OK\n"
	const timedOut = "wget: download timed out\n"
	for _, tc := range []struct {
		name, out, wantErr string
	}{
		{name: "denied", out: "--- control\n" + answered + "--- backend\n" + timedOut},
		{name: "backend reached", out: "--- control\n" + answered + "--- backend\n  HTTP/1.1 404 Not Found\n", wantErr: "the policy is not enforced"},
		{name: "control unreachable", out: "--- control\n" + timedOut + "--- backend\n" + timedOut, wantErr: "the probe's own network is broken"},
		{name: "no output", out: "sh: wget: not found\n", wantErr: "no control section"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := netpolProbeVerdict(tc.out)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("netpolProbeVerdict = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("netpolProbeVerdict = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestNewTmplDataNodeIP pins where NodeIP comes from: the node's address
// only while the policies are on, and empty — not an error — while the node
// does not exist.
func TestNewTmplDataNodeIP(t *testing.T) {
	saved := kindNodeIPFn
	t.Cleanup(func() { kindNodeIPFn = saved })
	for _, tc := range []struct {
		name string
		on   bool
		ip   string
		err  error
		want string
	}{
		{name: "policies off", ip: testNodeIP},
		{name: "on", on: true, ip: testNodeIP, want: testNodeIP},
		{name: "on, no node", on: true, err: errors.New("node not found")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kindNodeIPFn = func(string) (string, error) { return tc.ip, tc.err }
			cfg := config.Default()
			cfg.Platform.NetworkPolicy = tc.on
			d, err := newTmplData(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if d.NodeIP != tc.want {
				t.Errorf("NodeIP = %q, want %q", d.NodeIP, tc.want)
			}
		})
	}
}

// TestDexEgressPolicies pins patch 11's rule: an egress policy that selects
// a Deployment dialing the lab Dex gains the Dex rule — appended to its
// egress list, or as the list where it has none — while an ingress-only
// policy, a policy selecting other pods and one in another namespace do not.
func TestDexEgressPolicies(t *testing.T) {
	renders := map[string]string{
		agentManagerMCPServer: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-manager
  namespace: agent-platform
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: agent-manager
`,
		config.ConnectivityChartName: `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: am-egress
  namespace: agent-platform
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: agent-manager
  policyTypes: [Egress]
  egress:
    - ports:
        - port: 53
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: am-deny-egress
  namespace: agent-platform
spec:
  podSelector:
    matchExpressions:
      - key: app.kubernetes.io/name
        operator: In
        values: [agent-manager]
  policyTypes: [Egress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: am-ingress
  namespace: agent-platform
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: agent-manager
  policyTypes: [Ingress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: other-egress
  namespace: agent-platform
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: muster
  policyTypes: [Egress]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: am-elsewhere
  namespace: kagent
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: agent-manager
  policyTypes: [Egress]
`,
	}
	dialers := map[string][]dexLocalhostTarget{agentManagerMCPServer: {{deployment: agentManagerMCPServer}}}
	got, err := dexEgressPolicies(renders, dialers)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]dexEgressPolicy{config.ConnectivityChartName: {
		{name: "am-deny-egress", namespace: platformNamespace},
		{name: "am-egress", namespace: platformNamespace, hasEgress: true},
	}}
	if len(got) != 1 || !slices.Equal(got[config.ConnectivityChartName], want[config.ConnectivityChartName]) {
		t.Fatalf("dexEgressPolicies = %+v, want %+v", got, want)
	}
	for _, p := range got[config.ConnectivityChartName] {
		patch := dexEgressPatch(p)
		if patch.Target[kindKey] != kindNetworkPolicy || patch.Target[nameKey] != p.name || patch.Target[namespaceKey] != p.namespace {
			t.Errorf("%s: target %v", p.name, patch.Target)
		}
		var ops []struct {
			Op    string `yaml:"op"`
			Path  string `yaml:"path"`
			Value any    `yaml:"value"`
		}
		if err := yaml.Unmarshal([]byte(patch.Patch), &ops); err != nil || len(ops) != 1 {
			t.Fatalf("%s: %v\n%s", p.name, err, patch.Patch)
		}
		rule := ops[0].Value
		wantPath := "/spec/egress/-"
		if !p.hasEgress {
			wantPath = "/spec/egress"
			list, ok := rule.([]any)
			if !ok || len(list) != 1 {
				t.Fatalf("%s: value %v, want a one-rule list", p.name, rule)
			}
			rule = list[0]
		}
		if ops[0].Op != "add" || ops[0].Path != wantPath {
			t.Errorf("%s: %s %s, want add %s", p.name, ops[0].Op, ops[0].Path, wantPath)
		}
		out, _ := yaml.Marshal(rule)
		for _, w := range []string{"kubernetes.io/metadata.name: dex", "port: 5556", "protocol: TCP"} {
			if !strings.Contains(string(out), w) {
				t.Errorf("%s: rule lacks %q:\n%s", p.name, w, out)
			}
		}
	}
	if none, err := dexEgressPolicies(renders, nil); err != nil || len(none) != 0 {
		t.Errorf("no dialers: %v, %v; want none", none, err)
	}
}

// TestNetworkPolicyStandInsDelete pins patch 10's deletion: with the policies
// and the workspaces on, the Egress policy that would isolate the kagent
// controller is deleted; without the workspaces, or with the policies off,
// nothing is.
func TestNetworkPolicyStandInsDelete(t *testing.T) {
	for _, tc := range []struct {
		name           string
		on, workspaces bool
		want           bool
	}{
		{name: "on with workspaces", on: true, workspaces: true, want: true},
		{name: "on without workspaces", on: true},
		{name: "off with workspaces", workspaces: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.Agents = true
			cfg.Platform.NetworkPolicy = tc.on
			cfg.Platform.Workspaces.Enabled = tc.workspaces
			var got bool
			for _, p := range networkPolicyStandIns(cfg) {
				if p.Target[nameKey] == controllerToWorkspaceManagerPolicy {
					if !strings.Contains(string(p.Patch), "$patch: delete") || !strings.Contains(string(p.Patch), "namespace: "+componentKagent) {
						t.Errorf("not a deletion of the kagent namespace's policy:\n%s", p.Patch)
					}
					got = true
				}
			}
			if got != tc.want {
				t.Errorf("deletes %s = %v, want %v", controllerToWorkspaceManagerPolicy, got, tc.want)
			}
		})
	}
}
