package lab

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The model-manager component (platform.modelManager in agentlab.yaml): the
// umbrella's `model-manager` dependency in front of the model server that runs
// on the lab host — an Ollama (backend ollama), or a Lemonade Server (backend
// lemonade: FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on GPU/CPU). Pods reach
// the host only through the kind docker network's gateway — the same address
// the README documents for extraModels — so the endpoint is detected from
// `docker network inspect kind` and the backend's default port rather than
// asked for. Everything that can go wrong is host-side plumbing (bind address,
// firewall), which preflightModelServer turns into an early, actionable
// failure instead of a model-manager pod that reports an unhealthy backend
// after a ten-minute install.

// modelManagerMCPServer is the MCPServer CR name the model-manager chart
// registers with muster (model-manager.muster.mcpServer.name, the chart
// default); muster prefixes its tools with it: x_model-manager_<tool>.
const modelManagerMCPServer = "model-manager"

// kindDockerNetwork is the docker network kind creates its nodes on.
const kindDockerNetwork = "kind"

// probeImage runs the in-cluster reachability probe: busybox wget in the
// alpine image the umbrella already ships elsewhere (small, present in the
// host cache after the first run, side-loaded before the probe pod).
const probeImage = "gsoci.azurecr.io/giantswarm/alpine:3.22.1"

// versionFieldRe matches the "version":"..." field both servers answer with —
// Ollama's whole /api/version document, one field of Lemonade's /api/v1/health.
var versionFieldRe = regexp.MustCompile(`"version"\s*:\s*"[^"]*"`)

// DetectHostOllama probes the host's own Ollama on loopback and reports its
// version. It answers the configure-time question "is there an Ollama on
// this machine at all?" — reachability from pods (bind address, firewall) is
// preflightModelServer's job at platform time.
func DetectHostOllama() (version string, ok bool) {
	return detectVersion(fmt.Sprintf("http://127.0.0.1:%d/api/version", config.OllamaPort))
}

// DetectHostLemonade probes the host's own Lemonade Server on loopback
// (GET /api/v1/health) and reports its version — the configure-time question
// for the lemonade backend.
func DetectHostLemonade() (version string, ok bool) {
	return detectVersion(fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", config.LemonadePort))
}

// detectVersion fetches a JSON document with a "version" field.
func detectVersion(url string) (string, bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := decodeJSONBody(resp, &v); err != nil {
		return "", false
	}
	return v.Version, true
}

// kindGatewayIP returns the IPv4 gateway of the kind docker network — the
// address pods use to reach services on the host.
func kindGatewayIP() (string, error) {
	out, err := outputQuiet("docker", "network", "inspect", kindDockerNetwork,
		"-f", `{{range .IPAM.Config}}{{.Gateway}}{{"\n"}}{{end}}`)
	if err != nil {
		return "", fmt.Errorf("docker network %q not found (the kind cluster creates it): %w", kindDockerNetwork, err)
	}
	for _, line := range strings.Split(out, "\n") {
		ip := net.ParseIP(strings.TrimSpace(line))
		if ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("docker network %q has no IPv4 gateway", kindDockerNetwork)
}

// resolveModelManagerEndpoint is the endpoint model-manager dials: the
// configured override, else http://<kind gateway>:<the backend's default port>
// (11434 for Ollama, 13305 for Lemonade). The kind network exists once the
// cluster does, so callers that render before a boot get an error they may
// tolerate (render) or must not (platform).
func resolveModelManagerEndpoint(cfg *config.Config) (string, error) {
	if ep := cfg.Platform.ModelManager.Endpoint; ep != "" {
		return strings.TrimSuffix(ep, "/"), nil
	}
	gw, err := kindGatewayIP()
	if err != nil {
		return "", fmt.Errorf("autodetecting the %s endpoint: %w (set platform.modelManager.endpoint to skip the detection)", cfg.Platform.ModelManager.ServerName(), err)
	}
	return fmt.Sprintf("http://%s:%d", gw, cfg.Platform.ModelManager.Port()), nil
}

// healthPath is the version-answering path of the configured backend's
// server: Ollama's /api/version, Lemonade's /api/v1/health.
func healthPath(cfg *config.Config) string {
	if cfg.Platform.ModelManager.Backend == config.ModelManagerBackendLemonade {
		return "/api/v1/health"
	}
	return "/api/version"
}

// bindFix is the host-side fix for a server listening on loopback only.
func bindFix(cfg *config.Config) string {
	if cfg.Platform.ModelManager.Backend == config.ModelManagerBackendLemonade {
		return "  - bind Lemonade to every interface, not loopback: `lemonade config set host=0.0.0.0`\n" +
			"    (host in ~/.config/lemonade/config.json), then restart lemond"
	}
	return "  - bind Ollama to every interface, not loopback: OLLAMA_HOST=0.0.0.0 (systemd: an\n" +
		"    Environment= drop-in on ollama.service), then restart it"
}

// preflightModelServer proves the host model server answers from INSIDE the
// cluster before the platform install waits ten minutes on a model-manager
// whose backend is unreachable. A short-lived pod fetches the server's
// version document; the two known host-side failures are spelled out with
// their fixes: the server bound to 127.0.0.1 (connection refused from the
// bridge) and a host firewall dropping pod->host traffic on the docker bridge
// (timeout).
func preflightModelServer(cfg *config.Config, endpoint string) error {
	server := cfg.Platform.ModelManager.ServerName()
	// The probe image goes host cache -> node like every lab image, so the
	// pod never waits on an in-node pull (best-effort: a miss falls back to
	// the kubelet's pull under the pod-running timeout).
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	const pod = "model-server-preflight"
	// A leftover from an interrupted run would make `kubectl run` refuse.
	_ = runQuiet("kubectl", "-n", platformNamespace, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	out, err := outputAll("kubectl", "-n", platformNamespace, "run", pod,
		"--rm", "-i", "--quiet", "--restart=Never", "--pod-running-timeout=120s",
		"--image="+probeImage, "--image-pull-policy=IfNotPresent",
		"--command", "--", "wget", "-qO-", "-T", "5", endpoint+healthPath(cfg))
	if err == nil && strings.Contains(out, `"version"`) {
		// The combined output also carries kubectl's own attach chatter
		// ("warning: couldn't attach to pod ..., falling back to logs"), so
		// pick the version field out of it.
		version := strings.TrimSpace(out)
		if m := versionFieldRe.FindString(out); m != "" {
			version = m
		}
		note("host %s answers from inside the cluster: %s", server, version)
		return nil
	}
	out = strings.TrimSpace(out)
	reason := "the probe pod could not fetch it"
	fixes := bindFix(cfg) + "\n" +
		"  - allow TCP " + fmt.Sprint(cfg.Platform.ModelManager.Port()) + " from the docker bridge subnets (they fall inside\n" +
		"    172.16.0.0/12) through the host firewall — pod->host traffic arrives on the\n" +
		"    bridge like any other inbound connection"
	switch {
	case strings.Contains(out, "refused"):
		reason = fmt.Sprintf("connection refused — %s is not listening on the bridge address (the usual\n  cause: it is bound to 127.0.0.1)", server)
	case strings.Contains(out, "timed out"), strings.Contains(out, "timeout"):
		reason = "connection timed out — the host firewall drops pod->host traffic on the docker\n  bridge (the request never reaches the server)"
	}
	return fmt.Errorf("host %s is not reachable from pods at %s: %s.\n"+
		"  Fixes (README, \"Local backends on the lab host\"):\n%s\n"+
		"  Then re-run `agentlab platform`, or set platform.modelManager.enabled: false.\n"+
		"  Probe output: %.300s", server, endpoint, reason, fixes, out)
}

// modelManagerHint is the platform-up summary line for the component.
func modelManagerHint(cfg *config.Config, endpoint string) string {
	if !cfg.ModelManagerEnabled() {
		return "  Model manager is disabled (platform.modelManager in agentlab.yaml)."
	}
	return fmt.Sprintf("  Model manager: %s backend (%s) at %s; REST %s/api/v1 (Dex token required),\n"+
		"  MCP tools x_model-manager_* through muster; the portal's Models tab manages the same models.",
		cfg.Platform.ModelManager.Backend, cfg.Platform.ModelManager.ServerName(), endpoint, cfg.ModelManagerBaseURL())
}
