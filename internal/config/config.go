// Package config holds the lab configuration: everything the interactive
// form asks for, persisted to agentlab.yaml so re-runs are reproducible.
package config

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// File is the configuration file written next to the binary's working
// directory by `agentlab configure` and read by every other command.
const File = "agentlab.yaml"

// The three lab groups are a fixed vocabulary: RBAC binds exactly these to
// cluster-admin / edit-in-demo / view (see the rbac template).
const (
	groupPlatformAdmins = "platform-admins"
	groupDevelopers     = "developers"
	groupViewers        = "viewers"
)

var Groups = []string{groupPlatformAdmins, groupDevelopers, groupViewers}

// defaultPassword is the throwaway password every default lab user starts
// with; like everything else in the lab's identity, it guards nothing real.
const defaultPassword = "password"

// Static OAuth client IDs and secrets, paired per client. The IDs also appear
// in the templates (Dex staticClients and each consumer's own config), which
// must agree with these. The lab's identity is self-contained and throwaway
// by design; nothing here guards anything real.
const (
	KubernetesClientID     = "kubernetes"
	KubernetesClientSecret = "kubernetes-lab-secret" // #nosec G101 -- static throwaway lab credential, by design
	// The ONE platform client, following the chart's global.identity
	// convention: muster and Backstage authenticate with the same Dex client,
	// so a token minted for either carries an audience the other trusts.
	AgentPlatformClientID     = "agent-platform"
	AgentPlatformClientSecret = "agent-platform-lab-secret" // #nosec G101 -- static throwaway lab credential, by design
)

// MusterNodePort is the port muster's aggregator binds on the kind node
// (hostNetwork). It is the muster chart's default listen port, not a lab
// choice: in-node consumers (Backstage) always dial localhost:8090, while the
// host-side port mapping is configurable via Platform.MusterPort.
const MusterNodePort = 8090

// BrowserCallbackPort is the fixed local port for `agentlab browser`'s OAuth
// callback. Fixed because it must be pre-registered in Dex's redirectURIs.
const BrowserCallbackPort = 5555

// KagentUINodePort is the fixed NodePort the kagent-ui Service is pinned to,
// so the kind port mapping (created once, at cluster creation) has a stable
// node-side port to publish. The kagent chart renders `ui.service.type` but no
// `nodePort` field, so the pin is applied by `agentlab post-render` (HACKS.md
// U9). A real Kubernetes NodePort, hence the 30000-32767 range; the host side
// is configurable via Platform.AgentsPort.
const KagentUINodePort = 30880

// GatewayNodePort is the fixed NodePort publishing the agentgateway edge
// (the chart-owned Gateway's HTTPS :443 listener) on the kind node. The
// data-plane Service is created by the agentgateway controller at run time —
// not part of the Helm release, so neither values nor the post-renderer can
// pin its NodePort. The lab renders its own selector-matched NodePort Service
// instead (gateway-nodeport.yaml.tmpl), pinned here so the kind port mapping
// has a stable node-side port. Host side: Platform.GatewayPort.
const GatewayNodePort = 30443

// DefaultDexPort is the lab Dex NodePort when agentlab.yaml sets none.
const DefaultDexPort = 32000

type User struct {
	Email        string   `yaml:"email"`
	Username     string   `yaml:"username"`
	Name         string   `yaml:"name"`
	Password     string   `yaml:"password"`
	PasswordHash string   `yaml:"passwordHash"` // bcrypt of Password, cached so renders stay deterministic
	Groups       []string `yaml:"groups"`
}

func (u User) HasGroup(g string) bool {
	return slices.Contains(u.Groups, g)
}

type Platform struct {
	Enabled bool `yaml:"enabled"`
	// The agents runtime (kagent), an optional part of the platform install.
	// On real clusters agent delivery runs through Flux/GitOps, which the lab
	// does not run, so labs that are not exercising agents can skip the
	// runtime entirely. Inert when the platform itself is disabled.
	Agents bool `yaml:"agents"`
	// A minimal observability stack: the Giant Swarm kube-prometheus-stack
	// chart (the observability bundle's own pinned constituent, with the
	// Prometheus server re-enabled — the bundle itself is MC-shaped: Alloy
	// remote-writing to Mimir, no local query endpoint) plus mcp-prometheus
	// registered in muster, so agents can answer PromQL questions
	// (x_mcp-prometheus_<tool>). On by default: asking the platform about the
	// cluster's CPU/memory is part of the demo story. Not in the umbrella's
	// BOM (yet) — the lab pins the two charts itself (observability.go).
	// Inert when the platform itself is disabled.
	Observability bool `yaml:"observability"`
	// Host-side port for the kagent UI (http://localhost:<port>). The kind
	// mapping onto KagentUINodePort always exists — like the other mappings,
	// it is fixed at cluster creation — so agents can be enabled later.
	AgentsPort int `yaml:"agentsPort"`
	// Host-side port for muster's DIRECT debug access (hostNetwork + kind
	// mapping, bypassing the gateway). The platform's public URLs go through
	// the agentgateway edge on GatewayPort.
	MusterPort int `yaml:"musterPort"`
	// The platform's public domain. Every public hostname derives from it
	// (muster.<domain>, backstage.<domain>, ...). The default, nip.io's
	// loopback wildcard, resolves to 127.0.0.1 from anywhere without host
	// configuration; inside pods a CoreDNS rewrite points the same names at
	// the edge Gateway Service.
	Domain string `yaml:"domain"`
	// Host-side port of the agentgateway edge (HTTPS). 443 keeps the public
	// URLs port-free; any other value is only reachable from the host (the
	// edge Service inside the cluster stays on 443), so in-cluster callers of
	// a ported public URL — muster fetching its own OAuth metadata for the
	// lab-oauth-fixture — time out. Change only if 443 is taken.
	GatewayPort int `yaml:"gatewayPort"`
	// TLS optionally hands the edge an externally provisioned certificate
	// pair (PEM) instead of the minted lab-CA wildcard — for users who own a
	// real domain (wildcard record -> 127.0.0.1) and run their own ACME
	// tooling. Both fields set or both empty. The Dex issuer still serves
	// the lab CA either way; see the README's TLS section.
	TLS PlatformTLS `yaml:"tls"`
	// agent-platform-standalone has no chart release yet
	// (giantswarm/agent-platform-standalone#11), so it is vendored from git
	// at this pinned SHA. Once released this becomes an OCI ref.
	APSRepo string `yaml:"apsRepo"`
	APSRef  string `yaml:"apsRef"`
	// Additional kagent ModelConfigs beyond the chart-rendered default
	// (aiModel): self-hosted OpenAI-compatible endpoints (vLLM, Ollama),
	// OpenRouter, Gemini, plain OpenAI. Rendered as lab-labeled ModelConfig
	// CRs by `agentlab platform`; entries removed here are pruned on the next
	// run. Inert unless agents are enabled.
	ExtraModels []ExtraModel `yaml:"extraModels,omitempty"`
	// Managed models: the umbrella's model-manager component in front of the
	// model servers that run on the lab host — an Ollama, a Lemonade Server
	// (FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on GPU/CPU) — pull, load,
	// unload and delete models from the portal (or as x_model-manager_*
	// tools through muster), each pulled model wired into kagent as a
	// keyless ModelConfig automatically. Complements extraModels, which
	// wires endpoints statically and manages nothing. Requires agents.
	// `agentlab configure` fills the backends from what answers on this
	// machine, on every run.
	ModelManager ModelManager `yaml:"modelManager"`
}

// ModelManager configures the umbrella's model-manager component in the lab.
type ModelManager struct {
	// On, `agentlab platform` enables components.model-manager in front of
	// every listed backend, its agentgateway route (JWT-validated: the portal
	// backend forwards the user's Dex token) and the muster registration.
	// `agentlab configure` turns it on whenever a host model server answers
	// (and off when none does), unless --model-manager pins it.
	Enabled bool `yaml:"enabled"`
	// The host model servers, in order: `ollama` (an Ollama) and `lemonade`
	// (a Lemonade Server). ONE model-manager fronts all of them at once
	// (model-manager >= 0.17.0, `model-manager.backends` in the umbrella
	// values); the first entry is its default backend — where a request that
	// names none goes. `agentlab configure` fills the list from what answers
	// on this machine (Ollama first); --model-manager-backends pins it.
	// kserve is no lab backend: GPU nodes and a KServe install.
	Backends []string `yaml:"backends,omitempty"`
	// Per-backend base URL as pods reach it, keyed by backend. Empty
	// autodetects http://<kind docker network gateway>:<default port> at
	// platform time (11434 for Ollama, 13305 for Lemonade) — the same
	// address the README documents for extraModels (`docker network inspect
	// kind`). Set one for a server elsewhere on the LAN: a backend with an
	// endpoint here is kept by `agentlab configure` whether or not a server
	// answers on this machine.
	Endpoints map[string]string `yaml:"endpoints,omitempty"`
	// The one-backend form earlier versions wrote (backend + endpoint):
	// still read, folded into backends/endpoints on load, never written.
	Backend  string `yaml:"backend,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

// The model servers the lab runs against on the host, which model-manager
// and agent pods reach through the kind docker network's gateway.
const (
	// ModelManagerBackendOllama is a host Ollama.
	ModelManagerBackendOllama = "ollama"
	// ModelManagerBackendLemonade is a host Lemonade Server
	// (lemonade-server.ai): FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on
	// GPU and CPU, behind one OpenAI-compatible API plus a management API.
	ModelManagerBackendLemonade = "lemonade"
)

// ModelManagerBackends lists the backends the lab accepts, in the canonical
// order `agentlab configure` writes them — which is also the preference for
// the one model-manager fronts.
var ModelManagerBackends = []string{ModelManagerBackendOllama, ModelManagerBackendLemonade}

// The servers' default API ports, the ones the autodetected endpoints assume.
const (
	OllamaPort   = 11434
	LemonadePort = 13305
)

// BackendPort is the default API port of a backend's server.
func BackendPort(backend string) int {
	if backend == ModelManagerBackendLemonade {
		return LemonadePort
	}
	return OllamaPort
}

// BackendServerName is a backend's server as messages name it.
func BackendServerName(backend string) string {
	if backend == ModelManagerBackendLemonade {
		return "Lemonade Server"
	}
	return "Ollama"
}

// Primary is model-manager's default backend — where a request that names
// none goes: the first of the list, or the historical default (an Ollama)
// for an enabled block that names none.
func (m ModelManager) Primary() string {
	if len(m.Backends) == 0 {
		return ModelManagerBackendOllama
	}
	return m.Backends[0]
}

// Secondary lists the further backends — wired statically until
// model-manager is multi-backend.
func (m ModelManager) Secondary() []string {
	if len(m.Backends) < 2 {
		return nil
	}
	return m.Backends[1:]
}

// EndpointFor is the configured endpoint override of a backend, "" to
// autodetect.
func (m ModelManager) EndpointFor(backend string) string {
	return m.Endpoints[backend]
}

// normalize folds the legacy one-backend form into the lists, orders the
// backends canonically and gives an enabled block without backends the
// historical default (an Ollama), so files written by earlier versions read
// exactly as before.
func (m *ModelManager) normalize() {
	if m.Backend != "" {
		if !slices.Contains(m.Backends, m.Backend) {
			m.Backends = append([]string{m.Backend}, m.Backends...)
		}
		if m.Endpoint != "" {
			if m.Endpoints == nil {
				m.Endpoints = map[string]string{}
			}
			if _, set := m.Endpoints[m.Backend]; !set {
				m.Endpoints[m.Backend] = m.Endpoint
			}
		}
	}
	m.Backend, m.Endpoint = "", ""
	if m.Enabled && len(m.Backends) == 0 {
		m.Backends = []string{ModelManagerBackendOllama}
	}
	if len(m.Endpoints) == 0 {
		m.Endpoints = nil
	}
}

// ApplyDiscovered merges what `agentlab configure` found on this machine into
// the block: the backends are the servers that answer plus every backend kept
// by an explicit endpoint (a server elsewhere on the LAN), in canonical order;
// managed models go on when there is at least one and the agents runtime is
// on, and off otherwise. pinBackends (--model-manager-backends) replaces the
// list outright; pinEnabled (--model-manager) decides the flag instead of the
// discovery — a pinned-on block without a backend falls back to the Ollama
// default, so the platform preflight reports the real reachability error.
func (m *ModelManager) ApplyDiscovered(found []string, agents bool, pinEnabled *bool, pinBackends []string) {
	m.normalize()
	var backends []string
	switch {
	case pinBackends != nil:
		backends = slices.Clone(pinBackends)
	default:
		for _, b := range ModelManagerBackends {
			if slices.Contains(found, b) || m.Endpoints[b] != "" {
				backends = append(backends, b)
			}
		}
	}
	m.Backends = backends
	for b := range m.Endpoints {
		if !slices.Contains(m.Backends, b) {
			delete(m.Endpoints, b)
		}
	}
	if len(m.Endpoints) == 0 {
		m.Endpoints = nil
	}
	if pinEnabled != nil {
		m.Enabled = *pinEnabled
	} else {
		m.Enabled = agents && len(m.Backends) > 0
	}
	if m.Enabled && len(m.Backends) == 0 {
		m.Backends = []string{ModelManagerBackendOllama}
	}
}

// ExtraModel is one additional kagent ModelConfig. API keys are NOT config:
// APIKeyEnv only names the host env var read at deploy time — the value lands
// in the Secret kagent-<name>, never in this file or in rendered state/.
type ExtraModel struct {
	// ModelConfig CR name; also names the key Secret kagent-<name>.
	Name string `yaml:"name"`
	// One of ModelProviders (a subset of the kagent CRD's enum: the providers
	// expressible with just a model + base URL).
	Provider string `yaml:"provider"`
	// The provider's model id (what the endpoint serves, e.g. `qwen3-8-27b`
	// for a local vLLM or `deepseek/deepseek-chat` on OpenRouter).
	Model string `yaml:"model"`
	// Endpoint override. Required for Ollama (the in-cluster default would be
	// useless), optional for OpenAI/Anthropic (any compatible endpoint:
	// vLLM `http://host:8000/v1`, OpenRouter `https://openrouter.ai/api/v1`),
	// not applicable to Gemini.
	BaseURL string `yaml:"baseUrl,omitempty"`
	// Host env var holding the API key at deploy time. Empty means a keyless
	// endpoint: the Secret is still created with a placeholder, because the
	// kagent ADK runtime requires the provider's env var to exist.
	APIKeyEnv string `yaml:"apiKeyEnv,omitempty"`
	// Skip TLS verification on the provider connection (ModelConfig spec.tls.
	// disableVerify) — for self-hosted endpoints with self-signed certs.
	InsecureTLS bool `yaml:"insecureTLS,omitempty"`
}

// The provider vocabulary for extra models, spelled exactly as the kagent
// ModelConfig CRD's provider enum spells them.
const (
	ProviderOpenAI    = "OpenAI"
	ProviderAnthropic = "Anthropic"
	ProviderGemini    = "Gemini"
	ProviderOllama    = "Ollama"
)

// ModelProviders maps each provider to the key name inside the Secret. The
// kagent controller injects that key as an env var of the same name into
// agent pods, and the ADK runtime looks up exactly these canonical names —
// so the key name is provider-derived, not configurable. Ollama is keyless
// (empty key = no Secret).
var ModelProviders = map[string]string{
	ProviderOpenAI:    "OPENAI_API_KEY",
	ProviderAnthropic: "ANTHROPIC_API_KEY",
	ProviderGemini:    "GOOGLE_API_KEY",
	ProviderOllama:    "",
}

// ModelProviderNames is ModelProviders' keys in a stable order, for the form
// options and error messages.
var ModelProviderNames = []string{ProviderOpenAI, ProviderAnthropic, ProviderGemini, ProviderOllama}

// SecretName is the Kubernetes Secret (in ns kagent) holding this model's key.
func (m ExtraModel) SecretName() string { return "kagent-" + m.Name }

// SecretKey is the key inside the Secret — the provider's canonical env var
// name. Empty for keyless providers (no Secret attached at all).
func (m ExtraModel) SecretKey() string { return ModelProviders[m.Provider] }

// NeedsSecret reports whether this model's ModelConfig references a Secret.
func (m ExtraModel) NeedsSecret() bool { return m.SecretKey() != "" }

// PlatformTLS is an externally provisioned certificate for the edge; see
// Platform.TLS.
type PlatformTLS struct {
	CertFile string `yaml:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty"`
}

// Set reports whether an external edge certificate is configured.
func (t PlatformTLS) Set() bool { return t.CertFile != "" }

type Backstage struct {
	Enabled bool `yaml:"enabled"`
	// Backstage binds this port on the node (hostNetwork) and kind maps the
	// same number onto the host, so the URL is identical on both sides.
	// The image is not configured here: the umbrella chart's backstage
	// dependency (pinned by platform.apsRef) decides the version.
	Port int `yaml:"port"`
}

type Config struct {
	// Kind cluster name; also prefixes RBAC bindings and names the muster
	// installation surfaced in Backstage.
	ClusterName string `yaml:"clusterName"`
	// Dex NodePort == host port: the issuer https://localhost:<port>/dex must
	// be the same URL from the Mac and from inside the node, so both sides
	// use one number. Must sit in the NodePort range (30000-32767).
	DexPort  int    `yaml:"dexPort"`
	DexImage string `yaml:"dexImage"`

	// The Claude model both AI consumers use: the platform agents' default
	// ModelConfig (kagent) and Backstage's ai-chat. The API key is NOT config:
	// it is read from $ANTHROPIC_API_KEY at deploy time and lands only in
	// Kubernetes Secrets, never in this file or in rendered manifests.
	AIModel string `yaml:"aiModel"`

	Users     []User    `yaml:"users"`
	Platform  Platform  `yaml:"platform"`
	Backstage Backstage `yaml:"backstage"`
}

// Default returns the canonical lab setup: the agent platform enabled (it is
// what the lab exists to test) with the agentgateway edge and Backstage — the
// real platform topology — three users, and Dex on 32000.
func Default() *Config {
	return &Config{
		ClusterName: "agentlab",
		DexPort:     DefaultDexPort,
		// groups on staticPasswords requires Dex >= v2.45.0; see README.
		DexImage: "ghcr.io/dexidp/dex:v2.45.1",
		// The agent-platform BOM's own default, and the newest model the
		// pinned Backstage build's thinking-mode handling is known to cover.
		AIModel: "claude-sonnet-4-6",
		Users: []User{
			{Email: "admin@lab.local", Username: "admin", Name: "Lab Admin", Password: defaultPassword,
				Groups: []string{groupPlatformAdmins, groupDevelopers}},
			{Email: "dev@lab.local", Username: "dev", Name: "Lab Developer", Password: defaultPassword,
				Groups: []string{groupDevelopers}},
			{Email: "viewer@lab.local", Username: "viewer", Name: "Lab Viewer", Password: defaultPassword,
				Groups: []string{groupViewers}},
		},
		Platform: Platform{
			Enabled:       true,
			Agents:        true,
			Observability: true,
			AgentsPort:    8081,
			MusterPort:    8090,
			Domain:        "127.0.0.1.nip.io",
			GatewayPort:   443,
			APSRepo:       "https://github.com/giantswarm/agent-platform-standalone",
			APSRef:        "e946a2f2f68f7408042f1ba36ad539ecd286d39b", // main: backstage 0.226.0 (one Serving group per backend, #121) on muster 5.8.2 + agent-platform-mcps 0.7.0 (#125, #150, #153) + model-manager 0.17.0 (several backends per instance) + agent-platform 3.7.0
		},
		Backstage: Backstage{
			Enabled: true,
			Port:    7007,
		},
	}
}

// Load reads agentlab.yaml. A missing file is reported as os.ErrNotExist so
// callers can decide whether to fall back to the form or to defaults.
func Load() (*Config, error) {
	raw, err := os.ReadFile(File)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", File, err)
	}
	// Earlier versions wrote the one-backend form (backend/endpoint); read
	// it as the one-item lists so the same lab renders exactly as before.
	cfg.Platform.ModelManager.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	changed, err := cfg.EnsureHashes()
	if err != nil {
		return nil, err
	}
	if changed {
		if err := cfg.write(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func (c *Config) Save() error {
	if _, err := c.EnsureHashes(); err != nil {
		return err
	}
	return c.write()
}

// write persists the config as-is; Save and Load ensure the hashes first
// (exactly once — bcrypt-comparing every user is not free).
func (c *Config) write() error {
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := []byte("# agentlab lab configuration. Regenerate with `agentlab configure`.\n" +
		"# Passwords are lab-only throwaway credentials; the hash is cached so\n" +
		"# rendered manifests stay byte-identical across runs (no spurious pod rolls).\n")
	return os.WriteFile(File, append(header, out...), 0o600)
}

// EnsureHashes fills in User.PasswordHash wherever it is missing or no longer
// matches the password. The hash is cached in the config file on purpose:
// bcrypt salts are random, so hashing at render time would change the Dex
// manifest (and roll the pod) on every single run.
func (c *Config) EnsureHashes() (changed bool, err error) {
	for i := range c.Users {
		u := &c.Users[i]
		if u.PasswordHash != "" &&
			bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(u.Password)) == nil {
			continue
		}
		h, err := bcrypt.GenerateFromPassword([]byte(u.Password), 10)
		if err != nil {
			return false, fmt.Errorf("hashing password for %s: %w", u.Email, err)
		}
		u.PasswordHash = string(h)
		changed = true
	}
	return changed, nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateClusterName is the one home of the cluster-name rule; the huh form
// uses it directly as an input validator.
func ValidateClusterName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("lowercase alphanumeric and dashes only")
	}
	return nil
}

// ValidateAIModel constrains the DEFAULT model to Anthropic: both consumers
// (the kagent ModelConfig provider and Backstage's ai-chat prefix routing)
// are wired for Anthropic only, keyed by the one $ANTHROPIC_API_KEY secret.
// Other providers go through platform.extraModels instead.
func ValidateAIModel(s string) error {
	if !strings.HasPrefix(s, "claude-") {
		return fmt.Errorf("must be a claude-* model (other providers go in platform.extraModels)")
	}
	return nil
}

// reservedModelNames collide with what the kagent chart / the lab already
// own: the chart-rendered default ModelConfig, and the name whose Secret
// (kagent-anthropic) the default ModelConfig references.
var reservedModelNames = []string{"default-model-config", "anthropic"}

// ValidateModelName is the one home of the extra-model-name rule; the huh
// form uses it directly as an input validator.
func ValidateModelName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("lowercase alphanumeric and dashes only")
	}
	if slices.Contains(reservedModelNames, s) {
		return fmt.Errorf("%q is reserved (the default ModelConfig and its Secret)", s)
	}
	return nil
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateAPIKeyEnv accepts an env var name or empty (keyless endpoint).
func ValidateAPIKeyEnv(s string) error {
	if s != "" && !envNameRe.MatchString(s) {
		return fmt.Errorf("not an environment variable name")
	}
	return nil
}

// Validate checks one extra model entry; string-typed like the port
// validators where the form shares it, entry-level here.
func (m ExtraModel) Validate() error {
	if err := ValidateModelName(m.Name); err != nil {
		return fmt.Errorf("name %q: %w", m.Name, err)
	}
	key, known := ModelProviders[m.Provider]
	if !known {
		return fmt.Errorf("%s: unknown provider %q (one of %s)", m.Name, m.Provider, strings.Join(ModelProviderNames, ", "))
	}
	if m.Model == "" {
		return fmt.Errorf("%s: model is required", m.Name)
	}
	if err := ValidateAPIKeyEnv(m.APIKeyEnv); err != nil {
		return fmt.Errorf("%s: apiKeyEnv %q: %w", m.Name, m.APIKeyEnv, err)
	}
	switch m.Provider {
	case ProviderGemini:
		if m.BaseURL != "" {
			return fmt.Errorf("%s: Gemini takes no baseUrl (the ModelConfig CRD has no endpoint field for it)", m.Name)
		}
	case ProviderOllama:
		if m.BaseURL == "" {
			return fmt.Errorf("%s: Ollama requires baseUrl (the host serving the API, e.g. http://192.168.1.10:11434)", m.Name)
		}
	}
	if key == "" && m.APIKeyEnv != "" {
		return fmt.Errorf("%s: %s is keyless — apiKeyEnv would be silently ignored", m.Name, m.Provider)
	}
	return nil
}

// Normalize applies cross-field implications after an entry point sets the
// enable flags: Backstage's muster plugin is the reason to run it, so it
// implies the platform. Validate stays the backstop for hand-edited files.
func (c *Config) Normalize() {
	if c.Backstage.Enabled {
		c.Platform.Enabled = true
	}
	c.Platform.ModelManager.normalize()
}

func (c *Config) Validate() error {
	if err := ValidateClusterName(c.ClusterName); err != nil {
		return fmt.Errorf("cluster name %q: %w", c.ClusterName, err)
	}
	if err := ValidateNodePort(strconv.Itoa(c.DexPort)); err != nil {
		return fmt.Errorf("dexPort: %w", err)
	}
	if err := ValidateAIModel(c.AIModel); err != nil {
		return fmt.Errorf("aiModel %q: %w", c.AIModel, err)
	}
	if len(c.Users) == 0 {
		return fmt.Errorf("at least one user is required")
	}
	if c.AdminUser() == nil {
		return fmt.Errorf("at least one user must be in the platform-admins group (the up verification and muster RBAC depend on it)")
	}
	seen := map[string]bool{}
	for _, u := range c.Users {
		if u.Email == "" || u.Username == "" || u.Password == "" {
			return fmt.Errorf("user %q: email, username and password are all required", u.Email)
		}
		if seen[u.Email] {
			return fmt.Errorf("duplicate user email %q", u.Email)
		}
		seen[u.Email] = true
		for _, g := range u.Groups {
			if !slices.Contains(Groups, g) {
				return fmt.Errorf("user %s: unknown group %q (RBAC only binds %v)", u.Email, g, Groups)
			}
		}
	}
	if c.Backstage.Enabled && !c.Platform.Enabled {
		return fmt.Errorf("backstage requires the agent platform (the muster plugin is the reason to run it)")
	}
	if err := ValidatePort(strconv.Itoa(c.Platform.MusterPort)); err != nil {
		return fmt.Errorf("platform.musterPort: %w", err)
	}
	if c.Platform.Domain == "" {
		return fmt.Errorf("platform.domain is required (public hostnames derive from it)")
	}
	if err := ValidatePort(strconv.Itoa(c.Platform.GatewayPort)); err != nil {
		return fmt.Errorf("platform.gatewayPort: %w", err)
	}
	if (c.Platform.TLS.CertFile == "") != (c.Platform.TLS.KeyFile == "") {
		return fmt.Errorf("platform.tls: certFile and keyFile must be set together")
	}
	if c.Platform.TLS.Set() {
		for _, p := range []string{c.Platform.TLS.CertFile, c.Platform.TLS.KeyFile} {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("platform.tls: %w", err)
			}
		}
	}
	if err := ValidatePort(strconv.Itoa(c.Platform.AgentsPort)); err != nil {
		return fmt.Errorf("platform.agentsPort: %w", err)
	}
	seenModels := map[string]bool{}
	for _, m := range c.Platform.ExtraModels {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("platform.extraModels: %w", err)
		}
		if seenModels[m.Name] {
			return fmt.Errorf("platform.extraModels: duplicate name %q", m.Name)
		}
		seenModels[m.Name] = true
	}
	if err := c.Platform.ModelManager.Validate(c.Platform.Agents); err != nil {
		return fmt.Errorf("platform.modelManager: %w", err)
	}
	if err := ValidatePort(strconv.Itoa(c.Backstage.Port)); err != nil {
		return fmt.Errorf("backstage.port: %w", err)
	}
	return nil
}

var httpURLRe = regexp.MustCompile(`^https?://[^/]+`)

// Validate checks the model-manager block; agents reports whether the kagent
// runtime is on (model-manager wires ModelConfigs into it).
func (m ModelManager) Validate(agents bool) error {
	backends := m.Backends
	if m.Backend != "" && !slices.Contains(backends, m.Backend) {
		backends = append([]string{m.Backend}, backends...) // the legacy form before normalize
	}
	for i, b := range backends {
		if !slices.Contains(ModelManagerBackends, b) {
			return fmt.Errorf("backend %q: the lab supports %s (kserve needs GPU nodes and KServe)", b, strings.Join(ModelManagerBackends, ", "))
		}
		if slices.Contains(backends[:i], b) {
			return fmt.Errorf("backends: %q listed twice", b)
		}
	}
	if m.Endpoint != "" && !httpURLRe.MatchString(m.Endpoint) {
		return fmt.Errorf("endpoint %q: must be an http(s) URL, e.g. http://172.21.0.1:%d", m.Endpoint, OllamaPort)
	}
	for _, b := range slices.Sorted(maps.Keys(m.Endpoints)) {
		ep := m.Endpoints[b]
		if !slices.Contains(backends, b) {
			return fmt.Errorf("endpoints.%s: %q is not in backends %v", b, b, backends)
		}
		if !httpURLRe.MatchString(ep) {
			return fmt.Errorf("endpoints.%s %q: must be an http(s) URL, e.g. http://172.21.0.1:%d", b, ep, BackendPort(b))
		}
	}
	if m.Enabled && !agents {
		return fmt.Errorf("requires platform.agents (model-manager wires pulled models into kagent ModelConfigs)")
	}
	return nil
}

// ModelManagerEnabled reports whether the platform installs model-manager.
func (c *Config) ModelManagerEnabled() bool {
	return c.Platform.Enabled && c.Platform.Agents && c.Platform.ModelManager.Enabled
}

// AdminUser returns the first user in platform-admins: the identity the up
// verification loop and the smoke tests default to.
func (c *Config) AdminUser() *User {
	for i := range c.Users {
		if c.Users[i].HasGroup(groupPlatformAdmins) {
			return &c.Users[i]
		}
	}
	return nil
}

func (c *Config) FindUser(email string) *User {
	for i := range c.Users {
		if c.Users[i].Email == email {
			return &c.Users[i]
		}
	}
	return nil
}

// FindUserInGroup returns the first user carrying group, or nil.
func (c *Config) FindUserInGroup(group string) *User {
	for i := range c.Users {
		for _, g := range c.Users[i].Groups {
			if g == group {
				return &c.Users[i]
			}
		}
	}
	return nil
}

func (c *Config) Issuer() string {
	return fmt.Sprintf("https://localhost:%d/dex", c.DexPort)
}

// ControlPlaneNode is the docker container name kind gives the (only) node.
func (c *Config) ControlPlaneNode() string { return c.ClusterName + "-control-plane" }

// MCPServerName is the MCPServer CR name the umbrella chart registers for its
// bundled mcp-kubernetes (templates/mcp-kubernetes/mcpserver.yaml): a fixed
// name, independent of the cluster. Muster prefixes the server's tools with
// it: x_mcp-kubernetes_<tool>.
func (c *Config) MCPServerName() string { return "mcp-kubernetes" }

// gatewayURL builds the public URL of a platform hostname: through the
// agentgateway edge, port-free when the edge sits on 443. The chart's
// Backstage app-config renders port-free URLs regardless; the lab's overlay
// restates them from BackstageBaseURL (HACKS.md U15).
func (c *Config) gatewayURL(prefix string) string {
	if c.Platform.GatewayPort == 443 {
		return fmt.Sprintf("https://%s.%s", prefix, c.Platform.Domain)
	}
	return fmt.Sprintf("https://%s.%s:%d", prefix, c.Platform.Domain, c.Platform.GatewayPort)
}

// MusterBaseURL is muster's public URL: client -> agentgateway edge -> muster,
// the real platform topology. What Claude Code dials and what muster's OAuth
// server advertises.
func (c *Config) MusterBaseURL() string { return c.gatewayURL("muster") }

// MusterDirectURL bypasses the edge: the hostNetwork port mapping straight to
// muster, kept for debugging the lab's own plumbing.
func (c *Config) MusterDirectURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Platform.MusterPort)
}

// BackstageBaseURL is Backstage's public URL through the agentgateway edge.
func (c *Config) BackstageBaseURL() string { return c.gatewayURL("backstage") }

// ObservabilityBaseURL is the lab Prometheus's public query API through the
// edge, at the /prometheus prefix Backstage's Mimir integration expects
// (observability-route.yaml.tmpl). Backstage itself always dials it in-cluster
// on 443 (the CoreDNS rewrite), so only host-side callers see GatewayPort.
func (c *Config) ObservabilityBaseURL() string {
	return c.gatewayURL("observability") + "/prometheus"
}

// AgentgatewayBaseURL is the agentgateway hostname through the edge: the
// path-prefixed platform APIs (the kagent controller at /kagent,
// model-manager at /model-manager) live here, same as the chart's Backstage
// app-config derives them.
func (c *Config) AgentgatewayBaseURL() string { return c.gatewayURL("agentgateway") }

// ModelManagerBaseURL is the model-manager API through the edge — the
// umbrella's components.model-manager.route (pathPrefix /model-manager,
// stripped before the service, so its REST API sits at <base>/api/v1). The
// route's JWT policy wants a Dex token on every call.
func (c *Config) ModelManagerBaseURL() string { return c.AgentgatewayBaseURL() + "/model-manager" }

// BackstageDirectURL bypasses the edge (hostNetwork port mapping), kept for
// debugging.
func (c *Config) BackstageDirectURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Backstage.Port)
}

func (c *Config) KagentUIBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Platform.AgentsPort)
}

// ValidateNodePort checks the Kubernetes NodePort range; the Dex port must be
// a NodePort because the Service exposes it as one. String-typed (like
// ValidatePort) so the huh form inputs can use it directly.
func ValidateNodePort(s string) error {
	return validatePort(s, 30000, 32767, "must be in the NodePort range 30000-32767")
}

func ValidatePort(s string) error {
	return validatePort(s, 1, 65535, "must be a port between 1 and 65535")
}

func validatePort(s string, lo, hi int, rangeMsg string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("not a number")
	}
	if n < lo || n > hi {
		return fmt.Errorf("%s", rangeMsg)
	}
	return nil
}
