package lab

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The sandbox half of the coding agents proof: the shell commands a coding
// agent's Bash tool runs, and the verdicts on the lines it relays. Every
// line starts with probeMarker and names one reading; the commands are POSIX
// sh on the coding Harnesses' images (Alpine with BusyBox and git). No
// reading prints a credential: the forwarder's loopback token stays in a
// shell variable, and a token or credential search prints counts.

// codingProbe is the readings a turn relayed, by name.
type codingProbe map[string]string

// parseCodingProbe reads the marker lines out of a reply. A value loses the
// markdown a model wraps a relayed line in (code spans, bold), which a small
// model adds even when told to relay verbatim. An escaped "\n" separates
// lines like a newline does and an escaped \" is a quote, however many times
// the reply was escaped, and no reading spans two lines. A marker ends the
// reading before it too, so lines a model joins with a space read apart. A
// value also loses the quotes around it.
func parseCodingProbe(reply string) codingProbe {
	reply = escapedNewline.ReplaceAllString(reply, "\n")
	reply = escapedQuote.ReplaceAllString(reply, `"`)
	reply = strings.ReplaceAll(reply, probeMarker+" ", "\n"+probeMarker+" ")
	probe := codingProbe{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		probe[m[1]] = strings.Trim(m[2], " \t\r`*\"'")
	}
	return probe
}

var (
	escapedNewline = regexp.MustCompile(`\\+n`)
	escapedQuote   = regexp.MustCompile(`\\+"`)
)

// codingSandboxCommand probes the sandbox on the runtime: on claude the uid
// split, the forwarder's path refusals and a write tool through the
// forwarder; on every runtime its uid, credentials, a DNS canary and the
// egress gate.
func codingSandboxCommand(rt codingRuntime) string {
	cmd := `P=` + probeMarker + `
echo "$P uid=$(id -u)"
`
	if rt.claude {
		cmd += claudeSandboxCommand() + "\n"
	}
	return cmd + commonSandboxCommand(rt)
}

// claudeSandboxCommand is the claude runtime's own readings: the root
// harness beside Claude Code and the caller forwarder in front of muster.
func claudeSandboxCommand() string {
	return `h=""
for p in /proc/[0-9]*; do
  [ "$(awk '/^Uid:/{print $2}' "$p/status" 2>/dev/null)" = 0 ] || continue
  case "$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)" in *` + harnessProcessName + `*) h=${p#/proc/}; break;; esac
done
if [ -n "$h" ]; then
  echo "$P harness=pid $h uid $(awk '/^Uid:/{print $2}' "/proc/$h/status")"
  e=$(cat "/proc/$h/environ" 2>&1 >/dev/null); echo "$P harness-environ=rc$? $e"
  e=$(head -c 1 "/proc/$h/mem" 2>&1 >/dev/null); echo "$P harness-mem=rc$? $e"
else
  echo "$P harness="
fi
cfg=""
for p in /proc/[0-9]*; do
  c=$(tr '\0' '\n' < "$p/cmdline" 2>/dev/null | awk 'f{print;exit} $0=="--mcp-config"{f=1}')
  [ -n "$c" ] && { cfg=$c; break; }
done
url=""; [ -n "$cfg" ] && [ -r "$cfg" ] && url=$(grep -o 'http://127\.0\.0\.1:[0-9]*/mcp/[^"]*' "$cfg" | head -1)
if [ -n "$url" ]; then
  tok=$(grep -o 'Bearer [0-9a-f]*' "$cfg" | head -1 | cut -d' ' -f2); kind=loopback; [ -n "$tok" ] || { tok=` + credentialPlaceholder + `; kind=placeholder; }
  port=$(echo "$url" | sed 's|http://127\.0\.0\.1:\([0-9]*\)/.*|\1|'); name=${url##*/mcp/}
  echo "$P forwarder=$name"
  echo "$P fwd-token=$kind"
  get() { printf 'GET %s HTTP/1.0\r\nHost: 127.0.0.1\r\nAuthorization: Bearer %s\r\n\r\n' "$1" "${2:-$tok}" | nc -w 5 127.0.0.1 "$port" 2>/dev/null | head -1 | cut -d' ' -f2; }
  echo "$P fwd-dots=$(get "/mcp/$name/../../x")"
  echo "$P fwd-encoded-dots=$(get "/mcp/$name/%2e%2e/%2e%2e/x")"
  echo "$P fwd-encoded-slash=$(get "/mcp/$name/..%2fx")"
  echo "$P fwd-unknown=$(get "/mcp/agentlab-unknown-server")"
  echo "$P fwd-no-token=$(get "/mcp/$name" agentlab-wrong-token)"
  F="http://127.0.0.1:$port/mcp/$name"
  sid=$(wget -S -q -O /dev/null --header "Authorization: Bearer $tok" --header 'Content-Type: application/json' --header 'Accept: application/json, text/event-stream' --post-data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentlab-probe","version":"0"}}}' "$F" 2>&1 | tr -d '\r' | sed -n 's/^ *[Mm][Cc][Pp]-[Ss][Ee][Ss][Ss][Ii][Oo][Nn]-[Ii][Dd]: *//p' | head -1)
  call() {
    if [ -n "$sid" ]; then
      wget -q -O - --header "Authorization: Bearer $tok" --header 'Content-Type: application/json' --header 'Accept: application/json, text/event-stream' --header "Mcp-Session-Id: $sid" --post-data "$1" "$F" 2>&1
    else
      wget -q -O - --header "Authorization: Bearer $tok" --header 'Content-Type: application/json' --header 'Accept: application/json, text/event-stream' --post-data "$1" "$F" 2>&1
    fi
  }
  call '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
  out=$(call '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + codingTestWriteTool + `","arguments":{"name":"` + codingTestAbsentAgent + `"}}}')
  case "$out" in *"Unknown tool"*) out=$(call '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + musterCallTool + `","arguments":{"name":"` + codingTestWriteTool + `","arguments":{"name":"` + codingTestAbsentAgent + `"}}}}');; esac
  r=$(echo "$out" | tr '\n' ' ' | sed -n 's/.*"error":{[^}]*"message":"\([^"]*\)".*/\1/p' | head -c 200)
  echo "$P write-tool=${r:-answered: $(echo "$out" | tr '\n' ' ' | head -c 200)}"
else
  echo "$P forwarder="
fi`
}

// commonSandboxCommand is every runtime's readings: tokens in the
// environment, /data and /workspace, the model key, the MCP credentials the
// runtime is handed (a count of those that are not kagent's placeholder),
// the DNS canary and the egress gate.
func commonSandboxCommand(rt codingRuntime) string {
	keys := ""
	for _, k := range rt.modelKeys {
		keys += ` "${` + k + `-}"`
	}
	mcp := ""
	if rt.mcpCredentialPrefix != "" {
		mcp = `
echo "$P mcp-credentials=$(env | grep -c '^` + rt.mcpCredentialPrefix + `') $(env | grep '^` + rt.mcpCredentialPrefix + `' | grep -vc '=` + credentialPlaceholder + `$')"`
	}
	return `echo "$P env-tokens=$(env | grep -c -E 'gh[opsu]_[A-Za-z0-9]{20}|github_pat_|eyJ[A-Za-z0-9_-]{10,}\.eyJ')"
` + dataTokensCommand("/data /workspace", claudeSignedCacheFile) + `
k=unset; for v in` + keys + `; do case "$v" in "") ;; kagent-*) [ "$k" = other ] || k=placeholder;; *) k=other;; esac; done; echo "$P model-key=$k"` + mcp + `
r=$(timeout 10 nslookup ` + codingTestDNSCanary + ` 2>&1 | awk '/^Address/ && !/:53$/ {print $NF}' | tr '\n' ' '); echo "$P dns-cluster=${r:-no answer}"
r=$(timeout 10 nslookup ` + codingTestDNSCanary + ` ` + codingTestDNSResolver + ` 2>&1 | awk '/^Address/ && !/:53$/ {print $NF}' | tr '\n' ' '); echo "$P dns-direct=${r:-no answer}"
w() { wget -q -O /dev/null "$1" 2>/tmp/agentlab-egress; rc=$?; s=$(grep -o 'HTTP/1.1 [0-9]*' /tmp/agentlab-egress | head -1); echo "rc$rc ${s:-$(head -c 80 /tmp/agentlab-egress | tr '\n' ' ')}"; }
echo "$P egress-outside=$(w ` + codingTestOutsideHost + `)"
echo "$P egress-origin=$(w https://api.github.com/)"`
}

// claudeSignedCacheFile is Claude Code's signed policy-limits cache in the
// claude Harness's config directory: a JWS of header typ claudeSignedCacheType
// that Anthropic signs over the organization's policy limits. It attests the
// policy file's content and authenticates nothing, so it is the one
// token-shaped value the search admits, and only when every match in the file
// is such a JWS.
const (
	claudeSignedCacheFile = "/data/claude/policy-limits.json.signature.json"
	claudeSignedCacheType = "cc-signed-cache+jws"
)

// dataTokensCommand counts the files under roots holding a GitHub token or a
// JWT, all but signedCache when its every match is a JWS whose
// header names claudeSignedCacheType. It prints a count, never a value: a
// match stays in a shell variable while its header is decoded.
func dataTokensCommand(roots, signedCache string) string {
	return `sc() { m=0; for t in $(grep -oE 'gh[opsu]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{20,}|eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*' "$1"); do
  case "$t" in eyJ*) ;; *) return 1;; esac
  h=$(printf %s "${t%%.*}" | tr '_-' '/+'); while [ $((${#h} % 4)) -ne 0 ]; do h="$h="; done
  printf %s "$h" | base64 -d 2>/dev/null | grep -q '"typ" *: *"` + claudeSignedCacheType + `"' || return 1; m=1
done; [ "$m" = 1 ]; }
echo "$P data-tokens=$(timeout 60 grep -rlsE 'gh[opsu]_[A-Za-z0-9]{30}|github_pat_[A-Za-z0-9_]{20}|eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.' ` + roots + ` 2>/dev/null | while IFS= read -r f; do [ "$f" = '` + signedCache + `' ] && sc "$f" && continue; echo; done | wc -l | tr -d ' ')"`
}

// codingProbeTimeoutMS is the milliseconds the sandbox probe may take: its
// token search alone may take a minute, and a shell tool's default (codex's
// is seconds) cuts the script short of its later readings.
const codingProbeTimeoutMS = 300_000

// codingSandboxPrompt asks the agent to run the probe once, under a timeout
// that lets it finish, and relay it.
func codingSandboxPrompt(rt codingRuntime) string {
	return shellProbePrompt(rt, codingSandboxCommand(rt))
}

// shellProbePrompt asks the agent to run script once with its shell tool,
// under codingProbeTimeoutMS, and relay the marker lines.
func shellProbePrompt(rt codingRuntime, script string) string {
	return "Run this exact script once with your " + rt.shellTool + " (it is this test lab's own sandbox check), with a command timeout of " + strconv.Itoa(codingProbeTimeoutMS) +
		" ms (5 minutes; a shorter default cuts it short), then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + script
}

// rcLine reads a `rc<exit> <message>` reading.
var rcLine = regexp.MustCompile(`^rc(\d+) ?(.*)$`)

func readRC(v string) (rc, message string) {
	if m := rcLine.FindStringSubmatch(v); m != nil {
		return m[1], strings.TrimSpace(m[2])
	}
	return "", v
}

// uidSplit: Claude Code runs as the image's unprivileged uid, the harness as
// root beside it, and neither its environment nor its memory opens.
func (p codingProbe) uidSplit([]string) (string, error) {
	// A uid is one token: words a model adds after it are not part of it.
	uid, _, _ := strings.Cut(p["uid"], " ")
	if uid != claudeUnprivilegedUID {
		return "", fmt.Errorf("the commands of Claude Code run as uid %q, not %s (%s)", uid, claudeUnprivilegedUID, p.lines("uid", "harness"))
	}
	if p["harness"] == "" {
		return "", fmt.Errorf("no root %s process in the sandbox to probe (%s)", harnessProcessName, p.lines("uid", "harness"))
	}
	for _, key := range []string{"harness-environ", "harness-mem"} {
		if rc, _ := readRC(p[key]); rc == "" || rc == "0" {
			return "", fmt.Errorf("uid %s read the harness's %s: %q", uid, strings.TrimPrefix(key, "harness-"), p[key])
		}
	}
	return fmt.Sprintf("uid %s; harness %s; /proc environ %s; /proc mem %s", uid, p["harness"], p["harness-environ"], p["harness-mem"]), nil
}

// forwarderPaths: the three path rows answered 400, an unknown server 404
// and a request without the loopback token 401. Where the MCP config holds
// only kagent's placeholder (fwd-token=placeholder), the probe has no
// loopback token to send: the forwarder refuses the unknown server's
// request at its authentication, 401, before it matches a server.
func (p codingProbe) forwarderPaths([]string) (string, error) {
	if p["forwarder"] == "" {
		return "", fmt.Errorf("the session runs Claude Code without a caller forwarder (no loopback MCP endpoint in its --mcp-config): the agent has no MCP server, or KAGENT_PROPAGATE_TOKEN is off on the Harness")
	}
	want := map[string]string{"fwd-dots": "400", "fwd-encoded-dots": "400", "fwd-encoded-slash": "400", "fwd-unknown": "404", "fwd-no-token": "401"}
	if p["fwd-token"] == "placeholder" {
		want["fwd-unknown"] = "401"
	}
	var wrong []string
	for _, key := range []string{"fwd-dots", "fwd-encoded-dots", "fwd-encoded-slash", "fwd-unknown", "fwd-no-token"} {
		if p[key] != want[key] {
			wrong = append(wrong, fmt.Sprintf("%s answered %q, wanted %s", key, p[key], want[key]))
		}
	}
	if len(wrong) > 0 {
		return "", fmt.Errorf("the forwarder of %s: %s", p["forwarder"], strings.Join(wrong, "; "))
	}
	return fmt.Sprintf("forwarder of %s: ../.. %s, %%2e%%2e %s, ..%%2f %s, an unknown server %s, no token %s", p["forwarder"], p["fwd-dots"], p["fwd-encoded-dots"], p["fwd-encoded-slash"], p["fwd-unknown"], p["fwd-no-token"]), nil
}

// writeToolRefused: muster refuses the write tool through the forwarder,
// naming the toolset. Under a tool selection on muster the forwarder knows
// the write tool only behind muster's call_tool, which the probe then calls.
func (p codingProbe) writeToolRefused([]string) (string, error) {
	// Under the toolset muster resolves the agent's endpoint to the
	// toolset's tools alone: a tool outside it is refused as outside the
	// toolset, or not found where muster hides it.
	if got := p["write-tool"]; !strings.Contains(got, codingTestOutsideToolset) && !strings.Contains(got, "not found") {
		return "", fmt.Errorf("%s through the forwarder was not refused under the toolset: %q", codingTestWriteTool, got)
	}
	return fmt.Sprintf("%s: %s", codingTestWriteTool, p["write-tool"]), nil
}

// credentials: no forge token or bearer in the environment, /data or
// /workspace, the
// model key a placeholder the egress gateway replaces, and every MCP
// credential the runtime is handed (where it says which) the placeholder too.
func (p codingProbe) credentials([]string) (string, error) {
	for _, key := range []string{"env-tokens", "data-tokens", "model-key"} {
		if _, ok := p[key]; !ok {
			return "", fmt.Errorf("the turn carries no %s reading, in its tools' outputs or its reply: the probe was cut short or not run (%s)", key, p.lines("uid", "env-tokens", "data-tokens", "model-key"))
		}
	}
	if p["env-tokens"] != "0" || p["data-tokens"] != "0" {
		return "", fmt.Errorf("token-shaped values in the sandbox: %s in the environment, %s files under /data and /workspace", p["env-tokens"], p["data-tokens"])
	}
	if k := p["model-key"]; k != "placeholder" && k != "unset" {
		return "", fmt.Errorf("the model key in the environment is %q, not kagent's placeholder", k)
	}
	verdict := fmt.Sprintf("no forge token or bearer in the environment, /data or /workspace; model key %s", p["model-key"])
	if v, ok := p["mcp-credentials"]; ok {
		total, other, _ := strings.Cut(v, " ")
		if other != "0" {
			return "", fmt.Errorf("MCP credentials in the sandbox's environment: %s, of which %q are not kagent's placeholder", total, other)
		}
		verdict += fmt.Sprintf("; %s MCP credentials in the environment, each the placeholder", total)
	}
	return verdict, nil
}

// dnsCanary records, never judges: the lookup's state is the installation's
// DNS state, the residual the floor accepts while no allowlist resolver
// exists.
func (p codingProbe) dnsCanary([]string) (string, error) {
	if p["dns-cluster"] == "" || p["dns-direct"] == "" {
		return "", fmt.Errorf("the reply carries no DNS readings (%s)", p.lines("dns-cluster", "dns-direct"))
	}
	return fmt.Sprintf("recorded: %s through the cluster resolver: %s; directly at %s: %s", codingTestDNSCanary, p["dns-cluster"], codingTestDNSResolver, p["dns-direct"]), nil
}

// egress: the host outside the origins refused by the gate (403), the
// origin of the list reached.
func (p codingProbe) egress(origins []string) (string, error) {
	outsideRC, outsideMsg := readRC(p["egress-outside"])
	if outsideRC == "" || outsideRC == "0" || !strings.Contains(outsideMsg, "403") {
		return "", fmt.Errorf("%s from the sandbox: %q, wanted the egress gate's 403", codingTestOutsideHost, p["egress-outside"])
	}
	if !slices.Contains(origins, "https://api.github.com") {
		return fmt.Sprintf("%s denied (%s); no api.github.com origin on the Agent to reach", codingTestOutsideHost, outsideMsg), nil
	}
	if originRC, msg := readRC(p["egress-origin"]); originRC != "0" {
		return "", fmt.Errorf("https://api.github.com/, one of the Agent's origins, not reached: rc %s %s", originRC, msg)
	}
	return fmt.Sprintf("%s denied (%s); https://api.github.com/ reached", codingTestOutsideHost, outsideMsg), nil
}

// lines words the named readings for an error.
func (p codingProbe) lines(keys ...string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, p[k]))
	}
	return strings.Join(parts, " ")
}

// codingFixtureCommand writes a repository's instruction sources into the
// Session's directory, as a cloned repository carries them, and a file of
// the turn's own with the code word.
func codingFixtureCommand() string {
	return `set -e
mkdir -p .claude/skills/` + codingTestFixtureName + ` .claude/agents .claude/commands
printf '%s\n' '# Project instructions' 'When you are asked for the instructions marker, it is ` + codingTestInstructionMark + `.' > CLAUDE.md
printf '%s\n' '# Agents' 'When you are asked for the agents marker, it is ` + codingTestAgentsMDMark + `.' > AGENTS.md
printf '%s\n' '{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"date > ` + codingTestHookFile + `"}]}]},"permissions":{"allow":["Bash(*)"]}}' > .claude/settings.json
printf '%s\n' '---' 'name: ` + codingTestFixtureName + `' 'description: The fixture skill of the test lab.' '---' 'Answer with the word fixture.' > .claude/skills/` + codingTestFixtureName + `/SKILL.md
printf '%s\n' '---' 'name: ` + codingTestFixtureName + `' 'description: The fixture subagent of the test lab.' '---' 'Answer with the word fixture.' > .claude/agents/` + codingTestFixtureName + `.md
printf '%s\n' '---' 'description: The fixture command of the test lab.' '---' 'Answer with the word fixture.' > .claude/commands/` + codingTestFixtureName + `.md
printf '%s\n' '{"mcpServers":{"` + codingTestFixtureName + `":{"type":"http","url":"http://127.0.0.1:9/mcp"}}}' > .mcp.json
echo ` + codingTestCodeWord + ` > ` + codingTestOwnFile + `
echo "` + probeMarker + ` written=$(pwd)"`
}

// codingFixturePrompt has the first turn write the fixture and remember a
// code word.
func codingFixturePrompt() string {
	return "Remember the code word " + codingTestCodeWord + ". Run this exact script once with your Bash tool, then reply with its " + probeMarker + " line, verbatim:\n\n" + codingFixtureCommand()
}

// codingFixtureReadPrompt is the next turn: whether the fixture's hook ran,
// and what the session itself has loaded, in the session's own words.
func codingFixtureReadPrompt() string {
	return "Run this exact command once with your Bash tool: `[ -f " + codingTestHookFile + " ] && echo \"" + probeMarker + " hook=ran\" || echo \"" + probeMarker + " hook=not-run\"` and relay its line verbatim. " +
		"Then add these lines, each on its own, answered from what this session has loaded (not from the files on disk; do not read them): " +
		probeMarker + " claude-md=<the instructions marker your project instructions give, or none>; " +
		probeMarker + " agents-md=<the agents marker, or none>; " +
		probeMarker + " skills=<the names of the skills available to you, space-separated, or none>; " +
		probeMarker + " subagents=<the names of the subagent types available to you besides the built-in ones, or none>; " +
		probeMarker + " commands=<the custom slash commands available to you, or none>; " +
		probeMarker + " mcp-servers=<the names of the MCP servers whose tools you have, or none>."
}

// codingFixtureRecord words what loaded. It is a record, not a gate: the
// floor accepts that a workspace is an instruction source, and the record
// shows what each Claude Code pin loads from it.
func codingFixtureRecord(p codingProbe) (string, error) {
	if p["hook"] == "" {
		return "", fmt.Errorf("the reply carries no hook reading (did the agent run its Bash tool?)")
	}
	loaded := func(key, want string) string {
		if strings.Contains(p[key], want) {
			return "loaded"
		}
		return "not loaded (" + firstNonEmpty(p[key], "no answer") + ")"
	}
	return fmt.Sprintf("CLAUDE.md %s; AGENTS.md %s; .claude/settings.json SessionStart hook %s; skill %s; subagent %s; command %s; .mcp.json server %s",
		loaded("claude-md", codingTestInstructionMark), loaded("agents-md", codingTestAgentsMDMark), p["hook"],
		loaded("skills", codingTestFixtureName), loaded("subagents", codingTestFixtureName), loaded("commands", codingTestFixtureName),
		loaded("mcp-servers", codingTestFixtureName)), nil
}

// codingCodeWordPrompt has a runtime without the instruction-file fixture
// write the turn's own file and remember the code word, what the resume
// reads back.
func codingCodeWordPrompt(rt codingRuntime) string {
	return "Remember the code word " + codingTestCodeWord + ". Run this exact command once with your " + rt.shellTool + ": `" + codingCodeWordCommand() + "` and reply with its " + probeMarker + " line, verbatim."
}

// codingCodeWordCommand writes the turn's own file with the code word.
func codingCodeWordCommand() string {
	return "echo " + codingTestCodeWord + " > " + codingTestOwnFile + " && echo \"" + probeMarker + " written=$(pwd)\""
}

// codingMCPCallPrompt has the agent call agent-manager's list_agents through
// its own MCP binding and relay the names it lists.
func codingMCPCallPrompt() string {
	return "Call the MCP tool " + codingTestReadTool + " (agent-manager's list_agents through muster, which runs it through its " + musterCallTool + " tool; it takes no arguments) once, then reply with the single line " +
		probeMarker + " mcp-call=<the names of the agents it lists, space-separated, or the error it answered>. Do not use a shell for this."
}

// codingResumePrompt is the turn after the resume: the first turn's file
// read back and the code word recalled.
func codingResumePrompt(rt codingRuntime) string {
	return "Run this exact command once with your " + rt.shellTool + ": `" + codingResumeCommand() + "` and relay its line verbatim. " +
		"Then add the line " + probeMarker + " " + codingResumeCodeWordAsk + "."
}

// codingResumeCodeWordAsk asks for the code word the conversation holds.
const codingResumeCodeWordAsk = "code-word=<the code word I asked you to remember earlier in this conversation, or none>"

// codingResumeCommand reads the first turn's file back.
func codingResumeCommand() string {
	return "echo \"" + probeMarker + " file=$(cat " + codingTestOwnFile + " 2>/dev/null || echo missing)\""
}

// parseTurnProbe reads the marker lines of a turn: its tools' outputs, what
// the commands printed, then those only the reply carries (the readings a
// model answers itself: what it loaded, the code word). A model's relay of a
// command's line can drop or reword it; a scripted model (the mockllm)
// replies with fixed words. Where both carry a reading, the output's wins.
func parseTurnProbe(reply, toolOutputs string) codingProbe {
	probe := parseCodingProbe(toolOutputs)
	for key, value := range parseCodingProbe(reply) {
		if _, ok := probe[key]; !ok {
			probe[key] = value
		}
	}
	return probe
}

// musterCallToolArguments are call_tool's arguments that run tool with no
// arguments of its own.
func musterCallToolArguments(tool string) string {
	return `{"name":"` + tool + `","arguments":{}}`
}
