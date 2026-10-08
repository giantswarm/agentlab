package lab

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// dexLocalhostServer is one OAuth resource server platform-test proved the
// lab's Dex bridge on: its Deployment, the key the sidecar rule selected it
// by (dexLocalhostKey) and the MCPServer that reads Connected for it (""
// when it registers none).
type dexLocalhostServer struct {
	namespace, deployment, key, mcpServer string
}

func (s dexLocalhostServer) String() string {
	name := s.deployment
	if s.mcpServer == "" {
		name = s.namespace + "/" + s.deployment
	}
	return name + " (" + s.key + ")"
}

// proveDexLocalhostSidecars is platform-test's check of the lab's OAuth
// bridge, the sidecar rule (dexLocalhostKey) read off the live objects this
// time: every Deployment in the platform namespace (and monitoring, with
// observability) whose containers are told the lab Dex's localhost address
// through a name they dial it by, or that the annotation selects, and that
// is not on the host network must carry the dex-localhost container; its
// rollout
// must be complete with every pod Ready, every container running and none
// restarted since the release (restartedSinceRelease) — an unreachable
// issuer is a crash loop, and that is what this catches; and the MCPServer of its name, when it registers one, must
// read Connected: muster reached it through the bridge with this session's
// token. A server the rule finds without the sidecar is the lab's gap — a
// component turned on by the chart or an overlay that `agentlab platform`
// did not patch.
func proveDexLocalhostSidecars(ctx context.Context, cfg *config.Config) ([]dexLocalhostServer, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil, err
	}
	namespaces := []string{platformNamespace}
	if cfg.Platform.Observability {
		namespaces = append(namespaces, observabilityNamespace)
	}
	addr := dexLocalhostAddr(cfg)
	var proven []dexLocalhostServer
	for _, ns := range namespaces {
		list, err := k.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing Deployments in %s: %w", ns, err)
		}
		slices.SortFunc(list.Items, func(a, b appsv1.Deployment) int { return strings.Compare(a.Name, b.Name) })
		for i := range list.Items {
			d := &list.Items[i]
			key, ok, err := deploymentDexLocalhostKey(d, addr)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if !dexLocalhostBridged(cfg) {
				// Under the platform domain the issuer is cluster DNS's
				// to answer: a leftover bridge would hide a pod that
				// cannot resolve it.
				if hasDexLocalhostSidecar(d) {
					return nil, fmt.Errorf("deployment %s/%s (%s) still carries the %s sidecar, though the issuer %s is answered by cluster DNS", ns, d.Name, key, dexLocalhostContainer, cfg.Issuer())
				}
			} else if err := checkDexLocalhostDeployment(ctx, k, d, key); err != nil {
				return nil, err
			}
			server := dexLocalhostServer{namespace: ns, deployment: d.Name, key: key}
			if registered, err := objectExists(ctx, mcpServers, platformNamespace, d.Name); err != nil {
				return nil, err
			} else if registered {
				server.mcpServer = d.Name
				if err := waitMCPServerState(d.Name, mcpServerStateConnected); err != nil {
					return nil, err
				}
			} else {
				note("%s/%s (%s) carries the sidecar and registers no MCPServer of its name", ns, d.Name, key)
			}
			proven = append(proven, server)
		}
	}
	if len(proven) == 0 {
		return nil, fmt.Errorf("no Deployment in %s is told the lab Dex address %s through a name it dials it by (%s) — the platform's OAuth resource servers (mcp-kubernetes at least) should be; check `kubectl -n %s get deploy` and `agentlab logs mcp-kubernetes`", strings.Join(namespaces, ", "), addr, strings.Join(dexDialNames, ", "), platformNamespace)
	}
	return proven, nil
}

// hasDexLocalhostSidecar reports whether a Deployment's pods carry the
// bridge (an init container, restartPolicy Always).
func hasDexLocalhostSidecar(d *appsv1.Deployment) bool {
	return slices.ContainsFunc(d.Spec.Template.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == dexLocalhostContainer })
}

// checkDexLocalhostDeployment asserts one selected Deployment (key: what the
// rule selected it by) carries the sidecar — a native sidecar, among the init
// containers — rolled out completely, with every pod Ready, every container
// (the sidecar included) running and none restarted since the release.
func checkDexLocalhostDeployment(ctx context.Context, k *kubeClients, d *appsv1.Deployment, key string) error {
	where := d.Namespace + "/" + d.Name
	if !hasDexLocalhostSidecar(d) {
		return fmt.Errorf("the Deployment %s is told the lab Dex address through %s but carries no %s sidecar — the lab did not patch this component; check the `%s sidecar on …` line of `agentlab platform` and components.%s.postRenderers in state/agent-platform-values.yaml; a server that carries the address without dialing it opts out with the annotation %s=false",
			where, key, dexLocalhostContainer, dexLocalhostContainer, d.Name, dexLocalhostAnnotation)
	}
	status, done, err := deploymentRolloutStatus(d)
	if err != nil {
		return fmt.Errorf("the Deployment %s: %w", where, err)
	}
	if !done {
		return fmt.Errorf("the Deployment %s has not rolled out: %s", where, status)
	}
	pods, err := k.targetPods(ctx, d.Namespace, "deploy/"+d.Name)
	if err != nil {
		return err
	}
	released, err := deploymentReleasedAt(ctx, d)
	if err != nil {
		return err
	}
	now := time.Now()
	var restarts, settled []string
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("pod %s/%s of Deployment %s is %s, want Running (%s)", pod.Namespace, pod.Name, d.Name, pod.Status.Phase, podStateSummary(pod))
		}
		// The sidecar is the one init container that keeps running; the
		// chart's own init containers complete and are not looked at.
		sidecar := slices.DeleteFunc(slices.Clone(pod.Status.InitContainerStatuses), func(c corev1.ContainerStatus) bool { return c.Name != dexLocalhostContainer })
		for _, c := range slices.Concat(sidecar, pod.Status.ContainerStatuses) {
			if !c.Ready || c.State.Running == nil {
				return fmt.Errorf("container %s of pod %s/%s is not running and ready (%s) — with %s the issuer is reachable, so look at `kubectl -n %s logs %s -c %s`",
					c.Name, pod.Namespace, pod.Name, podStateSummary(pod), dexLocalhostContainer, pod.Namespace, pod.Name, c.Name)
			}
			if c.RestartCount == 0 {
				continue
			}
			restart := fmt.Sprintf("%s/%s %s ×%d (running since %s)", pod.Namespace, pod.Name, c.Name, c.RestartCount, c.State.Running.StartedAt.UTC().Format(time.RFC3339))
			if restartedSinceRelease(c, released, now) {
				restarts = append(restarts, restart)
			} else {
				settled = append(settled, restart)
			}
		}
	}
	if len(restarts) > 0 {
		return fmt.Errorf("the Deployment %s: containers restarted since the release — %s; a server that could not reach the issuer crash-loops until the sidecar is there, so read the previous container's log (`kubectl -n %s logs -p …`)",
			where, strings.Join(restarts, ", "), d.Namespace)
	}
	if len(settled) > 0 {
		note("%s: restarts before the release (%s) or running steadily for %s since, not a crash loop: %s", where, releasedLabel(released), crashLoopSettle, strings.Join(settled, ", "))
	}
	note("%s (%s): %s sidecar, %d pods Running, 0 restarts since the release", where, key, dexLocalhostContainer, len(pods))
	return nil
}

// crashLoopSettle is how long a container runs before kubelet resets its
// crash-loop back-off: one that has run this long since its last restart is
// out of the loop.
const crashLoopSettle = 10 * time.Minute

// restartedSinceRelease reports whether a running container restarted in a
// way platform-test fails on: its current run started after the release
// (a restart before it — a node restart during the bring-up, an earlier
// revision's — is history) and less than crashLoopSettle ago (a restart the
// container has since run steadily through — a host suspend — is no crash
// loop). A zero release counts every restart the settle allows.
func restartedSinceRelease(c corev1.ContainerStatus, released, now time.Time) bool {
	if c.RestartCount == 0 || c.State.Running == nil {
		return false
	}
	started := c.State.Running.StartedAt.Time
	return started.After(released) && now.Sub(started) < crashLoopSettle
}

// deploymentReleasedAt is when the Flux HelmRelease that renders the
// Deployment (its helm.toolkit.fluxcd.io labels) last deployed it
// (status.history[0].lastDeployed); zero for a Deployment no HelmRelease
// renders.
func deploymentReleasedAt(ctx context.Context, d *appsv1.Deployment) (time.Time, error) {
	name, ns := d.Labels[fluxHelmReleaseNameLabel], d.Labels[gitopsHelmNamespace]
	if name == "" || ns == "" {
		return time.Time{}, nil
	}
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return time.Time{}, err
	}
	hr, err := getObject(ctx, gvr, ns, name)
	if err != nil {
		return time.Time{}, fmt.Errorf("the Deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	return helmReleaseLastDeployed(hr)
}

// helmReleaseLastDeployed reads status.history[0].lastDeployed off a Flux
// HelmRelease: the time of the revision running now.
func helmReleaseLastDeployed(hr *unstructured.Unstructured) (time.Time, error) {
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return time.Time{}, fmt.Errorf("HelmRelease %s/%s has no revision in status.history", hr.GetNamespace(), hr.GetName())
	}
	snapshot, _ := history[0].(map[string]any)
	raw, _, _ := unstructured.NestedString(snapshot, "lastDeployed")
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("HelmRelease %s/%s: status.history[0].lastDeployed %q: %w", hr.GetNamespace(), hr.GetName(), raw, err)
	}
	return t, nil
}

// releasedLabel names a release time for a note.
func releasedLabel(released time.Time) string {
	if released.IsZero() {
		return "no HelmRelease"
	}
	return "released " + released.UTC().Format(time.RFC3339)
}

// deploymentDexLocalhostKey is the sidecar rule (dexLocalhostKey) over a live
// Deployment: the key it is selected by and whether it is, or the error of an
// annotation the rule cannot read.
func deploymentDexLocalhostKey(d *appsv1.Deployment, addr string) (string, bool, error) {
	key, ok, err := dexLocalhostKey(addr, d.Spec.Template.Spec.HostNetwork, podSpecTold(&d.Spec.Template.Spec), d.Annotations, d.Spec.Template.Annotations)
	if err != nil {
		return "", false, fmt.Errorf("the Deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	return key, ok, nil
}

// podSpecTold yields every argument (toldArgs) and environment variable of
// the pod spec's containers as name/value pairs — the live counterpart of
// renderedWorkload.told. A variable read from a Secret or ConfigMap
// (valueFrom) has no value here and selects nothing.
func podSpecTold(spec *corev1.PodSpec) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for _, c := range spec.Containers {
			if !toldArgs(c.Args, yield) {
				return
			}
			for _, env := range c.Env {
				if !yield(env.Name, env.Value) {
					return
				}
			}
		}
	}
}
