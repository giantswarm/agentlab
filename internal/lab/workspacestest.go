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
	"math/big"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
	ateapi "github.com/giantswarm/agentlab/internal/kagent/gen"
)

// WorkspacesTestOptions tunes the workspaces proof.
type WorkspacesTestOptions struct {
	// StorageOnly proves the storage and an actor's external volume, with no
	// harness turn against the workspace: the only mode so far, named so the
	// command keeps its shape once the turn is part of it.
	StorageOnly bool
	// ReadyTimeout bounds each wait: a PVC bound, a snapshot ready, a pod
	// finished, an actor's state.
	ReadyTimeout time.Duration
}

// DefaultWorkspacesReadyTimeout is the proof's default wait.
const DefaultWorkspacesReadyTimeout = 5 * time.Minute

const (
	// workspacesTestNamespace holds the proof's PVCs, snapshot and pods;
	// workspacesTestAtespace its actor template and actor, both named
	// workspacesTestName.
	workspacesTestNamespace   = "agentlab-workspaces-test"
	workspacesTestAtespace    = "agentlab-workspaces-test"
	workspacesTestName        = "heartbeat"
	workspacesTestSourcePVC   = "source"
	workspacesTestRestoredPVC = "restored"
	workspacesTestSnapshot    = "source"
	workspacesTestCapacity    = "100Mi"
	workspacesTestMountPath   = "/workspace"
	workspacesTestHeartbeat   = "heartbeat"
	// workspacesTestHeartbeatImage is the actor's container, pinned by digest
	// as Substrate requires (a changed image invalidates its snapshots).
	workspacesTestHeartbeatImage = "gsoci.azurecr.io/giantswarm/busybox:1.38.0@sha256:b6762ddf4a50aabb5f4d21aa6f447d05d5633fb09f09c08b33f22356a2f98be0"
	// workspacesTestSnapshotLocation is a prefix of the lab Substrate's
	// snapshot bucket, which the chart's own actors use too.
	workspacesTestSnapshotLocation = "s3://ate-snapshots/agentlab-workspaces-test"
	// workspacesTestSettle is how long a paused actor's volume is watched
	// for writes.
	workspacesTestSettle = 3 * time.Second
)

// WorkspacesTest is the headless workspaces proof: the storage in place ->
// a PVC from the class written by a pod -> a VolumeSnapshot of it ready ->
// a PVC restored from the snapshot whose files match -> the controller
// endpoint refused without Substrate's client certificate -> an actor with
// an external volume on the class: its heartbeat grows, stops across pause,
// keeps its content across resume, and the volume is gone with the actor.
func WorkspacesTest(cfg *config.Config, opts WorkspacesTestOptions) error {
	if !cfg.WorkspacesEnabled() {
		return fmt.Errorf("platform.workspaces is off in %s — `agentlab configure --workspaces` turns it on, then `agentlab platform`", config.File)
	}
	if !opts.StorageOnly {
		return errors.New("the proof covers the storage and an actor's external volume so far; a harness turn against a workspace is not part of it: `agentlab workspaces-test --storage-only`")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultWorkspacesReadyTimeout
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	ctx := context.Background()
	timeout := opts.ReadyTimeout
	node, onWorker := workspacesNode(cfg)

	step("The workspace storage in place (namespace %s)", workspacesNamespace)
	if err := proveWorkspacesInstalled(ctx, timeout); err != nil {
		return err
	}
	note("snapshot controller rolled out; driver %s and its proxy Ready on %s; StorageClass and VolumeSnapshotClass %s", workspacesCSIDriver, node, workspacesStorageClass)

	step("Removing what an interrupted run left (namespace %s, atespace %s)", workspacesTestNamespace, workspacesTestAtespace)
	cleanupWorkspacesTest(ctx)
	defer cleanupWorkspacesTest(context.Background())
	if err := ensureNamespace(workspacesTestNamespace); err != nil {
		return err
	}

	step("A PVC of %s from StorageClass %s, written by a pod", workspacesTestCapacity, workspacesStorageClass)
	sums, err := workspacesWriteSource(ctx, node, onWorker, timeout)
	if err != nil {
		return err
	}
	note("bound and written (a.bin 1 MiB random, b.bin 256 KiB random, c.txt):\n%s", indent(sums, "  "))

	step("A VolumeSnapshot of it from VolumeSnapshotClass %s", workspacesSnapshotClass)
	if err := workspacesSnapshotSource(ctx, timeout); err != nil {
		return err
	}
	note("readyToUse")

	step("A PVC restored from the snapshot, read by a pod")
	restored, err := workspacesReadRestored(ctx, node, onWorker, timeout)
	if err != nil {
		return err
	}
	if restored != sums {
		return fmt.Errorf("the restored PVC's files differ from the source's:\nsource:\n%s\nrestored:\n%s", indent(sums, "  "), indent(restored, "  "))
	}
	note("the restored PVC's files match the source's (sha256)")

	step("The controller endpoint without Substrate's client certificate (the proxy %s-0:%d, server name %s)", workspacesProxyStatefulSet, workspacesProxyPort, workspacesControllerServerName())
	if err := proveWorkspacesProxyRefuses(ctx); err != nil {
		return err
	}

	step("An actor with an external volume on the class, through ate-api-server (atespace %s)", workspacesTestAtespace)
	if err := workspacesActorProof(ctx, node, timeout); err != nil {
		return err
	}

	fmt.Println("\nWorkspaces proof passed: PVC -> snapshot -> restore with matching files, the controller endpoint refused without Substrate's certificate, an actor's external volume kept across pause and resume and gone with the actor.")
	return nil
}

// proveWorkspacesInstalled is the precondition: the classes, the CSIDriver,
// the snapshot controller and both StatefulSets, each waited for briefly.
func proveWorkspacesInstalled(ctx context.Context, timeout time.Duration) error {
	for _, o := range []struct{ resource, name, what string }{
		{"storageclasses.storage.k8s.io", workspacesStorageClass, "StorageClass"},
		{"volumesnapshotclasses.snapshot.storage.k8s.io", workspacesSnapshotClass, "VolumeSnapshotClass"},
		{"csidrivers.storage.k8s.io", workspacesCSIDriver, "CSIDriver"},
	} {
		if !clusterObjectExists(ctx, o.resource, o.name) {
			return fmt.Errorf("no %s %s: `agentlab platform` installs the workspace storage", o.what, o.name)
		}
	}
	if err := waitDeploymentRolledOut(ctx, workspacesNamespace, snapshotControllerDeployment, timeout); err != nil {
		return fmt.Errorf("the snapshot controller: %w", err)
	}
	for _, name := range []string{workspacesPluginStatefulSet, workspacesProxyStatefulSet} {
		if err := waitStatefulSetReady(ctx, workspacesNamespace, name, timeout); err != nil {
			return err
		}
	}
	return nil
}

// cleanupWorkspacesTest removes the proof's actor, template and atespace
// through ate-api-server and its namespace, best effort: the proof's
// opening and its end, whatever happened in between.
func cleanupWorkspacesTest(ctx context.Context) {
	if api, err := dialAteAPI(ctx); err == nil {
		ref := workspacesTestActorRef()
		if _, err := api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref, AnyState: true}); err == nil {
			_ = waitActorGone(ctx, api, ref, 2*time.Minute)
		}
		_, _ = api.DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{ActorTemplate: ref})
		_, _ = api.DeleteAtespace(ctx, &ateapi.DeleteAtespaceRequest{Atespace: &ateapi.ObjectRef{Name: workspacesTestAtespace}})
		api.Close()
	}
	_ = deleteNamespace(ctx, workspacesTestNamespace)
}

func workspacesTestActorRef() *ateapi.ObjectRef {
	return &ateapi.ObjectRef{Atespace: workspacesTestAtespace, Name: workspacesTestName}
}

// workspacesTestPVC is a claim of the proof's capacity on the class; a
// restore names the snapshot as its data source.
func workspacesTestPVC(name, fromSnapshot string) *corev1.PersistentVolumeClaim {
	class := workspacesStorageClass
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workspacesTestNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(workspacesTestCapacity)},
			},
		},
	}
	if fromSnapshot != "" {
		group := gvrVolumeSnapshots.Group
		pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: &group, Kind: "VolumeSnapshot", Name: fromSnapshot}
	}
	return pvc
}

// workspacesTestPod is a probe pod with the claim mounted at /data, pinned
// to the driver's node like the driver (the PV's topology pins it too; the
// substrate worker's taint needs the toleration).
func workspacesTestPod(name, pvc, node string, onWorker bool, script string) *corev1.Pod {
	pod := probePod(name, probeImage, []string{"sh", "-ec", script})
	pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
	}}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	pod.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	if onWorker {
		pod.Spec.Tolerations = []corev1.Toleration{{Key: config.SubstrateNodeKey, Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	}
	return pod
}

// workspacesWriteSource creates the source claim and fills it: two random
// files and a text file, their sha256 sums returned.
func workspacesWriteSource(ctx context.Context, node string, onWorker bool, timeout time.Duration) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	if _, err := k.clientset.CoreV1().PersistentVolumeClaims(workspacesTestNamespace).Create(ctx, workspacesTestPVC(workspacesTestSourcePVC, ""), metav1.CreateOptions{FieldManager: applyFieldManager}); err != nil {
		return "", fmt.Errorf("creating the PVC %s/%s: %w", workspacesTestNamespace, workspacesTestSourcePVC, err)
	}
	if err := waitPVCBound(ctx, workspacesTestSourcePVC, timeout); err != nil {
		return "", err
	}
	script := "head -c 1048576 /dev/urandom > /data/a.bin && head -c 262144 /dev/urandom > /data/b.bin && echo 'restored from a snapshot' > /data/c.txt && sync && cd /data && sha256sum a.bin b.bin c.txt"
	out, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("writer", workspacesTestSourcePVC, node, onWorker, script), timeout)
	if err != nil {
		return "", fmt.Errorf("the writer pod: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// workspacesReadRestored restores a claim from the snapshot and sums its
// files the same way.
func workspacesReadRestored(ctx context.Context, node string, onWorker bool, timeout time.Duration) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	if _, err := k.clientset.CoreV1().PersistentVolumeClaims(workspacesTestNamespace).Create(ctx, workspacesTestPVC(workspacesTestRestoredPVC, workspacesTestSnapshot), metav1.CreateOptions{FieldManager: applyFieldManager}); err != nil {
		return "", fmt.Errorf("creating the PVC %s/%s from the snapshot: %w", workspacesTestNamespace, workspacesTestRestoredPVC, err)
	}
	if err := waitPVCBound(ctx, workspacesTestRestoredPVC, timeout); err != nil {
		return "", err
	}
	out, err := runPod(ctx, workspacesTestNamespace, workspacesTestPod("reader", workspacesTestRestoredPVC, node, onWorker, "cd /data && sha256sum a.bin b.bin c.txt"), timeout)
	if err != nil {
		return "", fmt.Errorf("the reader pod: %w\n%s", err, out)
	}
	return strings.TrimSpace(out), nil
}

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
		phase := "not read"
		if last != nil {
			phase = string(last.Status.Phase)
		}
		return fmt.Errorf("the PVC %s/%s is %s after %s: `kubectl -n %s describe pvc %s`, the provisioner's log in %s", workspacesTestNamespace, name, phase, timeout, workspacesTestNamespace, name, workspacesNamespace)
	}
	return nil
}

// workspacesSnapshotSource snapshots the source claim and waits for
// readyToUse; a status.error is the failure's words.
func workspacesSnapshotSource(ctx context.Context, timeout time.Duration) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	snapshot := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvrVolumeSnapshots.Group + "/" + gvrVolumeSnapshots.Version,
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]any{"name": workspacesTestSnapshot, "namespace": workspacesTestNamespace},
		"spec": map[string]any{
			"volumeSnapshotClassName": workspacesSnapshotClass,
			"source":                  map[string]any{"persistentVolumeClaimName": workspacesTestSourcePVC},
		},
	}}
	snapshots := k.dynamic.Resource(gvrVolumeSnapshots).Namespace(workspacesTestNamespace)
	if _, err := snapshots.Create(ctx, snapshot, metav1.CreateOptions{FieldManager: applyFieldManager}); err != nil {
		return fmt.Errorf("creating the VolumeSnapshot %s/%s: %w", workspacesTestNamespace, workspacesTestSnapshot, err)
	}
	var lastErr string
	if err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		s, err := snapshots.Get(ctx, workspacesTestSnapshot, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		if msg, _, _ := unstructured.NestedString(s.Object, "status", "error", "message"); msg != "" {
			lastErr = msg
		}
		ready, _, _ := unstructured.NestedBool(s.Object, "status", "readyToUse")
		return ready, nil
	}); err != nil {
		if lastErr != "" {
			return fmt.Errorf("the VolumeSnapshot %s/%s is not readyToUse after %s: %s", workspacesTestNamespace, workspacesTestSnapshot, timeout, lastErr)
		}
		return fmt.Errorf("the VolumeSnapshot %s/%s is not readyToUse after %s: `kubectl -n %s describe volumesnapshot %s`, the snapshotter's log in %s", workspacesTestNamespace, workspacesTestSnapshot, timeout, workspacesTestNamespace, workspacesTestSnapshot, workspacesNamespace)
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
	port, stop, err := portForwardPod(ctx, workspacesNamespace, workspacesProxyStatefulSet+"-0", workspacesProxyPort)
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
	defer conn.Close()
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

// workspacesActorTemplate is the proof's template: one busybox container
// appending the date to the workspace every second, on the pool's workers,
// in the gVisor sandbox, its workspace an external volume on the class.
func workspacesActorTemplate(pool string) *ateapi.ActorTemplate {
	return &ateapi.ActorTemplate{
		Metadata:       &ateapi.ResourceMetadata{Atespace: workspacesTestAtespace, Name: workspacesTestName},
		WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{substrateWorkerPoolLabel: pool}},
		Containers: []*ateapi.Container{{
			Name:         workspacesTestName,
			Image:        workspacesTestHeartbeatImage,
			Command:      []string{"sh", "-c", "while true; do date >> " + workspacesTestMountPath + "/" + workspacesTestHeartbeat + "; sleep 1; done"},
			VolumeMounts: []*ateapi.VolumeMount{{Name: "workspace", MountPath: workspacesTestMountPath}},
		}},
		Volumes: []*ateapi.Volume{{
			Name:                   "workspace",
			ExternalVolumeTemplate: &ateapi.ExternalVolumeTemplate{Capacity: workspacesTestCapacity, StorageClassName: workspacesStorageClass},
		}},
		SandboxConfig: &ateapi.SandboxConfig{SandboxClass: ateapi.SandboxClass_SANDBOX_CLASS_GVISOR, ConfigName: substrateGVisorConfig},
		SnapshotConfig: &ateapi.SnapshotConfig{
			OnPause:         ateapi.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnCommit:        ateapi.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
			OnResume:        &ateapi.OnResumeConfig{FromData: ateapi.ResumeSource_RESUME_SOURCE_COLD_BOOT},
			StorageLocation: workspacesTestSnapshotLocation,
		},
	}
}

const (
	// substrateWorkerPoolLabel is the label Substrate's workers carry with
	// their pool's name, what a template's worker selector matches.
	substrateWorkerPoolLabel = "ate.dev/worker-pool"
	// substrateGVisorConfig is the chart's gVisor sandbox configuration.
	substrateGVisorConfig = "gvisor-default"
)

// firstWorkerPool is the name of the first WorkerPool in the kagent
// namespace: the chart's.
func firstWorkerPool(ctx context.Context) (string, error) {
	gvr, err := gvrFor(workerPoolsResource)
	if err != nil {
		return "", err
	}
	pools, err := listObjects(ctx, gvr, kagentNamespace, "")
	if err != nil {
		return "", fmt.Errorf("listing the WorkerPools in %s: %w", kagentNamespace, err)
	}
	if len(pools) == 0 {
		return "", fmt.Errorf("no WorkerPool in %s: the kagent chart creates it", kagentNamespace)
	}
	return pools[0].GetName(), nil
}

// workspacesActorProof drives an actor whose workspace is an external
// volume on the class: created and RUNNING -> its heartbeat on the node
// grows -> PAUSED, the heartbeat stands still -> RUNNING again, the paused
// lines kept and growing -> deleted, the volume's directory gone.
func workspacesActorProof(ctx context.Context, node string, timeout time.Duration) error {
	if !clusterObjectExists(ctx, csiDriverConfigResource, workspacesCSIDriver) {
		return fmt.Errorf("no CSIDriverConfig %s: `agentlab platform` registers the driver with Substrate", workspacesCSIDriver)
	}
	api, err := dialAteAPI(ctx)
	if err != nil {
		return err
	}
	defer api.Close()
	pool, err := firstWorkerPool(ctx)
	if err != nil {
		return err
	}
	ref := workspacesTestActorRef()
	if _, err := api.CreateAtespace(ctx, &ateapi.CreateAtespaceRequest{Atespace: &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: workspacesTestAtespace}}}); err != nil {
		return fmt.Errorf("creating the atespace %s: %w", workspacesTestAtespace, err)
	}
	if _, err := api.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: workspacesActorTemplate(pool)}); err != nil {
		return fmt.Errorf("creating the actor template %s/%s: %w", workspacesTestAtespace, workspacesTestName, err)
	}
	note("atespace %s, actor template %s (worker pool %s, sandbox %s, workspace: an external volume of %s on %s)", workspacesTestAtespace, workspacesTestName, pool, substrateGVisorConfig, workspacesTestCapacity, workspacesStorageClass)

	if _, err := api.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{Metadata: &ateapi.ResourceMetadata{Atespace: workspacesTestAtespace, Name: workspacesTestName}, ActorTemplate: ref}}); err != nil {
		return fmt.Errorf("creating the actor %s/%s: %w", workspacesTestAtespace, workspacesTestName, err)
	}
	actor, err := waitActorState(ctx, api, ref, ateapi.ActorState_ACTOR_STATE_RUNNING, timeout)
	if err != nil {
		return err
	}
	volumes := actor.GetStatus().GetActorVolumes()
	if len(volumes) == 0 || volumes[0].GetStorageVolumeId() == "" {
		return fmt.Errorf("the actor is RUNNING with no external volume in its status (%d volumes)", len(volumes))
	}
	volume := volumes[0]
	file := workspacesDataDir + "/" + volume.GetStorageVolumeId() + "/" + workspacesTestHeartbeat
	running, err := waitHeartbeatLines(node, file, 3, timeout)
	if err != nil {
		return err
	}
	note("RUNNING; volume %s (%s): %s on %s has %d lines", volume.GetStorageVolumeId(), volume.GetStatus(), file, node, running)

	if _, err := api.PauseActor(ctx, &ateapi.PauseActorRequest{Actor: ref}); err != nil {
		return fmt.Errorf("pausing the actor: %w", err)
	}
	if _, err := waitActorState(ctx, api, ref, ateapi.ActorState_ACTOR_STATE_PAUSED, timeout); err != nil {
		return err
	}
	paused, err := heartbeatLines(node, file)
	if err != nil {
		return err
	}
	pausedSum, err := heartbeatPrefixSum(node, file, paused)
	if err != nil {
		return err
	}
	time.Sleep(workspacesTestSettle)
	if still, err := heartbeatLines(node, file); err != nil {
		return err
	} else if still != paused {
		return fmt.Errorf("the paused actor's volume still grows: %d lines, then %d after %s", paused, still, workspacesTestSettle)
	}
	note("PAUSED; %d lines, unchanged over %s", paused, workspacesTestSettle)

	if _, err := api.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref}); err != nil {
		return fmt.Errorf("resuming the actor: %w", err)
	}
	if _, err := waitActorState(ctx, api, ref, ateapi.ActorState_ACTOR_STATE_RUNNING, timeout); err != nil {
		return err
	}
	resumed, err := waitHeartbeatLines(node, file, paused+1, timeout)
	if err != nil {
		return err
	}
	if sum, err := heartbeatPrefixSum(node, file, paused); err != nil {
		return err
	} else if sum != pausedSum {
		return fmt.Errorf("the resumed actor's volume lost its content: the first %d lines changed (%s, then %s)", paused, pausedSum, sum)
	}
	note("RUNNING again; the %d paused lines kept, %d lines now", paused, resumed)

	if _, err := api.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref, AnyState: true}); err != nil {
		return fmt.Errorf("deleting the actor: %w", err)
	}
	if err := waitActorGone(ctx, api, ref, timeout); err != nil {
		return err
	}
	dir := workspacesDataDir + "/" + volume.GetStorageVolumeId()
	if err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(context.Context) (bool, error) {
		_, err := outputQuiet(dockerBin, "exec", node, "test", "-e", dir)
		return err != nil, nil
	}); err != nil {
		return fmt.Errorf("the actor is gone, its volume's directory %s on %s is still there after %s", dir, node, timeout)
	}
	note("deleted; GetActor NotFound, %s gone from %s", dir, node)
	return nil
}

// waitActorState polls the actor until it is in the state; CRASHED is the
// failure with the crash's words.
func waitActorState(ctx context.Context, api *ateAPI, ref *ateapi.ObjectRef, want ateapi.ActorState, timeout time.Duration) (*ateapi.Actor, error) {
	var last *ateapi.Actor
	var crash error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		actor, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
		if err != nil {
			return false, nil
		}
		last = actor
		switch actor.GetStatus().GetState() {
		case want:
			return true, nil
		case ateapi.ActorState_ACTOR_STATE_CRASHED:
			crash = fmt.Errorf("the actor %s/%s CRASHED on the way to %s: %s", ref.Atespace, ref.Name, want, actor.GetStatus().GetCrash().GetMessage())
			return false, crash
		}
		return false, nil
	})
	if crash != nil {
		return nil, crash
	}
	if err != nil {
		state := "not read"
		if last != nil {
			state = last.GetStatus().GetState().String()
		}
		return nil, fmt.Errorf("the actor %s/%s is %s, not %s, after %s: `kubectl -n %s logs deploy/ate-controller`", ref.Atespace, ref.Name, state, want, timeout, substrateNamespace)
	}
	return last, nil
}

// waitActorGone polls until GetActor answers NotFound.
func waitActorGone(ctx context.Context, api *ateAPI, ref *ateapi.ObjectRef, timeout time.Duration) error {
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := api.GetActor(ctx, &ateapi.GetActorRequest{Actor: ref})
		return status.Code(err) == codes.NotFound, nil
	}); err != nil {
		return fmt.Errorf("the actor %s/%s is still there %s after its deletion", ref.Atespace, ref.Name, timeout)
	}
	return nil
}

// heartbeatLines counts the heartbeat file's lines on the node, through
// `docker exec`: the driver keeps the volume's bytes on the node's disk.
func heartbeatLines(node, file string) (int, error) {
	out, err := outputQuiet(dockerBin, "exec", node, "sh", "-c", "wc -l < "+file)
	if err != nil {
		return 0, fmt.Errorf("reading %s on %s: %w", file, node, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("reading %s on %s: %q is no line count", file, node, strings.TrimSpace(out))
	}
	return n, nil
}

// heartbeatPrefixSum is the sha256 of the file's first n lines.
func heartbeatPrefixSum(node, file string, n int) (string, error) {
	out, err := outputQuiet(dockerBin, "exec", node, "sh", "-c", fmt.Sprintf("head -n %d %s | sha256sum", n, file))
	if err != nil {
		return "", fmt.Errorf("reading %s on %s: %w", file, node, err)
	}
	return strings.Fields(out)[0], nil
}

// waitHeartbeatLines waits until the file has at least min lines.
func waitHeartbeatLines(node, file string, min int, timeout time.Duration) (int, error) {
	var n int
	if err := wait.PollUntilContextTimeout(context.Background(), time.Second, timeout, true, func(context.Context) (bool, error) {
		var err error
		n, err = heartbeatLines(node, file)
		return err == nil && n >= min, nil
	}); err != nil {
		return 0, fmt.Errorf("%s on %s has %d lines, fewer than %d, after %s: the actor's container writes nothing into its workspace", file, node, n, min, timeout)
	}
	return n, nil
}
