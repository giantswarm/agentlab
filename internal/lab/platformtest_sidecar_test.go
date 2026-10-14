package lab

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// platform-test reads the sidecar rule off the live Deployments: one told the
// lab Dex address through a name it dials it by is selected, keyed by that
// name; one that carries the address as an identity only — the authorization
// server it names in its metadata (OAUTH_AUTHORIZATION_SERVER) — passes
// without a sidecar; the annotation decides where it is set; a variable read
// from a Secret has no value to match.
func TestDeploymentDexLocalhostKey(t *testing.T) {
	const addr = "localhost:32000"
	const dexIssuer = "https://localhost:32000/dex"
	const on, off, unreadable = "true", "false", "nope"
	deployment := func(name string, annotations map[string]string, spec corev1.PodSpec) *appsv1.Deployment {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: platformNamespace, Name: name, Annotations: annotations}}
		d.Spec.Template.Spec = spec
		return d
	}
	env := func(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }
	for _, tc := range []struct {
		name string
		d    *appsv1.Deployment
		key  string
		ok   bool
	}{
		{"mcp-oauth resource server", deployment("mcp-kubernetes", nil, corev1.PodSpec{Containers: []corev1.Container{{
			Env: []corev1.EnvVar{env(dexIssuerURLVar, dexIssuer), env("DEX_CLIENT_ID", "agent-platform")},
		}}}), dexIssuerURLVar, true},
		{"manager flag", deployment("cluster-manager", nil, corev1.PodSpec{Containers: []corev1.Container{{
			Args: []string{"--enable-oauth=true", dexIssuerURLFlag + "=" + dexIssuer},
		}}}), dexIssuerURLFlag, true},
		{"oauth2-proxy", deployment("kagent-oauth2-proxy", nil, corev1.PodSpec{Containers: []corev1.Container{{
			Args: []string{"--oidc-issuer-url=$(OIDC_ISSUER_URL)"},
			Env:  []corev1.EnvVar{env(oidcIssuerURLVar, dexIssuer)},
		}}}), oidcIssuerURLVar, true},
		{"authorization-server identity only", deployment("repo-manager", nil, corev1.PodSpec{Containers: []corev1.Container{{
			Env: []corev1.EnvVar{env("OAUTH_ENABLED", "true"), env("OAUTH_AUTHORIZATION_SERVER", "https://localhost:32000/dex/apps/repo-manager")},
		}}}), "", false},
		{"another Dex", deployment("elsewhere", nil, corev1.PodSpec{Containers: []corev1.Container{{
			Env: []corev1.EnvVar{env(dexIssuerURLVar, "https://dex.example.test/dex")},
		}}}), "", false},
		{"host network", deployment("muster", nil, corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{
			Args: []string{dexIssuerURLFlag + "=" + dexIssuer},
		}}}), "", false},
		{"value from a Secret", deployment("secret-issuer", nil, corev1.PodSpec{Containers: []corev1.Container{{
			Env: []corev1.EnvVar{{Name: dexIssuerURLVar, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "issuer"}}}},
		}}}), "", false},
		{"opted out", deployment("opted-out", map[string]string{dexLocalhostAnnotation: off}, corev1.PodSpec{Containers: []corev1.Container{{
			Env: []corev1.EnvVar{env(dexIssuerURLVar, dexIssuer)},
		}}}), "", false},
		{"opted in", deployment("opted-in", map[string]string{dexLocalhostAnnotation: on}, corev1.PodSpec{Containers: []corev1.Container{{
			Env: []corev1.EnvVar{env("ISSUER", dexIssuer)},
		}}}), dexLocalhostAnnotation + "=" + on, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, ok, err := deploymentDexLocalhostKey(tc.d, addr)
			if err != nil {
				t.Fatal(err)
			}
			if key != tc.key || ok != tc.ok {
				t.Errorf("deploymentDexLocalhostKey = %q, %v, want %q, %v", key, ok, tc.key, tc.ok)
			}
		})
	}
	// The pod template's annotation counts too, and one the rule cannot read
	// is the error, naming the Deployment.
	d := deployment("template-annotated", nil, corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{env(dexIssuerURLVar, dexIssuer)}}}})
	d.Spec.Template.Annotations = map[string]string{dexLocalhostAnnotation: off}
	if key, ok, err := deploymentDexLocalhostKey(d, addr); err != nil || ok {
		t.Errorf("pod template opted out: got %q, %v, %v", key, ok, err)
	}
	d.Spec.Template.Annotations[dexLocalhostAnnotation] = unreadable
	if _, _, err := deploymentDexLocalhostKey(d, addr); err == nil || !strings.Contains(err.Error(), platformNamespace+"/template-annotated") || !strings.Contains(err.Error(), strconv.Quote(unreadable)) {
		t.Errorf("unreadable annotation: err = %v", err)
	}
}

// dexIssuerURLFlag is the managers' issuer flag as the tests spell it.
const dexIssuerURLFlag = "--dex-issuer-url"

// flagVariable spells a flag as the variable it stands in for.
func TestFlagVariable(t *testing.T) {
	for in, want := range map[string]string{dexIssuerURLFlag: dexIssuerURLVar, "-oidc-issuer-url": oidcIssuerURLVar, dexIssuerURLVar: dexIssuerURLVar, "": ""} {
		if got := flagVariable(in); got != want {
			t.Errorf("flagVariable(%q) = %q, want %q", in, got, want)
		}
	}
}

// platform-test dates a restart against the component's release: one before
// it (a node restart during the bring-up) is history, one after it fails
// until the container has run through kubelet's crash-loop reset — unless
// the container restarted with its node.
func TestRestartedSinceRelease(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	released := now.Add(-30 * time.Minute)
	nodeStarted := now.Add(-3 * time.Minute)
	running := func(restarts int32, startedAgo time.Duration) corev1.ContainerStatus {
		return corev1.ContainerStatus{RestartCount: restarts, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-startedAgo))}}}
	}
	lastEnded := func(c corev1.ContainerStatus, reason string, endedAgo time.Duration) corev1.ContainerStatus {
		c.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: reason, ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(-endedAgo))}
		return c
	}
	for _, tc := range []struct {
		name        string
		c           corev1.ContainerStatus
		released    time.Time
		nodeStarted time.Time
		want        bool
	}{
		{"never restarted", running(0, time.Minute), released, time.Time{}, false},
		{"restart before the release", running(3, 40*time.Minute), released, time.Time{}, false},
		{"restart after the release", running(1, 2*time.Minute), released, time.Time{}, true},
		{"restart after the release, settled since", running(1, 20*time.Minute), released, time.Time{}, false},
		{"no HelmRelease, fresh restart", running(2, time.Minute), time.Time{}, time.Time{}, true},
		{"not running", corev1.ContainerStatus{RestartCount: 4}, released, time.Time{}, false},
		{"node restart: the last run ended before the node started", lastEnded(running(1, 2*time.Minute), "Error", 10*time.Minute), released, nodeStarted, false},
		{"node restart: kubelet lost the last run with its sandbox", lastEnded(running(1, 2*time.Minute), terminatedUnknown, time.Minute), released, time.Time{}, false},
		{"crash loop after the node restart", lastEnded(running(3, time.Minute), "Error", 90*time.Second), released, nodeStarted, true},
		{"crash loop, node start unknown", lastEnded(running(3, time.Minute), "Error", 90*time.Second), released, time.Time{}, true},
		{"crash loop on a node that did not restart", lastEnded(running(2, time.Minute), "Error", 90*time.Second), released, now.Add(-2 * time.Hour), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartedSinceRelease(tc.c, tc.released, tc.nodeStarted, now); got != tc.want {
				t.Errorf("restartedSinceRelease = %v, want %v", got, tc.want)
			}
		})
	}
}

// After a kind node restart every pod's containers restarted once together:
// platform-test passes on that, and still fails a server that crash-loops
// since — the case of the issue, over whole pods.
func TestClassifyRestartsNodeRestart(t *testing.T) {
	now := time.Date(2026, 10, 14, 21, 0, 0, 0, time.UTC)
	released := now.Add(-2 * time.Hour)
	const node = "agentlab-control-plane"
	nodeDown, nodeStarted := now.Add(-5*time.Minute), now.Add(-4*time.Minute)
	nodes := nodeStarts{node: nodeStarted}
	container := func(name string, restarts int32, startedAt, lastEnded time.Time, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name: name, Ready: true, RestartCount: restarts,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(startedAt)}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: 255, FinishedAt: metav1.NewTime(lastEnded)}},
		}
	}
	pod := func(name string, sidecar, server corev1.ContainerStatus) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: platformNamespace, Name: name}}
		p.Spec.NodeName = node
		p.Status.Phase = corev1.PodRunning
		sidecar.Name = dexLocalhostContainer
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{sidecar, {Name: "chart-init", RestartCount: 1}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{server}
		return p
	}
	booted := now.Add(-3 * time.Minute)
	nodeRestarted := []corev1.Pod{
		pod("mcp-kubernetes-1", container("", 1, booted, nodeDown, "Unknown"), container("server", 1, booted, nodeDown, "Unknown")),
		pod("cluster-manager-1", container("", 1, booted, nodeDown, "Completed"), container("server", 1, booted, nodeDown, "Error")),
	}
	crashLoops, history := classifyRestarts(nodeRestarted, released, nodes, now)
	if len(crashLoops) != 0 {
		t.Errorf("node restart: crash loops %v, want none", crashLoops)
	}
	if len(history) != 4 {
		t.Errorf("node restart: history %v, want the 4 running containers", history)
	}

	// The server crashed twice more after the node came back.
	crashed := append(slices.Clone(nodeRestarted),
		pod("agent-manager-1", container("", 1, booted, nodeDown, "Unknown"), container("server", 3, now.Add(-30*time.Second), now.Add(-45*time.Second), "Error")))
	crashLoops, _ = classifyRestarts(crashed, released, nodes, now)
	if len(crashLoops) != 1 || !strings.Contains(crashLoops[0], "agent-manager-1 server ×3") {
		t.Errorf("crash loop after the node restart: crash loops %v, want agent-manager-1's server", crashLoops)
	}
}

// The release time is the HelmRelease's newest revision.
func TestHelmReleaseLastDeployed(t *testing.T) {
	hr := &unstructured.Unstructured{Object: map[string]any{crStatus: map[string]any{"history": []any{
		map[string]any{"lastDeployed": "2026-10-08T11:30:00Z"},
		map[string]any{"lastDeployed": "2026-10-07T09:00:00Z"},
	}}}}
	hr.SetNamespace("flux-system")
	hr.SetName("mcp-kubernetes")
	got, err := helmReleaseLastDeployed(hr)
	if err != nil || !got.Equal(time.Date(2026, 10, 8, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("helmReleaseLastDeployed = %v, %v", got, err)
	}
	unstructured.RemoveNestedField(hr.Object, crStatus)
	if _, err := helmReleaseLastDeployed(hr); err == nil || !strings.Contains(err.Error(), "flux-system/mcp-kubernetes") {
		t.Errorf("no history: err = %v", err)
	}
}
