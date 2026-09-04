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
// umbrella's `model-manager` dependency in front of the model servers that
// run on the lab host — an Ollama (backend ollama), a Lemonade Server
// (backend lemonade: FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on GPU/CPU).
// model-manager fronts ONE backend per instance today, the first of
// platform.modelManager.backends; the further ones are wired statically
// (hostmodels.go) until it is multi-backend. Pods reach the host only through
// the kind docker network's gateway — the same address the README documents
// for extraModels — so every endpoint is detected from `docker network
// inspect kind` plus the server's default port rather than asked for.
// Everything that can go wrong is host-side plumbing (bind address,
// firewall), which preflightHostServer turns into an early, actionable
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

// The version-answering paths of the servers: Ollama's whole document is
// {"version":"..."}, Lemonade's health document carries a version field.
const (
	ollamaVersionPath   = "/api/version"
	lemonadeHealthPath  = "/api/v1/health"
	lemonadeModelsPath  = "/api/v1/models"
	lemonadeAPIBasePath = "/api/v1"
)

// versionFieldRe matches the "version":"..." field both servers answer with.
var versionFieldRe = regexp.MustCompile(`"version"\s*:\s*"[^"]*"`)

// healthPath is the version-answering path of a backend's server.
func healthPath(backend string) string {
	if backend == config.ModelManagerBackendLemonade {
		return lemonadeHealthPath
	}
	return ollamaVersionPath
}

// detectHostServer asks a model server at base for its version — the
// configure-time question "is there one on this machine at all?" —
// reachability from pods (bind address, firewall) is preflightHostServer's
// job at platform time.
func detectHostServer(backend, base string) (version string, ok bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + healthPath(backend))
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
	if err := decodeJSONBody(resp, &v); err != nil || v.Version == "" {
		return "", false
	}
	return v.Version, true
}

// loopbackBase is where a server on this machine answers on its default port.
func loopbackBase(backend string) string {
	return fmt.Sprintf("http://127.0.0.1:%d", config.BackendPort(backend))
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

// resolveBackendEndpoint is the URL model-manager (or an agent pod) dials for
// a backend: the configured override, else http://<kind gateway>:<the
// server's default port>. The kind network exists once the cluster does, so
// callers that render before a boot get an error they may tolerate (render)
// or must not (platform).
func resolveBackendEndpoint(cfg *config.Config, backend string) (string, error) {
	if ep := cfg.Platform.ModelManager.EndpointFor(backend); ep != "" {
		return strings.TrimSuffix(ep, "/"), nil
	}
	gw, err := kindGatewayIP()
	if err != nil {
		return "", fmt.Errorf("autodetecting the %s endpoint: %w (set platform.modelManager.endpoints.%s to skip the detection)",
			config.BackendServerName(backend), err, backend)
	}
	return fmt.Sprintf("http://%s:%d", gw, config.BackendPort(backend)), nil
}

// resolveBackendEndpoints resolves every configured backend's endpoint.
func resolveBackendEndpoints(cfg *config.Config) (map[string]string, error) {
	endpoints := map[string]string{}
	for _, b := range cfg.Platform.ModelManager.Backends {
		ep, err := resolveBackendEndpoint(cfg, b)
		if err != nil {
			return nil, err
		}
		endpoints[b] = ep
	}
	return endpoints, nil
}

// bindFix is the host-side fix for a server listening on loopback only.
func bindFix(backend string) string {
	if backend == config.ModelManagerBackendLemonade {
		return "  - bind Lemonade to every interface, not loopback: `lemonade config set host=0.0.0.0`\n" +
			"    (host in ~/.config/lemonade/config.json), then restart lemond"
	}
	return "  - bind Ollama to every interface, not loopback: OLLAMA_HOST=0.0.0.0 (systemd: an\n" +
		"    Environment= drop-in on ollama.service), then restart it"
}

// preflightHostServer proves a host model server answers from INSIDE the
// cluster before the platform install waits ten minutes on a model-manager
// whose backend is unreachable (or wires ModelConfigs to a dead endpoint). A
// short-lived pod fetches the server's version document; the two known
// host-side failures are spelled out with their fixes: the server bound to
// 127.0.0.1 (connection refused from the bridge) and a host firewall
// dropping pod->host traffic on the docker bridge (timeout).
func preflightHostServer(cfg *config.Config, backend, endpoint string) error {
	server := config.BackendServerName(backend)
	// The probe image goes host cache -> node like every lab image, so the
	// pod never waits on an in-node pull (best-effort: a miss falls back to
	// the kubelet's pull under the pod-running timeout).
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	pod := backend + "-preflight"
	// A leftover from an interrupted run would make `kubectl run` refuse.
	_ = runQuiet("kubectl", "-n", platformNamespace, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	out, err := outputAll("kubectl", "-n", platformNamespace, "run", pod,
		"--rm", "-i", "--quiet", "--restart=Never", "--pod-running-timeout=120s",
		"--image="+probeImage, "--image-pull-policy=IfNotPresent",
		"--command", "--", "wget", "-qO-", "-T", "5", endpoint+healthPath(backend))
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
	fixes := bindFix(backend) + "\n" +
		"  - allow TCP " + fmt.Sprint(config.BackendPort(backend)) + " from the docker bridge subnets (they fall inside\n" +
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
		"  Then re-run `agentlab platform`, or drop %s from platform.modelManager.backends\n"+
		"  (`agentlab configure --defaults` rewrites the list from what answers on this machine).\n"+
		"  Probe output: %.300s", server, endpoint, reason, fixes, backend, out)
}

// modelManagerHint is the platform-up summary for managed models: the one
// model-manager and every host server it fronts, the first being its default
// backend.
func modelManagerHint(cfg *config.Config, endpoints map[string]string) string {
	if !cfg.ModelManagerEnabled() {
		return "  Model manager is disabled (platform.modelManager in agentlab.yaml)."
	}
	mm := cfg.Platform.ModelManager
	parts := make([]string, 0, len(mm.Backends))
	for _, b := range mm.Backends {
		parts = append(parts, fmt.Sprintf("%s (%s) at %s", b, config.BackendServerName(b), endpoints[b]))
	}
	return fmt.Sprintf("  Model manager: one instance fronting %s — default backend %s;\n"+
		"  REST %s/api/v1 (Dex token required; ?backend= / \"backend\" name a server), MCP tools\n"+
		"  x_model-manager_* through muster; the portal's Models tab manages the same models.",
		strings.Join(parts, ", "), mm.Primary(), cfg.ModelManagerBaseURL())
}
