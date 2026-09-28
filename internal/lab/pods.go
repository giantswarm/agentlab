package lab

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/giantswarm/agentlab/internal/config"
)

// Pods lists the lab's pods — every namespace, or only namespace — through the
// embedded client, in the shape of `kubectl get pods -A`: the look at the lab
// that needs no kubectl. A lab that is not running is refused with the fact
// and `agentlab up`, not a stack of client-go errors.
func Pods(cfg *config.Config, namespace string) error {
	if err := requireRunningCluster(cfg); err != nil {
		return err
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return printPods(ctx, os.Stdout, namespace, time.Now())
}

// printPods writes the table, sorted by namespace then name.
func printPods(ctx context.Context, w io.Writer, namespace string, now time.Time) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	list, err := k.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing the lab's pods: %w", err)
	}
	pods := list.Items
	var b strings.Builder
	switch {
	case len(pods) == 0 && namespace != "":
		fmt.Fprintf(&b, "No pods in namespace %s.\n", namespace)
	case len(pods) == 0:
		b.WriteString("No pods.\n")
	default:
		slices.SortFunc(pods, func(a, b corev1.Pod) int {
			if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
				return c
			}
			return strings.Compare(a.Name, b.Name)
		})
		// A tabwriter over a strings.Builder buffers: the writes cannot
		// fail, and Flush reports what could.
		tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAMESPACE\tNAME\tREADY\tSTATUS\tRESTARTS\tAGE")
		for i := range pods {
			p := &pods[i]
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
				p.Namespace, p.Name, podReadyColumn(p), podStatus(p), podRestarts(p), podAge(p, now))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// podReadyColumn is kubectl's READY column: ready containers of all containers.
func podReadyColumn(p *corev1.Pod) string {
	ready := 0
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
}

// podRestarts sums the restarts of the pod's containers, init containers
// included (a restartable sidecar restarts too).
func podRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, c := range p.Status.InitContainerStatuses {
		n += c.RestartCount
	}
	for _, c := range p.Status.ContainerStatuses {
		n += c.RestartCount
	}
	return n
}

// podAge is kubectl's AGE column.
func podAge(p *corev1.Pod, now time.Time) string {
	if p.CreationTimestamp.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(now.Sub(p.CreationTimestamp.Time))
}

// podStatus is kubectl's STATUS verdict: the pod's reason or phase, an init
// container's progress or failure (Init:1/2, Init:CrashLoopBackOff), a
// container's waiting or terminated reason (ImagePullBackOff, Completed,
// OOMKilled), Terminating once the pod is being deleted.
func podStatus(p *corev1.Pod) string {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}

	initializing := false
	for i, c := range p.Status.InitContainerStatuses {
		switch {
		case c.State.Terminated != nil && c.State.Terminated.ExitCode == 0:
			continue
		case c.State.Running != nil && isRestartableInit(p, c.Name):
			// A sidecar (restartPolicy: Always) runs for the pod's life.
			continue
		case c.State.Terminated != nil:
			reason = "Init:" + terminatedReason(c.State.Terminated)
		case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + c.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}

	if !initializing {
		running := false
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			c := p.Status.ContainerStatuses[i]
			switch {
			case c.State.Waiting != nil && c.State.Waiting.Reason != "":
				reason = c.State.Waiting.Reason
			case c.State.Terminated != nil:
				reason = terminatedReason(c.State.Terminated)
			case c.Ready && c.State.Running != nil:
				running = true
			}
		}
		// One container done, another still running: the pod runs.
		if reason == "Completed" && running {
			reason = string(corev1.PodRunning)
		}
	}

	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	return reason
}

// terminatedReason names how a container ended: its reason, else the signal
// or exit code.
func terminatedReason(t *corev1.ContainerStateTerminated) string {
	switch {
	case t.Reason != "":
		return t.Reason
	case t.Signal != 0:
		return fmt.Sprintf("Signal:%d", t.Signal)
	default:
		return fmt.Sprintf("ExitCode:%d", t.ExitCode)
	}
}

// isRestartableInit reports whether the named init container is a sidecar.
func isRestartableInit(p *corev1.Pod, name string) bool {
	for _, c := range p.Spec.InitContainers {
		if c.Name == name {
			return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		}
	}
	return false
}
