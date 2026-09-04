package lab

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// Discovery is what `agentlab configure` learns about this machine before it
// writes agentlab.yaml — on every run, not only the first, so the file
// follows the host: the tools `agentlab up` shells out to, whether this
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

// ToolVersion is one of the CLIs the lab shells out to; Version is empty
// when the tool is not on PATH (or does not answer).
type ToolVersion struct {
	Name    string
	Version string
}

// HostServer is a model server found on this machine.
type HostServer struct {
	Backend string // config.ModelManagerBackend*
	Version string
	Port    int
	// OnGateway: the server also answers on the kind docker gateway address,
	// i.e. it is bound to every interface and pods can reach it. nil when
	// the kind network does not exist yet (nothing to dial).
	OnGateway *bool
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
	d.Tools = []ToolVersion{
		{"docker", dockerVersion()},
		{"kind", kindVersion()},
		{"kubectl", kubectlVersion()},
		{"helm", helmVersion()},
	}
	d.ClusterExists, d.ClusterPorts = kindNodePublishedPorts(cfg.ControlPlaneNode())
	if gw, err := kindGatewayIP(); err == nil {
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
			answers := tcpAnswers(net.JoinHostPort(d.KindGateway, strconv.Itoa(s.Port)))
			s.OnGateway = &answers
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

// MissingTools names the CLIs `agentlab up` needs that did not answer.
func (d *Discovery) MissingTools() []string {
	var missing []string
	for _, t := range d.Tools {
		if t.Version == "" {
			missing = append(missing, t.Name)
		}
	}
	return missing
}

// Report is the human-readable account of the discovery, printed by
// `agentlab configure` before it changes anything.
func (d *Discovery) Report(cfg *config.Config) string {
	var b strings.Builder
	line := func(label, format string, a ...any) {
		fmt.Fprintf(&b, "  %-18s"+format+"\n", append([]any{label}, a...)...)
	}
	b.WriteString("Discovering this machine:\n")
	tools := make([]string, 0, len(d.Tools))
	for _, t := range d.Tools {
		if t.Version == "" {
			tools = append(tools, t.Name+" MISSING")
			continue
		}
		tools = append(tools, t.Name+" "+t.Version)
	}
	line("tools", "%s", strings.Join(tools, ", "))
	if missing := d.MissingTools(); len(missing) > 0 {
		line("", "`agentlab up` needs docker, kind, kubectl and helm >= 4 — install: %s", strings.Join(missing, ", "))
	} else if v := d.toolVersion("helm"); v != "" {
		if major, err := helmMajor(v); err == nil && major < 4 {
			line("", "helm %s cannot install the platform: Helm >= 4 is required (agent-platform-standalone#21)", v)
		}
	}
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
		if s.OnGateway != nil && *s.OnGateway {
			reach = fmt.Sprintf("listens on the kind gateway %s: yes", d.KindGateway)
		} else if s.OnGateway != nil {
			reach = fmt.Sprintf("does NOT listen on the kind gateway %s — pods cannot reach it (%s)", d.KindGateway, bindHint(s.Backend))
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

func dockerVersion() string {
	out, err := outputQuiet("docker", "version", "-f", "{{.Server.Version}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func kindVersion() string {
	// "kind v0.32.0 go1.26.4 linux/amd64"
	out, err := outputQuiet("kind", "version")
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return strings.TrimSpace(out)
	}
	return fields[1]
}

func kubectlVersion() string {
	out, err := outputQuiet("kubectl", "version", "--client", "-o", "json")
	if err != nil {
		return ""
	}
	var v struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return ""
	}
	return v.ClientVersion.GitVersion
}

func helmVersion() string {
	out, err := outputQuiet("helm", "version", "--template", "{{.Version}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
