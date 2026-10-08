package lab

import (
	"context"
	"maps"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// overlayFor renders the overlay manifest for a config.
func overlayFor(t *testing.T, cfg *config.Config) []byte {
	t.Helper()
	raw, err := renderTemplate(cfg, backstageOverlayTemplate, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return raw
}

// valuesOverlayChecksum is the checksum the values carry in the overlay's
// extraAppConfig entry, "" without one.
func valuesOverlayChecksum(t *testing.T, cfg *config.Config) string {
	t.Helper()
	raw, err := renderTemplate(cfg, platformValuesTemplate, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var values struct {
		Backstage struct {
			Backstage struct {
				ExtraAppConfig []struct {
					ConfigMapRef string `yaml:"configMapRef"`
					Checksum     string `yaml:"checksum"`
				} `yaml:"extraAppConfig"`
			} `yaml:"backstage"`
		} `yaml:"backstage"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("values are not YAML: %v", err)
	}
	for _, e := range values.Backstage.Backstage.ExtraAppConfig {
		if e.ConfigMapRef == labAppConfigMap {
			return e.Checksum
		}
	}
	t.Fatalf("the values list no %s", labAppConfigMap)
	return ""
}

// TestOverlayChecksum: the overlay ConfigMap carries the checksum of its own
// data, the values carry the same in its extraAppConfig entry, and a config
// change that moves the overlay's content moves both; an older chart line
// gets no checksum key.
func TestOverlayChecksum(t *testing.T) {
	cfg := config.Default()
	cm, err := configMapIn(overlayFor(t, cfg), labAppConfigMap)
	if err != nil {
		t.Fatal(err)
	}
	stamp := cm.GetAnnotations()[dataChecksumAnnotation]
	if stamp == "" || stamp != dataChecksum(configMapData(cm)) {
		t.Fatalf("stamp %q, want the data's checksum %q", stamp, dataChecksum(configMapData(cm)))
	}
	if got := valuesOverlayChecksum(t, cfg); got != stamp {
		t.Errorf("values carry %q, want the overlay's stamp %q", got, stamp)
	}

	toggled := config.Default()
	toggled.Platform.Observability = !cfg.Platform.Observability
	moved, err := configMapIn(overlayFor(t, toggled), labAppConfigMap)
	if err != nil {
		t.Fatal(err)
	}
	if moved.GetAnnotations()[dataChecksumAnnotation] == stamp {
		t.Error("toggling platform.observability changed the overlay but not its checksum")
	}
	if valuesOverlayChecksum(t, toggled) == stamp {
		t.Error("toggling platform.observability changed the overlay but not the values' checksum")
	}

	seed := config.Default()
	seed.Platform.ChartVersion, seed.Platform.UpgradeSeed = upgradeSeedVersion, true
	if got := valuesOverlayChecksum(t, seed); got != "" {
		t.Errorf("an older chart line's values carry checksum %q, which its backstage chart refuses", got)
	}
}

// backstageDeployment is the portal's Deployment as the chart leaves it.
func backstageDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: kindDeployment},
		ObjectMeta: metav1.ObjectMeta{Name: componentBackstage, Namespace: platformNamespace},
	}
}

// TestApplyBackstageOverlay: a fresh install applies and rolls nothing; an
// unchanged re-run rolls nothing; a hand edit of the overlay is put back and
// asks for one roll, a key a hand edit added is dropped; a changed render is
// left to the chart's checksum, and rolled here only on a chart line whose
// values cannot carry it.
func TestApplyBackstageOverlay(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	overlay := overlayFor(t, cfg)

	f := newFakeLab(t)
	if roll, err := applyBackstageOverlay(ctx, overlay, true); err != nil || roll {
		t.Fatalf("fresh install: roll=%v err=%v, want neither", roll, err)
	}
	want := configMapData(f.stored(t, gvrConfigMaps, platformNamespace, labAppConfigMap))

	f = newFakeLab(t, backstageDeployment())
	if _, err := applyManifests(ctx, overlay); err != nil {
		t.Fatal(err)
	}
	if roll, err := applyBackstageOverlay(ctx, overlay, true); err != nil || roll {
		t.Errorf("unchanged re-run: roll=%v err=%v, want neither", roll, err)
	}

	edit := `{"data":{"app-config.agentlab.yaml":"app:\n  baseUrl: https://edited\n","extra.yaml":"x: 1\n"}}`
	if err := patchObject(ctx, gvrConfigMaps, platformNamespace, labAppConfigMap, types.MergePatchType, []byte(edit)); err != nil {
		t.Fatal(err)
	}
	drift, err := appConfigDrift(ctx)
	if err != nil || len(drift) != 1 || !strings.Contains(drift[0], labAppConfigMap) {
		t.Errorf("status after the edit: %v err=%v, want the overlay named", drift, err)
	}
	if roll, err := applyBackstageOverlay(ctx, overlay, true); err != nil || !roll {
		t.Errorf("after a hand edit: roll=%v err=%v, want a roll", roll, err)
	}
	if got := configMapData(f.stored(t, gvrConfigMaps, platformNamespace, labAppConfigMap)); !maps.Equal(got, want) {
		t.Errorf("after the restore the data is %v, want the render %v", got, want)
	}
	if drift, err := appConfigDrift(ctx); err != nil || len(drift) != 0 {
		t.Errorf("status after the restore: %v err=%v, want in sync", drift, err)
	}
	if roll, err := applyBackstageOverlay(ctx, overlay, true); err != nil || roll {
		t.Errorf("the run after the restore: roll=%v err=%v, want neither", roll, err)
	}

	toggled := config.Default()
	toggled.Platform.Observability = !cfg.Platform.Observability
	if roll, err := applyBackstageOverlay(ctx, overlayFor(t, toggled), true); err != nil || roll {
		t.Errorf("a changed render with the checksum in the values: roll=%v err=%v, want the chart to roll it", roll, err)
	}
	if roll, err := applyBackstageOverlay(ctx, overlay, false); err != nil || !roll {
		t.Errorf("a changed render without the checksum in the values: roll=%v err=%v, want a roll", roll, err)
	}
}

// chartConfigMap is the chart's app-config ConfigMap with Helm's release
// annotations and the given data.
func chartConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: chartAppConfigMap, Namespace: platformNamespace, Annotations: map[string]string{
			helmReleaseNameAnnotation:      "connectivity",
			helmReleaseNamespaceAnnotation: platformNamespace,
		}},
		Data: data,
	}
}

// TestRestoreChartAppConfig: the chart's ConfigMap as its release rendered
// it is left alone; a hand edit is reported by status, put back to the
// release's manifest (an added key dropped) and reported as restored once.
func TestRestoreChartAppConfig(t *testing.T) {
	ctx := context.Background()
	rendered := map[string]string{"app-config.agent-platform.yaml": "app:\n  title: Agent Platform\n"}
	manifest := "---\n# Source: connectivity/templates/backstage/app-config.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + chartAppConfigMap +
		"\n  namespace: agent-platform\ndata:\n  app-config.agent-platform.yaml: |\n    app:\n      title: Agent Platform\n"
	prev := chartReleaseManifest
	chartReleaseManifest = func(ns, name string) (string, int, error) {
		if ns != platformNamespace || name != "connectivity" {
			t.Errorf("release read as %s/%s, want the ConfigMap's annotations", ns, name)
		}
		return manifest, 3, nil
	}
	t.Cleanup(func() { chartReleaseManifest = prev })

	newFakeLab(t, chartConfigMap(rendered))
	if restored, err := restoreChartAppConfig(ctx); err != nil || restored {
		t.Errorf("as rendered: restored=%v err=%v, want neither", restored, err)
	}

	f := newFakeLab(t, chartConfigMap(map[string]string{"app-config.agent-platform.yaml": "app:\n  title: Edited\n", "extra.yaml": "x: 1\n"}))
	drift, err := appConfigDrift(ctx)
	if err != nil || len(drift) != 1 || !strings.Contains(drift[0], chartAppConfigMap) || !strings.Contains(drift[0], "revision 3") {
		t.Errorf("status after the edit: %v err=%v, want the chart's ConfigMap and its release named", drift, err)
	}
	if restored, err := restoreChartAppConfig(ctx); err != nil || !restored {
		t.Errorf("after a hand edit: restored=%v err=%v, want a restore", restored, err)
	}
	if got := configMapData(f.stored(t, gvrConfigMaps, platformNamespace, chartAppConfigMap)); !maps.Equal(got, rendered) {
		t.Errorf("after the restore the data is %v, want the release's %v", got, rendered)
	}
	if restored, err := restoreChartAppConfig(ctx); err != nil || restored {
		t.Errorf("the run after the restore: restored=%v err=%v, want neither", restored, err)
	}
}

// TestAppConfigLine: in sync it says so; drifted it names each ConfigMap and
// the command that restores it.
func TestAppConfigLine(t *testing.T) {
	if got := appConfigLine(nil); got != "as rendered" {
		t.Errorf("in sync: %q", got)
	}
	got := appConfigLine([]string{labAppConfigMap + " differs from the lab's render"})
	if !strings.Contains(got, labAppConfigMap) || !strings.Contains(got, "agentlab platform") {
		t.Errorf("drifted: %q, want the ConfigMap and the command named", got)
	}
}
