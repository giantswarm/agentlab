package lab

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// patchCount counts the patches the dynamic fake saw on one resource — the
// restarts of a Deployment, the JSON patch of a CRD.
func (f *fakeLab) patchCount(gvr schema.GroupVersionResource) int {
	n := 0
	for _, a := range f.dyn.Actions() {
		if p, ok := a.(clienttesting.PatchActionImpl); ok && p.GetResource() == gvr {
			n++
		}
	}
	return n
}

// TestApplyRestartingOnChange: the first apply creates the ConfigMap and
// rolls the Deployment that reads it, a re-apply of the same render changes
// nothing and leaves the Deployment alone, a changed render rolls it again,
// and without the Deployment (a fresh install) the apply lands and nothing
// fails.
func TestApplyRestartingOnChange(t *testing.T) {
	f := newFakeLab(t, &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{Name: corednsDeployment, Namespace: kubeSystemNamespace},
	})
	ctx := context.Background()
	corefile := func(rewrite string) []byte {
		return []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: coredns\n  namespace: kube-system\ndata:\n  Corefile: " + rewrite + "\n")
	}
	restarted, err := applyRestartingOnChange(ctx, corefile("rewrite a"), kubeSystemNamespace, corednsDeployment)
	if err != nil || !restarted {
		t.Fatalf("first apply: restarted=%v err=%v, want a restart", restarted, err)
	}
	stamp, _, _ := unstructured.NestedString(f.stored(t, gvrDeployments, kubeSystemNamespace, corednsDeployment).Object,
		"spec", "template", "metadata", "annotations", restartedAtAnnotation)
	if stamp == "" {
		t.Error("the Deployment carries no restartedAt stamp after the first apply")
	}
	restarted, err = applyRestartingOnChange(ctx, corefile("rewrite a"), kubeSystemNamespace, corednsDeployment)
	if err != nil || restarted {
		t.Errorf("unchanged re-apply: restarted=%v err=%v, want no restart", restarted, err)
	}
	if n := f.patchCount(gvrDeployments); n != 1 {
		t.Errorf("Deployment patched %d times after an unchanged re-apply, want 1", n)
	}
	restarted, err = applyRestartingOnChange(ctx, corefile("rewrite b"), kubeSystemNamespace, corednsDeployment)
	if err != nil || !restarted {
		t.Errorf("changed render: restarted=%v err=%v, want a restart", restarted, err)
	}
	if n := f.patchCount(gvrDeployments); n != 2 {
		t.Errorf("Deployment patched %d times after a changed render, want 2", n)
	}

	f2 := newFakeLab(t)
	restarted, err = applyRestartingOnChange(ctx, corefile("rewrite a"), kubeSystemNamespace, corednsDeployment)
	if err != nil || restarted {
		t.Errorf("without the Deployment: restarted=%v err=%v, want neither", restarted, err)
	}
	if data, _, _ := unstructured.NestedString(f2.stored(t, gvrConfigMaps, kubeSystemNamespace, corednsDeployment).Object, "data", "Corefile"); data != "rewrite a" {
		t.Errorf("the ConfigMap was not applied without the Deployment: %q", data)
	}
}

// TestEnsurePlatformSecrets: a first run creates every key with the fixed Dex
// client secret, a second run leaves the generated values alone, and an
// older lab's Secret without the Backstage session key gets that key merged
// in and nothing else touched.
func TestEnsurePlatformSecrets(t *testing.T) {
	f := newFakeLab(t)
	ctx := context.Background()
	if err := ensurePlatformSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedMap(f.stored(t, gvrSecrets, platformNamespace, platformSecretsName).Object, "data")
	for _, key := range []string{"dex-client-secret", "registration-token", "oauth-encryption-key", "valkey-password", backstageSessionSecretKey} {
		if v, _ := data[key].(string); v == "" {
			t.Errorf("the created Secret lacks %s: %v", key, data)
		}
	}
	if got, want := data["dex-client-secret"], base64.StdEncoding.EncodeToString([]byte(config.AgentPlatformClientSecret)); got != want {
		t.Errorf("dex-client-secret = %v, want the Dex static client's %v", got, want)
	}
	if err := ensurePlatformSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := unstructured.NestedMap(f.stored(t, gvrSecrets, platformNamespace, platformSecretsName).Object, "data"); !reflect.DeepEqual(again, data) {
		t.Errorf("a second run regenerated the secrets: %v -> %v", data, again)
	}

	f2 := newFakeLab(t, &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: platformSecretsName, Namespace: platformNamespace},
		Data:       map[string][]byte{"dex-client-secret": []byte("old")},
	})
	if err := ensurePlatformSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	old := f2.stored(t, gvrSecrets, platformNamespace, platformSecretsName)
	if v, _, _ := unstructured.NestedString(old.Object, "stringData", backstageSessionSecretKey); v == "" {
		t.Errorf("the session key was not merged into the older Secret: %v", old.Object)
	}
	if v, _, _ := unstructured.NestedString(old.Object, "data", "dex-client-secret"); v != base64.StdEncoding.EncodeToString([]byte("old")) {
		t.Errorf("the older Secret's data was touched: %v", old.Object)
	}
}

// fakeHelmRelease is a Flux HelmRelease in the platform namespace as
// helm-controller leaves it: with a Ready condition, or none before the
// first reconcile (ready "").
func fakeHelmRelease(name, ready, message string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		fieldAPIVersion: fluxHelmReleaseGVK.GroupVersion().String(),
		fieldKind:       fluxHelmReleaseGVK.Kind,
		fieldMetadata:   map[string]any{nameKey: name, fieldNamespace: platformNamespace},
	}}
	if ready != "" {
		u.Object[fieldStatus] = map[string]any{"conditions": []any{
			map[string]any{fieldType: "Reconciling", fieldStatus: "Unknown"},
			map[string]any{fieldType: condReady, fieldStatus: ready, fieldMessage: message},
		}}
	}
	return u
}

// TestPlatformReleases lists the HelmReleases with their Ready condition: the
// status and helm-controller's message, "" for a release not yet reconciled.
func TestPlatformReleases(t *testing.T) {
	newFakeLab(t,
		fakeHelmRelease(componentMuster, conditionTrue, "Helm install succeeded"),
		fakeHelmRelease(componentKagent, condFalse, retriesExhausted),
		fakeHelmRelease("fresh", "", ""))
	releases, err := platformReleases()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]platformReleaseStatus{}
	for _, r := range releases {
		got[r.name] = r
	}
	want := map[string]platformReleaseStatus{
		componentMuster: {name: componentMuster, ready: conditionTrue, message: "Helm install succeeded"},
		componentKagent: {name: componentKagent, ready: condFalse, message: retriesExhausted},
		"fresh":         {name: "fresh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("platformReleases = %v, want %v", got, want)
	}
}

// TestMCPServerState reads status.state off muster's MCPServer: the state,
// "" before the first reconcile, the apiserver's NotFound for a missing CR.
func TestMCPServerState(t *testing.T) {
	server := func(name, state string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			fieldAPIVersion: musterMCPServerGVK.GroupVersion().String(),
			fieldKind:       musterMCPServerGVK.Kind,
			fieldMetadata:   map[string]any{nameKey: name, fieldNamespace: platformNamespace},
		}}
		if state != "" {
			u.Object[fieldStatus] = map[string]any{"state": state}
		}
		return u
	}
	newFakeLab(t, server("k8s", "Connected"), server("fresh", ""))
	if state, err := mcpServerState("k8s"); err != nil || state != "Connected" {
		t.Errorf("mcpServerState(k8s) = %q, %v", state, err)
	}
	if state, err := mcpServerState("fresh"); err != nil || state != "" {
		t.Errorf("mcpServerState(fresh) = %q, %v; want \"\" before the first reconcile", state, err)
	}
	if _, err := mcpServerState("absent"); err == nil || !apierrors.IsNotFound(err) {
		t.Errorf("mcpServerState(absent) = %v, want the apiserver's NotFound", err)
	}
}

// TestKagentControllerMonitored: the kagent scrape target is expected only
// while a ServiceMonitor for the kagent controller exists in the kagent
// namespace — the connectivity chart's or the kagent chart's own; monitors of
// other components or namespaces do not count.
func TestKagentControllerMonitored(t *testing.T) {
	newFakeLab(t, customObject(serviceMonitorGVK, platformNamespace, "agent-platform-connectivity-kagent-controller", nil))
	if kagentControllerMonitored() {
		t.Error("a monitor in another namespace counted")
	}
	newFakeLab(t, customObject(serviceMonitorGVK, kagentNamespace, "kagent-ui", nil))
	if kagentControllerMonitored() {
		t.Error("a monitor of another component counted")
	}
	newFakeLab(t, customObject(serviceMonitorGVK, kagentNamespace, "agent-platform-connectivity-kagent-controller", nil))
	if !kagentControllerMonitored() {
		t.Error("the connectivity chart's controller monitor not seen")
	}
}

// TestPlatformTopologyForRefusesAChartThatWillNotTakeTheValues: the boot's
// FIRST render holds the verdict, so a chart that refuses the lab's values is
// reported there — before the certs, the kind cluster and Dex, which is the
// five minutes the refusal exists to save. Every other render failure stays a
// note and the boot proceeds on the budgeted topology.
func TestPlatformTopologyForRefusesAChartThatWillNotTakeTheValues(t *testing.T) {
	dir := isolateHelm(t)
	cfg := config.Default()
	cfg.Platform.Enabled = true

	// A meta chart whose closed schema takes nothing the lab sends it.
	cfg.Platform.ChartPath = writeClosedSchemaChart(t, dir)

	_, err := platformTopologyFor(cfg)
	if err == nil {
		t.Fatal("a chart that refuses the lab's values did not stop the boot at its first render")
	}
	// Whole, with the advice for THIS mode — a local chart directory, where
	// platform.chartVersion is ignored.
	for _, want := range []string{"does not accept the values", "Fix the chart at " + cfg.Platform.ChartPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), chartVersionKnob) {
		t.Errorf("a chartPath lab is pointed at platform.chartVersion, which it ignores:\n%s", err)
	}
	if strings.Contains(err.Error(), "...") {
		t.Errorf("the refusal is truncated, hiding the schema path:\n%s", err)
	}

	// A chart that is simply not there is the best-effort case: noted, and
	// the boot goes on with the budgeted topology.
	cfg.Platform.ChartPath = filepath.Join(dir, "no-such-chart")
	if _, err := platformTopologyFor(cfg); err != nil {
		t.Errorf("a missing chart stopped the boot instead of falling back: %v", err)
	}
}

// TestPlatformChartRemedy: the advice a refusal ends with names the knob that
// selects the chart in the lab's mode — never chartVersion where it is
// ignored (chartPath) or overwritten on the next run (an unpinned branch).
func TestPlatformChartRemedy(t *testing.T) {
	for _, c := range []struct {
		chart       platformChart
		want, never string
	}{
		{platformChart{ref: "/src/agent-platform"}, "/src/agent-platform", chartVersionKnob},
		{platformChart{ref: config.ChartRepository, version: "4.0.0-dev.feat.x.20260915.h1", branch: "feat/x"}, "feat/x", chartVersionKnob},
		{platformChart{ref: config.ChartRepository, version: "4.15.2"}, chartVersionKnob, "branch"},
	} {
		got := c.chart.remedy()
		if !strings.Contains(got, c.want) {
			t.Errorf("remedy for %s lacks %q: %s", c.chart, c.want, got)
		}
		if strings.Contains(got, c.never) {
			t.Errorf("remedy for %s names %q, which cannot help there: %s", c.chart, c.never, got)
		}
	}
}
