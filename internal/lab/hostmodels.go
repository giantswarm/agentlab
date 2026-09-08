package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// The inventories of the host model servers: what each has downloaded and
// whether it can call tools — the discovery's summary, and the ground truth
// models-test checks a delete against. Nothing here wires models: one
// model-manager fronts every server of platform.modelManager.backends and
// writes their ModelConfigs itself.

// HostModel is one model a host model server has downloaded.
type HostModel struct {
	ID string
	// Tools: the model supports tool calling — what agents need, since they
	// send tool schemas with every turn (a model without it fails each turn
	// with "does not support tools").
	Tools bool
	// Size on disk in bytes (Lemonade reports decimal GB; converted).
	Size int64
}

// The capability names the servers use for tool calling.
const (
	ollamaToolsCapability = "tools"
	lemonadeToolsLabel    = "tool-calling"
	// LM Studio reports tool calling as the model's training. It accepts
	// `tools` for any model and emulates them through the prompt, but only a
	// model trained for them calls them reliably, which is what an agent
	// needs.
	//
	// Its model types are llm, vlm (vision-language) and embedding. Only the
	// embedding one cannot serve an agent, so the inventory excludes THAT
	// rather than keeping only `llm`: a vlm is a chat model, and keying on
	// llm dropped it from the count and from models-test's ground truth.
	// Both spellings, since /api/v0 said embeddings.
	lmStudioEmbeddingType  = "embedding"
	lmStudioEmbeddingsType = "embeddings"
)

// hostModelsFn lists a server's downloaded models; a variable so tests can
// substitute an inventory.
var hostModelsFn = hostServerModels

// hostServerModels lists the downloaded models of a backend's server at base
// and whether each one can call tools. An unknown kind is an error rather
// than an Ollama read: config.Validate rejects one when the file loads, so
// reaching this with one is a bug worth seeing.
func hostServerModels(backend, base string) ([]HostModel, error) {
	spec, known := backendSpec(backend)
	if !known {
		return nil, fmt.Errorf("unknown host model server backend %q", backend)
	}
	return spec.models(base)
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
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := decodeJSONBody(resp, &tags); err != nil {
		return nil, fmt.Errorf("reading %s/api/tags: %w", base, err)
	}
	models := make([]HostModel, 0, len(tags.Models))
	for _, m := range tags.Models {
		body, _ := json.Marshal(map[string]string{modelField: m.Name})
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
		models = append(models, HostModel{ID: m.Name, Tools: slices.Contains(info.Capabilities, ollamaToolsCapability), Size: m.Size})
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
			Size       float64  `json:"size"` // decimal GB
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
		models = append(models, HostModel{ID: m.ID, Tools: slices.Contains(m.Labels, lemonadeToolsLabel), Size: int64(m.Size * 1e9)})
	}
	return models, nil
}

// lmStudioModels reads LM Studio's /api/v1/models (0.4.0+): its library, so
// every entry is downloaded. Everything but an embedding model can serve an
// agent — a vlm is a chat model with vision, and embedding entries carry no
// capability object at all — and tool calling is the model's
// training, which LM Studio reports directly, so there is no second request
// per model as on Ollama. Size is already bytes here: no conversion, unlike
// Lemonade's decimal GB above.
func lmStudioModels(base string) ([]HostModel, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + lmStudioModelsPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Models []struct {
			Key          string `json:"key"`
			Type         string `json:"type"`
			SizeBytes    int64  `json:"size_bytes"`
			Capabilities *struct {
				TrainedForToolUse bool `json:"trained_for_tool_use"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if err := decodeJSONBody(resp, &list); err != nil {
		return nil, fmt.Errorf("reading %s%s: %w", base, lmStudioModelsPath, err)
	}
	var models []HostModel
	for _, m := range list.Models {
		if m.Type == lmStudioEmbeddingType || m.Type == lmStudioEmbeddingsType {
			continue
		}
		tools := m.Capabilities != nil && m.Capabilities.TrainedForToolUse
		models = append(models, HostModel{ID: m.Key, Tools: tools, Size: m.SizeBytes})
	}
	return models, nil
}
