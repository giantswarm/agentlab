// Package forms is the interactive face of agentlab: charm.land/huh forms
// that ask for every configuration option and fill in a config.Config.
package forms

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"charm.land/huh/v2"

	"github.com/giantswarm/agentlab/internal/config"
)

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)

// The keep/edit/remove vocabulary shared by the list editors (users, extra
// models).
const (
	actionKeep   = "keep"
	actionEdit   = "edit"
	actionRemove = "remove"
)

// testInput/testOutput let tests drive the real TUI form with a scripted
// keystroke stream instead of a terminal. nil outside of tests.
var (
	testInput  io.Reader
	testOutput io.Writer
)

// newForm wraps huh.NewForm so the test IO overrides apply to every form this
// package builds.
func newForm(groups ...*huh.Group) *huh.Form {
	form := huh.NewForm(groups...)
	if testInput != nil {
		form = form.WithInput(testInput)
	}
	if testOutput != nil {
		form = form.WithOutput(testOutput)
	}
	return form
}

// Run walks the user through the lab configuration and mutates cfg in place.
// The passed cfg provides the defaults shown in the form (so re-configuring
// starts from the current answers, not from scratch).
func Run(cfg *config.Config, accessible bool) error {
	clusterName := cfg.ClusterName
	dexPort := strconv.Itoa(cfg.DexPort)
	dexImage := cfg.DexImage
	customizeUsers := false

	var components []string
	if cfg.Platform.Enabled {
		components = append(components, "platform")
	}
	if cfg.Backstage.Enabled {
		components = append(components, "backstage")
	}
	musterPort := strconv.Itoa(cfg.Platform.MusterPort)
	apsRef := cfg.Platform.APSRef
	agentsEnabled := cfg.Platform.Agents
	observabilityEnabled := cfg.Platform.Observability
	agentsPort := strconv.Itoa(cfg.Platform.AgentsPort)
	aiModel := cfg.AIModel
	customizeModels := false
	backstagePort := strconv.Itoa(cfg.Backstage.Port)

	userSummary := func() string {
		lines := make([]string, 0, len(cfg.Users))
		for _, u := range cfg.Users {
			lines = append(lines, fmt.Sprintf("%s (%s)", u.Email, strings.Join(u.Groups, ", ")))
		}
		return strings.Join(lines, "\n")
	}

	modelSummary := func() string {
		if len(cfg.Platform.ExtraModels) == 0 {
			return "(none)"
		}
		lines := make([]string, 0, len(cfg.Platform.ExtraModels))
		for _, m := range cfg.Platform.ExtraModels {
			lines = append(lines, fmt.Sprintf("%s (%s %s)", m.Name, m.Provider, m.Model))
		}
		return strings.Join(lines, "\n")
	}

	form := newForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Cluster name").
				Description("Names the kind cluster; also prefixes RBAC bindings and the muster installation.").
				Value(&clusterName).
				Validate(config.ValidateClusterName),
			huh.NewInput().
				Title("Dex port").
				Description("The issuer becomes https://localhost:<port>/dex on BOTH sides of the\nkind boundary — one URL for the browser and the apiserver. NodePort range (30000-32767).").
				Value(&dexPort).
				Validate(config.ValidateNodePort),
			huh.NewInput().
				Title("Dex image").
				Description("groups on staticPasswords needs Dex >= v2.45.0.").
				Value(&dexImage).
				Validate(notEmpty),
		).Title("Cluster"),

		huh.NewGroup(
			huh.NewConfirm().
				Title("Customize the lab users?").
				DescriptionFunc(func() string {
					return "Current users:\n" + userSummary() +
						"\n\nGroups map to fixed roles: platform-admins -> cluster-admin,\ndevelopers -> edit in ns/demo, viewers -> view cluster-wide."
				}, &cfg.Users).
				Affirmative("Edit them").
				Negative("Keep as is").
				Value(&customizeUsers),
		).Title("Users"),

		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Components").
				Description("The agent platform is what this lab tests, so it is on by default; deselect it\nfor a bare kind+Dex OIDC sandbox. Deployed by `agentlab up` (or later via\n`agentlab platform` / `agentlab backstage`). Backstage's muster plugin needs\nthe platform, so selecting it selects both.").
				Options(
					huh.NewOption("Giant Swarm agent platform (muster + Kubernetes MCP)", "platform"),
					huh.NewOption("Giant Swarm Backstage (developer portal)", "backstage"),
				).
				Value(&components),
		).Title("Components"),

		huh.NewGroup(
			huh.NewInput().
				Title("muster port on this machine").
				Description("What Claude Code dials: http://localhost:<port>/mcp.").
				Value(&musterPort).
				Validate(config.ValidatePort),
			huh.NewInput().
				Title("agent-platform-standalone git ref").
				Description("The umbrella chart has no release yet; it is vendored from git at this pinned SHA.").
				Value(&apsRef).
				Validate(notEmpty),
			huh.NewConfirm().
				Title("Install the agents runtime (kagent)?").
				Description("Optional: on real clusters agent delivery runs through Flux/GitOps, which\nthis lab does not run. Skip it if you are not exercising agents.").
				Affirmative("Install").
				Negative("Skip").
				Value(&agentsEnabled),
			huh.NewInput().
				Title("kagent UI port on this machine").
				Description("The agents web UI: http://localhost:<port>. The kind mapping exists even\nwith agents off (fixed at cluster creation), so they can be enabled later.").
				Value(&agentsPort).
				Validate(config.ValidatePort),
			huh.NewConfirm().
				Title("Install the observability stack (Prometheus + mcp-prometheus)?").
				Description("Optional: a minimal Prometheus (Giant Swarm kube-prometheus-stack) plus the\nPrometheus MCP server, registered in muster as x_mcp-prometheus_* tools —\nask the platform about pod CPU/memory. ~5 extra pods.").
				Affirmative("Install").
				Negative("Skip").
				Value(&observabilityEnabled),
			huh.NewInput().
				Title("Claude model").
				Description("Used by the platform agents' ModelConfig and Backstage's AI chat.\nThe API key comes from $ANTHROPIC_API_KEY at deploy time, never from this file.").
				Value(&aiModel).
				Validate(config.ValidateAIModel),
			huh.NewConfirm().
				Title("Customize extra model configs?").
				DescriptionFunc(func() string {
					return "Additional kagent ModelConfigs beyond the default Claude one — a self-hosted\nOpenAI-compatible endpoint (vLLM, Ollama), OpenRouter, Gemini or plain OpenAI.\nCurrent extras:\n" + modelSummary()
				}, &cfg.Platform.ExtraModels).
				Affirmative("Edit them").
				Negative("Keep as is").
				Value(&customizeModels),
		).Title("Agent platform").
			WithHideFunc(func() bool { return !slices.Contains(components, "platform") }),

		huh.NewGroup(
			huh.NewInput().
				Title("Backstage port").
				Description("Backstage binds this port on the kind node (hostNetwork) and kind maps the\nsame number onto this machine, so the URL is identical on both sides.").
				Value(&backstagePort).
				Validate(config.ValidatePort),
		).Title("Backstage").
			WithHideFunc(func() bool { return !slices.Contains(components, "backstage") }),
	).WithAccessible(accessible)

	if err := form.Run(); err != nil {
		return err
	}

	cfg.ClusterName = clusterName
	cfg.DexPort = mustAtoi(dexPort)
	cfg.DexImage = dexImage
	cfg.Backstage.Enabled = slices.Contains(components, "backstage")
	cfg.Platform.Enabled = slices.Contains(components, "platform")
	cfg.Normalize() // backstage implies the platform
	cfg.Platform.MusterPort = mustAtoi(musterPort)
	cfg.Platform.APSRef = apsRef
	cfg.Platform.Agents = agentsEnabled
	cfg.Platform.Observability = observabilityEnabled
	cfg.Platform.AgentsPort = mustAtoi(agentsPort)
	cfg.AIModel = aiModel
	cfg.Backstage.Port = mustAtoi(backstagePort)

	if customizeUsers {
		if err := editUsers(cfg, accessible); err != nil {
			return err
		}
	}
	if customizeModels {
		if err := editExtraModels(cfg, accessible); err != nil {
			return err
		}
	}
	return cfg.Validate()
}

// editUsers rebuilds the user list interactively: each existing user can be
// kept, edited or dropped, then new users can be appended.
func editUsers(cfg *config.Config, accessible bool) error {
	var kept []config.User
	for i := range cfg.Users {
		u := cfg.Users[i]
		action := actionKeep
		err := newForm(huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("User %s", u.Email)).
				Description(fmt.Sprintf("groups: %s", strings.Join(u.Groups, ", "))).
				Options(
					huh.NewOption("Keep", actionKeep),
					huh.NewOption("Edit", actionEdit),
					huh.NewOption("Remove", actionRemove),
				).
				Value(&action),
		)).WithAccessible(accessible).Run()
		if err != nil {
			return err
		}
		switch action {
		case actionKeep:
			kept = append(kept, u)
		case actionEdit:
			edited, err := userForm(u, accessible)
			if err != nil {
				return err
			}
			kept = append(kept, edited)
		case actionRemove:
		}
	}

	for {
		addMore := false
		err := newForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Add another user?").
				Description(fmt.Sprintf("%d user(s) so far. At least one must be in platform-admins.", len(kept))).
				Value(&addMore),
		)).WithAccessible(accessible).Run()
		if err != nil {
			return err
		}
		if !addMore {
			break
		}
		u, err := userForm(config.User{Password: "password", Groups: []string{"developers"}}, accessible)
		if err != nil {
			return err
		}
		kept = append(kept, u)
	}

	cfg.Users = kept
	return nil
}

func userForm(u config.User, accessible bool) (config.User, error) {
	email := u.Email
	name := u.Name
	password := u.Password
	groups := slices.Clone(u.Groups)

	form := newForm(huh.NewGroup(
		huh.NewInput().
			Title("Email").
			Description("The username claim; the local part becomes the Backstage identity.").
			Value(&email).
			Validate(func(s string) error {
				if !emailRe.MatchString(s) {
					return fmt.Errorf("not an email address")
				}
				return nil
			}),
		huh.NewInput().
			Title("Display name").
			Value(&name).
			Validate(notEmpty),
		huh.NewInput().
			Title("Password").
			Description("Lab-only; stored in agentlab.yaml alongside its bcrypt hash.").
			Value(&password).
			Validate(notEmpty),
		huh.NewMultiSelect[string]().
			Title("Groups").
			Description("platform-admins -> cluster-admin, developers -> edit in ns/demo, viewers -> view.").
			Options(huh.NewOptions(config.Groups...)...).
			Value(&groups).
			Validate(func(sel []string) error {
				if len(sel) == 0 {
					return fmt.Errorf("pick at least one group")
				}
				return nil
			}),
	)).WithAccessible(accessible)

	if err := form.Run(); err != nil {
		return config.User{}, err
	}

	u.Email = email
	u.Name = name
	u.Password = password
	u.PasswordHash = "" // recomputed on save
	u.Groups = orderGroups(groups)
	u.Username = strings.SplitN(email, "@", 2)[0]
	return u, nil
}

// editExtraModels rebuilds the extra-model list interactively, mirroring
// editUsers: each existing entry can be kept, edited or dropped, then new
// ones can be appended.
func editExtraModels(cfg *config.Config, accessible bool) error {
	var kept []config.ExtraModel
	for i := range cfg.Platform.ExtraModels {
		m := cfg.Platform.ExtraModels[i]
		action := actionKeep
		err := newForm(huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Model %s", m.Name)).
				Description(fmt.Sprintf("%s %s%s", m.Provider, m.Model, baseURLNote(m.BaseURL))).
				Options(
					huh.NewOption("Keep", actionKeep),
					huh.NewOption("Edit", actionEdit),
					huh.NewOption("Remove", actionRemove),
				).
				Value(&action),
		)).WithAccessible(accessible).Run()
		if err != nil {
			return err
		}
		switch action {
		case actionKeep:
			kept = append(kept, m)
		case actionEdit:
			edited, err := extraModelForm(m, accessible)
			if err != nil {
				return err
			}
			kept = append(kept, edited)
		case actionRemove:
		}
	}

	for {
		addMore := false
		err := newForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Add another model config?").
				Description(fmt.Sprintf("%d extra model(s) so far. Removed ones are pruned from the cluster on the next run.", len(kept))).
				Value(&addMore),
		)).WithAccessible(accessible).Run()
		if err != nil {
			return err
		}
		if !addMore {
			break
		}
		m, err := extraModelForm(config.ExtraModel{Provider: config.ProviderOpenAI}, accessible)
		if err != nil {
			return err
		}
		kept = append(kept, m)
	}

	cfg.Platform.ExtraModels = kept
	return nil
}

func baseURLNote(u string) string {
	if u == "" {
		return ""
	}
	return " @ " + u
}

func extraModelForm(m config.ExtraModel, accessible bool) (config.ExtraModel, error) {
	name := m.Name
	provider := m.Provider
	model := m.Model
	baseURL := m.BaseURL
	apiKeyEnv := m.APIKeyEnv
	insecure := m.InsecureTLS

	form := newForm(huh.NewGroup(
		huh.NewInput().
			Title("Name").
			Description("Names the ModelConfig CR and its key Secret (kagent-<name>).").
			Value(&name).
			Validate(config.ValidateModelName),
		huh.NewSelect[string]().
			Title("Provider").
			Description("OpenAI also covers every OpenAI-compatible endpoint (vLLM, OpenRouter, ...)\nvia the base URL below.").
			Options(huh.NewOptions(config.ModelProviderNames...)...).
			Value(&provider),
		huh.NewInput().
			Title("Model").
			Description("The model id the endpoint serves, e.g. qwen3-8-27b or deepseek/deepseek-chat.").
			Value(&model).
			Validate(notEmpty),
		huh.NewInput().
			Title("Base URL").
			Description("Endpoint override: vLLM http://host:8000/v1, OpenRouter\nhttps://openrouter.ai/api/v1. Required for Ollama, not applicable to Gemini.").
			Value(&baseURL).
			Validate(func(s string) error {
				switch {
				case provider == config.ProviderOllama && s == "":
					return fmt.Errorf("required for Ollama")
				case provider == config.ProviderGemini && s != "":
					return fmt.Errorf("not applicable to Gemini")
				}
				return nil
			}),
		huh.NewInput().
			Title("API key env var").
			Description("Host env var read at deploy time (e.g. OPENROUTER_API_KEY); the value lands\nonly in the Secret. Empty = keyless endpoint (a placeholder key is shipped).").
			Value(&apiKeyEnv).
			Validate(func(s string) error {
				if provider == config.ProviderOllama && s != "" {
					return fmt.Errorf("keyless provider — an API key env would be silently ignored")
				}
				return config.ValidateAPIKeyEnv(s)
			}),
		huh.NewConfirm().
			Title("Skip TLS verification?").
			Description("Only for self-hosted https endpoints with self-signed certificates.").
			Affirmative("Skip verification").
			Negative("Verify").
			Value(&insecure),
	)).WithAccessible(accessible)

	if err := form.Run(); err != nil {
		return config.ExtraModel{}, err
	}

	m.Name = name
	m.Provider = provider
	m.Model = model
	m.BaseURL = baseURL
	m.APIKeyEnv = apiKeyEnv
	m.InsecureTLS = insecure
	return m, m.Validate()
}

// orderGroups keeps the canonical group order regardless of selection order,
// so renders stay deterministic.
func orderGroups(sel []string) []string {
	var out []string
	for _, g := range config.Groups {
		if slices.Contains(sel, g) {
			out = append(out, g)
		}
	}
	return out
}

func notEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("required")
	}
	return nil
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(fmt.Sprintf("validated input %q is not a number", s))
	}
	return n
}
