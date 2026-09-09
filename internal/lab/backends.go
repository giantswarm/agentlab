package lab

import (
	"encoding/json"
	"regexp"

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
	// marker is a substring every answer of this server carries. The
	// preflight runs busybox wget in a pod and its output also holds
	// kubectl's attach chatter, so nothing there can be parsed.
	marker string
	// detail picks the identifying field out of that output for the note.
	detail *regexp.Regexp
	// ident reads the body the loopback probe got and returns what the
	// discovery prints for the server; ok is false when the body is not this
	// server's answer.
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
			path:   "/api/version",
			marker: versionMarker,
			detail: versionFieldRe,
			ident:  versionIdent,
		},
		models:         ollamaModels,
		bindFix:        "  - bind Ollama to every interface, not loopback: OLLAMA_HOST=0.0.0.0 (systemd: an\n    Environment= drop-in on ollama.service), then restart it",
		bindHint:       "OLLAMA_HOST=0.0.0.0, restart Ollama",
		proofModel:     ModelsTestModelOllama,
		provider:       config.ProviderOllama,
		providerNote:   "keyless native provider",
		deleteOverREST: true,
	},
	config.ModelManagerBackendLemonade: {
		probe: backendProbe{
			path:   lemonadeHealthPath,
			marker: versionMarker,
			detail: versionFieldRe,
			ident:  versionIdent,
		},
		models:         lemonadeModels,
		bindFix:        "  - bind Lemonade to every interface, not loopback: `lemonade config set host=0.0.0.0`\n    (host in ~/.config/lemonade/config.json), then restart lemond",
		bindHint:       "`lemonade config set host=0.0.0.0`, restart lemond",
		proofModel:     ModelsTestModelLemonade,
		provider:       config.ProviderOpenAI,
		providerNote:   "OpenAI-compatible /api/v1, placeholder key",
		deleteOverREST: true,
	},
	config.ModelManagerBackendLMStudio: {
		probe: backendProbe{
			path:   lmStudioModelsPath,
			marker: lmStudioMarker,
			detail: lmStudioKeyFieldRe,
			ident:  lmStudioIdent,
		},
		models:         lmStudioModels,
		bindFix:        "  - serve LM Studio on the local network, not loopback: `lms server start --bind 0.0.0.0`\n    (or the app's Developer > \"Serve on Local Network\" toggle, or LMS_SERVER_HOST=0.0.0.0)",
		bindHint:       "`lms server start --bind 0.0.0.0`, or the app's \"Serve on Local Network\" toggle",
		proofModel:     ModelsTestModelLMStudio,
		provider:       config.ProviderOpenAI,
		providerNote:   "OpenAI-compatible /v1, placeholder key",
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

// The identity a version-reporting server answers with: Ollama's whole
// /api/version document is {"version":"..."}, Lemonade's health document
// carries the field.
const versionMarker = `"version"`

// versionFieldRe matches that field in a probe pod's output.
var versionFieldRe = regexp.MustCompile(`"version"\s*:\s*"[^"]*"`)

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
	lmStudioMarker     = `"models"`
	// lmStudioIdentAPIv1 stands in for the version LM Studio does not report.
	lmStudioIdentAPIv1 = "api v1"
)

// lmStudioKeyFieldRe matches the key field of an inventory entry, the detail
// the preflight quotes back.
var lmStudioKeyFieldRe = regexp.MustCompile(`"key"\s*:\s*"[^"]*"`)

// lmStudioIdent recognises LM Studio by the shape of its inventory: a
// `models` array whose entries are keyed by `key`. The array is a pointer so
// that absent and empty are distinguishable — a fresh install with nothing
// downloaded is still an LM Studio, while every other answer (including LM
// Studio's own 200 for an unknown path, and Lemonade's `data` envelope on the
// very same path) carries no `models` key at all.
func lmStudioIdent(body []byte) (string, bool) {
	var doc struct {
		Models *[]struct {
			Key string `json:"key"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Models == nil {
		return "", false
	}
	if len(*doc.Models) > 0 && (*doc.Models)[0].Key == "" {
		return "", false
	}
	return lmStudioIdentAPIv1, true
}
