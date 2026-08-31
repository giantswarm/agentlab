// Package config holds the lab configuration: everything the interactive
// form asks for, persisted to agentlab.yaml so re-runs are reproducible.
package config

import (
	"fmt"
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
	// URLs port-free, which the chart's Backstage app-config assumes
	// (its baseUrl carries no port); change only if 443 is taken, and expect
	// Backstage to break behind a non-443 edge.
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
}

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
		DexPort:     32000,
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
			APSRef:        "a6d2c0f197de4197c897cfef906f5ed4f4804fa4", // main: backstage 0.201.0 (#54) — sessions can be continued from the portal
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

// ValidateAIModel constrains the model to Anthropic: both consumers (the
// kagent ModelConfig provider and Backstage's ai-chat prefix routing) are
// wired for Anthropic only, keyed by the one $ANTHROPIC_API_KEY secret.
func ValidateAIModel(s string) error {
	if !strings.HasPrefix(s, "claude-") {
		return fmt.Errorf("must be a claude-* model (the lab only wires Anthropic)")
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
	if err := ValidatePort(strconv.Itoa(c.Backstage.Port)); err != nil {
		return fmt.Errorf("backstage.port: %w", err)
	}
	return nil
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

func (c *Config) Issuer() string {
	return fmt.Sprintf("https://localhost:%d/dex", c.DexPort)
}

func (c *Config) KubeContext() string { return "kind-" + c.ClusterName }

// ControlPlaneNode is the docker container name kind gives the (only) node.
func (c *Config) ControlPlaneNode() string { return c.ClusterName + "-control-plane" }

// MCPServerName is the MCPServer CR name the umbrella chart registers for its
// bundled mcp-kubernetes (templates/mcp-kubernetes/mcpserver.yaml): a fixed
// name, independent of the cluster. Muster prefixes the server's tools with
// it: x_mcp-kubernetes_<tool>.
func (c *Config) MCPServerName() string { return "mcp-kubernetes" }

// gatewayURL builds the public URL of a platform hostname: through the
// agentgateway edge, port-free when the edge sits on 443 (the chart's
// app-config assumes exactly that).
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
