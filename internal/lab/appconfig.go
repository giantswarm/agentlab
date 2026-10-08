package lab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Backstage's app-config ConfigMaps, the two files backstage.extraAppConfig
// lists (agent-platform-values.yaml.tmpl): the chart's, rendered by the
// connectivity release, and the lab's overlay on top of it
// (backstage-catalog.yaml.tmpl). Backstage reads both at start only, so a
// hand edit of either — a debugging session, a test's patch — is what the
// portal runs on after its next restart while the lab looks converged.
//
// The platform step puts both back on every run. The overlay is the lab's
// own render, server-side applied under the lab's field manager and forced
// (applyManifests); the chart's ConfigMap is the connectivity release's,
// restored from that release's manifest with a merge patch, so the field
// ownership stays helm-controller's. Data keys neither render carries are
// dropped. Backstage rolls only when a restore changed what it reads: a
// changed overlay render rolls it through the chart instead (the overlay's
// extraAppConfig checksum, folded into the pod template), and a run without
// drift restarts nothing. `agentlab status` names a drifted ConfigMap.
const (
	chartAppConfigMap = "agent-platform-backstage-app-config"
	labAppConfigMap   = "agentlab-backstage-app-config"
	// dataChecksumAnnotation carries the checksum of the overlay's data as
	// the lab rendered it: a hand edit changes the data, never the stamp.
	dataChecksumAnnotation = "agentlab.giantswarm.io/data-checksum"
	// The annotations Helm stamps on every object of a release: where the
	// chart's ConfigMap's rendered content is stored.
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
	// backstageRolloutTimeout bounds the wait for a Backstage pod the lab
	// rolled after a restore: image cached, one pod, its startup probe.
	backstageRolloutTimeout = 5 * time.Minute
)

// dataChecksum is the sha256 of a ConfigMap's data, its keys in order: the
// same for the render and the live object exactly when the data is.
func dataChecksum(data map[string]string) string {
	raw, _ := json.Marshal(data) // a map of strings always encodes
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// overlayDataChecksum renders the overlay with the data in hand and returns
// its app-config ConfigMap's data checksum. The stamp itself is metadata, so
// rendering with an empty one yields the checksum it is about to carry.
func overlayDataChecksum(data *tmplData) (string, error) {
	raw, err := executeTemplate(backstageOverlayTemplate, data)
	if err != nil {
		return "", err
	}
	cm, err := configMapIn(raw, labAppConfigMap)
	if err != nil {
		return "", err
	}
	return dataChecksum(configMapData(cm)), nil
}

// configMapIn finds the named ConfigMap among a manifest's documents.
func configMapIn(manifest []byte, name string) (*unstructured.Unstructured, error) {
	objs, err := decodeManifests(manifest)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if obj.GetKind() == kindConfigMap && obj.GetName() == name {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("the manifest carries no ConfigMap %s", name)
}

// configMapData is a ConfigMap's data, empty when it has none.
func configMapData(cm *unstructured.Unstructured) map[string]string {
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	if data == nil {
		data = map[string]string{}
	}
	return data
}

// applyBackstageOverlay applies the overlay manifest (the catalog and the
// app-config ConfigMaps), drops the data keys a hand edit added, and reports
// whether Backstage must roll for it: a changed catalog, or an app-config
// overlay put back to the render the pod template already carries. A changed
// render is rolled by the chart when the values carry the checksum
// (checksumInValues) and by this restart when they cannot; a Backstage that
// is not deployed yet starts on the final files.
func applyBackstageOverlay(ctx context.Context, manifest []byte, checksumInValues bool) (bool, error) {
	deployed, err := objectExists(ctx, gvrDeployments, platformNamespace, componentBackstage)
	if err != nil {
		return false, err
	}
	rendered, err := configMapIn(manifest, labAppConfigMap)
	if err != nil {
		return false, err
	}
	prev, err := liveDataChecksumStamp(ctx)
	if err != nil {
		return false, err
	}
	results, err := applyManifests(ctx, manifest)
	if err != nil {
		return false, err
	}
	objs, err := decodeManifests(manifest)
	if err != nil {
		return false, err
	}
	for _, obj := range objs {
		if obj.GetKind() != kindConfigMap {
			continue
		}
		if _, err := pruneConfigMapData(ctx, obj.GetNamespace(), obj.GetName(), configMapData(obj)); err != nil {
			return false, err
		}
	}
	if !deployed {
		return false, nil
	}
	renderMoved := prev != rendered.GetAnnotations()[dataChecksumAnnotation]
	for _, r := range results {
		if !r.changed || r.gvk.Kind != kindConfigMap {
			continue
		}
		if r.name == labAppConfigMap && renderMoved && checksumInValues {
			continue
		}
		if r.name == labAppConfigMap && !renderMoved {
			note("restored ConfigMap %s to the lab's render (it had been edited)", labAppConfigMap)
		}
		return true, nil
	}
	return false, nil
}

// liveDataChecksumStamp is the overlay's stamp on the cluster: the checksum
// of the render the previous run applied, "" before the first.
func liveDataChecksumStamp(ctx context.Context) (string, error) {
	live, err := getObject(ctx, gvrConfigMaps, platformNamespace, labAppConfigMap)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return live.GetAnnotations()[dataChecksumAnnotation], nil
}

// pruneConfigMapData makes a live ConfigMap's data exactly want: changed and
// missing keys set, keys outside want removed, by one merge patch. Reports
// whether there was anything to patch.
func pruneConfigMapData(ctx context.Context, ns, name string, want map[string]string) (bool, error) {
	live, err := getObject(ctx, gvrConfigMaps, ns, name)
	if err != nil {
		return false, err
	}
	have := configMapData(live)
	if maps.Equal(have, want) {
		return false, nil
	}
	data := map[string]any{}
	for k, v := range want {
		if have[k] != v {
			data[k] = v
		}
	}
	for k := range have {
		if _, ok := want[k]; !ok {
			data[k] = nil
		}
	}
	patch, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return false, err
	}
	return true, patchObject(ctx, gvrConfigMaps, ns, name, types.MergePatchType, patch)
}

// chartReleaseManifest reads the newest revision of a Helm release: its
// manifest and revision number. A test seam.
var chartReleaseManifest = func(ns, name string) (string, int, error) {
	rel, err := helmReleaseNewest(ns, name)
	if err != nil {
		return "", 0, err
	}
	if rel == nil {
		return "", 0, fmt.Errorf("helm release %s/%s has no revision", ns, name)
	}
	return rel.Manifest, rel.Version, nil
}

// chartAppConfig is the chart's app-config ConfigMap live and as its release
// rendered it, with the release it came from ("agent-platform/connectivity
// revision 3"). Nil without the ConfigMap: a chart line that does not
// render it.
type chartAppConfig struct {
	live, want map[string]string
	release    string
}

func (c *chartAppConfig) drifted() bool { return !maps.Equal(c.live, c.want) }

func readChartAppConfig(ctx context.Context) (*chartAppConfig, error) {
	live, err := getObject(ctx, gvrConfigMaps, platformNamespace, chartAppConfigMap)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ann := live.GetAnnotations()
	relName, relNs := ann[helmReleaseNameAnnotation], ann[helmReleaseNamespaceAnnotation]
	if relName == "" || relNs == "" {
		return nil, fmt.Errorf("ConfigMap %s/%s names no Helm release (%s, %s)", platformNamespace, chartAppConfigMap, helmReleaseNameAnnotation, helmReleaseNamespaceAnnotation)
	}
	manifest, revision, err := chartReleaseManifest(relNs, relName)
	if err != nil {
		return nil, err
	}
	rendered, err := configMapIn([]byte(manifest), chartAppConfigMap)
	if err != nil {
		return nil, fmt.Errorf("helm release %s/%s revision %d: %w", relNs, relName, revision, err)
	}
	return &chartAppConfig{
		live:    configMapData(live),
		want:    configMapData(rendered),
		release: fmt.Sprintf("%s/%s revision %d", relNs, relName, revision),
	}, nil
}

// restoreChartAppConfig puts the chart's app-config ConfigMap back to its
// release's render and reports whether it had drifted.
func restoreChartAppConfig(ctx context.Context) (bool, error) {
	c, err := readChartAppConfig(ctx)
	if err != nil || c == nil || !c.drifted() {
		return false, err
	}
	if _, err := pruneConfigMapData(ctx, platformNamespace, chartAppConfigMap, c.want); err != nil {
		return false, err
	}
	note("restored ConfigMap %s to Helm release %s (it had been edited)", chartAppConfigMap, c.release)
	return true, nil
}

// rollBackstage restarts the portal and waits for the new pod.
func rollBackstage(ctx context.Context) error {
	step("Rolling Backstage onto the restored app-config")
	if err := restartDeployment(ctx, platformNamespace, componentBackstage); err != nil {
		return err
	}
	return waitDeploymentRolledOut(ctx, platformNamespace, componentBackstage, backstageRolloutTimeout)
}

// appConfigDrift names the app-config ConfigMaps whose data is not what the
// lab or the chart rendered, each with what it drifted from — `agentlab
// status`. An overlay without the stamp (applied by an earlier agentlab) is
// not judged.
func appConfigDrift(ctx context.Context) ([]string, error) {
	var drift []string
	live, err := getObject(ctx, gvrConfigMaps, platformNamespace, labAppConfigMap)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, err
	default:
		if stamp := live.GetAnnotations()[dataChecksumAnnotation]; stamp != "" && stamp != dataChecksum(configMapData(live)) {
			drift = append(drift, labAppConfigMap+" differs from the lab's render")
		}
	}
	c, err := readChartAppConfig(ctx)
	if err != nil {
		return nil, err
	}
	if c != nil && c.drifted() {
		drift = append(drift, fmt.Sprintf("%s differs from Helm release %s", chartAppConfigMap, c.release))
	}
	return drift, nil
}

// appConfigLine is `agentlab status`'s app-config line.
func appConfigLine(drift []string) string {
	if len(drift) == 0 {
		return "as rendered"
	}
	return "drifted: " + strings.Join(drift, "; ") + " — `agentlab platform` restores it and rolls Backstage"
}
