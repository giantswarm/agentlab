package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/shaper"
	"github.com/giantswarm/agentlab/internal/xdg"
)

// The bandwidth shaper (bandwidthShaper, docs/bandwidth.md): one container
// per machine, beside the registry mirror and shared by every lab like it,
// that caps what the mirror downloads from gsoci by the uplink's latency
// (internal/shaper). Everything the mirror downloads reaches it through one
// interface on the host, the host side of its veth on the kind network, so
// one CAKE qdisc as that veth's root caps the labs' node lane and nothing
// else, and goes away with the mirror's container.
//
// The shaper runs this agentlab (`agentlab shaper run`) in the lab's probe
// image, the way the proofs' fakes do, with the host's network namespace
// (where the veth is) and the mirror's PID namespace: the mirror's own
// process, PID 1 there, names the mirror's network namespace, so the shaper
// finds the veth by asking the kernel which veth leads there — no docker
// socket, and a recreated mirror's new veth is found the same way. It needs
// NET_ADMIN for the qdisc and NET_RAW for its latency probes (Podman's
// default capabilities lack it); the lab's nodes run privileged, so this asks
// for nothing new. A shaper that cannot shape (no sch_cake in the kernel, no
// NET_ADMIN) says so in its state and leaves the mirror as it is.

// shaperName is the shaper's container.
const shaperName = "agentlab-shaper"

// Inside the shaper's container: this agentlab, read-only, and the state
// directory it publishes in and reads the person's mode from.
const (
	shaperBinary   = "/agentlab"
	shaperStateDir = "/state"
)

// shaperSpecLabel carries the digest of what the container was made from:
// this agentlab (its path, size and modification time), the image and the
// flags. A changed one recreates the shaper, so it upgrades with agentlab.
const shaperSpecLabel = "agentlab.giantswarm.io/shaper-spec"

// shaperStopSeconds is how long `docker stop` gives the shaper to take CAKE
// off before it kills it.
const shaperStopSeconds = 10

// shaperStartWait bounds the wait for a started shaper's first state.
const shaperStartWait = 8 * time.Second

// shaperStateHome is the shaper's state directory on the host, beside the
// mirror's configuration in agentlab's state folder.
func shaperStateHome() (string, error) {
	state, err := xdg.StateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "shaper"), nil
}

// shaperBinaryPath is this agentlab, links resolved: the file the shaper's
// container mounts.
var shaperBinaryPath = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// dockerRootless reports whether the docker engine runs rootless: its
// "host" network namespace is the engine's own, not the host's. Cached for
// the process.
var dockerRootless = sync.OnceValue(func() bool {
	out, err := outputQuiet(dockerBin, "info", "-f", "{{.SecurityOptions}}")
	return err == nil && strings.Contains(out, "rootless")
})

// shaperUnavailable says why this machine runs no shaper, "" when it can:
// the shaper is a Linux binary in a Linux container with NET_ADMIN in the
// engine's host network namespace.
var shaperUnavailable = func(binary string) string {
	switch {
	case runtime.GOOS != goosLinux:
		return fmt.Sprintf("it runs this agentlab in a Linux container, and this one is a %s build", runtime.GOOS)
	case rootlessPodmanOnLinux():
		return "rootless Podman has no NET_ADMIN in the host's network namespace"
	case dockerRootless():
		return "rootless Docker has no NET_ADMIN in the host's network namespace"
	}
	if err := linuxStaticBinary(binary, "the bandwidth shaper", ""); err != nil {
		return err.Error()
	}
	return ""
}

// shaperFlags are the shaper container's own flags, for its `docker run` and
// for the one-shot cleanup.
func shaperFlags(binary, stateDir string) []string {
	return []string{
		"--pid", "container:" + mirrorName,
		"--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW",
		"--read-only",
		"-v", binary + ":" + shaperBinary + ":ro",
		"-v", stateDir + ":" + shaperStateDir,
		"--entrypoint", shaperBinary,
	}
}

// shaperRunArgs is the shaper's `docker run`, restarted with the docker
// daemon.
func shaperRunArgs(binary, stateDir, spec string) []string {
	flags := append([]string{"-d", "--restart=always", "--label", shaperSpecLabel + "=" + spec}, shaperFlags(binary, stateDir)...)
	args := dockerRun(shaperName, "host", flags...)
	return append(args, probeImage, "shaper", "run", "--state-dir", shaperStateDir)
}

// shaperSpec is the digest the container carries in shaperSpecLabel.
func shaperSpec(binary, stateDir string) (string, error) {
	fi, err := os.Stat(binary)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	_, _ = fmt.Fprintf(sum, "%s\x00%d\x00%d\x00%s", binary, fi.Size(), fi.ModTime().UnixNano(), strings.Join(shaperRunArgs(binary, stateDir, ""), "\x00"))
	return hex.EncodeToString(sum.Sum(nil))[:16], nil
}

// shaperContainer is the shaper's container as docker reports it.
type shaperContainer struct {
	state, spec, pidMode string
}

// inspectShaper reads the shaper's container; false when there is none.
func inspectShaper() (shaperContainer, bool) {
	out, err := outputQuiet(dockerBin, "inspect", "-f",
		"{{.State.Status}}\t{{index .Config.Labels \""+shaperSpecLabel+"\"}}\t{{.HostConfig.PidMode}}", shaperName)
	if err != nil {
		return shaperContainer{}, false
	}
	f := strings.Split(strings.TrimRight(out, "\r\n"), "\t")
	for len(f) < 3 {
		f = append(f, "")
	}
	return shaperContainer{state: f[0], spec: f[1], pidMode: f[2]}, true
}

// mirrorContainerID is the registry mirror's container id; "" when there is
// none.
func mirrorContainerID() string {
	out, err := outputQuiet(dockerBin, "inspect", "-f", "{{.Id}}", mirrorName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// joinsMirror says whether a shaper's PID namespace is the mirror's in
// place: docker records the name it was given or the id it resolved it to.
func joinsMirror(pidMode, mirrorID string) bool {
	target, ok := strings.CutPrefix(pidMode, "container:")
	return ok && target != "" && (target == mirrorName || mirrorID != "" && strings.HasPrefix(mirrorID, target))
}

// ensureShaper applies bandwidthShaper: on, the shaper running on this
// agentlab, joined to the mirror in place (created, recreated or started);
// off, a shaper in place left to the other labs. Never a reason to stop:
// without it the mirror downloads as it would anyway.
func ensureShaper(cfg *config.Config) {
	have, exists := inspectShaper()
	if !cfg.BandwidthShaperOn() {
		if exists {
			note("bandwidthShaper is off for this lab; %s (%s) stays for the other labs (`agentlab shaper remove` removes it)", shaperName, have.state)
		}
		return
	}
	if !cfg.RegistryMirrorOn() {
		note("bandwidthShaper caps the registry mirror, and this lab's nodes are off it (registryMirror: false): nothing to shape for this lab")
		return
	}
	binary, err := shaperBinaryPath()
	if err != nil {
		warn("the bandwidth shaper: finding this agentlab's binary: %v; the mirror's downloads are not limited", err)
		return
	}
	if why := shaperUnavailable(binary); why != "" {
		note("the bandwidth shaper is skipped: %s; the mirror's downloads are not limited", why)
		return
	}
	mirrorID := mirrorContainerID()
	if mirror, ok := inspectZot(mirrorName); !ok || mirror.state != stateRunning || mirrorID == "" {
		warn("the bandwidth shaper joins %s, which is not running; the mirror's downloads are not limited until it is", mirrorName)
		return
	}
	step("Ensuring the bandwidth shaper %s", shaperName)
	stateDir, err := shaperStateHome()
	if err == nil {
		err = os.MkdirAll(stateDir, 0o755) // #nosec G301 -- the shaper's container, root, writes its state there
	}
	if err != nil {
		warn("the bandwidth shaper's state directory: %v; the mirror's downloads are not limited", err)
		return
	}
	spec, err := shaperSpec(binary, stateDir)
	if err != nil {
		warn("the bandwidth shaper: %v; the mirror's downloads are not limited", err)
		return
	}
	if len(hostPullImages([]string{probeImage})) == 0 {
		warn("the bandwidth shaper runs in %s, which could not be pulled (`docker pull %s`); the mirror's downloads are not limited", probeImage, probeImage)
		return
	}
	// A shaper started here reports a state newer than this moment; one
	// left running, any state fresh enough.
	since, begun := time.Now().Add(-shaper.StaleAfter), time.Now()
	switch {
	case !exists:
		since = begun
		err = startShaperContainer(binary, stateDir, spec)
		if err == nil {
			note("created %s (this agentlab in %s, on the host network, joined to %s)", shaperName, probeImage, mirrorName)
		}
	case have.spec != spec || !joinsMirror(have.pidMode, mirrorID):
		why := "this agentlab changed"
		if have.spec == spec {
			why = mirrorName + " was recreated"
		}
		since = begun
		if err = removeShaperContainer(have); err == nil {
			err = startShaperContainer(binary, stateDir, spec)
		}
		if err == nil {
			note("recreated %s (%s)", shaperName, why)
		}
	case have.state != stateRunning:
		// Starting it resets the cap a killed shaper left on the veth: the
		// shaper takes the veth over from whatever it finds there.
		since = begun
		// One that cannot start (its mirror recreated under it) is made anew.
		if _, err = outputQuiet(dockerBin, verbStart, shaperName); err != nil {
			if err = removeShaperContainer(have); err == nil {
				err = startShaperContainer(binary, stateDir, spec)
			}
		}
		if err == nil {
			note("started %s (was %s)", shaperName, have.state)
		}
	}
	if err != nil {
		warn("the bandwidth shaper: %v; the mirror's downloads are not limited", err)
		return
	}
	reportShaper(stateDir, since)
}

// startShaperContainer runs a new shaper.
func startShaperContainer(binary, stateDir, spec string) error {
	if _, err := outputQuiet(dockerBin, shaperRunArgs(binary, stateDir, spec)...); err != nil {
		_, _ = outputQuiet(dockerBin, "rm", "-f", shaperName)
		return fmt.Errorf("creating %s: %w", shaperName, err)
	}
	return nil
}

// reportShaper says what the running shaper does, once it published a state
// newer than since that its latency probes speak in (on a network that drops
// ICMP, the last one within shaperStartWait); a shaper that publishes
// nothing in time is named with its log.
func reportShaper(stateDir string, since time.Time) {
	var st shaper.State
	published := false
	waitFor(int(shaperStartWait/(250*time.Millisecond)), 250*time.Millisecond, func() bool {
		s, err := shaper.ReadState(stateDir)
		if err != nil || !s.Updated.After(since) {
			return false
		}
		st, published = s, true
		return st.Problem != "" || st.Shaping && st.DelayKnown
	})
	if !published {
		warn("%s published no state within %s (`docker logs %s`)", shaperName, shaperStartWait, shaperName)
		return
	}
	if st.Problem != "" {
		warn("%s runs, but %s", shaperName, st.Problem)
		return
	}
	note("%s shapes %s's downloads on %s: %s", shaperName, mirrorName, st.Device, st.Summary())
}

// removeShaperContainer removes the shaper the way that takes CAKE off the
// veth: a running one is stopped (SIGTERM, then shaperStopSeconds to take it
// off), one that is not running gets a one-shot cleanup first for the cap
// it may have left when it was killed.
func removeShaperContainer(have shaperContainer) error {
	if have.state == stateRunning || have.state == statePaused {
		if have.state == statePaused {
			_ = runQuiet(dockerBin, verbUnpause, shaperName)
		}
		_ = runQuiet(dockerBin, "stop", "-t", strconv.Itoa(shaperStopSeconds), shaperName)
	} else {
		cleanupShaperCap()
	}
	if err := runQuiet(dockerBin, "rm", "-f", shaperName); err != nil {
		return fmt.Errorf("removing %s: %w", shaperName, err)
	}
	return nil
}

// cleanupShaperCap runs the shaper once to take CAKE off the mirror's veth;
// nothing to do without a running mirror (its veth went with it).
func cleanupShaperCap() {
	if mirror, ok := inspectZot(mirrorName); !ok || mirror.state != stateRunning {
		return
	}
	binary, err := shaperBinaryPath()
	if err != nil || shaperUnavailable(binary) != "" {
		return
	}
	stateDir, err := shaperStateHome()
	if err != nil {
		return
	}
	args := dockerRun(shaperName+"-cleanup", "host", append([]string{dockerRemoveOnExit}, shaperFlags(binary, stateDir)...)...)
	args = append(args, probeImage, "shaper", "cleanup")
	if out, err := outputQuiet(dockerBin, args...); err != nil {
		note("taking a stopped shaper's cap off %s's veth failed (%v): %s", mirrorName, err, strings.TrimSpace(out))
	}
}

// RemoveShaper removes the shaper, its cap taken off the mirror's veth, for
// `agentlab shaper remove` and `agentlab down --mirror`. The labs' next `up`
// or `platform` with bandwidthShaper on creates it again.
func RemoveShaper() error {
	have, ok := inspectShaper()
	if !ok {
		return nil
	}
	if err := removeShaperContainer(have); err != nil {
		return err
	}
	note("removed the bandwidth shaper %s; the mirror's downloads are not limited", shaperName)
	return nil
}

// RunShaper is `agentlab shaper run`, the shaper in its container.
func RunShaper(ctx context.Context, cfg shaper.Config) error {
	return shaper.Run(ctx, cfg, log.New(os.Stdout, "", log.LstdFlags))
}

// CleanupShaper is `agentlab shaper cleanup`: CAKE off the mirror's veth.
func CleanupShaper() error {
	return shaper.Cleanup(1, log.New(os.Stdout, "", log.LstdFlags))
}

// shaperModeWait bounds how long `agentlab shaper <mode>` waits for the
// shaper to take the mode.
const shaperModeWait = 3 * time.Second

// SetShaperMode is `agentlab shaper auto|max|pause|off`: the person's mode
// for every lab on this machine, left in the shaper's state directory; it
// waits for a running shaper to take it and prints what it does then.
func SetShaperMode(w io.Writer, m shaper.Mode) error {
	dir, err := shaperStateHome()
	if err != nil {
		return err
	}
	if err := shaper.WriteMode(dir, m); err != nil {
		return err
	}
	if have, ok := inspectShaper(); !ok || have.state != stateRunning {
		_, err := fmt.Fprintf(w, "saved %q; %s is not running — `agentlab platform` with bandwidthShaper on starts it, and it takes the mode then\n", m.String(), shaperName)
		return err
	}
	var st shaper.State
	if !waitFor(int(shaperModeWait/(100*time.Millisecond)), 100*time.Millisecond, func() bool {
		var err error
		st, err = shaper.ReadState(dir)
		return err == nil && st.Mode == m.String() && time.Since(st.Updated) < shaper.StaleAfter
	}) {
		return fmt.Errorf("saved %q, but %s has not taken it within %s (`docker logs %s`)", m.String(), shaperName, shaperModeWait, shaperName)
	}
	_, err = io.WriteString(w, st.Describe())
	return err
}

// ShaperStatus is the shaper as `agentlab status` and `agentlab shaper
// status` report it.
type ShaperStatus struct {
	// On is the lab's bandwidthShaper; false when read without a lab.
	On bool `json:"on"`
	// State is the container's docker state, "missing" when there is none.
	State string `json:"state"`
	// Skipped says why this machine runs no shaper; "" when it can.
	Skipped string `json:"skipped,omitempty"`
	// Shaper is what the shaper published last; nil when it published
	// nothing.
	Shaper *shaper.State `json:"shaper,omitempty"`
	// Age is how old that is.
	Age time.Duration `json:"age,omitempty"`
}

// readShaperStatus reads the shaper on this machine; nil for a lab that has
// it off while no shaper is in place, which leaves the status without the
// line.
func readShaperStatus(cfg *config.Config) *ShaperStatus {
	s := ReadShaperStatus()
	s.On = cfg != nil && cfg.BandwidthShaperOn()
	if !s.On && s.State == registryMissing {
		return nil
	}
	return s
}

// ReadShaperStatus reads the shaper on this machine, without a lab.
func ReadShaperStatus() *ShaperStatus {
	s := &ShaperStatus{State: registryMissing}
	if binary, err := shaperBinaryPath(); err == nil {
		s.Skipped = shaperUnavailable(binary)
	}
	if have, ok := inspectShaper(); ok {
		s.State = have.state
	}
	if dir, err := shaperStateHome(); err == nil {
		if st, err := shaper.ReadState(dir); err == nil {
			s.Shaper, s.Age = &st, time.Since(st.Updated).Round(time.Second)
		}
	}
	return s
}

// String is the status line on the shaper.
func (s *ShaperStatus) String() string {
	off := ""
	if !s.On {
		off = " — this lab is off it (bandwidthShaper)"
	}
	switch {
	case s.Skipped != "" && s.State == registryMissing:
		return "skipped: " + s.Skipped
	case s.State == registryMissing:
		return shaperName + " missing — the mirror's downloads are not limited; `agentlab platform` creates it"
	case s.State != stateRunning:
		return fmt.Sprintf("%s %s — the mirror's downloads are not limited, unless it was killed while it capped them; `agentlab platform` starts it%s", shaperName, s.State, off)
	case s.Shaper == nil:
		return fmt.Sprintf("%s running, no state published yet (`docker logs %s`)%s", shaperName, shaperName, off)
	case s.Age > shaper.StaleAfter:
		return fmt.Sprintf("%s running, its state is %s old (`docker logs %s`)%s", shaperName, s.Age, shaperName, off)
	}
	return shaperName + " running: " + s.Shaper.Summary() + off
}

// PrintShaperStatus is `agentlab shaper status`: what the shaper does, or
// why it does nothing, as text or JSON.
func PrintShaperStatus(w io.Writer, asJSON bool) error {
	s := ReadShaperStatus()
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	if s.State != stateRunning || s.Shaper == nil || s.Age > shaper.StaleAfter {
		_, err := fmt.Fprintln(w, s.String())
		return err
	}
	_, err := io.WriteString(w, s.Shaper.Describe())
	return err
}
