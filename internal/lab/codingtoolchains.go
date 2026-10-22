package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// `agentlab coding-test --part toolchains [--harness claude|codex]
// [--toolchains go,github]`: toolchains mounted into a coding agent's
// Session, headless. agent-manager resolves each toolchain to its digest
// and passes it to kagent with the Session's creation; kagent mounts each
// image read-only at /opt/kagent/tools/<name>, its bin/ on the runtime's
// PATH.
//
//  1. The coding Harness Ready from the values.
//  2. agent-manager's get_info reports the capability toolchains: its
//     toolchains.repository is set (kagent's controller admitting it for
//     session tool images, on a kagent line that mounts them).
//  3. create_agent on the Harness as the person, Ready.
//  4. start_session as the person with the toolchains: the answer mounts
//     exactly those, each pinned by digest.
//  5. One turn: the mount lists exactly the toolchains, the agent's shell
//     finds each one's tool on its PATH under its mount and the tool answers
//     its version; with go, a fixture module builds and runs offline.
//  6. On codex without --model-config, every model call answered by the
//     in-cluster mockllm's script (mockllm.go).
//
// The agent and its Session are removed on every exit path.

// CodingPartToolchains is the toolchains part of coding-test.
const CodingPartToolchains = "toolchains"

// DefaultCodingToolchains are the toolchains the part mounts without
// --toolchains: go, whose fixture module is the build.
var DefaultCodingToolchains = []string{toolchainGo}

const (
	toolchainGo     = "go"
	toolchainGitHub = "github"
	// toolchainsMountRoot is where kagent mounts a Session's toolchains,
	// each under its name.
	toolchainsMountRoot = "/opt/kagent/tools"
	// toolchainsFixtureDir is the fixture's directory under the Session's
	// working directory.
	toolchainsFixtureDir = ".agentlab-toolchain-fixture"
	// toolchainsFixtureOutput is what the built fixture prints.
	toolchainsFixtureOutput = "agentlab-toolchain-fixture-built"
	// toolchainsPrompt starts the turn's prompt; the mockllm's toolchain
	// entry matches it.
	toolchainsPrompt = "Run this test lab's toolchain check"
	// startSessionToolchainsArg is start_session's list of toolchains,
	// name[@version] each.
	startSessionToolchainsArg = "toolchains"
	// agentManagerToolchainsCapability is get_info's capability of an
	// agent-manager that publishes toolchains.
	agentManagerToolchainsCapability = "toolchains"
	// The check's lines: the mount's listing, and per toolchain its tool's
	// path and version (<line>-<toolchain>); the fixture's build.
	toolchainsProbeTools  = "mounted"
	toolchainsProbePath   = "path"
	toolchainsProbeOnPath = "onpath"
	// toolchainsOnPath is the onpath line of a toolchain whose bin/ is on
	// the PATH.
	toolchainsOnPath       = "yes"
	toolchainsProbeVersion = "version"
	toolchainsProbeBuilt   = "built"
)

// toolchainTools are the tools the check asks for by toolchain, where the
// tool is not named like the toolchain.
var toolchainTools = map[string]string{toolchainGitHub: "gh", "gitlab": "glab", "python": "python3"}

// toolchainTool is the tool the check runs of a toolchain.
func toolchainTool(name string) string {
	if tool, ok := toolchainTools[name]; ok {
		return tool
	}
	return name
}

// toolchainName is a toolchain's name in a name[@version] entry.
var toolchainName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// toolchainNames are the names of name[@version] entries, refusing one
// that is not a toolchain name.
func toolchainNames(entries []string) ([]string, error) {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "@")
		if !toolchainName.MatchString(name) || slices.Contains(names, name) {
			return nil, fmt.Errorf("--toolchains %q: name[@version] entries of distinct toolchains", strings.Join(entries, ","))
		}
		names = append(names, name)
	}
	return names, nil
}

// toolchainsStarted is start_session's answer as the part reads it.
type toolchainsStarted struct {
	Session struct {
		ID string `json:"id"`
	} `json:"session"`
	Toolchains []startedToolchain `json:"toolchains"`
}

// startedToolchain is one toolchain start_session mounted: the version it
// was named with, the digest-pinned image and the origins it declares.
type startedToolchain struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Image   string   `json:"image"`
	Egress  []string `json:"egress"`
}

// runToolchains is the part on the proof's Harness and model.
func (p *codingProof) runToolchains() error {
	ctx := context.Background()
	harness := p.rt.name
	names, err := toolchainNames(p.toolchains)
	if err != nil {
		return err
	}
	p.stages.begin("toolchains: harness", "The coding Harness %s Ready from the platform values", harness)
	if err := waitHarnessReady(ctx, harness, p.timeout); err != nil {
		return err
	}
	if err := p.connect(ctx); err != nil {
		return err
	}

	p.stages.begin("toolchains: capability", "%sget_info reports the capability %s", agentManagerToolPrefix, agentManagerToolchainsCapability)
	info, err := readAgentManagerInfo(p.muster)
	if err != nil {
		return err
	}
	if !info.Capabilities[agentManagerToolchainsCapability] {
		return fmt.Errorf("agent-manager %s publishes no toolchains (capabilities %v): the part needs agent-manager.toolchains.repository and the kagent controller's KAGENT_SESSION_TOOL_IMAGE_REPOSITORIES admitting it and KAGENT_SESSION_TOOL_IMAGE_EGRESS_ORIGINS every origin its toolchains declare, on a kagent line that mounts session tool images (a platform.valuesFiles overlay; %s)", info.Version, info.Capabilities, p.line)
	}
	note("agent-manager %s publishes toolchains", info.Version)

	modelConfig, err := p.model()
	if err != nil {
		return err
	}
	p.removeAgents()
	spec := agentSpec{
		Name: codingTestAgent, ModelConfig: modelConfig, Harness: harness, SystemMessage: codingTestSystemPrompt,
		Toolset:     []string{codingTestToolset},
		Description: "Throwaway coding agent of `agentlab coding-test --part toolchains`; deleted by the same run.",
	}
	p.stages.begin("toolchains: agent", "%screate_agent %s on Harness %s as %s: Ready", agentManagerToolPrefix, codingTestAgent, harness, p.user.Email)
	if _, err := p.createReady(spec); err != nil {
		return err
	}
	if p.mockBinary != "" {
		if err := p.startMock(); err != nil {
			return err
		}
	}

	p.stages.begin("toolchains: start", "%sstart_session of %s as %s with the toolchains %s: exactly those mounted, each pinned by digest", agentManagerToolPrefix, codingTestAgent, p.user.Email, strings.Join(p.toolchains, ", "))
	id, err := p.startToolchainSession(names)
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	session, err := p.api.getSession(opCtx, id)
	if err != nil {
		return err
	}

	built := ""
	if slices.Contains(names, toolchainGo) {
		built = "; a fixture module builds offline"
	}
	p.stages.begin("toolchains: tools", "A turn whose %s finds %s on its PATH under %s, each answering its version%s", p.rt.shellTool, strings.Join(names, ", "), toolchainsMountRoot, built)
	turn, err := p.turn(ctx, session, toolchainsCheckPrompt(p.rt))
	if err != nil {
		return err
	}
	verdict, err := judgeToolchainCheck(turn.probe, names)
	if err != nil {
		return fmt.Errorf("on Harness %s: %w; the reply: %q (%s)", harness, err, excerpt(turn.reply, 1200), p.line)
	}
	note("%s", verdict)
	if p.mock == nil {
		return nil
	}

	p.stages.begin("toolchains: model", "The %s turns ran on the in-cluster mockllm: every model call answered by its script", harness)
	return p.mockAnswered()
}

// startToolchainSession starts a Session of the proof's agent as the person
// with the proof's toolchains, tracked for removal, and judges what
// start_session says it mounted.
func (p *codingProof) startToolchainSession(names []string) (string, error) {
	env, err := p.muster.callToolEnvelope(agentManagerToolPrefix+"start_session", map[string]any{
		nameKey: codingTestAgent, namespaceKey: kagentNamespace, startSessionRequestIDArg: uuid.NewString(),
		startSessionToolchainsArg: p.toolchains,
	})
	if err != nil {
		return "", err
	}
	if env.IsError {
		return "", fmt.Errorf("start_session of %s with the toolchains %s refused: %.400s (%s)", codingTestAgent, strings.Join(p.toolchains, ","), env.Content[0].Text, p.line)
	}
	var started toolchainsStarted
	if err := json.Unmarshal([]byte(env.Content[0].Text), &started); err != nil || started.Session.ID == "" {
		return "", fmt.Errorf("start_session answered no session: %.400s", env.Content[0].Text)
	}
	p.sessions.add(started.Session.ID)
	mounted, err := judgeStartedToolchains(started.Toolchains, names)
	if err != nil {
		return "", fmt.Errorf("the Session %s: %w", started.Session.ID, err)
	}
	note("Session %s: %s", started.Session.ID, mounted)
	return started.Session.ID, nil
}

// judgeStartedToolchains is the start's verdict: exactly the toolchains
// named, each image pinned by digest.
func judgeStartedToolchains(toolchains []startedToolchain, want []string) (string, error) {
	names := make([]string, 0, len(toolchains))
	var mounted []string
	for _, t := range toolchains {
		names = append(names, t.Name)
		if !strings.Contains(t.Image, "@sha256:") {
			return "", fmt.Errorf("start_session mounted %s as %q, wanted an image pinned by digest", t.Name, t.Image)
		}
		mounted = append(mounted, fmt.Sprintf("%s %s as %s, origins %v", t.Name, t.Version, t.Image, t.Egress))
	}
	if !sameNames(names, want) {
		return "", fmt.Errorf("start_session mounted the toolchains %v, wanted exactly %v", names, want)
	}
	return "mounted " + strings.Join(mounted, "; "), nil
}

// sameNames reports whether two lists hold the same names, in any order.
func sameNames(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// toolchainsCheckPrompt asks the agent to run the toolchain check once and
// relay its lines.
func toolchainsCheckPrompt(rt codingRuntime) string {
	return toolchainsPrompt + ": run this exact script once with your " + rt.shellTool + ", then reply with every output line that starts with " + probeMarker + ", verbatim and nothing else:\n\n" + toolchainsCheckCommand()
}

// toolchainsCheckCommand lists the mounted toolchains and, per toolchain,
// whether its bin/ is on the PATH, where the PATH finds its tool (the
// image's own copy comes first, the toolchain's after it) and the version
// the mounted tool answers; with go mounted it builds and runs a fixture
// module in the Session's working directory with the toolchain's go: no
// dependency, no module proxy. The script names no toolchain of a run, so
// the mockllm's script is one for every run.
func toolchainsCheckCommand() string {
	var tools strings.Builder
	for _, name := range slices.Sorted(maps.Keys(toolchainTools)) {
		tools.WriteString(name + ") b=" + toolchainTools[name] + ";; ")
	}
	return `P=` + probeMarker + `
echo "$P ` + toolchainsProbeTools + `=$(ls ` + toolchainsMountRoot + ` 2>/dev/null | tr '\n' ' ')"
for t in $(ls ` + toolchainsMountRoot + ` 2>/dev/null); do
  case "$t" in ` + tools.String() + `*) b="$t";; esac
  m="` + toolchainsMountRoot + `/$t/bin/$b"
  case ":$PATH:" in *":` + toolchainsMountRoot + `/$t/bin:"*) o=` + toolchainsOnPath + `;; *) o=no;; esac
  echo "$P ` + toolchainsProbeOnPath + `-$t=$o"
  echo "$P ` + toolchainsProbePath + `-$t=$(command -v "$b")"
  if [ ! -x "$m" ]; then v="no $m"; elif [ "$b" = go ]; then v=$("$m" version 2>&1); else v=$("$m" --version 2>&1 | head -n 1); fi
  echo "$P ` + toolchainsProbeVersion + `-$t=$v"
done
g="` + toolchainsMountRoot + `/go/bin/go"
if [ ! -x "$g" ]; then echo "$P ` + toolchainsProbeBuilt + `=no go toolchain"; exit 0; fi
d="$(pwd)/` + toolchainsFixtureDir + `"
rm -rf "$d" && mkdir -p "$d/src" && cd "$d/src" || exit 1
printf 'module agentlab.example/fixture\n\ngo 1.22\n' > go.mod
printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("` + toolchainsFixtureOutput + `") }\n' > main.go
export GOCACHE="$d/cache" GOPATH="$d/gopath" GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=0
if out=$("$g" build -o "$d/fixture" . 2>&1); then echo "$P ` + toolchainsProbeBuilt + `=$("$d/fixture")"; else echo "$P ` + toolchainsProbeBuilt + `=failed: $(printf '%s' "$out" | tail -n 3 | tr '\n' ' ')"; fi`
}

// judgeToolchainCheck is the verdict on the check's lines: the mount lists
// exactly the toolchains, each one's bin/ on the PATH and its tool
// answering its version; with go, the fixture built and ran. Where the PATH
// finds a tool is said, not judged: an image's own copy of a tool comes
// before the toolchain's by the mount's contract.
func judgeToolchainCheck(probe codingProbe, want []string) (string, error) {
	listed := strings.Fields(probe[toolchainsProbeTools])
	if !sameNames(listed, want) {
		return "", fmt.Errorf("%s lists %v, wanted exactly %v", toolchainsMountRoot, listed, want)
	}
	var found []string
	for _, name := range want {
		path := toolchainsMountRoot + "/" + name + "/bin/" + toolchainTool(name)
		got, version := probe[toolchainsProbePath+"-"+name], probe[toolchainsProbeVersion+"-"+name]
		switch {
		case probe[toolchainsProbeOnPath+"-"+name] != toolchainsOnPath:
			return "", fmt.Errorf("%s/%s/bin is not on the PATH (%s=%q)", toolchainsMountRoot, name, toolchainsProbeOnPath, probe[toolchainsProbeOnPath+"-"+name])
		case version == "" || strings.HasPrefix(version, "no ") || strings.Contains(version, "not found"):
			return "", fmt.Errorf("%s's version answered %q", path, version)
		case name == toolchainGo && !strings.HasPrefix(version, "go version go"):
			return "", fmt.Errorf("go version answered %q", version)
		}
		resolved := "the PATH finds it"
		if got != path {
			resolved = fmt.Sprintf("the PATH finds %s at %q first, the image's own", toolchainTool(name), got)
		}
		found = append(found, path+": "+version+" ("+resolved+")")
	}
	verdict := toolchainsMountRoot + " lists [" + strings.Join(listed, " ") + "]; " + strings.Join(found, "; ")
	if !slices.Contains(want, toolchainGo) {
		return verdict, nil
	}
	if probe[toolchainsProbeBuilt] != toolchainsFixtureOutput {
		return "", fmt.Errorf("the fixture module: %q, wanted it built and printing %s", probe[toolchainsProbeBuilt], toolchainsFixtureOutput)
	}
	return verdict + "; the fixture module built and ran", nil
}
