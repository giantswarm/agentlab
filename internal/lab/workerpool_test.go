package lab

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// The fakes' node and the pool's node selector.
const (
	testWorkerNode = "agentlab-control-plane"
	testPoolArch   = "amd64"
	testWorker     = "gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.3.0"
)

// pinnedPool is the fakes' WorkerPool with replicas and a node selector.
func pinnedPool(replicas int64, selector map[string]string) *unstructured.Unstructured {
	pool := workerPool(testWorkerPool, testWorker)
	_ = unstructured.SetNestedField(pool.Object, replicas, fieldSpec, "replicas")
	if selector != nil {
		_ = unstructured.SetNestedStringMap(pool.Object, selector, fieldSpec, "template", "nodeSelector")
	}
	return pool
}

// workerPod is one of the pool's workers, Running and Ready on node.
func workerPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: kagentNamespace, Name: name, Labels: map[string]string{substrateWorkerLabel: testWorkerPool}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "worker"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}, {Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{running(true, 0)},
		},
	}
}

// unschedulable is a worker the scheduler could not place.
func unschedulable(name string) *corev1.Pod {
	p := workerPod(name, "")
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector.",
	}}}
	return p
}

// A pool whose workers all run where its node selector says passes, with
// the workers and their node reported. Images that agree are not enough: a
// worker the scheduler cannot place fails the proof naming the worker and
// the scheduler's reason, as does a worker that is not Ready, one on a node
// without the selector, and a pool short of its replicas.
func TestProveWorkerPoolsRunning(t *testing.T) {
	ctx := context.Background()
	pin := map[string]string{workerPoolArchLabel: testPoolArch}
	node := archNode(testWorkerNode, testPoolArch)

	newFakeLab(t, node, pinnedPool(2, pin), workerPod("w-a", testWorkerNode), workerPod("w-b", testWorkerNode))
	pools, err := proveWorkerPoolsRunning(ctx, 0)
	if err != nil {
		t.Fatalf("a healthy pool: %v", err)
	}
	if len(pools) != 1 || pools[0].running != 2 || strings.Join(pools[0].nodes, ",") != testWorkerNode {
		t.Fatalf("pools = %+v, want 2 workers on %s", pools, testWorkerNode)
	}
	if got := pools[0].String(); !strings.Contains(got, "2/2 workers Running on "+testWorkerNode) || !strings.Contains(got, workerPoolArchLabel+"="+testPoolArch) {
		t.Errorf("the report names the workers, the node and the pin, got %q", got)
	}

	notReady := workerPod("w-b", testWorkerNode)
	notReady.Status.Conditions[1].Status = corev1.ConditionFalse
	waiting := workerPod("w-b", testWorkerNode)
	waiting.Status.Phase = corev1.PodPending
	waiting.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull refused"}}}}
	for _, tc := range []struct {
		name string
		pool *unstructured.Unstructured
		pods []*corev1.Pod
		want []string
	}{
		{
			name: "a worker the scheduler cannot place",
			pool: pinnedPool(2, map[string]string{"agentlab.giantswarm.io/impossible": "true"}),
			pods: []*corev1.Pod{workerPod("w-a", testWorkerNode), unschedulable("w-b")},
			want: []string{"WorkerPool " + kagentNamespace + "/" + testWorkerPool, "worker w-b is Pending (Unschedulable: 0/1 nodes are available", "kubectl -n " + kagentNamespace + " describe pod"},
		},
		{
			name: "a worker on a node without the pool's selector",
			pool: pinnedPool(1, map[string]string{workerPoolArchLabel: "arm64"}),
			pods: []*corev1.Pod{workerPod("w-a", testWorkerNode)},
			want: []string{"worker w-a runs on node " + testWorkerNode + ", which does not carry the pool's node selector " + workerPoolArchLabel + "=arm64"},
		},
		{
			name: "a worker Running but not Ready",
			pool: pinnedPool(2, pin),
			pods: []*corev1.Pod{workerPod("w-a", testWorkerNode), notReady},
			want: []string{"worker w-b is Running but not Ready"},
		},
		{
			name: "a worker whose container waits",
			pool: pinnedPool(2, pin),
			pods: []*corev1.Pod{workerPod("w-a", testWorkerNode), waiting},
			want: []string{"worker w-b is Pending: ImagePullBackOff: pull refused"},
		},
		{
			name: "fewer workers than replicas",
			pool: pinnedPool(4, pin),
			pods: []*corev1.Pod{workerPod("w-a", testWorkerNode)},
			want: []string{"has 1 of its 4 workers Running: only 1 of its 4 workers exist"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{node, tc.pool}
			for _, p := range tc.pods {
				objs = append(objs, p)
			}
			newFakeLab(t, objs...)
			_, err := proveWorkerPoolsRunning(ctx, 0)
			if err == nil {
				t.Fatal("want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal lacks %q:\n%v", want, err)
				}
			}
		})
	}

	// A worker on its way out is the pool's to replace, not a failure.
	leaving := unschedulable("w-old")
	leaving.DeletionTimestamp = &metav1.Time{}
	nodes := map[string]map[string]string{testWorkerNode: node.Labels}
	if _, refusal := judgeWorkerPool(pinnedPool(1, pin), []corev1.Pod{*workerPod("w-a", testWorkerNode), *leaving}, nodes); refusal != "" {
		t.Errorf("a terminating worker: %s", refusal)
	}

	newFakeLab(t, node)
	if _, err := proveWorkerPoolsRunning(ctx, 0); err == nil || !strings.Contains(err.Error(), "no WorkerPool in "+kagentNamespace) {
		t.Errorf("no WorkerPool: want the refusal, got %v", err)
	}
}
