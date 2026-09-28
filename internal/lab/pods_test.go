package lab

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The columns several cases share.
const (
	notReady        = "0/1"
	aMinute         = "60s"
	anHour          = "60m"
	reasonCompleted = "Completed"
)

// podsNow is the fixed clock the AGE column is computed against.
var podsNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func testPod(ns, name string, age time.Duration, containers int) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(podsNow.Add(-age)),
	}}
	for i := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "c" + string(rune('0'+i))})
	}
	p.Status.Phase = corev1.PodRunning
	return p
}

func running(ready bool, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Ready: ready, RestartCount: restarts,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func TestPodColumns(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	for _, tc := range []struct {
		name               string
		pod                func() *corev1.Pod
		ready, status, age string
		restarts           int32
	}{
		{
			name: "all containers ready",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", 90*time.Minute, 2)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{running(true, 1), running(true, 2)}
				return p
			},
			ready: "2/2", status: string(corev1.PodRunning), restarts: 3, age: "90m",
		},
		{
			name: "a waiting reason",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", 3*time.Second, 1)
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
				return p
			},
			ready: notReady, status: "ImagePullBackOff", age: "3s",
		},
		{
			name: "pending without a container status",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Minute, 1)
				p.Status.Phase = corev1.PodPending
				return p
			},
			ready: notReady, status: string(corev1.PodPending), age: aMinute,
		},
		{
			name: "completed",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", 49*time.Hour, 1)
				p.Status.Phase = corev1.PodSucceeded
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: reasonCompleted}}}}
				return p
			},
			ready: notReady, status: reasonCompleted, age: "2d1h",
		},
		{
			name: "terminated without a reason",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Hour, 1)
				p.Status.Phase = corev1.PodFailed
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}}}
				return p
			},
			ready: notReady, status: "ExitCode:2", age: anHour,
		},
		{
			name: "one done, one running",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Hour, 2)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{
					{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reasonCompleted}}},
					running(true, 0),
				}
				return p
			},
			ready: "1/2", status: string(corev1.PodRunning), age: anHour,
		},
		{
			name: "terminating",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Hour, 1)
				now := metav1.NewTime(podsNow)
				p.DeletionTimestamp = &now
				p.Status.ContainerStatuses = []corev1.ContainerStatus{running(true, 0)}
				return p
			},
			ready: "1/1", status: "Terminating", age: anHour,
		},
		{
			name: "an init container still running",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Minute, 1)
				p.Status.Phase = corev1.PodPending
				p.Spec.InitContainers = []corev1.Container{{Name: "i0"}, {Name: "i1"}}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{
					{Name: "i0", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reasonCompleted}}},
					{Name: "i1", RestartCount: 4, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				}
				return p
			},
			ready: notReady, status: "Init:1/2", restarts: 4, age: aMinute,
		},
		{
			name: "an init container crash-looping",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Minute, 1)
				p.Status.Phase = corev1.PodPending
				p.Spec.InitContainers = []corev1.Container{{Name: "i0"}}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "i0", State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
				return p
			},
			ready: notReady, status: "Init:CrashLoopBackOff", age: aMinute,
		},
		{
			name: "a running sidecar does not hold the pod in Init",
			pod: func() *corev1.Pod {
				p := testPod("a", "p", time.Minute, 1)
				p.Spec.InitContainers = []corev1.Container{{Name: testSidecarContainer, RestartPolicy: &always}}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: testSidecarContainer,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
				p.Status.ContainerStatuses = []corev1.ContainerStatus{running(true, 0)}
				return p
			},
			ready: "1/1", status: string(corev1.PodRunning), age: aMinute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pod()
			if got := podReadyColumn(p); got != tc.ready {
				t.Errorf("READY = %q, want %q", got, tc.ready)
			}
			if got := podStatus(p); got != tc.status {
				t.Errorf("STATUS = %q, want %q", got, tc.status)
			}
			if got := podRestarts(p); got != tc.restarts {
				t.Errorf("RESTARTS = %d, want %d", got, tc.restarts)
			}
			if got := podAge(p, podsNow); got != tc.age {
				t.Errorf("AGE = %q, want %q", got, tc.age)
			}
		})
	}
}

// TestPrintPods lists through the typed client: every namespace sorted, or
// one namespace with -n.
func TestPrintPods(t *testing.T) {
	f := newFakeLab(t)
	ctx := context.Background()
	for _, p := range []*corev1.Pod{
		testPod("muster", "muster-0", time.Hour, 1),
		testPod("agent-platform", "zeta", time.Hour, 1),
		testPod("agent-platform", "alpha", time.Hour, 1),
	} {
		if _, err := f.cs.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	var all bytes.Buffer
	if err := printPods(ctx, &all, "", podsNow); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(all.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "NAMESPACE") {
		t.Fatalf("printPods = %q, want a header and three rows", all.String())
	}
	for i, want := range []string{"agent-platform   alpha", "agent-platform   zeta", "muster           muster-0"} {
		if !strings.HasPrefix(lines[i+1], want) {
			t.Errorf("row %d = %q, want it to start with %q", i+1, lines[i+1], want)
		}
	}

	var one bytes.Buffer
	if err := printPods(ctx, &one, "muster", podsNow); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(one.String(), "\n"); got != 2 || !strings.Contains(one.String(), "muster-0") {
		t.Errorf("printPods(muster) = %q, want the header and muster-0", one.String())
	}

	var none bytes.Buffer
	if err := printPods(ctx, &none, "empty", podsNow); err != nil {
		t.Fatal(err)
	}
	if got := none.String(); got != "No pods in namespace empty.\n" {
		t.Errorf("printPods(empty) = %q", got)
	}
}
