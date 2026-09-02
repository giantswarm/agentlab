package lab

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The model-manager component (platform.modelManager in agentlab.yaml): the
// umbrella's `model-manager` dependency with the Ollama backend, proxying the
// Ollama that runs on the lab host. Pods reach the host only through the kind
// docker network's gateway — the same address the README documents for
// extraModels — so the endpoint is detected from `docker network inspect
// kind` rather than asked for. Everything that can go wrong is host-side
// plumbing (bind address, firewall), which preflightOllama turns into an
// early, actionable failure instead of a model-manager pod that reports an
// unhealthy backend after a ten-minute install.

// modelManagerMCPServer is the MCPServer CR name the model-manager chart
// registers with muster (model-manager.muster.mcpServer.name, the chart
// default); muster prefixes its tools with it: x_model-manager_<tool>.
const modelManagerMCPServer = "model-manager"

// kindDockerNetwork is the docker network kind creates its nodes on.
const kindDockerNetwork = "kind"

// ollamaProbeImage runs the in-cluster reachability probe: busybox wget in
// the alpine image the umbrella already ships elsewhere (small, present in
// the host cache after the first run, side-loaded before the probe pod).
const ollamaProbeImage = "gsoci.azurecr.io/giantswarm/alpine:3.22.1"

// DetectHostOllama probes the host's own Ollama on loopback and reports its
// version. It answers the configure-time question "is there an Ollama on
// this machine at all?" — reachability from pods (bind address, firewall) is
// preflightOllama's job at platform time.
func DetectHostOllama() (version string, ok bool) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/version", config.OllamaPort))
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

// resolveModelManagerEndpoint is the Ollama endpoint model-manager dials: the
// configured override, else http://<kind gateway>:11434. The kind network
// exists once the cluster does, so callers that render before a boot get an
// error they may tolerate (render) or must not (platform).
func resolveModelManagerEndpoint(cfg *config.Config) (string, error) {
	if ep := cfg.Platform.ModelManager.Endpoint; ep != "" {
		return strings.TrimSuffix(ep, "/"), nil
	}
	gw, err := kindGatewayIP()
	if err != nil {
		return "", fmt.Errorf("autodetecting the Ollama endpoint: %w (set platform.modelManager.endpoint to skip the detection)", err)
	}
	return fmt.Sprintf("http://%s:%d", gw, config.OllamaPort), nil
}

// preflightOllama proves host Ollama answers from INSIDE the cluster before
// the platform install waits ten minutes on a model-manager whose backend is
// unreachable. A short-lived pod fetches <endpoint>/api/version; the two
// known host-side failures are spelled out with their fixes: the server
// bound to 127.0.0.1 (connection refused from the bridge) and a host
// firewall dropping pod->host traffic on the docker bridge (timeout).
func preflightOllama(cfg *config.Config, endpoint string) error {
	// The probe image goes host cache -> node like every lab image, so the
	// pod never waits on an in-node pull (best-effort: a miss falls back to
	// the kubelet's pull under the pod-running timeout).
	sideloadImages(cfg, hostPullImages([]string{ollamaProbeImage}))
	const pod = "ollama-preflight"
	// A leftover from an interrupted run would make `kubectl run` refuse.
	_ = runQuiet("kubectl", "-n", platformNamespace, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	out, err := outputAll("kubectl", "-n", platformNamespace, "run", pod,
		"--rm", "-i", "--quiet", "--restart=Never", "--pod-running-timeout=120s",
		"--image="+ollamaProbeImage, "--image-pull-policy=IfNotPresent",
		"--command", "--", "wget", "-qO-", "-T", "5", endpoint+"/api/version")
	if err == nil && strings.Contains(out, `"version"`) {
		version := strings.TrimSpace(out)
		if len(version) > 60 {
			version = version[:60] + "..."
		}
		note("host Ollama answers from inside the cluster: %s", version)
		return nil
	}
	out = strings.TrimSpace(out)
	reason := "the probe pod could not fetch it"
	fixes := "  - bind Ollama to every interface, not loopback: OLLAMA_HOST=0.0.0.0 (systemd: an\n" +
		"    Environment= drop-in on ollama.service), then restart it\n" +
		"  - allow TCP " + fmt.Sprint(config.OllamaPort) + " from the docker bridge subnets (they fall inside\n" +
		"    172.16.0.0/12) through the host firewall — pod->host traffic arrives on the\n" +
		"    bridge like any other inbound connection"
	switch {
	case strings.Contains(out, "refused"):
		reason = "connection refused — Ollama is not listening on the bridge address (the usual\n  cause: OLLAMA_HOST left at 127.0.0.1)"
	case strings.Contains(out, "timed out"), strings.Contains(out, "timeout"):
		reason = "connection timed out — the host firewall drops pod->host traffic on the docker\n  bridge (the request never reaches Ollama)"
	}
	return fmt.Errorf("host Ollama is not reachable from pods at %s: %s.\n"+
		"  Fixes (README, \"Local backends on the lab host\"):\n%s\n"+
		"  Then re-run `agentlab platform`, or set platform.modelManager.enabled: false.\n"+
		"  Probe output: %.300s", endpoint, reason, fixes, out)
}

// modelManagerHint is the platform-up summary line for the component.
func modelManagerHint(cfg *config.Config, endpoint string) string {
	if !cfg.ModelManagerEnabled() {
		return "  Model manager is disabled (platform.modelManager in agentlab.yaml)."
	}
	return fmt.Sprintf("  Model manager: Ollama backend at %s; REST %s/api/v1 (Dex token required),\n"+
		"  MCP tools x_model-manager_* through muster; the portal's Models tab manages the same models.",
		endpoint, cfg.ModelManagerBaseURL())
}
