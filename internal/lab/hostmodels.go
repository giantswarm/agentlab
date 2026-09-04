package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The inventories of the host model servers, and the static wiring of the
// backends model-manager does not front. model-manager manages one backend
// per instance today; until it is multi-backend (giantswarm/model-manager,
// agentlab#60), every further entry of platform.modelManager.backends gets
// its downloaded tool-calling models rendered as lab-labeled ModelConfigs —
// the shape model-manager itself writes for that backend (Ollama: the native
// keyless provider; Lemonade: the OpenAI provider on its /api/v1) — refreshed
// on every `agentlab platform` run and pruned like extraModels entries.

// HostModel is one model a host model server has downloaded.
type HostModel struct {
	ID string
	// Tools: the model supports tool calling — what agents need, since they
	// send tool schemas with every turn (a model without it fails each turn
	// with "does not support tools").
	Tools bool
}

// The capability names the servers use for tool calling.
const (
	ollamaToolsCapability = "tools"
	lemonadeToolsLabel    = "tool-calling"
)

// hostModelsFn lists a server's downloaded models; a variable so tests can
// substitute an inventory.
var hostModelsFn = hostServerModels

// hostServerModels lists the downloaded models of a backend's server at base
// and whether each one can call tools.
func hostServerModels(backend, base string) ([]HostModel, error) {
	if backend == config.ModelManagerBackendLemonade {
		return lemonadeModels(base)
	}
	return ollamaModels(base)
}

// ollamaModels reads /api/tags and asks /api/show for each model's
// capabilities (Ollama reports `tools` per model, not in the tag list).
func ollamaModels(base string) ([]HostModel, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + "/api/tags")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := decodeJSONBody(resp, &tags); err != nil {
		return nil, fmt.Errorf("reading %s/api/tags: %w", base, err)
	}
	models := make([]HostModel, 0, len(tags.Models))
	for _, m := range tags.Models {
		body, _ := json.Marshal(map[string]string{"model": m.Name})
		show, err := client.Post(base+"/api/show", "application/json", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		var info struct {
			Capabilities []string `json:"capabilities"`
		}
		err = decodeJSONBody(show, &info)
		_ = show.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s/api/show for %s: %w", base, m.Name, err)
		}
		models = append(models, HostModel{ID: m.Name, Tools: slices.Contains(info.Capabilities, ollamaToolsCapability)})
	}
	return models, nil
}

// lemonadeModels reads Lemonade's /api/v1/models — the downloaded models with
// their labels (the catalog needs ?show_all=true, which this does not ask).
func lemonadeModels(base string) ([]HostModel, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + lemonadeModelsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Data []struct {
			ID         string   `json:"id"`
			Downloaded bool     `json:"downloaded"`
			Labels     []string `json:"labels"`
		} `json:"data"`
	}
	if err := decodeJSONBody(resp, &list); err != nil {
		return nil, fmt.Errorf("reading %s%s: %w", base, lemonadeModelsPath, err)
	}
	var models []HostModel
	for _, m := range list.Data {
		if !m.Downloaded {
			continue
		}
		models = append(models, HostModel{ID: m.ID, Tools: slices.Contains(m.Labels, lemonadeToolsLabel)})
	}
	return models, nil
}

// hostBackendModelConfigs synthesizes the ModelConfig entries for the
// backends model-manager does not front: every tool-calling model each
// further host server has downloaded, as the entry model-manager would write
// for it. Entries the user already wired by hand (a platform.extraModels
// entry for the same model on the same host:port, or the same name) are left
// to that entry. Inert when managed models are off.
func hostBackendModelConfigs(cfg *config.Config, endpoints map[string]string) ([]config.ExtraModel, error) {
	if !cfg.ModelManagerEnabled() {
		return nil, nil
	}
	var out []config.ExtraModel
	for _, b := range cfg.Platform.ModelManager.Secondary() {
		endpoint := endpoints[b]
		if endpoint == "" {
			return nil, fmt.Errorf("no endpoint resolved for backend %s", b)
		}
		models, err := hostModelsFn(b, endpoint)
		if err != nil {
			return nil, fmt.Errorf("listing %s's models at %s: %w", config.BackendServerName(b), endpoint, err)
		}
		for _, m := range models {
			if !m.Tools {
				continue
			}
			entry := hostModelConfig(b, endpoint, m.ID)
			if manualEntryCovers(cfg.Platform.ExtraModels, entry) {
				continue
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// hostModelConfig is the ModelConfig entry for one model of a host backend,
// in the shape model-manager writes for that backend.
func hostModelConfig(backend, endpoint, model string) config.ExtraModel {
	entry := config.ExtraModel{Name: hostModelConfigName(backend, model), Model: model}
	if backend == config.ModelManagerBackendLemonade {
		entry.Provider = config.ProviderOpenAI
		entry.BaseURL = endpoint + lemonadeAPIBasePath
	} else {
		entry.Provider = config.ProviderOllama
		entry.BaseURL = endpoint
	}
	return entry
}

// manualEntryCovers reports whether a platform.extraModels entry already
// wires this model on this server (same model id and host:port, whatever the
// path — Lemonade answers on /v1 and /api/v1 alike) or takes the name.
func manualEntryCovers(manual []config.ExtraModel, entry config.ExtraModel) bool {
	for _, m := range manual {
		if m.Name == entry.Name {
			return true
		}
		if m.Model == entry.Model && urlHost(m.BaseURL) == urlHost(entry.BaseURL) {
			return true
		}
	}
	return false
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

var nonNameRunRe = regexp.MustCompile(`[^a-z0-9]+`)

// hostModelConfigName derives the ModelConfig name for a host backend's model:
// <backend>-<model id sanitized to lowercase alphanumerics and dashes>, e.g.
// lemonade-qwen3-it-4b-flm, ollama-qwen3-5-9b. Prefixed so the entries of
// several servers never collide with each other or with model-manager's own
// (which uses the bare sanitized name).
func hostModelConfigName(backend, model string) string {
	s := nonNameRunRe.ReplaceAllString(strings.ToLower(model), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "model"
	}
	return backend + "-" + s
}
