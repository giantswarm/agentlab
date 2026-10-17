package lab

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// The teardown tests' lab: its cluster, its node, the namespace of the pod
// that mounts the workspace claim, and the claim's volume.
const (
	teardownCluster        = "teardown"
	teardownNode           = teardownCluster + "-control-plane"
	teardownProofNamespace = "workspace-proof"
	teardownVolume         = "pvc-workspace"
	teardownClaim          = "workspace-claim"
)

// workspacesNamespaceObject is the NFS server's namespace as the fake lab
// stores it.
func workspacesNamespaceObject() *corev1.Namespace { return namespaceObject(workspacesNamespace) }

// namespaceObject is a namespace as the fake apiserver serves it.
func namespaceObject(name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// workspaceFixtures seeds the typed fake with what a lab with a workspace
// volume mounted by a running pod carries: the class's claim and its volume,
// the pod mounting the claim, the driver's node plugin (which goes with the
// server, not before it) and a pod that has nothing to do with workspaces.
func workspaceFixtures(t *testing.T, f *fakeLab) {
	t.Helper()
	ctx := context.Background()
	class := workspacesStorageClass
	core := f.cs.CoreV1()
	if _, err := core.PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: teardownVolume},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: class,
			ClaimRef:         &corev1.ObjectReference{Namespace: teardownProofNamespace, Name: teardownClaim},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: workspacesCSIDriver, VolumeHandle: "h"},
			},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := core.PersistentVolumeClaims(teardownProofNamespace).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: teardownProofNamespace, Name: teardownClaim},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class, VolumeName: teardownVolume},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: teardownProofNamespace, Name: "holder"},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "w", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: teardownClaim},
			}}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: workspacesNamespace, Name: workspacesNodePlugin + "-x"},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "ate", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: substrateVolumesDir},
			}}}},
		},
		{ObjectMeta: metav1.ObjectMeta{Namespace: kubeSystemNamespace, Name: "bystander"}},
	}
	for _, p := range pods {
		if _, err := core.Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWorkspacesTeardownOrder drives the one teardown order against a lab
// with a workspace volume mounted by a running pod: the claim and the pod
// mounting it are gone, and the volume deleted, before the nodes unmount;
// the nodes unmount while the NFS server's namespace is still there; the
// server goes after; the export's bytes last. Nothing else is touched.
func TestWorkspacesTeardownOrder(t *testing.T) {
	f := newFakeLab(t, workspacesNamespaceObject())
	workspaceFixtures(t, f)
	calls := installFakeTool(t, t.TempDir(), "docker", "true")
	cfg := &config.Config{ClusterName: teardownCluster}
	ctx := context.Background()
	core := f.cs.CoreV1()

	var order []string
	prev := teardownTrace
	t.Cleanup(func() { teardownTrace = prev })
	teardownTrace = func(step string) {
		order = append(order, step)
		switch step {
		case stepUnmountNodes:
			if _, err := core.Pods(teardownProofNamespace).Get(ctx, "holder", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Errorf("the pod mounting the volume must be gone before the nodes unmount: %v", err)
			}
			if _, err := core.PersistentVolumeClaims(teardownProofNamespace).Get(ctx, teardownClaim, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Errorf("the claim must be gone before the nodes unmount: %v", err)
			}
			if _, err := core.PersistentVolumes().Get(ctx, teardownVolume, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Errorf("the volume must be deleted before the nodes unmount: %v", err)
			}
			if exists, err := objectExists(ctx, gvrNamespaces, "", workspacesNamespace); err != nil || !exists {
				t.Errorf("the NFS server's namespace must still be there while the nodes unmount (exists %v, %v)", exists, err)
			}
			if log := readCalls(t, calls); log != "" {
				t.Errorf("no docker call before the unmount step, got:\n%s", log)
			}
		case stepServerDown:
			if !strings.Contains(readCalls(t, calls), "umount") {
				t.Error("the NFS server must go only after the nodes unmounted")
			}
		}
	}

	if err := workspacesTeardown(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{stepDeleteClaims, stepDeletePods, stepDeleteVolumes, stepUnmountNodes, stepServerDown, stepRemoveExport}
	if !slices.Equal(order, want) {
		t.Errorf("teardown order\n got %q\nwant %q", order, want)
	}
	if exists, _ := objectExists(ctx, gvrNamespaces, "", workspacesNamespace); exists {
		t.Error("the workspace namespace (the NFS server) must be gone after the teardown")
	}
	for _, p := range []struct{ ns, name string }{{workspacesNamespace, workspacesNodePlugin + "-x"}, {kubeSystemNamespace, "bystander"}} {
		if _, err := core.Pods(p.ns).Get(ctx, p.name, metav1.GetOptions{}); err != nil {
			t.Errorf("pod %s/%s does not mount a workspace volume and must be left alone: %v", p.ns, p.name, err)
		}
	}
	log := readCalls(t, calls)
	umount := strings.Index(log, "docker exec "+teardownNode+" sh -c")
	rm := strings.Index(log, "docker exec "+teardownNode+" rm -rf "+workspacesExportDir)
	if umount < 0 || rm < 0 || rm < umount {
		t.Errorf("the node unmounts first and loses the export last, got:\n%s", log)
	}
}

// TestReleaseWorkspaceVolumesLeavesTheServer: Down's release stops short of
// the server, which goes with the cluster.
func TestReleaseWorkspaceVolumesLeavesTheServer(t *testing.T) {
	f := newFakeLab(t, workspacesNamespaceObject())
	workspaceFixtures(t, f)
	installFakeTool(t, t.TempDir(), "docker", "true")
	var order []string
	prev := teardownTrace
	t.Cleanup(func() { teardownTrace = prev })
	teardownTrace = func(step string) { order = append(order, step) }

	ctx := context.Background()
	if err := releaseWorkspaceVolumes(ctx, &config.Config{ClusterName: teardownCluster}); err != nil {
		t.Fatal(err)
	}
	if want := []string{stepDeleteClaims, stepDeletePods, stepDeleteVolumes, stepUnmountNodes}; !slices.Equal(order, want) {
		t.Errorf("release order\n got %q\nwant %q", order, want)
	}
	if exists, _ := objectExists(ctx, gvrNamespaces, "", workspacesNamespace); !exists {
		t.Error("the release must leave the NFS server running")
	}
}

// TestUnmountWorkspacesNodeScript: every NFS mount and the driver's
// publishes, deepest first, forced and then lazily inside the node.
func TestUnmountWorkspacesNodeScript(t *testing.T) {
	s := unmountWorkspacesNodeScript()
	for _, want := range []string{"$3 ~ /^nfs/", `"^` + substrateVolumesDir + `/"`, "sort -r", `umount -f "$m"`, `umount -l "$m"`} {
		if !strings.Contains(s, want) {
			t.Errorf("unmount script lacks %q:\n%s", want, s)
		}
	}
}

// TestMountsWorkspace: a claim of the set, an inline volume of the driver
// and a host path under Substrate's directory mount a workspace; anything
// else does not.
func TestMountsWorkspace(t *testing.T) {
	claims := map[string]bool{"ns/w": true}
	pod := func(v corev1.VolumeSource) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{VolumeSource: v}}}}
	}
	for name, tc := range map[string]struct {
		v    corev1.VolumeSource
		want bool
	}{
		"claim":          {corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "w"}}, true},
		"other claim":    {corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "x"}}, false},
		"inline csi":     {corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: workspacesCSIDriver}}, true},
		"publish dir":    {corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: substrateVolumesDir + "/a"}}, true},
		"similar prefix": {corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: substrateVolumesDir + "x"}}, false},
		"empty dir":      {corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}, false},
	} {
		if got := mountsWorkspace(pod(tc.v), claims); got != tc.want {
			t.Errorf("%s: mountsWorkspace = %v, want %v", name, got, tc.want)
		}
	}
}

// TestWithinBoundNeverExits: a docker that never returns is a broken node
// once the bound passes, in the words the lease holder reads, instead of a
// wait; one that returns in time passes its own result through.
func TestWithinBoundNeverExits(t *testing.T) {
	installFakeTool(t, t.TempDir(), "docker", "exec sleep 5")
	bound := 50 * time.Millisecond
	start := time.Now()
	err := withinBound(teardownNode, bound, func() error {
		_, err := outputQuiet(dockerBin, "rm", "-f", teardownNode)
		return err
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the bound did not hold: %s", elapsed)
	}
	var broken *brokenNodeError
	if !errors.As(err, &broken) {
		t.Fatalf("a call that never returns must be a brokenNodeError, got %v", err)
	}
	if want := "broken: " + teardownNode + " did not stop within 50ms; needs a host-level fix"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("verdict %q does not start with %q", err, want)
	}

	installFakeTool(t, t.TempDir(), "docker", "exit 3")
	err = withinBound(teardownNode, time.Second, func() error {
		_, err := outputQuiet(dockerBin, "rm", "-f", teardownNode)
		return err
	})
	if err == nil || errors.As(err, &broken) {
		t.Errorf("a call that fails in time keeps its own error, got %v", err)
	}
}

// TestDeleteClusterWithinBoundNeverExits: kind's delete against a docker
// that never answers is reported broken within the bound — the constant
// sits beside nodeExitTimeout and is the one Down passes.
func TestDeleteClusterWithinBoundNeverExits(t *testing.T) {
	if nodeStopTimeout <= 0 || nodeStopTimeout > 10*time.Minute {
		t.Errorf("nodeStopTimeout %s must be a bound of minutes", nodeStopTimeout)
	}
	dir := t.TempDir()
	installFakeTool(t, dir, "docker", "exec sleep 5")
	installFakeTool(t, dir, "podman", "exec sleep 5")
	err := withinBound(teardownNode, 50*time.Millisecond, func() error { return kindDeleteCluster(teardownCluster) })
	var broken *brokenNodeError
	if !errors.As(err, &broken) {
		t.Fatalf("a delete that never returns must be a brokenNodeError, got %v", err)
	}
	if broken.node != teardownNode {
		t.Errorf("verdict names %q, want the node", broken.node)
	}
}
