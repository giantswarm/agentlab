package lab

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
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
//  2. One turn whose bash tool runs the probe. Each stage is judged from the
//     egress gateway's record of the turn's actor (egressrecord.go), never
//     from the agent's reply, which a model can word as it likes; the reply's
//     lines are only shown beside the verdict:
//     - the declared origin's request forwarded by the gateway;
//     - the undeclared HTTPS host's request answered 403 by the gateway itself,
//     its reason the actor egress policy's denial, none forwarded;
//     - the probe's GET to muster, the agent's MCP server, forwarded with no
//     egress entry for it;
//     - a raw TCP connection to SSH on a public git host, by name and by
//     address, held at the gate: the gateway's tunnel to the address and no
//     upstream connection to it; an upstream connection fails the proof,
//     naming the destination.
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
	// egressGateStatus and egressPolicyDenied are the gate's answer to an
	// origin outside the actor's policy, and its reason in the access log.
	egressGateStatus   = "403"
	egressPolicyDenied = "actor egress policy denied"
	// egressMusterStage is the muster stage's name and its probe line's key.
	egressMusterStage = "muster"
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
	turnStarted := time.Now()
	reply, session, err := firstSessionTurnAs(p.cfg, egressTestAgent, token, egressProbePrompt(target))
	if err != nil {
		return err
	}
	logs, err := readEgressGatewayLog(time.Since(turnStarted) + time.Minute)
	if err != nil {
		return err
	}
	record := readEgressGatewayRecord(logs, substrateActorName(session))
	note("the egress gateway's record of actor %s: %d request(s), %d tunnel(s)", record.actor, len(record.requests), len(record.tunnels))
	return judgeEgress(p.stages, record, parseCodingProbe(reply), target)
}

// judgeEgress runs the verdict stages on the gateway's record; the reply's
// lines for each stage (its own name's when it names none) are shown beside
// the verdict, never judged.
func judgeEgress(stages *proofStages, record egressGatewayRecord, reply codingProbe, target egressProbeTarget) error {
	for _, s := range []struct {
		name, claim string
		replyKeys   []string
		verdict     func(egressGatewayRecord, egressProbeTarget) (string, error)
	}{
		{"declared", fmt.Sprintf("%s, the Agent's declared origin, reached from the actor", target.origin), nil, egressGatewayRecord.declared},
		{"undeclared", fmt.Sprintf("%s, outside the Agent's egress, answered %s by the egress gate: %q", target.undeclared, egressGateStatus, egressPolicyDenied), nil, egressGatewayRecord.undeclared},
		{egressMusterStage, fmt.Sprintf("muster (%s), the agent's MCP server, answers from the actor with no egress entry for it", target.muster), nil, egressGatewayRecord.muster},
		{"raw tcp", fmt.Sprintf("A raw TCP connection to %s:%d (and %s:%d), outside every list, held at the gate", target.tcpHost, target.tcpPort, target.tcpAddress, target.tcpPort), []string{"tcp-name", "tcp-address"}, egressGatewayRecord.rawTCP},
	} {
		stages.begin(s.name, "%s", s.claim)
		keys := s.replyKeys
		if keys == nil {
			keys = []string{s.name}
		}
		for _, key := range keys {
			note("the reply's %s line, not evidence: %s", key, firstNonEmpty(reply[key], "absent"))
		}
		verdict, err := s.verdict(record, target)
		if err != nil {
			return err
		}
		note("%s", verdict)
	}
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
