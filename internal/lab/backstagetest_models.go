package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The Models half of the portal proof: the Dev Portal's Models and Serving
// pages reach model-manager through the installation's muster as the
// signed-in person (x_model-manager_* tools over POST /api/muster/call, the
// hop agent-manager's calls take); the portal has no model-manager REST
// proxy. One read — list_backends, the Serving page's first — and, on a host
// backend with a downloaded model, one write: load_model and unload_model,
// the model left as it was found. Both are attributed at muster to the
// person, the write also in model-manager's own log. A viewer's write is
// refused: wire_model, the ModelConfig model-manager writes as the caller (a
// host server's load and unload touch no Kubernetes object, so no RBAC
// stands in front of them).

// modelManagerToolPrefix is the aggregated prefix of model-manager's tools.
const modelManagerToolPrefix = "x_" + modelManagerMCPServer + "_"

// model-manager's log messages for a load and an unload; each line carries
// caller=<email> for a call made as a person.
const (
	modelManagerLoggedLoad   = `msg="model loaded"`
	modelManagerLoggedUnload = `msg="model unloaded"`
)

// portalModelInfo is one entry of list_models: the name, the size and
// whether the backend holds it in memory and kagent has a ModelConfig for it.
type portalModelInfo struct {
	Name        string `json:"name"`
	SizeBytes   int64  `json:"sizeBytes"`
	Loaded      bool   `json:"loaded"`
	ModelConfig *struct {
		Name string `json:"name"`
	} `json:"modelConfig"`
}

// portalModelPlan pins the inventory selection announced before the proof.
// Loaded state is read again at the write, so the model is still left as found.
type portalModelPlan struct {
	backend string
	model   portalModelInfo
	skip    string
}

func (p portalModelPlan) String() string {
	if p.skip != "" {
		return "no host model: the Models write is skipped (" + p.skip + ")"
	}
	return fmt.Sprintf("%s on %s, %s (%d bytes from list_models); load + unload; skip with --skip-model-write (inspect only with --plan)",
		p.model.Name, p.backend, humanBytes(p.model.SizeBytes), p.model.SizeBytes)
}

// backstageModelPlan only signs in and reads inventory; it never loads a model.
func backstageModelPlan(cfg *config.Config, emails []string, opts BackstageTestOptions) (portalModelPlan, error) {
	if opts.SkipModelWrite {
		return portalModelPlan{skip: "--skip-model-write"}, nil
	}
	if !cfg.ModelManagerEnabled() {
		return portalModelPlan{skip: "model-manager is off"}, nil
	}
	backend := hostBackend(cfg.ChartBackends())
	if backend == "" {
		return portalModelPlan{skip: "the lab runs no host model server"}, nil
	}
	for _, email := range emails {
		user := cfg.FindUser(email)
		if user == nil {
			return portalModelPlan{}, fmt.Errorf("no user %q in %s", email, config.File)
		}
		if !user.HasGroup(platformAdminsGroup) {
			continue
		}
		ps, err := backstageLogin(cfg, user)
		if err != nil {
			return portalModelPlan{}, fmt.Errorf("host model plan: %w", err)
		}
		return readPortalModelPlan(ps, backend)
	}
	return portalModelPlan{skip: "no platform-admins user selected"}, nil
}

func readPortalModels(ps *portalSession, backend string) ([]portalModelInfo, error) {
	var inventory struct {
		Models []portalModelInfo `json:"models"`
	}
	if err := portalMusterCall(ps, modelManagerToolPrefix+"list_models", map[string]any{backendField: backend}, &inventory); err != nil {
		return nil, err
	}
	return inventory.Models, nil
}

func readPortalModelPlan(ps *portalSession, backend string) (portalModelPlan, error) {
	models, err := readPortalModels(ps, backend)
	if err != nil {
		return portalModelPlan{}, fmt.Errorf("host model plan: %w", err)
	}
	spec, _ := backendSpec(backend)
	model, ok := proofModelOf(models, spec.proofModel)
	if !ok {
		return portalModelPlan{skip: backend + " holds no downloaded model"}, nil
	}
	return portalModelPlan{backend: backend, model: model}, nil
}

// currentModel refuses a changed inventory rather than silently loading a
// different model. Its current loaded state determines the restoration order.
func (p portalModelPlan) currentModel(ps *portalSession) (portalModelInfo, error) {
	models, err := readPortalModels(ps, p.backend)
	if err != nil {
		return portalModelInfo{}, err
	}
	for _, model := range models {
		if model.Name == p.model.Name && model.SizeBytes == p.model.SizeBytes {
			return model, nil
		}
	}
	return portalModelInfo{}, fmt.Errorf("host model inventory changed since the plan; rerun backstage-test --plan before loading a model")
}

// proveModelsPage drives the Models page's calls through the portal as the
// primary user, the viewer's write as the boundary. Returns the verdict
// lines.
func proveModelsPage(cfg *config.Config, primary, viewer *portalSession, plan portalModelPlan) ([]string, error) {
	var verdicts []string
	email := primary.user.Email
	subject := stringAt(primary.claims, claimSubject)

	step("The Serving page's first read: %slist_backends through the portal as %s", modelManagerToolPrefix, email)
	started := time.Now()
	var listed struct {
		Backends []struct {
			Backend string `json:"backend"`
			Version string `json:"version"`
		} `json:"backends"`
	}
	if err := portalMusterCall(primary, modelManagerToolPrefix+"list_backends", nil, &listed); err != nil {
		return verdicts, err
	}
	names := make([]string, 0, len(listed.Backends))
	for _, b := range listed.Backends {
		names = append(names, b.Backend)
	}
	if missing := missingBackends(cfg.ChartBackends(), names); len(missing) > 0 {
		return verdicts, fmt.Errorf("list_backends through the portal lists %v, missing %v of the lab's %v", names, missing, cfg.ChartBackends())
	}
	if err := musterAttributes(started, email, subject, 1); err != nil {
		return verdicts, err
	}
	note("backends %v; muster attributes the call to %s", names, email)
	verdicts = append(verdicts, fmt.Sprintf("PASS: the Serving page's read (POST /api/muster/call %slist_backends as %s) lists %v, attributed at muster to %s",
		modelManagerToolPrefix, email, names, email))

	if plan.skip != "" {
		return append(verdicts, "SKIP: "+plan.String()), nil
	}
	backend := plan.backend
	model, err := plan.currentModel(primary)
	if err != nil {
		return verdicts, err
	}

	order := writeTools(model.Loaded)
	step("The Models page's write: %s of %s on %s through the portal as %s (loaded before: %v)", strings.Join(order, " + "), model.Name, backend, email, model.Loaded)
	started = time.Now()
	args := map[string]any{backendField: backend, modelField: model.Name}
	for _, tool := range order {
		if err := portalMusterCall(primary, tool, args, nil); err != nil {
			return verdicts, err
		}
		note("%s answered", tool)
	}
	if model.ModelConfig == nil {
		// The load wired the model (model-manager's auto-wire); a model that
		// had no ModelConfig is left without one.
		if err := portalMusterCall(primary, modelManagerToolPrefix+"unwire_model", args, nil); err != nil && !strings.Contains(err.Error(), "not_found") {
			return verdicts, err
		}
	}
	if err := musterAttributes(started, email, subject, len(order)); err != nil {
		return verdicts, err
	}
	if err := modelManagerLoggedWrites(started, email, model.Name); err != nil {
		return verdicts, err
	}
	note("muster attributes the calls to %s; model-manager logged the load and the unload with caller=%s", email, email)
	verdicts = append(verdicts, fmt.Sprintf("PASS: the Models page's write (POST /api/muster/call %s of %s (%s) on %s as %s) answers, attributed at muster and in model-manager's log (caller=%s); the model is left %s",
		strings.Join(order, " + "), model.Name, humanBytes(model.SizeBytes), backend, email, email, loadedWord(model.Loaded)))

	if viewer == nil {
		return verdicts, nil
	}
	step("%s's write refused: %swire_model of %s on %s through the portal", viewer.user.Email, modelManagerToolPrefix, model.Name, backend)
	err = portalMusterCall(viewer, modelManagerToolPrefix+"wire_model", args, nil)
	switch {
	case err == nil:
		return verdicts, fmt.Errorf("%s wired %s through the portal although the view role writes no ModelConfigs — model-manager is not writing as the caller", viewer.user.Email, model.Name)
	case !strings.Contains(err.Error(), refusalForbidden):
		return verdicts, fmt.Errorf("%s: wanted `%s …`, got: %w", viewer.user.Email, refusalForbidden, err)
	}
	note("%s: %s", viewer.user.Email, excerpt(err.Error(), 200))
	verdicts = append(verdicts, fmt.Sprintf("PASS: %s's wire_model through the portal answers `%s …` (the ModelConfig write is the person's RBAC, not model-manager's ServiceAccount's)", viewer.user.Email, refusalForbidden))
	return verdicts, nil
}

// missingBackends is the lab's backends list_backends does not list.
func missingBackends(want, listed []string) []string {
	var missing []string
	for _, b := range want {
		if !slices.Contains(listed, b) {
			missing = append(missing, b)
		}
	}
	return missing
}

// hostBackend is the first host model server among the chart's backends, ""
// when the lab fronts none (serving alone).
func hostBackend(backends []string) string {
	for _, b := range backends {
		if _, ok := backendSpec(b); ok {
			return b
		}
	}
	return ""
}

// proofModelOf picks the write's model from the backend's inventory: the
// backend's proof model when it is downloaded, the smallest model otherwise.
func proofModelOf(models []portalModelInfo, preferred string) (portalModelInfo, bool) {
	var picked portalModelInfo
	found := false
	for _, m := range models {
		switch {
		case m.Name == "":
			continue
		case sameModel(m.Name, preferred):
			return m, true
		case !found || m.SizeBytes < picked.SizeBytes:
			picked, found = m, true
		}
	}
	return picked, found
}

// writeTools is the write's model-manager tools in call order, each named in
// full: load then unload, or unload then load for a loaded model, so the
// model is left as it was found.
func writeTools(loaded bool) []string {
	order := []string{loadModel, unloadModel}
	if loaded {
		order = []string{unloadModel, loadModel}
	}
	for i, tool := range order {
		order[i] = modelManagerToolPrefix + tool
	}
	return order
}

// loadedWord says how the write left the model.
func loadedWord(loaded bool) string {
	if loaded {
		return "loaded"
	}
	return "unloaded"
}

// musterAttributes checks muster's log since the instant for at least n
// tools/call requests by the person's subject and the forwarded id_token
// accepted for the email: the portal's hop as the person, not as Backstage.
func musterAttributes(since time.Time, email, subject string, n int) error {
	calls, err := musterCallsSince(since, email, subject)
	if err != nil {
		return err
	}
	if calls.accepted == 0 || calls.calls < n {
		return fmt.Errorf("muster's log since %s attributes %d tools/call requests to %s's subject (wanted %d) and accepted %d forwarded id_tokens for %s",
			since.Format(time.RFC3339), calls.calls, email, n, calls.accepted, email)
	}
	return nil
}

// modelManagerLoggedWrites checks model-manager's log for the load and the
// unload of the model attributed to the person since the instant.
func modelManagerLoggedWrites(since time.Time, email, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	logs, err := podLogs(ctx, platformNamespace, "deploy/"+modelManagerMCPServer, modelManagerMCPServer, time.Since(since)+5*time.Second)
	if err != nil {
		return fmt.Errorf("reading model-manager's log: %w", err)
	}
	for _, msg := range []string{modelManagerLoggedLoad, modelManagerLoggedUnload} {
		if !loggedFor(logs, msg, model, email) {
			return fmt.Errorf("model-manager's log carries no `%s model=%s … caller=%s` line", msg, model, email)
		}
	}
	return nil
}

// loggedFor reports whether a line of a text-format log carries the message,
// the model and the caller.
func loggedFor(logs, msg, model, email string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, msg) && strings.Contains(line, " model="+model+" ") && strings.Contains(line, " caller="+email) {
			return true
		}
	}
	return false
}
