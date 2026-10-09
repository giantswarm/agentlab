package lab

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The Session half of the skills proof: what a Session's sandbox sends to
// the skill's host. kagent binds a credentialed source's Secret to the
// source's host as Authorization; the egress gateway replaces only a header
// a request carries, with the placeholder kagent's runtimes send. Whether
// that binding is the golden boot's alone or every actor's shows in a
// request from the sandbox with the placeholder: a Session's policy without
// the binding lets it through unreplaced, one with it sends the Secret. The
// second turn has the agent's bash tool run `git ls-remote` of the source
// twice, without Authorization (git before a challenge) and with the
// placeholder, and print the exit codes. On the fixture the fixture's
// record is the verdict; on another host the exit codes are.

// sandboxTrustBundle is where kagent's runtimes find the egress gateway's
// CA in the sandbox; git is pointed at it when it is there.
const sandboxTrustBundle = "/run/kagent/egress/trust-bundle.pem"

// probeMarker starts each line the probe command prints.
const probeMarker = "AGENTLAB_PROBE"

// sandboxProbeCommand is the shell command of the second turn: two
// ls-remotes of source, their exit codes and git's first error line of the
// placeholder one.
func sandboxProbeCommand(source string) string {
	return fmt.Sprintf(`export GIT_TERMINAL_PROMPT=0; [ -f %[1]s ] && export GIT_SSL_CAINFO=%[1]s; `+
		`git ls-remote %[2]s >/dev/null 2>/tmp/agentlab-probe-bare.err; echo "%[3]s bare=$?"; `+
		`git -c 'http.extraHeader=Authorization: Basic %[4]s' ls-remote %[2]s >/dev/null 2>/tmp/agentlab-probe.err; echo "%[3]s placeholder=$?"; `+
		`echo "%[3]s error=$(head -c 200 /tmp/agentlab-probe.err | tr '\n' ' ')"`,
		sandboxTrustBundle, source, probeMarker, credentialPlaceholder)
}

// sandboxProbePrompt asks the agent to run the command once and relay its
// marker lines.
func sandboxProbePrompt(source string) string {
	return "Run this exact command once with your bash tool, then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + sandboxProbeCommand(source)
}

// sandboxProbe is what the second turn relayed: the exit codes of the bare
// and the placeholder ls-remote, git's error line of the latter.
type sandboxProbe struct {
	bare, placeholder int
	err               string
}

var (
	probeBare        = regexp.MustCompile(probeMarker + ` bare=(\d+)`)
	probePlaceholder = regexp.MustCompile(probeMarker + ` placeholder=(\d+)`)
	probeError       = regexp.MustCompile(probeMarker + ` error=([^\n]*)`)
)

// parseSandboxProbe reads the marker lines out of the reply.
func parseSandboxProbe(reply string) (sandboxProbe, error) {
	bare, placeholder := probeBare.FindStringSubmatch(reply), probePlaceholder.FindStringSubmatch(reply)
	if bare == nil || placeholder == nil {
		return sandboxProbe{}, fmt.Errorf("the reply carries no %s exit codes (did the agent run its bash tool?): %s", probeMarker, excerpt(reply, 300))
	}
	p := sandboxProbe{}
	p.bare, _ = strconv.Atoi(bare[1])
	p.placeholder, _ = strconv.Atoi(placeholder[1])
	if e := probeError.FindStringSubmatch(reply); e != nil {
		p.err = strings.TrimSpace(strings.Trim(strings.TrimSpace(e[1]), "`"))
	}
	return p, nil
}

func (p sandboxProbe) String() string {
	s := fmt.Sprintf("git ls-remote without Authorization exit %d, with the placeholder exit %d", p.bare, p.placeholder)
	if p.err != "" {
		s += " (" + excerpt(p.err, 120) + ")"
	}
	return s
}

// gitAuthRefusal reports whether git's error line is the host refusing the
// request for its credential — 401, or GitHub's 400 to the placeholder,
// which is no valid Basic credential — not the gateway denying it or a
// failed TLS handshake, which would tell nothing about the credential.
func gitAuthRefusal(line string) bool {
	lower := strings.ToLower(line)
	for _, words := range []string{"authentication failed", "could not read username", "invalid username or password", "invalid credentials", "returned error: 401", "returned error: 400", "repository not found"} {
		if strings.Contains(lower, words) {
			return true
		}
	}
	return false
}

// hostProbeVerdict judges the probe against a host other than the fixture
// (GitHub for the default fixture and the usual --skill-secret): with a
// credential bound, a placeholder request the host accepted carried the
// Secret (the finding), one it refused for its credential carried none; a
// public source has nothing bound, so its status is reported as it is.
func hostProbeVerdict(source string, credentialed bool, p sandboxProbe) (verdict string, finding bool, err error) {
	host := source
	if u, err := url.Parse(source); err == nil {
		host = u.Host
	}
	status := fmt.Sprintf("session request (%s): %s", host, p)
	switch {
	case !credentialed:
		return status + "; no credential is bound to a public source", false, nil
	case p.placeholder == 0:
		return status + " — the host accepted the placeholder request: the gateway replaced it with the source's credential", true, nil
	case gitAuthRefusal(p.err):
		return sessionNoCredential + " (" + status + ": the host refused the placeholder for its credential)", false, nil
	}
	return "", false, fmt.Errorf("%s: the placeholder request failed for another reason than the host's credential check, which tells nothing about the credential", status)
}
