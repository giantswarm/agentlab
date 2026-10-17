package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// Fields on the platform's workloads that no release renders. A Deployment
// the platform's Helm releases own carries what the chart renders, plus what
// a hand on the cluster wrote beside it — a `kubectl patch` of hostNetwork,
// a `kubectl set env`. helm-controller's drift correction removes no such
// field: Helm and drift detection own and correct only the fields the render
// carries. So a restored lab — its agentlab.yaml back by checksum, every
// workload Ready — would still run the patch, and a later release that no
// longer needs it leaves pods that cannot schedule without it. The apiserver
// records who wrote each field (the object's managedFields): kubectl's edits
// `agentlab platform` puts back to the render (handfields.go); a field
// written by any other manager outside the release's writers is named by
// `agentlab status`, warned by every proof's opening and by `agentlab
// platform`, and fails platform-test.

// releaseFieldManagers are the managers that write a Helm-owned workload as
// the lab means it: the Helm SDK the lab runs and the CLI (helmFieldManager),
// Flux's helm-controller for the component releases, the lab's own client
// (applyFieldManager: its rollout restarts), the connectivity release's
// config-reload hook (configReloadFieldManager), and the controller-manager's
// writes beside the spec.
var releaseFieldManagers = []string{helmFieldManager, "helm-controller", applyFieldManager, configReloadFieldManager, "kube-controller-manager"}

// configReloadFieldManager is the field manager of the agent-platform
// connectivity chart's config-reload hook: after every install or upgrade of
// that release, its Job stamps the checksum of the rendered Backstage
// app-config into the Backstage pod template
// (AGENT_PLATFORM_APP_CONFIG_CHECKSUM), so a config change rolls the portal.
// The stamp is the release's own write, never a hand's.
const configReloadFieldManager = "agent-platform-config-reload"

// restartedAtPath is the pod-template annotation a rollout restart stamps:
// `kubectl rollout restart` and the lab's own (restartDeployment) — a roll,
// not a field the workload runs with, so it is no foreign field whoever
// wrote it.
var restartedAtPath = fieldpath.MakePathOrDie("spec", "template", "metadata", "annotations", restartedAtAnnotation)

// helmOwnedSelector selects the workloads a Helm release owns: Helm's own
// label on every object a chart renders the standard way.
const helmOwnedSelector = "app.kubernetes.io/managed-by=Helm"

// workloadResources are the workload kinds the check reads.
var workloadResources = []schema.GroupVersionResource{
	gvrDeployments,
	appsv1.SchemeGroupVersion.WithResource("statefulsets"),
	appsv1.SchemeGroupVersion.WithResource("daemonsets"),
}

// foreignFields is one workload's fields written by one manager outside the
// release's writers.
type foreignFields struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Manager   string `json:"manager"`
	// Fields are the outermost paths the manager owns, in the apiserver's
	// field-path notation (.spec.template.spec.hostNetwork).
	Fields []string `json:"fields"`
}

func (f foreignFields) String() string {
	return fmt.Sprintf("%s %s/%s: %s by %s", strings.ToLower(f.Kind), f.Namespace, f.Name, strings.Join(f.Fields, ", "), f.Manager)
}

// foreignFieldsOf reads one workload's managedFields: an entry per manager
// outside releaseFieldManagers that owns a field of the object itself (a
// status or scale subresource write is none), the rollout annotation left
// out.
func foreignFieldsOf(obj *unstructured.Unstructured) ([]foreignFields, error) {
	var found []foreignFields
	for _, entry := range obj.GetManagedFields() {
		if entry.Subresource != "" || entry.FieldsV1 == nil || slices.Contains(releaseFieldManagers, entry.Manager) {
			continue
		}
		set := &fieldpath.Set{}
		if err := set.FromJSON(entry.FieldsV1.GetRawReader()); err != nil {
			return nil, fmt.Errorf("reading the fields %s owns on %s %s/%s: %w", entry.Manager, obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
		if fields := outermostFields(set); len(fields) > 0 {
			found = append(found, foreignFields{
				Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(),
				Manager: entry.Manager, Fields: fields,
			})
		}
	}
	return found, nil
}

// outermostFields words a manager's field set: the paths no other path of
// the set contains, so a container the manager added is the container and
// not its every field. The rollout annotation and the maps that hold it
// (marked owned when the annotation created them) are left out.
func outermostFields(set *fieldpath.Set) []string {
	var fields []string
	for _, p := range outermostPaths(set) {
		fields = append(fields, p.String())
	}
	return fields
}

// outermostPaths are the paths outermostFields words.
func outermostPaths(set *fieldpath.Set) []fieldpath.Path {
	var paths []fieldpath.Path
	set.Iterate(func(p fieldpath.Path) {
		if !hasPrefix(restartedAtPath, p) {
			paths = append(paths, p.Copy())
		}
	})
	var outer []fieldpath.Path
	for _, p := range paths {
		if !slices.ContainsFunc(paths, func(q fieldpath.Path) bool { return len(q) < len(p) && hasPrefix(p, q) }) {
			outer = append(outer, p)
		}
	}
	return outer
}

// hasPrefix reports whether prefix is p or one of its ancestors.
func hasPrefix(p, prefix fieldpath.Path) bool {
	return len(prefix) <= len(p) && p[:len(prefix)].Equals(prefix)
}

// foreignWorkloadFields lists, across the cluster, the Helm-owned workloads'
// fields written outside the release's writers.
func foreignWorkloadFields(ctx context.Context) ([]foreignFields, error) {
	var found []foreignFields
	for _, gvr := range workloadResources {
		items, err := listObjects(ctx, gvr, "", helmOwnedSelector)
		if err != nil {
			return nil, err
		}
		for i := range items {
			fields, err := foreignFieldsOf(&items[i])
			if err != nil {
				return nil, err
			}
			found = append(found, fields...)
		}
	}
	return found, nil
}

// foreignFieldsError words the foreign fields as the failure of a lab that
// claims to run its releases' render, with the way back.
func foreignFieldsError(found []foreignFields) error {
	lines := make([]string, 0, len(found))
	for _, f := range found {
		lines = append(lines, "  "+f.String())
	}
	return fmt.Errorf("%d platform workload field(s) were written outside the platform's Helm releases, which no release renders and drift correction never removes:\n%s\n"+
		"`agentlab platform` puts back what kubectl set, patched or edited; remove the rest with the inverse edit (`kubectl patch --type=json` with a remove op) — a lab change belongs in agentlab.yaml (platform.devImages, platform.chartPath), never a patch",
		len(found), strings.Join(lines, "\n"))
}

// warnForeignFields warns of the foreign fields on the lab's workloads; a
// read that fails is warned too, never the caller's failure.
func warnForeignFields(ctx context.Context) {
	found, err := foreignWorkloadFields(ctx)
	switch {
	case err != nil:
		warn("could not read the platform workloads' field managers: %v", err)
	case len(found) > 0:
		warn("%v", foreignFieldsError(found))
	}
}
