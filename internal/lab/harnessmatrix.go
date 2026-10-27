package lab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/agentlab/internal/config"
)

// The harness matrix records what each Harness does with a Shared subagent
// binding and with a Claude-format package's subagents and commands. Every
// cell is an observation, not a pass or fail against an expectation: a
// refusal the controller reports is an outcome like an answer. A child's
// answer counts only through a codeword that lives in the child's own system
// prompt, which the parent cannot know without invoking it. On codex without
// --codex-model-config the shared-prompt cell's turn runs on the mockllm,
// whose script delegates the way a model would (mockLLMDelegation): the
// codeword still reaches the reply only through the child's own request.

const (
	harnessMatrixPrefix = "agentlab-hm"
	harnessMatrixReport = "harness-matrix.txt"
	// featureDevPath is a Claude-format package with agents/ and commands/
	// (no hooks), at claudePluginCommit.
	featureDevPath     = "plugins/feature-dev"
	featureDevSubagent = "code-explorer"
	// harnessMatrixMCPServer is the RemoteMCPServer a child binds: kagent's
	// own, rendered by the chart and Accepted without a credential.
	harnessMatrixMCPServer = "kagent-api"
	// matrixDelegatePrompt opens a Shared case's prompt, the child's name
	// quoted after it; the mockllm's script reads the name there.
	matrixDelegatePrompt = "Delegate to your subagent "
	// matrixNone is a reply field's value when there is nothing to name.
	matrixNone = "none"
	// claudeAgentTool and claudeTaskTool are the names Claude Code's
	// subagent tool has gone by.
	claudeAgentTool = "Agent"
	claudeTaskTool  = "Task"
)

// The matrix's case keys.
const (
	caseSharedPrompt = "shared-prompt"
	caseSharedSkill  = "shared-skill"
	caseSharedMCP    = "shared-mcp"
	caseSharedNested = "shared-nested"
	casePluginParts  = "plugin-parts"
)

// HarnessMatrixOptions are `agentlab harness-matrix-test`'s.
type HarnessMatrixOptions struct {
	// Harnesses are the Harnesses probed; empty is kagent, claude, codex and
	// omp.
	Harnesses []string
	// CodexModelConfig is the ModelConfig of codex's cells; empty boots
	// them on the mockllm's, where only a mockTurn case takes its turn.
	CodexModelConfig string
	// MockLLMBinary is the agentlab binary the mockllm's container runs;
	// empty is this one.
	MockLLMBinary string
	// Cases are the cases run, by key; empty is all.
	Cases        []string
	ReadyTimeout time.Duration
}

// matrixCase is one row of the matrix.
type matrixCase struct {
	key   string
	title string
	// claudeOnly runs the case on claude alone.
	claudeOnly bool
	// mockTurn takes the case's turn on codex on the mockllm, whose script
	// answers it.
	mockTurn bool
}

// matrixCases are the rows, in order.
var matrixCases = []matrixCase{
	{key: caseSharedPrompt, title: "Shared child, prompt and model", mockTurn: true},
	{key: caseSharedSkill, title: "Shared child with a skill"},
	{key: caseSharedMCP, title: "Shared child with an MCP tool"},
	{key: caseSharedNested, title: "Shared child binding a Shared child"},
	{key: casePluginParts, title: "Claude-format package: subagents and commands", claudeOnly: true},
}

// matrixCell is one case on one Harness, and what was seen.
type matrixCell struct {
	c           matrixCase
	harness     string
	modelConfig string
	parent      string
	child       string
	grandchild  string
	codeword    string
	// noTurn boots the cell without a turn (codex on the mockllm, a case
	// its script does not answer).
	noTurn bool
	// mock is the running mockllm of a turn on it.
	mock *mockLLM

	outcome  string
	evidence string
}

// HarnessMatrixTest is `agentlab harness-matrix-test`.
func HarnessMatrixTest(cfg *config.Config, opts HarnessMatrixOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s: enable it and run `agentlab platform` first", config.File)
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultCodingReadyTimeout
	}
	harnesses := opts.Harnesses
	if len(harnesses) == 0 {
		harnesses = []string{kagentHarness, config.CodingRuntimeClaude, config.CodingRuntimeCodex, config.CodingRuntimeOmp}
	}
	for _, key := range opts.Cases {
		if !slices.ContainsFunc(matrixCases, func(c matrixCase) bool { return c.key == key }) {
			return fmt.Errorf("no case %q: the cases are %s", key, strings.Join(matrixCaseKeys(), ", "))
		}
	}
	cells := matrixCells(harnesses, opts)
	if slices.ContainsFunc(cells, func(c *matrixCell) bool { return c.modelConfig == MockLLMModelConfig }) {
		if err := ensureMockLLMModelConfig(); err != nil {
			return err
		}
		defer removeMockLLMModelConfig()
	}
	if slices.ContainsFunc(cells, (*matrixCell).onMock) {
		binary, err := mockLLMBinary(opts.MockLLMBinary)
		if err != nil {
			return err
		}
		mock, err := startMockLLM(cfg, binary, harnessMatrixMCPServer, "", nil)
		if err != nil {
			return err
		}
		defer mock.close()
		for _, cell := range cells {
			if cell.onMock() {
				cell.mock = mock
			}
		}
	}

	user := cfg.AdminUser()
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	api, err := dialKagentAPI(cfg, token)
	if err != nil {
		return err
	}
	defer api.close()

	var errs []error
	for _, cell := range cells {
		step("%s on %s", cell.c.title, cell.harness)
		if err := cell.run(api, opts.ReadyTimeout); err != nil {
			cell.outcome, cell.evidence = "ERROR", err.Error()
			errs = append(errs, fmt.Errorf("%s on %s: %w", cell.c.key, cell.harness, err))
		}
		note("%s: %s", cell.outcome, excerpt(cell.evidence, 300))
	}
	report := matrixReport(cells)
	fmt.Print("\n" + report)
	if err := os.MkdirAll(StateDir, 0o750); err == nil {
		path := filepath.Join(StateDir, harnessMatrixReport)
		if err := os.WriteFile(path, []byte(report), 0o600); err == nil {
			note("the matrix is in %s", path)
		}
	}
	return errors.Join(errs...)
}

func matrixCaseKeys() []string {
	keys := make([]string, 0, len(matrixCases))
	for _, c := range matrixCases {
		keys = append(keys, c.key)
	}
	return keys
}

// matrixCells are the cells of the cases selected on the Harnesses given.
func matrixCells(harnesses []string, opts HarnessMatrixOptions) []*matrixCell {
	var cells []*matrixCell
	for _, c := range matrixCases {
		if len(opts.Cases) > 0 && !slices.Contains(opts.Cases, c.key) {
			continue
		}
		for _, h := range harnesses {
			if c.claudeOnly && h != config.CodingRuntimeClaude {
				continue
			}
			parent := fmt.Sprintf("%s-%s-%s", harnessMatrixPrefix, h, strings.TrimPrefix(c.key, "shared-"))
			cell := &matrixCell{c: c, harness: h, modelConfig: defaultModelConfig, parent: parent, child: parent + "-c", grandchild: parent + "-g", codeword: "hm-" + randHex(4)}
			if h == config.CodingRuntimeCodex {
				cell.modelConfig = opts.CodexModelConfig
				if cell.modelConfig == "" {
					cell.modelConfig, cell.noTurn = MockLLMModelConfig, !c.mockTurn
				}
			}
			cells = append(cells, cell)
		}
	}
	return cells
}

// onMock is whether the cell takes its turn on the mockllm.
func (m *matrixCell) onMock() bool {
	return m.modelConfig == MockLLMModelConfig && !m.noTurn
}

// run applies the cell's templates, waits for the parent and, Ready, has
// one turn with it. The cell's objects are removed on every path.
func (m *matrixCell) run(api *kagentAPI, timeout time.Duration) error {
	defer m.cleanup(api)
	if _, err := applyManifests(context.Background(), []byte(m.manifests())); err != nil {
		m.outcome = "REFUSED"
		m.evidence = "at apply: " + err.Error()
		return nil
	}
	boot, err := waitAgentReadyOn(m.parent, m.harness, timeout)
	if err != nil {
		return err
	}
	switch {
	case boot.terminal:
		m.outcome, m.evidence = "REFUSED", boot.reason
		return nil
	case !boot.ready:
		m.outcome, m.evidence = "NOT READY", fmt.Sprintf("after %s: %s", timeout, boot.reason)
		if boot.agent != nil {
			_, message := boot.agent.Status.condition(conditionReady)
			m.evidence += "; Ready: " + message
		}
		return nil
	case m.noTurn:
		m.outcome, m.evidence = "READY", "booted on "+m.modelConfig+"; no turn (--codex-model-config names a real model)"
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*kagentTurnTimeout+sessionReadyTimeout)
	defer cancel()
	session, err := api.createSession(ctx, m.parent, uuid.NewString())
	if err != nil {
		return err
	}
	defer api.removeSession(session.GetId())
	t, err := api.turn(ctx, session, userMessage(m.prompt()))
	if err != nil {
		return err
	}
	reply, err := t.completedText()
	if err != nil {
		m.outcome, m.evidence = "TURN FAILED", err.Error()
		m.noteMockRequests()
		return nil
	}
	m.outcome, m.evidence = m.judge(reply, turnToolNames(t))
	if strings.Contains(m.outcome, "NOT INVOKED") {
		m.evidence += "; tool outputs: " + excerpt(t.toolOutputs(), 400)
		m.noteMockRequests()
	}
	return nil
}

// noteMockRequests prints the mockllm's last requests of a turn on it.
func (m *matrixCell) noteMockRequests() {
	if m.mock != nil {
		note("the mockllm's requests, the tools and the last input item of each:\n%s", m.mock.logTail())
	}
}

func (m *matrixCell) cleanup(api *kagentAPI) {
	for _, left := range proofAgentsCleanup(api, m.parent) {
		note("cleanup: still in Substrate: %s", left)
	}
	for _, name := range []string{m.child, m.grandchild} {
		if agentExists(name) {
			if err := removeAgent(name); err != nil {
				note("cleanup: %v", err)
			}
		}
	}
}

func turnToolNames(t *turn) []string {
	names := make([]string, 0, len(t.tools))
	for _, tool := range t.tools {
		names = append(names, tool.name)
	}
	return names
}

var matrixLine = regexp.MustCompile(`(?m)^\s*\x60?(CODEWORD|SKILLS|TOOL|COMMANDS|SUBAGENTS|EXPLORED)=([^\n\x60]*)`)

// replyFields are the KEY=value lines of a reply.
func replyFields(reply string) map[string]string {
	fields := map[string]string{}
	for _, m := range matrixLine.FindAllStringSubmatch(reply, -1) {
		fields[m[1]] = strings.TrimSpace(m[2])
	}
	return fields
}

// judge words what the turn showed.
func (m *matrixCell) judge(reply string, tools []string) (string, string) {
	fields := replyFields(reply)
	toolList := "tools run: " + cmpJoin(tools)
	if m.c.key == casePluginParts {
		invoked := slices.ContainsFunc(tools, func(n string) bool { return n == claudeAgentTool || n == claudeTaskTool })
		outcome := "SUBAGENT NOT INVOKED"
		if invoked {
			outcome = "SUBAGENT INVOKED"
		}
		return outcome, fmt.Sprintf("COMMANDS=%s; SUBAGENTS=%s; EXPLORED=%s; %s", fields["COMMANDS"], fields["SUBAGENTS"], excerpt(fields["EXPLORED"], 80), toolList)
	}
	evidence := fmt.Sprintf("CODEWORD=%s", fields["CODEWORD"])
	switch m.c.key {
	case caseSharedSkill:
		evidence += "; SKILLS=" + fields["SKILLS"]
	case caseSharedMCP:
		evidence += "; TOOL=" + fields["TOOL"]
	}
	evidence += "; " + toolList
	if m.onMock() {
		evidence += "; proven on the mockllm (" + MockLLMModelConfig + "), no model: its script spawns the child and answers the child's request with the codeword that request's own developer instructions carry"
	}
	if strings.Contains(reply, m.codeword) {
		return "CHILD INVOKED", evidence
	}
	return "CHILD NOT INVOKED", evidence + "; reply: " + excerpt(reply, 200)
}

func cmpJoin(s []string) string {
	if len(s) == 0 {
		return matrixNone
	}
	return strings.Join(s, ",")
}

// prompt is the cell's one turn.
func (m *matrixCell) prompt() string {
	if m.c.key == casePluginParts {
		return fmt.Sprintf("Answer in exactly three lines:\n"+
			"COMMANDS=<the slash commands and skills of the feature-dev plugin you can invoke (names starting feature-dev:), comma-separated, or none>\n"+
			"SUBAGENTS=<the subagent types available to you besides the built-in ones, comma-separated, or none>\n"+
			"EXPLORED=<the first line of the answer of the %s subagent, which you must invoke with the task: name the top-level directories of the current working directory; or not available>", featureDevSubagent)
	}
	extra, lines := "", ""
	switch m.c.key {
	case caseSharedSkill:
		extra, lines = " and the names of the skills it has", "\nSKILLS=<the skill names it answered>"
	case caseSharedMCP:
		extra, lines = " and to call one of its MCP tools that only lists something", "\nTOOL=<the name of the tool it called, or none>"
	}
	return fmt.Sprintf(matrixDelegatePrompt+"%q: ask it for its codeword%s. Then answer in exactly these lines:\nCODEWORD=<what it answered>%s\n"+
		"If you have no subagent of that name, answer CODEWORD=none and name the subagents and tools you do have.", m.child, extra, lines)
}

// manifests are the cell's objects: its children's AgentTemplates, then
// the parent's with its Agent on the cell's Harness.
func (m *matrixCell) manifests() string {
	var docs []string
	var parentTools, parentPlugins string
	switch m.c.key {
	case casePluginParts:
		parentPlugins = fmt.Sprintf("  plugins:\n  - source:\n      git:\n        url: %s\n        commit: %s\n      path: %s\n", claudePluginURL, claudePluginCommit, featureDevPath)
	default:
		var childTools, childSkills string
		switch m.c.key {
		case caseSharedSkill:
			childSkills = fmt.Sprintf("  skills:\n  - name: %s\n    source:\n      git:\n        url: %s\n        commit: %s\n      path: skills/%s\n", pluginExampleSkill, pluginExampleURL, pluginExampleCommit, pluginExampleSkill)
		case caseSharedMCP:
			childTools = fmt.Sprintf("  tools:\n  - mcp:\n      server:\n        kind: RemoteMCPServer\n        name: %s\n", harnessMatrixMCPServer)
		case caseSharedNested:
			childTools = subAgentTools(m.grandchild)
			docs = append(docs, matrixTemplate(m.grandchild, m.modelConfig, "You are a nested subagent of a test lab. Answer: grandchild.", "", ""))
		}
		docs = append(docs, matrixTemplate(m.child, m.modelConfig, fmt.Sprintf("You are the subagent %s of a test lab. Whenever you get control, whether as a delegated task or as the conversation transferred to you, your answer starts with the line CODEWORD=%s. When asked for your skills, add the line SKILLS=<the names of the skills available to you>. When asked to call a tool, call one of yours that only lists something and add the line TOOL=<its name>.", m.child, m.codeword), childTools, childSkills))
		parentTools = subAgentTools(m.child)
	}
	docs = append(docs, matrixTemplate(m.parent, m.modelConfig, "You are a probe in a test lab. Follow the user's instructions exactly and answer in the format asked.", parentTools, parentPlugins),
		matrixAgent(m.parent, m.harness))
	return strings.Join(docs, "---\n")
}

func subAgentTools(child string) string {
	return fmt.Sprintf("  tools:\n  - subAgent:\n      name: %s\n      description: %q\n      templateRef:\n        name: %s\n", child, "The test lab's subagent "+child+"; ask it for its codeword.", child)
}

// matrixTemplate is an AgentTemplate of the kagent namespace; extra is
// spec lines (tools, skills, plugins) appended as given.
func matrixTemplate(name, modelConfig, systemPrompt string, extra ...string) string {
	return fmt.Sprintf(`apiVersion: %s
kind: AgentTemplate
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
spec:
  description: "Throwaway agent of agentlab harness-matrix-test; deleted by the same run."
  modelConfig:
    name: %s
  systemPrompt: %q
%s`, agentTemplateAPIVersion, name, kagentNamespace, managedByLabel, managedByAgentlabValue, modelConfig, systemPrompt, strings.Join(extra, ""))
}

// matrixAgent pairs the parent with the Harness; its egress reaches GitHub
// for the skill and plugin sources fetched at boot.
func matrixAgent(name, harness string) string {
	return agentManifest(name, name, harness, nil) + "  egress:\n  - https://github.com\n  - https://codeload.github.com\n"
}

// matrixReport is the matrix as a table, one line per cell.
func matrixReport(cells []*matrixCell) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CASE\tHARNESS\tOUTCOME\tEVIDENCE")
	for _, c := range cells {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.c.key, c.harness, c.outcome, strings.ReplaceAll(excerpt(c.evidence, 400), "\n", " "))
	}
	_ = w.Flush()
	return b.String()
}
