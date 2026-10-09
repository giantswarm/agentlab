package lab

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// TestWorkspacesActorTemplateFrom: the proof's template runs on the
// Harness's worker pool and snapshot storage in the chart's gVisor sandbox,
// and declares the two existing volumes, the mirrors mounted read-only.
func TestWorkspacesActorTemplateFrom(t *testing.T) {
	got := workspacesActorTemplateFrom(workspacesActorRuntime{harness: "kagent/kagent", workerPool: "kagent-default", snapshotLocation: "s3://ate-snapshots/kagent"})
	if got.GetMetadata().GetName() != workspacesActorTemplate || got.GetMetadata().GetAtespace() != kagentNamespace {
		t.Errorf("metadata = %v", got.GetMetadata())
	}
	if got.GetWorkerSelector().GetMatchLabels()[workspacesKagentWorkerPoolLabel] != "kagent-default" {
		t.Errorf("worker selector = %v", got.GetWorkerSelector())
	}
	if got.GetSandboxConfig().GetSandboxClass() != ateapi.SandboxClass_SANDBOX_CLASS_GVISOR || got.GetSandboxConfig().GetConfigName() != workspacesSandboxConfig {
		t.Errorf("sandbox = %v", got.GetSandboxConfig())
	}
	if got.GetSnapshotConfig().GetStorageLocation() != "s3://ate-snapshots/kagent" {
		t.Errorf("snapshot config = %v", got.GetSnapshotConfig())
	}
	if len(got.GetContainers()) != 1 || got.GetContainers()[0].GetImage() != workspacesActorImage {
		t.Fatalf("containers = %v, want one on %s", got.GetContainers(), workspacesActorImage)
	}
	mounts := map[string]*ateapi.VolumeMount{}
	for _, m := range got.GetContainers()[0].GetVolumeMounts() {
		mounts[m.GetName()] = m
	}
	if m := mounts[workspacesActorSessionVolume]; m == nil || m.GetMountPath() != workspacesTestMountPath || m.GetReadOnly() {
		t.Errorf("session mount = %v, want read-write at %s", m, workspacesTestMountPath)
	}
	if m := mounts[workspacesActorMirrorsVolume]; m == nil || m.GetMountPath() != workspacesActorMirrorsPath || !m.GetReadOnly() {
		t.Errorf("mirrors mount = %v, want read-only at %s", m, workspacesActorMirrorsPath)
	}
	if len(got.GetVolumes()) != 2 {
		t.Fatalf("volumes = %v, want the two existing volumes", got.GetVolumes())
	}
	for _, v := range got.GetVolumes() {
		if v.GetExistingVolume() == nil {
			t.Errorf("volume %s is not an existing volume", v.GetName())
		}
	}
}

// TestWorkspacesActorVolumes: both volumes from the one handle, the session
// read-write-many at its directory, the mirrors read-only-many.
func TestWorkspacesActorVolumes(t *testing.T) {
	evs := workspacesActorVolumes("server#share#pvc-1##", "b")
	want := map[string]struct {
		mode ateapi.VolumeAccessMode
		sub  string
	}{
		workspacesActorSessionVolume: {ateapi.VolumeAccessMode_VOLUME_ACCESS_MODE_READ_WRITE_MANY, "sessions/b"},
		workspacesActorMirrorsVolume: {ateapi.VolumeAccessMode_VOLUME_ACCESS_MODE_READ_ONLY_MANY, "mirrors"},
	}
	if len(evs) != len(want) {
		t.Fatalf("existing volumes = %v", evs)
	}
	for _, ev := range evs {
		w, ok := want[ev.GetName()]
		if !ok || ev.GetAccessMode() != w.mode || ev.GetSubPath() != w.sub || ev.GetDriver() != workspacesCSIDriver || ev.GetVolumeHandle() != "server#share#pvc-1##" {
			t.Errorf("existing volume %v", ev)
		}
	}
}

// TestExistingVolumesUnsupported: only an InvalidArgument refusing the
// template volumes' unknown field is the skip; any other refusal fails the proof.
func TestExistingVolumesUnsupported(t *testing.T) {
	for _, c := range []struct {
		err  error
		skip bool
	}{
		// Substrate 1.6.3's words.
		{status.Error(codes.InvalidArgument, "[actor_template.containers[0].volume_mounts[1]: Invalid value: unknown field with protobuf tag 10002, actor_template.volumes[0]: Invalid value: unknown field with protobuf tag 10001, actor_template.volumes[1]: Invalid value: unknown field with protobuf tag 10001]"), true},
		{status.Error(codes.InvalidArgument, "actor_template.snapshot_config.on_resume: Required value"), false},
		{status.Error(codes.PermissionDenied, "actor_template.volumes[0]: unknown field with protobuf tag 10001"), false},
		{errors.New("actor_template.volumes[0]: unknown field with protobuf tag 10001"), false},
	} {
		if _, got := existingVolumesUnsupported(c.err); got != c.skip {
			t.Errorf("existingVolumesUnsupported(%v) = %t, want %t", c.err, got, c.skip)
		}
	}
}

func TestParseWorkspacesBeat(t *testing.T) {
	boot, n, err := parseWorkspacesBeat("boot 0b9f-11 beat 42: actor a, mirrors ref: refs/heads/main\n")
	if err != nil || boot != "0b9f-11" || n != 42 {
		t.Errorf("parseWorkspacesBeat = %q, %d, %v", boot, n, err)
	}
	if _, _, err := parseWorkspacesBeat("cat: can't open"); err == nil {
		t.Error("a line without a beat parsed")
	}
}

// TestWorkspacesActorScriptsParse: the scripts the actors and the reader
// pods run are valid sh.
func TestWorkspacesActorScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for name, script := range map[string]string{
		"actor":        workspacesActorScript,
		"reports":      workspacesActorReportsScript(time.Minute),
		"after-actors": workspacesAfterActorsScript,
		"beat-after":   workspacesBeatAfterScript("sessions/a/actor-beat", "0b9f-11", 42, time.Minute),
	} {
		if out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput(); err != nil { //nolint:gosec // the proof's own scripts, parsed only
			t.Errorf("%s script: %v\n%s", name, err, out)
		}
	}
}
