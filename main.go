// agentlab is a local lab for the Giant Swarm agent platform: muster + the
// Kubernetes MCP (and optionally Backstage) on a throwaway kind cluster, with
// a bundled Dex as the OIDC provider — users that exist nowhere but this
// cluster.
//
// The binary embeds every manifest as a template; `agentlab configure` asks for
// the configuration interactively and persists it to agentlab.yaml, and the
// lifecycle commands render + apply from there.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/forms"
	"github.com/giantswarm/agentlab/internal/lab"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "agentlab",
		Short: "A local lab for the Giant Swarm agent platform (muster + Kubernetes MCP + Backstage), on kind + Dex",
		Long: `agentlab runs the Giant Swarm agent platform on your machine so it can be
tested end to end: muster and the Kubernetes MCP (plus optionally Backstage)
on a throwaway kind cluster. A bundled Dex provides the identity — users that
exist nowhere else, RBAC driven by the groups claim, the apiserver and the
platform trusting the same issuer.

Start with:  agentlab configure   (interactive; asks every option)
Then:        agentlab up          (cluster + Dex + the platform, verified end to end)
Then:        agentlab trust       (once: the lab CA into the trust stores — green locks)
Then:        claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
	}

	root.AddCommand(
		configureCmd(),
		labCmd("up", "Create the kind cluster, deploy Dex and the enabled components, and verify the OIDC chain", lab.Up),
		labCmd("down", "Destroy the kind cluster", lab.Down),
		labCmd("test", "Assert RBAC for every configured user (token from Dex, kubectl auth can-i)", lab.Test),
		loginCmd(),
		labCmd("browser", "Log in through the real Dex login page in a browser (authorization-code flow)", lab.BrowserLogin),
		labCmd("reload", "Re-render and re-apply the Dex config (after editing agentlab.yaml)", lab.ApplyDex),
		certsCmd(),
		labCmd("trust", "Install the lab CA into the system and browser trust stores (one sudo prompt; reversible)", lab.Trust),
		labCmd("untrust", "Remove the lab CA from the system and browser trust stores", lab.Untrust),
		labCmd("platform", "Install the Giant Swarm agent platform (muster + Kubernetes MCP)", lab.PlatformUp),
		platformTestCmd(),
		modelsTestCmd(),
		labCmd("platform-down", "Remove the agent platform (leaves Dex and the cluster alone)", lab.PlatformDown),
		labCmd("backstage", "Retired: Backstage deploys with the platform now (backstage.enabled + `agentlab up`)", lab.BackstageUp),
		backstageTestCmd(),
		logsCmd(),
		labCmd("render", "Render every manifest from agentlab.yaml into state/ without applying anything", lab.RenderAll),
		postRenderCmd(),
	)
	return root
}

// labCmd wires a no-arg lifecycle command: load (or interactively create) the
// config, then hand it to the lab function. Commands with flags or positional
// args keep their own constructors below.
func labCmd(use, short string, run func(*config.Config) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return run(cfg)
		},
	}
}

// loadConfig returns the saved configuration; if none exists yet it runs the
// interactive form on a terminal, and otherwise refuses with a pointer to
// `agentlab configure --defaults`.
func loadConfig() (*config.Config, error) {
	cfg, err := loadOrCreateConfig()
	if err != nil {
		return nil, err
	}
	// The lab's HTTP clients dial *.<domain> on loopback (the kind port
	// mappings) so checks never flake on external DNS; see lab.SetDomain.
	lab.SetDomain(cfg.Platform.Domain)
	return cfg, nil
}

func loadOrCreateConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("no %s found; run `agentlab configure` (or `agentlab configure --defaults` for the canonical lab)", config.File)
	}
	fmt.Printf("No %s yet — let's create one.\n\n", config.File)
	cfg = config.Default()
	reportPortChanges(cfg.ChooseFreePorts())
	detectModelManager(cfg)
	if err := forms.Run(cfg, accessibleMode()); err != nil {
		return nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	fmt.Printf("Saved %s.\n\n", config.File)
	return cfg, nil
}

// reportPortChanges tells the user which default ports were already occupied
// on this machine and what the fresh configuration uses instead. The form (or
// the saved file) shows the adjusted numbers, but only this message explains
// why they differ from the documented defaults.
func reportPortChanges(changes []config.PortChange) {
	if len(changes) == 0 {
		return
	}
	fmt.Println("Some default ports are already in use on this machine; picked free ones:")
	for _, ch := range changes {
		fmt.Printf("  %s\n", ch)
	}
	fmt.Println()
}

// detectModelManager turns managed models on for a FRESH configuration when
// an Ollama answers on this machine: the lab then installs the umbrella's
// model-manager component with the Ollama backend. Reachability from pods
// (bind address, firewall) is checked at platform time, with the fixes.
func detectModelManager(cfg *config.Config) {
	version, ok := lab.DetectHostOllama()
	if !ok {
		return
	}
	cfg.Platform.ModelManager.Enabled = true
	fmt.Printf("Found Ollama %s on this machine: managed models (model-manager, Ollama backend) enabled.\n", version)
	fmt.Printf("Turn it off with `agentlab configure --defaults --model-manager=false`.\n\n")
}

// accessibleMode switches huh to its prompt-per-question accessible mode;
// also what screen readers want.
func accessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

func configureCmd() *cobra.Command {
	var defaults, accessible bool
	var platform, agents, observability, backstage, modelManager bool
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Ask for the lab configuration interactively and save agentlab.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if errors.Is(err, os.ErrNotExist) {
				// Only a FRESH config gets its ports moved off occupied
				// defaults: an existing agentlab.yaml describes a cluster
				// whose kind mappings are already fixed (and would count as
				// "occupied" themselves while the lab runs). Same for the
				// model-manager detection: an existing file keeps its choice.
				cfg = config.Default()
				reportPortChanges(cfg.ChooseFreePorts())
				detectModelManager(cfg)
			} else if err != nil {
				return err
			}
			if defaults {
				if cmd.Flags().Changed("platform") {
					cfg.Platform.Enabled = platform
				}
				if cmd.Flags().Changed("agents") {
					cfg.Platform.Agents = agents
				}
				if cmd.Flags().Changed("observability") {
					cfg.Platform.Observability = observability
				}
				if cmd.Flags().Changed("model-manager") {
					cfg.Platform.ModelManager.Enabled = modelManager
				}
				if cmd.Flags().Changed("backstage") {
					cfg.Backstage.Enabled = backstage
					cfg.Normalize() // backstage implies the platform
				}
				if err := cfg.Validate(); err != nil {
					return err
				}
			} else {
				if err := forms.Run(cfg, accessible || accessibleMode()); err != nil {
					return err
				}
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("Saved %s:\n", config.File)
			fmt.Printf("  cluster    %s (Dex on %s)\n", cfg.ClusterName, cfg.Issuer())
			fmt.Printf("  users      %d\n", len(cfg.Users))
			fmt.Printf("  platform   %v (agents %v, observability %v)\n", cfg.Platform.Enabled, cfg.Platform.Agents, cfg.Platform.Observability)
			fmt.Printf("  backstage  %v\n", cfg.Backstage.Enabled)
			fmt.Printf("  ai model   %s (key from $%s at deploy time)\n", cfg.AIModel, lab.AnthropicKeyEnv)
			for _, m := range cfg.Platform.ExtraModels {
				fmt.Printf("  extra model %s (%s %s)\n", m.Name, m.Provider, m.Model)
			}
			if cfg.ModelManagerEnabled() {
				endpoint := cfg.Platform.ModelManager.Endpoint
				if endpoint == "" {
					endpoint = "autodetected from the kind docker network at platform time"
				}
				fmt.Printf("  models     managed by model-manager (%s backend, %s)\n", cfg.Platform.ModelManager.Backend, endpoint)
			}
			fmt.Println("\nNext: agentlab up")
			return nil
		},
	}
	cmd.Flags().BoolVar(&defaults, "defaults", false, "skip the form; keep current values (or the canonical defaults)")
	cmd.Flags().BoolVar(&platform, "platform", false, "with --defaults: enable/disable the agent platform")
	cmd.Flags().BoolVar(&agents, "agents", false, "with --defaults: enable/disable the agents runtime (kagent, part of the platform install)")
	cmd.Flags().BoolVar(&observability, "observability", false, "with --defaults: enable/disable the observability stack (Prometheus + mcp-prometheus)")
	cmd.Flags().BoolVar(&backstage, "backstage", false, "with --defaults: enable/disable Backstage (implies the platform)")
	cmd.Flags().BoolVar(&modelManager, "model-manager", false, "with --defaults: enable/disable managed models (the model-manager component with the host Ollama; needs agents)")
	cmd.Flags().BoolVar(&accessible, "accessible", false, "prompt-per-question form mode (for screen readers and plain terminals)")
	return cmd
}

func loginCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "login [email]",
		Short: "Headless login (password grant); writes .token and kubeconfig.oidc",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			pw := password
			if pw == "" {
				if u := cfg.FindUser(email); u != nil {
					pw = u.Password
				} else {
					return fmt.Errorf("no user %q in %s (pass --password for an ad-hoc one)", email, config.File)
				}
			}
			return lab.Login(cfg, email, pw)
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "password (default: the one in agentlab.yaml)")
	return cmd
}

func certsCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "certs",
		Short: "Generate the lab CA and Dex server cert (re-mints only what config/policy require)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.GenCerts(cfg.Platform.Domain, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "regenerate even if certs/ exists (breaks a running cluster's trust)")
	return cmd
}

func platformTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "platform-test [email]",
		Short: "Headless Dex -> muster -> Kubernetes MCP proof",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.PlatformTest(cfg, email)
		},
	}
}

func modelsTestCmd() *cobra.Command {
	var model string
	cmd := &cobra.Command{
		Use:   "models-test [email]",
		Short: "Headless managed-models proof: 401 without a token, then pull -> ModelConfig -> agent turn -> MCP via muster -> unload -> delete",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.ModelsTest(cfg, email, model)
		},
	}
	cmd.Flags().StringVar(&model, "model", lab.ModelsTestModel, "the Ollama model to pull and delete (small and tool-calling capable)")
	return cmd
}

func backstageTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backstage-test [email...]",
		Short: "Headless Backstage sign-in + muster proof (default: every user)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.BackstageTest(cfg, args)
		},
	}
}

func logsCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "logs <" + strings.Join(lab.LogComponents(), "|") + ">",
		Short:     "Tail a component's logs",
		Args:      cobra.ExactArgs(1),
		ValidArgs: lab.LogComponents(),
		RunE: func(cmd *cobra.Command, args []string) error {
			return lab.Logs(args[0])
		},
	}
}

func postRenderCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "post-render",
		Hidden: true,
		Short:  "Helm post-renderer for the agent-platform install (stdin -> stdout)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return lab.PostRender(os.Stdin, os.Stdout)
		},
	}
}
