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
	Version string
	Port    int
	// OnGateway: the server also answers on the address pods dial, i.e. it is
	// bound to every interface and pods can reach it. nil when that address
	// is not known yet (nothing to dial) or the probe could not run, which
	// ReachErr then explains.
	OnGateway *bool
	ReachErr  error
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
		version, ok := detectHostServer(b, base)
		if !ok {
			continue
		}
		s := HostServer{Backend: b, Version: version, Port: config.BackendPort(b)}
		if d.KindGateway != "" {
			answers, err := hostServerAnswers(cfg.ControlPlaneNode(), net.JoinHostPort(d.KindGateway, strconv.Itoa(s.Port)))
			if err != nil {
				s.ReachErr = err
			} else {
				s.OnGateway = &answers
			}
		}
		s.Models, s.ModelsErr = hostModelsFn(b, base)
		d.Servers = append(d.Servers, s)
	}
	d.FLM = detectFLM(fmt.Sprintf("http://127.0.0.1:%d", flmDefaultPort), flmDefaultPort)
	return d
}

// Backends lists the backends whose servers answer, in canonical order.
func (d *Discovery) Backends() []string {
	var out []string
	for _, s := range d.Servers {
		out = append(out, s.Backend)
	}
	return out
}

// ModelServersHint names the servers found for the configure form.
func (d *Discovery) ModelServersHint() string {
	parts := make([]string, 0, len(d.Servers))
	for _, s := range d.Servers {
		parts = append(parts, fmt.Sprintf("%s %s (:%d)", config.BackendServerName(s.Backend), s.Version, s.Port))
	}
	return strings.Join(parts, ", ")
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
		line("model servers", "none — no Ollama on :%d, no Lemonade Server on :%d", config.OllamaPort, config.LemonadePort)
	}
	for _, s := range d.Servers {
		reach := "kind gateway not known yet (first `agentlab up` creates the network)"
		if dockerIsPodman() {
			reach = "the address pods dial is not known while the node is not running (`agentlab up` starts it)"
		}
		switch {
		case s.ReachErr != nil:
			// Not a verdict on the server: the probe itself did not run, so
			// the bind hint would send the user to fix the wrong thing.
			reach = fmt.Sprintf("cannot tell whether pods reach it on %s (%v)", d.KindGateway, s.ReachErr)
		case s.OnGateway != nil && *s.OnGateway:
			reach = fmt.Sprintf("answers on %s (the address pods dial): yes", d.KindGateway)
		case s.OnGateway != nil:
			reach = fmt.Sprintf("does NOT answer on %s (the address pods dial) — pods cannot reach it (%s)", d.KindGateway, bindHint(s.Backend))
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
		line(config.BackendServerName(s.Backend), "%s on :%d — %s; %s", s.Version, s.Port, reach, models)
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
	if backend == config.ModelManagerBackendLemonade {
		return "`lemonade config set host=0.0.0.0`, restart lemond"
	}
	return "OLLAMA_HOST=0.0.0.0, restart Ollama"
}

// nodeDialTimeout bounds the node-side dial, the podman counterpart of
// tcpAnswers' own timeout. Longer, because it pays for `docker exec` too.
const nodeDialTimeout = 2 * time.Second

// hostServerAnswers is tcpAnswers from where it matters: under podman the
// host cannot dial host.containers.internal itself, so the node dials it. A
// non-nil error means the probe did not run — never that the server is
// unreachable.
func hostServerAnswers(node, addr string) (bool, error) {
	if !dockerIsPodman() {
		return tcpAnswers(addr), nil
	}
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

// exitCode digs the process exit status out of a wrapped command error; -1
// when the command did not run or did not exit normally.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// tcpAnswers reports whether something accepts a TCP connection at addr.
func tcpAnswers(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
