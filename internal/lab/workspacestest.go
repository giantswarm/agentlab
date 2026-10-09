package lab

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// WorkspacesTestOptions tunes the workspaces proof.
type WorkspacesTestOptions struct {
	// StorageOnly proves the storage, with no harness turn against a
	// workspace: the only mode so far, named so the command keeps its shape
	// once the turn is part of it.
	StorageOnly bool
	// ReadyTimeout bounds each wait: the storage's rollouts, a claim bound,
	// a pod finished, a volume gone.
	ReadyTimeout time.Duration
}

// DefaultWorkspacesReadyTimeout is the proof's default wait.
const DefaultWorkspacesReadyTimeout = 5 * time.Minute

const (
	// workspacesTestNamespace holds the proof's claim and pods.
	workspacesTestNamespace = "agentlab-workspaces-test"
	workspacesTestClaim     = "workspace"
	workspacesTestCapacity  = "1Gi"
	// workspacesTestGitImage runs the proof's git: the Debian golang image
	// gsoci mirrors carries git, the lab's alpine does not.
	workspacesTestGitImage = "gsoci.azurecr.io/giantswarm/golang:1.27.2"
	// The volume's layout, as the workspace-manager keeps it: bare mirrors
	// and a directory per Session.
	workspacesTestMirrors  = "mirrors"
	workspacesTestSessions = "sessions"
	workspacesTestRepo     = "repo.git"
	workspacesTestVolume   = "workspace"
	// workspacesTestMountPath is where a proof pod sees its part of the volume.
	workspacesTestMountPath = "/workspace"
)

// WorkspacesTest is the headless workspaces proof: the storage in place -> a
// read-write-many claim of the class bound -> a bare mirror with an
// executable and a symbolic link seeded on it -> two pods on their own
// session sub-paths, each with a shared clone of the mirror, the modes and
// links intact, the mirrors mount refusing writes, nothing of the other
// session visible -> a read-only mount of the whole volume refusing writes ->
// the controller endpoint refused without Substrate's client certificate ->
// two actors through ate-api-server on the volume at their own session
// directories (workspacesactors.go) -> everything removed, the volume's directory gone from the export.
func WorkspacesTest(cfg *config.Config, opts WorkspacesTestOptions) error {
	if !cfg.WorkspacesEnabled() {
		return fmt.Errorf("platform.workspaces is off in %s — `agentlab configure --workspaces` turns it on, then `agentlab platform`", config.File)
	}
	if !opts.StorageOnly {
		return errors.New("the proof covers the storage so far; a harness turn against a workspace is not part of it: `agentlab workspaces-test --storage-only`")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultWorkspacesReadyTimeout
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	ctx := context.Background()
	timeout := opts.ReadyTimeout
	node := WorkspacesNode(cfg)

	step("The workspace storage in place (namespace %s)", workspacesNamespace)
	if err := proveWorkspacesInstalled(ctx, timeout); err != nil {
		return err
	}
	note("NFS server, controller with its mTLS proxy and node plugin Ready; StorageClass %s (%s)", workspacesStorageClass, workspacesCSIDriver)

	step("Removing what an interrupted run left (namespace %s)", workspacesTestNamespace)
	cleanupWorkspacesTest(ctx, node)
	defer cleanupWorkspacesTest(context.Background(), node)
	if err := ensureNamespace(workspacesTestNamespace); err != nil {
		return err
	}
	if res := sideloadImages(cfg, hostPullImages([]string{workspacesTestGitImage})); res.n > 0 {
		note("side-loaded the proof's git image (%s)", res.d)
	}

	step("A read-write-many claim of %s from StorageClass %s", workspacesTestCapacity, workspacesStorageClass)
	k, err := labKube()
	if err != nil {
		return err
	}
	if _, err := k.clientset.CoreV1().PersistentVolumeClaims(workspacesTestNamespace).Create(ctx, workspacesTestPVC(), metav1.CreateOptions{FieldManager: applyFieldManager}); err != nil {
		return fmt.Errorf("creating the PVC %s/%s: %w", workspacesTestNamespace, workspacesTestClaim, err)
	}
	if err := waitPVCBound(ctx, workspacesTestClaim, timeout); err != nil {
		return err
	}
	note("Bound (a directory of the export on %s, mounted NFSv4.1)", node)

	step("A bare mirror seeded on the volume, from a repository with an executable and a symbolic link")
	out, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("seed", workspacesSeedScript, []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath}}, false), timeout)
	if err != nil {
		return fmt.Errorf("the seed pod: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))

	step("Session a: its own sub-path read-write, the mirrors read-only — a shared clone, the modes and the link, a commit, a write into the mirrors refused")
	out, err = runPod(ctx, workspacesTestNamespace, workspacesTestPod("session-a", workspacesSessionScript("a"), workspacesSessionMounts("a"), false), timeout)
	if err != nil {
		return fmt.Errorf("the session-a pod: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))

	step("Session b: its own sub-path, nothing of session a visible, its own shared clone")
	out, err = runPod(ctx, workspacesTestNamespace, workspacesTestPod("session-b", workspacesSessionScript("b"), workspacesSessionMounts("b"), false), timeout)
	if err != nil {
		return fmt.Errorf("the session-b pod: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))

	step("The whole volume mounted read-only: both sessions' directories visible, a write refused")
	out, err = runPod(ctx, workspacesTestNamespace, workspacesTestPod("reader", workspacesReaderScript, []corev1.VolumeMount{{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, ReadOnly: true}}, true), timeout)
	if err != nil {
		return fmt.Errorf("the reader pod: %w\n%s", err, out)
	}
	note("%s", indent(strings.TrimSpace(out), "  "))

	step("The controller endpoint without Substrate's client certificate (the proxy in the %s pod, port %d, server name %s)", workspacesController, workspacesProxyPort, workspacesControllerServerName())
	if err := proveWorkspacesProxyRefuses(ctx); err != nil {
		return err
	}

	step("Two actors through ate-api-server on the volume at their own session directories, the mirrors read-only")
	actors, err := proveWorkspacesActorMount(ctx, timeout)
	if err != nil {
		return err
	}

	step("Removing the proof's namespace: the claim's volume goes with it")
	cleanupWorkspacesTest(ctx, node)
	if err := proveWorkspacesVolumeGone(ctx, node, timeout); err != nil {
		return err
	}
	note("no PersistentVolume of the proof left; nothing of it under %s on %s", workspacesExportDir, node)

	actorLevel := "the actor-level mount skipped (the Substrate line refuses existing volumes)"
	if actors {
		actorLevel = "two actors on the volume at their own session directories through ate-api-server"
	}
	fmt.Printf("\nWorkspaces proof passed: a read-write-many claim, a shared clone per session sub-path with modes and links intact, the sessions isolated, the read-only mounts refusing writes, the controller endpoint refused without Substrate's certificate, %s, everything removed.\n", actorLevel)
	return nil
}

// proveWorkspacesInstalled is the precondition: the class, the CSIDriver,
// the NFS server, the controller and the node plugin, each waited for
// briefly.
func proveWorkspacesInstalled(ctx context.Context, timeout time.Duration) error {
	for _, o := range []struct{ resource, name, what string }{
		{"storageclasses.storage.k8s.io", workspacesStorageClass, "StorageClass"},
		{"csidrivers.storage.k8s.io", workspacesCSIDriver, "CSIDriver"},
	} {
		if !clusterObjectExists(ctx, o.resource, o.name) {
			return fmt.Errorf("no %s %s: `agentlab platform` installs the workspace storage", o.what, o.name)
		}
	}
	for _, name := range []string{workspacesNFSServer, workspacesController} {
		if err := waitDeploymentRolledOut(ctx, workspacesNamespace, name, timeout); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return waitDaemonSetReady(ctx, workspacesNamespace, workspacesNodePlugin, timeout)
}

// cleanupWorkspacesTest removes the proof's namespace, with its pods and
// claim, and waits for the claim's volume to go: the proof's opening and
// its end, whatever happened in between. Best effort.
func cleanupWorkspacesTest(ctx context.Context, node string) {
	_ = deleteNamespace(ctx, workspacesTestNamespace)
	_ = waitWorkspacesVolumesGone(ctx, 2*time.Minute)
	_ = node
}

// workspacesTestPVC is a read-write-many claim of the proof's capacity on
// the class.
func workspacesTestPVC() *corev1.PersistentVolumeClaim {
	class := workspacesStorageClass
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: workspacesTestClaim, Namespace: workspacesTestNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(workspacesTestCapacity)},
			},
		},
	}
}

// workspacesTestPod is a probe pod on the git image with the claim as its
// one volume, mounted as the caller says; readOnly mounts the volume itself
// read-only, the way a reader of the whole workspace would.
func workspacesTestPod(name, script string, mounts []corev1.VolumeMount, readOnly bool) *corev1.Pod {
	pod := probePod(name, workspacesTestGitImage, []string{"sh", "-ec", script})
	pod.Spec.Volumes = []corev1.Volume{{Name: workspacesTestVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspacesTestClaim, ReadOnly: readOnly},
	}}}
	pod.Spec.Containers[0].VolumeMounts = mounts
	return pod
}

// workspacesSessionMounts are a Session's mounts as the actor gets them:
// its own session directory read-write at the working directory, the
// mirrors read-only beside it.
func workspacesSessionMounts(session string) []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: workspacesTestVolume, MountPath: workspacesTestMountPath, SubPath: workspacesTestSessions + "/" + session},
		{Name: workspacesTestVolume, MountPath: "/mirrors", SubPath: workspacesTestMirrors, ReadOnly: true},
	}
}

// workspacesGitSetup is every pod's git identity; the volume's files are
// root's through the export's no_root_squash, like the pod.
const workspacesGitSetup = `git config --global user.email lab@agentlab.local
git config --global user.name agentlab
git config --global init.defaultBranch main
git config --global --add safe.directory '*'
`

// workspacesSeedScript lays the volume out and seeds a bare mirror from a
// repository with an executable and a symbolic link.
const workspacesSeedScript = workspacesGitSetup + `mkdir -p /workspace/` + workspacesTestMirrors + ` /workspace/` + workspacesTestSessions + `
rm -rf /tmp/src && mkdir -p /tmp/src/bin && cd /tmp/src
git init -q
echo '# workspace' > README.md
printf '#!/bin/sh\necho run.sh ran\n' > bin/run.sh && chmod 0755 bin/run.sh
ln -s README.md link
git add -A && git commit -q -m 'seed: an executable and a symbolic link'
git clone -q --bare /tmp/src /workspace/` + workspacesTestMirrors + `/` + workspacesTestRepo + `
echo "mirror ` + workspacesTestMirrors + `/` + workspacesTestRepo + ` at $(git -C /workspace/` + workspacesTestMirrors + `/` + workspacesTestRepo + ` rev-parse --short HEAD)"
echo "layout: $(ls /workspace | tr '\n' ' ')"
`

// workspacesSessionScript is a Session's work on its sub-path: a shared
// clone of the mirror (its objects borrowed read-only), the executable bit
// and the symbolic link checked after the checkout, a clean status, a commit
// of its own, a marker, and a write into the mirrors refused. Session b also
// proves it sees nothing of session a.
func workspacesSessionScript(session string) string {
	seesNothing := ""
	if session != "a" {
		seesNothing = `n=$(ls -A /workspace | wc -l); if [ "$n" != 0 ]; then echo "sees $n entries of another session: $(ls -A /workspace | tr '\n' ' ')"; exit 1; fi; echo 'nothing of session a visible on its sub-path'
`
	}
	return workspacesGitSetup + seesNothing + `git clone -q --shared /mirrors/` + workspacesTestRepo + ` /workspace/repo
echo "alternates: $(cat /workspace/repo/.git/objects/info/alternates)"
test -x /workspace/repo/bin/run.sh && echo 'executable bit kept by the checkout'
[ "$(readlink /workspace/repo/link)" = README.md ] && echo 'symbolic link kept by the checkout'
/workspace/repo/bin/run.sh
[ -z "$(git -C /workspace/repo status --porcelain)" ] && echo 'status clean'
echo 'session ` + session + `' > /workspace/repo/` + "`" + `echo ` + session + "`" + `.txt
git -C /workspace/repo add -A && git -C /workspace/repo commit -q -m 'session ` + session + `'
echo "commit $(git -C /workspace/repo rev-parse --short HEAD) stored in the session directory ($(ls /workspace/repo/.git/objects | grep -v -c '^info$\|^pack$') object dirs), the mirror untouched: $(git -C /workspace/repo log --oneline | wc -l) commits here, $(git -C /mirrors/` + workspacesTestRepo + ` log --oneline | wc -l) in the mirror"
echo 'marker' > /workspace/marker-` + session + `
if touch /mirrors/x 2>/tmp/err; then echo 'the mirrors mount took a write'; exit 1; fi
echo "mirrors read-only: $(cat /tmp/err)"
`
}

// workspacesReaderScript reads the whole volume read-only.
const workspacesReaderScript = `echo "sessions: $(ls /workspace/` + workspacesTestSessions + ` | tr '\n' ' ')"
cat /workspace/` + workspacesTestSessions + `/a/marker-a /workspace/` + workspacesTestSessions + `/b/marker-b >/dev/null && echo 'both session directories readable'
if touch /workspace/x 2>/tmp/err; then echo 'the read-only volume took a write'; exit 1; fi
echo "read-only: $(cat /tmp/err)"
`

// waitPVCBound waits for a claim of the proof's namespace to be Bound.
func waitPVCBound(ctx context.Context, name string, timeout time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	var last *corev1.PersistentVolumeClaim
	if err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pvc, err := k.clientset.CoreV1().PersistentVolumeClaims(workspacesTestNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		last = pvc
		return pvc.Status.Phase == corev1.ClaimBound, nil
	}); err != nil {
		phase := stateNotRead
		if last != nil {
			phase = string(last.Status.Phase)
		}
		return fmt.Errorf("the PVC %s/%s is %s after %s: `kubectl -n %s describe pvc %s`, the provisioner's log in %s", workspacesTestNamespace, name, phase, timeout, workspacesTestNamespace, name, workspacesNamespace)
	}
	return nil
}

// waitWorkspacesVolumesGone waits until no PersistentVolume claimed from the
// proof's namespace is left: the driver deletes a volume's directory on
// the claim's deletion.
func waitWorkspacesVolumesGone(ctx context.Context, timeout time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pvs, err := k.clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		for _, pv := range pvs.Items {
			if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == workspacesTestNamespace {
				return false, nil
			}
		}
		return true, nil
	})
}

// proveWorkspacesVolumeGone is the end: no PersistentVolume of the proof,
// and no directory of it in the export on the node.
func proveWorkspacesVolumeGone(ctx context.Context, node string, timeout time.Duration) error {
	if err := waitWorkspacesVolumesGone(ctx, timeout); err != nil {
		return fmt.Errorf("a PersistentVolume of the proof is still there %s after its claim went: `kubectl get pv`, the provisioner's log in %s", timeout, workspacesNamespace)
	}
	out, err := outputQuiet(dockerBin, "exec", node, "sh", "-c", "ls -A "+workspacesExportDir+" 2>/dev/null | grep '^pvc-' || true")
	if err != nil {
		return fmt.Errorf("reading the export on %s: %w", node, err)
	}
	if left := strings.Fields(out); len(left) > 0 {
		return fmt.Errorf("the export on %s still holds %d volume directories after the proof: %s", node, len(left), strings.Join(left, ", "))
	}
	return nil
}

// proveWorkspacesProxyRefuses opens the proxy's port as a client without
// Substrate's identity, twice: with no certificate, and with a self-signed
// one that claims ate-api-server's SPIFFE ID. Both must be refused — the
// controller endpoint is Substrate's alone.
func proveWorkspacesProxyRefuses(ctx context.Context) error {
	roots, err := signerTrustPool(ctx, serviceDNSSignerName)
	if err != nil {
		return err
	}
	pod, err := readyPodOf(ctx, workspacesNamespace, "app.kubernetes.io/name="+workspacesController)
	if err != nil {
		return err
	}
	port, stop, err := portForwardPod(ctx, workspacesNamespace, pod, workspacesProxyPort)
	if err != nil {
		return err
	}
	defer stop()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	base := &tls.Config{RootCAs: roots, ServerName: workspacesControllerServerName(), MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}

	refusal, err := workspacesProxyRefusal(ctx, addr, base.Clone())
	if err != nil {
		return fmt.Errorf("without a client certificate: %w", err)
	}
	note("without a client certificate: %s", refusal)

	impostor, err := selfSignedClientCert(ateAPIServerSPIFFEID)
	if err != nil {
		return err
	}
	withImpostor := base.Clone()
	withImpostor.Certificates = []tls.Certificate{impostor}
	if refusal, err = workspacesProxyRefusal(ctx, addr, withImpostor); err != nil {
		return fmt.Errorf("with a self-signed certificate claiming %s: %w", ateAPIServerSPIFFEID, err)
	}
	note("with a self-signed certificate claiming %s: %s", ateAPIServerSPIFFEID, refusal)
	return nil
}

// readyPodOf is the name of a Ready pod the label selector matches.
func readyPodOf(ctx context.Context, ns, selector string) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	pods, err := k.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("listing the pods %s in %s: %w", selector, ns, err)
	}
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			return pods.Items[i].Name, nil
		}
	}
	return "", fmt.Errorf("no Ready pod %s in %s", selector, ns)
}

// workspacesProxyRefusal connects with the client config, sends the HTTP/2
// preface and reads: a server that requires a trusted client certificate
// ends the connection with a TLS alert at or right after the handshake (TLS
// 1.3 delivers it after the client's Finished), which the read reports. The
// refusal's words are returned; an answer, or no verdict in time, is the
// error.
func workspacesProxyRefusal(ctx context.Context, addr string, c *tls.Config) (string, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: c}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err.Error(), nil
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")); err != nil {
		return err.Error(), nil
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err == nil {
		return "", fmt.Errorf("the proxy accepted the connection and answered %d bytes", n)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "", fmt.Errorf("no verdict within 5s: the proxy neither refused nor answered")
	}
	if errors.Is(err, io.EOF) {
		// The alert did not make it through the port-forward: the proxy
		// closed the connection right after the handshake.
		return "the proxy closed the connection after the handshake (EOF)", nil
	}
	return err.Error(), nil
}

// selfSignedClientCert is an ECDSA client certificate signed by nobody, with
// the SPIFFE ID as its URI SAN: the impostor's.
func selfSignedClientCert(spiffeID string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	id, err := url.Parse(spiffeID)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "impostor"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{id},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
