package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	releasecommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fluxActionTimeout is helm-controller's timeout for an install, upgrade or
// uninstall of a HelmRelease without spec.timeout; orphanDeleteMargin is
// what the wait for an orphan's deletion adds to its two actions.
const (
	fluxActionTimeout  = 5 * time.Minute
	orphanDeleteMargin = time.Minute
)

// orphanDeleteWait bounds the wait for an orphaned component's HelmRelease to
// be gone. helm-controller finishes the action it is running first — a
// failed upgrade's leftover is often mid-install, retrying — then uninstalls
// the component's release, each bounded by the HelmRelease's own timeout
// (spec.uninstall.timeout for the uninstall), and only then releases its
// finalizer.
func orphanDeleteWait(obj *unstructured.Unstructured) time.Duration {
	timeout := func(fields ...string) time.Duration {
		s, _, _ := unstructured.NestedString(obj.Object, fields...)
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
		return 0
	}
	action := timeout("spec", "timeout")
	if action == 0 {
		action = fluxActionTimeout
	}
	uninstall := timeout("spec", "uninstall", "timeout")
	if uninstall == 0 {
		uninstall = action
	}
	return action + uninstall + orphanDeleteMargin
}

// platformFluxKinds are the component kinds the meta chart renders, in the
// order an orphan goes: the HelmRelease first, while its chart source is
// still there for helm-controller's uninstall, then the OCIRepository.
var platformFluxKinds = []struct{ kind, resource string }{
	{kindHelmRelease, fluxHelmReleaseResource},
	{kindOCIRepository, fluxOCIRepositoryResource},
}

// removePlatformOrphans reconciles the platform's component set with the
// release's deployed revision: every HelmRelease and OCIRepository the
// agent-platform release owns (Helm's release annotations) that the deployed
// revision does not render is deleted and named. Helm's own upgrade never
// removes them: it diffs against the last deployed revision, so the
// components a failed upgrade added stay behind after the next deployed one,
// and later proofs would read the lab as carrying them. Nothing happens while
// the newest revision is not deployed (a failed or pending one): there is no
// component set to reconcile against until a run deploys one, and that run
// cleans up. Objects of other owners (the lab's own mcp-prometheus,
// agent-manager's agent sources) are never touched.
func removePlatformOrphans(ctx context.Context) ([]string, error) {
	rel, err := helmReleaseNewest(platformNamespace, platformRelease)
	if err != nil {
		return nil, err
	}
	if rel == nil || rel.Info == nil || rel.Info.Status != releasecommon.StatusDeployed {
		return nil, nil
	}
	rendered, err := renderedFluxObjects(releaseManifests(rel), platformNamespace)
	if err != nil {
		return nil, fmt.Errorf("reading the Flux objects of %s revision %d: %w", platformRelease, rel.Version, err)
	}
	return removeFluxOrphans(ctx, rendered, platformRelease, platformNamespace)
}

// releaseManifests is everything a revision renders: its manifest and its
// hooks' manifests.
func releaseManifests(rel *release.Release) string {
	docs := []string{rel.Manifest}
	for _, h := range rel.Hooks {
		if h != nil {
			docs = append(docs, h.Manifest)
		}
	}
	return strings.Join(docs, "\n---\n")
}

// renderedFluxObjects are the HelmReleases and OCIRepositories of a
// multi-document manifest, keyed by fluxObjectKey; an object without a
// namespace lands in the release's.
func renderedFluxObjects(manifests, releaseNamespace string) (map[string]bool, error) {
	out := map[string]bool{}
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	for {
		var doc fluxDoc
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		ns := doc.Metadata.Namespace
		if ns == "" {
			ns = releaseNamespace
		}
		for _, k := range platformFluxKinds {
			if doc.Kind == k.kind {
				out[fluxObjectKey(k.kind, ns, doc.Metadata.Name)] = true
			}
		}
	}
}

func fluxObjectKey(kind, ns, name string) string {
	return kind + " " + ns + "/" + name
}

// removeFluxOrphans deletes every HelmRelease and OCIRepository the release
// owns that rendered does not hold, the HelmReleases first and waited for,
// and returns them as "<Kind> <namespace>/<name>", sorted per kind.
func removeFluxOrphans(ctx context.Context, rendered map[string]bool, releaseName, releaseNamespace string) ([]string, error) {
	var removed []string
	for _, k := range platformFluxKinds {
		gvr, err := gvrFor(k.resource)
		if err != nil {
			return removed, err
		}
		items, err := listObjects(ctx, gvr, "", "")
		if err != nil {
			return removed, err
		}
		var orphans []unstructured.Unstructured
		for _, obj := range items {
			if ownedByRelease(&obj, releaseName, releaseNamespace) && !rendered[fluxObjectKey(k.kind, obj.GetNamespace(), obj.GetName())] {
				orphans = append(orphans, obj)
			}
		}
		sort.Slice(orphans, func(i, j int) bool {
			return fluxObjectKey(k.kind, orphans[i].GetNamespace(), orphans[i].GetName()) < fluxObjectKey(k.kind, orphans[j].GetNamespace(), orphans[j].GetName())
		})
		for _, obj := range orphans {
			var waitGone time.Duration
			if k.kind == kindHelmRelease {
				waitGone = orphanDeleteWait(&obj)
			}
			if err := deleteObject(ctx, gvr, obj.GetNamespace(), obj.GetName(), waitGone); err != nil {
				return removed, err
			}
			removed = append(removed, fluxObjectKey(k.kind, obj.GetNamespace(), obj.GetName()))
		}
	}
	return removed, nil
}

// ownedByRelease reports whether Helm's release annotations — what makes an
// object a release's, whatever its labels say — name the release.
func ownedByRelease(obj *unstructured.Unstructured, releaseName, releaseNamespace string) bool {
	a := obj.GetAnnotations()
	return a[helmReleaseNameAnnotation] == releaseName && a[helmReleaseNamespaceAnnotation] == releaseNamespace
}
