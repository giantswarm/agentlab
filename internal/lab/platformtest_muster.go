package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// muster on the pod network (platform.musterPodNetwork), as platform-test
// proves it: the Deployment runs MusterReplicas Ready pods off the host
// network — each with the dex-localhost sidecar while the issuer is spelled
// localhost — behind a PodDisruptionBudget of minAvailable 1; then one pod is
// deleted while the proof's MCP session keeps calling muster through the
// edge, and not one call may fail until the replacement is Ready and the
// Deployment is whole again. muster's MCP sessions need no affinity: a
// session ID is checked for its format only, and the session and OAuth
// stores are Valkey's, so the surviving pod serves the session as is.

// musterProbeInterval spaces the session's calls through the edge while a
// pod is replaced.
const musterProbeInterval = 250 * time.Millisecond

// musterReplaceWait bounds the deleted pod's replacement: scheduled, its
// image on the node, the sidecar's startup probe, muster's readiness (OIDC
// discovery, the Valkey stores, the downstream servers).
const musterReplaceWait = 3 * time.Minute

// musterSettle is how long the probe goes on once the Deployment is whole
// again: the edge's endpoints catching up with the replacement.
const musterSettle = 5 * time.Second

// musterAvailability is what the stage found, for the verdict.
type musterAvailability struct {
	pods                 []string
	deleted, replacement string
	calls, failed        int
	firstFailure         string
	replaced             time.Duration
}

// proveMusterAvailability runs the stage; probe is one MCP call on the
// proof's session through the edge, its error the call's failure.
func proveMusterAvailability(ctx context.Context, cfg *config.Config, probe func() error) (*musterAvailability, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	deploys := k.clientset.AppsV1().Deployments(platformNamespace)
	d, err := deploys.Get(ctx, componentMuster, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", describe(gvrDeployments, platformNamespace, componentMuster), err)
	}
	want := cfg.MusterReplicas()
	if d.Spec.Replicas == nil || int(*d.Spec.Replicas) != want {
		return nil, fmt.Errorf("%s asks for %s replicas, want %d (platform.musterPodNetwork; `agentlab platform` renders replicaCount)", describe(gvrDeployments, platformNamespace, componentMuster), replicasText(d.Spec.Replicas), want)
	}
	pods, err := readyMusterPods(ctx, k, d, want)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		if err := assertPodNetworkMuster(cfg, &pods[i]); err != nil {
			return nil, err
		}
	}
	if err := assertMusterBudget(ctx, k); err != nil {
		return nil, err
	}
	out := &musterAvailability{}
	old := map[string]bool{}
	for i := range pods {
		out.pods = append(out.pods, pods[i].Name)
		old[pods[i].Name] = true
	}
	slices.Sort(out.pods)
	note("%d Ready pods off the host network: %s; PodDisruptionBudget %s minAvailable 1", want, strings.Join(out.pods, ", "), componentMuster)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wg.Go(func() {
		tick := time.NewTicker(musterProbeInterval)
		defer tick.Stop()
		for {
			err := probe()
			mu.Lock()
			out.calls++
			if err != nil {
				out.failed++
				if out.firstFailure == "" {
					out.firstFailure = err.Error()
				}
			}
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	})
	finish := func() {
		close(stop)
		wg.Wait()
	}

	out.deleted = out.pods[0]
	began := time.Now()
	note("deleting pod %s while the session calls muster through the edge every %s", out.deleted, musterProbeInterval)
	if err := k.clientset.CoreV1().Pods(platformNamespace).Delete(ctx, out.deleted, metav1.DeleteOptions{}); err != nil {
		finish()
		return nil, fmt.Errorf("deleting muster pod %s: %w", out.deleted, err)
	}
	// Whole again: want Ready pods, the deleted one gone — its terminating
	// pod still counts until it is.
	ready, err := readyMusterPods(ctx, k, d, want)
	for i := range ready {
		if !old[ready[i].Name] {
			out.replacement = ready[i].Name
		}
	}
	if err == nil && out.replacement == "" {
		err = fmt.Errorf("muster has %d Ready pods but none replaced %s", want, out.deleted)
	}
	out.replaced = time.Since(began).Round(time.Second)
	if err == nil {
		time.Sleep(musterSettle)
	}
	finish()
	if err != nil {
		return nil, err
	}
	if out.failed > 0 {
		return nil, fmt.Errorf("%d of %d calls on the session failed while pod %s was replaced (the first: %s): one muster replica did not keep muster available", out.failed, out.calls, out.deleted, out.firstFailure)
	}
	return out, nil
}

// readyMusterPods waits, within musterReplaceWait, until the Deployment has
// want Ready pods and no other, and returns them.
func readyMusterPods(ctx context.Context, k *kubeClients, d *appsv1.Deployment, want int) ([]corev1.Pod, error) {
	var ready []corev1.Pod
	var last []string
	err := wait.PollUntilContextTimeout(ctx, time.Second, musterReplaceWait, true, func(ctx context.Context) (bool, error) {
		pods, err := deploymentPods(ctx, k, d)
		if err != nil {
			return false, nil
		}
		ready, last = ready[:0], last[:0]
		for i := range pods {
			last = append(last, pods[i].Name+" "+podStateSummary(&pods[i]))
			if podReady(&pods[i]) {
				ready = append(ready, pods[i])
			}
		}
		return len(ready) == want && len(pods) == want, nil
	})
	if wait.Interrupted(err) {
		return nil, fmt.Errorf("%s has not %d Ready pods within %s (%s)", describe(gvrDeployments, d.Namespace, d.Name), want, musterReplaceWait, strings.Join(last, "; "))
	}
	return ready, err
}

// assertPodNetworkMuster checks one muster pod's shape: off the host network,
// and with the dex-localhost sidecar while the issuer is spelled localhost.
func assertPodNetworkMuster(cfg *config.Config, pod *corev1.Pod) error {
	if pod.Spec.HostNetwork {
		return fmt.Errorf("muster pod %s runs on the host network although platform.musterPodNetwork is on", pod.Name)
	}
	hasSidecar := slices.ContainsFunc(pod.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == dexLocalhostContainer })
	if musterSidecar(cfg) && !hasSidecar {
		return fmt.Errorf("muster pod %s has no %s sidecar: it cannot reach the issuer %s", pod.Name, dexLocalhostContainer, cfg.Issuer())
	}
	if !musterSidecar(cfg) && hasSidecar {
		return fmt.Errorf("muster pod %s carries the %s sidecar although the issuer %s resolves through cluster DNS", pod.Name, dexLocalhostContainer, cfg.Issuer())
	}
	return nil
}

// assertMusterBudget checks the PodDisruptionBudget keeps one muster pod.
func assertMusterBudget(ctx context.Context, k *kubeClients) error {
	pdb, err := k.clientset.PolicyV1().PodDisruptionBudgets(platformNamespace).Get(ctx, componentMuster, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading PodDisruptionBudget %s/%s: %w", platformNamespace, componentMuster, err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 || pdb.Spec.MaxUnavailable != nil {
		return fmt.Errorf("PodDisruptionBudget %s/%s is minAvailable %v maxUnavailable %v, want minAvailable 1 alone", platformNamespace, componentMuster, pdb.Spec.MinAvailable, pdb.Spec.MaxUnavailable)
	}
	return nil
}

// replicasText is a Deployment's replica count as the error names it.
func replicasText(r *int32) string {
	if r == nil {
		return "the default"
	}
	return fmt.Sprint(*r)
}
