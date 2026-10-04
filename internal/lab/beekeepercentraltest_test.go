package lab

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A machine's configuration keeps its state apart and reaches the central
// instance through its muster, which names the endpoint and prints the
// person's token.
func TestCentralMachine(t *testing.T) {
	dir := t.TempDir()
	cfgFile, err := centralMachine(dir, "ana", "admin@lab.local", "https://muster.example/mcp", "a-token")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgFile) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		StateDir  string   `yaml:"stateDir"`
		Resources []string `yaml:"resources"`
		Identity  struct{ Person, Host string }
		Central   struct{ Context, Muster string }
		Lanes     []struct {
			Name, Installation string
			Repositories       []string
		}
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	machine := filepath.Join(dir, "machine-ana")
	if cfg.StateDir != filepath.Join(machine, "state") || cfg.Identity.Person != "admin@lab.local" || cfg.Identity.Host != "agentlab-ana" ||
		cfg.Central.Context != centralContext || len(cfg.Resources) != 1 || cfg.Resources[0] != centralLocal ||
		len(cfg.Lanes) != 1 || cfg.Lanes[0].Installation != centralEnvironment || cfg.Lanes[0].Repositories[0] != centralRepo {
		t.Errorf("config:\n%s", raw)
	}
	for args, want := range map[string]string{"context show agentlab -o json": `"endpoint":"https://muster.example/mcp"`, "auth token --context agentlab": "a-token"} {
		out, err := exec.Command(cfg.Central.Muster, strings.Fields(args)...).Output() //nolint:gosec // the test's own script
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("muster %s = %q, %v; want %q", args, out, err, want)
		}
	}
}
