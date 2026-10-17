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
