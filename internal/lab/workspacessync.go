package lab

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// A workspace as the workspace proofs read it through the workspace-manager's
// tools, through muster as a signed-in member of the lab's Organization:
// get_workspace's detail, its cycles waited for, its claim and volume, the
// manifest and its mirrors probed on the volume, and its delete.

const (
	// syncListingFreshness is the workspace-manager's listing cache for a
	// cycle that is not requested, a minute, and a margin.
	syncListingFreshness = time.Minute + 5*time.Second
	// syncWorkspaceLabel labels a workspace's claim with its name.
	syncWorkspaceLabel = "workspace-manager.giantswarm.io/workspace"
	syncManifestKey    = "manifest.json"
	syncResultFailed   = "Failed"
	syncResultNoChange = "NoChange"
	syncResultSucceed  = "Succeeded"
	syncMainBranch     = "main"
	syncPushedFile     = "sync-proof.md"
	// The workspace-manager tools' argument names.
	wmWorkspaceArg    = "workspace"
	wmOrganizationArg = "organization"
	wmOwnerArg        = "owner"
	wmSourcesArg      = "sources"
	wmProviderArg     = "provider"
	gitAdd            = "add"
	// repositoriesKey is the "repositories" key of a workspace source.
	repositoriesKey = "repositories"
	// gitOrigin is a clone's remote.
	gitOrigin = "origin"
	// neverRead says a wait never read what it waited for.
	neverRead = "never read"
	// shExitOnError runs a probe's script to its first failure.
	shExitOnError = "-ec"
)

// The workspace-manager's answers, as far as the part reads them.
type syncLastSync struct {
	Time    time.Time `json:"time"`
	Result  string    `json:"result"`
	Trigger string    `json:"trigger"`
	Job     string    `json:"job"`
	Message string    `json:"message"`
}

type syncWorkspace struct {
	Name         string        `json:"name"`
	Organization string        `json:"organization"`
	Deleting     bool          `json:"deleting"`
	LastSync     *syncLastSync `json:"lastSync"`
	ActiveSync   *struct {
		Job string `json:"job"`
	} `json:"activeSync"`
	SyncRequest *struct {
		RequestedAt time.Time `json:"requestedAt"`
	} `json:"syncRequest"`
	Repositories int `json:"repositories"`
	Volume       *struct {
		ClaimName string `json:"claimName"`
		Handle    string `json:"handle"`
		Pending   string `json:"pending"`
	} `json:"volume"`
	Sessions int `json:"sessions"`
}

type syncMirror struct {
	Repository string     `json:"repository"`
	Head       string     `json:"head"`
	PushedAt   time.Time  `json:"pushedAt"`
	SizeBytes  int64      `json:"sizeBytes"`
	DroppedAt  *time.Time `json:"droppedAt"`
}

type syncSessionDirectory struct {
	Session string `json:"session"`
	Unknown bool   `json:"unknown"`
}

type syncDetail struct {
	Workspace          syncWorkspace          `json:"workspace"`
	Mirrors            []syncMirror           `json:"mirrors"`
	SessionDirectories []syncSessionDirectory `json:"sessionDirectories"`
}

// mirror is the manifest's entry of repo, nil without one.
func (d *syncDetail) mirror(repo string) *syncMirror {
	for i := range d.Mirrors {
		if d.Mirrors[i].Repository == repo {
			return &d.Mirrors[i]
		}
	}
	return nil
}

// live are the manifest's repositories that have not left the selection,
// sorted.
func (d *syncDetail) live() []string {
	var out []string
	for _, m := range d.Mirrors {
		if m.DroppedAt == nil {
			out = append(out, m.Repository)
		}
	}
	slices.Sort(out)
	return out
}

// syncMirrorLines word a manifest as a person reads it: each repository,
// its head, last push and mirror size, and when it left the selection.
func syncMirrorLines(mirrors []syncMirror) []string {
	sorted := slices.Clone(mirrors)
	slices.SortFunc(sorted, func(a, b syncMirror) int { return strings.Compare(a.Repository, b.Repository) })
	out := make([]string, 0, len(sorted))
	for _, m := range sorted {
		line := fmt.Sprintf("%s  head %s  pushed %s  %d KiB", m.Repository, shortSHA(m.Head), m.PushedAt.UTC().Format(time.RFC3339), (m.SizeBytes+1023)/1024)
		if m.DroppedAt != nil {
			line += "  dropped " + m.DroppedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, line)
	}
	return out
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// parseMirrorProbe reads the volume probe's lines, `<owner>/<name> k=v ...`,
// into each repository's values.
func parseMirrorProbe(out string) map[string]map[string]string {
	probe := map[string]map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.Contains(fields[0], "/") || !strings.Contains(fields[1], "=") {
			continue
		}
		values := map[string]string{}
		for _, f := range fields[1:] {
			if k, v, ok := strings.Cut(f, "="); ok {
				values[k] = v
			}
		}
		probe[fields[0]] = values
	}
	return probe
}

// syncMirrorsScript reports each repository's mirror on the volume: bare,
// its HEAD, its branches and tags, and the configuration that keeps its
// objects to the sync's own garbage collection.
func syncMirrorsScript(repos []string) string {
	var b strings.Builder
	b.WriteString(workspacesGitSetup)
	for _, r := range repos {
		fmt.Fprintf(&b, `m=/workspace/%s/%s.git
test "$(git -C $m rev-parse --is-bare-repository)" = true
echo "%s bare=true head=$(git -C $m rev-parse HEAD) branches=$(git -C $m for-each-ref refs/heads | wc -l) tags=$(git -C $m for-each-ref refs/tags | wc -l) gc.auto=$(git -C $m config gc.auto) gc.pruneExpire=$(git -C $m config gc.pruneExpire)"
`, workspacesTestMirrors, r, r)
	}
	return b.String()
}

// probeValue is key's value in a probe's `key=value` output, "" without
// one.
func probeValue(out, key string) string {
	for _, f := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

// syncVolume is a workspace's claim as the part finds it: its namespace
// (the workspace-manager's), name and PersistentVolume.
type syncVolume struct {
	namespace, claim, pv string
}

// syncProof is a part's workspaces through the workspace-manager: the
// member who defines them, their volumes, and the names it removes on every
// path.
type syncProof struct {
	cfg         *config.Config
	timeout     time.Duration
	member      *musterSession
	memberEmail string
	volumes     map[string]syncVolume
	restored    bool
	// names are the workspaces the part creates, removed on every path.
	names []string
}

// newSyncProof is a part's state on the workspaces it creates.
func newSyncProof(cfg *config.Config, timeout time.Duration, names ...string) *syncProof {
	return &syncProof{cfg: cfg, timeout: timeout, volumes: map[string]syncVolume{}, names: names}
}

// removeLeftovers deletes the workspaces of an interrupted run, their
// session directory first so the volume can go.
func (sp *syncProof) removeLeftovers() error {
	for _, name := range sp.names {
		gone, err := sp.gone(name)
		if err != nil {
			return err
		}
		if gone {
			continue
		}
		note("removing the leftover workspace %s of an interrupted run", name)
		if err := sp.delete(name); err != nil {
			return err
		}
	}
	return nil
}

func (sp *syncProof) printManifest(d *syncDetail) {
	note("manifest of %s:\n%s", d.Workspace.Name, indent(strings.Join(syncMirrorLines(d.Mirrors), "\n"), "  "))
}

// get is get_workspace's answer.
func (sp *syncProof) get(name string) (*syncDetail, error) {
	var d syncDetail
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name}, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// gone says whether get_workspace answers not found.
func (sp *syncProof) gone(name string) (bool, error) {
	env, err := sp.member.callToolEnvelope(workspaceManagerToolPrefix+"get_workspace", map[string]any{wmWorkspaceArg: name})
	if err != nil {
		return false, err
	}
	return env.IsError && strings.Contains(env.Content[0].Text, "not found"), nil
}

// waitCycle waits for a cycle that ends after prev (nil: the first) with no
// request pending and no sync job running; a failed cycle is the error.
func (sp *syncProof) waitCycle(name string, prev *syncLastSync) (*syncDetail, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	var last *syncDetail
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, sp.timeout, true, func(context.Context) (bool, error) {
		d, err := sp.get(name)
		if err != nil {
			lastErr = err
			return false, nil
		}
		last = d
		w := d.Workspace
		return w.ActiveSync == nil && w.SyncRequest == nil && w.LastSync != nil && (prev == nil || w.LastSync.Time.After(prev.Time)), nil
	})
	if err != nil {
		state := neverRead
		if last != nil {
			state = fmt.Sprintf("last sync %+v, active %v, request %v", last.Workspace.LastSync, last.Workspace.ActiveSync != nil, last.Workspace.SyncRequest != nil)
		}
		return nil, fmt.Errorf("no cycle of %s ended within %s (%s; last error %v): the workspace-manager's log, `kubectl -n kagent get jobs`", name, sp.timeout, state, lastErr)
	}
	if r := last.Workspace.LastSync; r.Result == syncResultFailed {
		return nil, fmt.Errorf("the cycle of %s failed (trigger %s, job %s): %s", name, r.Trigger, r.Job, r.Message)
	}
	return last, nil
}

// volume finds the workspace's claim by its label.
func (sp *syncProof) volume(name string) (syncVolume, error) {
	if v, ok := sp.volumes[name]; ok {
		return v, nil
	}
	k, err := labKube()
	if err != nil {
		return syncVolume{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	claims, err := k.clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{LabelSelector: syncWorkspaceLabel + "=" + name})
	if err != nil {
		return syncVolume{}, fmt.Errorf("listing the claim of workspace %s: %w", name, err)
	}
	if len(claims.Items) != 1 || claims.Items[0].Spec.VolumeName == "" {
		return syncVolume{}, fmt.Errorf("workspace %s has %d claims labelled %s=%s, want one bound", name, len(claims.Items), syncWorkspaceLabel, name)
	}
	c := claims.Items[0]
	v := syncVolume{namespace: c.Namespace, claim: c.Name, pv: c.Spec.VolumeName}
	sp.volumes[name] = v
	return v, nil
}

// workspaceVolumeUser is the user and group a workspace volume probe runs
// as, with the group as its fsGroup: the workspace-manager's sync Job's, the
// identity that lays the volume out and removes the session directories on
// it, so a probe reads, writes and removes what the Job can.
const workspaceVolumeUser int64 = 1000

// volumeOwner is the user and group a file on a workspace volume belongs to,
// and a probe that must enter it runs as.
type volumeOwner struct{ uid, gid int64 }

// workspaceVolumeOwner is the sync Job's user and group.
var workspaceVolumeOwner = volumeOwner{workspaceVolumeUser, workspaceVolumeUser}

// runnable is the owner a restricted probe can run as: root, whom
// runAsNonRoot refuses, and the zero owner (unread) are the sync Job's user.
func (o volumeOwner) runnable() volumeOwner {
	if o.uid == 0 {
		return workspaceVolumeOwner
	}
	return o
}

// workspaceVolumePod is a probe pod on the git image with the workspace's
// claim mounted whole at /workspace, as the sync Job's user and admitted
// under the PodSecurity `restricted` profile an installation enforces.
func workspaceVolumePod(name, claim, script string) *corev1.Pod {
	pod := probePod(name, workspacesTestGitImage, []string{"sh", shExitOnError, script})
	pod.Spec.Volumes = []corev1.Volume{{Name: workspacesTestVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath}}
	// The user has no home in the image: git's global config goes to /tmp,
	// as the sync Job's does.
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "HOME", Value: "/tmp"}}
	user := workspaceVolumeUser
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: &user, RunAsGroup: &user, FSGroup: &user}
	restrictPodSecurity(pod)
	return pod
}

// onVolume runs a probe on the git image with the workspace's whole volume
// at /workspace and returns its output.
func (sp *syncProof) onVolume(name, probe, script string) (string, error) {
	v, err := sp.volume(name)
	if err != nil {
		return "", err
	}
	pod := workspaceVolumePod("agentlab-sync-"+probe, v.claim, script)
	out, err := runPod(context.Background(), v.namespace, pod, sp.timeout)
	if err != nil {
		return "", fmt.Errorf("the volume probe %s on workspace %s: %w\n%s", probe, name, err, out)
	}
	return strings.TrimSpace(out), nil
}

// delete removes the workspace's session directory, deletes it through
// delete_workspace and waits for it, its claim and its volume to go.
func (sp *syncProof) delete(name string) error {
	v, err := sp.volume(name)
	if err == nil {
		out, err := sp.onVolume(name, "clear", accessClearSessionsScript)
		if err != nil {
			return err
		}
		note("%s", indent(out, "  "))
	}
	var res struct {
		Sessions []string `json:"sessions"`
	}
	if err := sp.member.callServerStructured(workspaceManagerToolPrefix+"delete_workspace", map[string]any{wmWorkspaceArg: name}, &res); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sp.timeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return err
	}
	var state string
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, sp.timeout, true, func(ctx context.Context) (bool, error) {
		gone, err := sp.gone(name)
		if err != nil || !gone {
			state = "the workspace is still listed"
			return false, nil
		}
		if v.claim == "" {
			return true, nil
		}
		if _, err := k.clientset.CoreV1().PersistentVolumeClaims(v.namespace).Get(ctx, v.claim, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			state = "its claim " + v.claim + " is still there"
			return false, nil
		}
		if _, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, v.pv, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			state = "its PersistentVolume " + v.pv + " is still there"
			return false, nil
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("workspace %s is not gone %s after delete_workspace: %s", name, sp.timeout, state)
	}
	delete(sp.volumes, name)
	if v.pv == "" {
		note("deleted %s (no volume was claimed)", name)
		return nil
	}
	node := WorkspacesNode(sp.cfg)
	dirs, err := workspaceVolumeDirs(node, workspacesBackendFor(sp.cfg))
	if err != nil {
		return err
	}
	if slices.Contains(dirs, v.pv) {
		return fmt.Errorf("the export on %s still holds %s, the volume of workspace %s", node, v.pv, name)
	}
	note("deleted %s (%d Sessions with a directory at the delete): the workspace, its claim %s/%s and its volume %s gone, nothing of it under %s", name, len(res.Sessions), v.namespace, v.claim, v.pv, workspacesExportDir)
	return nil
}

// restore removes the part's workspaces, best effort past the first
// failure, and returns the first failure.
func (sp *syncProof) restore() error {
	sp.restored = true
	var errs []error
	if sp.member != nil {
		for _, name := range sp.names {
			if gone, err := sp.gone(name); err == nil && !gone {
				errs = append(errs, sp.delete(name))
			}
		}
	}
	return errors.Join(errs...)
}
