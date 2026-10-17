package lab

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The sandbox-tree spike times git inside the Session's own sandbox (the
// Harness's runtime, not a probe pod) with the working tree in four places:
// a second clone on the workspace volume, a clone with alternates in the
// actor's durable /data, a full copy of the mirror in /data and a clone in
// the sandbox's /tmp. A second turn, after the first turn's end suspended the
// actor, says which trees survived and times git status again. The raw
// lines go to sandbox-tree.txt in the run directory.

const sandboxTreeFile = "sandbox-tree.txt"

// sandboxTreeCommand is the first turn: each location's clone, first
// checkout, cold and warm status, a status after touching 50 files, a
// commit, and its size; then the mounts.
// sandboxTreePrelude defines the clock, the /data directory, the mirror and
// one(), which times one location.
func sandboxTreePrelude(repo, branch string) string {
	return `ms() { e=${EPOCHREALTIME/./}; echo "${e:0:13}"; }; echo "` + probeMarker + ` pwd=$(pwd)"; echo "` + probeMarker + ` clock=epochrealtime=$EPOCHREALTIME date=$(date +%s%3N | tr ' ' _)"; git config --global --add safe.directory '*'; ` +
		`dd=$(for x in "$HOME" /data/workspace /data/home /data; do case "$x" in /data*) [ -d "$x" ] && [ -w "$x" ] && { echo "$x"; break; };; esac; done); [ -n "$dd" ] || dd=$(find /data -maxdepth 3 -type d -writable 2>/dev/null | head -n 1); ` +
		`echo "` + probeMarker + ` who=uid=$(id -u) home=$HOME datadir=${dd:-none} data-perm=$(stat -c %U:%a /data 2>&1 | tr ' ' '_')"; ` +
		`c=/workspace/` + repo + `; alt=$(head -n 1 "$c/.git/objects/info/alternates"); mirror=$(dirname "$alt"); b=` + branch + `; ` +
		`one() { loc=$1; d=$2; mode=$3; rm -rf "$d"; mkdir -p "$(dirname "$d")" || { echo "` + probeMarker + ` tree=loc=$loc error=mkdir"; return; }; ` +
		`a=$(ms); if [ "$mode" = full ]; then git clone -q --no-local --no-checkout "$mirror" "$d" 2>/tmp/.spike-clone-err; else git clone -q --shared --no-checkout "$mirror" "$d" 2>/tmp/.spike-clone-err; fi; rc=$?; z=$(ms); cl=$((z-a)); [ $rc = 0 ] || { echo "` + probeMarker + ` tree=loc=$loc error=clone-$(head -c 160 /tmp/.spike-clone-err | tr ' \n' '__')"; return; }; ` +
		`git -C "$d" config checkout.workers 0; git -C "$d" config core.untrackedCache true; git -C "$d" config feature.manyFiles true; ` +
		`b=$(git -C "$d" symbolic-ref --short -q HEAD); [ -n "$b" ] || b=$(git -C "$d" for-each-ref --format='%(refname:lstrip=3)' refs/remotes/origin | grep -v '^HEAD$' | head -n 1); a=$(ms); git -C "$d" switch -q -C spike-tree "origin/$b" 2>/tmp/.spike-clone-err; rc=$?; z=$(ms); co=$((z-a)); [ $rc = 0 ] || { echo "` + probeMarker + ` tree=loc=$loc error=switch-$(head -c 160 /tmp/.spike-clone-err | tr ' \n' '__')"; return; }; ` +
		`a=$(ms); git -C "$d" status --porcelain >/dev/null; z=$(ms); s1=$((z-a)); ` +
		`a=$(ms); git -C "$d" status --porcelain >/dev/null; z=$(ms); s2=$((z-a)); ` +
		`git -C "$d" ls-files | head -n 50 | while read -r f; do echo "// spike" >> "$d/$f"; done; ` +
		`a=$(ms); git -C "$d" status --porcelain >/dev/null; z=$(ms); s3=$((z-a)); ` +
		`a=$(ms); git -C "$d" -c user.name=spike -c user.email=spike@example.com -c commit.gpgsign=false commit -qam spike >/dev/null; z=$(ms); cm=$((z-a)); ` +
		`echo "` + probeMarker + ` tree=loc=$loc clone=$cl checkout=$co status-cold=$s1 status-warm=$s2 status-dirty=$s3 commit=$cm bytes=$(du -sb "$d" | cut -f1) files=$(git -C "$d" ls-files | wc -l)"; }; ` +
		``
}

// sandboxTreeLocations are the first turns, one per location, so a slow
// location cannot run a turn past the gateway's stream timeout.
var sandboxTreeLocations = []string{
	`one volume /workspace/.spike-tree/clone shared`,
	`one data-shared $dd/.spike-tree/shared shared`,
	`one data-full $dd/.spike-tree/full full`,
	`one tmp /tmp/.spike-tree/clone shared`,
}

// sandboxTreeInfo is the turn after them: the mounts, the free space and
// what each tree packs to.
const sandboxTreeInfo = `echo "` + probeMarker + ` mounts=$(awk '$2=="/" || $2 ~ /^\/(data|tmp|workspace|mirrors)/ {printf "%s:%s:%s ", $2, $3, $1}' /proc/mounts)"; ` +
	`echo "` + probeMarker + ` df=$(df -kP / /data /tmp /workspace 2>/dev/null | awk 'NR>1 {printf "%s:%sK-used:%sK-avail ", $6, $3, $4}')"; ` +
	`echo "` + probeMarker + ` data-bytes=$(du -sb /data 2>/dev/null | cut -f1)"; ` +
	`for p in data-shared:$dd/.spike-tree/shared data-full:$dd/.spike-tree/full tmp:/tmp/.spike-tree/clone; do echo "` + probeMarker + ` packed=loc=${p%%:*} gzip1=$(tar -C "${p#*:}" -cf - . 2>/dev/null | gzip -1 | wc -c)"; done`

// sandboxTreeAfterCommand is the second turn, after a suspend and resume:
// which trees are still there, with their spike commit, and git status in
// each.
const sandboxTreeAfterCommand = `ms() { e=${EPOCHREALTIME/./}; echo "${e:0:13}"; }; echo "` + probeMarker + ` pwd=$(pwd)"; ` +
	`dd=$(find /data -maxdepth 4 -type d -name .spike-tree 2>/dev/null | head -n 1); dd=${dd%/.spike-tree}; ` +
	`for p in volume:/workspace/.spike-tree/clone data-shared:$dd/.spike-tree/shared data-full:$dd/.spike-tree/full tmp:/tmp/.spike-tree/clone; do loc=${p%%:*}; d=${p#*:}; ` +
	`if [ -d "$d/.git" ]; then a=$(ms); git -C "$d" status --porcelain >/dev/null 2>&1; rc=$?; z=$(ms); echo "` + probeMarker + ` after=loc=$loc present=yes status-cold=$((z-a)) rc=$rc head=$(git -C "$d" log -1 --format=%s 2>&1 | head -n 1 | tr ' ' '_')"; ` +
	`else echo "` + probeMarker + ` after=loc=$loc present=no"; fi; done; ` +
	`echo "` + probeMarker + ` data-bytes=$(du -sb /data 2>/dev/null | cut -f1)"`

// sandboxTree runs the two turns on the Session and appends what they
// reported to the run directory's sandbox-tree.txt; the snapshot the first
// turn's end left is read by actorSnapshots in between.
func (m *measurement) sandboxTree(ctx context.Context, w *measureWorkspace, ks *apiv1alpha1.Session) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s session=%s repo=%s at=%s\n", w.name, ks.GetId(), w.worked, time.Now().UTC().Format(time.RFC3339))
	prelude := sandboxTreePrelude(w.worked, w.branch)
	for _, step := range append(slices.Clone(sandboxTreeLocations), sandboxTreeInfo) {
		reply, _, err := measureTurn(ctx, m.api, ks, prelude+step)
		if err != nil {
			warn("%s: sandbox tree: %v", w.name, err)
			fmt.Fprintf(&b, "error first-turn %v\n", err)
		}
		for _, mm := range probeLine.FindAllStringSubmatch(reply, -1) {
			fmt.Fprintf(&b, "%s=%s\n", mm[1], strings.Trim(strings.TrimSpace(mm[2]), "`"))
		}
	}
	// The first turn's end suspends the actor (kagent's Quiesce): wait for
	// the snapshot to be taken before the second turn resumes it.
	time.Sleep(45 * time.Second)
	fmt.Fprintf(&b, "snapshot-wait=45s\n")
	// SPIKE_AFTER_SUSPEND reads the store while the actor is suspended
	// (the lab: du of the RustFS bucket).
	if c := os.Getenv("SPIKE_AFTER_SUSPEND"); c != "" {
		out, _ := exec.Command("sh", "-c", c).CombinedOutput() // #nosec G204 -- the spike's own command
		fmt.Fprintf(&b, "store:\n%s\n", out)
	}
	reply, _, err := measureTurn(ctx, m.api, ks, sandboxTreeAfterCommand)
	if err != nil {
		warn("%s: sandbox tree after the resume: %v", w.name, err)
		fmt.Fprintf(&b, "error second-turn %v\n", err)
	}
	for _, mm := range probeLine.FindAllStringSubmatch(reply, -1) {
		fmt.Fprintf(&b, "%s=%s\n", mm[1], strings.Trim(strings.TrimSpace(mm[2]), "`"))
	}
	note("%s: sandbox tree:\n%s", w.name, b.String())
	if m.runDir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(m.runDir, sandboxTreeFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		warn("sandbox tree: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}
