// dexlab is a self-contained OIDC lab: kind + Dex, with users that exist
// nowhere but this cluster, plus the optional Giant Swarm agent platform
// (muster + Kubernetes MCP) and Backstage on top.
//
// The binary embeds every manifest as a template; `dexlab configure` asks for
// the configuration interactively and persists it to dexlab.yaml, and the
// lifecycle commands render + apply from there.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"dexlab/internal/config"
	"dexlab/internal/forms"
	"dexlab/internal/lab"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dexlab",
		Short: "A self-contained OIDC lab: kind + Dex (+ Giant Swarm agent platform + Backstage)",
		Long: `dexlab runs a throwaway OIDC lab on your machine: a kind cluster whose
apiserver trusts a local Dex, users that exist nowhere else, RBAC driven by
the groups claim, and optionally the Giant Swarm agent platform (muster +
Kubernetes MCP) and Backstage wired to the same Dex.

Start with:  dexlab configure   (interactive; asks every option)
Then:        dexlab up          (creates the cluster and deploys everything enabled)`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		configureCmd(),
		upCmd(),
		downCmd(),
		testCmd(),
		loginCmd(),
		browserCmd(),
		reloadCmd(),
		certsCmd(),
		platformCmd(),
		platformTestCmd(),
		platformDownCmd(),
		backstageCmd(),
		backstageTestCmd(),
		logsCmd(),
		renderCmd(),
		postRenderCmd(),
	)
	return root
}

// loadConfig returns the saved configuration; if none exists yet it runs the
// interactive form on a terminal, and otherwise refuses with a pointer to
// `dexlab configure --defaults`.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("no %s found; run `dexlab configure` (or `dexlab configure --defaults` for the canonical lab)", config.File)
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

func configureCmd() *cobra.Command {
	var defaults, accessible bool
	var platform, backstage bool
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Ask for the lab configuration interactively and save dexlab.yaml",
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
				if cmd.Flags().Changed("backstage") {
					cfg.Backstage.Enabled = backstage
					if backstage {
						cfg.Platform.Enabled = true
					}
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
			fmt.Printf("  platform   %v\n", cfg.Platform.Enabled)
			fmt.Printf("  backstage  %v\n", cfg.Backstage.Enabled)
			fmt.Println("\nNext: dexlab up")
			return nil
		},
	}
	cmd.Flags().BoolVar(&defaults, "defaults", false, "skip the form; keep current values (or the canonical defaults)")
	cmd.Flags().BoolVar(&platform, "platform", false, "with --defaults: enable/disable the agent platform")
	cmd.Flags().BoolVar(&backstage, "backstage", false, "with --defaults: enable/disable Backstage (implies the platform)")
	cmd.Flags().BoolVar(&accessible, "accessible", false, "prompt-per-question form mode (for screen readers and plain terminals)")
	return cmd
}

func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Create the kind cluster, deploy Dex (+ enabled components) and verify the OIDC chain",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.Up(cfg)
		},
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Destroy the kind cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.Down(cfg)
		},
	}
}

func testCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Assert RBAC for every configured user (token from Dex, kubectl auth can-i)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.Test(cfg)
		},
	}
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
	cmd.Flags().StringVar(&password, "password", "", "password (default: the one in dexlab.yaml)")
	return cmd
}

func browserCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "browser",
		Short: "Log in through the real Dex login page in a browser (authorization-code flow)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.BrowserLogin(cfg)
		},
	}
}

func reloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Re-render and re-apply the Dex config (after editing dexlab.yaml)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.ApplyDex(cfg)
		},
	}
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

func platformCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "platform",
		Short: "Install the Giant Swarm agent platform (muster + Kubernetes MCP)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.PlatformUp(cfg)
		},
	}
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

func platformDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "platform-down",
		Short: "Remove the agent platform (leaves Dex and the cluster alone)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.PlatformDown(cfg)
		},
	}
}

func backstageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backstage",
		Short: "Deploy Giant Swarm Backstage wired to Dex + muster (needs `dexlab platform`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.BackstageUp(cfg)
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
		Use:       "logs <dex|muster|backstage>",
		Short:     "Tail a component's logs",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"dex", "muster", "backstage"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return lab.Logs(args[0])
		},
	}
}

func renderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "render",
		Short: "Render every manifest from dexlab.yaml into state/ without applying anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.RenderAll(cfg)
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
