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
	"slices"
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
		agentsTestCmd(),
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
	disc := discoverInto(cfg, nil, nil)
	if err := forms.Run(cfg, accessibleMode(), forms.Hints{ModelServers: disc.ModelServersHint()}); err != nil {
		return nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	fmt.Printf("Saved %s.\n\n", config.File)
	return cfg, nil
}

// discoverInto probes this machine (lab.Discover), prints what it found, and
// applies it to cfg — on EVERY `agentlab configure` run, so agentlab.yaml
// follows the host: the ports move off foreign listeners while no cluster
// holds them (an existing cluster's mappings are fixed, so conflicts are
// reported instead), and platform.modelManager.backends becomes the host
// model servers that answer (pins from the --model-manager flags win).
// Reachability from pods (bind address, firewall) is checked at platform
// time, with the fixes.
func discoverInto(cfg *config.Config, pinEnabled *bool, pinBackends []string) *lab.Discovery {
	disc := lab.Discover(cfg)
	fmt.Print(disc.Report(cfg))
	fmt.Println()
	applyPorts(cfg, disc)
	before := cfg.Platform.ModelManager
	cfg.Platform.ModelManager.ApplyDiscovered(disc.Backends(), cfg.Platform.Agents, pinEnabled, pinBackends)
	reportModelManager(before, cfg.Platform.ModelManager, disc, cfg.Platform.Agents)
	return disc
}

// applyPorts moves the configuration's host ports off foreign listeners when
// no kind node of this configuration exists (fresh, or after `agentlab
// down`); with the cluster in place its port mappings are fixed at node
// creation, so a foreign listener on one of them is reported, not renumbered
// around — the lab's own published ports never count as occupied.
func applyPorts(cfg *config.Config, disc *lab.Discovery) {
	if !disc.ClusterExists {
		reportPortChanges(cfg.ChooseFreePorts(disc.ClusterPorts))
		return
	}
	conflicts := cfg.PortConflicts(disc.ClusterPorts)
	if len(conflicts) == 0 {
		return
	}
	fmt.Println("Ports of the existing cluster are held by other processes on this machine:")
	for _, c := range conflicts {
		fmt.Printf("  %s\n", c)
	}
	fmt.Println("  The kind node cannot bind them while they are taken. Free the port, or `agentlab down`,")
	fmt.Println("  re-run `agentlab configure` (which then picks free ones) and `agentlab up`.")
	fmt.Println()
}

// reportPortChanges tells the user which ports were already occupied on this
// machine and what the configuration uses instead. The form (or the saved
// file) shows the adjusted numbers, but only this message explains why they
// differ from the documented defaults.
func reportPortChanges(changes []config.PortChange) {
	if len(changes) == 0 {
		return
	}
	fmt.Println("Some ports are already in use on this machine; picked free ones:")
	for _, ch := range changes {
		fmt.Printf("  %s\n", ch)
	}
	fmt.Println()
}

// reportModelManager says what the discovery did to platform.modelManager.
func reportModelManager(before, after config.ModelManager, disc *lab.Discovery, agents bool) {
	var lines []string
	if !slices.Equal(before.Backends, after.Backends) {
		lines = append(lines, fmt.Sprintf("platform.modelManager.backends: %s -> %s", listOrNone(before.Backends), listOrNone(after.Backends)))
	}
	if before.Enabled != after.Enabled {
		reason := "a host model server answers"
		switch {
		case !after.Enabled && !agents:
			reason = "the agents runtime is off, and model-manager wires models into it"
		case !after.Enabled:
			reason = "no host model server answers on this machine"
		}
		lines = append(lines, fmt.Sprintf("platform.modelManager.enabled: %v -> %v (%s)", before.Enabled, after.Enabled, reason))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Println("Applied to the configuration:")
	for _, l := range lines {
		fmt.Printf("  %s\n", l)
	}
	if len(disc.Servers) > 0 && !after.Enabled && agents {
		fmt.Println("  (managed models pinned off — `agentlab configure --defaults --model-manager` turns them on)")
	}
	fmt.Println()
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return "[" + strings.Join(items, ", ") + "]"
}

// accessibleMode switches huh to its prompt-per-question accessible mode;
// also what screen readers want.
func accessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

func configureCmd() *cobra.Command {
	var defaults, accessible bool
	var platform, agents, observability, backstage, modelManager bool
	var modelManagerBackends []string
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Discover this machine, then ask for the lab configuration (or keep it with --defaults) and save agentlab.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if errors.Is(err, os.ErrNotExist) {
				cfg = config.Default()
			} else if err != nil {
				return err
			}
			// The component flags first: what the discovery applies depends
			// on them (managed models need the agents runtime).
			if cmd.Flags().Changed("platform") {
				cfg.Platform.Enabled = platform
			}
			if cmd.Flags().Changed("agents") {
				cfg.Platform.Agents = agents
			}
			if cmd.Flags().Changed("observability") {
				cfg.Platform.Observability = observability
			}
			if cmd.Flags().Changed("backstage") {
				cfg.Backstage.Enabled = backstage
				cfg.Normalize() // backstage implies the platform
			}
			var pinEnabled *bool
			if cmd.Flags().Changed("model-manager") {
				pinEnabled = &modelManager
			}
			var pinBackends []string
			if cmd.Flags().Changed("model-manager-backends") {
				pinBackends = modelManagerBackends
			}
			// Every run discovers the machine — an existing agentlab.yaml
			// follows the host too: a server that appeared is added, one that
			// is gone drops out, ports move while no cluster holds them.
			disc := discoverInto(cfg, pinEnabled, pinBackends)
			if defaults {
				if err := cfg.Validate(); err != nil {
					return err
				}
			} else {
				if err := forms.Run(cfg, accessible || accessibleMode(), forms.Hints{ModelServers: disc.ModelServersHint()}); err != nil {
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
				mm := cfg.Platform.ModelManager
				primary := mm.Primary()
				fmt.Printf("  models     model-manager fronts the host %s (%s backend, %s)\n",
					config.BackendServerName(primary), primary, endpointNote(mm, primary))
				for _, b := range mm.Secondary() {
					fmt.Printf("             %s (%s): tool-calling models wired as ModelConfigs at platform time — statically, model-manager fronts one backend per instance today\n",
						config.BackendServerName(b), endpointNote(mm, b))
				}
			}
			fmt.Println("\nNext: agentlab up")
			return nil
		},
	}
	cmd.Flags().BoolVar(&defaults, "defaults", false, "skip the form; keep current values (or the canonical defaults) plus what the discovery finds")
	cmd.Flags().BoolVar(&platform, "platform", false, "enable/disable the agent platform")
	cmd.Flags().BoolVar(&agents, "agents", false, "enable/disable the agents runtime (kagent, part of the platform install)")
	cmd.Flags().BoolVar(&observability, "observability", false, "enable/disable the observability stack (Prometheus + mcp-prometheus)")
	cmd.Flags().BoolVar(&backstage, "backstage", false, "enable/disable Backstage (implies the platform)")
	cmd.Flags().BoolVar(&modelManager, "model-manager", false, "pin managed models on/off instead of following the host model servers the discovery finds (needs agents)")
	cmd.Flags().StringSliceVar(&modelManagerBackends, "model-manager-backends", nil, "pin the host model servers, in order (ollama, lemonade; the first is model-manager's backend) instead of the ones the discovery finds")
	cmd.Flags().BoolVar(&accessible, "accessible", false, "prompt-per-question form mode (for screen readers and plain terminals)")
	return cmd
}

// endpointNote says where a backend is dialed: the configured override or
// the autodetection.
func endpointNote(mm config.ModelManager, backend string) string {
	if ep := mm.EndpointFor(backend); ep != "" {
		return ep
	}
	return fmt.Sprintf("autodetected as http://<kind docker gateway>:%d at platform time", config.BackendPort(backend))
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

func agentsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agents-test [email]",
		Short: "Headless agent-manager proof through muster: get_info (caller) -> create -> ready -> update -> delete as the admin, a viewer's create Forbidden by the apiserver, the ServiceAccount without RBAC",
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
			return lab.AgentsTest(cfg, email)
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
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.Logs(cfg, args[0])
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
			// The Dex NodePort from agentlab.yaml when the lab is configured;
			// the default otherwise (the plugin runs in the lab's cwd).
			dexPort := config.DefaultDexPort
			if cfg, err := config.Load(); err == nil && cfg.DexPort != 0 {
				dexPort = cfg.DexPort
			}
			return lab.PostRender(os.Stdin, os.Stdout, dexPort)
		},
	}
}
