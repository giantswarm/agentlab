package lab

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// The actor-level half of the workspaces proof: Substrate mounts a directory
// of an existing read-write-many volume per actor (ActorTemplate volumes
// marked existing_volume, Actor.existing_volumes on CreateActor), the way
// the workspace-manager hands a Session its part of the workspace. The
// template declares the volume twice, once per mount — the session read-write
// at the working directory, the mirrors read-only beside it — and each actor
// supplies both from the claim's one PersistentVolume, at its own session
// directory and at the mirrors. An actor talks back through the volume: a
// report of what it sees, then a heartbeat whose boot id and count tell a
// resume from a reboot.

const (
	// workspacesActorTemplate is the proof's ActorTemplate in the atespace.
	workspacesActorTemplate = "agentlab-workspaces-test"
	// workspacesActorPrefix names the proof's actors: <prefix><session>.
	workspacesActorPrefix = "agentlab-workspaces-"
	// The template's existing volumes and where the actor sees them.
	workspacesActorSessionVolume = "session"
	workspacesActorMirrorsVolume = "mirrors"
	workspacesActorMirrorsPath   = "/mirrors"
	// workspacesActorRefused names the actor the bad references are tried on.
	workspacesActorRefused = workspacesActorPrefix + "refused"
	// workspacesActorReport is the actor's report in its session directory,
	// written once at boot; workspacesActorBeat its heartbeat after.
	workspacesActorReport = "actor-report"
	workspacesActorBeat   = "actor-beat"
)

// workspacesActorSessions are the sessions an actor runs for: the ones the
// pod steps laid out and wrote markers into.
var workspacesActorSessions = []string{"a", "b"}

// proveWorkspacesActorMount is the actor-level step: ate-api-server reached
// as the ate-client ServiceAccount; an ActorTemplate with the existing
// volumes, its worker pool, sandbox and snapshot storage taken from a kagent
// template of the atespace — refused by a Substrate without the field, which
// is the skip; references with an unknown driver or a missing handle refused
// at create; two actors on the claim's volume at their own session
// directories, each seeing only its own session read-write and the mirrors
// read-only; a pause and resume keeping the process and the content; both
// deleted, the volume and its files left. False is the skip.
func proveWorkspacesActorMount(ctx context.Context, timeout time.Duration) (bool, error) {
	version, verr := helmReleaseVersion(substrateNamespace, substrateRelease)
	if verr != nil || version == "" {
		version = unreadable(verr)
	}
	note("Substrate chart %s (release %s/%s)", version, substrateNamespace, substrateRelease)

	api, err := dialAteAPI(ctx)
	if err != nil {
		return false, err
	}
	defer api.Close()
	if _, err := api.GetAtespace(ctx, &ateapi.GetAtespaceRequest{Atespace: &ateapi.ObjectRef{Name: kagentNamespace}}); err != nil {
		return false, fmt.Errorf("reading the atespace %s through ate-api-server: %w", kagentNamespace, err)
	}
	if !clusterObjectExists(ctx, csiDriverConfigResource, workspacesCSIDriver) {
		return false, fmt.Errorf("the driver %s is not registered with Substrate: no CSIDriverConfig %s; `agentlab platform` registers it with the workspace storage", workspacesCSIDriver, workspacesCSIDriver)
	}
	note("ate-api-server reached as the ate-client ServiceAccount, atespace %s read; the driver registered with Substrate (CSIDriverConfig %s)", kagentNamespace, workspacesCSIDriver)

	base, err := workspacesActorBaseTemplate(ctx, api)
	if err != nil {
		return false, err
	}
	removeWorkspacesActors(ctx, api, timeout)
	defer removeWorkspacesActors(context.Background(), api, timeout)

	tmpl := workspacesActorTemplateFrom(base)
	if _, err := api.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: tmpl}); err != nil {
		if reason, ok := existingVolumesUnsupported(err); ok {
			note("skipped: ate-api-server on Substrate chart %s refuses an ActorTemplate with an existing volume: %s", version, reason)
			return false, nil
		}
		return false, fmt.Errorf("CreateActorTemplate %s/%s: %w", kagentNamespace, workspacesActorTemplate, err)
	}
	if err := waitActorTemplateGolden(ctx, api, timeout); err != nil {
		return false, err
	}
	note("ActorTemplate %s/%s with the existing volumes %s (read-write at %s) and %s (read-only at %s), golden snapshot built; worker pool, sandbox and snapshot storage of %s", kagentNamespace, workspacesActorTemplate, workspacesActorSessionVolume, workspacesTestMountPath, workspacesActorMirrorsVolume, workspacesActorMirrorsPath, base.GetMetadata().GetName())

	pv, err := workspacesClaimVolume(ctx)
	if err != nil {
		return false, err
	}
	handle := pv.Spec.CSI.VolumeHandle
	note("the claim's PersistentVolume %s: driver %s, volume handle %s", pv.Name, pv.Spec.CSI.Driver, handle)

	if err := proveWorkspacesBadReferencesRefused(ctx, api, handle); err != nil {
		return false, err
	}

	for _, s := range workspacesActorSessions {
		if err := startWorkspacesActor(ctx, api, s, handle, timeout); err != nil {
			return false, err
		}
	}
	out, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("actor-reports", workspacesActorReportsScript(timeout), []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, ReadOnly: true}}, true), timeout+time.Minute)
	if err != nil {
		return false, fmt.Errorf("the actors' reports: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))

	if err := proveWorkspacesActorPauseResume(ctx, api, "a", timeout); err != nil {
		return false, err
	}

	for _, s := range workspacesActorSessions {
		if err := deleteWorkspacesActor(ctx, api, workspacesActorPrefix+s, timeout); err != nil {
			return false, err
		}
	}
	k, err := labKube()
	if err != nil {
		return false, err
	}
	if _, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{}); err != nil {
		return false, fmt.Errorf("the PersistentVolume %s after the actors' deletion: %w", pv.Name, err)
	}
	out, err = runPod(ctx, workspacesTestNamespace, workspacesTestPod("after-actors", workspacesAfterActorsScript, []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, ReadOnly: true}}, true), timeout)
	if err != nil {
		return false, fmt.Errorf("the volume after the actors' deletion: %w\n%s", err, out)
	}
	note("both actors deleted; the PersistentVolume %s is there, and so are the files:\n%s", pv.Name, indent(strings.TrimSpace(out), "  "))
	return true, nil
}

// existingVolumesUnsupported tells a Substrate without existing volumes from
// any other refusal: such a server does not know Volume.existing_volume, so
// the template's volumes carry no source it knows and it refuses them as
// invalid. The server's words are the reason.
func existingVolumesUnsupported(err error) (string, bool) {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		return "", false
	}
	msg := st.Message()
	for _, v := range []string{workspacesActorSessionVolume, workspacesActorMirrorsVolume} {
		if strings.Contains(msg, "volumes") && strings.Contains(msg, v) {
			return msg, true
		}
	}
	return "", false
}

// workspacesActorBaseTemplate is the kagent template of the atespace whose
// golden snapshot exists: the proof's template runs on its worker pool, in
// its sandbox, with its snapshot storage.
func workspacesActorBaseTemplate(ctx context.Context, api *ateAPI) (*ateapi.ActorTemplate, error) {
	req := &ateapi.ListActorTemplatesRequest{Atespace: kagentNamespace}
	for {
		resp, err := api.ListActorTemplates(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("ListActorTemplates in %s: %w", kagentNamespace, err)
		}
		for _, t := range resp.GetActorTemplates() {
			if t.GetMetadata().GetName() != workspacesActorTemplate && t.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag().GetName() != "" {
				return t, nil
			}
		}
		if resp.GetNextPageToken() == "" {
			return nil, fmt.Errorf("no ActorTemplate with a golden snapshot in the atespace %s to take the worker pool, sandbox and snapshot storage from: the platform's Agents create them, `agentlab platform-test` proves one", kagentNamespace)
		}
		req.PageToken = resp.GetNextPageToken()
	}
}

// workspacesActorTemplateFrom is the proof's template: base's worker
// selector, sandbox, snapshot storage and resources, one container on the
// lab's alpine running workspacesActorScript, and the two existing volumes.
func workspacesActorTemplateFrom(base *ateapi.ActorTemplate) *ateapi.ActorTemplate {
	return &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: workspacesActorTemplate},
		WorkerSelector: base.GetWorkerSelector(),
		SandboxConfig:  base.GetSandboxConfig(),
		SnapshotConfig: base.GetSnapshotConfig(),
		Resources:      base.GetResources(),
		Containers: []*ateapi.Container{{
			Name:    "session",
			Image:   probeImage,
			Command: []string{"sh", "-c", workspacesActorScript},
			VolumeMounts: []*ateapi.VolumeMount{
				{Name: workspacesActorSessionVolume, MountPath: workspacesTestMountPath},
				{Name: workspacesActorMirrorsVolume, MountPath: workspacesActorMirrorsPath, ReadOnly: true},
			},
		}},
		Volumes: []*ateapi.Volume{
			{Name: workspacesActorSessionVolume, ExistingVolume: &ateapi.ExistingVolumeSource{}},
			{Name: workspacesActorMirrorsVolume, ExistingVolume: &ateapi.ExistingVolumeSource{}},
		},
	}
}

// workspacesActorVolumes supply both of the template's existing volumes from
// the claim's one handle: the session's directory read-write, the mirrors
// read-only.
func workspacesActorVolumes(handle, session string) []*ateapi.ExistingVolume {
	return []*ateapi.ExistingVolume{
		{Name: workspacesActorSessionVolume, Driver: workspacesCSIDriver, VolumeHandle: handle, AccessMode: ateapi.VolumeAccessMode_VOLUME_ACCESS_MODE_READ_WRITE_MANY, SubPath: workspacesTestSessions + "/" + session},
		{Name: workspacesActorMirrorsVolume, Driver: workspacesCSIDriver, VolumeHandle: handle, AccessMode: ateapi.VolumeAccessMode_VOLUME_ACCESS_MODE_READ_ONLY_MANY, SubPath: workspacesTestMirrors},
	}
}

// workspacesActorScript is an actor's work: what it sees of its session and
// of the mirrors, a write into the mirrors refused, a file of its own, all in
// a report renamed into place at the end; then a heartbeat every second with
// its boot id, its count and what it reads, which a resume continues and a
// reboot restarts. The session's name is its directory's marker.
const workspacesActorScript = `set -u
r=/workspace/` + workspacesActorReport + `.tmp
: > "$r"
say() { echo "$*" >> "$r"; }
s=$(ls /workspace | sed -n 's/^marker-//p')
[ -n "$s" ] || { say "FAIL: no marker in /workspace: $(ls -A /workspace | tr '\n' ' ')"; s=unknown; }
say "session $s"
for m in /workspace/marker-*; do [ "$m" = /workspace/marker-$s ] || say "FAIL: sees $m of another session"; done
say "sees on /workspace: $(ls /workspace | grep -v '^` + workspacesActorReport + `' | tr '\n' ' ')"
[ "$(cat /workspace/repo/$s.txt 2>/dev/null)" = "session $s" ] && say "its session's clone and commit readable" || say "FAIL: /workspace/repo/$s.txt is not the session's file"
[ -f /mirrors/` + workspacesTestRepo + `/HEAD ] && say "mirrors readable: ` + workspacesTestRepo + ` HEAD $(cat /mirrors/` + workspacesTestRepo + `/HEAD)" || say "FAIL: /mirrors/` + workspacesTestRepo + `/HEAD not readable"
if touch /mirrors/actor-$s 2>/tmp/err; then say "FAIL: the mirrors mount took a write"; else say "mirrors read-only: $(cat /tmp/err)"; fi
echo "actor $s" > /workspace/actor.txt && say "wrote actor.txt into its session directory" || say "FAIL: no write into its session directory"
mv "$r" /workspace/` + workspacesActorReport + `
boot=$(cat /proc/sys/kernel/random/uuid)
n=0
while :; do
  n=$((n+1))
  echo "boot $boot beat $n: $(cat /workspace/actor.txt), mirrors $(cat /mirrors/` + workspacesTestRepo + `/HEAD)" > /workspace/` + workspacesActorBeat + `.tmp
  mv /workspace/` + workspacesActorBeat + `.tmp /workspace/` + workspacesActorBeat + `
  sleep 1
done
`

// workspacesActorReportsScript waits for both actors' reports on the whole
// volume, read-only, prints them and fails on a FAIL line.
func workspacesActorReportsScript(timeout time.Duration) string {
	return fmt.Sprintf(`for s in %[1]s; do
  f=/workspace/%[2]s/$s/%[3]s
  i=0; until [ -f "$f" ]; do i=$((i+1)); [ $i -gt %[4]d ] && { echo "actor $s wrote no report in %[4]ds"; exit 1; }; sleep 1; done
  sed "s/^/actor $s: /" "$f"
done
! grep -h FAIL /workspace/%[2]s/*/%[3]s
`, strings.Join(workspacesActorSessions, " "), workspacesTestSessions, workspacesActorReport, int(timeout.Seconds()))
}

// workspacesAfterActorsScript lists what the actors and the pods left.
const workspacesAfterActorsScript = `for f in sessions/a/actor.txt sessions/b/actor.txt sessions/a/marker-a sessions/b/marker-b mirrors/` + workspacesTestRepo + `/HEAD; do
  [ -f /workspace/$f ] || { echo "missing: $f"; exit 1; }
  echo "kept: $f"
done
[ -z "$(ls /workspace/mirrors | grep actor-)" ] || { echo 'the mirrors took a write'; exit 1; }
`

// proveWorkspacesBadReferencesRefused tries an actor with an unknown driver
// and one with a handle no PersistentVolume holds: ate-api-server refuses
// both at create, before any resume.
func proveWorkspacesBadReferencesRefused(ctx context.Context, api *ateAPI, handle string) error {
	for _, c := range []struct {
		what string
		mod  func(*ateapi.ExistingVolume)
	}{
		{"an unknown driver", func(ev *ateapi.ExistingVolume) { ev.Driver = "unknown.csi.agentlab.local" }},
		{"a handle no PersistentVolume holds", func(ev *ateapi.ExistingVolume) { ev.VolumeHandle = "agentlab-no-such-volume" }},
	} {
		evs := workspacesActorVolumes(handle, "a")
		c.mod(evs[0])
		_, err := api.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: workspacesActor(workspacesActorRefused, evs)})
		if err == nil {
			_, _ = api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: workspacesActorRefused}})
			return fmt.Errorf("CreateActor with %s was accepted: ate-api-server must refuse it at create", c.what)
		}
		if code := status.Code(err); code != codes.FailedPrecondition && code != codes.InvalidArgument {
			return fmt.Errorf("CreateActor with %s: %w (want a refusal of the reference)", c.what, err)
		}
		note("%s refused at create: %s", c.what, status.Convert(err).Message())
	}
	return nil
}

// workspacesActor is the proof's actor of the template with the volumes.
func workspacesActor(name string, evs []*ateapi.ExistingVolume) *ateapi.Actor {
	return &ateapi.Actor{
		Metadata:        &ateapi.ResourceMetadata{Atespace: kagentNamespace, Name: name},
		ActorTemplate:   &ateapi.ObjectRef{Atespace: kagentNamespace, Name: workspacesActorTemplate},
		ExistingVolumes: evs,
	}
}

// startWorkspacesActor creates the session's actor and resumes it until it
// runs.
func startWorkspacesActor(ctx context.Context, api *ateAPI, session, handle string, timeout time.Duration) error {
	name := workspacesActorPrefix + session
	if _, err := api.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: workspacesActor(name, workspacesActorVolumes(handle, session))}); err != nil {
		return fmt.Errorf("CreateActor %s/%s: %w", kagentNamespace, name, err)
	}
	if err := resumeWorkspacesActor(ctx, api, name, timeout); err != nil {
		return err
	}
	note("actor %s running: %s at %s, %s read-only at %s", name, workspacesTestSessions+"/"+session, workspacesTestMountPath, workspacesTestMirrors, workspacesActorMirrorsPath)
	return nil
}

// resumeWorkspacesActor resumes an actor, retrying while the pool has no
// free worker, and waits for it to run.
func resumeWorkspacesActor(ctx context.Context, api *ateAPI, name string, timeout time.Duration) error {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}
	var last error
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, last = api.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
		switch status.Code(last) {
		case codes.OK:
			return true, nil
		case codes.ResourceExhausted, codes.Unavailable, codes.DeadlineExceeded:
			return false, nil
		default:
			return false, last
		}
	}); err != nil {
		return fmt.Errorf("ResumeActor %s/%s: %w (last: %v)", kagentNamespace, name, err, last)
	}
	return waitWorkspacesActorState(ctx, api, name, ateapi.ActorState_ACTOR_STATE_RUNNING, timeout)
}

// waitWorkspacesActorState waits for an actor to reach the state.
func waitWorkspacesActorState(ctx context.Context, api *ateAPI, name string, want ateapi.ActorState, timeout time.Duration) error {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}
	var state ateapi.ActorState
	if err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		actor, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
		if err != nil {
			return false, nil
		}
		state = actor.GetStatus().GetState()
		if state == ateapi.ActorState_ACTOR_STATE_CRASHED {
			return false, fmt.Errorf("the actor crashed")
		}
		return state == want, nil
	}); err != nil {
		return fmt.Errorf("actor %s/%s is %s, not %s after %s: %w", kagentNamespace, name, state, want, timeout, err)
	}
	return nil
}

// proveWorkspacesActorPauseResume pauses the session's actor and resumes
// it: its heartbeat carries on with the same boot id past the count before
// the pause, still reading its own file and the mirrors.
func proveWorkspacesActorPauseResume(ctx context.Context, api *ateAPI, session string, timeout time.Duration) error {
	name := workspacesActorPrefix + session
	beat := workspacesTestSessions + "/" + session + "/" + workspacesActorBeat
	before, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("beat-before", "cat /workspace/"+beat, []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, ReadOnly: true}}, true), timeout)
	if err != nil {
		return fmt.Errorf("reading %s before the pause: %w\n%s", name, err, before)
	}
	boot, count, err := parseWorkspacesBeat(before)
	if err != nil {
		return err
	}
	if _, err := api.PauseActor(ctx, &ateapi.PauseActorRequest{Actor: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}}); err != nil {
		return fmt.Errorf("PauseActor %s: %w", name, err)
	}
	if err := waitWorkspacesActorState(ctx, api, name, ateapi.ActorState_ACTOR_STATE_PAUSED, timeout); err != nil {
		return err
	}
	if err := resumeWorkspacesActor(ctx, api, name, timeout); err != nil {
		return err
	}
	script := workspacesBeatAfterScript(beat, boot, count, timeout)
	after, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("beat-after", script, []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, ReadOnly: true}}, true), timeout+time.Minute)
	if err != nil {
		return fmt.Errorf("actor %s after the resume: %w\n%s", name, err, after)
	}
	note("actor %s paused and resumed: before %q, after %q — the same boot, its file and the mirrors still read", name, strings.TrimSpace(before), strings.TrimSpace(after))
	return nil
}

// workspacesBeatAfterScript waits for the heartbeat at beat to go past
// count with the same boot id, and fails on another boot id: a reboot.
func workspacesBeatAfterScript(beat, boot string, count int, timeout time.Duration) string {
	return fmt.Sprintf(`i=0; while :; do
  l=$(cat /workspace/%[1]s)
  set -- $l
  if [ "$2" = %[2]q ] && [ "${4%%:}" -gt %[3]d ]; then echo "$l"; exit 0; fi
  [ "$2" != %[2]q ] && { echo "rebooted, not resumed: $l"; exit 1; }
  i=$((i+1)); [ $i -gt %[4]d ] && { echo "no beat past %[3]d in %[4]ds: $l"; exit 1; }
  sleep 1
done`, beat, boot, count, int(timeout.Seconds()))
}

// parseWorkspacesBeat reads "boot <id> beat <n>: …".
func parseWorkspacesBeat(line string) (string, int, error) {
	var boot string
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "boot %s beat %d:", &boot, &n); err != nil {
		return "", 0, fmt.Errorf("the actor's heartbeat %q: %w", strings.TrimSpace(line), err)
	}
	return boot, n, nil
}

// deleteWorkspacesActor suspends an actor (a running one is not deletable;
// the suspend unmounts its volumes), deletes it and waits for it to go.
func deleteWorkspacesActor(ctx context.Context, api *ateAPI, name string, timeout time.Duration) error {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}
	if _, err := api.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref}); err != nil {
		return fmt.Errorf("SuspendActor %s: %w", name, err)
	}
	if err := waitWorkspacesActorState(ctx, api, name, ateapi.ActorState_ACTOR_STATE_SUSPENDED, timeout); err != nil {
		return err
	}
	if _, err := api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref}); err != nil {
		return fmt.Errorf("DeleteActor %s: %w", name, err)
	}
	return waitWorkspacesActorGone(ctx, api, name, timeout)
}

// waitWorkspacesActorGone waits for GetActor's NotFound.
func waitWorkspacesActorGone(ctx context.Context, api *ateAPI, name string, timeout time.Duration) error {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}
	return wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
		return status.Code(err) == codes.NotFound, nil
	})
}

// removeWorkspacesActors removes the proof's actors and template, whatever
// state an interrupted run left them in. Best effort.
func removeWorkspacesActors(ctx context.Context, api *ateAPI, timeout time.Duration) {
	for _, name := range []string{workspacesActorPrefix + "a", workspacesActorPrefix + "b", workspacesActorRefused} {
		ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: name}
		if _, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref}); status.Code(err) == codes.NotFound {
			continue
		}
		_, _ = api.SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref})
		_ = waitWorkspacesActorState(ctx, api, name, ateapi.ActorState_ACTOR_STATE_SUSPENDED, timeout)
		_, _ = api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref})
		_ = waitWorkspacesActorGone(ctx, api, name, timeout)
	}
	_, _ = api.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: &ateapi.ObjectRef{Atespace: kagentNamespace, Name: workspacesActorTemplate}})
}

// waitActorTemplateGolden waits for the proof's template's golden snapshot.
func waitActorTemplateGolden(ctx context.Context, api *ateAPI, timeout time.Duration) error {
	ref := &ateapi.ObjectRef{Atespace: kagentNamespace, Name: workspacesActorTemplate}
	var golden *ateapi.GoldenSnapshotStatus
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		t, err := api.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: ref})
		if err != nil {
			return false, nil
		}
		golden = t.GetStatus().GetGoldenSnapshotStatus()
		if msg := golden.GetErrorMessage(); msg != "" {
			return false, errors.New(msg)
		}
		return golden.GetGoldenTag().GetName() != "", nil
	})
	if err != nil {
		return fmt.Errorf("the golden snapshot of ActorTemplate %s/%s: %w", kagentNamespace, workspacesActorTemplate, err)
	}
	return nil
}

// workspacesClaimVolume is the proof claim's PersistentVolume, a volume of
// the driver.
func workspacesClaimVolume(ctx context.Context) (*corev1.PersistentVolume, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	pvc, err := k.clientset.CoreV1().PersistentVolumeClaims(workspacesTestNamespace).Get(ctx, workspacesTestClaim, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the PVC %s/%s: %w", workspacesTestNamespace, workspacesTestClaim, err)
	}
	pv, err := k.clientset.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != workspacesCSIDriver {
		return nil, fmt.Errorf("the PersistentVolume %s is not a volume of %s", pv.Name, workspacesCSIDriver)
	}
	return pv, nil
}
