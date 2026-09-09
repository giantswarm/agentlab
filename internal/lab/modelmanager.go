package lab

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The model-manager component (platform.modelManager in agentlab.yaml): the
// umbrella's `model-manager` dependency in front of the model servers that
// run on the lab host — an Ollama (backend ollama), a Lemonade Server
// (backend lemonade: FastFlowLM on AMD Ryzen AI NPUs, llama.cpp on GPU/CPU),
// an LM Studio (backend lmstudio: llama.cpp on GPU/CPU, MLX on Apple
// silicon). One model-manager fronts every backend of
// platform.modelManager.backends, the first being its default. Pods reach the
// host only through the kind docker network's gateway — the same address
// docs/models.md documents for extraModels — so every endpoint is detected
// from `docker network inspect kind` plus the server's default port rather
// than asked for. Everything that can go wrong is host-side plumbing (bind
// address, firewall), which preflightHostServer turns into an early,
// actionable failure instead of a model-manager pod that reports an unhealthy
// backend after a ten-minute install.
//
// Which server answers is decided by the SHAPE of its answer, never by a
// status code (backends.go): LM Studio answers HTTP 200 with an
// {"error": ...} document for every path outside its /api/v1, Ollama's
// /api/version among them.

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

// probePodTimeout bounds the reachability probe: the pod scheduled, its image
// present (side-loaded first), wget's own per-read timeout inside.
const probePodTimeout = 120 * time.Second

// Lemonade's own API paths.
const (
	lemonadeHealthPath  = "/api/v1/health"
	lemonadeModelsPath  = "/api/v1/models"
	lemonadeAPIBasePath = "/api/v1"
)

// maxProbeBody bounds the identity probe's read: an inventory grows with the
// host's library, and the whole document has to parse to prove its shape. A
// document that hits the bound is an error, not a short read — truncated JSON
// would fail to parse and read as "no such server".
const maxProbeBody = 8 << 20

// probeTimeout is the identity probe's budget. It matches the inventory
// readers in hostmodels.go on purpose: LM Studio's identifying document IS
// its library, so a machine with a large one needs the same room here. Two
// seconds silently dropped such a server from the discovery, and
// ApplyDiscovered then rewrote platform.modelManager.backends without it.
const probeTimeout = 10 * time.Second

// detectHostServer asks a model server at base to identify itself — the
// configure-time question "is there one on this machine at all?" —
// reachability from pods (bind address, firewall) is preflightHostServer's
// job at platform time. A 200 is necessary and never sufficient: what the
// body looks like decides (backends.go).
func detectHostServer(backend, base string) (ident string, ok bool) {
	spec, known := backendSpec(backend)
	if !known {
		return "", false
	}
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get(base + spec.probe.path)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
	if err != nil || len(body) > maxProbeBody {
		return "", false
	}
	return spec.probe.ident(body)
}

// loopbackBaseFn resolves a backend's loopback base; a variable so tests can
// point the host-side fallback at a stand-in server.
var loopbackBaseFn = loopbackBase

// loopbackBase is where a server on this machine answers on its default port.
func loopbackBase(backend string) string {
	return fmt.Sprintf("http://127.0.0.1:%d", config.BackendPort(backend))
}

// kindGatewayIP returns the address pods dial to reach services on the host:
// the IPv4 gateway of the kind docker network. Under rootless podman the
// bridge gateway is not the host (pasta routes host traffic through
// host.containers.internal, 169.254.1.2, and nothing answers on the
// gateway), so there the address is what the node resolves that name to —
// which needs the node, like the network needs the cluster.
func kindGatewayIP(node string) (string, error) {
	if dockerIsPodman() {
		out, err := outputQuiet("docker", "exec", node, "getent", "hosts", "host.containers.internal")
		if err != nil {
			return "", fmt.Errorf("node %q cannot resolve host.containers.internal (the node must be running, and podman writes the name into its /etc/hosts): %w", node, err)
		}
		if ip := firstIPv4(out); ip != "" {
			return ip, nil
		}
		return "", fmt.Errorf("node %q resolves host.containers.internal to no IPv4 address", node)
	}
	out, err := outputQuiet("docker", "network", "inspect", kindDockerNetwork,
		"-f", `{{range .IPAM.Config}}{{.Gateway}}{{"\n"}}{{end}}`)
	if err != nil {
		return "", fmt.Errorf("docker network %q not found (the kind cluster creates it): %w", kindDockerNetwork, err)
	}
	if ip := firstIPv4(out); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("docker network %q has no IPv4 gateway", kindDockerNetwork)
}

// firstIPv4 returns the first IPv4 address leading a line of out.
func firstIPv4(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			if ip := net.ParseIP(f[0]); ip != nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return ""
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
	gw, err := kindGatewayIP(cfg.ControlPlaneNode())
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

// preflightHostServer proves a host model server answers from INSIDE the
// cluster before the platform install waits ten minutes on a model-manager
// whose backend is unreachable (or wires ModelConfigs to a dead endpoint). A
// short-lived pod fetches the server's identifying document; the two known
// host-side failures are spelled out with their fixes: the server bound to
// 127.0.0.1 (connection refused from the bridge) and a host firewall
// dropping pod->host traffic on the docker bridge (timeout).
//
// The marker it looks for is deliberately weaker than the probe's ident:
// busybox wget writes the body next to kubectl's own chatter, so nothing here
// can be parsed as JSON. This step answers reachability — `agentlab
// configure` already established on loopback that this is the server it says
// it is.
func preflightHostServer(cfg *config.Config, backend, endpoint string) error {
	server := config.BackendServerName(backend)
	spec, known := backendSpec(backend)
	if !known {
		return fmt.Errorf("unknown host model server backend %q", backend)
	}
	// The probe image goes host cache -> node like every lab image, so the
	// pod never waits on an in-node pull (best-effort: a miss falls back to
	// the kubelet's pull under the pod-running timeout).
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	pod := backend + "-preflight"
	// One probe pod running wget (a leftover of the same name from an
	// interrupted run is removed first); its output is the container's log,
	// wget's error message included.
	out, err := runProbePod(context.Background(), platformNamespace, pod, probeImage,
		[]string{"wget", "-qO-", "-T", "5", endpoint + spec.probe.path}, probePodTimeout)
	if err == nil && strings.Contains(out, spec.probe.marker) {
		// The document may carry more than the identifying field (Lemonade's
		// health document does), so pick that field out of it.
		detail := strings.TrimSpace(out)
		if m := spec.probe.detail.FindString(out); m != "" {
			detail = m
		}
		note("host %s answers from inside the cluster: %s", server, detail)
		return nil
	}
	out = strings.TrimSpace(out)
	reason := "the probe pod could not fetch it"
	fixes := spec.bindFix + "\n" +
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
		"  Fixes (docs/models.md, \"Local backends on the lab host\"):\n%s\n"+
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
