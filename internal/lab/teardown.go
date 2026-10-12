package lab

import (
	"context"
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

// The workspace storage's NFS server runs inside the lab cluster (its
// Deployment workspacesNFSServer), and every node's kernel NFS client mounts
// it: the PersistentVolumes of the class, the driver's publishes under
// Substrate's volumes directory. A server that stops while a node still
// holds such a mount leaves that client waiting uninterruptibly: `docker
// exec` into the node fails with setns errors, and the node can be neither
// stopped nor removed until the mount is lazily unmounted on the host.
//
// So every path that removes the server — `agentlab down`, `agentlab
// platform-down`, the switch going off on `agentlab platform`, and with them
// a recreate (down, then up) — releases the volumes first, in one order:
//
//  1. delete the workspace PersistentVolumeClaims (Terminating from here, so
//     a pod a controller recreates cannot mount them again),
//  2. delete the pods that mount a workspace volume and wait for them to be
//     gone, so nothing holds a file open on the export,
//  3. wait for the claims to go and delete the PersistentVolumes, while the
//     driver's controller and the server still run to remove their
//     directories,
//  4. unmount every NFS mount and the driver's publishes inside each node,
//     while the server still answers the unmount,
//
// and only then the server goes: with its namespace (workspacesTeardown), or
// with the cluster (Down).

// workspacesReleaseTimeout bounds each wait of the release: the pods gone,
// the claims gone.
const workspacesReleaseTimeout = 2 * time.Minute

// The release's steps as they are announced, in their order; teardownTrace
// sees each name before its step runs.
const (
	stepDeleteClaims  = "delete the workspace PersistentVolumeClaims"
	stepDeletePods    = "delete the pods that mount a workspace volume"
	stepDeleteVolumes = "delete the workspace PersistentVolumes"
	stepUnmountNodes  = "unmount the NFS mounts inside the nodes"
	stepServerDown    = "remove the NFS server with the workspace storage"
	stepRemoveExport  = "remove the export from the nodes"
)

// teardownTrace is called with each step's name before it runs; a test seam
// that proves the order against the lab's state.
var teardownTrace = func(string) {}

// teardownStep is one step of the order above.
type teardownStep struct {
	name string
	run  func(ctx context.Context) error
}

// workspacesReleaseSteps are steps 1 to 4: what holds the server, released
// while it still runs.
func workspacesReleaseSteps(nodes []string) []teardownStep {
	return []teardownStep{
		{stepDeleteClaims, deleteWorkspaceClaims},
		{stepDeletePods, deleteWorkspacePods},
		{stepDeleteVolumes, deleteWorkspaceVolumes},
		{stepUnmountNodes, func(context.Context) error {
			for _, node := range nodes {
				unmountWorkspacesNode(node)
			}
			return nil
		}},
	}
}

// workspacesTeardownSteps are the release, then the server with the rest of
// the workspace storage, then the export's bytes on the nodes.
func workspacesTeardownSteps(nodes []string) []teardownStep {
	return append(workspacesReleaseSteps(nodes),
		teardownStep{stepServerDown, workspacesDown},
		teardownStep{stepRemoveExport, func(context.Context) error {
			for _, node := range nodes {
				removeWorkspacesExport(node)
			}
			return nil
		}},
	)
}

// runTeardown runs steps in order and stops at the first that fails.
func runTeardown(ctx context.Context, steps []teardownStep) error {
	for _, s := range steps {
		teardownTrace(s.name)
		if err := s.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// workspacesTeardown removes the workspace storage in the one order: the
// volumes released, then the NFS server. Used by `agentlab platform-down`
// and by `agentlab platform` with the switch off; quiet on a lab that never
// had the storage.
func workspacesTeardown(ctx context.Context, cfg *config.Config) error {
	return runTeardown(ctx, workspacesTeardownSteps(labNodes(cfg)))
}

// releaseWorkspaceVolumes is the release alone, for Down: the cluster's
// deletion takes the server down with it.
func releaseWorkspaceVolumes(ctx context.Context, cfg *config.Config) error {
	return runTeardown(ctx, workspacesReleaseSteps(labNodes(cfg)))
}

// labNodes is the lab's node containers: the control plane and the
// Substrate nodes.
func labNodes(cfg *config.Config) []string {
	return append([]string{cfg.ControlPlaneNode()}, cfg.SubstrateNodeNames()...)
}

// isWorkspaceVolume reports whether a PersistentVolume is served by the
// lab's NFS server: of the class, or provisioned by the driver.
func isWorkspaceVolume(pv *corev1.PersistentVolume) bool {
	if pv.Spec.StorageClassName == workspacesStorageClass {
		return true
	}
	return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == workspacesCSIDriver
}

// workspaceVolumeClaims is the "<namespace>/<name>" of every claim a
// workspace volume is bound to; a volume names its claim until it is
// deleted, after the claim itself is gone.
func workspaceVolumeClaims(ctx context.Context, k *kubeClients) (map[string]bool, error) {
	pvs, err := k.clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing PersistentVolumes: %w", err)
	}
	refs := map[string]bool{}
	for i := range pvs.Items {
		if ref := pvs.Items[i].Spec.ClaimRef; ref != nil && isWorkspaceVolume(&pvs.Items[i]) {
			refs[ref.Namespace+"/"+ref.Name] = true
		}
	}
	return refs, nil
}

// workspaceClaims lists the claims of the class across all namespaces, with
// those bound to a workspace volume of another class.
func workspaceClaims(ctx context.Context) ([]corev1.PersistentVolumeClaim, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	bound, err := workspaceVolumeClaims(ctx, k)
	if err != nil {
		return nil, err
	}
	pvcs, err := k.clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing PersistentVolumeClaims: %w", err)
	}
	var out []corev1.PersistentVolumeClaim
	for _, pvc := range pvcs.Items {
		class := pvc.Spec.StorageClassName
		if (class != nil && *class == workspacesStorageClass) || bound[pvc.Namespace+"/"+pvc.Name] {
			out = append(out, pvc)
		}
	}
	return out, nil
}

// workspaceClaimKeys is every "<namespace>/<name>" a workspace volume is
// bound to or a workspace claim carries.
func workspaceClaimKeys(ctx context.Context) (map[string]bool, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	keys, err := workspaceVolumeClaims(ctx, k)
	if err != nil {
		return nil, err
	}
	pvcs, err := workspaceClaims(ctx)
	if err != nil {
		return nil, err
	}
	for _, pvc := range pvcs {
		keys[pvc.Namespace+"/"+pvc.Name] = true
	}
	return keys, nil
}

// deleteWorkspaceClaims is step 1: every workspace claim deleted, unwaited —
// the protection finalizer keeps a mounted one Terminating until its pods
// are gone (step 2), and a Terminating claim admits no new pod.
func deleteWorkspaceClaims(ctx context.Context) error {
	pvcs, err := workspaceClaims(ctx)
	if err != nil {
		return err
	}
	k, err := labKube()
	if err != nil {
		return err
	}
	for _, pvc := range pvcs {
		err := k.clientset.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting PersistentVolumeClaim %s/%s: %w", pvc.Namespace, pvc.Name, err)
		}
		note("PersistentVolumeClaim %s/%s deleted", pvc.Namespace, pvc.Name)
	}
	return nil
}

// mountsWorkspace reports whether a pod mounts a workspace volume: one of
// the claims, an inline volume of the driver, or a host path under
// Substrate's volumes directory, where the driver publishes.
func mountsWorkspace(pod *corev1.Pod, claims map[string]bool) bool {
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil && claims[pod.Namespace+"/"+v.PersistentVolumeClaim.ClaimName]:
			return true
		case v.CSI != nil && v.CSI.Driver == workspacesCSIDriver:
			return true
		case v.HostPath != nil && (v.HostPath.Path == substrateVolumesDir || strings.HasPrefix(v.HostPath.Path, substrateVolumesDir+"/")):
			return true
		}
	}
	return false
}

// deleteWorkspacePods is step 2: the pods that mount a workspace volume,
// deleted and waited for. A claim step 1 deleted still counts, Terminating
// or already gone: the volumes name their claims until step 3.
func deleteWorkspacePods(ctx context.Context) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	claims, err := workspaceClaimKeys(ctx)
	if err != nil {
		return err
	}
	pods, err := k.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}
	var mounting []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !mountsWorkspace(pod, claims) {
			continue
		}
		// The driver's own node plugin publishes under Substrate's
		// directory; it goes with the server, not before it.
		if pod.Namespace == workspacesNamespace {
			continue
		}
		err := k.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		mounting = append(mounting, pod.Namespace+"/"+pod.Name)
	}
	if len(mounting) == 0 {
		return nil
	}
	note("deleting %d pod(s) that mount a workspace volume: %s", len(mounting), strings.Join(mounting, ", "))
	return waitGone(ctx, workspacesReleaseTimeout, "pod", mounting, func(ctx context.Context, ns, name string) error {
		_, err := k.clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		return err
	})
}

// deleteWorkspaceVolumes is step 3: the claims waited for, then every
// workspace PersistentVolume deleted — a Delete-policy volume is already
// going with its claim; a Retain one is removed here.
func deleteWorkspaceVolumes(ctx context.Context) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	pvcs, err := workspaceClaims(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(pvcs))
	for _, pvc := range pvcs {
		names = append(names, pvc.Namespace+"/"+pvc.Name)
	}
	if err := waitGone(ctx, workspacesReleaseTimeout, "PersistentVolumeClaim", names, func(ctx context.Context, ns, name string) error {
		_, err := k.clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		return err
	}); err != nil {
		return err
	}
	pvs, err := k.clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing PersistentVolumes: %w", err)
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if !isWorkspaceVolume(pv) {
			continue
		}
		err := k.clientset.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting PersistentVolume %s: %w", pv.Name, err)
		}
		note("PersistentVolume %s deleted", pv.Name)
	}
	return nil
}

// waitGone polls until every "<namespace>/<name>" of kind reads NotFound,
// failing with the ones left once timeout passes.
func waitGone(ctx context.Context, timeout time.Duration, kind string, names []string, get func(ctx context.Context, ns, name string) error) error {
	left := slices.Clone(names)
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		left = slices.DeleteFunc(left, func(n string) bool {
			ns, name, _ := strings.Cut(n, "/")
			return apierrors.IsNotFound(get(ctx, ns, name))
		})
		return len(left) == 0, nil
	})
	if wait.Interrupted(err) {
		return fmt.Errorf("%s %s still there %s after the deletion was requested; the NFS server stays up so no node is left holding its mounts",
			kind, strings.Join(left, ", "), timeout)
	}
	return err
}

// unmountWorkspacesNodeScript unmounts, deepest first, every NFS mount of a
// node and the driver's publishes under Substrate's directory: forced, and
// lazily inside the node's own mount namespace when a forced unmount is
// refused, so the node's client lets go of the server before it stops.
func unmountWorkspacesNodeScript() string {
	return fmt.Sprintf(`for m in $(awk '$3 ~ /^nfs/ || $2 ~ "^%s/" {print $2}' /proc/mounts | sort -r); do umount -f "$m" 2>/dev/null || umount -l "$m" 2>/dev/null; done; true`,
		substrateVolumesDir)
}

// unmountWorkspacesNode is step 4 on one node, best effort through `docker
// exec` and bounded by nodeStopTimeout: a node that is not running holds no
// mount, and a wedged one is Down's to report (brokenNodeError).
func unmountWorkspacesNode(node string) {
	_ = withinBound(node, nodeStopTimeout, func() error {
		_, err := outputQuiet(dockerBin, "exec", node, "sh", "-c", unmountWorkspacesNodeScript())
		return err
	})
}

// removeWorkspacesExport removes the export's bytes from a node, once the
// server that served them is gone; best effort like the unmount.
func removeWorkspacesExport(node string) {
	_ = withinBound(node, nodeStopTimeout, func() error {
		_, err := outputQuiet(dockerBin, "exec", node, "rm", "-rf", workspacesExportDir)
		return err
	})
}
