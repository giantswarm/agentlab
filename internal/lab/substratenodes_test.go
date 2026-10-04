package lab

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"

	"github.com/giantswarm/agentlab/internal/config"
)

// substrateNodeLabelValue is the value of the substrateNodes label and taint.
const substrateNodeLabelValue = "true"

// renderKindConfig decodes the rendered kind config into kind's own type,
// so the field names are the ones the embedded kind reads.
func renderKindConfig(t *testing.T, cfg *config.Config) v1alpha4.Cluster {
	t.Helper()
	out, err := renderTemplate(cfg, "kind-config.yaml.tmpl", nil)
	if err != nil {
		t.Fatal(err)
	}
	var kindCfg v1alpha4.Cluster
	if err := yaml.Unmarshal(out, &kindCfg); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return kindCfg
}

// nodeRegistration is the part of a kubeadm Init/JoinConfiguration patch the
// substrateNodes render sets.
type nodeRegistration struct {
	Kind             string
	NodeRegistration struct {
		Taints *[]struct{ Key, Value, Effect string }
	} `yaml:"nodeRegistration"`
}

func nodePatches(t *testing.T, node v1alpha4.Node) []nodeRegistration {
	t.Helper()
	var out []nodeRegistration
	for _, patch := range node.KubeadmConfigPatches {
		var p nodeRegistration
		if err := yaml.Unmarshal([]byte(patch), &p); err != nil {
			t.Fatalf("%v\n%s", err, patch)
		}
		out = append(out, p)
	}
	return out
}

// The default is the single-node lab, its node without patches of its own.
func TestKindConfigDefaultIsOneNode(t *testing.T) {
	kindCfg := renderKindConfig(t, config.Default())
	if len(kindCfg.Nodes) != 1 || kindCfg.Nodes[0].Role != v1alpha4.ControlPlaneRole {
		t.Fatalf("nodes = %+v, want the one control-plane node", kindCfg.Nodes)
	}
	if len(kindCfg.Nodes[0].KubeadmConfigPatches) != 0 {
		t.Errorf("the single node carries kubeadm patches %q, want none", kindCfg.Nodes[0].KubeadmConfigPatches)
	}
}

// substrateNodes: N renders N workers labelled and tainted for Substrate,
// each with the host-kernel guard, and a control plane without a taint, so
// the platform schedules there and only Substrate on the workers.
func TestKindConfigSubstrateNodes(t *testing.T) {
	cfg := config.Default()
	cfg.SubstrateNodes = 2
	kindCfg := renderKindConfig(t, cfg)
	if len(kindCfg.Nodes) != 3 {
		t.Fatalf("got %d nodes, want the control plane and 2 workers", len(kindCfg.Nodes))
	}
	cp := nodePatches(t, kindCfg.Nodes[0])
	if len(cp) != 1 || cp[0].Kind != "InitConfiguration" || cp[0].NodeRegistration.Taints == nil || len(*cp[0].NodeRegistration.Taints) != 0 {
		t.Errorf("control-plane patches = %+v, want one InitConfiguration with an empty taint list", cp)
	}
	guard := kindCfg.Nodes[0].ExtraMounts[1:]
	for _, worker := range kindCfg.Nodes[1:] {
		if worker.Role != v1alpha4.WorkerRole {
			t.Errorf("role = %s, want worker", worker.Role)
		}
		if worker.Labels[config.SubstrateNodeKey] != substrateNodeLabelValue {
			t.Errorf("labels = %v, want %s=true", worker.Labels, config.SubstrateNodeKey)
		}
		if len(worker.ExtraPortMappings) != 0 {
			t.Errorf("a worker publishes %v, want nothing: the host reaches the platform on the control plane", worker.ExtraPortMappings)
		}
		if !reflect.DeepEqual(worker.ExtraMounts, guard) {
			t.Errorf("worker mounts = %+v, want the host-kernel guard %+v", worker.ExtraMounts, guard)
		}
		patches := nodePatches(t, worker)
		if len(patches) != 1 || patches[0].Kind != "JoinConfiguration" || patches[0].NodeRegistration.Taints == nil {
			t.Fatalf("worker patches = %+v, want one JoinConfiguration with the taint", patches)
		}
		want := []struct{ Key, Value, Effect string }{{config.SubstrateNodeKey, substrateNodeLabelValue, "NoSchedule"}}
		if got := *patches[0].NodeRegistration.Taints; !reflect.DeepEqual(got, want) {
			t.Errorf("worker taints = %+v, want %+v", got, want)
		}
	}
}

// The values pin atelet and the WorkerPool's workers to the substrateNodes
// workers with the matching toleration, and leave both alone by default.
func TestPlatformValuesSubstrateNodes(t *testing.T) {
	toleration := []any{map[string]any{"key": config.SubstrateNodeKey, "operator": "Equal", "value": substrateNodeLabelValue, "effect": "NoSchedule"}}
	for _, n := range []int{0, 2} {
		cfg := config.Default()
		cfg.SubstrateNodes = n
		out, err := renderTemplate(cfg, "agent-platform-values.yaml.tmpl", nil)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(out, &values); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		atelet := values["substrate"].(map[string]any)["atelet"].(map[string]any)
		pool := values["kagent"].(map[string]any)["substrateWorkerPool"].(map[string]any)["template"].(map[string]any)
		selector := pool["nodeSelector"].(map[string]any)
		if n == 0 {
			if atelet["nodeSelector"] != nil || atelet["tolerations"] != nil || pool["tolerations"] != nil || selector[config.SubstrateNodeKey] != nil {
				t.Errorf("substrateNodes: 0 pins Substrate: atelet %v, pool %v", atelet, pool)
			}
			continue
		}
		if !reflect.DeepEqual(atelet["nodeSelector"], map[string]any{config.SubstrateNodeKey: substrateNodeLabelValue}) || !reflect.DeepEqual(atelet["tolerations"], toleration) {
			t.Errorf("atelet = %v, want the substrateNodes selector and toleration", atelet)
		}
		if selector[config.SubstrateNodeKey] != substrateNodeLabelValue || selector[workerPoolArchLabel] == nil || !reflect.DeepEqual(pool["tolerations"], toleration) {
			t.Errorf("WorkerPool template = %v, want the arch pin, the substrateNodes selector and toleration", pool)
		}
	}
}

// An existing cluster with other nodes than the configuration's is refused
// with the fix; the same nodes pass, a stopped one included (kind lists it).
func TestCheckClusterNodes(t *testing.T) {
	prev := kindNodeNames
	t.Cleanup(func() { kindNodeNames = prev })
	cfg := config.Default()
	cfg.SubstrateNodes = 2
	cp, workers := cfg.ControlPlaneNode(), cfg.SubstrateNodeNames()
	for _, tc := range []struct {
		nodes []string
		ok    bool
	}{
		{[]string{workers[1], cp, workers[0]}, true},
		{[]string{cp}, false},
		{[]string{cp, workers[0]}, false},
	} {
		kindNodeNames = func(string) ([]string, error) { return tc.nodes, nil }
		err := checkClusterNodes(cfg)
		if tc.ok != (err == nil) {
			t.Errorf("nodes %v: err = %v, want ok=%v", tc.nodes, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "agentlab down") {
			t.Errorf("the refusal names no fix: %v", err)
		}
	}
}
