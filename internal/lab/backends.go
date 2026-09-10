package lab

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/giantswarm/agentlab/internal/config"
)

// What the lab knows about talking to each host model server: how to
// recognise one, how to read its inventory, the host-side fix when pods
// cannot reach it, and what models-test expects of the ModelConfig
// model-manager writes for it. internal/config owns the rest (the kind, the
// display name, the default port), so adding a server is one literal there
// and one here.

// backendProbe fingerprints one host model server. A status code is not
// evidence: LM Studio answers HTTP 200 with an {"error": ...} document for
// every path outside its own /api/v1 — Ollama's /api/version, /api/tags and
// /api/show included — so identity is always a claim about the body's shape.
type backendProbe struct {
	// path is the GET that identifies the server. The loopback detection and
	// the in-cluster preflight fetch the same one.
	path string
	// ident reads the document the probe got and returns what the discovery
	// prints for the server; ok is false when the body is not this server's
	// answer. The loopback probe and the in-cluster preflight both decide
	// with it, so "reachable" and "is the server it claims to be" are never
	// answered by a substring.
	ident func(body []byte) (string, bool)
}

// hostServer is one host model server's behaviour.
type hostServer struct {
	probe backendProbe
	// models lists what the server has downloaded.
	models func(base string) ([]HostModel, error)
	// bindFix is the host-side fix a failed preflight prints, indented to sit
	// in its list of fixes.
	bindFix string
	// bindHint is the one-line form for the discovery report.
	bindHint string
	// proofModel is models-test's default: small and tool-calling capable.
	proofModel string
	// provider is the kagent provider model-manager writes for this server's
	// models, providerNote how the proof words it.
	provider     string
	providerNote string
	// agentPath is the suffix the ModelConfig's baseUrl must carry — the
	// OpenAI-compatible surface of this server, which is the ONE thing the
	// lemonade and lmstudio wirings do not share (/api/v1 against /v1).
	// Empty where the provider takes no path (the native Ollama one), and
	// models-test asserts it so providerNote cannot claim a path nobody
	// checked: a release writing the other one passes the wiring step and
	// then fails as an opaque agent-turn timeout.
	agentPath string
	// deleteOverREST: the server can delete a model through its API. LM
	// Studio cannot (`lms rm` on the host only), so model-manager reports the
	// delete capability false and models-test proves the refusal instead.
	deleteOverREST bool
	// removeHint is the host-side command that removes a model, for the
	// closing note of the one run that leaves its model behind. Empty for a
	// server that deletes over its API, where nothing is left to remove.
	removeHint string
}

// hostServers is the per-backend behaviour, keyed by the kinds
// config.ModelManagerBackends lists.
var hostServers = map[string]hostServer{
	config.ModelManagerBackendOllama: {
		probe: backendProbe{
			path:  "/api/version",
			ident: versionIdent,
		},
		models:         ollamaModels,
		bindFix:        "  - bind Ollama to every interface, not loopback: OLLAMA_HOST=0.0.0.0 (systemd: an\n    Environment= drop-in on ollama.service), then restart it",
		bindHint:       "OLLAMA_HOST=0.0.0.0, restart Ollama",
		proofModel:     ModelsTestModelOllama,
		provider:       config.ProviderOllama,
		providerNote:   "keyless native provider",
		agentPath:      "",
		deleteOverREST: true,
	},
	config.ModelManagerBackendLemonade: {
		probe: backendProbe{
			path:  lemonadeHealthPath,
			ident: versionIdent,
		},
		models:         lemonadeModels,
		bindFix:        "  - bind Lemonade to every interface, not loopback: `lemonade config set host=0.0.0.0`\n    (host in ~/.config/lemonade/config.json), then restart lemond",
		bindHint:       "`lemonade config set host=0.0.0.0`, restart lemond",
		proofModel:     ModelsTestModelLemonade,
		provider:       config.ProviderOpenAI,
		providerNote:   "OpenAI-compatible /api/v1, placeholder key",
		agentPath:      "/api/v1",
		deleteOverREST: true,
	},
	config.ModelManagerBackendLMStudio: {
		probe: backendProbe{
			path:  lmStudioModelsPath,
			ident: lmStudioIdent,
		},
		models:         lmStudioModels,
		bindFix:        "  - serve LM Studio on the local network, not loopback: `lms server start --bind 0.0.0.0`\n    (or the app's Developer > \"Serve on Local Network\" toggle, or LMS_SERVER_HOST=0.0.0.0)",
		bindHint:       "`lms server start --bind 0.0.0.0`, or the app's \"Serve on Local Network\" toggle",
		proofModel:     ModelsTestModelLMStudio,
		provider:       config.ProviderOpenAI,
		providerNote:   "OpenAI-compatible /v1, placeholder key",
		agentPath:      "/v1",
		deleteOverREST: false,
		removeHint:     "lms rm %s",
	},
}

// backendSpec is a backend's behaviour; ok is false for a kind the lab does
// not know (config.Validate rejects one when the file loads).
func backendSpec(backend string) (hostServer, bool) {
	s, ok := hostServers[backend]
	return s, ok
}

// versionIdent reads the version both servers report.
func versionIdent(body []byte) (string, bool) {
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Version == "" {
		return "", false
	}
	return v.Version, true
}

// LM Studio's own API. It reports no version anywhere — not in /api/v1, not
// in the OpenAI-compatible /v1, not in a health document — so the inventory
// envelope is both the identity and the "version" the discovery prints.
const (
	lmStudioModelsPath = "/api/v1/models"
	// lmStudioIdentAPIv1 stands in for the version LM Studio does not report.
	lmStudioIdentAPIv1 = "api v1"
)

// decodeLMStudioLibrary is the ONE decoder of LM Studio's /api/v1/models
// document: the fingerprint that recognises the server and the inventory the
// lab reads are the same question asked of the same body, and two hand-written
// decoders drifted apart once already ({"models":[{"name":"x"}]} was "not an
// LM Studio" to one and a one-model library to the other).
//
// The array is a pointer so absent and empty are distinguishable — a fresh
// install with nothing downloaded is still an LM Studio, while every other
// answer (LM Studio's own 200 for an unknown path, Lemonade's `data` envelope
// on the very same path) carries no `models` key at all. Entries must be
// keyed by `key`; anything else is a document this lab cannot read as an
// inventory, so it is not an LM Studio for either caller's purpose.
//
// Everything but an embedding model can serve an agent — a vlm is a chat
// model with vision, and embedding entries carry no capability object at all
// — and tool calling is the model's training, which LM Studio reports
// directly, so there is no second request per model as on Ollama. Size is
// already bytes: no conversion, unlike Lemonade's decimal GB.
func decodeLMStudioLibrary(body []byte) ([]HostModel, error) {
	var doc struct {
		Models *[]struct {
			Key          string `json:"key"`
			Type         string `json:"type"`
			SizeBytes    int64  `json:"size_bytes"`
			Capabilities *struct {
				TrainedForToolUse bool `json:"trained_for_tool_use"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("not an LM Studio 0.4.0+ answer (%w)", err)
	}
	if doc.Models == nil {
		return nil, errors.New("not an LM Studio 0.4.0+ answer (no `models` array)")
	}
	models := make([]HostModel, 0, len(*doc.Models))
	for _, m := range *doc.Models {
		if m.Key == "" {
			return nil, errors.New("not an LM Studio 0.4.0+ answer (an entry carries no `key`)")
		}
		if m.Type == lmStudioEmbeddingType || m.Type == lmStudioEmbeddingsType {
			continue
		}
		tools := m.Capabilities != nil && m.Capabilities.TrainedForToolUse
		models = append(models, HostModel{ID: m.Key, Tools: tools, Size: m.SizeBytes})
	}
	return models, nil
}

// lmStudioIdent recognises LM Studio by whether its inventory decodes at all.
func lmStudioIdent(body []byte) (string, bool) {
	if _, err := decodeLMStudioLibrary(body); err != nil {
		return "", false
	}
	return lmStudioIdentAPIv1, true
}
