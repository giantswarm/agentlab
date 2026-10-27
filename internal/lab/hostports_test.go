package lab

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// freeHostPort returns a port nothing listens on, from the kernel.
func freeHostPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// labOnFreePorts is the default lab moved onto ports nothing holds, so the
// check judges only what the test binds.
func labOnFreePorts(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.ClusterName = "agentlab-test"
	cfg.DexPort = freeHostPort(t)
	cfg.APIServerPort = freeHostPort(t)
	cfg.Backstage.Port = freeHostPort(t)
	cfg.Platform.MusterPort = freeHostPort(t)
	cfg.Platform.AgentsPort = freeHostPort(t)
	cfg.Platform.GatewayPort = freeHostPort(t)
	cfg.Platform.DevRegistryPort = freeHostPort(t)
	return cfg
}

func TestCheckHostPortsFree(t *testing.T) {
	cfg := labOnFreePorts(t)
	if err := checkHostPortsFree(cfg); err != nil {
		t.Fatalf("all ports free: %v", err)
	}

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	cfg.DexPort = held.Addr().(*net.TCPAddr).Port

	err = checkHostPortsFree(cfg)
	if err == nil {
		t.Fatalf("dexPort %d held: no error", cfg.DexPort)
	}
	msg := err.Error()
	t.Log(msg)
	want := []string{
		fmt.Sprintf("127.0.0.1:%d", cfg.DexPort),
		"dexPort",
		`the lab "agentlab-test"`,
		"agentlab configure",
	}
	if runtime.GOOS == goosLinux {
		comm, err := os.ReadFile("/proc/self/comm")
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, fmt.Sprintf("%s (pid %d)", strings.TrimSpace(string(comm)), os.Getpid()))
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q lacks %q", msg, w)
		}
	}
	if strings.Count(msg, "is held by") != 1 {
		t.Errorf("error %q names more than the one held port", msg)
	}
}
