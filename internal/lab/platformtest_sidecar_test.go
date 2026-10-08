package lab

import (
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
// until the container has run through kubelet's crash-loop reset.
func TestRestartedSinceRelease(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	released := now.Add(-30 * time.Minute)
	running := func(restarts int32, startedAgo time.Duration) corev1.ContainerStatus {
		return corev1.ContainerStatus{RestartCount: restarts, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-startedAgo))}}}
	}
	for _, tc := range []struct {
		name     string
		c        corev1.ContainerStatus
		released time.Time
		want     bool
	}{
		{"never restarted", running(0, time.Minute), released, false},
		{"restart before the release", running(3, 40*time.Minute), released, false},
		{"restart after the release", running(1, 2*time.Minute), released, true},
		{"restart after the release, settled since", running(1, 20*time.Minute), released, false},
		{"no HelmRelease, fresh restart", running(2, time.Minute), time.Time{}, true},
		{"not running", corev1.ContainerStatus{RestartCount: 4}, released, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartedSinceRelease(tc.c, tc.released, now); got != tc.want {
				t.Errorf("restartedSinceRelease = %v, want %v", got, tc.want)
			}
		})
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
