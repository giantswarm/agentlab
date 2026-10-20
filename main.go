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
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/forms"
	"github.com/giantswarm/agentlab/internal/lab"
	"github.com/giantswarm/agentlab/internal/labs"
	"github.com/giantswarm/agentlab/internal/telemetry"
	"github.com/giantswarm/agentlab/internal/update"
	"github.com/giantswarm/agentlab/pkg/project"
)

func main() {
	err := rootCmd().Execute()
	// After Execute rather than in a PersistentPostRun, which cobra skips
	// when the command failed: the usage signal counts failed runs too.
	telemetry.Flush(context.Background())
	if errors.Is(err, update.ErrOutdated) {
		// `self-update --check` has reported both versions; the status is
		// the answer (devctl's `version check` exits the same way).
		os.Exit(125)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	// Commands show up in the order they are registered below, not
	// alphabetically: inside a group the order is the order a person needs
	// them (`up` before `configure`, `open` before `logs`).
	cobra.EnableCommandSorting = false

	root := &cobra.Command{
		Use:   "agentlab",
		Short: "A local lab for the Giant Swarm agent platform (muster + Kubernetes MCP + Backstage), on kind + Dex",
		Long: `agentlab runs the Giant Swarm agent platform on your machine so it can be
tested end to end: muster and the Kubernetes MCP (plus optionally Backstage)
on a throwaway kind cluster. A bundled Dex provides the identity — users that
exist nowhere else, RBAC driven by the groups claim, the apiserver and the
platform trusting the same issuer.

Start with:  agentlab up            (cluster + Dex + the platform, verified end to end;
                                    it asks whether to trust the lab CA and to open the portal)
Then:        agentlab open portal   (the portal in your browser)
Customize:   agentlab configure     (interactive; asks every option)
Claude Code: claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		Version:       project.VersionLine(),
		// Runs for every subcommand, none of which has a PersistentPreRun of
		// its own: one anonymous usage signal per command a person runs, like
		// kubectl-gs (docs/telemetry.md; AGENTLAB_TELEMETRY_OPTOUT=1 to
		// disable; main gives it a bounded moment to be delivered once the
		// command is done), and the hint that a newer release exists, ahead of the
		// command's own output (docs/cli.md "Keeping agentlab current";
		// AGENTLAB_NO_UPDATE_CHECK=1 to disable). Plumbing, completion and
		// help stay quiet for both; self-update reports the versions itself.
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			telemetry.Command(cmd)
			if telemetry.UserFacing(cmd) && cmd.Name() != "self-update" {
				update.Remind(cmd.Context(), cmd.ErrOrStderr())
			}
		},
	}

	// The help groups, in the order a person meets them. docs/cli.md carries
	// the same groups in the same order.
	root.AddGroup(
		&cobra.Group{ID: groupSetup, Title: "Setup"},
		&cobra.Group{ID: groupEveryday, Title: "Everyday"},
		&cobra.Group{ID: groupTesting, Title: "Testing"},
		&cobra.Group{ID: groupCleanup, Title: "Cleanup"},
		&cobra.Group{ID: groupAdvanced, Title: "Advanced"},
	)
	root.PersistentFlags().StringVar(&labFlag, "lab", "", "the registered lab to run against (its clusterName), from any directory; default: the lab the agentlab.yaml here names; no lab is ever implied")
	root.SetHelpCommandGroupID(groupAdvanced)
	root.SetCompletionCommandGroupID(groupAdvanced)

	root.AddCommand(
		inGroup(groupSetup, upCmd()),
		inGroup(groupSetup, configureCmd()),
		inGroup(groupSetup, labCmd("trust", "Install the lab CA into the system and browser trust stores (one sudo prompt; reversible)", lab.Trust)),

		inGroup(groupEveryday, openCmd()),
		inGroup(groupEveryday, listCmd()),
		inGroup(groupEveryday, statusCmd()),
		inGroup(groupEveryday, podsCmd()),
		inGroup(groupEveryday, logsCmd()),
		inGroup(groupEveryday, loginCmd()),
		inGroup(groupEveryday, turnCmd()),

		inGroup(groupTesting, labCmd("test", "Assert RBAC for every configured user (token from Dex, kubectl auth can-i)", lab.Test)),
		inGroup(groupTesting, platformTestCmd()),
		inGroup(groupTesting, agentsTestCmd()),
		inGroup(groupTesting, agentsGitOpsTestCmd()),
		inGroup(groupTesting, tenantsTestCmd()),
		inGroup(groupTesting, toolsetsTestCmd()),
		inGroup(groupTesting, modelsTestCmd()),
		inGroup(groupTesting, pmTestCmd()),
		inGroup(groupTesting, servingTestCmd()),
		inGroup(groupTesting, vmManagerTestCmd()),
		inGroup(groupTesting, workspacesTestCmd()),
		inGroup(groupTesting, workspacesMeasureCmd()),
		inGroup(groupTesting, codingTestCmd()),
		inGroup(groupTesting, egressTestCmd()),
		inGroup(groupTesting, workspacesSignInTestCmd()),
		inGroup(groupTesting, skillsTestCmd()),
		inGroup(groupTesting, substrateStepTestCmd()),
		inGroup(groupTesting, kagentStepTestCmd()),
		inGroup(groupTesting, substrateTunnelTestCmd()),
		inGroup(groupTesting, a2aTestCmd()),
		inGroup(groupTesting, klausGatewayTestCmd()),
		inGroup(groupTesting, decisionsTestCmd()),
		inGroup(groupTesting, beekeeperDecisionsTestCmd()),
		inGroup(groupTesting, beekeeperCentralTestCmd()),
		inGroup(groupTesting, backstageTestCmd()),

		inGroup(groupCleanup, labCmd("down", "Destroy the kind cluster", lab.Down)),
		inGroup(groupCleanup, labCmd("untrust", "Remove the lab CA from the system and browser trust stores", lab.Untrust)),
		inGroup(groupCleanup, labCmd("platform-down", "Remove the agent platform (leaves Dex and the cluster alone)", lab.PlatformDown)),

		inGroup(groupAdvanced, platformCmd()),
		inGroup(groupAdvanced, certsCmd()),
		inGroup(groupAdvanced, renderCmd()),
		inGroup(groupAdvanced, labCmd("reload", "Re-render and re-apply the Dex config (after editing agentlab.yaml)", lab.ApplyDex)),
		inGroup(groupAdvanced, selfUpdateCmd()),

		browserCmd(),
		slackFakeCmd(),
		mockLLMCmd(),
		githubFakeCmd(),
		skillHostCmd(),
	)
	return root
}

// slackFakeCmd is the fake Slack Web API klaus-gateway-test runs in a
// container on the kind network, where the component's pods reach it:
// plumbing, hidden, never typed by a person.
func slackFakeCmd() *cobra.Command {
	var listen string
	var emails []string
	cmd := &cobra.Command{
		Use:    "slack-fake",
		Short:  "Serve klaus-gateway-test's fake Slack Web API (run by the proof, in a container)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return lab.ServeFakeSlack(ctx, listen, emails)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:8080", "address to serve the fake Slack Web API on")
	cmd.Flags().StringArrayVar(&emails, "email", nil, "a person users.info answers for, as <slack user id>=<e-mail> (repeatable)")
	return cmd
}

// mockLLMCmd is the scripted model coding-test runs a codex agent on, in a
// container on the kind network: plumbing, hidden, never typed by a person.
func mockLLMCmd() *cobra.Command {
	var listen, mcpServer string
	cmd := &cobra.Command{
		Use:    "mockllm",
		Short:  "Serve coding-test's scripted OpenAI Responses API model, the mockllm (run by the proof, in a container)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return lab.ServeMockLLM(ctx, listen, mcpServer)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:8080", "address to serve the Responses API on (under /v1)")
	cmd.Flags().StringVar(&mcpServer, "mcp-server", "", "the RemoteMCPServer whose tools the script's MCP call names")
	return cmd
}

// githubFakeCmd is the fake GitHub REST API models-test runs in a container
// on the kind network, where model-manager's pods reach it: plumbing, hidden,
// never typed by a person.
func githubFakeCmd() *cobra.Command {
	var listen, repo, branch string
	var files []string
	var workspaces, platformManager bool
	var ws lab.WorkspaceGitHubOptions
	cmd := &cobra.Command{
		Use:    "github-fake",
		Short:  "Serve a fake GitHub: models-test's REST API, or with --workspaces the workspace proofs' GitHub (run by the proofs, in a container)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if platformManager {
				if workspaces || repo != "" || len(files) > 0 {
					return fmt.Errorf("--platform-manager serves the registry fixture's repositories: drop --workspaces, --repo and --file")
				}
				return lab.ServeRegistryGitHub(ctx, listen)
			}
			if workspaces {
				if repo != "" || len(files) > 0 {
					return fmt.Errorf("--workspaces serves its fixture's repositories: drop --repo and --file")
				}
				ws.Listen = listen
				return lab.ServeWorkspaceGitHub(ctx, ws)
			}
			return lab.ServeFakeGitHub(ctx, listen, repo, branch, files)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:8080", "address to serve the fake GitHub API on")
	cmd.Flags().StringVar(&repo, "repo", "", "the one repository the fake holds, owner/name")
	cmd.Flags().StringVar(&branch, "branch", "main", "its base branch")
	cmd.Flags().StringArrayVar(&files, "file", nil, "a file of the base branch, as <path>=<base64 content> (repeatable)")
	cmd.Flags().BoolVar(&workspaces, "workspaces", false, "serve the workspace proofs' GitHub over TLS: App, OAuth, listings, git and pull requests (docs/workspaces.md)")
	cmd.Flags().BoolVar(&platformManager, "platform-manager", false, "serve pm-test's registry fixture to the platform manager, read-only (docs/platform-manager.md)")
	cmd.Flags().StringVar(&ws.Fixture, "fixture", "", "with --workspaces: the fixture file (default: the embedded one)")
	cmd.Flags().StringVar(&ws.Credentials, "credentials", "", "with --workspaces: the directory `agentlab github-fake credentials` generated")
	cmd.Flags().StringVar(&ws.DataDir, "data-dir", "", "with --workspaces: where the bare repositories live (default: a temporary directory)")
	cmd.Flags().StringSliceVar(&ws.APIHosts, "api-host", nil, "with --workspaces: a host name api.<git host> that serves the REST API at its root, shaped like api.github.com (repeatable)")
	cmd.Flags().IntVar(&ws.LargeRepoMiB, "large-repo-mib", 0, "with --workspaces: the size of the fixture's generated repositories in MiB (default: the fixture's)")
	cmd.Flags().StringVar(&ws.TokenStore, "token-store", "", "with --workspaces: a file the users' access and refresh tokens are kept in across restarts (default: memory only)")
	for _, on := range []bool{true, false} {
		use, short := "suspend <owner>", "Suspend the App's installation on <owner> in the lab's GitHub, as an organization owner's Suspend on github.com does (the kill-switch runbook's first switch)"
		if !on {
			use, short = "unsuspend <owner>", "Unsuspend the App's installation on <owner> in the lab's GitHub"
		}
		cmd.AddCommand(&cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				return lab.SuspendLabGitHubInstallation(cfg, args[0], on)
			},
		})
	}
	cmd.AddCommand(labCmd("installations", "List the App's installations in the lab's GitHub, with when each suspended one was suspended", lab.LabGitHubInstallations))
	cmd.AddCommand(labCmd("credentials", "Generate the workspace fake's TLS pair, App key pair and OAuth client secret under certs/github-fake",
		func(cfg *config.Config) error {
			dir, err := lab.EnsureGitHubFakeCredentials(cfg.Platform.Domain)
			if err == nil {
				fmt.Println(dir)
			}
			return err
		}))
	return cmd
}

// skillHostCmd is the private git host `skills-test --skill-fixture` runs in
// a container on the kind network: plumbing, hidden, never typed by a
// person.
func skillHostCmd() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:    "skill-host",
		Short:  "Serve skills-test's private git host fixture (run by the proof, in a container)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return lab.ServeSkillHost(ctx, listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:8080", "address to serve the git host on")
	return cmd
}

// The help group IDs; their titles and order are in rootCmd.
const (
	groupSetup    = "setup"
	groupEveryday = "everyday"
	groupTesting  = "testing"
	groupCleanup  = "cleanup"
	groupAdvanced = "advanced"
)

// inGroup puts a command in one of the help groups, so the AddCommand list
// above is the one place that says where a command shows up. Every registered
// command needs a group (main_test.go asserts it).
func inGroup(id string, cmd *cobra.Command) *cobra.Command {
	cmd.GroupID = id
	return cmd
}

// renderCmd writes every manifest of the lab into state/ without applying
// anything. It renders the agentlab.yaml of the current directory into that
// directory (labs.Here): a copy of a registered lab's file renders a scratch
// lab beside it, never the registered lab's certs/ and state/, which only
// --lab or its own directory renders.
func renderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "render",
		Short: "Render every manifest from agentlab.yaml into state/ without applying anything",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadLab(labs.Here)
			if err != nil {
				return err
			}
			var taken *labs.TakenError
			if errors.As(labs.Check(cfg.ClusterName, "."), &taken) {
				fmt.Fprintf(os.Stderr, "Rendering this directory's %s; the lab %s registered in %s is left untouched (--lab %s renders that one)\n",
					config.File, taken.Name, taken.Other, taken.Name)
			}
			return lab.RenderAll(cfg)
		},
	}
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

// loadConfig enters the lab the command runs against (enterLab) and returns
// its saved configuration; if none exists yet it runs the interactive form on
// a terminal, and otherwise refuses with a pointer to `agentlab configure
// --defaults`.
func loadConfig() (*config.Config, error) {
	return loadLab(labs.Enter)
}

// loadProofLab is loadConfig for the proofs: before a proof's first step,
// the chart it runs against is named from the cluster, with the drift from
// agentlab.yaml's pin when there is one (lab.NoteChartUnderTest) — a verdict
// quotes the release under test, never a pin a restore left behind.
func loadProofLab() (*config.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if _, err := lab.NoteChartUnderTest(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadLab is loadConfig for every kind of command: mode is enterLab's.
func loadLab(mode labs.Mode) (*config.Config, error) {
	if err := enterLab(mode); err != nil {
		return nil, err
	}
	cfg, err := loadOrCreateConfig()
	if err != nil {
		return nil, err
	}
	// The lab's HTTP clients dial *.<domain> on loopback (the kind port
	// mappings) so checks never flake on external DNS, and trust the lab's
	// CA bundle; see lab.SetPlatform.
	lab.SetPlatform(cfg)
	return cfg, nil
}

// labFlag is --lab, the root's persistent flag: the registered lab a command
// runs against from any directory (enterLab).
var labFlag string

// enterLab resolves the lab the command runs against — --lab, the lab the
// agentlab.yaml in the current directory names, the person's pick on a
// terminal; never a default (labs.Resolve) — and changes into its
// directory, so every lab path (agentlab.yaml, certs/, state/) resolves there
// unchanged. mode is labs.Resolve's: labs.Create for the commands that make
// a lab where they run (`configure`, `up`), labs.Here for `render`, which
// writes the files of the agentlab.yaml here and no other lab's. A lab
// entered from elsewhere is named on stderr, so the output never leaves the
// person guessing which lab it came from.
func enterLab(mode labs.Mode) error {
	var pick labs.Picker
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pick = pickLab
	}
	l, err := labs.Resolve(labFlag, mode, pick)
	if err != nil {
		return err
	}
	if l.Dir == "" {
		registerHere()
		return nil
	}
	if err := os.Chdir(l.Dir); err != nil {
		return fmt.Errorf("entering lab %s: %w", l.Name, err)
	}
	fmt.Fprintf(os.Stderr, "Lab %s (%s)\n", l.Name, l.Dir)
	return nil
}

// registerHere registers the lab of the current directory, if there is one:
// a lab from before the registry, or one whose entry is gone, is found from
// any directory again after any command run in it. A convenience: silent,
// it never fails the command, and a name another lab holds stays refused
// (`up` and `configure` say so).
func registerHere() {
	cfg, err := config.Peek(".")
	if err != nil {
		return
	}
	_ = labs.Register(cfg.ClusterName, ".")
}

// pickLab asks which registered lab to use, on a terminal.
func pickLab(ls []labs.Lab) (labs.Lab, error) {
	options := make([]string, len(ls))
	for i, l := range ls {
		options[i] = fmt.Sprintf("%-16s %s", l.Name, l.Dir)
	}
	i, err := forms.Choose("Which lab?",
		fmt.Sprintf("There is no %s here; these labs are registered on this machine.\n--lab <name> skips this question.", config.File),
		options)
	if err != nil {
		return labs.Lab{}, err
	}
	return ls[i], nil
}

// saveLab saves a new or changed agentlab.yaml and registers the lab: its
// clusterName checked first, so a name another lab holds is refused before
// anything is written.
func saveLab(cfg *config.Config) error {
	if err := labs.Check(cfg.ClusterName, "."); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	return labs.Register(cfg.ClusterName, ".")
}

// freeClusterName gives a new lab the default name, or, when another lab on
// this machine holds it, the first free one after it (labs.FreeName) — named
// on stdout, so a second lab needs no --cluster-name.
func freeClusterName(cfg *config.Config) error {
	name, err := labs.FreeName(cfg.ClusterName, ".")
	if err != nil {
		return err
	}
	if name != cfg.ClusterName {
		fmt.Printf("  clusterName %s: another lab on this machine is %s (--cluster-name picks another)\n", name, cfg.ClusterName)
		cfg.ClusterName = name
	}
	return nil
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
	fmt.Printf("No %s yet — checking this machine first.\n\n", config.File)
	cfg = config.Default()
	if err := freeClusterName(cfg); err != nil {
		return nil, err
	}
	disc := discoverInto(cfg, true, nil, nil, nil, false)
	// The tools before the questions: a missing tool is refused here, not
	// after the form and a cluster boot.
	if err := disc.Preflight(); err != nil {
		return nil, err
	}
	useDefaults, err := forms.UseDefaults(false)
	if err != nil {
		return nil, err
	}
	if useDefaults {
		err = cfg.Validate()
	} else {
		err = forms.Run(cfg, accessibleMode(), forms.Hints{ModelServers: disc.ModelServersHint()})
	}
	if err != nil {
		return nil, err
	}
	if err := saveLab(cfg); err != nil {
		return nil, err
	}
	if useDefaults {
		printSaved(cfg, disc)
		fmt.Println("  (`agentlab configure` changes any of it)")
		fmt.Println()
		return cfg, nil
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
// Whether managed models are on follows the discovery on a first configure
// only (first: no agentlab.yaml yet); a later run keeps the recorded choice.
// A codex host model joins platform.extraModels only when asked for
// (codexHostModel, --codex-host-model): each is over a proof's memory budget.
// Reachability from pods (bind address, firewall) is checked at platform
// time, with the fixes.
func discoverInto(cfg *config.Config, first bool, pinEnabled *bool, pinBackends []string, pinVMManager *bool, codexHostModel bool) *lab.Discovery {
	disc := lab.Discover(cfg)
	fmt.Print(disc.Report(cfg))
	fmt.Println()
	applyPorts(cfg, disc)
	before := cfg.Platform.ModelManager
	cfg.Platform.ModelManager.ApplyDiscovered(disc.Backends(), cfg.Platform.Agents, first, pinEnabled, pinBackends)
	vmBefore := cfg.Platform.VMManager.Enabled
	cfg.Platform.VMManager.ApplyDiscovered(disc.KVMReady(), pinVMManager)
	reportModelManager(before, cfg.Platform.ModelManager, disc, cfg.Platform.Agents)
	reportVMManager(vmBefore, cfg.Platform.VMManager.Enabled, disc)
	if m, ok := lab.ApplyCodexHostModel(cfg, disc, codexHostModel); ok {
		fmt.Println("Applied to the configuration:")
		fmt.Printf("  platform.extraModels: + %s (%s on %s, Responses API): a model for the codex Harness (coding-test --harness codex --model-config %s runs on it)\n", m.Name, m.Model, m.BaseURL, m.Name)
		fmt.Println()
	} else if codexHostModel && cfg.CodexModelConfig() == "" {
		fmt.Printf("--codex-host-model: no codex host model added; it needs the codex Harness (--coding --coding-harnesses ...,codex) and one of %s with tools on a host Ollama pods reach.\n\n", lab.CodexHostModelIDs())
	}
	return disc
}

// reportVMManager says what the discovery did to platform.vmManager.
func reportVMManager(before, after bool, disc *lab.Discovery) {
	if before == after {
		return
	}
	reason := "--vm-manager"
	if !after {
		reason = "--vm-manager=false"
		if !disc.KVMReady() {
			reason = "this machine has no " + strings.Join(disc.KVMMissing, " and ") + ", so the VM provisioner cannot run as a pod of the node"
		}
	}
	fmt.Println("Applied to the configuration:")
	fmt.Printf("  platform.vmManager.enabled: %v -> %v (%s)\n", before, after, reason)
	fmt.Println()
}

// applyPorts moves the configuration's host ports off foreign listeners and
// off the ports every other registered lab records when no kind node of this
// configuration exists (fresh, or after `agentlab down`); with the cluster in place its port mappings are fixed at node
// creation, so a foreign listener on one of them is reported, not renumbered
// around — the lab's own published ports never count as occupied.
func applyPorts(cfg *config.Config, disc *lab.Discovery) {
	if !disc.ClusterExists {
		// Another registered lab's ports are taken even while it is down:
		// both labs could never be up together.
		recorded, err := labs.RecordedPorts(".")
		if err != nil {
			fmt.Printf("The other labs' ports are unknown (%v): only the ports in use count as taken.\n\n", err)
		}
		reportPortChanges(cfg.ChooseFreePorts(disc.ClusterPorts, recorded, lab.MinPublishablePort()))
		return
	}
	if !disc.ClusterPorts[cfg.APIServerPort] {
		fmt.Printf("The existing cluster's API server is not on apiServerPort %d: it keeps its port until it is recreated (`agentlab down`, then `agentlab up`).\n\n", cfg.APIServerPort)
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

// reportPortChanges tells the user which ports this machine cannot serve the
// lab on and what the configuration uses instead. The form (or the saved
// file) shows the adjusted numbers, but only this message explains why they
// differ from the documented defaults.
func reportPortChanges(changes []config.PortChange) {
	if len(changes) == 0 {
		return
	}
	fmt.Println("Some ports are not usable on this machine; picked ones that are:")
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
		fmt.Println("  (managed models stay off — `agentlab configure --defaults --model-manager` turns them on)")
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
// also what screen readers want. internal/forms is the one reader of the
// environment variable, since the questions it asks outside `configure` have
// no flag to OR with.
func accessibleMode() bool {
	return forms.Accessible()
}

// upCmd boots the lab. --trust and --open pre-answer the two questions the
// boot ends with on a terminal, for scripted runs.
func upCmd() *cobra.Command {
	var trust, open bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create the kind cluster, deploy Dex and the enabled components, and verify the OIDC chain",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadLab(labs.Create)
			if err != nil {
				return err
			}
			// A lab from before the registry registers on its next boot. A
			// convenience: it never fails the boot.
			if err := labs.Register(cfg.ClusterName, "."); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: not registering this lab: %v\n", err)
			}
			return lab.Up(cfg, offersFromFlags(cmd, &trust, &open))
		},
	}
	addOfferFlags(cmd, &trust, &open)
	return cmd
}

// addOfferFlags declares the pre-answers for the questions a boot ends with,
// and offersFromFlags reads them back: an unset flag means "ask on a terminal
// and stay silent off one", a set one is the answer wherever the command runs.
func addOfferFlags(cmd *cobra.Command, trust, open *bool) {
	cmd.Flags().BoolVar(trust, "trust", false, "install the lab CA into the trust stores without asking (--trust=false asks nothing and leaves it)")
	cmd.Flags().BoolVar(open, "open", false, "open the portal in the browser without asking (--open=false asks nothing)")
}

func offersFromFlags(cmd *cobra.Command, trust, open *bool) lab.Offers {
	var offers lab.Offers
	if cmd.Flags().Changed("trust") {
		offers.Trust = trust
	}
	if cmd.Flags().Changed("open") {
		offers.Open = open
	}
	return offers
}

// openCmd opens one lab URL in the browser. The target is required: a default
// would hide the fact that there is more than one thing to open.
func openCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "open <" + strings.Join(lab.OpenTargets(), "|") + ">",
		Short:     "Open a lab URL in the browser and print it: the portal, the kagent UI or the lab Prometheus",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: lab.OpenTargets(),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			// No argument is lab.Open's case too, so one place names the
			// targets in both messages.
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return lab.Open(cfg, target)
		},
	}
}

// outputJSON is the one `--output` format besides the default, for scripts.
const outputJSON = "json"

// listCmd shows the labs of this machine. It never asks which lab: it is
// the command that shows them.
func listCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the labs on this machine: cluster state, components, URLs, whether the lab CA is trusted",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != outputJSON {
				return fmt.Errorf("--output %q: the one format besides the default is json", output)
			}
			registered, err := labs.List()
			if err != nil {
				return err
			}
			here, err := os.Getwd()
			if err != nil {
				return err
			}
			return lab.PrintLabs(cmd.OutOrStdout(), lab.ListLabs(registered, here), output == outputJSON)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "json: the list as JSON, for scripts")
	return cmd
}

// statusCmd shows the lab's live state: the chart the cluster runs against
// the one agentlab.yaml installs, and the platform's HelmReleases.
func statusCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "The lab's live state: the chart in place (a dev chart named as such) against agentlab.yaml's, and the platform HelmReleases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" && output != outputJSON {
				return fmt.Errorf("--output %q: the one format besides the default is json", output)
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := lab.RequireLabCerts(cfg); err != nil {
				return err
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			status, err := lab.LabStatus(cfg, dir)
			if err != nil {
				return err
			}
			return lab.PrintStatus(cmd.OutOrStdout(), status, output == outputJSON)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "json: the status as JSON, for scripts")
	return cmd
}

// podsCmd lists the lab's pods without kubectl.
func podsCmd() *cobra.Command {
	var namespace string
	cmd := &cobra.Command{
		Use:   "pods",
		Short: "List the lab's pods across namespaces (kubectl get pods -A, without kubectl)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.Pods(cfg, namespace)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "only this namespace (default: every namespace)")
	return cmd
}

// turnCmd is `agentlab turn`: one conversation with an agent as a lab user
// through the edge, or the roster that user sees.
func turnCmd() *cobra.Command {
	var user, template, harness, session, decide, reason, events, shareWith string
	var shareTTL time.Duration
	var list, keep, suspend, del bool
	cmd := &cobra.Command{
		Use:   "turn (--list | --template <name> <prompt> | --session <id> --delete)",
		Short: "One turn with an agent as a lab user through the edge, the roster that user sees (--list), or the delete of a kept Session (--delete)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if user == "" {
				user = cfg.AdminUser().Email
			}
			if del {
				switch {
				case session == "":
					return fmt.Errorf("--delete needs --session")
				case list || template != "" || len(args) == 1 || keep || suspend || shareWith != "" || decide != "" || events != "":
					return fmt.Errorf("--delete takes only --session and --user")
				}
				return lab.DeleteSession(cfg, user, session)
			}
			switch {
			case list && (template != "" || len(args) == 1):
				return fmt.Errorf("--list takes no template and no prompt")
			case !list && (template == "" || len(args) != 1):
				return fmt.Errorf("give --list, --template <name> and one prompt, or --session <id> --delete")
			}
			prompt := ""
			if len(args) == 1 {
				prompt = args[0]
			}
			if suspend && session == "" {
				return fmt.Errorf("--suspend needs --session")
			}
			if shareWith != "" && session == "" {
				return fmt.Errorf("--share-with needs --session")
			}
			if shareTTL != 0 && shareWith == "" {
				return fmt.Errorf("--share-ttl needs --share-with")
			}
			if decide != "" && decide != "approve" && decide != "reject" {
				return fmt.Errorf("--decide takes approve or reject, not %q", decide)
			}
			return lab.Turn(cfg, user, template, harness, prompt, session, decide, reason, events, shareWith, shareTTL, keep, suspend)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "the lab user to act as (default: the first admin in agentlab.yaml)")
	cmd.Flags().StringVar(&template, "template", "", "the AgentTemplate in the kagent namespace to converse with (the Session is created of the Agent that pairs it with a Harness)")
	cmd.Flags().StringVar(&harness, "harness", "", "the Harness the Agent must reference: the conversation runs on the Agent pairing the template with it (default: the Agent of the template that reports Ready, as the portal picks it)")
	cmd.Flags().StringVar(&session, "session", "", "continue this Session instead of creating one (from a previous --keep)")
	cmd.Flags().StringVar(&session, "instance", "", "the former name of --session")
	_ = cmd.Flags().MarkDeprecated("instance", "use --session")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the Session in place after the turn, for a later --session turn")
	cmd.Flags().BoolVar(&del, "delete", false, "with --session: delete that Session (one kept by --keep) as the --user, its creator, instead of a turn")
	cmd.Flags().BoolVar(&suspend, "suspend", false, "with --session: suspend the session to the snapshot store before the turn, so the turn restores it")
	cmd.Flags().StringVar(&decide, "decide", "", "answer every tool approval the turn pauses for: approve or reject (default: print the paused state and stop)")
	cmd.Flags().StringVar(&shareWith, "share-with", "", "with --session: the --user shares the session read-write with this lab user, the turn runs as that user with the share token, and the share is revoked afterwards")
	cmd.Flags().DurationVar(&shareTTL, "share-ttl", 0, "with --share-with: how long the share's token grants access (default: until it is revoked)")
	cmd.Flags().StringVar(&events, "events", "", "write every streamed A2A event of the turn to this file, one JSON object per line")
	cmd.Flags().StringVar(&reason, "reason", "", "with --decide reject: the reason sent with the rejection")
	cmd.Flags().BoolVar(&list, "list", false, "print the roster this user sees instead of a turn")
	return cmd
}

// browserCmd is `agentlab login --browser` under the name it had before the
// two logins merged: hidden, so --help lists one way to log in, and kept
// because it is a name people type.
func browserCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "browser",
		Short:  "Deprecated: use `agentlab login --browser`",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.BrowserLogin(cfg)
		},
	}
}

// errHeadlessForm is what `configure` answers when its form has no terminal:
// the way without one, in one line.
var errHeadlessForm = errors.New("the configure form needs a terminal (stdin and stdout); run `agentlab configure --defaults` to skip it, adding the option flags (see --help) to change values, or --accessible for the prompt-per-question mode")

// formTerminal reports whether the configure form can run: stdin and stdout
// are both terminals. A variable so tests can stand in for the terminal.
var formTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func configureCmd() *cobra.Command {
	var defaults, accessible bool
	var platform, agents, observability, backstage, modelManager, vmManager, klausGateway, musterPodNetwork, alertmanager, installationsPage bool
	var serving, workspaces, workspacesCSICompanion, coding, tenants, github, githubSignIn bool
	var githubSignInClientID string
	var codingHarnesses []string
	var codingImages map[string]string
	var workspacesProvider, workspacesGrantProvider, workspacesGitHubAppID, workspacesGitHubClientID, workspacesCleanupAfter, workspacesRefreshAfter string
	var workspacesGitHubPrivateKeySource, workspacesGitHubClientSecretSource string
	var githubSignInOrgs, modelManagerBackends []string
	var vmManagerImageDir, aiKeySource, gitHubTokenSource, portal string
	var gitHubAppID, gitHubAppInstallationID, gitHubAppPrivateKeySource, gitHubAppClientID, gitHubAppClientSecretSource string
	var chartVersion, chartPath, chartBranch, clusterName, mcpPrometheusChartVersion string
	var upgradeSeed, adoptChart, released, codexHostModel bool
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Discover this machine, then ask for the lab configuration (or keep it with --defaults) and save agentlab.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The form needs a terminal; refused here, before anything is
			// discovered, instead of dying in the TUI.
			if !defaults && !accessible && !accessibleMode() && !formTerminal() {
				return errHeadlessForm
			}
			if err := enterLab(labs.Create); err != nil {
				return err
			}
			cfg, err := config.LoadForConfigure()
			fresh := errors.Is(err, os.ErrNotExist)
			if fresh {
				cfg = config.Default()
			} else if err != nil {
				return err
			}
			if cmd.Flags().Changed("cluster-name") {
				if err := config.ValidateClusterName(clusterName); err != nil {
					return fmt.Errorf("--cluster-name %q: %w", clusterName, err)
				}
				cfg.ClusterName = clusterName
			} else if fresh {
				if err := freeClusterName(cfg); err != nil {
					return err
				}
			}
			// The component flags first: what the discovery applies depends
			// on them (managed models need the agents runtime).
			if cmd.Flags().Changed("platform") {
				cfg.Platform.Enabled = platform
				// Backstage implies the platform (Normalize), so turning
				// the platform off turns Backstage off with it unless
				// --backstage says otherwise — `--platform=false` alone is
				// the bare kind+Dex sandbox the docs promise, not a
				// validation error about Backstage.
				if !platform && !cmd.Flags().Changed("backstage") {
					cfg.Backstage.Enabled = false
				}
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
			if cmd.Flags().Changed("portal") {
				if !slices.Contains(config.PortalModes, portal) {
					return fmt.Errorf("--portal %q: the portal modes are %s", portal, strings.Join(config.PortalModes, ", "))
				}
				cfg.Backstage.Portal = portal
			}
			if cmd.Flags().Changed("installations-page") {
				cfg.Backstage.InstallationsPage = installationsPage
			}
			if cmd.Flags().Changed("chart-version") {
				cfg.Platform.ChartVersion = chartVersion
			}
			if cmd.Flags().Changed("chart-path") {
				cfg.Platform.ChartPath = chartPath
			}
			if cmd.Flags().Changed("upgrade-seed") {
				cfg.Platform.UpgradeSeed = upgradeSeed
			}
			if cmd.Flags().Changed("chart-branch") {
				// A new branch (or none) starts unpinned: the pin froze a
				// build of the previous one.
				cfg.Platform.ChartBranch = chartBranch
				cfg.Platform.ChartPinned = false
				// Back to the stable channel: the build the branch left in
				// chartVersion goes with it, unless this call pins a version.
				if chartBranch == "" && !cmd.Flags().Changed("chart-version") {
					if old := lab.ResetDevBuildPin(cfg); old != "" {
						fmt.Printf("  chartVersion %s was the branch's dev build; on the stable channel the lab pins %s (--chart-version picks another release)\n", old, cfg.Platform.ChartVersion)
					}
				}
			}
			// The restore step before a lab is handed on: the proof state
			// goes, so the next holder starts on the released platform
			// (overlays.go).
			if released {
				for _, flag := range []string{"chart-path", "chart-branch", "adopt-chart", "vm-manager-image-dir"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--released drops the proof state; --%s sets some — one or the other", flag)
					}
				}
				dropped := lab.DropProofState(cfg)
				if len(dropped) == 0 {
					fmt.Println("  no proof state to drop: the lab is on the released platform")
				}
				for _, d := range dropped {
					fmt.Printf("  dropped %s\n", d)
				}
			}
			// The restore step for a config put back apart from its
			// cluster: the chart in place becomes the pin (chartdrift.go).
			if adoptChart {
				if cmd.Flags().Changed("chart-version") || cmd.Flags().Changed("chart-path") || cmd.Flags().Changed("chart-branch") {
					return fmt.Errorf("--adopt-chart takes the chart from the cluster; --chart-version, --chart-path and --chart-branch choose one by hand — one or the other")
				}
				installed, err := lab.AdoptInstalledChart(cfg)
				if err != nil {
					return err
				}
				fmt.Printf("  chartVersion %s adopted from the cluster: %s, Helm revision %d\n", cfg.Platform.ChartVersion, installed, installed.Revision)
			}
			cfg.Normalize()
			var pinEnabled *bool
			if cmd.Flags().Changed("model-manager") {
				pinEnabled = &modelManager
			}
			var pinBackends []string
			if cmd.Flags().Changed("model-manager-backends") {
				pinBackends = modelManagerBackends
			}
			var pinVMManager *bool
			if cmd.Flags().Changed("vm-manager") {
				pinVMManager = &vmManager
			}
			if cmd.Flags().Changed("vm-manager-image-dir") {
				cfg.Platform.VMManager.ImageDir = vmManagerImageDir
			}
			if cmd.Flags().Changed("klaus-gateway") {
				cfg.Platform.KlausGateway.Enabled = klausGateway
			}
			if cmd.Flags().Changed("muster-pod-network") {
				cfg.Platform.MusterPodNetwork = musterPodNetwork
			}
			if cmd.Flags().Changed("alertmanager") {
				cfg.Platform.Alertmanager = alertmanager
			}
			if cmd.Flags().Changed("mcp-prometheus-chart-version") {
				cfg.Platform.MCPPrometheusChartVersion = mcpPrometheusChartVersion
			}
			if cmd.Flags().Changed("github") {
				cfg.Platform.GitHub.Enabled = github
			}
			// The client id turns the sign-in on by itself: it is the one
			// value the lab needs from the App, the secret being placed on
			// the cluster.
			if cmd.Flags().Changed("github-signin-client-id") {
				cfg.Platform.GitHubSignIn.ClientID = githubSignInClientID
				cfg.Platform.GitHubSignIn.Enabled = githubSignInClientID != ""
			}
			if cmd.Flags().Changed("github-signin") {
				cfg.Platform.GitHubSignIn.Enabled = githubSignIn
			}
			if cmd.Flags().Changed("github-signin-orgs") {
				cfg.Platform.GitHubSignIn.Orgs = githubSignInOrgs
			}
			if cmd.Flags().Changed("serving") {
				cfg.Platform.Serving.Enabled = serving
			}
			if cmd.Flags().Changed("workspaces") {
				cfg.Platform.Workspaces.Enabled = workspaces
			}
			if cmd.Flags().Changed("coding") {
				cfg.Platform.Coding.Enabled = coding
			}
			if cmd.Flags().Changed("coding-harnesses") {
				cfg.Platform.Coding.Harnesses = codingHarnesses
			}
			if cmd.Flags().Changed("tenants") {
				cfg.Platform.Tenants.Enabled = tenants
			}
			if cmd.Flags().Changed("tenants") && tenants {
				// tenants-test's tenant users, one per organization, in a
				// group the lab binds nothing to (docs/identity.md); added
				// when --tenants is given, so a user removed by hand stays
				// removed.
				if added := cfg.EnsureTenantUsers(); len(added) > 0 {
					fmt.Printf("  users      added %s for tenants-test (`agentlab reload` renders them into Dex)\n", strings.Join(added, ", "))
				}
			}
			if cmd.Flags().Changed("coding-images") {
				cfg.Platform.Coding.Images = codingImages
			}
			if cmd.Flags().Changed("workspaces-provider") {
				cfg.Platform.Workspaces.Provider = workspacesProvider
			}
			if cmd.Flags().Changed("workspaces-grant-provider") {
				cfg.Platform.Workspaces.GrantProvider = workspacesGrantProvider
			}
			if cmd.Flags().Changed("workspaces-cleanup-after") {
				cfg.Platform.Workspaces.SessionsCleanupAfter = workspacesCleanupAfter
			}
			if cmd.Flags().Changed("workspaces-refresh-after") {
				cfg.Platform.Workspaces.SyncRefreshAfter = workspacesRefreshAfter
			}
			if cmd.Flags().Changed("workspaces-csi-companion") {
				cfg.Platform.Workspaces.CSICompanion = workspacesCSICompanion
			}
			if cmd.Flags().Changed("workspaces-github-app-id") {
				cfg.Platform.Workspaces.GitHub.AppID = workspacesGitHubAppID
			}
			if cmd.Flags().Changed("workspaces-github-client-id") {
				cfg.Platform.Workspaces.GitHub.ClientID = workspacesGitHubClientID
			}
			if cmd.Flags().Changed("workspaces-github-private-key-source") {
				cfg.Platform.Workspaces.GitHub.PrivateKeySource = workspacesGitHubPrivateKeySource
			}
			if cmd.Flags().Changed("workspaces-github-client-secret-source") {
				cfg.Platform.Workspaces.GitHub.ClientSecretSource = workspacesGitHubClientSecretSource
			}
			if cmd.Flags().Changed("ai-key-source") {
				cfg.AIKey.Source = aiKeySource
			}
			if cmd.Flags().Changed("github-token-source") {
				cfg.GitHubToken.Source = gitHubTokenSource
			}
			if cmd.Flags().Changed("github-app-id") {
				cfg.GitHubApp.AppID = gitHubAppID
			}
			if cmd.Flags().Changed("github-app-installation-id") {
				cfg.GitHubApp.InstallationID = gitHubAppInstallationID
			}
			if cmd.Flags().Changed("github-app-private-key-source") {
				cfg.GitHubApp.PrivateKeySource = gitHubAppPrivateKeySource
			}
			if cmd.Flags().Changed("github-app-client-id") {
				cfg.GitHubApp.ClientID = gitHubAppClientID
			}
			if cmd.Flags().Changed("github-app-client-secret-source") {
				cfg.GitHubApp.ClientSecretSource = gitHubAppClientSecretSource
			}
			// Every run discovers the machine — an existing agentlab.yaml
			// follows the host too: a server that appeared is added, one that
			// is gone drops out, ports move while no cluster holds them.
			disc := discoverInto(cfg, fresh, pinEnabled, pinBackends, pinVMManager, codexHostModel)
			// The tools before the questions (or, with --defaults, before
			// the file): what `agentlab up` would refuse is refused here,
			// with the install hints, instead of after the whole form.
			if err := disc.Preflight(); err != nil {
				return err
			}
			// A first configure on a terminal asks the one question first;
			// only Customize opens the form. An existing agentlab.yaml opens
			// the form with its values, as --defaults keeps them.
			if !defaults && fresh && term.IsTerminal(int(os.Stdin.Fd())) {
				if defaults, err = forms.UseDefaults(accessible); err != nil {
					return err
				}
			}
			if defaults {
				if err := cfg.Validate(); err != nil {
					return err
				}
			} else {
				if err := forms.Run(cfg, accessible || accessibleMode(), forms.Hints{ModelServers: disc.ModelServersHint()}); err != nil {
					return err
				}
			}
			// The dev channel: the branch's newest build becomes the pinned
			// chartVersion now, so the file says what `up` will install and
			// a branch without builds is refused here, not after a boot.
			if _, err := lab.ResolveChartVersion(cfg); err != nil {
				return err
			}
			if err := saveLab(cfg); err != nil {
				return err
			}
			printSaved(cfg, disc)
			fmt.Println("\nNext: agentlab up")
			return nil
		},
	}
	cmd.Flags().BoolVar(&defaults, "defaults", false, "skip the form; keep current values (or the canonical defaults) plus what the discovery finds")
	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "the lab's name (clusterName): its kind cluster, its registration and --lab <name>; default "+config.DefaultClusterName+", or the first free "+config.DefaultClusterName+"-<n> when another lab on this machine holds it")
	cmd.Flags().BoolVar(&platform, "platform", false, "enable/disable the agent platform")
	cmd.Flags().BoolVar(&agents, "agents", false, "enable/disable the agents runtime (kagent, part of the platform install)")
	cmd.Flags().BoolVar(&observability, "observability", false, "enable/disable the observability stack (Prometheus + mcp-prometheus)")
	cmd.Flags().BoolVar(&backstage, "backstage", false, "enable/disable Backstage (implies the platform)")
	cmd.Flags().BoolVar(&installationsPage, "installations-page", false, "enable the portal's Installations page (page:gs/installations and its nav item, off in the agent-platform chart) for the portal's installations e2e specs, run with AGENTLAB_INSTALLATIONS_PAGE=1; the next agentlab platform rolls Backstage onto it; --installations-page=false turns it off again")
	cmd.Flags().StringVar(&portal, "portal", "", fmt.Sprintf("the Backstage portal `mode` (%s): %s serves the Agent Platform shell (app.portals.agentPlatform), %s the classic Platform Portal alone; the next agentlab platform rolls Backstage onto it", strings.Join(config.PortalModes, ", "), config.PortalAgentPlatform, config.PortalClassic))
	cmd.Flags().BoolVar(&modelManager, "model-manager", false, "pin managed models on/off (needs agents); without it a first configure follows the host model servers the discovery finds and a later one keeps the recorded choice")
	cmd.Flags().StringVar(&chartVersion, "chart-version", "", "the agent-platform chart release to install (an exact version; default "+config.DefaultChartVersion+")")
	cmd.Flags().StringVar(&chartPath, "chart-path", "", "install the agent-platform chart from this local directory (an agent-platform checkout's helm/agent-platform) instead of the pinned release; \"\" clears it")
	cmd.Flags().BoolVar(&upgradeSeed, "upgrade-seed", false, "seed an upgrade proof: install a released agent-platform chart below agentlab's floor in the shape of its line, to upgrade it in place later with --chart-version and `agentlab platform`; --upgrade-seed=false clears it")
	cmd.Flags().BoolVar(&released, "released", false, "the restore step before handing the lab on: drop the proof state — every platform.valuesFiles overlay, platform.devImages, a chart directory, the dev channel (a dev build in chartVersion goes back to the default release) and a local guest image — so the next `agentlab platform` installs the released platform; `agentlab status` names what is left; not with --chart-path, --chart-branch, --adopt-chart or --vm-manager-image-dir")
	cmd.Flags().BoolVar(&adoptChart, "adopt-chart", false, "write the agent-platform chart the running lab cluster carries into chartVersion, so agentlab.yaml says what the cluster runs — the restore step for a config put back from a copy apart from its cluster (`agentlab status` names the drift); a release puts the lab on the stable channel at that release, a dev build of the followed branch pins it; refused for a chart directory's build, whose version is a placeholder; not with --chart-version, --chart-path or --chart-branch")
	cmd.Flags().StringVar(&chartBranch, "chart-branch", "", "the dev channel: follow this agent-platform branch's newest dev build (resolved now and on every up/platform, written to chartVersion); \"\" returns to the stable channel and, unless --chart-version pins one, the default release takes the branch's build's place in chartVersion")
	cmd.Flags().StringSliceVar(&modelManagerBackends, "model-manager-backends", nil, fmt.Sprintf("pin the host model servers, in order (%s; the first is model-manager's default backend) instead of the ones the discovery finds", strings.Join(config.ModelManagerBackends, ", ")))
	cmd.Flags().BoolVar(&vmManager, "vm-manager", false, "run the platform's VM provisioner (vm-manager) as a pod of the node; --vm-manager=false turns it off (needs /dev/kvm and /dev/vhost-vsock on this machine)")
	cmd.Flags().StringVar(&vmManagerImageDir, "vm-manager-image-dir", "", "a local guest image build the vm-manager pod boots instead of its release's: a vm-manager checkout's images/build after `make -C images`, pushed into the lab registry at `agentlab platform` (empty for the release's)")
	cmd.Flags().BoolVar(&klausGateway, "klaus-gateway", false, "run Swarmgeist (klaus-gateway) as the meta chart's in-cluster component: A2A on the in-cluster controller target, Slack on a placeholder Secret (its Web API the proof's fake), the OBO link store in a Secret (needs agents); --klaus-gateway=false turns it off")
	cmd.Flags().BoolVar(&musterPodNetwork, "muster-pod-network", false, "run muster as an installation does: on the pod network at two replicas behind the edge with a PodDisruptionBudget of minAvailable 1, the issuer reached through the dex-localhost sidecar (muster's direct debug port answers nothing then); --muster-pod-network=false returns it to the host network at one replica")
	cmd.Flags().BoolVar(&alertmanager, "alertmanager", false, "run an Alertmanager in the observability stack with an always-firing AgentlabWatchdog alert, and point mcp-prometheus at it (ALERTMANAGER_URL) so its alert tools have alerts to read; needs platform.observability")
	cmd.Flags().StringVar(&mcpPrometheusChartVersion, "mcp-prometheus-chart-version", "", "install this mcp-prometheus chart release (an exact version, a release candidate included) instead of the one agentlab pins, for proving a chart change the devImages swap cannot carry; \"\" returns to the pin; needs platform.observability")
	cmd.Flags().BoolVar(&github, "github", false, "register GitHub's hosted MCP server with muster as MCPServer github, signed in to as the person through an OAuth App or GitHub App client whose client-id and client-secret are the Secret platform.github.secret names (default agent-platform/github-oauth-client), placed with `beekeeper secret copy --to-secret`, never read by agentlab; --github=false removes the server")
	cmd.Flags().StringVar(&githubSignInClientID, "github-signin-client-id", "", "GitHub sign-in through the lab Dex: the GitHub App's client id (turns the sign-in on; \"\" turns it off); the App's callback URL is the lab Dex's own, <issuer>/callback, and its client secret is the Secret platform.githubSignIn.secret names in the dex namespace (default github-signin-client, key client-secret), placed with `beekeeper secret copy --to-secret`, never read by agentlab")
	cmd.Flags().BoolVar(&githubSignIn, "github-signin", false, "turn the GitHub sign-in on (needs --github-signin-client-id once) or, with =false, off; the client Secret stays")
	cmd.Flags().StringSliceVar(&githubSignInOrgs, "github-signin-orgs", nil, "GitHub sign-in: admit members of these GitHub organizations only, their teams as groups (`<org>:<team-slug>`); empty admits any GitHub account")
	cmd.Flags().BoolVar(&workspaces, "workspaces", false, "workspace storage: a read-write-many StorageClass that serves git — an in-cluster NFS server, the NFS CSI driver behind an mTLS proxy only Agent Substrate's API server may reach, and the StorageClass "+lab.WorkspacesStorageClass+"; with --agents=false the storage and the workspace-manager run without Agent Substrate, the shape a later --agents switches on in place; --workspaces=false turns it off")
	cmd.Flags().BoolVar(&coding, "coding", false, "coding agents: the chart's kagent.harnesses renders the coding Harnesses (claude, Claude Code run headless, unless --coding-harnesses names others), which `agentlab coding-test` proves (needs agents); --coding=false turns it off")
	cmd.Flags().BoolVar(&tenants, "tenants", false, "the Organizations' namespaces as tenants: the chart's tenants.organizations.enabled turns on agent-manager's tenant reconciler for every namespace labelled giantswarm.io/organization, which `agentlab tenants-test` proves (needs agents); --tenants=false turns it off")
	cmd.Flags().StringSliceVar(&codingHarnesses, "coding-harnesses", nil, fmt.Sprintf("the coding Harnesses rendered with --coding, by runtime (%s; each named after its runtime; default claude alone); codex needs a kagent line whose chart takes runtime codex, omp one whose chart takes runtime byo and an image from --coding-images; \"\" returns to claude alone", strings.Join(config.CodingRuntimes, ", ")))
	cmd.Flags().BoolVar(&codexHostModel, "codex-host-model", false, fmt.Sprintf("add the extraModels entry %s for the codex Harness: the first of %s the host's Ollama has, on its Responses API (coding-test --harness codex --model-config %s); each needs more memory than a lab proof's host model gets, so configure adds none without this flag", lab.CodexHostModelName, lab.CodexHostModelIDs(), lab.CodexHostModelName))
	cmd.Flags().StringToStringVar(&codingImages, "coding-images", nil, "the image of a coding Harness the chart has none of its own for, <runtime>=<image@sha256:digest> (repeatable): "+config.CodingRuntimeOmp+"=<the kagent line's omp-harness, runtimeImages.ompHarness.digest of the release's chart>; a dev image (platform.devImages "+config.DevImageOmpHarness+") wins; an empty map clears it")
	cmd.Flags().StringVar(&workspacesProvider, "workspaces-provider", "", "the workspace-manager's provider instances: fake (the lab's own GitHub, the default), fake-bearer (the same GitHub shaped like github.com, its API on a Bearer host of its own; beside fake), github (a GitHub App of the lab's own, docs/workspaces.md), gitlab (a GitLab's OAuth application, docs/workspaces.md) or several, such as fake,github,gitlab")
	cmd.Flags().StringVar(&workspacesGrantProvider, "workspaces-grant-provider", "", "the provider instance whose App muster pins the workspace-manager to, the one App a person grants (platform.workspaces.grantProvider): one of --workspaces-provider, such as fake-bearer for 'workspaces-test --part caller-credential' and '--part write'; \"\" returns to the first instance")
	cmd.Flags().BoolVar(&workspacesCSICompanion, "workspaces-csi-companion", false, "with workspaces on, the connectivity chart's CSI companion is Substrate's workspace driver: its efs preset running the lab's NFS plugin, the lab's NFS driver serving the volumes under the preset's driver name — the lab path for proving a companion chart change (docs/workspaces.md); --workspaces-csi-companion=false returns to the lab's own registration")
	cmd.Flags().StringVar(&workspacesCleanupAfter, "workspaces-cleanup-after", "", "how long a Session's directory on a workspace volume outlives its last turn, a Go duration (the workspace-manager's sessions.cleanupAfter); 'workspaces-test --part sessions' needs a window of "+lab.SessionsCleanupWindowRange()+" (such as "+lab.SessionsCleanupWindowExample()+"); \"\" returns to the chart's 30 days")
	cmd.Flags().StringVar(&workspacesRefreshAfter, "workspaces-refresh-after", "", "the refresh threshold, a Go duration (the workspace-manager's sync.refreshAfter): a Session started on a workspace whose last sync is older requests a sync; 'workspaces-test --part refresh' needs "+lab.RefreshThresholdRange()+" (such as "+lab.RefreshThresholdExample()+"); \"\" returns to the manager's 1h")
	cmd.Flags().StringVar(&workspacesGitHubAppID, "workspaces-github-app-id", "", "the App id of the workspaces GitHub App (public; its private key and client secret go into the Secret agent-platform/"+config.WorkspaceGitHubSecretName+")")
	cmd.Flags().StringVar(&workspacesGitHubClientID, "workspaces-github-client-id", "", "the client id of the workspaces GitHub App (public)")
	cmd.Flags().StringVar(&workspacesGitHubPrivateKeySource, "workspaces-github-private-key-source", "", "where the workspaces GitHub App's private key is: a reference `beekeeper secret copy` resolves (op://<vault>/<item>/<field>, or <file>#<path> of a SOPS file), never the key; every up and platform place it into the Secret agent-platform/"+config.WorkspaceGitHubSecretName)
	cmd.Flags().StringVar(&workspacesGitHubClientSecretSource, "workspaces-github-client-secret-source", "", "where the workspaces GitHub App's client secret is, in the form --workspaces-github-private-key-source takes; placed the same way")
	cmd.Flags().BoolVar(&serving, "serving", false, "serve models on llm-d in the lab: the KServe llmisvc controller and its CRDs, the well-known runtime configs, the connectivity chart's serving slice with the models Gateway, model-manager's kserve backend and one CPU preset of the lab's (needs agents; installs cert-manager); --serving=false turns it off")
	cmd.Flags().StringVar(&aiKeySource, "ai-key-source", "", "where the Anthropic key of the agents' default ModelConfig lives, a reference `beekeeper secret copy` resolves (op://<vault>/<item>/<field>, or <file>#<path> of a SOPS file), never a value: every up and platform place it into the Secret kagent/kagent-anthropic through beekeeper, so a recreated lab carries it without a manual step; \"\" clears it (the key then comes from $ANTHROPIC_API_KEY, else a placeholder)")
	cmd.Flags().StringVar(&gitHubTokenSource, "github-token-source", "", "where the GitHub token of the portal's skill discovery and agent-manager's skill resolution lives, a reference `beekeeper secret copy` resolves (op://<vault>/<item>/<field>, or <file>#<path> of a SOPS file), never a value: every up and platform place it into the Secret agentlab-github-token through beekeeper, lifting GitHub's anonymous 60 requests an hour this machine shares; \"\" clears it (GitHub is then called as the skills GitHub App when one is recorded, else unauthenticated); agentlab never reads $GITHUB_TOKEN")
	cmd.Flags().StringVar(&gitHubAppID, "github-app-id", "", "the skills GitHub App agent-manager resolves skills with (installation tokens at the App's rate limit instead of a token or GitHub's anonymous window): its numeric App id (public); set with --github-app-installation-id and --github-app-private-key-source, \"\" on all three clears it")
	cmd.Flags().StringVar(&gitHubAppInstallationID, "github-app-installation-id", "", "the skills GitHub App: the numeric id of its installation (public); alone, the App is the workspaces GitHub App (--workspaces-github-app-id and --workspaces-github-private-key-source)")
	cmd.Flags().StringVar(&gitHubAppPrivateKeySource, "github-app-private-key-source", "", "the skills GitHub App: where its private key lives, a reference `beekeeper secret copy` resolves (op://<vault>/<item>/<field>, or <file>#<path> of a SOPS file), never a value: every up and platform place it into the Secret agentlab-github-app through beekeeper")
	cmd.Flags().StringVar(&gitHubAppClientID, "github-app-client-id", "", "the skills GitHub App: its OAuth client id (public); with --github-app-client-secret-source the portal's skill discovery calls GitHub as the App too (the workspaces GitHub App's client comes with --github-app-installation-id alone)")
	cmd.Flags().StringVar(&gitHubAppClientSecretSource, "github-app-client-secret-source", "", "the skills GitHub App: where its client secret lives, in the form --github-app-private-key-source takes: every up and platform place it into the Secret agentlab-portal-github-app through beekeeper")
	cmd.Flags().BoolVar(&accessible, "accessible", false, "prompt-per-question form mode (for screen readers and plain terminals)")
	return cmd
}

// printSaved is the summary of a saved agentlab.yaml: what `configure` and a
// first run that took the defaults wrote.
func printSaved(cfg *config.Config, disc *lab.Discovery) {
	fmt.Printf("Saved %s:\n", config.File)
	fmt.Printf("  cluster    %s (Dex on %s)\n", cfg.ClusterName, cfg.Issuer())
	fmt.Printf("  users      %d\n", len(cfg.Users))
	fmt.Printf("  platform   %v (agents %v, observability %v)\n", cfg.Platform.Enabled, cfg.Platform.Agents, cfg.Platform.Observability)
	switch {
	case cfg.Platform.ChartPath != "":
		fmt.Printf("  chart      local checkout %s (chartVersion %s ignored while set); its connectivity chart from %s, pushed into the lab registry\n",
			cfg.Platform.ChartPath, cfg.Platform.ChartVersion, config.ConnectivityChartDir(cfg.Platform.ChartPath))
	case cfg.Platform.ChartBranch != "":
		fmt.Printf("  chart      agent-platform %s (branch %s, dev channel%s)\n", cfg.Platform.ChartVersion, cfg.Platform.ChartBranch, pinnedNote(cfg))
	case cfg.Platform.UpgradeSeed:
		fmt.Printf("  chart      agent-platform %s (upgrade seed: a release below agentlab's floor installs)\n", cfg.Platform.ChartVersion)
	default:
		fmt.Printf("  chart      agent-platform %s\n", cfg.Platform.ChartVersion)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Platform.DevImages)) {
		fmt.Printf("  dev image  %s -> %s\n", name, cfg.Platform.DevImages[name])
	}
	if cfg.Backstage.Enabled {
		page := ""
		if cfg.Backstage.InstallationsPage {
			page = ", installations page"
		}
		fmt.Printf("  backstage  true (portal %s%s)\n", cfg.Backstage.Portal, page)
	} else {
		fmt.Println("  backstage  false")
	}
	switch {
	case cfg.AIKey.Source != "":
		fmt.Printf("  ai model   %s (key placed from %s through beekeeper at deploy time)\n", cfg.AIModel, cfg.AIKey.Source)
	case disc.AnthropicKey:
		fmt.Printf("  ai model   %s (key from $%s at deploy time)\n", cfg.AIModel, lab.AnthropicKeyEnv)
	default:
		fmt.Printf("  ai model   %s (no key source and $%s not set: a placeholder key at deploy time, agent turns fail until one is placed)\n", cfg.AIModel, lab.AnthropicKeyEnv)
	}
	for _, m := range cfg.Platform.ExtraModels {
		fmt.Printf("  extra model %s (%s %s)\n", m.Name, m.Provider, m.Model)
	}
	if backends := cfg.ChartBackends(); cfg.ModelManagerEnabled() && len(backends) > 0 {
		mm := cfg.Platform.ModelManager
		fmt.Printf("  models     one model-manager fronts %d backend(s) (default %s):\n", len(backends), backends[0])
		for _, b := range backends {
			if b == config.ModelManagerBackendKServe {
				fmt.Printf("             the platform's own serving on llm-d (%s backend): the lab preset %s on the CPU runtime\n", b, lab.ServingPresetName)
				continue
			}
			fmt.Printf("             %s (%s backend, %s)\n", config.BackendServerName(b), b, endpointNote(mm, b, disc))
		}
	}
	if cfg.ServingEnabled() {
		fmt.Printf("  serving    llm-d on the node: the llmisvc controller and its CRDs, the well-known runtime configs, the models Gateway at %s, cert-manager\n", lab.ModelsGatewayHost(cfg))
	}
	if cfg.WorkspacesEnabled() {
		fmt.Printf("  workspaces an NFS server on %s and the NFS CSI driver behind its mTLS proxy; the read-write-many StorageClass %s\n", lab.WorkspacesNode(cfg), lab.WorkspacesStorageClass)
	}
	if cfg.WorkspaceFakeProvider() {
		fmt.Printf("  ws-fake    the workspace-manager's provider instance fake: the lab's own GitHub at %s (its App, key pair and client secret the lab's; Secret %s/%s), the App's callback URL %s\n",
			cfg.GitHubFakeURL(), config.DefaultGitHubSecretNamespace, config.WorkspaceFakeSecretName, lab.WorkspaceAppCallback(cfg))
	}
	if cfg.WorkspaceGitHubProvider() {
		gh := cfg.Platform.Workspaces.GitHub
		fmt.Printf("  ws-github  the workspace-manager's provider instance github: App id %s, client id %s, the App's callback URL %s; private key and client secret from Secret %s/%s (keys %s from %s, %s from %s)\n",
			orUnset(gh.AppID), orUnset(gh.ClientID), lab.WorkspaceAppCallback(cfg),
			config.DefaultGitHubSecretNamespace, config.WorkspaceGitHubSecretName,
			config.WorkspaceGitHubPrivateKeyKey, orByHand(gh.PrivateKeySource), config.GitHubClientSecretKey, orByHand(gh.ClientSecretSource))
	}
	if grant := cfg.WorkspaceGrantProvider(); grant != "" {
		fmt.Printf("  ws-grant   muster pins a workspace-manager of 0.10.0 or newer to the App of %s: each person grants it once (core_auth_login)\n", grant)
	}
	if cfg.VMManagerEnabled() {
		fmt.Printf("  vm-manager the platform's VM provisioner as a pod of the node, registered with muster as x_vm-manager_* (%s)\n",
			vmManagerImagesNote(cfg.Platform.VMManager))
	}
	if cfg.KlausGatewayEnabled() {
		fmt.Println("  klaus-gtw  Swarmgeist as the meta chart's component: A2A on the in-cluster controller, Slack on a placeholder Secret (its Web API the proof's fake), the OBO link store in a Secret")
	}
	if cfg.GitHubSignInEnabled() {
		s := cfg.Platform.GitHubSignIn
		fmt.Printf("  github     sign-in through the lab Dex as client %s (callback URL %s; the client secret from Secret %s/%s, key %s)%s\n",
			s.ClientID, cfg.GitHubSignInCallbackURL(), config.GitHubSignInSecretNamespace, s.ClientSecret().Name, config.GitHubClientSecretKey, gitHubSignInOrgsNote(s.Orgs))
	}
}

// orByHand is a recorded secret source as the summary prints it.
func orByHand(source string) string {
	if source == "" {
		return "a placement by hand"
	}
	return source
}

// orUnset is a configured value as the summary prints it.
func orUnset(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}

// gitHubSignInOrgsNote says who the GitHub sign-in admits.
func gitHubSignInOrgsNote(orgs []string) string {
	if len(orgs) == 0 {
		return ", any GitHub account"
	}
	return ", members of " + strings.Join(orgs, ", ")
}

// pinnedNote marks a pinned dev-channel lab in the configure summary.
func pinnedNote(cfg *config.Config) string {
	if cfg.Platform.ChartPinned {
		return ", pinned"
	}
	return ""
}

// platformCmd installs the platform on a running cluster; --pin freezes (or
// --pin=false releases) the dev channel's recorded build first.
func platformCmd() *cobra.Command {
	var pin bool
	var trust, open bool
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "Install the Giant Swarm agent platform (the agent-platform chart in the lab shape)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := lab.RequireLabCerts(cfg); err != nil {
				return err
			}
			if cmd.Flags().Changed("pin") {
				if cfg.Platform.ChartBranch == "" {
					return fmt.Errorf("--pin only applies to the dev channel (platform.chartBranch is not set; the stable channel is always pinned)")
				}
				cfg.Platform.ChartPinned = pin
				if err := cfg.Save(); err != nil {
					return err
				}
			}
			// The lab Dex first, so agentlab.yaml's identity (the users, the
			// GitHub sign-in) applies with the platform in one run; an
			// unchanged Dex is a no-op.
			if err := lab.ApplyDex(cfg); err != nil {
				return err
			}
			return lab.PlatformUp(cfg, offersFromFlags(cmd, &trust, &open))
		},
	}
	addOfferFlags(cmd, &trust, &open)
	cmd.Flags().BoolVar(&pin, "pin", false, "dev channel: freeze platform.chartVersion at the recorded build instead of following platform.chartBranch (--pin=false follows it again)")
	return cmd
}

// endpointNote says where a backend is dialed: the configured override or
// the autodetection.
func endpointNote(mm config.ModelManager, backend string, disc *lab.Discovery) string {
	if ep := mm.EndpointFor(backend); ep != "" {
		return ep
	}
	// The discovery already established which address pods reach this server
	// on, so name it rather than the gateway it may not be.
	if host := disc.PodHostFor(backend); host != "" {
		return fmt.Sprintf("autodetected as http://%s:%d", host, config.BackendPort(backend))
	}
	return fmt.Sprintf("autodetected at platform time on port %d", config.BackendPort(backend))
}

// vmManagerImagesNote says what the vm-manager pod boots from.
func vmManagerImagesNote(vmm config.VMManager) string {
	if vmm.ImageDir == "" {
		return "the guest image its release published (--vm-manager-image-dir <a vm-manager checkout's images/build> boots a local build)"
	}
	return "the local guest image build of " + vmm.ImageDir + ", pushed into the lab registry at `agentlab platform`"
}

func loginCmd() *cobra.Command {
	var password string
	var browser bool
	var audiences []string
	cmd := &cobra.Command{
		Use:   "login [email]",
		Short: "Log in as a lab user and write .token and kubeconfig.oidc (--audience agent-platform: a token muster's agent-platform servers accept; --browser: the real Dex login page)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			// Both paths end in the same place (lab.saveLoginArtifacts); they
			// differ only in how the token is obtained, and the login page
			// asks for the user itself.
			if browser {
				if len(args) == 1 || password != "" {
					return fmt.Errorf("--browser takes no user: the Dex login page asks for it (drop the argument and --password)")
				}
				if cmd.Flags().Changed("audience") {
					return fmt.Errorf("--browser mints the kubernetes audience only: --audience is the headless login's")
				}
				return lab.BrowserLogin(cfg)
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
			return lab.Login(cfg, email, pw, audiences)
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "password (default: the one in agentlab.yaml)")
	cmd.Flags().StringSliceVar(&audiences, "audience", lab.DefaultLoginAudiences,
		"the token's audience, repeatable: kubernetes (the apiserver; writes kubeconfig.oidc), agent-platform (muster and the agent-platform MCP servers, workspace-manager among them), dex-k8s-authenticator")
	cmd.Flags().BoolVar(&browser, "browser", false, "log in on the real Dex login page in a browser (authorization-code flow) instead of the headless password grant")
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
			cfg, err := loadProofLab()
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
	var opts lab.ModelsTestOptions
	cmd := &cobra.Command{
		Use:   "models-test [email]",
		Short: "Headless managed-models proof: 401 without a token, then pull -> ModelConfig -> dry runs, gitops_owned, a commit as the person on a fake GitHub -> agent turn -> MCP via muster -> unload -> delete (a refused delete + unwire where the server has none)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.ModelsTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Backend, "backend", "", "the backend to prove, one of platform.modelManager.backends (default: the first — model-manager's default backend)")
	cmd.Flags().StringVar(&opts.Model, "model", "", fmt.Sprintf("the model to pull, small and tool-calling capable (default: %s)", lab.ModelsTestModelDefaults()))
	cmd.Flags().StringVar(&opts.GitHubFakeBinary, "github-fake-binary", "", "the static Linux agentlab the fake GitHub API container of the commit proof runs on the kind network (default: this binary)")
	return cmd
}

func servingTestCmd() *cobra.Command {
	var opts lab.ServingTestOptions
	cmd := &cobra.Command{
		Use:   "serving-test [email]",
		Short: "Headless llm-d serving proof: the llmisvc controller, the well-known template and the models Gateway up -> 401 at muster without a token -> model-manager's tools through muster -> the lab preset fits the node (CPU, allocatable budget) -> load -> LLMInferenceService Ready on the CPU runtime -> ModelConfig wired at the model's route -> a completion through the models Gateway (401 without a token, 200 with) -> agent turn -> unload",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.ServingTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Preset, "preset", "", "the published preset to serve (default: the lab's, "+lab.ServingPresetName+")")
	cmd.Flags().BoolVar(&opts.SkipChat, "skip-chat", false, "skip the agent turn on the wired ModelConfig")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.DefaultServingReadyTimeout, "how long the model may take to serve: the weights download and the runtime's start")
	return cmd
}

func pmTestCmd() *cobra.Command {
	var opts lab.PMTestOptions
	cmd := &cobra.Command{
		Use:   "pm-test [email]",
		Short: "Headless platform-manager proof (platform.platformManager): the released giantswarm-platform-manager on the lab's registry fixture -> signed in through muster -> reconcile_capability's dry run of an installation with workspaces on -> the kept workspace-manager values",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.PMTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.GitHubFakeBinary, "github-fake-binary", "", "the static Linux agentlab the registry's container runs on the kind network (default: this binary)")
	cmd.Flags().StringVar(&opts.Record, "record", "", "write reconcile_capability's dry run to this file (how internal/lab/testdata/platform-manager/dry-run.json is recorded)")
	return cmd
}

func vmManagerTestCmd() *cobra.Command {
	var opts lab.VMManagerTestOptions
	cmd := &cobra.Command{
		Use:   "vm-manager-test [email]",
		Short: "Headless vm-manager proof (the VM provisioner as a pod of the node): 401 anonymous -> the person's token accepted -> x_vm-manager_* through muster (annotations, get_host, images, networks) -> create_vm -> ready -> attestation -> delete_vm",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.VMManagerTest(cfg, email, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.SkipVM, "skip-vm", false, "prove the registration, the identity boundary and the read tools only; boot no VM")
	cmd.Flags().DurationVar(&opts.VMTimeout, "vm-timeout", lab.DefaultVMManagerTestVMTimeout, "how long the proof's VM may take to reach ready (installer boot + installed boot to READY=1)")
	return cmd
}

func codingTestCmd() *cobra.Command {
	var opts lab.CodingTestOptions
	cmd := &cobra.Command{
		Use:   "coding-test [email]",
		Short: "Headless coding agents proof (platform.coding) on one coding Harness (--harness, default claude): the Harness Ready from the platform values (claude: no limits on it) -> validate_agent accepting a toolset with a muster write tool and refusing an origin outside the Harness's list -> a coding agent created through agent-manager as [email] (default the admin), Ready on the Harness (codex: its MCP binding's credential from a Secret) -> on a lab with the provider instance fake-bearer, a workspace the Session starts in through start_session -> one turn probing its sandbox: on claude, Claude Code as uid 65532 cannot read the root harness's environment or memory, the caller forwarder refuses dot segments and encoded slashes, muster refuses a write tool outside the toolset through it; everywhere no forge token or bearer in the environment, /data or /workspace, the model key and MCP credentials placeholders, a DNS canary recorded, a host outside the Agent's origins denied at the egress gate -> an MCP call through the agent's binding listing the agent (in a workspace Session audited by muster as the person) -> on claude a repository's instruction files written, the next turn records what Claude Code loaded -> suspend and resume keeping the file and the conversation -> the resumed turn's usage on its task status -> a share of the Session refused. On codex the model is the in-cluster mockllm (ModelConfig " + lab.MockLLMModelConfig + "), a mock Responses API scripted for these turns, which needs no OpenAI or Bedrock account; --model-config runs a real one",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				opts.Email = args[0]
			}
			return lab.CodingTest(cfg, opts)
		},
	}
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.DefaultCodingReadyTimeout, "how long each agent's boot and each wait outside a turn may take")
	cmd.Flags().DurationVar(&opts.TurnTimeout, "turn-timeout", 0, "how long each turn may take (default by the agent's model: 40m on a host model server's model, 10m on a hosted one); every turn prints its deadline")
	cmd.Flags().StringVar(&opts.Harness, "harness", config.CodingRuntimeClaude, fmt.Sprintf("the coding Harness the proof runs on (%s), one the lab renders (platform.coding.harnesses)", strings.Join(config.CodingRuntimes, ", ")))
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the agent runs on (default on codex the in-cluster mockllm, "+lab.MockLLMModelConfig+", else agent-manager's default one); codex needs an OpenAI Responses API or Bedrock gpt-* ModelConfig, such as the codex host model configure adds")
	cmd.Flags().StringVar(&opts.MockLLMBinary, "mockllm-binary", "", "the static linux agentlab binary the mockllm's container runs (default this one)")
	return cmd
}

func egressTestCmd() *cobra.Command {
	var opts lab.EgressTestOptions
	cmd := &cobra.Command{
		Use:   "egress-test [email]",
		Short: "Headless agent egress proof: an agent created through agent-manager as [email] (default the admin) with one declared origin, Ready with that spec.egress -> one turn whose bash tool probes from inside the actor: the declared origin reached, an undeclared HTTPS host answered 403 by the egress gate (its log: actor egress policy denied), muster answering with no egress entry, raw TCP to github.com:22 refused by name and by address",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				opts.Email = args[0]
			}
			return lab.EgressTest(cfg, opts)
		},
	}
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.DefaultEgressReadyTimeout, "how long the agent's boot may take")
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the agent runs on (default agent-manager's default one)")
	cmd.Flags().StringVar(&opts.Origin, "origin", lab.DefaultEgressOrigin, "the origin the agent declares and the probe reaches")
	cmd.Flags().BoolVar(&opts.WithoutEgress, "without-egress", false, "the control run: create the agent without its egress entry and probe the same origin; the declared stage must fail")
	return cmd
}

func workspacesMeasureCmd() *cobra.Command {
	var opts lab.WorkspacesMeasureOptions
	cmd := &cobra.Command{
		Use:   "workspaces-measure [email]",
		Short: "Measure workspaces through muster as [email] (default the admin): two workspaces, a small one and one with a large repository, created and deleted --runs times; the first sync, a no-op and a changed cycle, authorize_workspace, a Session's directory set up through start_session, the on-demand fetch, a first checkout, git status and a build on the workspace volume and on a local disk, the session directory's size, the volume's capacity and use, each mirror against the provider's size (W8's factor 1.5), the headroom max(4 GiB, 50 %) and one online expansion; median, p90 and max per figure, a figure the cluster cannot report listed as not available; JSON and a Markdown table written to the run directory and printed. --kube-context and --muster-url measure another cluster with the platform as the person signed in to its muster, pushing to a scratch branch on the provider with the person's git credentials, deleted afterwards. Lab only: [email] and --large-repo-mib; another cluster refuses them",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.Email = args[0]
			}
			if opts.KubeContext != "" || opts.MusterURL != "" {
				return lab.WorkspacesMeasure(nil, opts)
			}
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			return lab.WorkspacesMeasure(cfg, opts)
		},
	}
	cmd.Flags().IntVar(&opts.Runs, "runs", lab.DefaultWorkspacesMeasureRuns, "how many times each figure is measured: the workspaces are created and deleted once per run")
	cmd.Flags().IntVar(&opts.LargeRepoMiB, "large-repo-mib", 0, "the lab: add a repository of this many MiB to the lab's GitHub for the large workspace, removed afterwards (default: the fixture's agentlab-org/monorepo)")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.DefaultWorkspacesReadyTimeout, "how long each wait may take: a cycle, a Session's turn, a probe pod")
	cmd.Flags().StringVar(&opts.RunDir, "run-dir", "", "where the report is written (default state/workspaces-measure-<time>)")
	cmd.Flags().StringVar(&opts.GoProxy, "goproxy", "", "the module proxy the build's download goes through, a GOPROXY list (default the git image's, proxy.golang.org)")
	cmd.Flags().StringVar(&opts.KubeContext, "kube-context", "", "another cluster: the context of your kubeconfig the Kubernetes reads and probe pods use (with --muster-url)")
	cmd.Flags().StringVar(&opts.MusterURL, "muster-url", "", "another cluster: its muster, as the person `muster auth login --endpoint <url>` signed in (with --kube-context)")
	cmd.Flags().StringVar(&opts.Organization, "organization", "", "another cluster: the Organization the workspaces belong to")
	cmd.Flags().StringVar(&opts.Provider, "provider", "", "another cluster: the provider instance the repositories are on")
	cmd.Flags().StringSliceVar(&opts.SmallRepos, "small-repos", nil, "another cluster: the small workspace's repositories, owner/name; the first is the one checked out and built")
	cmd.Flags().StringVar(&opts.LargeRepo, "large-repo", "", "another cluster: the large workspace's repository, owner/name")
	cmd.Flags().StringVar(&opts.AgentgatewayURL, "agentgateway-url", "", "another cluster: its agentgateway, through which the Sessions' turns run (with --agent; without both the Session figures are not available)")
	cmd.Flags().StringVar(&opts.Agent, "agent", "", "another cluster: the Agent the Sessions start on, namespace/name")
	cmd.Flags().StringVar(&opts.GitURL, "git-url", "", "another cluster: the provider's git base URL the pushes go to, <url>/<owner>/<name>.git (default https://github.com)")
	return cmd
}

func workspacesTestCmd() *cobra.Command {
	var opts lab.WorkspacesTestOptions
	cmd := &cobra.Command{
		Use:   "workspaces-test [email]",
		Short: "Headless workspace storage proof: the NFS server, the CSI driver and the class in place -> a read-write-many claim bound -> a bare mirror seeded -> two pods on their own session sub-paths with a shared clone each, modes and links intact, isolated, the mirrors read-only -> the whole volume read-only -> the controller endpoint refused without Substrate's client certificate -> the actor-level mount (skipped until the Substrate line carries it) -> a claude Harness Session admitted on the volume with a workspace grant the manager's key set verifies, one headless turn in its own directory (not with --storage-only) -> everything removed. --part sync instead defines workspaces through the workspace-manager as [email] and proves their mirrors: preview, create, the Organization's boundary, the first cycle, a no-op cycle, a push and a force-push fetched alone, a dropped repository's mirror kept while borrowed, update, delete. --part access instead proves the access check at a Session's start as the admin and [email] (default dev@lab.local): connect_required with the sign-in link, connect_provider, refused naming the unreadable repository, the admin's grant and start_session with it, kagent's admission refusing the grant used by another person, an expired one and another workspace's, disconnect_provider revoking at the provider; no Session or actor after any refused start; the latency of authorize_workspace. --part gitlab instead proves the provider instance gitlab against a real GitLab: the picker, connect_provider through GitLab's sign-in, the token exchange, a workspace's mirror, start_session with the clone and no token in the Session, disconnect_provider; it skips without the instance. --part sessions instead starts --sessions Sessions (default 10) at once through start_session as the admin on the sync part's filter workspace after a push since its sync, alternating the claude and the kagent Harness: each in its own directory on the volume with a clone of every repository at its head, the pushed one fetched once per Session and nothing else, each isolated with the mirrors read-only, a deleted Session's directory gone with no cycle requested (at the next cycle before workspace-manager 0.6.0), a Session without a workspace in an empty directory, and after the lab's short cleanup window (configure --workspaces-cleanup-after) every directory removed, SessionsCleanedUp=True and a resumed Session in a fresh directory; the Sessions' timings go to state/workspaces-sessions-report.json. Lab only: the whole command, [email] included, runs against the lab",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				opts.Email = args[0]
			}
			return lab.WorkspacesTest(cfg, opts)
		},
	}
	cmd.Flags().BoolVar(&opts.StorageOnly, "storage-only", false, "prove the storage alone, without the claude Session admitted on the volume")
	cmd.Flags().StringVar(&opts.Part, "part", "", "run one part instead of the storage proof: "+strings.Join(lab.WorkspacesParts, ", ")+" (sync: the workspaces defined through the workspace-manager and their mirrors, as [email], default the admin; access: the provider sign-in and the access check at a Session's start, as the admin and [email], default dev@lab.local; gitlab: the provider instance gitlab end to end, skipped without one; sessions: parallel Sessions in their session directories and the cleanup, as the admin; caller-credential: a Session's actor JWT on the Bearer API host of the provider instance fake-bearer and muster's binding of its sandbox removed by delete_session, as the admin; write: git push and gh pr create from a workspace Session on the claude Harness with the workspace App's token from muster, pushes the App may not make refused, as the admin; refresh: a Session start past the workspace-manager's refresh threshold requests a sync without waiting for it, none within it or without a start, as the admin)")
	cmd.Flags().IntVar(&opts.Sessions, "sessions", 0, fmt.Sprintf("how many Sessions --part sessions starts at once, alternating the claude and the kagent Harness (default %d)", lab.DefaultWorkspacesSessions))
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.DefaultWorkspacesReadyTimeout, "how long each wait may take: the storage's rollouts, a claim bound, a pod finished, a volume gone, a sync cycle")
	return cmd
}

func workspacesSignInTestCmd() *cobra.Command {
	var opts lab.WorkspacesSignInTestOptions
	cmd := &cobra.Command{
		Use:   "workspaces-signin-test",
		Short: "Headless workspace-manager provider sign-in proof, the person's grant held by muster: a developer without it gets muster's sign-in link -> the admin and the developer grant the lab GitHub's App by the browser's walk (core_auth_login's link -> the fake's consent -> muster's callback), list_providers showing each connected -> a workspace over agentlab-org/platform-api and platform-infra created and synced -> authorize_workspace granted for the admin and refused naming platform-infra for the developer -> the workspace deleted, both grants dropped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			return lab.WorkspacesSignInTest(cfg, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Provider, "provider", "fake", "the provider instance whose App the people grant (the lab's GitHub, the one muster pins the workspace-manager to)")
	cmd.Flags().DurationVar(&opts.SyncTimeout, "sync-timeout", lab.DefaultWorkspacesSignInSyncTimeout, "how long the workspace's first sync may take")
	return cmd
}

func agentsGitOpsTestCmd() *cobra.Command {
	var opts lab.AgentsGitOpsTestOptions
	cmd := &cobra.Command{
		Use:   "agents-gitops-test [email]",
		Short: "Headless proof of editing an agent applied from git, as the portal does it: validate_agent refuses mode apply (gitops_owned) and validates mode commit, update_agent mode commit opens the pull request as the person on a fake GitHub, the live release untouched",
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
			return lab.AgentsGitOpsTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.GitHubFakeBinary, "github-fake-binary", "", "the static Linux agentlab the fake GitHub API container runs on the kind network (default: this binary)")
	return cmd
}

func agentsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agents-test [email]",
		Short: "Headless agent-manager proof through muster: get_info (caller) -> create -> ready -> update -> delete as the admin, a viewer's create Forbidden by the apiserver, the ServiceAccount without RBAC",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
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

func tenantsTestCmd() *cobra.Command {
	var opts lab.TenantsTestOptions
	cmd := &cobra.Command{
		Use:   "tenants-test [email email]",
		Short: "Headless tenant proof (platform.tenants) over two organization namespaces, one person an editor in each (default the first two users outside platform-admins and the tenant groups) and one tenant user bound by group in each: only the always-kept objects before an agent -> the other organization's create refused, the first agent's copies -> the worker admitted, a turn on the namespace's own credentials -> the same in the second -> through the edge, each tenant user lists their organization's agents and the platform tenant's, the other organization's refused, a user writes nothing, an editor writes their organization only, all_creators refused -> in Slack, per-person rosters with tenant labels, the other organization's pick and a collaborator refused -> the workers held against a cluster-admin -> the last delete removing the copies",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return fmt.Errorf("tenants-test takes no email or two (one editor per organization), got %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			return lab.TenantsTest(cfg, args, opts)
		},
	}
	cmd.Flags().StringVar(&opts.GatewayImage, "gateway-image", "", "the klaus-gateway image the Slack steps run on the host network (on macOS the kind network, HACKS.md U35; default: the release the lab's meta chart runs as its klaus-gateway component); the steps need one with giantswarm/klaus-gateway#486, 4.10.0 or later")
	cmd.Flags().StringVar(&opts.GatewayBinary, "gateway-binary", "", "a local klaus-gateway build to run instead of the image")
	return cmd
}

func a2aTestCmd() *cobra.Command {
	var readyTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "a2a-test [email]",
		Short: "Headless A2A proof: native gRPC through the edge as the surfaces drive it — the GRPCRoute and its JWT policy, no token refused, a forged x-user-id replaced, ListAgents with annotations, CreateSession idempotent, a streamed turn, HITL pause → approve / reject, CancelTask server-side",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.A2ATest(cfg, email, lab.A2ATestOptions{ReadyTimeout: readyTimeout})
		},
	}
	cmd.Flags().DurationVar(&readyTimeout, "ready-timeout", 4*time.Minute, "how long the fixture agent's golden boot may take before the proof gives up")
	return cmd
}

func toolsetsTestCmd() *cobra.Command {
	var opts lab.ToolsetsTestOptions
	cmd := &cobra.Command{
		Use:   "toolsets-test [email]",
		Short: "Headless toolset proof: agent-manager requires a toolset; the Agent carries the X-Muster-Toolset header; muster resolves and refuses per request; agents through kagent see their toolset; the OAuth-fixture sign-in scopes a server's tools to the person and client until the sign-out (G6); the portal's Tools step endpoints and apply path",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.ToolsetsTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the throwaway agents run on (default: default-model-config, the Anthropic one the lab renders from $ANTHROPIC_API_KEY)")
	cmd.Flags().BoolVar(&opts.SkipChat, "skip-chat", false, "skip the turns that need the model to answer (the runtime path, the chat-only agent, the real agent's view of the fixture)")
	cmd.Flags().BoolVar(&opts.SkipPortal, "skip-portal", false, "kagent API v2 only: skip the portal's apply path (the composed AgentTemplate through the scaffolder template) for a portal that does not speak kagent main yet; the Tools step's endpoints are proven regardless")
	return cmd
}

func skillsTestCmd() *cobra.Command {
	var opts lab.SkillsTestOptions
	cmd := &cobra.Command{
		Use:   "skills-test [email]",
		Short: "Headless skills proof (kagent API v2): an AgentTemplate with a git skill pinned to a full commit boots on the Go ADK Harness — the golden boot fetches the skill under Substrate's egress gate — and one turn as the user answers from the skill; a failed boot prints the evidence for the line's upstream issue",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.SkillsTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the throwaway agent runs on (default: default-model-config, the Anthropic one the lab renders from $ANTHROPIC_API_KEY)")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.SkillsTestReadyTimeout, "how long the golden boot may take to reach Ready on the Harness before the proof reports the failure")
	cmd.Flags().StringVar(&opts.Fixture.Repo, "skill-repo", "", "another fixture: the git repository of the skill to boot (an http(s) URL), with --skill-commit, --skill-path, --skill-question and --skill-expect (default: the public fixture, agent-self-awareness of giantswarm/agent-skills)")
	cmd.Flags().StringVar(&opts.Fixture.Commit, "skill-commit", "", "the full 40- or 64-hex commit id the skill is pinned to")
	cmd.Flags().StringVar(&opts.Fixture.Skill, "skill-path", "", "the skill's directory within the repository; its last element is the skill's name")
	cmd.Flags().StringVar(&opts.Fixture.Question, "skill-question", "", "the question the turn asks the agent to answer from the skill's text only")
	cmd.Flags().StringVar(&opts.Fixture.Expect, "skill-expect", "", "the answer only the skill's text has; the turn passes when the reply names the skill and carries it (case-insensitive)")
	cmd.Flags().StringVar(&opts.Fixture.CredentialSecret, "skill-secret", "", "a private repository: the Secret in the kagent namespace whose `token` key holds the base64 of \"<user>:<token>\" for the repository's host (git's Basic credential; x-access-token as the user for a GitHub token), referenced as skills[].source.git.credentialRef; the Secret is yours to create, the proof never reads it")
	cmd.Flags().BoolVar(&opts.SkillHost, "skill-fixture", false, "boot the skill from the lab's own private git host instead (no GitHub token): a Secret the proof creates, and the host's record of every request — `golden fetch: credential sent` when the golden boot's fetch carried it, `session request: no credential` when the Session's sandbox reaches the host without it")
	cmd.Flags().StringVar(&opts.SkillHostBinary, "skill-host-binary", "", "the static Linux agentlab the skill host fixture's container runs on the kind network (default: this binary)")
	return cmd
}

func substrateStepTestCmd() *cobra.Command {
	var opts lab.SubstrateStepOptions
	cmd := &cobra.Command{
		Use:   "substrate-step-test [email]",
		Short: "Headless Substrate step proof: an agent on the chart's Substrate with a conversation of three turns and a known file on its durable volume, suspended -> the substrate components pinned to --to and the platform upgraded in place under it -> the conversation resumes with its earlier turns and the file, and a new conversation starts from a golden in the snapshot layout the new release writes (taken again on a layout change, kept otherwise); by default under a drain (the WorkerPool's worker pods deleted every 8s for a minute from the candidate's ate-api-server Ready, the resume issued during it once the release before's ate-api-server pods are gone and its ResumeActor read in the candidate's RPC log, every golden of the lab in that layout with no terminal error); the Substrate versions before and after and PASS or FAIL with the cause per step, then the lab back on the chart's Substrate",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.SubstrateStepTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.To, "to", "", "the Substrate release to step to, exact (1.8.0, 1.8.1-rc.4); the lab must run an older release before it")
	cmd.Flags().StringToStringVar(&opts.DevImages, "step-dev-image", nil, "a platform.devImages entry for the step alone, <target>=<image> (repeatable), e.g. "+config.DevImageSubstrateAPI+"=<a local build of a change no release carries yet>")
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the agent runs on (default: default-model-config, the Anthropic one the lab renders)")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.SubstrateStepReadyTimeout, "how long the agent's golden boot before the step may take")
	cmd.Flags().DurationVar(&opts.RetakeTimeout, "retake-timeout", lab.SubstrateStepRetakeTimeout, "how long the goldens may take to be in the snapshot layout the new release writes (the re-take on a layout change) before the retake and new conversation steps fail")
	cmd.Flags().BoolVar(&opts.Stay, "stay", false, "leave the lab on the --to release instead of returning it to the chart's Substrate")
	cmd.Flags().BoolVar(&opts.NoDrain, "no-drain", false, "skip the drain case: by default every worker pod of the WorkerPool is deleted every 8s for a minute from the candidate's ate-api-server Ready, the suspended conversation is resumed during that drain once the release before's ate-api-server pods are gone (its ResumeActor must show in the candidate's RPC log while the drain runs), and every golden of the lab must be in the snapshot layout the candidate writes (taken again on a layout change, kept otherwise) with no terminal error")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func kagentStepTestCmd() *cobra.Command {
	var opts lab.KagentStepOptions
	cmd := &cobra.Command{
		Use:   "kagent-step-test [email]",
		Short: "Headless kagent step proof: an agent on the kagent release the lab runs with a conversation of one turn, suspended -> the kagent and kagent-crds components pinned to --to, the meta chart moved to the candidate --chart, or both, and the platform upgraded in place under it -> the conversation resumes, a turn on it answers the codeword of its first turn, it suspends and resumes again, and the conversation and the agent delete; the kagent line before and after and PASS per step, or FAIL naming the step, the resume, the turn, the suspend or the delete with the cause, then the lab back on its own configuration",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.KagentStepTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.To, "to", "", "the kagent release to step to, exact (1.14.0, 1.15.1-rc.2), pinned on the kagent and kagent-crds components; the lab must run an older release before it")
	cmd.Flags().StringVar(&opts.Chart, "chart", "", "the agent-platform release to step to, exact (4.145.1-rc.6): a candidate whose kagent range moves, or a chart-only one that keeps kagent; with --to as well, the kagent pin rides on it")
	cmd.Flags().StringToStringVar(&opts.DevImages, "step-dev-image", nil, "a platform.devImages entry for the step alone, <target>=<image> (repeatable), e.g. kagent=<a local build of a change no release carries yet>")
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the agent runs on (default: default-model-config, the Anthropic one the lab renders)")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.KagentStepReadyTimeout, "how long the agent's golden boot before the step may take")
	cmd.Flags().BoolVar(&opts.Stay, "stay", false, "leave the lab on the step instead of returning it to its own configuration")
	cmd.MarkFlagsOneRequired("to", "chart")
	return cmd
}

func substrateTunnelTestCmd() *cobra.Command {
	var opts lab.SubstrateTunnelOptions
	cmd := &cobra.Command{
		Use:   "substrate-tunnel-test",
		Short: "Headless Substrate tunnel-close proof: an HTTP/1.1-only actor (busybox nc on port 80, an ActorTemplate on the lab's alpine on the platform Harness's worker pool) reached through atenet-router's HTTP port -> HTTP/1.1 and h2c GETs answered (h2c downgraded to HTTP/1.1 at the actor) -> a gRPC call (h2c, application/grpc) fails at once, HTTP 502 or gRPC Unavailable within --promptly, not with its client's timeout: atunnel ends the CONNECT tunnel when the actor closes its connection; --substrate pins the substrate components to a release for the run and returns the lab to the chart's Substrate after",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			return lab.SubstrateTunnelTest(cfg, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Substrate, "substrate", "", "pin the substrate and substrate-crds components to this exact Substrate release for the run (1.11.3, 1.11.4-rc.1), older or newer than the lab's (default: the lab's Substrate, no pin)")
	cmd.Flags().DurationVar(&opts.ClientTimeout, "client-timeout", lab.SubstrateTunnelClientTimeout, "the gRPC call's client timeout")
	cmd.Flags().DurationVar(&opts.Promptly, "promptly", lab.SubstrateTunnelPromptly, "how soon the gRPC call must fail to pass: the tunnel ended with the actor's connection")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", lab.SubstrateTunnelReadyTimeout, "how long the actor's golden boot, its resume and its route through atenet-router may take")
	cmd.Flags().BoolVar(&opts.Stay, "stay", false, "leave the lab on the --substrate release instead of returning it to the chart's Substrate")
	return cmd
}

func klausGatewayTestCmd() *cobra.Command {
	var opts lab.KlausGatewayTestOptions
	cmd := &cobra.Command{
		Use:   "klaus-gateway-test [email]",
		Short: "Headless Swarmgeist proof (kagent API v2) through the Slack adapter: klaus-gateway runs on the host against the lab's edge (A2A v1 over gRPC, TLS with the lab CA, JWT at the edge) with a fake Slack Web API, signed Events API messages and Block Kit clicks, the user linked by a record in its OBO link store that carries the user's Dex id_token — `@bot agents` lists the Agent (one on a Harness that does not exist hidden there and in the picker its Select button opens, and refused), an unlinked person is asked to sign in, one branded turn attributed to the person at muster, Approve and Deny on the approval card, `stop` cancelled server-side, a restart on the stores that keeps the thread → Session mapping, a restart mid-turn with the edge out of reach for 45 s whose answer is still posted, a restart mid-sign-in that replays the parked message and one mid-approval whose Approve resumes the paused task; with platform.klausGateway on, the meta chart's in-cluster component too — the Role scoped to the OBO link Secret, two links seeded through the store package, the pod deleted and its replacement Ready with the same links, one Slack turn through the pod; with --workspaces, the workspace a thread's Session works in",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.KlausGatewayTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.GatewayImage, "gateway-image", "", "the klaus-gateway image to run on the host network (default: the release the lab's meta chart runs as its klaus-gateway component; a 0.x image is the documented negative: it cannot speak A2A v1 over gRPC)")
	cmd.Flags().StringVar(&opts.GatewayBinary, "gateway-binary", "", "a local klaus-gateway build to run instead of the image — the proof of a branch")
	cmd.Flags().IntVar(&opts.Port, "gateway-port", 0, "host port of the gateway's Slack endpoints; the admin endpoints take the next port, the fake Slack Web API the one after when it runs in this process (default: the lab's own, 18090 plus ten per port its apiServerPort sits above 6443, so two labs never share one)")
	cmd.Flags().StringVar(&opts.Harness, "harness", "", "the Harness the fixture is admitted by and booted on, e.g. a coding Harness (default: the platform Harness kagent)")
	cmd.Flags().StringVar(&opts.SlackFakeBinary, "slack-fake-binary", "", "the static Linux agentlab the fake Slack Web API container runs on the kind network while platform.klausGateway is on (default: this binary)")
	cmd.Flags().StringVar(&opts.ModelConfig, "model-config", "", "the kagent ModelConfig the fixture runs on (default: default-model-config, the Anthropic one the lab renders)")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", 0, "how long the fixture's golden boot may take to reach Ready on the Harness (default 10m)")
	cmd.Flags().StringVar(&opts.RunDir, "run-dir", "", "directory for the gateway's stores, keys and log, kept afterwards (default: a temporary directory, removed)")
	cmd.Flags().BoolVar(&opts.Workspaces, "workspaces", false, "add the workspaces steps, the workspace a thread's Session works in, on a lab with the workspace-manager (`configure --workspaces`): a new thread asked once and its Session in its session directory, two more turns and a restart asking nothing, No workspace, the slash command's picker and its Workspace select preset, a workspace refused naming the repository with no Session, a collaborator refused privately; without the workspace-manager it fails before anything runs")
	return cmd
}

func decisionsTestCmd() *cobra.Command {
	var opts lab.DecisionsTestOptions
	cmd := &cobra.Command{
		Use:   "decisions-test [email]",
		Short: "Headless proof of klaus-gateway's decisions (POST /decisions): the gateway on the host with its reviews endpoint, a ServiceAccount token the lab's API server vouches for, a fake Slack Web API; a team decision answered by a Choose click, a person's (found by email, a direct message) in the modal with an option and own words, a team decision by a reply in its thread, each answer a muster tool call as the linked user; a refused answer as a status line, a decision closed as defaulted refusing a late click; a conversation (POST /conversations) whose thread reply calls a muster tool as the person and whose service answer lands in the thread, a refused reply noted as not delivered",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			email := cfg.AdminUser().Email
			if len(args) == 1 {
				email = args[0]
			}
			return lab.DecisionsTest(cfg, email, opts)
		},
	}
	cmd.Flags().StringVar(&opts.GatewayImage, "gateway-image", "", "the klaus-gateway image to run on the host network (default: the release the lab's meta chart runs as its klaus-gateway component)")
	cmd.Flags().StringVar(&opts.GatewayBinary, "gateway-binary", "", "a local klaus-gateway build to run instead of the image — the proof of a branch")
	cmd.Flags().IntVar(&opts.Port, "gateway-port", 0, "host port of the gateway's endpoints; the admin endpoints take the next port, the fake Slack Web API the one after (default: the lab's own, 18090 plus ten per port its apiServerPort sits above 6443, so two labs never share one)")
	cmd.Flags().StringVar(&opts.RunDir, "run-dir", "", "directory for the gateway's stores, keys and log, kept afterwards (default: a temporary directory, removed)")
	return cmd
}

func beekeeperDecisionsTestCmd() *cobra.Command {
	var opts lab.BeekeeperDecisionsTestOptions
	cmd := &cobra.Command{
		Use:   "beekeeper-decisions-test",
		Short: "Headless proof of beekeeper's decisions in Slack: beekeeper serve on the host over the lab's beekeeper resources, behind muster with the person's token forwarded, klaus-gateway and the fake Slack Web API; a decision for a person answered by their Choose click after another user's click was refused, a team's answered by a thread reply, one closed with its default at its due time refusing a late click, one withdrawn, each outcome an Event",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.BeekeeperDecisionsTest(cfg, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Version, "beekeeper-version", lab.BeekeeperDecisionsVersionDefault, "the beekeeper release whose binary and CRDs run")
	cmd.Flags().StringVar(&opts.Binary, "beekeeper-binary", "", "a local linux beekeeper build to run instead of the release's binary — the proof of a branch")
	cmd.Flags().StringVar(&opts.GatewayImage, "gateway-image", "", "the klaus-gateway image to run on the host network (default: the release the lab's meta chart runs as its klaus-gateway component)")
	cmd.Flags().BoolVar(&opts.GuideOnly, "guide-only", false, "run the guide's conversation steps (6 to 9) without the decisions (1 to 5)")
	cmd.Flags().StringVar(&opts.LocalBeekeeper, "local-beekeeper", "", "a host beekeeper build for step 9: the person's own machine, its standby watch delivering the Slack reply into a background Claude Code guide session (claude on PATH)")
	cmd.Flags().StringVar(&opts.GatewayBinary, "gateway-binary", "", "a local klaus-gateway build to run instead of the image")
	cmd.Flags().IntVar(&opts.Port, "gateway-port", 0, "host port of the gateway's endpoints; the next four take its admin endpoints, the fake Slack Web API, beekeeper serve and its Postgres (default: the lab's own, 18090 plus ten per port its apiServerPort sits above 6443, so two labs never share one)")
	cmd.Flags().StringVar(&opts.RunDir, "run-dir", "", "directory for the stores, keys, serve's configuration and the logs, kept afterwards (default: a temporary directory, removed)")
	return cmd
}

func beekeeperCentralTestCmd() *cobra.Command {
	var opts lab.BeekeeperCentralTestOptions
	cmd := &cobra.Command{
		Use:   "beekeeper-central-test",
		Short: "Headless proof of beekeeper's central resources: beekeeper serve on the host behind muster, two local beekeepers as two lab users through it; a lease one claims refused to the other with its holder, a central lane hold stopping the other's merges, two merges into one lane taking turns, a machine's agent on the central roster, an unreachable central instance refused with exit 69",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return lab.BeekeeperCentralTest(cfg, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Version, "beekeeper-version", lab.BeekeeperCentralVersionDefault, "the beekeeper release whose binary and CRDs run")
	cmd.Flags().StringVar(&opts.Binary, "beekeeper-binary", "", "a local static linux beekeeper build for serve and both machines instead of the release's binary — the proof of a branch")
	cmd.Flags().IntVar(&opts.Port, "port", 18093, "beekeeper serve's port in the node's network; the next one takes its Postgres")
	cmd.Flags().StringVar(&opts.RunDir, "run-dir", "", "directory for serve's configuration, the two machines and the logs, kept afterwards (default: a temporary directory, removed)")
	return cmd
}

func backstageTestCmd() *cobra.Command {
	var opts lab.BackstageTestOptions
	cmd := &cobra.Command{
		Use:   "backstage-test [email...]",
		Short: "Headless Backstage sign-in + muster proof (default: every user)",
		Long: `Drives the Backstage sign-in headlessly for every user (or the ones named) and
the muster hop with each user's own forwarded token; with platform.agents on,
the Dev Portal's Agent Platform pages the way a person drives them: the
wizard's create path, the agents list per user, the chat, HITL and Stop, the
edit path and the delete. Everything it creates is removed on every path.

--agent <namespace>/<name> runs the Agent Platform half against an agent that
already exists instead: the create path, the HITL fixture, the edits and every
delete are skipped, the agent is waited for until Ready, listed on every
user's roster and chatted with, and left in place. Use it where the create
step fails or is not wanted, or to repeat only the chat steps.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadProofLab()
			if err != nil {
				return err
			}
			return lab.BackstageTest(cfg, args, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", "", "an existing agent as <namespace>/<name> (namespace kagent): skip the create and teardown steps and prove the roster and the chat on it, leaving it in place")
	return cmd
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

// selfUpdateCmd replaces the running binary with the latest GitHub release
// once its signature verifies — the command muster and mcp-kubernetes ship —
// or, with --check, only says whether one exists.
func selfUpdateCmd() *cobra.Command {
	var opts update.Options
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this binary with the latest GitHub release (--check only reports whether one exists)",
		Long: `Looks up the latest release of ` + update.Repository + ` on GitHub and, when it is
newer than this binary, installs its binary for this OS and architecture over
the running executable. --check only reports both versions (exit status 125
when a newer release exists).

Release binaries are signed in CI (cosign, keyless) and published next to
their Sigstore bundle. The downloaded binary is installed only after that
bundle verifies for a CircleCI build of ` + update.Repository + `; a release
without a bundle, or a download that does not match its signature, is refused
and the installed binary stays as it is.

Release candidates (v0.97.0-rc.1, published as GitHub prereleases) are left
out unless --pre: then the newest of the release candidates and the stable
releases is installed, so a release candidate moves to the next one, or to
the stable release that follows it, and is verified the same way. The hint
ahead of other commands names stable releases only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return update.Run(cmd.Context(), cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.Pre, "pre", false, "follow release candidates: install the newest release candidate or stable release, whichever is newer")
	cmd.Flags().BoolVar(&opts.Check, "check", false, "report the running and the latest release without installing anything; exit status 125 when a newer one exists")
	return cmd
}
