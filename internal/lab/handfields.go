package lab

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/structured-merge-diff/v6/value"
)

// A hand's edits on the platform's workloads, put back to the release's
// render. A `kubectl set env`, `kubectl patch` or `kubectl edit` on a
// Helm-owned workload outlives every platform run (foreignfields.go): Helm
// and drift correction own only what the render carries. So a lab restored to
// its recorded agentlab.yaml would still hand the next holder the previous
// holder's patch. `agentlab platform` — the step every restore ends with —
// puts each field one of kubectl's imperative edits wrote back to what the
// object's release rendered: a field the render carries is set to the
// render's value, one it does not carry is removed, and the edit's
// managedFields entry is dropped. A field the walk cannot put back fails the
// run, named. A write by any other manager (an operator's, a controller's)
// is no hand's edit to undo: the foreign-field check names it.

// handFieldManagers are the field managers of kubectl's imperative edits.
// `kubectl rollout` is not among them: its restart stamp is a roll
// (restartedAtPath), its undo the rollback of a revision.
var handFieldManagers = []string{
	"kubectl-set", "kubectl-patch", "kubectl-edit", "kubectl-client-side-apply", "kubectl-label", "kubectl-annotate",
}

// handRolloutTimeout bounds the wait for a Deployment the put-back rolled.
const handRolloutTimeout = 5 * time.Minute

// isHandEdit reports whether a managedFields entry is one of kubectl's
// imperative edits of the object itself.
func isHandEdit(entry metav1.ManagedFieldsEntry) bool {
	return entry.Subresource == "" && entry.FieldsV1 != nil && slices.Contains(handFieldManagers, entry.Manager)
}

// putBackRender puts the hand's edits on live back to rendered, the object as
// its release rendered it, and drops their managedFields entries. It returns
// what it put back; on an error live is unchanged.
func putBackRender(live, rendered *unstructured.Unstructured) ([]foreignFields, error) {
	work := live.DeepCopy()
	var put []foreignFields
	var kept []metav1.ManagedFieldsEntry
	for _, entry := range work.GetManagedFields() {
		if !isHandEdit(entry) {
			kept = append(kept, entry)
			continue
		}
		set := &fieldpath.Set{}
		if err := set.FromJSON(entry.FieldsV1.GetRawReader()); err != nil {
			return nil, fmt.Errorf("reading the fields %s owns on %s %s/%s: %w", entry.Manager, live.GetKind(), live.GetNamespace(), live.GetName(), err)
		}
		f := foreignFields{Kind: live.GetKind(), Namespace: live.GetNamespace(), Name: live.GetName(), Manager: entry.Manager}
		for _, p := range outermostPaths(set) {
			f.Fields = append(f.Fields, p.String())
			want, rendersIt, err := fieldAt(rendered.Object, p)
			if err == nil {
				work.Object, err = putField(work.Object, p, runtime.DeepCopyJSONValue(want), !rendersIt)
			}
			if err != nil {
				return nil, fmt.Errorf("putting %s %s/%s: %s by %s back to its release's render: %w",
					strings.ToLower(live.GetKind()), live.GetNamespace(), live.GetName(), p, entry.Manager, err)
			}
		}
		if len(f.Fields) > 0 {
			put = append(put, f)
		}
	}
	if len(put) == 0 {
		return nil, nil
	}
	work.SetManagedFields(kept)
	live.Object = work.Object
	return put, nil
}

// fieldAt reads the field at p: its value and whether it is there.
func fieldAt(node any, p fieldpath.Path) (any, bool, error) {
	for _, pe := range p {
		switch {
		case pe.FieldName != nil:
			m, ok := node.(map[string]any)
			if !ok {
				return nil, false, nil
			}
			if node, ok = m[*pe.FieldName]; !ok {
				return nil, false, nil
			}
		case pe.Key != nil || pe.Value != nil:
			l, _ := node.([]any)
			i := itemIndex(l, pe)
			if i < 0 {
				return nil, false, nil
			}
			node = l[i]
		default:
			return nil, false, errUnwalkable
		}
	}
	return node, true, nil
}

// errUnwalkable is a path element the put-back cannot address: a list item
// by position, which says nothing about which item it was once the list
// changed.
var errUnwalkable = errors.New("a list item by position cannot be put back")

// putField sets the field at p to v, or removes it with remove, under node,
// and returns node: a field already missing stays missing.
func putField(node any, p fieldpath.Path, v any, remove bool) (map[string]any, error) {
	m, _ := node.(map[string]any)
	out, err := putAt(m, p, v, remove)
	if err != nil {
		return nil, err
	}
	m, _ = out.(map[string]any)
	return m, nil
}

func putAt(node any, p fieldpath.Path, v any, remove bool) (any, error) {
	if len(p) == 0 {
		return v, nil
	}
	pe, rest := p[0], p[1:]
	switch {
	case pe.FieldName != nil:
		m, _ := node.(map[string]any)
		child, there := m[*pe.FieldName]
		if remove && (!there || len(rest) == 0) {
			delete(m, *pe.FieldName)
			return m, nil
		}
		if m == nil {
			m = map[string]any{}
		}
		child, err := putAt(child, rest, v, remove)
		if err != nil {
			return nil, err
		}
		m[*pe.FieldName] = child
		return m, nil
	case pe.Key != nil || pe.Value != nil:
		l, _ := node.([]any)
		i := itemIndex(l, pe)
		switch {
		case i < 0 && remove:
			return l, nil
		case i < 0 && len(rest) == 0:
			return append(l, v), nil
		case i < 0:
			return nil, fmt.Errorf("the list holds no item %s", pe)
		case remove && len(rest) == 0:
			return slices.Delete(l, i, i+1), nil
		}
		child, err := putAt(l[i], rest, v, remove)
		if err != nil {
			return nil, err
		}
		l[i] = child
		return l, nil
	default:
		return nil, errUnwalkable
	}
}

// itemIndex finds the list item a path element names: by its key fields, a
// key field the item does not carry matching (a render leaves out what the
// apiserver defaults, a port's protocol), or by its value; -1 when no item
// matches.
func itemIndex(l []any, pe fieldpath.PathElement) int {
	return slices.IndexFunc(l, func(item any) bool {
		if pe.Value != nil {
			return value.Equals(value.NewValueInterface(item), *pe.Value)
		}
		m, ok := item.(map[string]any)
		if !ok {
			return false
		}
		for _, f := range *pe.Key {
			if got, ok := m[f.Name]; ok && !value.Equals(value.NewValueInterface(got), f.Value) {
				return false
			}
		}
		return true
	})
}

// putBackHandFields puts the hand's edits on the platform's Helm-owned
// workloads back to their releases' render (putBackRender), naming each, and
// waits for a Deployment it rolled. A workload whose release cannot be read,
// or a field that cannot be put back, fails the run, named.
func putBackHandFields(ctx context.Context) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	var failed []string
	for _, gvr := range workloadResources {
		items, err := listObjects(ctx, gvr, "", helmOwnedSelector)
		if err != nil {
			return err
		}
		for _, item := range items {
			if !slices.ContainsFunc(item.GetManagedFields(), isHandEdit) {
				continue
			}
			ns, name := item.GetNamespace(), item.GetName()
			var put []foreignFields
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				live, err := k.resource(gvr, ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				rendered, err := renderedObject(live)
				if err != nil {
					return err
				}
				if put, err = putBackRender(live, rendered); err != nil || len(put) == 0 {
					return err
				}
				_, err = k.resource(gvr, ns).Update(ctx, live, metav1.UpdateOptions{FieldManager: applyFieldManager})
				return err
			})
			if err != nil && !apierrors.IsNotFound(err) {
				failed = append(failed, fmt.Sprintf("  %s %s/%s: %v", strings.ToLower(item.GetKind()), ns, name, err))
				continue
			}
			for _, f := range put {
				step("Put back to its release's render: %s", f)
			}
			if len(put) > 0 && gvr == gvrDeployments {
				if err := waitDeploymentRolledOut(ctx, ns, name, handRolloutTimeout); err != nil {
					failed = append(failed, fmt.Sprintf("  deployment %s/%s: %v", ns, name, err))
				}
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d platform workload(s) still carry a field `kubectl set`, `patch` or `edit` wrote, which the run could not put back to the release's render:\n%s\n"+
			"remove each with the inverse edit (`kubectl patch --type=json` with a remove op, or `kubectl edit`) and run `agentlab platform` again",
			len(failed), strings.Join(failed, "\n"))
	}
	return nil
}

// renderedObject is the workload as its Helm release rendered it, found in
// the release's newest manifest by the release annotations Helm stamps on it.
func renderedObject(live *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	a := live.GetAnnotations()
	releaseName, releaseNamespace := a[helmReleaseNameAnnotation], a[helmReleaseNamespaceAnnotation]
	if releaseName == "" || releaseNamespace == "" {
		return nil, fmt.Errorf("it names no Helm release (%s, %s)", helmReleaseNameAnnotation, helmReleaseNamespaceAnnotation)
	}
	manifest, _, err := chartReleaseManifest(releaseNamespace, releaseName)
	if err != nil {
		return nil, err
	}
	objs, err := decodeManifests([]byte(manifest))
	if err != nil {
		return nil, fmt.Errorf("helm release %s/%s: %w", releaseNamespace, releaseName, err)
	}
	for _, obj := range objs {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = releaseNamespace
		}
		if obj.GetKind() == live.GetKind() && obj.GetName() == live.GetName() && ns == live.GetNamespace() {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("helm release %s/%s renders no %s %s/%s", releaseNamespace, releaseName, live.GetKind(), live.GetNamespace(), live.GetName())
}
