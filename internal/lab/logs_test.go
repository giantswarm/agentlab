package lab

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestLogTargets: every component's target resolves to pods through the
// embedded client — the label selectors and the deploy/ targets alike — and
// the table is what cobra completes.
func TestLogTargets(t *testing.T) {
	f := newFakeLab(t)
	ctx := context.Background()
	for component, lt := range logTargets {
		labels := map[string]string{}
		if name, ok := strings.CutPrefix(lt.target, "deploy/"); ok {
			labels[appLabel] = name
			err := f.cs.Tracker().Add(&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lt.namespace},
				Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
			})
			if err != nil {
				t.Fatal(err)
			}
		} else {
			k, v, _ := strings.Cut(lt.target, "=")
			labels[k] = v
		}
		err := f.cs.Tracker().Add(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: component + "-0", Namespace: lt.namespace, Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: component}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		if err := streamLogs(ctx, lt.namespace, lt.target, &b); err != nil {
			t.Errorf("%s (%s in %s): %v", component, lt.target, lt.namespace, err)
			continue
		}
		if !strings.Contains(b.String(), fakeLogLine) {
			t.Errorf("%s: streamed %q", component, b.String())
		}
	}
	if got, want := LogComponents(), []string{componentBackstage, componentDex, mcpPrometheusRelease, componentMuster, componentPrometheus}; !slices.Equal(got, want) {
		t.Errorf("LogComponents = %v, want %v", got, want)
	}
}
