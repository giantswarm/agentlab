package lab

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/shaper"
)

// The shaper's container: the host's network namespace (the mirror's veth
// is there), the mirror's PID namespace (its process names the mirror's
// network namespace), NET_ADMIN and NET_RAW and nothing else asked for,
// this agentlab read-only in the probe image, the state directory, and a
// spec label that changes with the binary.
func TestShaperContainerSpec(t *testing.T) {
	args := shaperRunArgs("/home/me/.local/bin/agentlab", "/home/me/.local/state/agentlab/shaper", "abc123")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --name agentlab-shaper --network host",
		"--restart=always",
		"--label " + shaperSpecLabel + "=abc123",
		"--pid container:" + mirrorName,
		"--cap-add NET_ADMIN --cap-add NET_RAW",
		"--read-only",
		"-v /home/me/.local/bin/agentlab:/agentlab:ro",
		"-v /home/me/.local/state/agentlab/shaper:/state",
		"--entrypoint /agentlab " + probeImage + " shaper run --state-dir /state",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker %s\nlacks %q", joined, want)
		}
	}
	if strings.Contains(joined, "--privileged") {
		t.Error("the shaper must not run privileged")
	}
}

// A shaper's PID namespace is the mirror's in place when docker recorded
// the mirror's name or its current id; a recreated mirror (a new id) or none
// recreates the shaper.
func TestJoinsMirror(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for pidMode, want := range map[string]bool{
		"container:" + mirrorName:   true,
		"container:" + id:           true,
		"container:" + id[:12]:      true,
		"container:fedcba987654321": false,
		"private":                   false,
		"":                          false,
		"container:":                false,
	} {
		if got := joinsMirror(pidMode, id); got != want {
			t.Errorf("joinsMirror(%q) = %v, want %v", pidMode, got, want)
		}
	}
}

// bandwidthShaper off: nothing is created, and a shaper in place is left to
// the other labs with a note. On with the lab off the mirror, or on a
// machine that cannot run it: skipped with one line, nothing touched.
func TestEnsureShaperSkips(t *testing.T) {
	dir := t.TempDir()
	calls := installFakeTool(t, dir, "docker", `case "$1 $2" in
"inspect -f") exit 1 ;;
*) echo "unexpected docker $*" >&2; exit 2 ;;
esac`)
	prevUnavailable := shaperUnavailable
	t.Cleanup(func() { shaperUnavailable = prevUnavailable })
	cfg := config.Default()

	ensureShaper(cfg) // off by default
	cfg.SetBandwidthShaper(true)
	cfg.SetRegistryMirror(false)
	ensureShaper(cfg)
	cfg.SetRegistryMirror(true)
	shaperUnavailable = func(string) string { return "rootless Podman has no NET_ADMIN in the host's network namespace" }
	ensureShaper(cfg)

	for _, c := range callLines(t, calls) {
		if !strings.HasPrefix(c, "docker inspect -f ") {
			t.Errorf("a skipped shaper ran %q; want only reads", c)
		}
	}
}

func TestShaperStatusLines(t *testing.T) {
	fresh := &shaper.State{Mode: "auto", Shaping: true, Device: "veth9", CapBps: 12e6, MirrorBps: 11.4e6, DelayMs: 6, DelayKnown: true, Reason: "link has room, the mirror may take more"}
	for _, tc := range []struct {
		name string
		s    ShaperStatus
		want []string
	}{
		{"unavailable", ShaperStatus{On: true, State: registryMissing, Skipped: "rootless Podman has no NET_ADMIN"}, []string{"skipped: rootless Podman"}},
		{"missing", ShaperStatus{On: true, State: registryMissing}, []string{"agentlab-shaper missing", "`agentlab platform` creates it"}},
		{"exited", ShaperStatus{On: true, State: stateExited}, []string{"agentlab-shaper exited", "`agentlab platform` starts it"}},
		{"stale", ShaperStatus{On: true, State: stateRunning, Shaper: fresh, Age: time.Minute}, []string{"its state is 1m0s old"}},
		{"shaping", ShaperStatus{On: true, State: stateRunning, Shaper: fresh, Age: time.Second}, []string{
			"agentlab-shaper running: the mirror capped at 12.0 Mbit/s (auto), getting 11.4 Mbit/s, the link +6 ms over its baseline",
		}},
		{"another lab's", ShaperStatus{State: stateRunning, Shaper: fresh, Age: time.Second}, []string{"this lab is off it (bandwidthShaper)"}},
	} {
		got := tc.s.String()
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", tc.name, got, w)
			}
		}
	}
}

// readShaperStatus leaves the line out for a lab that has the shaper off
// while none is in place: off by default, it would otherwise be noise in
// every status.
func TestReadShaperStatusOmitsAnAbsentOffShaper(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	installFakeTool(t, dir, "docker", `exit 1`)
	cfg := config.Default()
	if s := readShaperStatus(cfg); s != nil {
		t.Errorf("an off lab without a shaper got a line: %v", s)
	}
	cfg.SetBandwidthShaper(true)
	if s := readShaperStatus(cfg); s == nil || !s.On {
		t.Errorf("an on lab without a shaper: %+v, want a line", s)
	}
}

func TestShaperSpecFollowsTheBinary(t *testing.T) {
	dir := t.TempDir()
	bin := dir + "/agentlab"
	writeFile := func(content string, mtime time.Time) {
		t.Helper()
		if err := writeIfChanged(bin, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(bin, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("v1", time.Unix(1_800_000_000, 0))
	a, err := shaperSpec(bin, dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile("v2!", time.Unix(1_800_000_100, 0))
	b, _ := shaperSpec(bin, dir)
	if a == b {
		t.Error("a new binary kept the shaper's spec: it would not upgrade")
	}
	if c, _ := shaperSpec(bin, dir); c != b {
		t.Error("the same binary changed the spec: every run would recreate the shaper")
	}
	if slices.Contains([]string{a, b}, "") {
		t.Error("an empty spec")
	}
}
