package lab

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// The egress proof: an agent's egress is its compiled destinations (the
// model, its MCP servers — muster —, skill sources, telemetry) plus the
// HTTP(S) origins its Agent's spec.egress declares, and nothing else. One
// agent on the platform Harness, created through agent-manager as the person
// with one declared origin, and one turn whose bash tool probes from inside
// the actor:
//
//  1. create_agent with the read-only toolset and egress [origin]: Ready, the
//     Agent's spec.egress exactly that list.
//  2. One turn whose bash tool runs the probe; its lines are judged stage by
//     stage:
//     - the declared origin reached;
//     - an undeclared HTTPS host answered 403 by the egress gate, the
//     gateway's access log naming the actor egress policy's denial;
//     - muster, the agent's MCP server, answering, with no egress entry for it;
//     - a raw TCP connection to SSH on a public git host refused, by name and
//     by address: a server banner back through the actor's egress fails the
//     proof, naming the destination.
//
// --without-egress creates the agent with no egress entry and probes the same
// origin: the declared stage then fails, the control that the reach is the
// declaration's. The agent is removed on every exit path.
// docs/platform.md "The egress proof".

const (
	// egressTestAgent is the proof's agent, deleted by the same run.
	egressTestAgent = "agentlab-egress"
	// DefaultEgressOrigin is the origin the agent declares: a host no
	// compiled destination of the platform names.
	DefaultEgressOrigin = "https://example.com"
	// egressTestUndeclaredURL is an HTTPS host outside every list.
	egressTestUndeclaredURL = "https://proxy.golang.org/"
	// egressTestTCPHost and egressTestTCPPort are the raw TCP destination:
	// SSH on a public git host, whose server speaks first.
	egressTestTCPHost = "github.com"
	egressTestTCPPort = 22
	// egressTestTCPBanner starts every SSH server's identification string; a
	// probe that reads it reached the server.
	egressTestTCPBanner = "SSH-"
	// egressGateStatus and egressPolicyDenied are the gate's answer to an
	// origin outside the actor's policy, and its reason in the access log.
	egressGateStatus   = "403"
	egressPolicyDenied = "actor egress policy denied"
	// egressProbeTimeout bounds each request of the probe, in seconds.
	egressProbeTimeout = 20
	// egressTestSystemPrompt keeps the turn to the probe.
	egressTestSystemPrompt = "You are a probe in a test lab. Run exactly the commands you are given with your bash tool, once, and reply with the output lines you are asked for, verbatim."
)

// DefaultEgressReadyTimeout bounds the agent's boot.
const DefaultEgressReadyTimeout = 10 * time.Minute

// EgressTestOptions tunes the egress proof.
type EgressTestOptions struct {
	// Email is the lab user the agent is created by and the turn sent as;
	// empty is the admin.
	Email string
	// ReadyTimeout bounds the agent's boot.
	ReadyTimeout time.Duration
	// ModelConfig is the kagent ModelConfig the agent runs on; empty is
	// agent-manager's default one.
	ModelConfig string
	// Origin is the origin the agent declares and the probe reaches; empty
	// is DefaultEgressOrigin.
	Origin string
	// WithoutEgress creates the agent without its egress entry: the control
	// run, whose declared stage must fail.
	WithoutEgress bool
}

// EgressTest is the egress proof (the stages above).
func EgressTest(cfg *config.Config, opts EgressTestOptions) error {
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	if !cfg.Platform.Enabled || !cfg.Platform.Agents {
		return fmt.Errorf("platform.agents is off in %s — enable it and run `agentlab platform` first", config.File)
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultEgressReadyTimeout
	}
	if opts.Origin == "" {
		opts.Origin = DefaultEgressOrigin
	}
	if _, err := egressOriginHost(opts.Origin); err != nil {
		return err
	}
	user := cfg.AdminUser()
	if opts.Email != "" {
		user = cfg.FindUser(opts.Email)
	}
	if user == nil {
		return fmt.Errorf("no lab user %q in %s", opts.Email, config.File)
	}
	p := &egressProof{cfg: cfg, opts: opts, user: user, stages: newProofStages()}
	defer p.cleanup()
	return p.stages.close(p.run())
}

// egressProof is one run's state.
type egressProof struct {
	cfg    *config.Config
	opts   EgressTestOptions
	user   *config.User
	stages *proofStages
}

func (p *egressProof) run() error {
	token, err := passwordGrant(p.cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret, p.user.Email, p.user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	muster, err := openMusterSession(p.cfg, token, "egress-test")
	if err != nil {
		return err
	}
	modelConfig := p.opts.ModelConfig
	if modelConfig == "" {
		if modelConfig, err = agentManagerModelConfig(muster); err != nil {
			return err
		}
	}
	p.removeAgent()

	spec := agentSpec{
		Name: egressTestAgent, ModelConfig: modelConfig, Toolset: []string{presetReadOnly}, SystemMessage: egressTestSystemPrompt,
		Description: "Throwaway agent of `agentlab egress-test`; deleted by the same run.",
	}
	if !p.opts.WithoutEgress {
		spec.Egress = []string{p.opts.Origin}
	}
	p.stages.begin("create", "%screate_agent %s as %s, toolset %v, egress %v: Ready, the Agent's spec.egress that list", agentManagerToolPrefix, egressTestAgent, p.user.Email, spec.Toolset, spec.Egress)
	if p.opts.WithoutEgress {
		note("--without-egress: the control run; %s is probed undeclared and the declared stage must fail", p.opts.Origin)
	}
	musterURL, err := p.createReady(muster, spec)
	if err != nil {
		return err
	}

	p.stages.begin("probe turn", "A Session of %s as %s, one turn whose bash tool probes the actor's egress", egressTestAgent, p.user.Email)
	target := egressProbeTarget{origin: p.opts.Origin, undeclared: egressTestUndeclaredURL, muster: musterURL, tcpHost: egressTestTCPHost, tcpPort: egressTestTCPPort}
	if target.tcpAddress, err = lookupIPv4(egressTestTCPHost); err != nil {
		return err
	}
	reply, err := firstTurnAs(p.cfg, egressTestAgent, token, egressProbePrompt(target))
	if err != nil {
		return err
	}
	probe := parseCodingProbe(reply)
	if probe["declared"] == "" {
		return fmt.Errorf("the reply carries no %s lines (did the agent run its bash tool?): %s", probeMarker, excerpt(reply, 400))
	}
	note("%d probe lines", len(probe))

	p.stages.begin("declared", "%s, the Agent's declared origin, reached from the actor", p.opts.Origin)
	verdict, err := probe.declared(target)
	if err != nil {
		return err
	}
	note("%s", verdict)

	p.stages.begin("undeclared", "%s, outside the Agent's egress, answered %s by the egress gate: %q", egressTestUndeclaredURL, egressGateStatus, egressPolicyDenied)
	if verdict, err = probe.undeclared(target); err != nil {
		return err
	}
	note("%s", verdict)
	if verdict, err = gateDenial(target.undeclared); err != nil {
		return err
	}
	note("%s", verdict)

	p.stages.begin("muster", "muster (%s), the agent's MCP server, answers from the actor with no egress entry for it", musterURL)
	if verdict, err = probe.muster(target); err != nil {
		return err
	}
	note("%s", verdict)

	p.stages.begin("raw tcp", "A raw TCP connection to %s:%d (and %s:%d), outside every list, refused", egressTestTCPHost, egressTestTCPPort, target.tcpAddress, egressTestTCPPort)
	if verdict, err = probe.rawTCP(target); err != nil {
		return err
	}
	note("%s", verdict)
	return nil
}

// createReady is create_agent through muster as the person, the Agent Ready
// with the spec's egress; it returns the muster URL the agent's MCP binding
// points at.
func (p *egressProof) createReady(muster *musterSession, spec agentSpec) (string, error) {
	written, err := (agentManagerWriter{muster}).createAgent(spec)
	if err != nil {
		return "", err
	}
	if written.RequestedBy != p.user.Email {
		return "", fmt.Errorf("create_agent carries requestedBy=%q, wanted %q", written.RequestedBy, p.user.Email)
	}
	boot, err := waitAgentReady(spec.Name, p.opts.ReadyTimeout)
	if err != nil {
		return "", err
	}
	if !boot.ready {
		return "", fmt.Errorf("agent %s is not Ready after %s: %s", spec.Name, boot.elapsed.Round(time.Second), boot.reason)
	}
	egress, err := readAgentEgress(context.Background(), spec.Name)
	if err != nil {
		return "", err
	}
	if !slices.Equal(egress, spec.Egress) {
		return "", fmt.Errorf("the Agent %s carries spec.egress %v, wanted %v", spec.Name, egress, spec.Egress)
	}
	t, err := readAgentTemplate(spec.Name)
	if err != nil {
		return "", err
	}
	rms, err := readKagentObject(remoteMCPServerResource, t.mcpServer())
	if err != nil {
		return "", fmt.Errorf("the agent's %s %s: %w", remoteMCPServerKind, t.mcpServer(), err)
	}
	musterURL, _, _ := unstructured.NestedString(rms.Object, "spec", "url")
	if musterURL == "" {
		return "", fmt.Errorf("%s %s of agent %s carries no spec.url", remoteMCPServerKind, t.mcpServer(), spec.Name)
	}
	note("%s Ready in %s (requestedBy %s), spec.egress %v, its MCP server %s", spec.Name, boot.elapsed.Round(time.Second), written.RequestedBy, egress, musterURL)
	return musterURL, nil
}

func (p *egressProof) removeAgent() {
	if !agentExists(egressTestAgent) {
		return
	}
	if err := removeAgent(egressTestAgent); err != nil {
		note("cleanup: %v", err)
	}
}

func (p *egressProof) cleanup() { p.removeAgent() }

// egressProbeTarget is what the probe reaches for: the declared origin, an
// undeclared HTTPS URL, muster, and the raw TCP destination by name and by
// the address the lab host resolves it to (a lookup the actor's resolver
// refuses must not pass for the gate's refusal).
type egressProbeTarget struct {
	origin, undeclared, muster string
	tcpHost, tcpAddress        string
	tcpPort                    int
}

// egressProbeCommand is the bash the turn runs: one line per destination.
// An HTTP line is `rc<exit> status=<the last HTTP status seen, empty for none>
// <wget's last words>`; a TCP line is `rc<exit> <the first line the
// destination sent, printable>`: exit 7 the connect failed, 124 the timeout,
// 1 the connection closed or the read timed out without a line.
func egressProbeCommand(t egressProbeTarget) string {
	timeout := strconv.Itoa(egressProbeTimeout)
	return `P=` + probeMarker + `
get() {
  o=$(timeout ` + strconv.Itoa(egressProbeTimeout+10) + ` wget -q -S -O /dev/null -T ` + timeout + ` "$2" 2>&1); rc=$?
  s=$(printf '%s\n' "$o" | grep -o 'HTTP/[0-9.]* [0-9][0-9][0-9]' | tail -1 | cut -d' ' -f2)
  echo "$P $1=rc$rc status=$s $(printf '%s\n' "$o" | grep -v '^  ' | tail -2 | tr '\n' ' ' | head -c 160)"
}
tcp() {
  o=$(timeout ` + timeout + ` bash -c 'exec 3<>"/dev/tcp/$0/$1" || exit 7; IFS= read -r -t ` + timeout + ` l <&3; r=$?; printf "%s" "$l"; exit $r' "$2" "$3" 2>&1); rc=$?
  echo "$P $1=rc$rc $(printf '%s' "$o" | tr -d '\000-\037\177-\377' | head -c 120)"
}
get declared '` + t.origin + `/'
get undeclared '` + t.undeclared + `'
get muster '` + t.muster + `'
tcp tcp-name ` + t.tcpHost + ` ` + strconv.Itoa(t.tcpPort) + `
tcp tcp-address ` + t.tcpAddress + ` ` + strconv.Itoa(t.tcpPort)
}

// egressProbePrompt asks the agent to run the probe once and relay it.
func egressProbePrompt(t egressProbeTarget) string {
	return "Run this exact script once with your bash tool (it is this test lab's own egress check), then reply with every output line that starts with " + probeMarker +
		", verbatim and nothing else:\n\n" + egressProbeCommand(t)
}

// httpReading is one HTTP line of the probe.
type httpReading struct {
	rc, status, words string
}

var httpReadingLine = regexp.MustCompile(`^rc(\d+) status=(\S*) ?(.*)$`)

func readHTTP(v string) (httpReading, bool) {
	m := httpReadingLine.FindStringSubmatch(v)
	if m == nil {
		return httpReading{}, false
	}
	return httpReading{rc: m[1], status: m[2], words: strings.TrimSpace(m[3])}, true
}

func (r httpReading) String() string {
	return fmt.Sprintf("exit %s, HTTP status %s (%s)", r.rc, firstNonEmpty(r.status, "absent"), firstNonEmpty(r.words, "no output"))
}

// http reads the named HTTP line, an error naming it when the reply lacks it.
func (p codingProbe) http(key, dest string) (httpReading, error) {
	r, ok := readHTTP(p[key])
	if !ok {
		return r, fmt.Errorf("the probe's %s line for %s is %q, not `rc<exit> status=<status> …`", key, dest, p[key])
	}
	return r, nil
}

// declared: the origin answered, wget exiting 0.
func (p codingProbe) declared(t egressProbeTarget) (string, error) {
	r, err := p.http("declared", t.origin)
	if err != nil {
		return "", err
	}
	if r.rc != "0" {
		return "", fmt.Errorf("%s, the Agent's declared origin, not reached from the actor: %s", t.origin, r)
	}
	return fmt.Sprintf("%s reached: %s", t.origin, r), nil
}

// undeclared: the gate's 403.
func (p codingProbe) undeclared(t egressProbeTarget) (string, error) {
	r, err := p.http("undeclared", t.undeclared)
	if err != nil {
		return "", err
	}
	if r.status != egressGateStatus {
		return "", fmt.Errorf("%s, outside the Agent's egress, from the actor: %s, wanted the egress gate's %s", t.undeclared, r, egressGateStatus)
	}
	return fmt.Sprintf("%s denied from the actor: %s", t.undeclared, r), nil
}

// muster: an HTTP answer that is not the gate's 403 (muster answers a
// request without a token itself, 401).
func (p codingProbe) muster(t egressProbeTarget) (string, error) {
	r, err := p.http("muster", t.muster)
	if err != nil {
		return "", err
	}
	if r.status == "" || r.status == egressGateStatus {
		return "", fmt.Errorf("muster (%s), the agent's MCP server, from the actor: %s, wanted muster's own answer", t.muster, r)
	}
	return fmt.Sprintf("muster (%s) answered from the actor: %s", t.muster, r), nil
}

// rawTCP: no server banner by name or by address. A banner is the finding,
// named with its destination.
func (p codingProbe) rawTCP(t egressProbeTarget) (string, error) {
	var refused []string
	for _, d := range []struct{ key, host string }{{"tcp-name", t.tcpHost}, {"tcp-address", t.tcpAddress}} {
		dest := net.JoinHostPort(d.host, strconv.Itoa(t.tcpPort))
		v, ok := p[d.key]
		if !ok {
			return "", fmt.Errorf("the reply carries no %s line for %s", d.key, dest)
		}
		rc, words := readRC(v)
		if rc == "" {
			return "", fmt.Errorf("the probe's %s line for %s is %q, not `rc<exit> …`", d.key, dest, v)
		}
		if strings.Contains(words, egressTestTCPBanner) {
			return "", fmt.Errorf("raw TCP to %s, outside every egress list, passed the actor's egress: the server's banner %q came back (exit %s)", dest, excerpt(words, 80), rc)
		}
		refused = append(refused, fmt.Sprintf("%s refused (exit %s%s)", dest, rc, firstNonEmpty(prefixed(": ", words), ", nothing read")))
	}
	return strings.Join(refused, "; "), nil
}

// prefixed is s after prefix, or "" for an empty s.
func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

// gateDenial reads the egress gateway's access log for the undeclared
// host's refusal and requires the policy's reason.
func gateDenial(rawURL string) (string, error) {
	host, err := egressOriginHost(rawURL)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), egressFindingsReadLimit)
	defer cancel()
	logs, err := podLogs(ctx, substrateNamespace, "deploy/"+atenetEgressDeployment, atenetEgressContainer, skillsEvidenceLogSince)
	if err != nil {
		return "", fmt.Errorf("the log of Substrate's egress gateway (deploy/%s) could not be read: %w", atenetEgressDeployment, err)
	}
	return denialVerdict(host, egressRefusals(logs))
}

// denialVerdict finds the host's refusal among the gateway's and requires
// the policy's reason on it.
func denialVerdict(host string, refusals []egressRefusal) (string, error) {
	var seen []string
	for _, r := range slices.Backward(refusals) {
		if !strings.Contains(r.host, host) {
			continue
		}
		if strings.Contains(r.reason, egressPolicyDenied) {
			return fmt.Sprintf("the gateway's access log: %s", r), nil
		}
		seen = append(seen, r.String())
	}
	if len(seen) > 0 {
		return "", fmt.Errorf("the egress gateway refused %s for another reason than %q: %s", host, egressPolicyDenied, strings.Join(seen, "; "))
	}
	return "", fmt.Errorf("the egress gateway (deploy/%s) logged no refusal of %s: the 403 came from elsewhere", atenetEgressDeployment, host)
}

// egressOriginHost is the host of an origin or URL.
func egressOriginHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != httpScheme && u.Scheme != httpScheme+"s") {
		return "", fmt.Errorf("%q is no HTTP(S) origin", raw)
	}
	return u.Hostname(), nil
}

// lookupIPv4 is the first IPv4 address the lab host resolves the name to.
func lookupIPv4(host string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("resolving %s on the lab host for the raw TCP probe by address: %v", host, err)
	}
	return ips[0].String(), nil
}
