package lab

import (
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The kind node is a docker container, and kind's notion of "the cluster
// exists" is that container's existence in ANY state: kind lists a cluster
// whose node is exited, and reading its kubeconfig off the node then fails
// inside `docker exec` — a runc "nsexec ... failed to open /proc/<pid>/ns/ipc"
// while the node is still dying, "container ... is not running" once it is
// dead — with nothing in the message that names the node's state. Two lab
// operations meet such a node:
//
//   - `agentlab down` while the node is busy (an `agentlab up` in another
//     shell side-loading a hundred images) races docker: kind's delete is
//     `docker rm -f`, docker gives the node ten seconds after
//     SIGKILL to exit, a node mid-import has taken 44 s, and docker answers
//     "cannot remove container: could not kill container: tried to kill
//     container, but did not receive an exit event". The container stays in
//     `docker ps -a` (Exited (137) once it does die), kind keeps listing the
//     cluster, and docker's on-failure restart policy never fires because an
//     API kill counts as a manual stop — nothing recovers or removes the node
//     by itself.
//   - `agentlab up` after that: the cluster "exists", its node is exited.
//
// kind supports a stopped node: it is a container like any other, and
// `docker start` brings containerd, the kubelet and the static pods back with
// the cluster state and the side-loaded images intact.

// kindClusterLabel is the docker label kind stamps on every node container of
// a cluster; `docker ps -a --filter label=<label>=<name>` is how kind itself
// finds them, exited ones included.
const kindClusterLabel = "io.x-k8s.kind.cluster"

// Docker container states as `docker ps` and `docker inspect` spell them, and
// the verbs that bring a container back to running.
const (
	stateRunning = "running"
	statePaused  = "paused"
	stateExited  = "exited"
	stateCreated = "created"
	stateDead    = "dead"
	verbStart    = "start"
	verbUnpause  = "unpause"
)

// nodeExitTimeout bounds how long Down waits for a node docker could not kill
// in time; the one observed case took 44 s.
const nodeExitTimeout = 90 * time.Second

// containerState is one `docker ps -a` row of a cluster's node containers.
type containerState struct {
	name, state string
}

// nodeState reads a node container's docker state (running, exited, paused,
// created, restarting, dead); the error carries docker's words when there is
// no such container.
func nodeState(node string) (string, error) {
	out, err := outputQuiet("docker", "inspect", "-f", "{{.State.Status}}", node)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// clusterContainers lists a kind cluster's node containers with their states,
// from `docker ps -a` — so an exited node counts, which is the whole point.
func clusterContainers(cluster string) ([]containerState, error) {
	out, err := outputQuiet("docker", "ps", "-a", "--filter", "label="+kindClusterLabel+"="+cluster,
		"--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	return parseContainerStates(out), nil
}

// parseContainerStates reads the "name<TAB>state" lines clusterContainers
// asks docker ps for.
func parseContainerStates(out string) []containerState {
	var cs []containerState
	for line := range strings.Lines(out) {
		name, state, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && name != "" {
			cs = append(cs, containerState{name: name, state: strings.TrimSpace(state)})
		}
	}
	return cs
}

// alive keeps the containers whose process still exists — everything but
// exited, created and dead. Paused and restarting count as alive: a paused
// container cannot exit until it is thawed, a restarting one is about to run.
func alive(cs []containerState) []containerState {
	var out []containerState
	for _, c := range cs {
		switch c.state {
		case stateExited, stateCreated, stateDead:
		default:
			out = append(out, c)
		}
	}
	return out
}

func containerNames(cs []containerState) string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.name+" ("+c.state+")")
	}
	return strings.Join(names, ", ")
}

// waitForNodeExit polls the cluster's node containers every interval until
// none is alive — exited, or already removed — thawing a paused one so the
// pending SIGKILL can land, and fails once timeout passes with a node still
// alive. Called by Down after kind's delete failed; says what it is waiting
// for, once.
func waitForNodeExit(cluster string, timeout, interval time.Duration) error {
	start := time.Now()
	announced := false
	for {
		cs, err := clusterContainers(cluster)
		if err != nil {
			return err
		}
		live := alive(cs)
		if len(live) == 0 {
			if announced {
				note("node exited after %s", time.Since(start).Round(time.Second))
			}
			return nil
		}
		if !announced {
			step("Waiting up to %s for %s to exit: docker could not kill it in time and kind still lists the cluster",
				timeout, containerNames(live))
			announced = true
		}
		for _, c := range live {
			if c.state == statePaused {
				note("%s is paused and cannot exit — unpausing it", c.name)
				_ = runQuiet("docker", "unpause", c.name)
			}
		}
		if time.Since(start) >= timeout {
			return fmt.Errorf("%s still alive %s after docker gave up killing it; see `journalctl -u docker`, and run `agentlab down` again once `docker ps -a` shows it exited",
				containerNames(live), timeout)
		}
		time.Sleep(interval)
	}
}

// nodeStartVerb is the docker verb that brings a node container in the given
// state back to running: nothing for a running one, unpause for a paused one,
// start for everything else (exited after a lost `down` race, created, dead).
func nodeStartVerb(state string) string {
	switch state {
	case stateRunning:
		return ""
	case statePaused:
		return verbUnpause
	default:
		return verbStart
	}
}

// ensureNodeRunning brings an existing cluster's node container back to
// running when it is not and waits for the apiserver to answer — the recovery
// for the exited node a lost `down` race leaves behind (see the file comment).
// A running node is left alone. Called by Up before anything reads the
// cluster, so a node that cannot come back fails by state and with the fix,
// not as an opaque error from kind reading the kubeconfig off it.
func ensureNodeRunning(cfg *config.Config) error {
	node := cfg.ControlPlaneNode()
	state, err := nodeState(node)
	if err != nil {
		return fmt.Errorf("kind lists cluster %q but its node container cannot be inspected: %w", cfg.ClusterName, err)
	}
	verb := nodeStartVerb(state)
	if verb == "" {
		return nil
	}
	step("Node container %s is %s — docker %s, then waiting for the apiserver", node, state, verb)
	start := time.Now()
	// outputQuiet, so the error carries docker's words ("is restarting", "no
	// such container") instead of leaving them on the terminal.
	if _, err := outputQuiet("docker", verb, node); err != nil {
		return fmt.Errorf("node container %s is %s and could not be started: %w;\nrun `agentlab up` again once `docker ps -a` shows it settled, or `agentlab down` to start over", node, state, err)
	}
	// Only a running node has a kubeconfig to export (kind reads it off the
	// node); from here on the lab's kubectl is pinned to it.
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !waitFor(60, 2*time.Second, func() bool {
		_, err := outputQuiet("kubectl", "get", "--raw=/readyz")
		return err == nil
	}) {
		return fmt.Errorf("node container %s started but its apiserver never answered /readyz; check `docker logs %s` and `docker exec %s crictl ps`, or `agentlab down` to start over", node, node, node)
	}
	note("apiserver answers again (%s)", time.Since(start).Round(time.Second))
	return nil
}
