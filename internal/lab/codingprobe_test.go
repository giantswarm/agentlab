package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// codingReply is a reply as an agent relays the sandbox probe on a line
// that holds the floor, wrapped the way models wrap it.
const codingReply = "Here is the output:\n```\n" +
	"AGENTLAB_PROBE uid=65532\n" +
	"AGENTLAB_PROBE harness=pid 7 uid 0\n" +
	"AGENTLAB_PROBE harness-environ=rc1 cat: can't open '/proc/7/environ': Permission denied\n" +
	"AGENTLAB_PROBE harness-mem=rc1 head: /proc/7/mem: Permission denied\n" +
	"AGENTLAB_PROBE forwarder=agentlab-coding\n" +
	"AGENTLAB_PROBE fwd-token=loopback\n" +
	"AGENTLAB_PROBE fwd-dots=400\n" +
	"AGENTLAB_PROBE fwd-encoded-dots=400\n" +
	"AGENTLAB_PROBE fwd-encoded-slash=400\n" +
	"AGENTLAB_PROBE fwd-unknown=404\n" +
	"AGENTLAB_PROBE fwd-no-token=401\n" +
	"AGENTLAB_PROBE write-tool=outside the toolset [preset:read-only]\n" +
	"AGENTLAB_PROBE env-tokens=0\n" +
	"AGENTLAB_PROBE data-tokens=0\n" +
	"AGENTLAB_PROBE model-key=placeholder\n" +
	"AGENTLAB_PROBE dns-cluster=93.184.215.14 \n" +
	"AGENTLAB_PROBE dns-direct=no answer\n" +
	"AGENTLAB_PROBE egress-outside=rc1 HTTP/1.1 403\n" +
	"AGENTLAB_PROBE egress-origin=rc0 \n" +
	"```\n"

var codingOrigins = []string{"https://github.com", "https://api.github.com"}

func TestCodingProbePasses(t *testing.T) {
	p := parseCodingProbe(codingReply)
	for name, verdict := range map[string]func(codingProbe, []string) (string, error){
		"uid split": codingProbe.uidSplit, "forwarder": codingProbe.forwarderPaths, "write tool": codingProbe.writeToolRefused,
		"credentials": codingProbe.credentials, "dns": codingProbe.dnsCanary, "egress": codingProbe.egress,
	} {
		if got, err := verdict(p, codingOrigins); err != nil || got == "" {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	if got, _ := p.dnsCanary(nil); !strings.Contains(got, "93.184.215.14") || !strings.Contains(got, "no answer") {
		t.Errorf("the DNS record = %q, want both readings", got)
	}
}

// A reply whose newlines arrive escaped, as one line of literal "\n"
// separators at any escape depth, or joined on one line with or without the
// markdown around each reading, reads the same as one with real newlines.
func TestCodingProbeLineSeparators(t *testing.T) {
	want := parseCodingProbe(codingReply)
	for name, reply := range map[string]string{
		"real newlines":      codingReply,
		"literal newlines":   strings.ReplaceAll(codingReply, "\n", `\n`),
		"double-escaped":     strings.ReplaceAll(codingReply, "\n", `\\n`),
		"triple-escaped":     strings.ReplaceAll(codingReply, "\n", `\\\\n`),
		"one line":           strings.ReplaceAll(codingReply, "\n"+probeMarker, " "+probeMarker),
		"one line, spans":    strings.ReplaceAll(codingReply, "\n"+probeMarker, "` `"+probeMarker),
		"one line, bold":     strings.ReplaceAll(codingReply, "\n"+probeMarker, "** **"+probeMarker),
		"one line, quotes":   strings.ReplaceAll(codingReply, "\n"+probeMarker, `" "`+probeMarker),
		"uid words appended": strings.Replace(codingReply, "uid=65532", "uid=65532 (the unprivileged user)", 1),
	} {
		got := parseCodingProbe(reply)
		if len(got) != len(want) {
			t.Errorf("%s: %d readings, want %d", name, len(got), len(want))
		}
		for key, value := range want {
			if key == "uid" {
				continue
			}
			if got[key] != value {
				t.Errorf("%s: %s = %q, want %q", name, key, got[key], value)
			}
		}
		if _, err := got.uidSplit(nil); err != nil {
			t.Errorf("%s: uid split: %v", name, err)
		}
		wrong := parseCodingProbe(strings.Replace(reply, "uid=65532", "uid=0", 1))
		if uid, _, _ := strings.Cut(wrong["uid"], " "); uid != "0" {
			t.Errorf("%s: wrong uid read as %q, want 0", name, wrong["uid"])
		}
		if _, err := wrong.uidSplit(nil); err == nil {
			t.Errorf("%s: uid split passed on uid 0", name)
		}
	}
}

// Quotes escaped at any depth read as quotes, and a value loses those around
// it.
func TestCodingProbeEscapedQuotes(t *testing.T) {
	for name, quote := range map[string]string{"once": `\"`, "twice": `\\\"`, "three times": `\\\\\\\"`} {
		got := parseCodingProbe(probeMarker + " forwarder=" + quote + "agentlab-coding" + quote + `\n` + probeMarker + " uid=65532")
		if got["forwarder"] != "agentlab-coding" || got["uid"] != "65532" {
			t.Errorf("%s: forwarder %q, uid %q", name, got["forwarder"], got["uid"])
		}
	}
}

// Each floor row fails on the reading that breaks it.
func TestCodingProbeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, line, replace string
		verdict             func(codingProbe, []string) (string, error)
	}{
		{"root claude", "uid=65532", "uid=0", codingProbe.uidSplit},
		{"no harness", "harness=pid 7 uid 0", "harness=", codingProbe.uidSplit},
		{"environ read", "harness-environ=rc1 cat: can't open '/proc/7/environ': Permission denied", "harness-environ=rc0 ", codingProbe.uidSplit},
		{"dots forwarded", "fwd-dots=400", "fwd-dots=200", codingProbe.forwarderPaths},
		{"encoded slash forwarded", "fwd-encoded-slash=400", "fwd-encoded-slash=404", codingProbe.forwarderPaths},
		{"no forwarder", "forwarder=agentlab-coding", "forwarder=", codingProbe.forwarderPaths},
		{"unknown server refused with the loopback token", "fwd-unknown=404", "fwd-unknown=401", codingProbe.forwarderPaths},
		{"write tool other error", "write-tool=outside the toolset [preset:read-only]", "write-tool=unauthorized", codingProbe.writeToolRefused},
		{"write tool answered", "write-tool=outside the toolset [preset:read-only]", "write-tool=answered: {\"result\":{}}", codingProbe.writeToolRefused},
		{"token in env", "env-tokens=0", "env-tokens=1", codingProbe.credentials},
		{"token in data", "data-tokens=0", "data-tokens=2", codingProbe.credentials},
		{"real key", "model-key=placeholder", "model-key=other", codingProbe.credentials},
		{"outside reached", "egress-outside=rc1 HTTP/1.1 403", "egress-outside=rc0 ", codingProbe.egress},
		{"outside failed otherwise", "egress-outside=rc1 HTTP/1.1 403", "egress-outside=rc1 wget: bad address", codingProbe.egress},
		{"origin denied", "egress-origin=rc0 ", "egress-origin=rc1 HTTP/1.1 403", codingProbe.egress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := strings.Replace(codingReply, "AGENTLAB_PROBE "+tc.line, "AGENTLAB_PROBE "+tc.replace, 1)
			if reply == codingReply {
				t.Fatalf("the fixture has no line %q", tc.line)
			}
			if got, err := tc.verdict(parseCodingProbe(reply), codingOrigins); err == nil {
				t.Errorf("passed: %q", got)
			}
		})
	}
}

// With only kagent's placeholder in the MCP config the probe sends the
// placeholder: the forwarder refuses an unknown server at its
// authentication (401); a 404 then means it routed an unauthenticated
// request.
func TestCodingProbeForwarderPlaceholderToken(t *testing.T) {
	reply := strings.Replace(codingReply, "fwd-token=loopback", "fwd-token=placeholder", 1)
	if _, err := parseCodingProbe(reply).forwarderPaths(nil); err == nil {
		t.Error("an unknown server answered 404 to the placeholder passed")
	}
	reply = strings.Replace(reply, "fwd-unknown=404", "fwd-unknown=401", 1)
	if got, err := parseCodingProbe(reply).forwarderPaths(nil); err != nil {
		t.Errorf("the placeholder's 401 for an unknown server: %v", err)
	} else if !strings.Contains(got, "an unknown server 401") {
		t.Errorf("verdict = %q", got)
	}
}

// An Agent without api.github.com among its origins has no origin to reach:
// the denial alone is judged.
// muster hiding the tool under the toolset is the refusal too.
func TestCodingProbeWriteToolNotFound(t *testing.T) {
	reply := strings.Replace(codingReply, "write-tool=outside the toolset [preset:read-only]", "write-tool=tool 'x_agent-manager_delete_agent' not found: tool not found", 1)
	if _, err := parseCodingProbe(reply).writeToolRefused(nil); err != nil {
		t.Error(err)
	}
}

func TestCodingProbeEgressWithoutOrigin(t *testing.T) {
	got, err := parseCodingProbe(codingReply).egress(nil)
	if err != nil || !strings.Contains(got, "no api.github.com origin") {
		t.Errorf("%q, %v", got, err)
	}
}

func TestCodingFixtureRecord(t *testing.T) {
	reply := "AGENTLAB_PROBE hook=ran\nAGENTLAB_PROBE claude-md=agentlab-claude-md-loaded\nAGENTLAB_PROBE agents-md=none\n" +
		"AGENTLAB_PROBE skills=agentlab-fixture\nAGENTLAB_PROBE subagents=agentlab-fixture\nAGENTLAB_PROBE commands=none\nAGENTLAB_PROBE mcp-servers=agentlab-coding\n"
	got, err := codingFixtureRecord(parseCodingProbe(reply))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CLAUDE.md loaded", "AGENTS.md not loaded (none)", "hook ran", "skill loaded", "command not loaded (none)", ".mcp.json server not loaded (agentlab-coding)"} {
		if !strings.Contains(got, want) {
			t.Errorf("record %q lacks %q", got, want)
		}
	}
	if _, err := codingFixtureRecord(parseCodingProbe("I did not run anything.")); err == nil {
		t.Error("a reply without the hook reading passed")
	}
}

// The probe never prints the forwarder's token: it stays in a variable, and
// every echo of a reading names no $tok.
func TestCodingSandboxCommandKeepsTheToken(t *testing.T) {
	claude := codingRuntimes[config.CodingRuntimeClaude]
	for _, line := range strings.Split(codingSandboxCommand(claude), "\n") {
		if strings.Contains(line, "echo") && strings.Contains(line, "$tok") {
			t.Errorf("a reading echoes the token: %s", line)
		}
	}
	for _, want := range []string{"/mcp/$name/../../x", "%2e%2e/%2e%2e", "..%2fx", codingTestWriteTool, codingTestOutsideHost} {
		if !strings.Contains(codingSandboxCommand(claude), want) {
			t.Errorf("the probe lacks %q", want)
		}
	}
}

// TestTakesAnyToolset: agent-manager writes a claude Harness agent with any
// muster toolset from 1.14.0 on, its release candidates and dev builds
// included.
func TestTakesAnyToolset(t *testing.T) {
	for version, want := range map[string]bool{
		"1.13.0":      false,
		"1.13.1-rc.1": false,
		"v1.13.0":     false,
		"1.14.0-rc.1": true,
		"1.14.0":      true,
		"2.0.0":       true,
		"dev-40bd6c1": true,
		"":            true,
	} {
		if got := takesAnyToolset(version); got != want {
			t.Errorf("takesAnyToolset(%q) = %v, want %v", version, got, want)
		}
	}
}

// On codex the probe has no root harness or forwarder to read, checks the
// codex model keys and counts the MCP credentials that are not kagent's
// placeholder.
func TestCodingSandboxCommandCodex(t *testing.T) {
	cmd := codingSandboxCommand(codingRuntimes[config.CodingRuntimeCodex])
	for _, absent := range []string{harnessProcessName, "--mcp-config", "ANTHROPIC_API_KEY"} {
		if strings.Contains(cmd, absent) {
			t.Errorf("the codex probe carries %q", absent)
		}
	}
	for _, want := range []string{codexOpenAIKeyEnv, codexBedrockKeyEnv, codexMCPCredentialEnvs, credentialPlaceholder, codingTestOutsideHost} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the codex probe lacks %q", want)
		}
	}
}

// An MCP credential in the sandbox that is not the placeholder fails the
// credentials stage; all placeholders pass it.
func TestCodingCredentialsMCP(t *testing.T) {
	base := "AGENTLAB_PROBE env-tokens=0\nAGENTLAB_PROBE data-tokens=0\nAGENTLAB_PROBE model-key=placeholder\n"
	if _, err := parseCodingProbe(base + "AGENTLAB_PROBE mcp-credentials=1 0\n").credentials(nil); err != nil {
		t.Errorf("one placeholder credential failed: %v", err)
	}
	if _, err := parseCodingProbe(base + "AGENTLAB_PROBE mcp-credentials=1 1\n").credentials(nil); err == nil {
		t.Error("a real MCP credential in the sandbox passed")
	}
}

// The credentials stage judges what the probe printed, not the model's relay
// of it: a reply that drops or rewords a line passes on valid credentials and
// fails on a token the output shows.
func TestCodingCredentialsFromToolOutput(t *testing.T) {
	const output = "AGENTLAB_PROBE uid=65532\nAGENTLAB_PROBE env-tokens=0\nAGENTLAB_PROBE data-tokens=0\nAGENTLAB_PROBE model-key=placeholder\nAGENTLAB_PROBE mcp-credentials=1 0\n"
	const reply = "AGENTLAB_PROBE uid=65532\nAGENTLAB_PROBE env-tokens=none found\n"
	if got, err := parseTurnProbe(reply, output).credentials(nil); err != nil {
		t.Errorf("a reply dropping and rewording probe lines failed valid credentials: %v", err)
	} else if !strings.Contains(got, "1 MCP credentials") {
		t.Errorf("the verdict %q misses the MCP credentials reading", got)
	}
	leaked := strings.Replace(output, "data-tokens=0", "data-tokens=2", 1)
	if got, err := parseTurnProbe(reply+"AGENTLAB_PROBE data-tokens=0\n", leaked).credentials(nil); err == nil {
		t.Errorf("a token under /data passed on the reply's word: %q", got)
	}
}

// A reading neither the output nor the reply carries is named missing, not
// judged as a token count.
func TestCodingCredentialsMissingReading(t *testing.T) {
	_, err := parseTurnProbe("", "AGENTLAB_PROBE uid=65532\nAGENTLAB_PROBE env-tokens=0\n").credentials(nil)
	if err == nil || !strings.Contains(err.Error(), "no data-tokens reading") {
		t.Errorf("a probe cut before its /data search: %v, wanted the missing reading named", err)
	}
}

// The sandbox prompt asks for the command timeout the probe needs: codex's
// default cuts the script at its /data search.
func TestCodingSandboxPromptTimeout(t *testing.T) {
	for name, rt := range codingRuntimes {
		if prompt := codingSandboxPrompt(rt); !strings.Contains(prompt, "300000 ms") {
			t.Errorf("the %s sandbox prompt names no command timeout: %q", name, prompt[:200])
		}
	}
}

// A relayed reading reads the same plain, with escaped quotes and newlines,
// or wrapped in quotes and markdown.
func TestCodingProbeValueShapes(t *testing.T) {
	for name, reply := range map[string]string{
		"plain":    "AGENTLAB_PROBE file=agentlab-saffron\nAGENTLAB_PROBE code-word=agentlab-saffron\n",
		"escaped":  `AGENTLAB_PROBE file=agentlab-saffron\"\nAGENTLAB_PROBE code-word=agentlab-saffron\"`,
		"quoted":   "\"AGENTLAB_PROBE file=agentlab-saffron\"\n'AGENTLAB_PROBE code-word=agentlab-saffron'\n",
		"markdown": "**AGENTLAB_PROBE file=agentlab-saffron**\n`AGENTLAB_PROBE code-word=agentlab-saffron`\n",
	} {
		got := parseCodingProbe(reply)
		for _, key := range []string{"file", "code-word"} {
			if got[key] != codingTestCodeWord {
				t.Errorf("%s: %s = %q, want %s", name, key, got[key], codingTestCodeWord)
			}
		}
	}
}
