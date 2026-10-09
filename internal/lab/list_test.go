package lab

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/labs"
)

// writeLab writes a lab directory whose agentlab.yaml is cfg.
func writeLab(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.File), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stubListProbes(t *testing.T, states map[string]string, ca map[string]string) {
	t.Helper()
	stubKindClusters(t)
	prevState, prevCA := labClusterState, labCAState
	labClusterState = func(cluster string) string { return states[cluster] }
	labCAState = func(dir string) string { return ca[dir] }
	t.Cleanup(func() { labClusterState, labCAState = prevState, prevCA })
}

// stubKindClusters stands in for docker's kind clusters.
func stubKindClusters(t *testing.T, names ...string) {
	t.Helper()
	prev := listKindClusters
	listKindClusters = func() ([]string, error) { return names, nil }
	t.Cleanup(func() { listKindClusters = prev })
}

// TestListLabs: two labs, one up on 443 and one down on 8443, the current
// directory's marked, the 8443 lab's URLs with the suffix.
func TestListLabs(t *testing.T) {
	up := writeLab(t, "clusterName: up\n")
	down := writeLab(t, "clusterName: down\nplatform:\n  enabled: true\n  gatewayPort: 8443\n  observability: false\nbackstage:\n  enabled: false\n")
	stubListProbes(t, map[string]string{"up": stateRunning, "down": "not created"}, map[string]string{up: caTrusted, down: caNone})

	listed := ListLabs([]labs.Lab{{Name: "down", Dir: down}, {Name: "up", Dir: up}}, up)
	if len(listed) != 2 {
		t.Fatalf("ListLabs = %+v", listed)
	}
	d, u := listed[0], listed[1]
	if d.Current || !u.Current {
		t.Errorf("Current = %v/%v, want only up", d.Current, u.Current)
	}
	if u.State != stateRunning || d.State != "not created" {
		t.Errorf("states = %q/%q", u.State, d.State)
	}
	if got := d.URLs["muster"]; !strings.HasSuffix(got, ":8443/mcp") {
		t.Errorf("down's muster URL = %q, want the :8443 suffix", got)
	}
	if _, ok := d.URLs["portal"]; ok {
		t.Errorf("down has Backstage off, but lists a portal URL")
	}
	if got, want := u.URLs["portal"], config.Default().BackstageBaseURL(); got != want {
		t.Errorf("up's portal URL = %q", got)
	}
	if got := strings.Join(u.Components, ", "); !strings.Contains(got, "platform (agent-platform ") || !strings.HasSuffix(got, "agents, observability, Backstage") {
		t.Errorf("up's components = %q", got)
	}

	var out bytes.Buffer
	if err := PrintLabs(&out, listed, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  down  not created  " + down, "* up  running  " + up, "lab CA      trusted", "* the lab in the current directory"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("PrintLabs output lacks %q:\n%s", want, out.String())
		}
	}

	var js bytes.Buffer
	if err := PrintLabs(&js, listed, true); err != nil {
		t.Fatal(err)
	}
	var back []ListedLab
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || len(back) != 2 || back[1].Name != "up" {
		t.Errorf("-o json = %s (%v)", js.String(), err)
	}
}

func TestListNoLabs(t *testing.T) {
	var out bytes.Buffer
	if err := PrintLabs(&out, nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "`agentlab up` in an empty directory creates one") {
		t.Errorf("PrintLabs(none) = %q", out.String())
	}
}

// A lab whose agentlab.yaml no longer parses is listed with the error, not
// dropped and not fatal for the others.
func TestListUnreadableLab(t *testing.T) {
	bad := writeLab(t, "clusterName: [\n")
	stubListProbes(t, nil, nil)
	listed := ListLabs([]labs.Lab{{Name: "bad", Dir: bad}}, "")
	if len(listed) != 1 || listed[0].Error == "" {
		t.Fatalf("ListLabs = %+v, want the parse error", listed)
	}
}

// A kind cluster no registered lab names — its registry entry or lab
// directory gone — is listed, marked, in name order among the registered
// ones; a registered lab's own cluster is not listed twice, also when its
// clusterName differs from the registry name.
func TestListUnregisteredCluster(t *testing.T) {
	const lostCluster, keptCluster, renamedCluster = "lost-lab", "kept-lab", "renamed-lab"
	reg := writeLab(t, "clusterName: "+keptCluster+"\n")
	renamed := writeLab(t, "clusterName: "+renamedCluster+"\n")
	stubListProbes(t, map[string]string{lostCluster: stateRunning, keptCluster: stateRunning, renamedCluster: stateRunning}, nil)
	stubKindClusters(t, lostCluster, keptCluster, renamedCluster)

	listed := ListLabs([]labs.Lab{{Name: keptCluster, Dir: reg}, {Name: "renamed", Dir: renamed}}, "")
	if len(listed) != 3 {
		t.Fatalf("ListLabs = %+v, want kept, lost and renamed", listed)
	}
	orphan := listed[1] // kept-lab, lost-lab, renamed
	if orphan.Name != lostCluster || !orphan.Unregistered || orphan.Dir != "" || orphan.State != stateRunning {
		t.Errorf("second = %+v, want the running, unregistered lost-lab", orphan)
	}
	if listed[0].Unregistered || listed[2].Unregistered {
		t.Errorf("registered labs marked unregistered: %+v, %+v", listed[0], listed[2])
	}

	var out bytes.Buffer
	if err := PrintLabs(&out, listed, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  " + lostCluster + "  running  lab directory unknown", "any agentlab command in its directory (`agentlab pods`) registers it again", "  " + keptCluster + "  running  " + reg} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("PrintLabs output lacks %q:\n%s", want, out.String())
		}
	}
	var js bytes.Buffer
	if err := PrintLabs(&js, listed, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"unregistered": true`) {
		t.Errorf("-o json lacks the unregistered mark:\n%s", js.String())
	}
}

// Without docker the registered labs still list.
func TestListWithoutDocker(t *testing.T) {
	lab := writeLab(t, "clusterName: a\n")
	stubListProbes(t, map[string]string{"a": "unknown"}, nil)
	listKindClusters = func() ([]string, error) { return nil, errors.New("docker: not found") }
	if listed := ListLabs([]labs.Lab{{Name: "a", Dir: lab}}, ""); len(listed) != 1 || listed[0].State != "unknown" {
		t.Fatalf("ListLabs = %+v, want a alone, state unknown", listed)
	}
}

// TestRequireLabCerts: a checkout without certs/ whose clusterName is an
// existing lab is refused with the lab's registered directory and --lab; a
// checkout with certs, and a lab not created yet, pass.
func TestRequireLabCerts(t *testing.T) {
	real := writeLab(t, "clusterName: lab-1\n")
	registry := t.TempDir()
	prevDir := labs.Dir
	labs.Dir = func() (string, error) { return registry, nil }
	t.Cleanup(func() { labs.Dir = prevDir })
	if err := labs.Register("lab-1", real); err != nil {
		t.Fatal(err)
	}
	stubListProbes(t, map[string]string{"lab-1": stateRunning, "fresh": "not created"}, nil)

	t.Chdir(t.TempDir())
	err := RequireLabCerts(&config.Config{ClusterName: "lab-1"})
	if err == nil {
		t.Fatal("a checkout without certs/ for an existing lab was accepted")
	}
	for _, want := range []string{real, "--lab lab-1", "certs/ca.crt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if err := RequireLabCerts(&config.Config{ClusterName: "fresh"}); err != nil {
		t.Errorf("a lab that is not created yet is refused: %v", err)
	}

	if err := os.MkdirAll("certs", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caCertPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireLabCerts(&config.Config{ClusterName: "lab-1"}); err != nil {
		t.Errorf("a checkout with certs/ is refused: %v", err)
	}
}
