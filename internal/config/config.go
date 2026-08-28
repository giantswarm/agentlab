// Package config holds the lab configuration: everything the interactive
// form asks for, persisted to agentlab.yaml so re-runs are reproducible.
package config

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// File is the configuration file written next to the binary's working
// directory by `agentlab configure` and read by every other command.
const File = "agentlab.yaml"

// The three lab groups are a fixed vocabulary: RBAC binds exactly these to
// cluster-admin / edit-in-demo / view (see the rbac template).
var Groups = []string{"platform-admins", "developers", "viewers"}

// Static OAuth client IDs and secrets, paired per client. The IDs also appear
// in the templates (Dex staticClients and each consumer's own config), which
// must agree with these. The lab's entire point is self-contained throwaway
// identity; nothing here guards anything real.
const (
	KubernetesClientID     = "kubernetes"
	KubernetesClientSecret = "kubernetes-lab-secret"
	BackstageClientID      = "backstage"
	BackstageClientSecret  = "backstage-lab-secret"
	MusterClientID         = "muster"
	MusterClientSecret     = "muster-lab-secret"
)

// MusterNodePort is the port muster's aggregator binds on the kind node
// (hostNetwork). It is the muster chart's default listen port, not a lab
// choice: in-node consumers (Backstage) always dial localhost:8090, while the
// host-side port mapping is configurable via Platform.MusterPort.
const MusterNodePort = 8090

// BrowserCallbackPort is the fixed local port for `agentlab browser`'s OAuth
// callback. Fixed because it must be pre-registered in Dex's redirectURIs.
const BrowserCallbackPort = 5555

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
	// Host-side port for muster; what Claude Code and the browser dial.
	MusterPort int `yaml:"musterPort"`
	// Chart version of the standalone mcp-kubernetes release.
	MCPKubernetesVersion string `yaml:"mcpKubernetesVersion"`
	// agent-platform-standalone has no chart release yet
	// (giantswarm/agent-platform-standalone#11), so it is vendored from git
	// at this pinned SHA. Once released this becomes an OCI ref.
	APSRepo string `yaml:"apsRepo"`
	APSRef  string `yaml:"apsRef"`
}

type Backstage struct {
	Enabled bool `yaml:"enabled"`
	// Backstage binds this port on the node (hostNetwork) and kind maps the
	// same number onto the host, so the URL is identical on both sides.
	Port  int    `yaml:"port"`
	Image string `yaml:"image"`
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

	Users     []User    `yaml:"users"`
	Platform  Platform  `yaml:"platform"`
	Backstage Backstage `yaml:"backstage"`
}

// Default returns the canonical lab setup: three users, Dex on 32000,
// optional components off (mirrors the old `make up`).
func Default() *Config {
	return &Config{
		ClusterName: "agentlab",
		DexPort:     32000,
		// groups on staticPasswords requires Dex >= v2.45.0; see README.
		DexImage: "ghcr.io/dexidp/dex:v2.45.1",
		Users: []User{
			{Email: "admin@lab.local", Username: "admin", Name: "Lab Admin", Password: "password",
				Groups: []string{"platform-admins", "developers"}},
			{Email: "dev@lab.local", Username: "dev", Name: "Lab Developer", Password: "password",
				Groups: []string{"developers"}},
			{Email: "viewer@lab.local", Username: "viewer", Name: "Lab Viewer", Password: "password",
				Groups: []string{"viewers"}},
		},
		Platform: Platform{
			Enabled:              false,
			MusterPort:           8090,
			MCPKubernetesVersion: "1.0.9",
			APSRepo:              "https://github.com/giantswarm/agent-platform-standalone",
			APSRef:               "672e08cb67cb210cdcc3bb9c5d11f78a42e92003", // PR #11 head (feat/curate-generator)
		},
		Backstage: Backstage{
			Enabled: false,
			Port:    7007,
			Image:   "gsoci.azurecr.io/giantswarm/backstage:0.192.0",
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
	return os.WriteFile(File, append(header, out...), 0o644)
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
	if err := ValidatePort(strconv.Itoa(c.Backstage.Port)); err != nil {
		return fmt.Errorf("backstage.port: %w", err)
	}
	return nil
}

// AdminUser returns the first user in platform-admins: the identity the up
// verification loop and the smoke tests default to.
func (c *Config) AdminUser() *User {
	for i := range c.Users {
		if c.Users[i].HasGroup("platform-admins") {
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

// MCPServerName is the MCPServer CR name the umbrella chart derives from the
// mcpServers entry: <cluster>-mcp-kubernetes. Muster family tool calls take it
// as the management_cluster argument.
func (c *Config) MCPServerName() string { return c.ClusterName + "-mcp-kubernetes" }

func (c *Config) MusterBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Platform.MusterPort)
}

func (c *Config) BackstageBaseURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Backstage.Port)
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
