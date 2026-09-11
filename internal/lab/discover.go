package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// Discovery is what `agentlab configure` learns about this machine before it
// writes agentlab.yaml — on every run, not only the first, so the file
// follows the host: the one tool `agentlab up` shells out to (the container
// engine) next to what the binary embeds (kind with the node image it boots,
// Helm, the Kubernetes client), whether this
// configuration's kind cluster exists (and which host ports it publishes, so
// they never count as conflicts), the kind docker network's gateway (the
// address pods reach the host on), the model servers answering on their
// default ports with their downloaded models, a standalone FastFlowLM server
// (report-only), and whether the Anthropic key is in the environment.
type Discovery struct {
	Tools         []ToolVersion
	AnthropicKey  bool
	ClusterExists bool
	ClusterPorts  map[int]bool
	KindGateway   string
	Servers       []HostServer
	FLM           *FLMServer
}

// ToolVersion is one entry of the report's tools line: the container engine
// the lab shells out to (Version empty when it is not on PATH or does not
// answer), or one of the libraries the binary carries — Embedded — whose
// Version is the one built in (kind's names its node image too).
type ToolVersion struct {
	Name     string
	Version  string
	Embedded bool
}

// toolVersions is the Tools entry of a discovery: docker with the version it
// answered (or "" when missing), then what this build embeds.
func toolVersions(docker string) []ToolVersion {
	return []ToolVersion{
		{Name: dockerBin, Version: docker},
		{Name: kindToolName, Version: kindToolVersion(), Embedded: true},
		{Name: helmToolName, Version: helmToolVersion(), Embedded: true},
		{Name: clientGoToolName, Version: clientGoToolVersion(), Embedded: true},
	}
}

// HostServer is a model server found on this machine.
type HostServer struct {
	Backend string // config.ModelManagerBackend*
	// Ident is what the server says it is: its version where it reports one,
	// the API generation for LM Studio, which reports none anywhere.
	Ident string
	Port  int
	// Probed: the reachability question was put to a running node, so PodHost
	// is an answer. False while there was nothing to dial or no node to dial
	// from, and when the probe itself failed, which ReachErr then explains.
	Probed   bool
	ReachErr error
	// PodHost is the host part pods reach this server on: the kind network
	// gateway where that is this machine, else the container runtime's host
	// alias, which resolves only inside the cluster. Empty when Probed and
	// neither answers.
	PodHost   string
	Models    []HostModel
	ModelsErr error
}

// FLMServer is a standalone `flm serve` (FastFlowLM's own OpenAI-compatible
// server). The lab does not wire it: it has no management API and lists its
// whole catalog rather than what is downloaded — Lemonade Server is the
// supported front for FLM.
type FLMServer struct {
	Port   int
	Models int
}

// flmDefaultPort is `flm port`, the default of `flm serve`.
const flmDefaultPort = 52625

// flmOwner is the owned_by FLM's /v1/models reports.
const flmOwner = "FastFlowLM"

// Discover probes this machine. Nothing here needs the cluster; every probe
// is loopback or a local CLI and degrades to "not found".
func Discover(cfg *config.Config) *Discovery {
	d := &Discovery{AnthropicKey: os.Getenv(AnthropicKeyEnv) != ""}
	d.Tools = toolVersions(dockerVersion())
	d.ClusterExists, d.ClusterPorts = kindNodePublishedPorts(cfg.ControlPlaneNode())
	if gw, err := kindGatewayIP(cfg.ControlPlaneNode()); err == nil {
		d.KindGateway = gw
	}
	for _, b := range config.ModelManagerBackends {
		base := loopbackBase(b)
		ident, ok := detectHostServer(b, base)
		if !ok {
			continue
		}
		s := HostServer{Backend: b, Ident: ident, Port: config.BackendPort(b)}
		// Reachability needs both an address pods dial and a node to dial it
		// from. The kind network outlives `kind delete cluster`, so a gateway
		// without a node is the normal state after `agentlab down` — probing
		// then would record "unreachable" from a probe that cannot run.
		if d.KindGateway != "" && d.ClusterExists {
			host, err := podReachableHost(cfg.ControlPlaneNode(), d.KindGateway, s.Port)
			if err != nil {
				s.ReachErr = err
			} else {
				s.Probed, s.PodHost = true, host
			}
		}
		s.Models, s.ModelsErr = hostModelsFn(b, base)
		d.Servers = append(d.Servers, s)
	}
	d.FLM = detectFLM(fmt.Sprintf("http://127.0.0.1:%d", flmDefaultPort), flmDefaultPort)
	return d
}

// podReachableHost is the address pods reach this machine on for port: the
// kind network gateway where that is this machine, else the container
// runtime's host alias, which resolves only inside the cluster. "" with no
// error means neither answers; an error is the probe failing to run, which is
// never a verdict about the server.
//
// Every caller asks from inside the node, and only from there. A host-side
// dial answers a different question — on a Linux host whose firewall drops
// traffic from the bridge it succeeds while pods still cannot connect — and
// two vantage points would let the discovery report and the endpoint the
// install wires disagree.
func podReachableHost(node, gateway string, port int) (string, error) {
	if !nodeRunning(node) {
		return "", fmt.Errorf("node %q is not running", node)
	}
	addr := func(host string) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
	answers, err := nodeDialOn(node, addr(gateway))
	if err != nil {
		return "", err
	}
	if answers {
		return gateway, nil
	}
	// The gateway is not this machine (a runtime in a VM), so the runtime's
	// host alias is the only other address pods have for it. Autodetecting
	// that is what keeps the server usable without an endpoints override no
	// form asks for.
	alias := hostAlias()
	answers, err = nodeDialOn(node, addr(alias))
	switch {
	case err != nil:
		return "", err
	case answers:
		return alias, nil
	}
	return "", nil
}

// hostAlias is the name that resolves to this machine from inside the
// cluster but not on it: what a container runtime in a VM publishes, since
// the kind network gateway is then a bridge inside that VM. kindGatewayIP
// resolves the podman one for the gateway itself.
func hostAlias() string {
	if dockerIsPodman() {
		return "host.containers.internal"
	}
	return "host.docker.internal"
}

// Backends lists the backends the configuration should carry, in canonical
// order: the servers that answer AND that pods can reach.
//
// A server pods cannot reach is no use to model-manager, which runs in one.
// Reachable means on the kind gateway OR on the runtime's host alias, so a
// runtime in a VM enrolls its servers like a native one.
//
// Unknown reachability still enrolls: Probed is false while there is no node
// to probe from — a fresh machine, or after `agentlab down`, which leaves the
// kind network behind but no container to dial from — and a lab that has not
// booted yet must still be configurable. Only an explicit "no" is left out,
// and Report says so where it says the rest.
func (d *Discovery) Backends() []string {
	var out []string
	for _, s := range d.Servers {
		if s.Probed && s.PodHost == "" {
			continue
		}
		out = append(out, s.Backend)
	}
	return out
}

// PodHostFor is the host pods reach a backend's server on, "" when the
// discovery could not establish one.
func (d *Discovery) PodHostFor(backend string) string {
	for _, s := range d.Servers {
		if s.Backend == backend {
			return s.PodHost
		}
	}
	return ""
}

// ModelServersHint names the servers found for the configure form, and says
// which of them the answer cannot enrol. Listing a server the form is about
// to drop reads as a promise the configuration does not keep — the confirm
// says "Found: X" and X never reaches platform.modelManager.backends.
func (d *Discovery) ModelServersHint() string {
	parts := make([]string, 0, len(d.Servers))
	for _, s := range d.Servers {
		part := fmt.Sprintf("%s %s (:%d)", config.BackendServerName(s.Backend), s.Ident, s.Port)
		if s.Probed && s.PodHost == "" {
			part += " — pods cannot reach it, so it is not enrolled"
		}
		parts = append(parts, part)
	}
	// "; " and not ", ": the not-enrolled note contains a comma, so a
	// comma-joined list reads as one more server.
	return strings.Join(parts, "; ")
}

// toolRequirement is a CLI `agentlab up` shells out to: why the lab needs it,
// and where to get it. There is no version floor. The container engine is the
// only one: kind, Helm and the Kubernetes client are embedded (kind.go,
// helm.go, kube.go), and the Kubernetes the lab boots is the kind release's
// default node image.
type toolRequirement struct {
	name    string
	why     string
	install string
}

// toolRequirements is what Preflight checks the discovered tools against.
var toolRequirements = []toolRequirement{
	{name: dockerBin, why: "the embedded kind runs the cluster as a container of this engine, through its CLI (Podman >= 4's docker-compatible CLI works too)",
		install: "https://docs.docker.com/get-started/get-docker/"},
}

// Preflight is the verdict on the tools, taken before `agentlab configure`
// asks its first question (or, with --defaults, writes anything): an error
// naming every tool `agentlab up` would fail on — not on PATH — with why and
// where to get it. So nobody walks through the whole form to be refused at
// boot time. The container engine is the one tool asked for; everything else
// the lab runs is in the binary.
func (d *Discovery) Preflight() error {
	var problems []string
	for _, req := range toolRequirements {
		if d.toolVersion(req.name) != "" {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s is not on PATH (or does not answer) — %s\n    install: %s", req.name, req.why, req.install))
	}
	if len(problems) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("this machine cannot run the lab yet:\n")
	for _, p := range problems {
		b.WriteString("  - " + p + "\n")
	}
	b.WriteString("Install the above and run the command again.")
	return errors.New(b.String())
}

// Report is the human-readable account of the discovery, printed by
// `agentlab configure` before it changes anything. The tools line states
// what answered on PATH and what the binary carries; the verdict on the
// former is Preflight's.
func (d *Discovery) Report(cfg *config.Config) string {
	var b strings.Builder
	line := func(label, format string, a ...any) {
		fmt.Fprintf(&b, "  %-18s"+format+"\n", append([]any{label}, a...)...)
	}
	b.WriteString("Discovering this machine:\n")
	var onPath, embedded []string
	for _, t := range d.Tools {
		switch {
		case t.Embedded:
			embedded = append(embedded, strings.TrimSpace(t.Name+" "+t.Version))
		case t.Version == "":
			onPath = append(onPath, t.Name+" MISSING")
		default:
			onPath = append(onPath, t.Name+" "+t.Version)
		}
	}
	line("tools", "%s — embedded: %s", strings.Join(onPath, ", "), strings.Join(embedded, ", "))
	switch {
	case d.ClusterExists:
		line("cluster", "kind %q exists — its port mappings are fixed at node creation (`agentlab down && agentlab up` to change them)", cfg.ClusterName)
	default:
		line("cluster", "kind %q does not exist yet — ports are free to move", cfg.ClusterName)
	}
	if len(d.Servers) == 0 {
		line("model servers", "none — %s", strings.Join(noServersFound(), ", "))
	}
	for _, s := range d.Servers {
		// The address pods dial is only knowable from a running node, and the
		// kind network outlives `kind delete cluster` — so a known gateway
		// with no node is the state after `agentlab down`, and saying the
		// network is missing would deny what is there.
		reach := "the address pods dial is not known yet (`agentlab up` creates the cluster)"
		if d.KindGateway != "" && !d.ClusterExists {
			reach = "the address pods dial is not known while the node is not running (`agentlab up` starts it)"
		}
		switch {
		case s.ReachErr != nil:
			// Not a verdict on the server: the probe itself did not run, so
			// the bind hint would send the user to fix the wrong thing.
			reach = fmt.Sprintf("cannot tell whether pods reach it on %s (%v)", d.KindGateway, s.ReachErr)
		case s.Probed && s.PodHost == d.KindGateway:
			reach = fmt.Sprintf("answers on %s (the address pods dial): yes", d.KindGateway)
		case s.Probed && s.PodHost != "":
			// The gateway is a bridge inside the runtime's VM, so it is not
			// an address of this machine and no bind setting makes the server
			// answer there; the alias is, from inside the cluster. Nothing to
			// fix, so the line reports the address instead of a remedy.
			reach = fmt.Sprintf("pods reach it at %s (%s is inside the container runtime's VM, not this machine)",
				s.PodHost, d.KindGateway)
		case s.Probed:
			reach = fmt.Sprintf("does NOT answer on %s (the address pods dial) — left out of platform.modelManager.backends until it does (%s)",
				d.KindGateway, bindHint(s.Backend))
		}
		models := "models not listed"
		if s.ModelsErr == nil {
			tools := 0
			for _, m := range s.Models {
				if m.Tools {
					tools++
				}
			}
			models = fmt.Sprintf("%d downloaded, %d tool-calling", len(s.Models), tools)
		}
		line(config.BackendServerName(s.Backend), "%s on :%d — %s; %s", s.Ident, s.Port, reach, models)
	}
	if d.FLM != nil {
		line("FastFlowLM", "standalone `flm serve` on :%d (%d catalog entries) — no management API and loopback by default; the lab drives FLM through Lemonade Server", d.FLM.Port, d.FLM.Models)
	}
	if d.AnthropicKey {
		line("Anthropic key", "$%s is set — the agents' default ModelConfig and Backstage's AI chat get the real key at deploy time", AnthropicKeyEnv)
	} else {
		line("Anthropic key", "$%s is not set — the default ModelConfig and Backstage's AI chat get a placeholder until it is exported and `agentlab platform` re-runs", AnthropicKeyEnv)
	}
	return b.String()
}

func (d *Discovery) toolVersion(name string) string {
	for _, t := range d.Tools {
		if t.Name == name {
			return t.Version
		}
	}
	return ""
}

// bindHint is the one-line version of the bind fix for the report.
func bindHint(backend string) string {
	if spec, known := backendSpec(backend); known {
		return spec.bindHint
	}
	return ""
}

// noServersFound names every server the discovery looked for, so the "none"
// line follows the backend table instead of a hand-kept sentence.
func noServersFound() []string {
	out := make([]string, 0, len(config.ModelManagerBackends))
	for _, b := range config.ModelManagerBackends {
		out = append(out, fmt.Sprintf("no %s on :%d", config.BackendServerName(b), config.BackendPort(b)))
	}
	return out
}

// nodeDialTimeout bounds the node-side dial, the podman counterpart of
// tcpAnswers' own timeout. Longer, because it pays for `docker exec` too.
const nodeDialTimeout = 2 * time.Second

// nodeDial dials addr from inside the node, which is the only vantage point
// that can answer for an address the host cannot resolve (the runtime's host
// alias). An error is the probe itself failing, never a verdict.
//
// The node has to be running, and that is checked rather than inferred:
// `docker exec` into a container that is not there exits 1, exactly as bash
// does on a refused dial, so without this a missing node would read as
// "nothing is listening".
func nodeDial(node, addr string) (bool, error) {
	if !nodeRunning(node) {
		return false, fmt.Errorf("dialing %s: node %q is not running", addr, node)
	}
	return nodeDialOn(node, addr)
}

// nodeDialOn is nodeDial for a caller that has already established the node is
// running, so a resolution does not pay one `docker inspect` per dial.
func nodeDialOn(node, addr string) (bool, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("probing %s: %w", addr, err)
	}
	_, err = outputQuiet("docker", "exec", node,
		"timeout", strconv.Itoa(int(nodeDialTimeout.Seconds())), "bash", "-c",
		fmt.Sprintf("exec 3<>/dev/tcp/%s/%s", host, port))
	if err == nil {
		return true, nil
	}
	// bash exits 1 when the dial is refused and `timeout` exits 124 when it
	// hangs: both are answers about the server. Every other code is the
	// probe failing (no bash or no timeout in the node, docker exec itself),
	// which says nothing about the server.
	switch exitCode(err) {
	case 1, 124:
		return false, nil
	default:
		return false, fmt.Errorf("dialing %s from node %q: %w", addr, node, err)
	}
}

// nodeRunning reports whether the node container is up, the precondition of
// every probe that dials from inside it.
func nodeRunning(node string) bool {
	out, err := outputQuiet("docker", "inspect", "-f", "{{.State.Running}}", node)
	return err == nil && strings.TrimSpace(out) == "true"
}

// exitCode digs the process exit status out of a wrapped command error; -1
// when the command did not run or did not exit normally.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// detectFLM probes a standalone FastFlowLM server: its OpenAI-compatible
// /v1/models lists entries owned by FastFlowLM.
func detectFLM(base string, port int) *FLMServer {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/v1/models")
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Data []struct {
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := decodeJSONBody(resp, &list); err != nil || len(list.Data) == 0 {
		return nil
	}
	for _, m := range list.Data {
		if m.OwnedBy != flmOwner {
			return nil
		}
	}
	return &FLMServer{Port: port, Models: len(list.Data)}
}

// kindNodePublishedPorts reads the host ports the kind node container of
// this configuration publishes (docker's HostConfig.PortBindings — set at
// creation, present whether the container runs or is stopped). exists is
// false when there is no such container (or no docker).
func kindNodePublishedPorts(node string) (exists bool, ports map[int]bool) {
	out, err := outputQuiet("docker", "inspect", "-f", "{{json .HostConfig.PortBindings}}", node)
	if err != nil {
		return false, nil
	}
	var bindings map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &bindings); err != nil {
		return true, nil
	}
	ports = map[int]bool{}
	for _, list := range bindings {
		for _, b := range list {
			if p, err := strconv.Atoi(b.HostPort); err == nil {
				ports[p] = true
			}
		}
	}
	return true, ports
}

// dockerVersion is the engine version, naming podman when its
// docker-compatible CLI is what answers (runtime.go).
func dockerVersion() string {
	out, err := outputQuiet("docker", "version", "-f", "{{.Server.Version}}")
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(out)
	if dockerIsPodman() {
		v += " (podman)"
	}
	return v
}
