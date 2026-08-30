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

	"agentlab/internal/config"
	"agentlab/internal/forms"
	"agentlab/internal/lab"
	"agentlab/internal/tui"
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
Then:        claude mcp add --transport http muster http://localhost:8090/mcp

Running agentlab with no arguments on a terminal opens the dashboard (also:
agentlab tui): live component status plus keys for the lifecycle actions.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		// Bare `agentlab` on a terminal is the dashboard; piped/CI invocations
		// get the usage text instead of a hung TUI.
		RunE: func(cmd *cobra.Command, args []string) error {
			if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return cmd.Help()
			}
			return runTUI()
		},
	}

	root.AddCommand(
		tuiCmd(),
		configureCmd(),
		labCmd("up", "Create the kind cluster, deploy Dex and the enabled components, and verify the OIDC chain", lab.Up),
		labCmd("down", "Destroy the kind cluster", lab.Down),
		labCmd("test", "Assert RBAC for every configured user (token from Dex, kubectl auth can-i)", lab.Test),
		loginCmd(),
		labCmd("browser", "Log in through the real Dex login page in a browser (authorization-code flow)", lab.BrowserLogin),
		labCmd("reload", "Re-render and re-apply the Dex config (after editing agentlab.yaml)", lab.ApplyDex),
		certsCmd(),
		labCmd("platform", "Install the Giant Swarm agent platform (muster + Kubernetes MCP)", lab.PlatformUp),
		platformTestCmd(),
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
	if err := forms.Run(cfg, accessibleMode()); err != nil {
		return nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	fmt.Printf("Saved %s.\n\n", config.File)
	return cfg, nil
}

// accessibleMode switches huh to its prompt-per-question accessible mode;
// also what screen readers want.
func accessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Interactive dashboard: live status, lifecycle actions, URLs, users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI()
		},
	}
}

func runTUI() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	return tui.Run(cfg)
}

func configureCmd() *cobra.Command {
	var defaults, accessible bool
	var platform, agents, backstage bool
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Ask for the lab configuration interactively and save agentlab.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if errors.Is(err, os.ErrNotExist) {
				cfg = config.Default()
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
			fmt.Printf("  platform   %v (agents %v)\n", cfg.Platform.Enabled, cfg.Platform.Agents)
			fmt.Printf("  backstage  %v\n", cfg.Backstage.Enabled)
			fmt.Printf("  ai model   %s (key from $%s at deploy time)\n", cfg.AIModel, lab.AnthropicKeyEnv)
			fmt.Println("\nNext: agentlab up")
			return nil
		},
	}
	cmd.Flags().BoolVar(&defaults, "defaults", false, "skip the form; keep current values (or the canonical defaults)")
	cmd.Flags().BoolVar(&platform, "platform", false, "with --defaults: enable/disable the agent platform")
	cmd.Flags().BoolVar(&agents, "agents", false, "with --defaults: enable/disable the agents runtime (kagent, part of the platform install)")
	cmd.Flags().BoolVar(&backstage, "backstage", false, "with --defaults: enable/disable Backstage (implies the platform)")
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
		Short: "Generate the lab CA and Dex server cert (no-op if present)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return lab.GenCerts(force)
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
