package lab

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/giantswarm/agentlab/internal/config"
)

// valkeyCNPRender is the valkey wrapper's policy as the meta chart renders
// it: the chart's two clients, the metrics collectors and the host entity.
const valkeyCNPRender = `---
apiVersion: v1
kind: Service
metadata:
  name: muster-valkey
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  namespace: agent-platform
  name: muster-valkey
spec:
  endpointSelector:
    matchLabels:
      app.kubernetes.io/name: "valkey"
      app.kubernetes.io/instance: "valkey"
  egress:
    - toEndpoints:
        - matchLabels:
            io.kubernetes.pod.namespace: kube-system
            k8s-app: coredns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
  ingress:
    - fromEndpoints:
        - matchLabels:
            app.kubernetes.io/name: muster
            io.kubernetes.pod.namespace: agent-platform
        - matchLabels:
            app.kubernetes.io/name: klaus-gateway
            io.kubernetes.pod.namespace: agent-platform
      toPorts:
        - ports:
            - port: "6379"
              protocol: TCP
    - fromEndpoints:
        - matchLabels:
            app.kubernetes.io/name: prometheus
            k8s:io.kubernetes.pod.namespace: monitoring
      authentication:
        mode: "required"
      toPorts:
        - ports:
            - port: "9121"
              protocol: TCP
    - fromEntities:
        - host
`

// TestCiliumStandIns pins the store's stand-in: the same pods, each client
// admitted by its labels in its namespace on the Valkey port, the
// collectors on the exporter's, the host entity as the node's address, and
// what the translation does not carry named.
func TestCiliumStandIns(t *testing.T) {
	got, err := ciliumStandIns(map[string]string{valkeyRelease: valkeyCNPRender, componentMuster: valkeyCNPRender}, platformNamespace, []string{testNodeIP})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d stand-ins, want the valkey release's one (a policy of another release is not stood in for)", len(got))
	}
	s := got[0]
	np := s.policy
	if np.Name != "muster-valkey"+standInNameSuffix || np.Namespace != platformNamespace {
		t.Errorf("stand-in %s/%s, want %s/muster-valkey%s", np.Namespace, np.Name, platformNamespace, standInNameSuffix)
	}
	if np.Labels[standInLabel] != standInLabelPrefix+"muster-valkey" {
		t.Errorf("label %s = %q", standInLabel, np.Labels[standInLabel])
	}
	if got, want := np.Spec.PodSelector.MatchLabels, map[string]string{appNameLabel: valkeyRelease, "app.kubernetes.io/instance": valkeyRelease}; !maps.Equal(got, want) {
		t.Errorf("podSelector %v, want %v", got, want)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes %v, want Ingress only", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 3 {
		t.Fatalf("got %d ingress rules, want 3", len(np.Spec.Ingress))
	}
	clients := np.Spec.Ingress[0]
	if len(clients.From) != 2 {
		t.Fatalf("clients rule has %d peers, want muster and klaus-gateway", len(clients.From))
	}
	for i, name := range []string{componentMuster, klausGatewayComponent} {
		p := clients.From[i]
		if !maps.Equal(p.PodSelector.MatchLabels, map[string]string{appNameLabel: name}) {
			t.Errorf("client %d podSelector %v, want %s's name only", i, p.PodSelector.MatchLabels, name)
		}
		if p.NamespaceSelector == nil || p.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != platformNamespace {
			t.Errorf("client %d namespaceSelector %v, want %s", i, p.NamespaceSelector, platformNamespace)
		}
	}
	if len(clients.Ports) != 1 || clients.Ports[0].Port.IntValue() != 6379 || *clients.Ports[0].Protocol != "TCP" {
		t.Errorf("clients ports %v, want 6379/TCP", clients.Ports)
	}
	scrapers := np.Spec.Ingress[1]
	if scrapers.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "monitoring" {
		t.Errorf("the k8s:-prefixed namespace label was not read: %v", scrapers.From[0])
	}
	host := np.Spec.Ingress[2]
	if len(host.From) != 1 || host.From[0].IPBlock == nil || host.From[0].IPBlock.CIDR != testNodeIP+"/32" || len(host.Ports) != 0 {
		t.Errorf("host rule %+v, want the node's address on every port", host)
	}
	unheld := strings.Join(s.unheld, "\n")
	for _, want := range []string{"egress", `mutual authentication "required"`} {
		if !strings.Contains(unheld, want) {
			t.Errorf("unheld %q does not name %s", unheld, want)
		}
	}
}

// TestCiliumStandInsNoPolicy: a render without the policy yields none.
func TestCiliumStandInsNoPolicy(t *testing.T) {
	got, err := ciliumStandIns(map[string]string{valkeyRelease: "apiVersion: v1\nkind: Service\nmetadata:\n  name: x\n"}, platformNamespace, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("ciliumStandIns = %v, %v; want none", got, err)
	}
}

// TestTranslateCiliumPolicyRefusesMore pins the stand-in's direction: what
// it cannot carry is left out, never opened — an unknown entity drops the
// rule rather than leaving one without peers, which would admit everyone.
func TestTranslateCiliumPolicyRefusesMore(t *testing.T) {
	var cnp ciliumPolicy
	raw := `{"metadata":{"name":"p","namespace":"ns"},"spec":{"endpointSelector":{"matchLabels":{"a":"b"}},
"ingress":[{"fromEntities":["world"]},{"fromEndpoints":[{"matchLabels":{"c":"d"}}],"toPorts":[{"ports":[{"port":"80","protocol":"TCP"}],"rules":{"http":[{"method":"GET"}]}}]},
{"fromCIDRSet":[{"cidr":"10.0.0.0/8","except":["10.1.0.0/16"]}],"toPorts":[{"ports":[{"port":"0","protocol":"ANY"}]}]}]}}`
	if err := json.Unmarshal([]byte(raw), &cnp); err != nil {
		t.Fatal(err)
	}
	s, err := translateCiliumPolicy(&cnp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.policy.Spec.Ingress) != 2 {
		t.Fatalf("got %d rules, want the world rule left out: %+v", len(s.policy.Spec.Ingress), s.policy.Spec.Ingress)
	}
	if p := s.policy.Spec.Ingress[0].From[0]; p.NamespaceSelector != nil || p.PodSelector.MatchLabels["c"] != "d" {
		t.Errorf("a peer without a namespace label is the policy's own namespace: %+v", p)
	}
	if p := s.policy.Spec.Ingress[0].Ports[0]; p.Port == nil || *p.Port != intstr.FromInt32(80) {
		t.Errorf("port %+v, want 80", p)
	}
	if r := s.policy.Spec.Ingress[1]; r.From[0].IPBlock.CIDR != "10.0.0.0/8" || r.From[0].IPBlock.Except[0] != "10.1.0.0/16" || r.Ports[0].Port != nil {
		t.Errorf("CIDR rule %+v, want the block on every port", r)
	}
	unheld := strings.Join(s.unheld, "\n")
	for _, want := range []string{`"world" entity`, "L7 rule"} {
		if !strings.Contains(unheld, want) {
			t.Errorf("unheld %q does not name %s", unheld, want)
		}
	}
}

// TestTranslateCiliumPolicyRefusals pins what the translation refuses: a
// host entity without the nodes' addresses, a label of another Cilium
// source, a namespace in the endpoint selector.
func TestTranslateCiliumPolicyRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"host without nodes": `{"spec":{"ingress":[{"fromEntities":["host"]}]}}`,
		"reserved source":    `{"spec":{"ingress":[{"fromEndpoints":[{"matchLabels":{"reserved:host":""}}]}]}}`,
		"namespaced subject": `{"spec":{"endpointSelector":{"matchLabels":{"io.kubernetes.pod.namespace":"x"}}}}`,
		"protocol":           `{"spec":{"ingress":[{"fromCIDR":["1.2.3.4/32"],"toPorts":[{"ports":[{"port":"1","protocol":"ICMP"}]}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var cnp ciliumPolicy
			if err := json.Unmarshal([]byte(raw), &cnp); err != nil {
				t.Fatal(err)
			}
			if _, err := translateCiliumPolicy(&cnp, nil); err == nil {
				t.Fatal("translateCiliumPolicy = nil error, want a refusal")
			}
		})
	}
}

// TestValkeyAnswered pins the store probe's verdict.
func TestValkeyAnswered(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"--- valkey\n-NOAUTH Authentication required.\n--- exit 0\n", true},
		{"--- valkey\n--- exit 1\n", false},
		{"--- valkey\nnc: bad address 'muster-valkey'\n--- exit 1\n", false},
	} {
		_, got, err := valkeyAnswered(tc.out)
		if err != nil || got != tc.want {
			t.Errorf("valkeyAnswered(%q) = %v, %v; want %v", tc.out, got, err, tc.want)
		}
	}
	if _, _, err := valkeyAnswered("nothing"); err == nil {
		t.Error("valkeyAnswered without a marker = nil error")
	}
}

// TestValkeyPolicyValues pins the store's values: the wrapper's Cilium
// policy on with platform.networkPolicy (the chart's clients left in place),
// the lab Prometheus beside alloy-metrics with observability, and the
// gateway on the Valkey routing store.
func TestValkeyPolicyValues(t *testing.T) {
	type values struct {
		Valkey struct {
			CiliumNetworkPolicy struct {
				Enabled bool `yaml:"enabled"`
				Ingress *struct {
					Clients         []map[string]any `yaml:"clients"`
					MetricsScrapers []struct {
						Namespace   string            `yaml:"namespace"`
						MatchLabels map[string]string `yaml:"matchLabels"`
					} `yaml:"metricsScrapers"`
				} `yaml:"ingress"`
			} `yaml:"ciliumNetworkPolicy"`
		} `yaml:"valkey"`
		KlausGateway struct {
			Routing struct {
				Store string `yaml:"store"`
			} `yaml:"routing"`
		} `yaml:"klausGateway"`
	}
	for _, tc := range []struct {
		name    string
		on, obs bool
	}{
		{name: "switch off"},
		{name: "on", on: true},
		{name: "on with observability", on: true, obs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.NetworkPolicy = tc.on
			cfg.Platform.MusterPodNetwork = tc.on
			cfg.Platform.Observability = tc.obs
			cfg.Platform.Agents = true
			cfg.Platform.KlausGateway.Enabled = true
			out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.NodeIP = testNodeIP })
			if err != nil {
				t.Fatal(err)
			}
			var v values
			if err := yaml.Unmarshal(out, &v); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			cnp := v.Valkey.CiliumNetworkPolicy
			if cnp.Enabled != tc.on {
				t.Errorf("valkey.ciliumNetworkPolicy.enabled = %v, want %v", cnp.Enabled, tc.on)
			}
			if !tc.obs {
				if cnp.Ingress != nil {
					t.Errorf("valkey.ciliumNetworkPolicy.ingress = %+v, want the chart's", cnp.Ingress)
				}
			} else if cnp.Ingress == nil || len(cnp.Ingress.Clients) != 0 || len(cnp.Ingress.MetricsScrapers) != 2 || cnp.Ingress.MetricsScrapers[1].Namespace != observabilityNamespace {
				t.Errorf("valkey.ciliumNetworkPolicy.ingress = %+v, want alloy-metrics and the lab Prometheus, the chart's clients untouched", cnp.Ingress)
			}
			if v.KlausGateway.Routing.Store != "valkey" {
				t.Errorf("klausGateway.routing.store = %q, want valkey", v.KlausGateway.Routing.Store)
			}
		})
	}
}
