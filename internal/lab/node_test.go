package lab

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// installFakeTool puts a stand-in for a command on PATH: a shell script that
// appends every invocation ("<name> <args>") to the returned log file and
// then runs body with the arguments in $@. The same device as installFakeKind,
// for the docker and kubectl the node recovery shells out to.
func installFakeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o750); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, name+"-calls")
	script := "#!/bin/sh\n" +
		"echo \"" + name + " $*\" >> \"" + calls + "\"\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { // #nosec G306 -- an executable stand-in under t.TempDir
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// readCalls returns a stand-in's invocation log; none yet reads as empty.
func readCalls(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- the call log this test's stand-in wrote under t.TempDir
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// The docker ps invocation Down's wait loop issues, as the stand-in logs it
// (the --format value carries a literal tab).
const dockerPSCall = "docker ps -a --filter label=io.x-k8s.kind.cluster=agentlab --format {{.Names}}\t{{.State}}\n"

// TestAliveContainers: the docker ps rows parse by name and state, and only
// exited, created and dead containers count as gone — paused and restarting
// ones still have a process, and no rows at all (docker finished the removal
// after all) is nothing alive.
func TestAliveContainers(t *testing.T) {
	cs := parseContainerStates("agentlab-control-plane\texited\nagentlab-worker\trunning\nagentlab-worker2\tpaused\n\n")
	if want := []containerState{
		{"agentlab-control-plane", stateExited}, {"agentlab-worker", stateRunning}, {"agentlab-worker2", statePaused},
	}; !slices.Equal(cs, want) {
		t.Fatalf("parseContainerStates = %v, want %v", cs, want)
	}
	if got, want := alive(cs), cs[1:]; !slices.Equal(got, want) {
		t.Errorf("alive = %v, want %v", got, want)
	}
	if got := alive(parseContainerStates("")); len(got) != 0 {
		t.Errorf("no containers: alive = %v, want none", got)
	}
	for _, state := range []string{stateExited, stateCreated, stateDead} {
		if got := alive([]containerState{{"n", state}}); len(got) != 0 {
			t.Errorf("%s container counted as alive", state)
		}
	}
	for _, state := range []string{stateRunning, statePaused, "restarting", "removing"} {
		if got := alive([]containerState{{"n", state}}); len(got) != 1 {
			t.Errorf("%s container not counted as alive", state)
		}
	}
}

// TestNodeStartVerb: a running node is left alone, a paused one unpaused,
// everything else started — `docker start` on a paused container fails.
func TestNodeStartVerb(t *testing.T) {
	for state, want := range map[string]string{
		stateRunning: "", statePaused: verbUnpause, stateExited: verbStart, stateCreated: verbStart, stateDead: verbStart,
	} {
		if got := nodeStartVerb(state); got != want {
			t.Errorf("nodeStartVerb(%q) = %q, want %q", state, got, want)
		}
	}
}

// TestWaitForNodeExit drives Down's wait against a stand-in docker whose
// node is still running on the first two polls and exited on the third: the
// wait returns once nothing is alive, having polled exactly that often; a
// paused node is unpaused so the pending SIGKILL can land; and a node still
// alive at the timeout fails by name and state with the fix.
func TestWaitForNodeExit(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "ps-count")
	calls := installFakeTool(t, dir, "docker", `
case "$1" in
ps)
  n=$(cat "`+counter+`" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "`+counter+`"
  if [ "$n" -le 2 ]; then
    printf '%s\n' "agentlab-control-plane	$FAKE_LIVE_STATE"
  else
    printf 'agentlab-control-plane\texited\n'
  fi ;;
esac`)

	t.Setenv("FAKE_LIVE_STATE", stateRunning)
	if err := waitForNodeExit("agentlab", time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	log := readCalls(t, calls)
	if n := strings.Count(log, dockerPSCall); n != 3 {
		t.Errorf("docker ps run %d times, want 3 (running, running, exited):\n%s", n, log)
	}
	if strings.Contains(log, "unpause") {
		t.Errorf("a running node must not be unpaused:\n%s", log)
	}

	_ = os.Remove(counter)
	_ = os.Remove(calls)
	t.Setenv("FAKE_LIVE_STATE", statePaused)
	if err := waitForNodeExit("agentlab", time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if log := readCalls(t, calls); strings.Count(log, "docker unpause agentlab-control-plane\n") != 2 {
		t.Errorf("a paused node is unpaused on every poll it is seen paused (2), got:\n%s", log)
	}

	// Never exits: the bound holds and the error says what is still alive.
	_ = os.Remove(counter)
	_ = os.Remove(calls)
	installFakeTool(t, dir, "docker", `printf 'agentlab-control-plane\trunning\n'`)
	err := waitForNodeExit("agentlab", 20*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("a node that never exits must fail the wait")
	}
	for _, want := range []string{"agentlab-control-plane (running)", "still alive", "journalctl -u docker", "agentlab down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error %q lacks %q", err, want)
		}
	}
}

// TestEnsureNodeRunning drives Up's node check against stand-ins for docker,
// kind's kubeconfig read and kubectl: a running node is left completely alone
// (no start, no kubeconfig export); an exited node is started, the kubeconfig
// exported and the apiserver probed before Up goes on; a paused node is
// unpaused; and a node docker cannot start fails by node, state and fix
// instead of as the opaque kubeconfig-read error it used to be.
func TestEnsureNodeRunning(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	stubKindKubeconfig(t)
	resetKindKubeconfigCache(t)
	kubectlCalls := installFakeTool(t, dir, "kubectl", "exit 0")
	dockerCalls := installFakeTool(t, dir, "docker", `
case "$1" in
inspect) echo "$FAKE_NODE_STATE" ;;
start|unpause)
  if [ -n "$FAKE_START_FAILS" ]; then
    echo "Error response from daemon: cannot start a dying container" >&2
    exit 1
  fi ;;
esac`)
	cfg := config.Default()
	const inspect = "docker inspect -f {{.State.Status}} agentlab-control-plane\n"

	t.Setenv("FAKE_NODE_STATE", stateRunning)
	if err := ensureNodeRunning(cfg); err != nil {
		t.Fatal(err)
	}
	if log := readCalls(t, dockerCalls); log != inspect {
		t.Errorf("running node: docker calls = %q, want the inspect alone", log)
	}
	if _, err := os.Stat(labKubeconfigPath); !os.IsNotExist(err) {
		t.Errorf("running node: kubeconfig export happened (%v); Up does that itself", err)
	}

	_ = os.Remove(dockerCalls)
	t.Setenv("FAKE_NODE_STATE", stateExited)
	if err := ensureNodeRunning(cfg); err != nil {
		t.Fatal(err)
	}
	if log, want := readCalls(t, dockerCalls), inspect+"docker start agentlab-control-plane\n"; log != want {
		t.Errorf("exited node: docker calls = %q, want %q", log, want)
	}
	if raw, err := os.ReadFile(labKubeconfigPath); err != nil || string(raw) != fakeKindKubeconfig {
		t.Errorf("exited node: kubeconfig not exported after the start (err %v)", err)
	}
	if log := readCalls(t, kubectlCalls); log != "kubectl get --raw=/readyz\n" {
		t.Errorf("exited node: kubectl calls = %q, want one /readyz probe", log)
	}

	_ = os.Remove(dockerCalls)
	t.Setenv("FAKE_NODE_STATE", statePaused)
	if err := ensureNodeRunning(cfg); err != nil {
		t.Fatal(err)
	}
	if log := readCalls(t, dockerCalls); !strings.Contains(log, "docker unpause agentlab-control-plane\n") {
		t.Errorf("paused node: docker calls = %q, want an unpause", log)
	}

	t.Setenv("FAKE_NODE_STATE", stateExited)
	t.Setenv("FAKE_START_FAILS", "1")
	err := ensureNodeRunning(cfg)
	if err == nil {
		t.Fatal("a node docker cannot start must fail Up")
	}
	for _, want := range []string{"agentlab-control-plane is exited", "cannot start a dying container", "agentlab up", "agentlab down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("start failure %q lacks %q", err, want)
		}
	}
}
