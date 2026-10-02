package lab

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

// The WorkerPool's workers are ate-controller's pods, not the Helm release's:
// the kagent release goes Ready and the Substrate release pair agrees
// (proveSubstrateLine) while every worker sits Pending — a taint, a resource
// ceiling, a node selector no node carries. Nothing else in the install says
// so; the first agent turn waits for a worker that never comes. So
// `platform-test` asserts the workers themselves: every one Running and
// Ready, at least the pool's replicas of them, each on a node that carries
// the pool's spec.template.nodeSelector.

// workerPoolRunningTimeout bounds the wait for the workers to settle: a pool
// rolling after `agentlab platform` replaces its workers within seconds, a
// Pending one stays Pending.
const workerPoolRunningTimeout = 2 * time.Minute

// poolWorkers is one WorkerPool's workers as the proof found them.
type poolWorkers struct {
	pool     string            // namespace/name
	replicas int64             // spec.replicas
	running  int               // workers Running and Ready
	nodes    []string          // the nodes they run on, sorted
	selector map[string]string // spec.template.nodeSelector
}

func (p poolWorkers) String() string {
	pin := "no node selector"
	if len(p.selector) > 0 {
		pin = selectorString(p.selector)
	}
	return fmt.Sprintf("WorkerPool %s: %d/%d workers Running on %s (%s)", p.pool, p.running, p.replicas, strings.Join(p.nodes, ", "), pin)
}

// proveWorkerPoolsRunning waits, up to timeout, for the workers of every
// WorkerPool in the kagent namespace to be Running and Ready on a node that
// carries the pool's node selector, and refuses the lab otherwise, naming
// each worker that is not and why (the scheduler's words for a Pending one).
func proveWorkerPoolsRunning(ctx context.Context, timeout time.Duration) ([]poolWorkers, error) {
	var (
		found   []poolWorkers
		lastErr error
	)
	waitErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		found, lastErr = readWorkerPools(ctx)
		var refused workerPoolRefusal
		switch {
		case lastErr == nil:
			return true, nil
		case errors.As(lastErr, &refused):
			return false, nil // the workers may still settle
		default:
			return false, lastErr // a read that failed is not a verdict
		}
	})
	if waitErr != nil && lastErr == nil {
		lastErr = waitErr
	}
	return found, lastErr
}

// workerPoolRefusal is the verdict on a pool whose workers are not all
// Running where they belong.
type workerPoolRefusal struct{ msg string }

func (e workerPoolRefusal) Error() string { return e.msg }

// readWorkerPools reads the pools, their workers and the workers' nodes once.
func readWorkerPools(ctx context.Context) ([]poolWorkers, error) {
	poolGVR, err := gvrFor(workerPoolsResource)
	if err != nil {
		return nil, err
	}
	pools, err := listObjects(ctx, poolGVR, kagentNamespace, "")
	if err != nil {
		return nil, fmt.Errorf("listing the WorkerPools in %s: %w", kagentNamespace, err)
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("no WorkerPool in %s — the kagent chart creates it (substrateWorkerPool.create) and the platform Harness runs its actors on it; `kubectl -n %s get helmrelease kagent`", kagentNamespace, kagentNamespace)
	}
	nodeGVR, err := gvrFor("nodes")
	if err != nil {
		return nil, err
	}
	nodeObjs, err := listObjects(ctx, nodeGVR, "", "")
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]map[string]string, len(nodeObjs))
	for _, n := range nodeObjs {
		nodes[n.GetName()] = n.GetLabels()
	}
	var (
		found    []poolWorkers
		refusals []string
	)
	for _, pool := range pools {
		selector, _, _ := unstructured.NestedString(pool.Object, "status", "selector")
		if selector == "" {
			selector = substrateWorkerLabel + "=" + pool.GetName()
		}
		podObjs, err := listObjects(ctx, gvrPods, pool.GetNamespace(), selector)
		if err != nil {
			return nil, err
		}
		pods := make([]corev1.Pod, len(podObjs))
		for i := range podObjs {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(podObjs[i].Object, &pods[i]); err != nil {
				return nil, fmt.Errorf("reading pod %s/%s: %w", podObjs[i].GetNamespace(), podObjs[i].GetName(), err)
			}
		}
		w, refusal := judgeWorkerPool(&pool, pods, nodes)
		found = append(found, w)
		if refusal != "" {
			refusals = append(refusals, refusal)
		}
	}
	if len(refusals) > 0 {
		return found, workerPoolRefusal{msg: strings.Join(refusals, "\n") + "\n" +
			"No agent turn can run until they do: the actors boot inside these workers. " +
			"`kubectl -n " + kagentNamespace + " describe pod <worker>` has the scheduler's events; a node selector or resource request " +
			"the lab's values set comes from kagent.substrateWorkerPool in agentlab.yaml's platform.valuesFiles, then `agentlab platform`"}
	}
	return found, nil
}

// judgeWorkerPool is the verdict on one pool from its workers and the
// cluster's node labels: the workers found, and the refusal ("" when the
// pool is healthy) naming every worker that is not Running and Ready, why,
// and every worker on a node without the pool's node selector.
func judgeWorkerPool(pool *unstructured.Unstructured, pods []corev1.Pod, nodes map[string]map[string]string) (poolWorkers, string) {
	name := pool.GetNamespace() + "/" + pool.GetName()
	replicas, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicas")
	selector, _, _ := unstructured.NestedStringMap(pool.Object, "spec", "template", "nodeSelector")
	w := poolWorkers{pool: name, replicas: replicas, selector: selector}

	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	var problems []string
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue // a worker on its way out, replaced by the pool
		}
		if reason := workerNotRunning(p); reason != "" {
			problems = append(problems, fmt.Sprintf("worker %s is %s", p.Name, reason))
			continue
		}
		if missing := missingLabels(nodes[p.Spec.NodeName], selector); len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("worker %s runs on node %s, which does not carry the pool's node selector %s", p.Name, p.Spec.NodeName, selectorString(missing)))
			continue
		}
		w.running++
		if !slices.Contains(w.nodes, p.Spec.NodeName) {
			w.nodes = append(w.nodes, p.Spec.NodeName)
		}
	}
	slices.Sort(w.nodes)
	if int64(w.running) < replicas && len(problems) == 0 {
		problems = append(problems, fmt.Sprintf("only %d of its %d workers exist", w.running, replicas))
	}
	if len(problems) == 0 {
		return w, ""
	}
	return w, fmt.Sprintf("WorkerPool %s has %d of its %d workers Running: %s", name, w.running, replicas, strings.Join(problems, "; "))
}

// workerNotRunning words why a worker is not Running and Ready ("" when it
// is): the scheduler's reason and message for one it could not place
// ("Pending (Unschedulable: 0/1 nodes are available: …)"), else kubectl's
// STATUS with the waiting container's message.
func workerNotRunning(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return fmt.Sprintf("%s (%s: %s)", p.Status.Phase, c.Reason, c.Message)
		}
	}
	if p.Status.Phase != corev1.PodRunning {
		return podStateSummary(p)
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return ""
		}
	}
	return "Running but not Ready (" + podStatus(p) + ")"
}

// missingLabels is the part of selector the labels do not carry.
func missingLabels(labels, selector map[string]string) map[string]string {
	missing := map[string]string{}
	for k, v := range selector {
		if labels[k] != v {
			missing[k] = v
		}
	}
	return missing
}

// selectorString words a node selector as k=v pairs, sorted.
func selectorString(selector map[string]string) string {
	pairs := make([]string, 0, len(selector))
	for _, k := range slices.Sorted(maps.Keys(selector)) {
		pairs = append(pairs, k+"="+selector[k])
	}
	return strings.Join(pairs, ",")
}
