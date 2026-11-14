package lab

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// A component release the nodes already pulled through the registry mirror
// is not pulled into the host's docker cache again: sideloadPlatformImages
// and the manifest lane drop what every node holds before the host pull, so
// a release costs the machine one download. A ref one node lacks (a worker
// that missed it) is still pulled and loaded.
func TestNodeHeldImagesSkipTheHostPull(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.ClusterName = "nodes"
	const (
		controlPlane = "nodes-control-plane"
		worker       = "nodes-worker"
		onBoth       = "gsoci.azurecr.io/giantswarm/muster:1.2.3"
		onBoth2      = "gsoci.azurecr.io/giantswarm/mcp-kubernetes:0.9.0"
		onCPOnly     = "gsoci.azurecr.io/giantswarm/agent-manager:2.24.0"
	)
	crictl := func(tags ...string) string {
		quoted := make([]string, len(tags))
		for i, tag := range tags {
			quoted[i] = `{"repoTags":["` + tag + `"]}`
		}
		return `{"images":[` + strings.Join(quoted, ",") + `]}`
	}
	for node, list := range map[string]string{
		controlPlane: crictl(onBoth, onBoth2, onCPOnly),
		worker:       crictl(onBoth, onBoth2, "registry.k8s.io/pause:3.10"),
	} {
		if err := os.WriteFile(filepath.Join(dir, node+".json"), []byte(list), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKE_NODES", dir)
	// docker: the nodes' crictl lists; every host lookup and pull is
	// logged and fails, so nothing is side-loaded.
	calls := installFakeTool(t, dir, "docker", `case "$1" in
exec) cat "$FAKE_NODES/$2.json" ;;
image|pull) exit 1 ;;
*) echo "unexpected docker $*" >&2; exit 2 ;;
esac`)
	prev := kindNodeNames
	kindNodeNames = func(string) ([]string, error) { return []string{controlPlane, worker}, nil }
	t.Cleanup(func() { kindNodeNames = prev })

	hostCalls := func() []string {
		var host []string
		for _, c := range callLines(t, calls) {
			if !strings.HasPrefix(c, "docker exec ") {
				host = append(host, c)
			}
		}
		return host
	}

	sideloadPlatformImages(cfg, []string{onBoth, onBoth2})
	if got := hostCalls(); len(got) != 0 {
		t.Errorf("a release every node holds reached the host cache: %v", got)
	}

	sideloadPlatformImages(cfg, []string{onBoth, onCPOnly})
	want := []string{"docker image inspect " + onCPOnly, "docker pull -q " + onCPOnly}
	if got := hostCalls(); !slices.Equal(got, want) {
		t.Errorf("a ref the worker lacks\n got  %v\n want %v", got, want)
	}

	if missing, onNodes := withoutNodeImages(cfg, []string{onBoth, onCPOnly, onBoth2}); !slices.Equal(missing, []string{onCPOnly}) || onNodes != 2 {
		t.Errorf("withoutNodeImages = %v, %d; want [%s], 2", missing, onNodes, onCPOnly)
	}
	// Nodes that cannot be listed keep every ref.
	kindNodeNames = func(string) ([]string, error) { return []string{controlPlane, "nodes-gone"}, nil }
	if missing, onNodes := withoutNodeImages(cfg, []string{onBoth}); !slices.Equal(missing, []string{onBoth}) || onNodes != 0 {
		t.Errorf("an unlisted node: %v, %d; want every ref kept", missing, onNodes)
	}
}
