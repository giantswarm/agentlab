package lab

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestMusterPodNetwork pins platform.musterPodNetwork's shape: off, muster
// keeps the host-network patch at one replica and the values name no replica
// count; on, muster loses that patch, takes the dex-localhost sidecar on the
// lab Dex port (none under platform.tls, where cluster DNS answers the
// issuer), and the values run two replicas behind minAvailable 1. Backstage
// stays on the host network either way.
func TestMusterPodNetwork(t *testing.T) {
	for _, tc := range []struct {
		name        string
		podNetwork  bool
		tls         bool
		hostNetwork bool
		sidecar     bool
		replicas    int
	}{
		{name: "host network, the default", hostNetwork: true, replicas: 1},
		{name: "pod network", podNetwork: true, sidecar: true, replicas: 2},
		{name: "pod network under platform.tls", podNetwork: true, tls: true, replicas: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Platform.MusterPodNetwork = tc.podNetwork
			if tc.tls {
				cfg.Platform.TLS = config.PlatformTLS{CertFile: "c", KeyFile: "k"}
			}
			if got := cfg.MusterReplicas(); got != tc.replicas {
				t.Errorf("MusterReplicas() = %d, want %d", got, tc.replicas)
			}
			rendered, err := componentPostRenderers(cfg, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			patches := func(component string) []string {
				var list []postRenderer
				if err := yaml.Unmarshal([]byte(rendered[component]), &list); err != nil {
					t.Fatalf("%s: %v", component, err)
				}
				var out []string
				for _, pr := range list {
					for _, p := range pr.Kustomize.Patches {
						out = append(out, string(p.Patch))
					}
				}
				return out
			}
			muster := strings.Join(patches(componentMuster), "\n---\n")
			if got := strings.Contains(muster, "hostNetwork: true"); got != tc.hostNetwork {
				t.Errorf("muster hostNetwork patch = %v, want %v:\n%s", got, tc.hostNetwork, muster)
			}
			if got := strings.Contains(muster, "name: "+dexLocalhostContainer); got != tc.sidecar {
				t.Errorf("muster %s sidecar = %v, want %v:\n%s", dexLocalhostContainer, got, tc.sidecar, muster)
			}
			if tc.sidecar && !strings.Contains(muster, "TCP6-LISTEN:32000,fork,reuseaddr") {
				t.Errorf("muster sidecar does not listen on the lab Dex port:\n%s", muster)
			}
			if backstage := strings.Join(patches(componentBackstage), "\n"); !strings.Contains(backstage, "hostNetwork: true") {
				t.Errorf("backstage lost its hostNetwork patch:\n%s", backstage)
			}

			if tc.tls {
				// The values are the pod-network case's; a platform.tls render
				// needs the pair on disk (byoissuer_test.go).
				return
			}
			out, err := renderTemplate(cfg, platformValuesTemplate, func(d *tmplData) { d.PostRenderers = rendered })
			if err != nil {
				t.Fatal(err)
			}
			var values struct {
				Muster struct {
					ReplicaCount *int `yaml:"replicaCount"`
					PDB          *struct {
						Enabled        bool   `yaml:"enabled"`
						MinAvailable   int    `yaml:"minAvailable"`
						MaxUnavailable string `yaml:"maxUnavailable"`
					} `yaml:"podDisruptionBudget"`
				} `yaml:"muster"`
			}
			if err := yaml.Unmarshal(out, &values); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !tc.podNetwork {
				if values.Muster.ReplicaCount != nil || values.Muster.PDB != nil {
					t.Errorf("default lab: want the chart's muster replicas and budget untouched, got replicaCount %v, podDisruptionBudget %+v", values.Muster.ReplicaCount, values.Muster.PDB)
				}
				return
			}
			if values.Muster.ReplicaCount == nil || *values.Muster.ReplicaCount != 2 {
				t.Errorf("muster.replicaCount = %v, want 2", values.Muster.ReplicaCount)
			}
			if pdb := values.Muster.PDB; pdb == nil || !pdb.Enabled || pdb.MinAvailable != 1 || pdb.MaxUnavailable != "" {
				t.Errorf("muster.podDisruptionBudget = %+v, want enabled, minAvailable 1, maxUnavailable cleared", pdb)
			}
		})
	}
}
