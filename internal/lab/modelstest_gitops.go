package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// models-test's GitOps half (docs/models.md "Dry runs, GitOps-owned objects
// and commit mode"): every write tool's dry run changes nothing, and a
// ModelConfig Flux applies from git is never written live (gitops_owned).
// The pull request wire_model opens in mode commit needs model-manager
// pinned to a GitHub App and the person's sign-in to it, which the headless
// proof does not make.

const (
	// gitopsFluxName/Namespace are the Flux provenance a Kustomization stamps
	// on every object it applies; gitopsFluxFixture is the Kustomization the
	// gitops_owned fixture applies in the platform namespace, its inventory
	// listing the wired ModelConfig (gitopsfixture.go).
	gitopsFluxName      = "kustomize.toolkit.fluxcd.io/name"
	gitopsFluxNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	gitopsFluxFixture   = "agentlab-gitops-fixture"
	// gitopsPath is the directory the fixture's Kustomization names.
	gitopsPath = "clusters/agentlab/kagent"
	// gitopsHelmNamespace is the label a HelmRelease stamps on what it
	// renders, naming its namespace.
	gitopsHelmNamespace = "helm.toolkit.fluxcd.io/namespace"
)

// model-manager's argument names and words the proof uses.
const (
	dryRunArg     = "dryRun"
	modeArg       = "mode"
	modeCommit    = "commit"
	repositoryArg = "repository"
	pathArg       = "path"
	deleteModel   = "delete_model"
	loadModel     = "load_model"
	unloadModel   = "unload_model"
	valuesEnabled = "enabled"
	musterValues  = "muster"
)

// ModelsTestOptions are models-test's flags.
type ModelsTestOptions struct {
	// Backend is the backend to prove (default: the first of the list).
	Backend string
	// Model is the model to pull (default: the backend's proof model).
	Model string
}

// proveDryRuns calls every write tool with dryRun on the pulled and wired
// model and asserts nothing changed: the ModelConfig (resourceVersion), the
// backend's models and loaded models, the jobs; and mode commit on a tool
// that never commits answers unsupported.
func proveDryRuns(api *modelManagerTools, backendName, model, mcName, jobID string) error {
	step("dryRun on every write tool changes nothing (%s on %s, ModelConfig %s)", model, backendName, mcName)
	before, err := modelsSnapshot(api, backendName, mcName, jobID)
	if err != nil {
		return err
	}
	onModel := map[string]any{modelField: model, backendField: backendName, dryRunArg: true}
	for _, tool := range []string{"pull_model", "wire_model", "unwire_model", loadModel, unloadModel, deleteModel} {
		var answer map[string]any
		if err := api.getJSON(tool, onModel, &answer); err != nil {
			return fmt.Errorf("%s dryRun: %w", tool, err)
		}
		if answer[dryRunArg] != true {
			return fmt.Errorf("%s dryRun answered no dryRun: true: %.300v", tool, answer)
		}
		note("%s: %s", tool, dryRunSummary(answer))
	}
	var cancel map[string]any
	if err := api.getJSON("cancel_job", map[string]any{"id": jobID, dryRunArg: true}, &cancel); err != nil {
		return fmt.Errorf("cancel_job dryRun: %w", err)
	}
	if cancel[dryRunArg] != true {
		return fmt.Errorf("cancel_job dryRun answered no dryRun: true: %.300v", cancel)
	}
	note("cancel_job: the job it would cancel, %s", jobID)
	after, err := modelsSnapshot(api, backendName, mcName, jobID)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("a dry run changed the lab:\nbefore %+v\nafter  %+v", before, after)
	}
	note("unchanged: ModelConfig %s at resourceVersion %s, %d models, loaded [%s], job %s %s",
		mcName, after.modelConfigVersion, after.models, after.loaded, jobID, after.jobState)

	for _, tool := range []string{"pull_model", deleteModel, loadModel} {
		_, err := api.call(tool, map[string]any{modelField: model, backendField: backendName, modeArg: modeCommit, dryRunArg: true})
		if refusalCode(err) != "unsupported" {
			return fmt.Errorf("%s mode commit on %s answered %v, wanted unsupported", tool, backendName, err)
		}
		note("%s mode commit: %s", tool, excerpt(err.Error(), 140))
	}
	return nil
}

// modelsState is what a dry run must leave as it is.
type modelsState struct {
	modelConfigVersion string
	models             int
	loaded             string
	jobState           string
}

func modelsSnapshot(api *modelManagerTools, backendName, mcName, jobID string) (modelsState, error) {
	mc, err := readModelConfig(mcName)
	if err != nil {
		return modelsState{}, err
	}
	models, err := api.modelNames("list_models", backendName)
	if err != nil {
		return modelsState{}, err
	}
	loaded, err := api.modelNames("list_loaded_models", backendName)
	if err != nil {
		return modelsState{}, err
	}
	var job struct {
		Phase string `json:"phase"`
	}
	if err := api.getJSON("get_job", map[string]any{"id": jobID}, &job); err != nil {
		return modelsState{}, err
	}
	return modelsState{mc.GetResourceVersion(), len(models), strings.Join(loaded, ","), job.Phase}, nil
}

// dryRunSummary names what a dry run answered: the manifests' kinds, or the
// plan's keys.
func dryRunSummary(answer map[string]any) string {
	if manifests, ok := answer["manifests"].([]any); ok && len(manifests) > 0 {
		var kinds []string
		for _, m := range manifests {
			if obj, ok := m.(map[string]any); ok {
				u := unstructured.Unstructured{Object: obj}
				kinds = append(kinds, u.GetKind()+" "+u.GetName())
			}
		}
		return "manifests " + strings.Join(kinds, ", ")
	}
	keys := slices.DeleteFunc(slices.Sorted(maps.Keys(answer)), func(k string) bool { return k == "manifests" || k == dryRunArg })
	return "plan {" + strings.Join(keys, ", ") + "}"
}

// proveGitOpsOwned makes the wired ModelConfig one Flux applies from git —
// the Kustomization agentlab-gitops-fixture with the ModelConfig in its
// inventory (applyGitOpsFixture), and on the ModelConfig the labels that
// Kustomization stamps, next to model-manager's own — and asserts wire,
// unwire and delete (unwire=true) refuse with gitops_owned, the object and
// the model untouched; the labels and the Kustomization go again on every
// exit path.
func proveGitOpsOwned(api *modelManagerTools, backendName, model, mcName string) error {
	step("A ModelConfig Flux applies from git is never written live: the Kustomization %s/%s lists %s in its inventory, %s labelled %s=%s, then wire, unwire and delete",
		platformNamespace, gitopsFluxFixture, mcName, mcName, gitopsFluxName, gitopsFluxFixture)
	gvr, err := modelConfigGVR()
	if err != nil {
		return err
	}
	wired, err := readModelConfig(mcName)
	if err != nil {
		return err
	}
	removeFixture, err := applyGitOpsFixture(wired)
	if err != nil {
		return err
	}
	defer removeFixture()
	label := func(name, namespace any) error {
		n, _ := json.Marshal(name)
		ns, _ := json.Marshal(namespace)
		body := fmt.Appendf(nil, `{"metadata":{"labels":{%q:%s,%q:%s}}}`, gitopsFluxName, n, gitopsFluxNamespace, ns)
		return patchObject(context.Background(), gvr, kagentNamespace, mcName, types.MergePatchType, body)
	}
	if err := label(gitopsFluxFixture, platformNamespace); err != nil {
		return err
	}
	defer func() {
		if err := label(nil, nil); err != nil {
			note("cleanup: removing the Flux labels from ModelConfig %s: %v", mcName, err)
		}
	}()
	owned, err := readModelConfig(mcName)
	if err != nil {
		return err
	}
	onModel := map[string]any{modelField: model, backendField: backendName}
	deleting := maps.Clone(onModel)
	deleting["unwire"] = true
	for _, c := range []struct {
		tool string
		args map[string]any
	}{{"wire_model", onModel}, {"unwire_model", onModel}, {deleteModel, deleting}} {
		_, err := api.call(c.tool, c.args)
		if refusalCode(err) != "gitops_owned" {
			return fmt.Errorf("%s on the Flux-labelled ModelConfig %s answered %v, wanted gitops_owned", c.tool, mcName, err)
		}
		if !strings.Contains(err.Error(), modeCommit) {
			return fmt.Errorf("%s's gitops_owned refusal does not point at mode commit: %v", c.tool, err)
		}
		note("%s: %s", c.tool, excerpt(err.Error(), 160))
	}
	after, err := readModelConfig(mcName)
	if err != nil {
		return fmt.Errorf("ModelConfig %s after the refusals: %w", mcName, err)
	}
	if after.GetResourceVersion() != owned.GetResourceVersion() {
		return fmt.Errorf("ModelConfig %s changed although every write was refused (resourceVersion %s -> %s)", mcName, owned.GetResourceVersion(), after.GetResourceVersion())
	}
	models, err := api.modelNames("list_models", backendName)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(models, func(n string) bool { return sameModel(n, model) }) {
		return fmt.Errorf("%s is gone from %s although delete_model was refused", model, backendName)
	}
	note("ModelConfig %s untouched (resourceVersion %s), %s still downloaded", mcName, after.GetResourceVersion(), model)
	return nil
}
