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
	backstagePort := strconv.Itoa(cfg.Backstage.Port)

	userSummary := func() string {
		lines := make([]string, 0, len(cfg.Users))
		for _, u := range cfg.Users {
			lines = append(lines, fmt.Sprintf("%s (%s)", u.Email, strings.Join(u.Groups, ", ")))
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
	return cfg.Validate()
}

// editUsers rebuilds the user list interactively: each existing user can be
// kept, edited or dropped, then new users can be appended.
func editUsers(cfg *config.Config, accessible bool) error {
	var kept []config.User
	for i := range cfg.Users {
		u := cfg.Users[i]
		action := "keep"
		err := newForm(huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("User %s", u.Email)).
				Description(fmt.Sprintf("groups: %s", strings.Join(u.Groups, ", "))).
				Options(
					huh.NewOption("Keep", "keep"),
					huh.NewOption("Edit", "edit"),
					huh.NewOption("Remove", "remove"),
				).
				Value(&action),
		)).WithAccessible(accessible).Run()
		if err != nil {
			return err
		}
		switch action {
		case "keep":
			kept = append(kept, u)
		case "edit":
			edited, err := userForm(u, accessible)
			if err != nil {
				return err
			}
			kept = append(kept, edited)
		case "remove":
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
