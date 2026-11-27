package lab

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// nodePidsUnlimited is the engine's --pids-limit for no limit. Podman
// creates every container at its compiled-in pids_limit of 2048 unless
// containers.conf says otherwise, and kind passes no --pids-limit (kind#2830
// leaves the default to the user); a full lab runs about 2000 processes on
// the node, so the vLLM runtime's threads fail with EAGAIN. Docker sets no
// limit, so its nodes are already unlimited (HACKS.md U51).
const nodePidsUnlimited = "-1"

// nodePidsNearLimit is the share of a numeric pids.max at which status warns.
const nodePidsNearLimit = 0.9

// ensureNodePidsLimit lifts the pids limit off every node container of the
// cluster. The engine keeps the update in the container's configuration, so
// it holds across node restarts; a recreated node gets it at the next `up`.
// Never a reason to stop the boot.
func ensureNodePidsLimit(cfg *config.Config) {
	nodes, err := kindNodeNames(cfg.ClusterName)
	if err != nil {
		warn("the nodes' pids limit: %v; a full lab may run out of threads on the node", err)
		return
	}
	for _, node := range nodes {
		out, err := outputQuiet(dockerBin, "inspect", "-f", "{{.HostConfig.PidsLimit}}", node)
		if err != nil {
			warn("reading the pids limit of node %s: %v", node, err)
			continue
		}
		limit, limited := parsePidsLimit(out)
		if !limited {
			continue
		}
		if err := runQuiet(dockerBin, "update", "--pids-limit="+nodePidsUnlimited, node); err != nil {
			warn("node %s stays at %d pids, which a full lab exceeds: %v; run `%s update --pids-limit=%s %s`",
				node, limit, err, dockerBin, nodePidsUnlimited, node)
			continue
		}
		note("node %s: pids limit %d lifted; a full lab runs about 2000 processes on the node", node, limit)
	}
}

// parsePidsLimit reads `inspect`'s HostConfig.PidsLimit: a positive number is
// a limit; 0, -1 and docker's "<nil>" are none.
func parsePidsLimit(out string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// NodePidsStatus is one node container's process count against its cgroup's
// limit, as the node sees them (the node runs in a private cgroup namespace).
type NodePidsStatus struct {
	Node    string `json:"node"`
	Current int64  `json:"current"`
	// Max is pids.max: a number, or "max" for no limit.
	Max string `json:"max"`
}

// readNodePids reads every node's pids.current and pids.max; a node that
// cannot be read is left out.
func readNodePids(cfg *config.Config) []NodePidsStatus {
	nodes, err := kindNodeNames(cfg.ClusterName)
	if err != nil {
		return nil
	}
	var all []NodePidsStatus
	for _, node := range nodes {
		out, err := outputQuiet(dockerBin, "exec", node, "cat", "/sys/fs/cgroup/pids.current", "/sys/fs/cgroup/pids.max")
		if err != nil {
			continue
		}
		if s, ok := parseNodePids(node, out); ok {
			all = append(all, s)
		}
	}
	return all
}

// parseNodePids reads the two lines of `cat pids.current pids.max`.
func parseNodePids(node, out string) (NodePidsStatus, bool) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return NodePidsStatus{}, false
	}
	current, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return NodePidsStatus{}, false
	}
	return NodePidsStatus{Node: node, Current: current, Max: fields[1]}, true
}

// String is the node's part of status's pids line, warning when a limit is
// nearly reached.
func (s NodePidsStatus) String() string {
	line := fmt.Sprintf("%s %d/%s", s.Node, s.Current, s.Max)
	if limit, err := strconv.ParseInt(s.Max, 10, 64); err == nil && limit > 0 &&
		float64(s.Current) >= nodePidsNearLimit*float64(limit) {
		line += " — near the limit; `agentlab up` lifts it"
	}
	return line
}

// nodePidsLine is `agentlab status`'s line on the nodes' pids.
func nodePidsLine(all []NodePidsStatus) string {
	parts := make([]string, len(all))
	for i, s := range all {
		parts[i] = s.String()
	}
	return strings.Join(parts, ", ")
}
