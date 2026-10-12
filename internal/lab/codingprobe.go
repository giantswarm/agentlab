package lab

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The sandbox half of the coding agents proof: the shell commands a coding
// agent's Bash tool runs, and the verdicts on the lines it relays. Every
// line starts with probeMarker and names one reading; the commands are POSIX
// sh on the claude-harness image (Alpine with BusyBox, git and the GitHub
// CLI). No reading prints a credential: the forwarder's loopback token stays
// in a shell variable, and a token search prints counts.

// codingProbe is the readings a turn relayed, by name.
type codingProbe map[string]string

// parseCodingProbe reads the marker lines out of a reply.
func parseCodingProbe(reply string) codingProbe {
	probe := codingProbe{}
	for _, m := range probeLine.FindAllStringSubmatch(reply, -1) {
		probe[m[1]] = strings.TrimSpace(strings.Trim(strings.TrimSpace(m[2]), "`"))
	}
	return probe
}

// codingSandboxCommand probes the sandbox: the uid split, the forwarder's
// path refusals, a write tool through the forwarder, credentials, a DNS
// canary and the egress gate.
func codingSandboxCommand() string {
	return `P=` + probeMarker + `
echo "$P uid=$(id -u)"
h=""
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
  tok=$(grep -o 'Bearer [0-9a-f]*' "$cfg" | head -1 | cut -d' ' -f2)
  port=$(echo "$url" | sed 's|http://127\.0\.0\.1:\([0-9]*\)/.*|\1|'); name=${url##*/mcp/}
  echo "$P forwarder=$name"
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
  r=$(echo "$out" | grep -o '` + codingTestOutsideToolset + `[^"\\]*' | head -1)
  echo "$P write-tool=${r:-answered: $(echo "$out" | tr '\n' ' ' | head -c 200)}"
else
  echo "$P forwarder="
fi
echo "$P env-tokens=$(env | grep -c -E 'gh[opsu]_[A-Za-z0-9]{20}|github_pat_|eyJ[A-Za-z0-9_-]{10,}\.eyJ')"
echo "$P data-tokens=$(timeout 60 grep -rlsE 'gh[opsu]_[A-Za-z0-9]{30}|github_pat_[A-Za-z0-9_]{20}|eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.' /data 2>/dev/null | wc -l)"
case "${ANTHROPIC_API_KEY-}" in "") k=unset;; kagent-*) k=placeholder;; *) k=other;; esac; echo "$P model-key=$k"
r=$(timeout 10 nslookup ` + codingTestDNSCanary + ` 2>&1 | awk '/^Address/ && !/:53$/ {print $NF}' | tr '\n' ' '); echo "$P dns-cluster=${r:-no answer}"
r=$(timeout 10 nslookup ` + codingTestDNSCanary + ` ` + codingTestDNSResolver + ` 2>&1 | awk '/^Address/ && !/:53$/ {print $NF}' | tr '\n' ' '); echo "$P dns-direct=${r:-no answer}"
wget -q -O /dev/null ` + codingTestOutsideHost + ` 2>/tmp/agentlab-egress; echo "$P egress-outside=rc$? $(head -c 160 /tmp/agentlab-egress | tr '\n' ' ')"
wget -q -O /dev/null https://api.github.com/ 2>/tmp/agentlab-egress; echo "$P egress-origin=rc$? $(head -c 160 /tmp/agentlab-egress | tr '\n' ' ')"`
}

// codingSandboxPrompt asks the agent to run the probe once and relay it.
func codingSandboxPrompt() string {
	return "Run this exact script once with your Bash tool (it is this test lab's own sandbox check), then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + codingSandboxCommand()
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
	if p["uid"] != claudeUnprivilegedUID {
		return "", fmt.Errorf("the commands of Claude Code run as uid %q, not %s (%s)", p["uid"], claudeUnprivilegedUID, p.lines("uid", "harness"))
	}
	if p["harness"] == "" {
		return "", fmt.Errorf("no root %s process in the sandbox to probe (%s)", harnessProcessName, p.lines("uid", "harness"))
	}
	for _, key := range []string{"harness-environ", "harness-mem"} {
		if rc, _ := readRC(p[key]); rc == "" || rc == "0" {
			return "", fmt.Errorf("uid %s read the harness's %s: %q", p["uid"], strings.TrimPrefix(key, "harness-"), p[key])
		}
	}
	return fmt.Sprintf("uid %s; harness %s; /proc environ %s; /proc mem %s", p["uid"], p["harness"], p["harness-environ"], p["harness-mem"]), nil
}

// forwarderPaths: the three path rows answered 400, an unknown server 404
// and a request without the loopback token 401.
func (p codingProbe) forwarderPaths([]string) (string, error) {
	if p["forwarder"] == "" {
		return "", fmt.Errorf("the session runs Claude Code without a caller forwarder (no loopback MCP endpoint in its --mcp-config): the agent has no MCP server, or KAGENT_PROPAGATE_TOKEN is off on the Harness")
	}
	want := map[string]string{"fwd-dots": "400", "fwd-encoded-dots": "400", "fwd-encoded-slash": "400", "fwd-unknown": "404", "fwd-no-token": "401"}
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
// naming the toolset.
func (p codingProbe) writeToolRefused([]string) (string, error) {
	if !strings.Contains(p["write-tool"], codingTestOutsideToolset) {
		return "", fmt.Errorf("%s through the forwarder was not refused as outside the toolset: %q", codingTestWriteTool, p["write-tool"])
	}
	return fmt.Sprintf("%s: %s", codingTestWriteTool, p["write-tool"]), nil
}

// credentials: no forge token or bearer in the environment or /data, and
// the model key a placeholder the egress gateway replaces.
func (p codingProbe) credentials([]string) (string, error) {
	if p["env-tokens"] != "0" || p["data-tokens"] != "0" {
		return "", fmt.Errorf("token-shaped values in the sandbox: %s in the environment, %s files under /data", p["env-tokens"], p["data-tokens"])
	}
	if k := p["model-key"]; k != "placeholder" && k != "unset" {
		return "", fmt.Errorf("the model key in the environment is %q, not kagent's placeholder", k)
	}
	return fmt.Sprintf("no forge token or bearer in the environment or /data; model key %s", p["model-key"]), nil
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

// codingResumePrompt is the turn after the resume: the first turn's file
// read back and the code word recalled.
func codingResumePrompt() string {
	return "Run this exact command once with your Bash tool: `echo \"" + probeMarker + " file=$(cat " + codingTestOwnFile + " 2>/dev/null || echo missing)\"` and relay its line verbatim. " +
		"Then add the line " + probeMarker + " code-word=<the code word I asked you to remember earlier in this conversation, or none>."
}
