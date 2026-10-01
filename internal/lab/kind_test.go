package lab

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/kind/pkg/cluster/nodes"
	kindexec "sigs.k8s.io/kind/pkg/exec"
)

// fakeNode stands in for a kind node: it answers `containerd config dump`
// with config and records every other command with what came down its stdin.
type fakeNode struct {
	nodes.Node
	config string
	calls  []fakeCall
}

type fakeCall struct {
	argv  []string
	stdin string
}

func (n *fakeNode) String() string { return "agentlab-control-plane" }

func (n *fakeNode) Command(name string, args ...string) kindexec.Cmd {
	return &fakeCmd{node: n, argv: append([]string{name}, args...)}
}

type fakeCmd struct {
	node   *fakeNode
	argv   []string
	stdin  io.Reader
	stdout io.Writer
}

func (c *fakeCmd) Run() error {
	if slices.Equal(c.argv, []string{"containerd", "config", "dump"}) {
		_, err := io.WriteString(c.stdout, c.node.config)
		return err
	}
	var in []byte
	if c.stdin != nil {
		var err error
		if in, err = io.ReadAll(c.stdin); err != nil {
			return err
		}
	}
	c.node.calls = append(c.node.calls, fakeCall{argv: c.argv, stdin: string(in)})
	return nil
}

func (c *fakeCmd) SetEnv(...string) kindexec.Cmd      { return c }
func (c *fakeCmd) SetStdin(r io.Reader) kindexec.Cmd  { c.stdin = r; return c }
func (c *fakeCmd) SetStdout(w io.Writer) kindexec.Cmd { c.stdout = w; return c }
func (c *fakeCmd) SetStderr(io.Writer) kindexec.Cmd   { return c }
func (n *fakeNode) CommandContext(_ context.Context, name string, args ...string) kindexec.Cmd {
	return n.Command(name, args...)
}

// TestLoadNodeArchiveImportsByName pins the side-load's import: the archive
// goes into the CRI's namespace and snapshotter under the names it carries,
// and no `--digests` record (`import-<date>@sha256:…`) is created next to
// them — a reference without a registry that the CRI reads as
// docker.io/library/import-…, which containerd does not have.
func TestLoadNodeArchiveImportsByName(t *testing.T) {
	node := &fakeNode{config: "version = 3\n[plugins.'io.containerd.cri.v1.images']\n  snapshotter = 'overlayfs'\n"}
	if err := loadNodeArchive(node, bytes.NewBufferString("archive")); err != nil {
		t.Fatal(err)
	}
	want := []string{"ctr", "--namespace=k8s.io", "images", "import", "--all-platforms", "--snapshotter=overlayfs", "-"}
	if len(node.calls) != 1 || !slices.Equal(node.calls[0].argv, want) {
		t.Fatalf("node commands %v, want one %q", node.calls, strings.Join(want, " "))
	}
	if node.calls[0].stdin != "archive" {
		t.Errorf("import read %q, want the archive", node.calls[0].stdin)
	}
}

// TestCRISnapshotter reads the CRI's snapshotter from each containerd config
// version a kind node image ships, and refuses a config that names none.
func TestCRISnapshotter(t *testing.T) {
	for _, tc := range []struct {
		name, dump, want string
	}{
		{"version 2", "version = 2\n[plugins.\"io.containerd.grpc.v1.cri\".containerd]\n  snapshotter = \"fuse-overlayfs\"\n", "fuse-overlayfs"},
		{"version 3", "version = 3\n[plugins.'io.containerd.cri.v1.images']\n  snapshotter = 'native'\n", "native"},
		{"version 4", "version = 4\n[plugins.'io.containerd.cri.v1.images']\n  snapshotter = 'overlayfs'\n", "overlayfs"},
		{"no snapshotter", "version = 4\n[plugins.'io.containerd.cri.v1.images']\n", ""},
		{"unknown version", "version = 9\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := criSnapshotter([]byte(tc.dump))
			if tc.want == "" {
				if err == nil {
					t.Fatalf("criSnapshotter = %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("criSnapshotter = %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}
