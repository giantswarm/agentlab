package lab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A Session's turns on a workspace as the workspace proofs and the
// measurement read them: the probe's marker lines parsed, the session
// directories' owners on the volume, and the setup's record of a directory.

const (
	// sessionsOwnFilePrefix names the file each Session writes into its own
	// directory, followed by a token only it knows.
	sessionsOwnFilePrefix = "agentlab-only-"
	// sessionsCleanupArg is the workspace-manager's flag of the window.
	sessionsCleanupArg = "--sessions-cleanup-after="
	// sessionsKagentArg is the workspace-manager's flag of the kagent
	// controller its cleanup lists Sessions from.
	sessionsKagentArg = "--kagent-address="
	// workspaceResource is the workspace-manager's Workspace.
	workspaceResource          = "workspaces.workspace-manager.giantswarm.io"
	conditionSessionsCleanedUp = "SessionsCleanedUp"
	// workspaceStateFile is the setup's record in a session directory.
	workspaceStateFile = ".kagent/workspace.json"
	// startSessionRequestIDArg is start_session's idempotency key,
	// startSessionMessageArg its first message, startSessionGrantArg its
	// workspace grant.
	startSessionRequestIDArg = "requestId"
	startSessionMessageArg   = "message"
	startSessionGrantArg     = "workspaceGrant"
	// The probes' words: a write refused, a directory absent, a mirror's
	// head.
	probeRefused = "refused"
	probeAbsent  = "absent"
	probeHeadKey = "head"
)

// sessionsTurnValues is what a probe turn relayed.
type sessionsTurnValues struct {
	pwd          string
	heads        map[string]string
	entries      string
	ownWrite     string
	mirrorsWrite string
	mirrors      string
	seen         []string
	own          []string
}

var sessionsRepoLine = regexp.MustCompile(probeMarker + ` repo=(\S+) (\S+)`)

// parseSessionsTurn reads the marker lines of a reply.
func parseSessionsTurn(reply string) (sessionsTurnValues, error) {
	values := map[string]string{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		if m[1] != "repo" {
			values[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
		}
	}
	if values["pwd"] == "" {
		return sessionsTurnValues{}, fmt.Errorf("the reply carries no %s pwd line (did the agent run its Bash tool?): %s", probeMarker, excerpt(reply, 300))
	}
	v := sessionsTurnValues{pwd: values["pwd"], entries: values["entries"], ownWrite: values["own-write"], mirrorsWrite: values["mirrors-write"],
		mirrors: values["mirrors"], seen: strings.Fields(values["seen"]), own: strings.Fields(values["own"]), heads: map[string]string{}}
	for _, m := range sessionsRepoLine.FindAllStringSubmatch(reply, -1) {
		v.heads[m[1]] = strings.Trim(m[2], "`")
	}
	return v, nil
}

// sessionsFirstTurnCommand lists the working directory and each clone's
// origin/HEAD, and writes the file only this Session knows.
func sessionsFirstTurnCommand(token string) string {
	return `echo "` + probeMarker + ` pwd=$(pwd)"; ` +
		`for g in $(find . -mindepth 3 -maxdepth 3 -name .git | sort); do r=${g#./}; r=${r%/.git}; echo "` + probeMarker + ` repo=$r $(git -C "$r" rev-parse origin/HEAD 2>&1 | head -n 1 | tr ' ' '_')"; done; ` +
		`echo "` + probeMarker + ` entries=$(ls -A | tr '\n' ' ')"; ` +
		`echo ` + token + ` > ./` + sessionsOwnFilePrefix + token + ` && echo "` + probeMarker + ` own-write=$(cat ./` + sessionsOwnFilePrefix + token + `)"`
}

// headsLine words a clone's heads.
func headsLine(heads map[string]string) string {
	var out []string
	for _, repo := range slices.Sorted(maps.Keys(heads)) {
		out = append(out, repo+" at "+shortSHA(heads[repo]))
	}
	if len(out) == 0 {
		return "no clones"
	}
	return "clones " + strings.Join(out, ", ")
}

// sessionDirectory is a session directory as the volume probe reads it.
type sessionDirectory struct {
	state *workspaceState
	own   []string
}

// workspaceState is the setup's record, .kagent/workspace.json.
type workspaceState struct {
	SetUp    time.Time `json:"setUp"`
	Cloned   []string  `json:"cloned"`
	Fetched  []string  `json:"fetched"`
	Problems []struct {
		Repository string `json:"repository"`
		Reason     string `json:"reason"`
	} `json:"problems"`
}

// sessionsDirectoriesScript lists every session directory on the volume:
// its setup record, base64, and the Session-only files in it.
const sessionsDirectoriesScript = `for d in /workspace/` + workspacesTestSessions + `/*/; do test -d "$d" || continue; id=$(basename "$d"); ` +
	`state=$(base64 -w0 "$d` + workspaceStateFile + `" 2>/dev/null); ` +
	`own=$(ls -A "$d" | grep '^` + sessionsOwnFilePrefix + `' | tr '\n' ','); ` +
	`echo "dir $id state=$state own=$own"; done; true
`

// parseSessionDirectories reads the probe's lines into each directory.
func parseSessionDirectories(out string) (map[string]sessionDirectory, error) {
	dirs := map[string]sessionDirectory{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "dir" {
			continue
		}
		var d sessionDirectory
		for _, f := range fields[2:] {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "state":
				if v == "" {
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(v)
				if err != nil {
					return nil, fmt.Errorf("the setup record of %s: %w", fields[1], err)
				}
				var s workspaceState
				if err := json.Unmarshal(raw, &s); err != nil {
					return nil, fmt.Errorf("the setup record of %s: %w", fields[1], err)
				}
				d.state = &s
			case "own":
				d.own = strings.FieldsFunc(v, func(r rune) bool { return r == ',' })
			}
		}
		dirs[fields[1]] = d
	}
	return dirs, nil
}

// workspaceManagerFlags are the workspace-manager container's arguments.
func workspaceManagerFlags(ctx context.Context) ([]string, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, workspaceManagerMCPServer, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the Deployment %s/%s: %w", platformNamespace, workspaceManagerMCPServer, err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == workspaceManagerMCPServer || len(d.Spec.Template.Spec.Containers) == 1 {
			return append(slices.Clone(c.Command), c.Args...), nil
		}
	}
	return nil, fmt.Errorf("the Deployment %s/%s has no container %s", platformNamespace, workspaceManagerMCPServer, workspaceManagerMCPServer)
}

// flagValue is the value of a --name= argument, "" without one.
func flagValue(args []string, prefix string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

// judgeKagentAddress is the verdict on the manager's kagent: without its
// address no session directory is ever cleaned up.
func judgeKagentAddress(args []string) error {
	if flagValue(args, sessionsKagentArg) == "" {
		return fmt.Errorf("the workspace-manager runs without kagent (no %s argument): no session directory is ever cleaned up, every Workspace reads %s=False KagentNotConfigured; agent-platform derives workspace-manager.kagent.address from 4.128.0 (the component's range floor at its kagent floor), or the values set it", strings.TrimSuffix(sessionsKagentArg, "="), conditionSessionsCleanedUp)
	}
	return nil
}
