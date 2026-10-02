package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// models-test's GitOps half (docs/models.md "Dry runs, GitOps-owned objects
// and commit mode"): every write tool's dry run changes nothing, a ModelConfig
// Flux applies from git is never written live (gitops_owned), and wire_model
// in mode commit opens a pull request as the person whose files are the dry
// run's.
//
// Commit mode needs model-manager pinned to a GitHub App (the chart's
// github.enabled): the bearer of every call is then the person's App user
// token, which the lab's own model-manager — forwarded the Dex token, the
// shape every other proof and the portal use — does not carry. So the commit
// proof runs a second, temporary release, model-manager-gitops — a copy of
// the platform's model-manager HelmRelease with the lab's post-renderers —
// pinned to the lab Dex as its
// authorization server (the sign-in completes headlessly, as the OAuth
// fixture's does) and to the fake GitHub API (githubfake.go) as its GitHub,
// signs the person in to it through muster, and uninstalls it afterwards.
// Its auto-wire is off: the platform's model-manager keeps the wiring.

// The commit proof's names: the release (also its fullname, Service and
// MCPServer, so its tools are x_model-manager-gitops_*), the fake's Service,
// and the repository the fake holds.
const (
	gitopsModelManager    = "model-manager-gitops"
	githubFakeService     = "agentlab-github-api"
	githubFakeServiceHost = "http://" + githubFakeService + "." + platformNamespace + ".svc.cluster.local"
	// gitopsAppIssuer is the pinned App's issuer identity: the key muster
	// files the person's grant under (apart from every other grant) and the
	// name model-manager's refusals give the consent. muster's CRD takes an
	// https URL; nothing fetches it — the endpoints are pinned — so it names
	// the fake's App on a host nothing resolves.
	gitopsAppIssuer  = "https://github.agentlab.invalid/apps/" + gitopsModelManager
	gitopsRepository = "agentlab/gitops"
	gitopsBranch     = "main"
	gitopsPath       = "clusters/agentlab/kagent"
	// gitopsAgeRecipient is the age recipient the fake repository's
	// .sops.yaml encrypts for: a public key whose private half was never
	// kept, so a placeholder Secret the commit writes (the OpenAI-provider
	// wirings) is encrypted as on a real repository and decrypted nowhere.
	gitopsAgeRecipient = "age13z6rzrnc2xd009m6l94jz2q734dlx8r33p0jj0rpzxl20ndhpvkqe4q4ax"
	// gitopsFluxLabel is the Flux provenance the gitops_owned fixture puts
	// on the wired ModelConfig: what a Kustomization stamps on every object
	// it applies.
	gitopsFluxName      = "kustomize.toolkit.fluxcd.io/name"
	gitopsFluxNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	gitopsFluxFixture   = "agentlab-gitops-fixture"
	// gitopsHelmName/Namespace are the labels a HelmRelease stamps on what it
	// renders; gitopsStaleRelease is a release that never exists.
	gitopsHelmName      = "helm.toolkit.fluxcd.io/name"
	gitopsHelmNamespace = "helm.toolkit.fluxcd.io/namespace"
	gitopsStaleRelease  = "agentlab-gone-release"
	gitopsReleaseWait   = 5 * time.Minute
)

// model-manager's argument names and words the proof uses.
const (
	dryRunArg     = "dryRun"
	modeArg       = "mode"
	modeCommit    = "commit"
	repositoryArg = "repository"
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
	// GitHubFakeBinary is the static Linux agentlab the fake GitHub API
	// container runs (default: this binary).
	GitHubFakeBinary string
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
	mc, err := readKagentObject(modelConfigResource, mcName)
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

// proveGitOpsOwned puts Flux provenance on the wired ModelConfig — the
// labels a Kustomization stamps, next to model-manager's own — and asserts
// wire, unwire and delete (unwire=true) refuse with gitops_owned, the object
// and the model untouched; the labels go again on every exit path.
func proveGitOpsOwned(api *modelManagerTools, backendName, model, mcName string) error {
	step("A ModelConfig Flux applies from git is never written live: %s labelled %s=%s, then wire, unwire and delete", mcName, gitopsFluxName, gitopsFluxFixture)
	gvr, err := gvrFor(modelConfigResource)
	if err != nil {
		return err
	}
	label := func(value any) error {
		v, _ := json.Marshal(value)
		body := fmt.Appendf(nil, `{"metadata":{"labels":{%q:%s,%q:%s}}}`, gitopsFluxName, v, gitopsFluxNamespace, v)
		return patchObject(context.Background(), gvr, kagentNamespace, mcName, types.MergePatchType, body)
	}
	if err := label(gitopsFluxFixture); err != nil {
		return err
	}
	defer func() {
		if err := label(nil); err != nil {
			note("cleanup: removing the Flux labels from ModelConfig %s: %v", mcName, err)
		}
	}()
	owned, err := readKagentObject(modelConfigResource, mcName)
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
	after, err := readKagentObject(modelConfigResource, mcName)
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

// gitopsSeed is the fake repository's base branch: the directory the
// commit targets with its kustomization.yaml, and the .sops.yaml a Secret
// file is encrypted for.
func gitopsSeed() map[string][]byte {
	return map[string][]byte{
		gitopsPath + "/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"),
		".sops.yaml":                       []byte("creation_rules:\n  - path_regex: .*\n    encrypted_regex: ^(data|stringData)$\n    age: " + gitopsAgeRecipient + "\n"),
	}
}

// proveCommit is the commit proof: the fake GitHub on the kind network, the
// App-pinned model-manager-gitops, the person signed in to it through muster,
// wire_model mode commit as a dry run and for real against the fake's
// repository, and the pull request read back from the fake — opened as the
// person, its files the dry run's, nothing written live. The pinned release
// is a model-manager instance of its own, and only the instance that created
// a ModelConfig writes it, so the caller unwires the platform's ModelConfig
// first and wires it again after.
func proveCommit(cfg *config.Config, user *config.User, token, binary, backendName, model, mcName string) (string, error) {
	step("Commit mode: the fake GitHub API as a container on the %s network (%s %s, in %s) holding %s@%s",
		kindDockerNetwork, binary, githubFakeCommand, probeImage, gitopsRepository, gitopsBranch)
	fake, err := startGitHubFakeContainer(cfg, binary, gitopsRepository, gitopsBranch, gitopsSeed())
	if err != nil {
		return "", err
	}
	defer fake.close()
	removeService, err := fakeServiceForPods(cfg, githubFakeService, "the fake GitHub API", fake.podIP, fakeContainerPort, githubFakeServiceHost+githubFakeHealthPath)
	if err != nil {
		return "", err
	}
	defer removeService()

	step("Applying HelmRelease %s: a copy of the platform's model-manager release, pinned to the lab Dex as its GitHub App and to %s%s as its GitHub API, auto-wire off", gitopsModelManager, githubFakeServiceHost, githubFakeAPIPath)
	uninstall, err := installGitOpsModelManager(cfg, fake.podIP)
	if err != nil {
		return "", err
	}
	defer uninstall()
	if err := waitMCPServerState(gitopsModelManager, mcpServerStateAuthRequired, mcpServerStateConnected); err != nil {
		return "", err
	}

	step("Signing %s in to %s through muster (core_auth_login, the Dex login form, muster's proxy callback)", user.Email, gitopsModelManager)
	session, err := openMusterSession(cfg, token, "models-test-gitops")
	if err != nil {
		return "", err
	}
	defer func() {
		_, _ = session.callToolEnvelope("core_auth_logout", map[string]any{serverKey: gitopsModelManager})
	}()
	challenge, err := signInChallenge(cfg, session, gitopsModelManager)
	if err != nil {
		return "", err
	}
	if err := completeSignIn(challenge.authURL, user); err != nil {
		return "", err
	}
	note("signed in: muster holds the pinned server's token for the session")
	api := &modelManagerTools{session: session, server: gitopsModelManager}

	var backend struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := api.getJSON("get_backend", map[string]any{backendField: backendName}, &backend); err != nil {
		return "", err
	}
	if !backend.Capabilities[modeCommit] {
		return "", fmt.Errorf("%s reports capabilities.commit=false on %s although it is pinned: %v", gitopsModelManager, backendName, backend.Capabilities)
	}
	note("%s: capabilities.commit=true", api.toolName("get_backend"))

	if _, err := readKagentObject(modelConfigResource, mcName); !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("ModelConfig %s is live before the commit (%v): the platform's model-manager unwires it first", mcName, err)
	}
	login, _, _ := strings.Cut(user.Email, "@")
	args := map[string]any{modelField: model, backendField: backendName, modeArg: modeCommit,
		repositoryArg: gitopsRepository, "branch": gitopsBranch, "path": gitopsPath}
	step("%s mode commit, dryRun: the files it would commit to %s@%s under %s", api.toolName("wire_model"), gitopsRepository, gitopsBranch, gitopsPath)
	dry, err := wireCommit(api, args, true)
	if err != nil {
		return "", err
	}
	if dry.Author != login || dry.PullRequest != "" || len(dry.Files) == 0 {
		return "", fmt.Errorf("the commit dry run answered author %q (wanted %q), pull request %q (wanted none), %d files", dry.Author, login, dry.PullRequest, len(dry.Files))
	}
	for _, f := range dry.Files {
		note("%s %s", f.Action, f.Path)
	}
	if pulls, err := fake.pulls(); err != nil || len(pulls) != 0 {
		return "", fmt.Errorf("the dry run reached the fake GitHub: %d pull requests (%v)", len(pulls), err)
	}

	step("%s mode commit: the pull request, opened as %s", api.toolName("wire_model"), login)
	done, err := wireCommit(api, args, false)
	if err != nil {
		return "", err
	}
	if done.PullRequest == "" || done.Number == 0 || done.Author != login {
		return "", fmt.Errorf("the commit answered pull request %q #%d as %q, wanted one opened as %q", done.PullRequest, done.Number, done.Author, login)
	}
	pulls, err := fake.pulls()
	if err != nil {
		return "", err
	}
	if len(pulls) != 1 {
		return "", fmt.Errorf("the fake GitHub holds %d pull requests, wanted the one", len(pulls))
	}
	pr := pulls[0]
	if pr.Number != done.Number || pr.Author != login || pr.Base != gitopsBranch || pr.Head != done.Branch {
		return "", fmt.Errorf("the fake's pull request is #%d %s -> %s by %q, the tool answered #%d %s -> %s by %q", pr.Number, pr.Head, pr.Base, pr.Author, done.Number, done.Branch, gitopsBranch, login)
	}
	if err := sameCommitFiles(dry.Files, pr); err != nil {
		return "", err
	}
	note("#%d %q by %s: %d files, byte-identical to the dry run", pr.Number, pr.Title, pr.Author, len(pr.Files))
	if _, err := readKagentObject(modelConfigResource, mcName); !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("ModelConfig %s is live after the commit (%v): commit mode writes git only", mcName, err)
	}
	note("no ModelConfig %s live: commit mode writes git only", mcName)
	if err := proveStaleProvenance(api, backendName, model); err != nil {
		return "", err
	}
	return fmt.Sprintf("wire_model mode commit as %s -> pull request #%d on the fake GitHub (%s -> %s, %d files, equal to the dry run), nothing written live; a stale namespace provenance answers invalid_request",
		login, pr.Number, pr.Head, pr.Base, len(pr.Files)), nil
}

// proveStaleProvenance gives the kagent namespace the Flux labels of a
// HelmRelease that does not exist — what a namespace left behind by a removed
// release carries — and asserts a commit following the namespace's provenance
// (no explicit target) answers invalid_request naming the gone release, before
// anything is written. The namespace's own labels come back on every exit path.
func proveStaleProvenance(api *modelManagerTools, backendName, model string) error {
	step("A namespace whose Flux owner is gone: %s labelled %s=%s, %s=%s, then %s mode commit without a target", kagentNamespace, gitopsHelmName, gitopsStaleRelease, gitopsHelmNamespace, gitopsFluxFixture, api.toolName("wire_model"))
	gvr := corev1.SchemeGroupVersion.WithResource("namespaces")
	ns, err := getObject(context.Background(), gvr, "", kagentNamespace)
	if err != nil {
		return err
	}
	own := ns.GetLabels()
	label := func(values map[string]any) error {
		body, err := json.Marshal(map[string]any{crMetadata: map[string]any{"labels": values}})
		if err != nil {
			return err
		}
		return patchObject(context.Background(), gvr, "", kagentNamespace, types.MergePatchType, body)
	}
	restore := map[string]any{}
	for _, key := range []string{gitopsFluxName, gitopsFluxNamespace, gitopsHelmName, gitopsHelmNamespace} {
		restore[key] = nil
		if v, ok := own[key]; ok {
			restore[key] = v
		}
	}
	if err := label(map[string]any{gitopsFluxName: nil, gitopsFluxNamespace: nil, gitopsHelmName: gitopsStaleRelease, gitopsHelmNamespace: gitopsFluxFixture}); err != nil {
		return err
	}
	defer func() {
		if err := label(restore); err != nil {
			note("cleanup: restoring the Flux labels of namespace %s: %v", kagentNamespace, err)
		}
	}()
	_, err = api.call("wire_model", map[string]any{modelField: model, backendField: backendName, modeArg: modeCommit, dryRunArg: true})
	if refusalCode(err) != "invalid_request" {
		return fmt.Errorf("wire_model mode commit on namespace %s labelled by the gone HelmRelease %s/%s answered %v, wanted invalid_request", kagentNamespace, gitopsFluxFixture, gitopsStaleRelease, err)
	}
	for _, want := range []string{"HelmRelease " + gitopsFluxFixture + "/" + gitopsStaleRelease, "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			return fmt.Errorf("the stale-provenance refusal does not say %q: %v", want, err)
		}
	}
	note("wire_model: %s", excerpt(err.Error(), 200))
	return nil
}

// commitAnswer is the commit part of a wire in mode commit.
type commitAnswer struct {
	Branch      string `json:"branch"`
	PullRequest string `json:"pullRequest"`
	Number      int    `json:"number"`
	Author      string `json:"author"`
	Files       []struct {
		Path    string `json:"path"`
		Action  string `json:"action"`
		Content string `json:"content"`
	} `json:"files"`
}

func wireCommit(api *modelManagerTools, args map[string]any, dryRun bool) (*commitAnswer, error) {
	call := maps.Clone(args)
	call[dryRunArg] = dryRun
	var answer struct {
		Commit *commitAnswer `json:"commit"`
		Next   string        `json:"next"`
	}
	if err := api.getJSON("wire_model", call, &answer); err != nil {
		return nil, err
	}
	if answer.Commit == nil {
		return nil, fmt.Errorf("wire_model mode commit (dryRun %v) answered no commit: %s", dryRun, answer.Next)
	}
	return answer.Commit, nil
}

// sameCommitFiles asserts the pull request's head writes exactly the dry
// run's files — the content the dry run showed, byte for byte; a secret
// file (no content shown) SOPS-encrypted — and removes the ones it removes.
func sameCommitFiles(dry []struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content"`
}, pr githubFakePullView) error {
	written := map[string]bool{}
	for _, f := range dry {
		if f.Action == "delete" {
			if !slices.Contains(pr.Removed, f.Path) {
				return fmt.Errorf("the dry run removes %s, the pull request keeps it", f.Path)
			}
			continue
		}
		written[f.Path] = true
		got, ok := pr.Files[f.Path]
		switch {
		case !ok:
			return fmt.Errorf("the dry run writes %s, the pull request does not", f.Path)
		case f.Content == "" && !strings.Contains(got, "sops:"):
			return fmt.Errorf("%s is a secret file the dry run hides, and the pull request carries it unencrypted", f.Path)
		case f.Content != "" && got != f.Content:
			return fmt.Errorf("%s differs from the dry run:\n--- dry run\n%s\n--- pull request\n%s", f.Path, f.Content, got)
		}
	}
	for p := range pr.Files {
		if !written[p] {
			return fmt.Errorf("the pull request writes %s, which the dry run did not name", p)
		}
	}
	return nil
}

// installGitOpsModelManager applies model-manager-gitops as a copy of the
// platform's model-manager HelmRelease — its chart, values and the lab's
// post-renderers (the dex-localhost sidecar, a dev image), retargeted at the
// copy's Deployment — pinned to Dex and the fake, and waits for it Ready;
// the returned func deletes it, and helm-controller uninstalls the release.
func installGitOpsModelManager(cfg *config.Config, fakeIP string) (func(), error) {
	gvr, err := gvrFor(helmReleaseResource)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitopsReleaseWait+time.Minute)
	defer cancel()
	platform, err := getObject(ctx, gvr, platformNamespace, modelManagerMCPServer)
	if err != nil {
		return nil, fmt.Errorf("the platform's model-manager HelmRelease: %w", err)
	}
	copied, err := gitopsHelmRelease(cfg, platform.Object, fakeIP)
	if err != nil {
		return nil, err
	}
	remove := func() {
		if err := deleteObject(context.Background(), gvr, platformNamespace, gitopsModelManager, gitopsReleaseWait); err != nil {
			note("cleanup: deleting the HelmRelease %s: %v", gitopsModelManager, err)
		}
	}
	remove() // a leftover of an aborted run
	if _, err := applyManifests(ctx, copied); err != nil {
		return nil, err
	}
	if err := waitCondition(ctx, gvr, platformNamespace, gitopsModelManager, "Ready", "True", gitopsReleaseWait); err != nil {
		gitopsPodDiagnosis()
		remove()
		return nil, fmt.Errorf("HelmRelease %s/%s not Ready: %w", platformNamespace, gitopsModelManager, err)
	}
	note("HelmRelease %s Ready (the chart and values of %s, its post-renderers)", gitopsModelManager, modelManagerMCPServer)
	return remove, nil
}

// helmReleaseResource is Flux's HelmRelease, as gvrFor takes it.
const helmReleaseResource = "helmreleases.helm.toolkit.fluxcd.io"

// gitopsHelmRelease is the copy of the platform's model-manager HelmRelease
// (its object) as a manifest: the release named model-manager-gitops, the
// values gitopsModelManagerValues makes, the post-renderers' Deployment
// target and patch renamed.
func gitopsHelmRelease(cfg *config.Config, platform map[string]any, fakeIP string) ([]byte, error) {
	raw, err := json.Marshal(platform["spec"])
	if err != nil {
		return nil, err
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	// The post-renderers name the Deployment in their target and in the
	// patch's metadata; the copy's Deployment is the release's fullname. Its
	// container keeps the chart's name, and the chart reference (the
	// OCIRepository) stays the platform's.
	if renderers, ok := spec["postRenderers"]; ok {
		raw, err := json.Marshal(renderers)
		if err != nil {
			return nil, err
		}
		raw = []byte(strings.NewReplacer(
			`"`+nameKey+`":"`+modelManagerMCPServer+`"`, `"`+nameKey+`":"`+gitopsModelManager+`"`,
			`metadata:\n  name: `+modelManagerMCPServer+`\n`, `metadata:\n  name: `+gitopsModelManager+`\n`,
		).Replace(string(raw)))
		var renamed any
		if err := json.Unmarshal(raw, &renamed); err != nil {
			return nil, err
		}
		spec["postRenderers"] = renamed
	}
	values, _ := spec["values"].(map[string]any)
	spec["values"] = gitopsModelManagerValues(cfg, values, fakeIP)
	spec["releaseName"] = gitopsModelManager
	src := unstructured.Unstructured{Object: platform}
	out := unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	out.SetAPIVersion(src.GetAPIVersion())
	out.SetKind(src.GetKind())
	out.SetName(gitopsModelManager)
	out.SetNamespace(platformNamespace)
	out.SetLabels(map[string]string{managedByLabel: managedByAgentlabValue})
	return json.Marshal(out.Object)
}

// gitopsModelManagerValues are the platform release's values with the
// release's own name, auto-wire off, no route, and the GitHub pin: Dex's
// endpoints with the platform client (the OAuth fixture's Secret, whose
// client lists muster's proxy callback) as the App, the fake as the API. The
// issuer is gitopsAppIssuer.
func gitopsModelManagerValues(cfg *config.Config, platformValues map[string]any, fakeIP string) map[string]any {
	overrides := map[string]any{
		"fullnameOverride": gitopsModelManager,
		"kagent":           map[string]any{"autoWire": false},
		"httpRoute":        map[string]any{valuesEnabled: false},
		"github": map[string]any{
			valuesEnabled: true,
			"apiURL":      githubFakeServiceHost + githubFakeAPIPath,
			"authorizationServer": map[string]any{
				"issuer":                     gitopsAppIssuer,
				"expectedIssuer":             "",
				"authorizationEndpoint":      cfg.Issuer() + "/auth",
				"tokenEndpoint":              cfg.Issuer() + "/token",
				"scopes":                     "openid profile email offline_access",
				"clientCredentialsSecretRef": map[string]any{nameKey: oauthFixtureServer + "-client", namespaceKey: platformNamespace},
			},
		},
		musterValues: map[string]any{"mcpServer": map[string]any{valuesEnabled: true, nameKey: gitopsModelManager,
			"description": "agentlab models-test: model-manager pinned to the fake GitHub, for the commit proof (temporary)"}},
	}
	if np, _ := platformValues["networkPolicy"].(map[string]any); np[valuesEnabled] == true {
		cidrs, _ := np["egressCIDRs"].([]any)
		overrides["networkPolicy"] = map[string]any{"egressCIDRs": append(slices.Clone(cidrs), fakeIP+"/32")}
	}
	if platformValues == nil {
		return overrides
	}
	return chartutil.MergeTables(overrides, platformValues)
}

// gitopsPodDiagnosis prints the release's pods and the tail of their logs:
// why an install did not become ready, read before the uninstall removes it.
func gitopsPodDiagnosis() {
	k, err := labKube()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	pods, err := k.clientset.CoreV1().Pods(platformNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + gitopsModelManager})
	if err != nil {
		return
	}
	tail := int64(20)
	for _, p := range pods.Items {
		note("pod %s: %s, restarts %d", p.Name, podStatus(&p), podRestarts(&p))
		raw, err := k.clientset.CoreV1().Pods(platformNamespace).GetLogs(p.Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
		if err == nil {
			note("its log:\n%s", strings.TrimSpace(string(raw)))
		}
	}
}
