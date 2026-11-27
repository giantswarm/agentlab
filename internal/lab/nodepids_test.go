package lab

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agentlab/internal/config"
)

func TestEnsureNodePidsLimit(t *testing.T) {
	const (
		limited   = "pidslab-control-plane"
		unlimited = "pidslab-worker"
		unset     = "pidslab-worker2"
		stuck     = "pidslab-worker3"
	)
	dir := t.TempDir()
	calls := installFakeTool(t, dir, "docker", `case "$1 $4" in
"inspect `+limited+`"|"inspect `+stuck+`") echo 2048 ;;
"inspect `+unlimited+`") echo -1 ;;
"inspect `+unset+`") echo '<nil>' ;;
esac
case "$1 $3" in
"update `+stuck+`") exit 1 ;;
esac`)
	prev := kindNodeNames
	kindNodeNames = func(string) ([]string, error) { return []string{limited, unlimited, unset, stuck}, nil }
	t.Cleanup(func() { kindNodeNames = prev })

	ensureNodePidsLimit(&config.Config{ClusterName: "pidslab"})

	var updates []string
	for _, c := range callLines(t, calls) {
		if strings.HasPrefix(c, "docker update ") {
			updates = append(updates, c)
		}
	}
	require.Equal(t, []string{
		"docker update --pids-limit=-1 " + limited,
		"docker update --pids-limit=-1 " + stuck,
	}, updates, "only the nodes with a positive limit are updated, a failed update does not stop the others")
}

func TestParsePidsLimit(t *testing.T) {
	for out, want := range map[string]int64{"2048\n": 2048, "0\n": 0, "-1\n": 0, "<nil>\n": 0, "": 0} {
		got, limited := parsePidsLimit(out)
		require.Equal(t, want, got, out)
		require.Equal(t, want > 0, limited, out)
	}
}

func TestParseNodePids(t *testing.T) {
	got, ok := parseNodePids("n", "1958\nmax\n")
	require.True(t, ok)
	require.Equal(t, NodePidsStatus{Node: "n", Current: 1958, Max: "max"}, got)

	got, ok = parseNodePids("n", "12\n2048\n")
	require.True(t, ok)
	require.Equal(t, NodePidsStatus{Node: "n", Current: 12, Max: "2048"}, got)

	for _, bad := range []string{"", "12\n", "x\nmax\n", "1\n2\n3\n"} {
		_, ok := parseNodePids("n", bad)
		require.False(t, ok, bad)
	}
}

func TestNodePidsLine(t *testing.T) {
	require.Equal(t, "cp 1958/max, w 100/2048", nodePidsLine([]NodePidsStatus{
		{Node: "cp", Current: 1958, Max: "max"},
		{Node: "w", Current: 100, Max: "2048"},
	}))
	require.Equal(t, "cp 1990/2048 — near the limit; `agentlab up` lifts it",
		NodePidsStatus{Node: "cp", Current: 1990, Max: "2048"}.String())
	require.Equal(t, "cp 1800/2048",
		NodePidsStatus{Node: "cp", Current: 1800, Max: "2048"}.String())
}
