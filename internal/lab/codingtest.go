package lab

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
	apiv1alpha1 "github.com/giantswarm/agentlab/internal/kagent/gen/kagent/api/v1alpha1"
)

// The coding agents proof: the security floor of a coding agent and its
// headless lifecycle on one coding Harness the platform values render
// (coding.go), with an agent created through agent-manager as the person,
// the way the portal creates one. --harness picks the Harness, claude by
// default; the stages that only make sense on claude (the root harness's
// uid split, the caller forwarder, Claude Code's instruction files) run on
// claude alone.
//
//  1. The Harness Ready from the values (on claude: no limits on it).
//  2. agent-manager's write-time checks on the Harness: validate_agent
//     accepts a toolset with a muster write tool and refuses an egress origin
//     outside the Harness's list. An agent-manager below
//     agentManagerAnyToolsetFloor refuses the write tool instead, and the
//     proof expects that refusal on it. Agent Plugins (codingplugins.go):
//     validate_agent accepts the example package with its skill selected on
//     every Harness of the namespace; a public Claude-format package is
//     refused by validate_agent and create_agent on codex, nothing written;
//     the refusal on kagent skips until agent-manager checks plugins there.
//  3. create_agent on the Harness with the read-only preset plus that write
//     tool (the read-only preset alone below the floor) and the example
//     package, on claude with the Claude-format package beside it:
//     Ready, its
//     RemoteMCPServer Accepted and sending the toolset, the Agent's egress
//     the Harness's origin list. On codex and omp a binding with a header
//     from a Secret carries its credential on upstream's Secret-backed path,
//     which the controller hands the runtime as kagent's placeholder: every
//     Secret key it names is there. A binding with none, a platform line that
//     ships no Secret-backed credential, skips the stage.
//  4. One Session, one turn whose shell tool probes the sandbox; its lines
//     are judged stage by stage. On claude: Claude Code runs as the
//     unprivileged uid and cannot read the harness's environment or memory;
//     the caller forwarder refuses dot segments and encoded slashes, answers
//     404 for a server it does not front and 401 without its token; a write
//     tool outside the agent's toolset is refused by muster through the
//     forwarder with the turn's bearer. On every Harness: no forge token or
//     bearer in the environment, /data or /workspace, the model key and
//     every MCP credential a placeholder; a DNS lookup of a name outside the
//     allowlist recorded (through the cluster resolver and directly); a host
//     outside the Agent's origins denied at the egress gate, an origin of
//     its list reached. Then the plugin skills: the agent has the selected
//     ones and no other (on the mockllm, the requests the runtime sent carry
//     the selected skill); on claude the Claude-format package's hook ran,
//     and update_agent moving the agent to codex refused, nothing written.
//  5. An MCP call: the agent calls agent-manager's list_agents through its
//     binding and finds itself listed.
//  6. The turn gap (codingturngap.go): a heartbeat one turn starts in the
//     sandbox does not run between turns and runs again in the next.
//  7. On claude, the instruction-file fixture: one turn writes a
//     repository's instruction sources into the Session's directory
//     (CLAUDE.md, AGENTS.md, .claude settings with a hook and
//     permissions.allow, a skill, a subagent, a command, .mcp.json), the next
//     records what Claude Code loaded. Elsewhere one turn writes the file the
//     resume reads back.
//  8. Suspend and resume: the Session suspended to the snapshot store and
//     resumed; a turn reads the first turn's file and recalls its code word.
//  9. Usage: the resumed turn's task status carries its usage.
// 10. A share of the Session refused: a colleague's turn in a coding agent's
//     Session would run under code the first sender's turns left.
// 11. The empty selection: update_agent with the example package's selection
//     empty, the agent Ready on a new revision, a new Session's agent
//     without its skill.
// 12. On codex without --model-config, the model: every model call of the
//     turns answered by the in-cluster mockllm's script (mockllm.go).
//
// The agents and Sessions are removed on every exit path.

// codingRuntime is what the proof knows of one coding runtime: how its
// agent is asked to run commands, where its model key is, and which of the
// runtime-bound stages apply.
type codingRuntime struct {
	// name is the runtime and the Harness the lab renders of it.
	name string
	// shellTool is how a prompt names the runtime's shell tool.
	shellTool string
	// modelKeys are the environment variables the runtime reads its model
	// key from; the probe reports whether any carries more than kagent's
	// placeholder.
	modelKeys []string
	// claude marks Claude Code's own stages: the Harness's limits, the root
	// harness's uid split, the caller forwarder and the instruction files.
	claude bool
	// mcpCredentialPrefix names the environment variables the runtime is
	// handed its Secret-backed MCP credentials in; "" where it has none of
	// its own.
	mcpCredentialPrefix string
	// secretBacked is whether the agent's MCP binding carries its credential
	// on upstream's Secret-backed path: a RemoteMCPServer header from a
	// Secret.
	secretBacked bool
}

// The environment variables the codex runtime and a BYO Harness (omp) read
// their model key and their Secret-backed MCP credentials from: names, never
// values.
const (
	codexOpenAIKeyEnv      = "OPENAI_API_KEY"               // #nosec G101 -- a variable's name
	codexBedrockKeyEnv     = "AWS_BEARER_TOKEN_BEDROCK"     // #nosec G101 -- a variable's name
	codexMCPCredentialEnvs = "KAGENT_CODEX_MCP_CREDENTIAL_" // #nosec G101 -- a variable name's prefix
	byoMCPCredentialEnvs   = "KAGENT_CREDENTIAL_"           // #nosec G101 -- a variable name's prefix
	anthropicKeyEnv        = "ANTHROPIC_API_KEY"            // #nosec G101 -- a variable's name
)

// codingRuntimes are the runtimes coding-test proves, by name.
var codingRuntimes = map[string]codingRuntime{
	config.CodingRuntimeClaude: {
		name: config.CodingRuntimeClaude, shellTool: "Bash tool",
		modelKeys: []string{anthropicKeyEnv}, claude: true,
	},
	config.CodingRuntimeCodex: {
		name: config.CodingRuntimeCodex, shellTool: "shell tool",
		modelKeys:           []string{codexOpenAIKeyEnv, codexBedrockKeyEnv},
		mcpCredentialPrefix: codexMCPCredentialEnvs, secretBacked: true,
	},
	config.CodingRuntimeOmp: {
		name: config.CodingRuntimeOmp, shellTool: "bash tool",
		modelKeys:           []string{anthropicKeyEnv},
		mcpCredentialPrefix: byoMCPCredentialEnvs, secretBacked: true,
	},
}

const (
	// codingTestAgent is the proof's agent, deleted by the same run.
	codingTestAgent = "agentlab-coding"
	// codingTestToolset is the coding agent's toolset below
	// agentManagerAnyToolsetFloor, where a claude Harness agent is held to
	// read-only tools.
	codingTestToolset = presetReadOnly
	// codingTestGrantedTool is the muster write tool the coding agent's
	// toolset adds from agentManagerAnyToolsetFloor on: which tools a coding
	// agent uses is the person's choice.
	codingTestGrantedTool = "tool:" + agentManagerToolPrefix + "create_agent"
	// agentManagerAnyToolsetFloor is the first agent-manager release that
	// writes a claude Harness agent with any muster toolset; a release
	// candidate of it counts.
	agentManagerAnyToolsetFloor = "1.14.0-0"
	// codingTestWriteTool is the write tool the sandbox calls through the
	// forwarder, outside the read-only toolset; its argument names no agent,
	// so a call muster let through would change nothing.
	codingTestWriteTool = agentManagerToolPrefix + "delete_agent"
	// codingTestReadTool is the tool the agent calls through its binding: it
	// lists the agent itself.
	codingTestReadTool = agentManagerToolPrefix + "list_agents"
	// musterCallTool is muster's meta-tool that runs a catalogue tool: muster
	// lists its meta-tools to an MCP client, and an aggregated tool such as
	// codingTestReadTool is called through it, never by its own name.
	musterCallTool            = "call_tool"
	codingTestAbsentAgent     = "agentlab-coding-absent"
	codingTestOutsideToolset  = "outside the toolset"
	codingTestRefusedOrigin   = "https://agentlab-refused-origin.example.org"
	codingTestOutsideHost     = "https://proxy.golang.org/"
	codingTestDNSCanary       = "example.org"
	codingTestDNSResolver     = "1.1.1.1"
	codingTestCodeWord        = "agentlab-saffron"
	codingTestOwnFile         = ".agentlab-first-turn"
	codingTestHookFile        = ".agentlab-hook-ran"
	codingTestInstructionMark = "agentlab-claude-md-loaded"
	codingTestAgentsMDMark    = "agentlab-agents-md-loaded"
	codingTestFixtureName     = "agentlab-fixture"
	// codingTestSystemPrompt keeps the turns to the probes.
	codingTestSystemPrompt = "You are a probe in a test lab. Run exactly the commands you are given with your shell tool, once, and reply with the output lines you are asked for, verbatim. Answer questions about this session plainly."
	// claudeUnprivilegedUID is the uid the claude-harness image runs Claude
	// Code as beside its root harness.
	claudeUnprivilegedUID = "65532"
	// harnessProcessName is the harness's binary, the image's entrypoint.
	harnessProcessName = "kagent-claude"
)

// DefaultCodingReadyTimeout bounds each boot and wait of the proof.
const DefaultCodingReadyTimeout = 10 * time.Minute

// The turn budgets by the model the agent runs on: a hosted model answers a
// turn's calls in seconds, a model a host model server serves on the lab
// machine's own hardware (qwen3.5:9b on Ollama, for one) takes a minute and
// more per call, and a coding runtime makes several calls per turn.
const (
	hostedModelTurnTimeout = 10 * time.Minute
	hostModelTurnTimeout   = 40 * time.Minute
)

// modelManagerBackendLabel names the backend a model-manager ModelConfig's
// model is served by; in the lab every backend is a host model server.
const modelManagerBackendLabel = "model-manager.giantswarm.io/backend"

// CodingTestOptions tunes the coding agents proof.
type CodingTestOptions struct {
	// Email is the lab user the agents are created by and the turns sent
	// as; empty is the admin.
	Email string
	// ReadyTimeout bounds each agent's boot and each wait outside a turn.
	ReadyTimeout time.Duration
	// TurnTimeout bounds each turn; zero is the budget of the model the
	// agent runs on (hostModelTurnTimeout on a host model server's model,
	// hostedModelTurnTimeout otherwise).
	TurnTimeout time.Duration
	// Harness is the coding Harness the proof runs on, one of the runtimes
	// the lab renders (platform.coding.harnesses); empty is claude.
	Harness string
	// ModelConfig is the kagent ModelConfig the agent runs on; empty is
	// the in-cluster mockllm on codex (MockLLMModelConfig), else
	// agent-manager's default one.
	ModelConfig string
	// MockLLMBinary is the agentlab binary the mockllm's container runs;
	// empty is this one.
	MockLLMBinary string
	// MusterTools is the agent's tool selection on its muster binding
	// (the Generic chart's muster.tools), as a platform agent selects
	// muster's meta-tools; MusterMetaTools names all of them. Empty binds
	// muster without a selection.
	MusterTools []string
	// Part runs one part instead of the coding agents proof: CodingPartDevctl
	// (codingdevctl.go). "" is the proof.
	Part string
	// Repository is the devctl part's proof repository, owner/name; empty is
	// DefaultDevctlProofRepository.
	Repository string
	// Toolchains are the toolchains part's, name[@version] each; empty is
	// DefaultCodingToolchains.
	Toolchains []string
}

// MusterMetaTools are muster's meta-tools, the selection a platform coding
// agent binds.
var MusterMetaTools = []string{"list_tools", "describe_tool", "list_core_tools", "filter_tools", "call_tool", "list_resources", "describe_resource", "get_resource", "list_prompts", "describe_prompt", "get_prompt"}

// CodingTest is the coding agents proof (the stages above).
func CodingTest(cfg *config.Config, opts CodingTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultCodingReadyTimeout
	}
	switch opts.Part {
	case "", CodingPartToolchains:
		if opts.Repository != "" {
			return fmt.Errorf("--repository is the devctl part's (--part %s)", CodingPartDevctl)
		}
		if opts.Part == "" && len(opts.Toolchains) > 0 {
			return fmt.Errorf("--toolchains is the toolchains part's (--part %s)", CodingPartToolchains)
		}
	case CodingPartDevctl:
		if opts.Harness != "" && opts.Harness != config.CodingRuntimeClaude || opts.ModelConfig != "" || opts.Email != "" || len(opts.MusterTools) > 0 || len(opts.Toolchains) > 0 {
			return fmt.Errorf("--part %s runs a claude agent of its own as the admin, on agent-manager's default model: it takes no [email], --harness, --model-config, --muster-tools or --toolchains", CodingPartDevctl)
		}
		return codingDevctlPart(cfg, opts)
	default:
		return fmt.Errorf("no part %q of coding-test: --part takes %s", opts.Part, strings.Join(CodingParts, ", "))
	}
	if !cfg.CodingEnabled() {
		return fmt.Errorf("platform.coding is off in %s — `agentlab configure --coding` turns it on, then `agentlab platform`", config.File)
	}
	rt, ok := codingRuntimes[cmp.Or(opts.Harness, config.CodingRuntimeClaude)]
	if !ok {
		return fmt.Errorf("--harness %q: coding-test proves %s", opts.Harness, strings.Join(slices.Sorted(maps.Keys(codingRuntimes)), ", "))
	}
	if !cfg.CodingHarnessRendered(rt.name) {
		return fmt.Errorf("the lab renders no Harness %s: add it to platform.coding.harnesses in %s (`agentlab configure --coding-harnesses %s`), then `agentlab platform`", rt.name, config.File, strings.Join(append(slices.Clone(cfg.CodingHarnesses()), rt.name), ","))
	}
	user := cfg.AdminUser()
	if opts.Email != "" {
		user = cfg.FindUser(opts.Email)
	}
	if user == nil {
		return fmt.Errorf("no lab user %q in %s", opts.Email, config.File)
	}
	p := &codingProof{cfg: cfg, rt: rt, modelConfig: opts.ModelConfig, user: user, timeout: opts.ReadyTimeout, turnTimeout: opts.TurnTimeout, musterTools: opts.MusterTools, stages: newProofStages(), toolchains: opts.Toolchains}
	if len(p.toolchains) == 0 {
		p.toolchains = DefaultCodingToolchains
	}
	if opts.ModelConfig == "" && rt.name == config.CodingRuntimeCodex {
		p.mockBinary = opts.MockLLMBinary
		if p.mockBinary == "" {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locating this binary for the mockllm's container: %w (pass --mockllm-binary)", err)
			}
			p.mockBinary = exe
		}
		if err := linuxStaticBinary(p.mockBinary, mockLLMWhat, "--mockllm-binary"); err != nil {
			return err
		}
	}
	defer p.cleanup()
	run := p.run
	if opts.Part == CodingPartToolchains {
		run = p.runToolchains
	}
	err := run()
	if err != nil && p.mock != nil {
		err = fmt.Errorf("%w\nthe mockllm's requests, the tools and the last input item of each:\n%s", err, p.mock.logTail())
	}
	return p.stages.close(err)
}

// codingProof is one run's state.
type codingProof struct {
	cfg *config.Config
	// rt is the runtime, and Harness, under proof.
	rt          codingRuntime
	modelConfig string
	user        *config.User
	timeout     time.Duration
	// turnTimeout bounds each turn, turnBudget says where it comes from:
	// the flag or the model the agent runs on.
	turnTimeout time.Duration
	turnBudget  string
	stages      *proofStages
	muster      *musterSession
	api         *kagentAPI
	sessions    *sessionSet
	// line names the releases the proof ran on, for every verdict.
	line string
	// anyToolset is whether the line's agent-manager writes a claude
	// Harness agent with any muster toolset.
	anyToolset bool
	// mockBinary is the binary the mockllm runs, set when the agent runs on
	// it; mock is the running mockllm.
	mockBinary string
	mock       *mockLLM
	// noMCPCredential is why the secret-backed credential stage skipped:
	// the runtime's binding carries no credential on the line.
	noMCPCredential string
	// musterTools is the agent's tool selection on muster; empty is none.
	musterTools []string
	// toolchains are the toolchains part's, name[@version] each.
	toolchains []string
	// harnesses are the Harnesses of the agents' namespace.
	harnesses []string
	// codexModel is the Responses API ModelConfig the codex plugin rows
	// write with; mockModelPlaced says the run placed the mockllm's for them.
	codexModel      string
	mockModelPlaced bool
}

func (p *codingProof) run() error {
	ctx := context.Background()
	harness := p.rt.name
	if p.rt.claude {
		p.stages.begin("harnesses", "The coding Harness %s Ready from the platform values, no limits on it", harness)
	} else {
		p.stages.begin("harnesses", "The coding Harness %s Ready from the platform values", harness)
	}
	if err := waitHarnessReady(ctx, harness, p.timeout); err != nil {
		return err
	}
	if p.rt.claude {
		limits, err := readHarnessLimits(ctx, harness)
		if err != nil {
			return err
		}
		if limits != nil {
			return fmt.Errorf("the Harness %s carries spec.claude.limits %v, which the values no longer render", harness, limits)
		}
		note("Harness %s/%s Ready, no limits", kagentNamespace, harness)
	} else {
		note("Harness %s/%s Ready", kagentNamespace, harness)
	}

	if err := p.connect(ctx); err != nil {
		return err
	}
	if !p.anyToolset {
		note("agent-manager below %s holds a coding agent to read-only tools: the write tool's refusal is expected", agentManagerAnyToolsetFloor)
	}
	modelConfig, err := p.model()
	if err != nil {
		return err
	}
	p.removeAgents()
	if p.harnesses, err = namespaceHarnesses(ctx); err != nil {
		return err
	}
	codex := slices.Contains(p.harnesses, config.CodingRuntimeCodex)
	if codex {
		if err := p.prepareCodexModel(modelConfig); err != nil {
			return err
		}
	}

	verdict := "accepted"
	if !p.anyToolset {
		verdict = "refused"
	}
	p.stages.begin("write-time checks", "%svalidate_agent on Harness %s as %s: a toolset with %s %s, an origin outside the Harness's list refused", agentManagerToolPrefix, harness, p.user.Email, codingTestGrantedTool, verdict)
	spec := agentSpec{
		Name: codingTestAgent, ModelConfig: modelConfig, Harness: harness, SystemMessage: codingTestSystemPrompt,
		Description: "Throwaway coding agent of `agentlab coding-test`; deleted by the same run.",
	}
	if err := p.proveGuards(spec); err != nil {
		return err
	}

	p.stages.begin("plugins accepted", "%svalidate_agent on every Harness of the namespace (%s): %s @ %.12s with skill %s selected, no plugin refused", agentManagerToolPrefix, strings.Join(p.harnesses, ", "), pluginExampleURL, pluginExampleCommit, pluginExampleSkill)
	if err := p.provePluginsAccepted(spec); err != nil {
		return err
	}
	p.stages.begin("claude-format refused on codex", "%s %s (Claude-format) on Harness codex: validate_agent and create_agent refuse it, naming the root plugin.json, and write nothing", claudePluginURL, claudePluginPath)
	if !codex {
		p.stages.skip("the lab renders no Harness codex: add it to platform.coding.harnesses in %s (`agentlab configure --coding-harnesses %s`), then `agentlab platform`", config.File, strings.Join(append(slices.Clone(p.cfg.CodingHarnesses()), config.CodingRuntimeCodex), ","))
	} else if err := p.proveClaudeFormatRefused(spec); err != nil {
		return err
	}
	p.stages.begin("claude-format refused on kagent", "The Claude-format package on Harness %s: validate_agent and create_agent refuse it, naming the root plugin.json", kagentHarness)
	p.stages.skip("agent-manager runs its plugin check on the kagent Harness from %s, not released on this line (%s): until then a Claude-format git plugin passes the write on kagent and fails at its golden boot", kagentPluginCheck, p.line)

	spec.Toolset, spec.Plugins = p.toolset(), p.codingPlugins([]string{pluginExampleSkill})
	p.stages.begin("create", "%screate_agent %s on Harness %s, toolset %v, plugins %s: Ready, its RemoteMCPServer Accepted with the toolset, the Agent's egress the Harness's origins", agentManagerToolPrefix, codingTestAgent, harness, spec.Toolset, p.pluginsWords())
	egress, err := p.createReady(spec)
	if err != nil {
		return err
	}
	if err := p.selectMusterTools(); err != nil {
		return err
	}
	if err := p.proveBinding(spec.Toolset); err != nil {
		return err
	}
	note("Agent %s Ready on %s, egress [%s]", codingTestAgent, harness, strings.Join(egress, " "))
	if p.mockBinary != "" {
		if err := p.startMock(); err != nil {
			return err
		}
	}

	if p.rt.secretBacked {
		p.stages.begin("secret-backed credential", "The agent's MCP binding carries its credential on upstream's Secret-backed path: a RemoteMCPServer header from a Secret")
		if err := p.proveSecretBacked(); err != nil {
			return err
		}
	}

	if p.rt.claude {
		// The write tool the sandbox calls must exist for the person: its
		// refusal through the agent's endpoint is then the toolset's.
		tools, err := p.muster.listTools()
		if err != nil {
			return err
		}
		if !slices.Contains(tools, codingTestWriteTool) {
			return fmt.Errorf("%s is not in %s's muster catalogue: the sandbox's write-tool refusal would prove nothing", codingTestWriteTool, p.user.Email)
		}
	}

	p.stages.begin("sandbox turn", "A Session of %s as %s, one turn whose %s probes the sandbox", codingTestAgent, p.user.Email, p.rt.shellTool)
	session, err := p.createSession(ctx, codingTestAgent)
	if err != nil {
		return err
	}
	sandbox, err := p.turn(ctx, session, codingSandboxPrompt(p.rt))
	if err != nil {
		return err
	}
	reply, probe := sandbox.reply, sandbox.probe
	if probe["uid"] == "" {
		return fmt.Errorf("the reply carries no %s lines (did the agent run its %s?): %s", probeMarker, p.rt.shellTool, excerpt(reply, 400))
	}
	note("Session %s: %d probe lines", session.GetId(), len(probe))

	for _, judged := range p.judgedStages() {
		p.stages.begin(judged.name, "%s", judged.title)
		verdict, err := judged.verdict(probe, egress)
		if err != nil {
			return fmt.Errorf("%s: %w; the reply: %q", p.line, err, excerpt(reply, 1200))
		}
		note("%s", verdict)
	}

	p.stages.begin("plugin skills", "The agent on %s has the plugin skills selected and no other: %v", harness, p.pluginSkillsKnown())
	listing, err := p.provePluginSkills(ctx, session, true, 0, p.rt.claude)
	if err != nil {
		return err
	}
	if p.rt.claude {
		p.stages.begin("plugin hook", "The Claude-format package loaded whole: its SessionStart hook added its context to the Session")
		verdict, err := judgeClaudePluginHook(listing)
		if err != nil {
			return fmt.Errorf("on Harness %s: %w (%s)", harness, err, p.line)
		}
		note("%s", verdict)
		p.stages.begin("harness change refused", "%supdate_agent moving %s, which binds the Claude-format package, to Harness codex refused as Claude-format: the HelmRelease not written, the Agent still on %s", agentManagerToolPrefix, codingTestAgent, harness)
		if !codex {
			p.stages.skip("the lab renders no Harness codex: add it to platform.coding.harnesses in %s, then `agentlab platform`", config.File)
		} else if err := p.proveHarnessChangeRefused(); err != nil {
			return err
		}
		p.stages.begin("harness change to kagent refused", "%supdate_agent moving %s, which binds the Claude-format package, to Harness %s refused as Claude-format", agentManagerToolPrefix, codingTestAgent, kagentHarness)
		p.stages.skip("agent-manager runs its plugin check on the kagent Harness from %s, not released on this line (%s): the move is not attempted, since it would write a revision that never boots", kagentPluginCheck, p.line)
	}

	p.stages.begin("mcp call", "The agent calls %s through its MCP binding and finds itself listed", codingTestReadTool)
	mcpTurn, err := p.turn(ctx, session, codingMCPCallPrompt())
	if err != nil {
		return err
	}
	reply = mcpTurn.reply
	called := firstNonEmpty(mcpTurn.probe["mcp-call"], mcpTurn.toolOutput(codingTestReadTool), mcpTurn.toolOutput(musterCallTool))
	switch {
	case strings.Contains(called, codingTestAgent):
		note("%s listed: %s", codingTestReadTool, excerpt(called, 240))
	case p.noMCPCredential != "":
		// The binding carries no credential on this line: muster answers
		// the runtime's MCP connection 401, and the runtime has none of the
		// binding's tools to call.
		p.stages.skip("%s through the agent's binding answered %q: the binding carries no credential on this line, so muster refuses the runtime's MCP connection (%s)", codingTestReadTool, excerpt(called, 200), p.noMCPCredential)
	default:
		return fmt.Errorf("%s through the agent's binding on Harness %s answered %q, wanted a list naming %s; the reply: %q (%s)", codingTestReadTool, harness, called, codingTestAgent, excerpt(reply, 400), p.line)
	}

	p.stages.begin("turn gap", "A heartbeat the turn starts in the sandbox does not run in the %s without a turn and runs again in the next", codingTurnGap)
	if err := p.proveTurnGap(ctx, session); err != nil {
		return err
	}

	if p.rt.claude {
		p.stages.begin("instruction fixture", "A repository's instruction sources written into the Session's directory, the next turn records what Claude Code loaded")
		if _, err := p.turn(ctx, session, codingFixturePrompt()); err != nil {
			return err
		}
		read, err := p.turn(ctx, session, codingFixtureReadPrompt())
		if err != nil {
			return err
		}
		loaded, err := codingFixtureRecord(read.probe)
		if err != nil {
			return err
		}
		note("on %s: %s", p.line, loaded)
	} else if _, err := p.turn(ctx, session, codingCodeWordPrompt(p.rt)); err != nil {
		return err
	}

	p.stages.begin("suspend and resume", "The Session suspended to the snapshot store and resumed: the first turn's file and code word are still there")
	meta, err := p.suspendResume(ctx, session)
	if err != nil {
		return err
	}

	p.stages.begin("usage", "The resumed turn's task status carries its usage")
	usage, _ := meta[usageMetadataKey].(map[string]any)
	if len(usage) == 0 {
		return fmt.Errorf("the turn on Harness %s completed without %s (status metadata %v; %s)", harness, usageMetadataKey, meta, p.line)
	}
	note("usage %v", usage)

	p.stages.begin("share refused", "A read-write share of the %s Session refused: a collaborator's turn would run under what the first sender's turns left", harness)
	if err := p.shareRefused(ctx, session); err != nil {
		return err
	}

	p.stages.begin("empty selection", "%supdate_agent with %s's skill selection empty: the agent Ready on the new revision, a new Session's agent has none of its skills", agentManagerToolPrefix, pluginExampleURL)
	if err := p.proveEmptySelection(ctx); err != nil {
		return err
	}
	if p.mock == nil {
		return nil
	}

	p.stages.begin("model", "The %s turns ran on the in-cluster mockllm: every model call answered by its script", harness)
	return p.mockAnswered()
}

// connect signs the person in to muster and kagent and reads the line.
func (p *codingProof) connect(ctx context.Context) error {
	token, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, p.user.Email, p.user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	if p.muster, err = openMusterSession(p.cfg, token, "coding-test"); err != nil {
		return err
	}
	if p.api, err = dialKagentAPI(p.cfg, token); err != nil {
		return err
	}
	p.sessions = &sessionSet{api: p.api}
	p.line = p.releases(ctx)
	note("the line: %s", p.line)
	return nil
}

// model is the ModelConfig the agent runs on, placing the mockllm's where
// the agent runs on it, and sets the turn budget by it.
func (p *codingProof) model() (string, error) {
	modelConfig := p.modelConfig
	if p.mockBinary != "" {
		if err := ensureMockLLMModelConfig(); err != nil {
			return "", err
		}
		modelConfig = MockLLMModelConfig
		note("the model: the in-cluster mockllm (ModelConfig %s, %s at %s), scripted for this proof's turns; --model-config runs a real model", MockLLMModelConfig, mockLLMModel, mockLLMURL())
	}
	if modelConfig == "" {
		var err error
		if modelConfig, err = agentManagerModelConfig(p.muster); err != nil {
			return "", err
		}
	}
	p.setTurnBudget(modelConfig)
	return modelConfig, nil
}

// startMock runs the mockllm for the agent's MCP binding, whose server the
// script's MCP call names.
func (p *codingProof) startMock() error {
	t, err := readAgentTemplate(codingTestAgent)
	if err != nil {
		return err
	}
	if p.mock, err = startMockLLM(p.cfg, p.mockBinary, t.mcpServer(), []string{pluginExampleSkill}); err != nil {
		return err
	}
	note("the mockllm serves %s for the MCP tools of %s", mockLLMURL(), t.mcpServer())
	return nil
}

// mockAnswered is the mockllm's count of the model calls: some answered,
// none outside its script.
func (p *codingProof) mockAnswered() error {
	counts, err := p.mock.requests()
	if err != nil {
		return fmt.Errorf("reading the mockllm's requests: %w", err)
	}
	if counts.Answered == 0 || counts.Unmatched > 0 {
		return fmt.Errorf("the mockllm answered %d model calls of the %s agent and matched no script entry for %d: the runtime asked what the script does not answer (`docker logs %s-%s`)", counts.Answered, p.rt.name, counts.Unmatched, p.cfg.ClusterName, mockLLMContainerSuffix)
	}
	note("the mockllm answered all %d model calls of the %s turns (%s, %s)", counts.Answered, p.rt.name, mockLLMModel, mockLLMURL())
	return nil
}

// judgedStage is one verdict on the sandbox turn's readings.
type judgedStage struct {
	name, title string
	verdict     func(codingProbe, []string) (string, error)
}

// judgedStages are the verdicts on the sandbox turn for the runtime: Claude
// Code's uid split, forwarder and write tool through it on claude alone.
func (p *codingProof) judgedStages() []judgedStage {
	var stages []judgedStage
	if p.rt.claude {
		stages = append(stages,
			judgedStage{"uid split", "Claude Code as uid " + claudeUnprivilegedUID + " cannot read the root harness's environment or memory", codingProbe.uidSplit},
			judgedStage{"forwarder paths", "The caller forwarder refuses dot segments and encoded slashes before it matches a server", codingProbe.forwarderPaths},
			judgedStage{"write tool refused", "A write tool outside the toolset, called through the forwarder with the turn's bearer, refused by muster", codingProbe.writeToolRefused},
		)
	}
	return append(stages,
		judgedStage{"credentials", "No forge token or bearer in the environment or /data; the model key and the MCP credentials placeholders", codingProbe.credentials},
		judgedStage{"dns canary", "A lookup of " + codingTestDNSCanary + ", outside the allowlist, recorded", codingProbe.dnsCanary},
		judgedStage{"egress", "A host outside the Agent's origins denied at the egress gate, an origin of its list reached", codingProbe.egress},
	)
}

// proveSecretBacked is the agent's binding on upstream's Secret-backed path:
// its RemoteMCPServer takes at least one header from a Secret, and every
// Secret key it names is there. A binding with no header from a Secret is a
// platform line that ships no Secret-backed credential: the stage is
// skipped, naming the headers it has. Names only; the values stay in the
// Secret.
func (p *codingProof) proveSecretBacked() error {
	t, err := readAgentTemplate(codingTestAgent)
	if err != nil {
		return err
	}
	rms, err := readKagentObject(remoteMCPServerResource, t.mcpServer())
	if err != nil {
		return fmt.Errorf("the bound %s %s: %w", remoteMCPServerKind, t.mcpServer(), err)
	}
	skip, err := judgeSecretBacked(rms, secretHasKey, p.line)
	if err != nil {
		return err
	}
	if skip != "" {
		p.stages.skip("%s", skip)
		p.noMCPCredential = skip
	}
	return nil
}

// judgeSecretBacked is the secret-backed credential stage's verdict on the
// bound RemoteMCPServer: why the stage is skipped when no header comes from
// a Secret, an error when a Secret key a header names is missing, neither
// when every one is there. hasKey reports whether a Secret carries a key,
// never its value.
func judgeSecretBacked(rms *unstructured.Unstructured, hasKey func(ns, name, key string) bool, line string) (string, error) {
	sources := secretBackedHeaders(rms)
	if len(sources) == 0 {
		return fmt.Sprintf("%s %s takes headers %v, none from a Secret: the platform line (%s) ships no Secret-backed MCP credential", remoteMCPServerKind, rms.GetName(), headerNames(rms), line), nil
	}
	var headers []string
	for _, s := range sources {
		if !hasKey(rms.GetNamespace(), s.secret, s.key) {
			return "", fmt.Errorf("%s %s takes header %s from Secret %s/%s key %s, which is missing: its MCP calls carry no credential (%s)", remoteMCPServerKind, rms.GetName(), s.header, rms.GetNamespace(), s.secret, s.key, line)
		}
		headers = append(headers, s.header)
	}
	note("binds %s/%s with %v from a Secret, every key there", remoteMCPServerKind, rms.GetName(), headers)
	return "", nil
}

// releases words the line under test: the chart and the versions the
// controller and agent-manager report; it sets anyToolset from the latter.
func (p *codingProof) releases(ctx context.Context) string {
	parts := []string{"agent-platform " + firstNonEmpty(p.cfg.Platform.ChartVersion, p.cfg.Platform.ChartPath, "unread")}
	if v, err := p.api.version(ctx); err == nil {
		parts = append(parts, "kagent "+v.GetKagentVersion())
	}
	if info, err := readAgentManagerInfo(p.muster); err == nil {
		parts = append(parts, "agent-manager "+info.Version)
		p.anyToolset = takesAnyToolset(info.Version)
	}
	return strings.Join(parts, ", ")
}

// takesAnyToolset reports whether an agent-manager version writes a coding
// agent with any muster toolset: a release at or above
// agentManagerAnyToolsetFloor, or a build whose version is no release (a dev
// build is of the current line).
func takesAnyToolset(version string) bool {
	v, err := semver.NewVersion(version)
	if err != nil {
		return true
	}
	return !v.LessThan(semver.MustParse(agentManagerAnyToolsetFloor))
}

// toolset is the coding agent's toolset on this line: the read-only preset
// plus a muster write tool, the read-only preset alone below the floor.
func (p *codingProof) toolset() []string {
	if p.anyToolset {
		return []string{codingTestToolset, codingTestGrantedTool}
	}
	return []string{codingTestToolset}
}

// proveGuards is validate_agent on a coding agent: the toolset with
// a muster write tool accepted (refused below the floor), an origin outside
// the Harness's list refused.
func (p *codingProof) proveGuards(spec agentSpec) error {
	writeToolset := []string{codingTestToolset, codingTestGrantedTool}
	report, err := p.validate(spec, func(args map[string]any) { args[toolsetKey] = writeToolset })
	if err != nil {
		return err
	}
	switch {
	case p.anyToolset && !report.Valid:
		return fmt.Errorf("toolset %v on %s: validate_agent answered valid=false errors=%v, wanted it accepted", writeToolset, p.line, report.Errors)
	case p.anyToolset:
		note("toolset %v accepted", writeToolset)
	case !report.refused("read-only"):
		return fmt.Errorf("toolset %v on %s: validate_agent answered valid=%v errors=%v, wanted the read-only refusal of an agent-manager below %s", writeToolset, p.line, report.Valid, report.Errors, agentManagerAnyToolsetFloor)
	default:
		note("toolset %v refused below %s: %s", writeToolset, agentManagerAnyToolsetFloor, excerpt(strings.Join(report.Errors, "; "), 240))
	}

	origin := "egress [" + codingTestRefusedOrigin + "]"
	report, err = p.validate(spec, func(args map[string]any) {
		args[toolsetKey] = []string{codingTestToolset}
		args[egressKey] = []string{codingTestRefusedOrigin}
	})
	if err != nil {
		return err
	}
	if !report.refused("is not one of Harness") {
		return fmt.Errorf("%s on %s: validate_agent answered valid=%v errors=%v, wanted the refusal %q", origin, p.line, report.Valid, report.Errors, "is not one of Harness")
	}
	note("%s refused: %s", origin, excerpt(strings.Join(report.Errors, "; "), 240))
	return nil
}

// validate is validate_agent on the spec's create arguments as mutate
// changes them.
func (p *codingProof) validate(spec agentSpec, mutate func(args map[string]any)) (validateReport, error) {
	var report validateReport
	args, err := createAgentArgs(spec)
	if err != nil {
		return report, err
	}
	mutate(args)
	err = p.muster.callServerJSON(agentManagerToolPrefix+"validate_agent", args, &report)
	return report, err
}

// refused reports whether validate_agent refused with an error containing want.
func (r validateReport) refused(want string) bool {
	return !r.Valid && slices.ContainsFunc(r.Errors, func(e string) bool { return strings.Contains(e, want) })
}

// createReady is create_agent through muster as the person, then the
// Agent Ready on its Harness; it returns the Agent's egress.
func (p *codingProof) createReady(spec agentSpec) ([]string, error) {
	written, err := (agentManagerWriter{p.muster}).createAgent(spec)
	if err != nil {
		return nil, err
	}
	if written.RequestedBy != p.user.Email {
		return nil, fmt.Errorf("create_agent carries requestedBy=%q, wanted %q", written.RequestedBy, p.user.Email)
	}
	boot, err := waitAgentReadyOn(spec.Name, spec.Harness, p.timeout)
	if err != nil {
		return nil, err
	}
	if !boot.ready {
		return nil, fmt.Errorf("agent %s is not Ready on Harness %s after %s: %s", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), boot.reason)
	}
	note("%s Ready on %s in %s (requestedBy %s)", spec.Name, spec.Harness, boot.elapsed.Round(time.Second), written.RequestedBy)
	return readAgentEgress(context.Background(), spec.Name)
}

// proveBinding is the agent's MCP binding: the RemoteMCPServer its template
// binds Accepted, sending the toolset as declared.
func (p *codingProof) proveBinding(toolset []string) error {
	t, err := readAgentTemplate(codingTestAgent)
	if err != nil {
		return err
	}
	header, found, err := toolsetHeaderOf(t)
	if err != nil {
		return fmt.Errorf("agent %s: %w", codingTestAgent, err)
	}
	if want := strings.Join(toolset, ","); !found || header != want {
		return fmt.Errorf("agent %s binds %s %s with %s=%q (present %v), wanted %q", codingTestAgent, remoteMCPServerKind, t.mcpServer(), toolsetHeader, header, found, want)
	}
	var accepted string
	waitFor(int(p.timeout/pollInterval), pollInterval, func() bool {
		rms, err := readKagentObject(remoteMCPServerResource, t.mcpServer())
		accepted = ""
		if err == nil {
			accepted = conditionStatus(rms, conditionAccepted)
		}
		return accepted == conditionTrue
	})
	if accepted != conditionTrue {
		return fmt.Errorf("%s %s of agent %s: Accepted=%q after %s, wanted True (%s)", remoteMCPServerKind, t.mcpServer(), codingTestAgent, accepted, p.timeout, p.line)
	}
	note("binds %s/%s Accepted, %s=%s", remoteMCPServerKind, t.mcpServer(), toolsetHeader, header)
	return nil
}

// createSession is a Session of the agent as the person, tracked for
// removal.
func (p *codingProof) createSession(ctx context.Context, agent string) (*apiv1alpha1.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	session, err := p.api.createSession(ctx, agent, uuid.NewString())
	if session != nil && session.GetId() != "" {
		p.sessions.add(session.GetId())
	}
	return session, err
}

// codingTurn is what one completed turn of the proof answered: the reply's
// text, the marker lines of the reply and its tools' outputs, the tools'
// results and the status metadata.
type codingTurn struct {
	reply string
	probe codingProbe
	tools []harnessToolResult
	meta  map[string]any
}

// toolOutput is the output of the turn's call of tool, an MCP tool on any
// server (codex names one <server>.<tool>), "" when it called none.
func (t codingTurn) toolOutput(tool string) string {
	for _, result := range t.tools {
		if result.name == tool || strings.HasSuffix(result.name, "."+tool) {
			return result.output
		}
	}
	return ""
}

// turn sends one message and waits for the task to settle completed.
func (p *codingProof) turn(ctx context.Context, session *apiv1alpha1.Session, prompt string) (codingTurn, error) {
	note("turn on Session %s, deadline %s (%s)", session.GetId(), p.turnTimeout, p.turnBudget)
	ctx, cancel := context.WithTimeout(ctx, p.turnTimeout)
	defer cancel()
	start := time.Now()
	t, err := p.api.turn(ctx, session, userMessage(prompt))
	if err != nil {
		return codingTurn{}, fmt.Errorf("the turn on Session %s after %s of its %s deadline (%s): %w", session.GetId(), time.Since(start).Round(time.Second), p.turnTimeout, p.turnBudget, err)
	}
	reply, err := t.completedText()
	if err != nil {
		return codingTurn{}, fmt.Errorf("the turn on Session %s: %w", session.GetId(), err)
	}
	return codingTurn{reply: reply, probe: parseTurnProbe(reply, t.toolOutputs()), tools: t.tools, meta: t.statusMeta}, nil
}

// suspendResume suspends the Session, resumes it and has a turn read the
// first turn's file and recall its code word; it returns that turn's status
// metadata.
func (p *codingProof) suspendResume(ctx context.Context, session *apiv1alpha1.Session) (map[string]any, error) {
	opCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	suspended, err := p.api.suspendSession(opCtx, session.GetId())
	if err != nil {
		return nil, err
	}
	resumed, err := p.api.resumeSession(opCtx, session.GetId())
	if err != nil {
		return nil, err
	}
	note("Session %s: %s, then %s", session.GetId(), sessionState(suspended), sessionState(resumed))
	read, err := p.turn(ctx, resumed, codingResumePrompt(p.rt))
	if err != nil {
		return nil, err
	}
	probe := read.probe
	if probe["file"] != codingTestCodeWord || !strings.Contains(probe["code-word"], codingTestCodeWord) {
		return nil, fmt.Errorf("after the resume the first turn's file reads %q and the code word recalled is %q, wanted %s for both (%s)", probe["file"], probe["code-word"], codingTestCodeWord, p.line)
	}
	recalled := "the code word recalled from the conversation"
	if p.mock != nil {
		recalled = "the code word the mockllm's script recalls"
	}
	note("the file %s reads %s, %s", codingTestOwnFile, probe["file"], recalled)
	return read.meta, nil
}

// shareRefused asks kagent for a read-write share of the coding Session;
// one created is revoked and is the failure.
func (p *codingProof) shareRefused(ctx context.Context, session *apiv1alpha1.Session) error {
	ctx, cancel := context.WithTimeout(ctx, kubeReadTimeout)
	defer cancel()
	resp, err := p.api.sessions.CreateSessionShare(p.api.callCtx(ctx), &apiv1alpha1.CreateSessionShareRequest{
		SessionId:  session.GetId(),
		Permission: apiv1alpha1.SessionSharePermission_SESSION_SHARE_PERMISSION_READ_WRITE,
	})
	if err != nil {
		note("refused: %s: %s", status.Code(err), status.Convert(err).Message())
		return nil
	}
	shareID := resp.GetShare().GetId()
	if _, rerr := p.api.sessions.RevokeSessionShare(p.api.callCtx(ctx), &apiv1alpha1.RevokeSessionShareRequest{ShareId: shareID}); rerr != nil {
		note("revoking share %s: %v", shareID, rerr)
	}
	return fmt.Errorf("%s created share %s of Session %s on Harness %s (revoked): the refusal of shares on a Harness that runs code is not on this line", p.line, shareID, session.GetId(), p.rt.name)
}

// removeAgents removes the proof's agents, a leftover of an earlier run too.
func (p *codingProof) removeAgents() {
	for _, name := range []string{codingTestAgent, codingTestPluginAgent} {
		if !agentExists(name) {
			continue
		}
		if err := removeAgent(name); err != nil {
			note("cleanup: %v", err)
		}
	}
}

func (p *codingProof) cleanup() {
	if p.sessions != nil {
		p.sessions.removeAll()
	}
	p.removeAgents()
	if p.mockBinary != "" || p.mockModelPlaced {
		p.mock.close()
		removeMockLLMModelConfig()
	}
	if p.api != nil {
		p.api.close()
	}
}

// usageMetadataKey is the status metadata a turn's usage is under.
const usageMetadataKey = "kagent.dev/a2a/usage"

// readHarnessLimits reads a claude Harness's spec.claude.limits, nil where
// it carries none.
func readHarnessLimits(ctx context.Context, name string) (any, error) {
	gvr, err := gvrFor(harnessResource)
	if err != nil {
		return nil, err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return nil, err
	}
	limits, _, _ := unstructured.NestedFieldNoCopy(obj.Object, harnessLimitsPath...)
	return limits, nil
}

// setTurnBudget sets the turn deadline: --turn-timeout where given, else the
// budget of the model the ModelConfig names, the host model server's
// longer one where the lab machine serves it. A ModelConfig that cannot be
// read gets the hosted budget, said so.
func (p *codingProof) setTurnBudget(modelConfig string) {
	if p.turnTimeout > 0 {
		p.turnBudget = "--turn-timeout"
		return
	}
	mc, err := readModelConfig(modelConfig)
	if err != nil {
		p.turnTimeout, p.turnBudget = hostedModelTurnTimeout, fmt.Sprintf("ModelConfig %s unread, the hosted model budget: %v", modelConfig, err)
		return
	}
	if server, ok := hostModelServer(mc); ok {
		p.turnTimeout, p.turnBudget = hostModelTurnTimeout, fmt.Sprintf("ModelConfig %s: %s on the host model server %s", modelConfig, modelOf(mc), server)
		return
	}
	p.turnTimeout, p.turnBudget = hostedModelTurnTimeout, fmt.Sprintf("ModelConfig %s: %s, a hosted model", modelConfig, modelOf(mc))
}

// hostModelServer names the host model server a ModelConfig's model runs
// on: model-manager's backend (every backend of the lab is one), else the
// lab host the ModelConfig's endpoint names (an extraModels entry on a host
// server). ok is false for a hosted model.
func hostModelServer(mc *unstructured.Unstructured) (string, bool) {
	if backend := mc.GetLabels()[modelManagerBackendLabel]; backend != "" {
		return backend, true
	}
	spec, _, _ := unstructured.NestedMap(mc.Object, "spec")
	for _, provider := range spec {
		fields, ok := provider.(map[string]any)
		if !ok {
			continue
		}
		for _, value := range fields {
			if endpoint, ok := value.(string); ok && strings.Contains(endpoint, "://"+labHostName) {
				return endpoint, true
			}
		}
	}
	return "", false
}

// modelOf is a ModelConfig's spec.model.
func modelOf(mc *unstructured.Unstructured) string {
	model, _, _ := unstructured.NestedString(mc.Object, "spec", "model")
	return firstNonEmpty(model, "a model")
}

// readAgentEgress reads the Agent's spec.egress.
func readAgentEgress(ctx context.Context, name string) ([]string, error) {
	gvr, err := gvrFor(agentResource)
	if err != nil {
		return nil, err
	}
	obj, err := getObject(ctx, gvr, kagentNamespace, name)
	if err != nil {
		return nil, err
	}
	egress, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "egress")
	return egress, err
}
