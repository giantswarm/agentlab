package lab

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The vm-manager wiring (platform.vmManager in agentlab.yaml): the platform's
// VM provisioner (github.com/giantswarm/vm-manager) runs on the KVM host
// itself — it needs /dev/kvm, /dev/vhost-vsock, QEMU, swtpm and a service
// manager for the VMs — never in a pod. The lab treats it like the host model
// servers model-manager fronts: pods reach it through the kind docker
// network's gateway, `agentlab configure` discovers it on this machine,
// `agentlab platform` proves it reachable from inside the cluster and then
// registers it with muster as an MCPServer of the agent-platform tool group
// (vm-manager.yaml.tmpl) — the CR the agent-platform chart renders for a host
// on a management cluster, rendered here for the machine that runs the lab.
//
// Identity is the whole point of the wiring, and it runs the same way as for
// every other downstream: muster forwards the person's Dex id_token
// (auth.forwardToken) and vm-manager validates it against the lab Dex's JWKS,
// trusting the platform client's audience. vm-manager needs to be told the
// lab's issuer, CA, client and audience for that, and the lab is what knows
// them — so `agentlab platform` writes state/vm-manager.env, the environment
// `vm-manager serve` reads (every flag has one), and the host runs it from
// there. What the wiring proves is `agentlab vm-manager-test`
// (vmmanagertest.go).

// vmManagerMCPServer is the MCPServer CR name the lab registers; muster
// prefixes its tools with it: x_vm-manager_<tool>.
const vmManagerMCPServer = "vm-manager"

// vmManagerHostServiceLabel marks the lab-created registration of a host
// service, as opposed to a fixture: the tool-group proof admits the platform
// tool group on such a server (checkToolGroupLabels).
const vmManagerHostServiceLabel = "agentlab.giantswarm.io/host-service"

// vmManagerSelector lists exactly the lab's vm-manager registration.
const vmManagerSelector = vmManagerHostServiceLabel + "=" + vmManagerMCPServer

// vm-manager's own paths. The MCP endpoint is its default; /metrics is the
// Prometheus exposition outside the OAuth guard, whose vm_manager_build_info
// series is how the lab recognises a vm-manager — the identity question is
// answered by the shape of a document, never by a status code (backends.go
// says why); /api/v1/host is the guarded capability report the proof calls.
const (
	vmManagerMCPPath     = "/mcp"
	vmManagerMetricsPath = "/metrics"
	vmManagerHostPath    = "/api/v1/host"
	vmManagerBuildInfo   = "vm_manager_build_info"
)

// vmManagerEnvFile is where `agentlab platform` writes the environment
// `vm-manager serve` needs to trust this lab's identity; `agentlab
// vm-manager-env` prints the same.
const vmManagerEnvFile = StateDir + "/vm-manager.env"

// HostVMManager is a vm-manager the discovery found on this machine — the
// same shape as a HostServer, the reachability question answered the same
// way (podReachableHost).
type HostVMManager struct {
	// Version is what vm_manager_build_info says.
	Version string
	// Addr is where the host-side identity probe found it.
	Addr string
	// Probed: the reachability question was put to a running node, so
	// PodHost is an answer; ReachErr explains a probe that could not run.
	Probed   bool
	ReachErr error
	// PodHost is the host part pods reach it on — the kind gateway, else the
	// runtime's host alias; empty when Probed and neither answers.
	PodHost string
	// HostReachesGateway: this machine itself dials the gateway address —
	// with Probed and no PodHost, the mark of a host firewall rejecting the
	// bridge rather than of a loopback bind.
	HostReachesGateway bool
}

// resolveVMManagerEndpoint is the base URL pods dial for vm-manager: the
// configured override, else the address pods reach this machine on (the
// kind gateway, or the container runtime's host alias where that gateway is
// inside its VM) on platform.vmManager.port — the one helper the discovery
// reports from, so the address named there and the one wired here agree.
// The kind network exists once the cluster does, so a caller that renders
// before a boot may tolerate the error; the platform run must not.
func resolveVMManagerEndpoint(cfg *config.Config) (string, error) {
	vmm := cfg.Platform.VMManager
	if vmm.Endpoint != "" {
		return strings.TrimSuffix(vmm.Endpoint, "/"), nil
	}
	node := cfg.ControlPlaneNode()
	gw, err := kindGatewayIP(node)
	if err != nil {
		return "", fmt.Errorf("autodetecting the vm-manager endpoint: %w (set platform.vmManager.endpoint to skip the detection)", err)
	}
	port := vmm.ListenPort()
	host, err := podReachableHost(node, gw, port)
	switch {
	case err != nil:
		// No verdict (a node that is not running): the gateway is the
		// documented default.
		return fmt.Sprintf("http://%s:%d", gw, port), nil
	case host == "":
		return "", fmt.Errorf("no address reaches the host vm-manager from pods: neither %s (the container runtime's gateway) nor %s (its host alias) answers — %s\n"+
			"  or set platform.vmManager.endpoint for a vm-manager elsewhere.",
			net.JoinHostPort(gw, strconv.Itoa(port)), net.JoinHostPort(hostAlias(), strconv.Itoa(port)), vmManagerUnreachableCause(gw, port))
	}
	return fmt.Sprintf("http://%s:%d", host, port), nil
}

// vmManagerUnreachableCause tells the three causes of "pods cannot reach it"
// apart from the host's side, where the discovery already found (or did not
// find) a vm-manager: nothing on loopback is a vm-manager that is not
// running; loopback but not the gateway address is one bound to loopback;
// both, while the node still cannot dial the gateway, is the host firewall
// rejecting the docker bridge on this port — the case a bind hint would
// misdiagnose. Each comes with its own fix.
func vmManagerUnreachableCause(gw string, port int) string {
	loopback := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	gateway := net.JoinHostPort(gw, strconv.Itoa(port))
	switch {
	case !tcpAnswers(loopback):
		return fmt.Sprintf("nothing listens on %s, so no vm-manager runs on this machine.\n"+
			"  Start it with this lab's settings (they bind every interface and trust the lab Dex):\n%s", loopback, vmManagerRunHint())
	case !tcpAnswers(gateway):
		return fmt.Sprintf("it answers on %s but not on %s: bound to loopback.\n"+
			"  Run it from this lab's environment, whose VM_MANAGER_LISTEN binds every interface:\n%s", loopback, gateway, vmManagerRunHint())
	default:
		return fmt.Sprintf("this machine reaches it on %s but the node does not: the host firewall rejects the docker bridge on this port\n"+
			"  (a default-deny INPUT chain that admits the model servers' ports and not this one).\n"+
			"  Allow TCP %d from the docker bridge subnets (they fall inside 172.16.0.0/12), the way the model servers' ports are allowed,", gateway, port)
	}
}

// vmManagerURL is the MCP endpoint muster dials for a base URL.
func vmManagerURL(endpoint string) string { return endpoint + vmManagerMCPPath }

// vmManagerRenderEndpoint is resolveVMManagerEndpoint for a render that may
// have no cluster to ask (`agentlab render` before a boot): the override, the
// gateway when the kind network exists, else the loopback placeholder the
// platform run replaces — it resolves strictly and re-renders.
func vmManagerRenderEndpoint(cfg *config.Config) string {
	if ep, err := resolveVMManagerEndpoint(cfg); err == nil {
		return ep
	}
	return fmt.Sprintf("http://127.0.0.1:%d", cfg.Platform.VMManager.ListenPort())
}

// vmManagerIdent reads vm-manager's Prometheus exposition and returns the
// version of the vm_manager_build_info series; ok is false when the document
// is not a vm-manager's.
func vmManagerIdent(body []byte) (string, bool) {
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, vmManagerBuildInfo+"{") {
			continue
		}
		version := "unknown version"
		// The label, not a suffix of another (go_version): it follows the
		// brace or a comma.
		labels := line[len(vmManagerBuildInfo):]
		for _, marker := range []string{`{version="`, `,version="`} {
			if i := strings.Index(labels, marker); i >= 0 {
				rest := labels[i+len(marker):]
				if j := strings.Index(rest, `"`); j >= 0 && rest[:j] != "" {
					version = rest[:j]
				}
				break
			}
		}
		return version, true
	}
	return "", false
}

// detectVMManager asks a vm-manager at base to identify itself through its
// metrics — the configure-time question "is there one on this machine?".
func detectVMManager(base string) (version string, ok bool) {
	resp, err := probeClient().Get(base + vmManagerMetricsPath)
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
	return vmManagerIdent(body)
}

// discoverVMManager probes this machine for a vm-manager on the configured
// port — on loopback, then on the kind gateway for one bound to that address
// alone — and, with a running node, asks from inside it which address pods
// reach it on. nil when none answers.
func discoverVMManager(cfg *config.Config, gateway string, clusterExists bool) *HostVMManager {
	port := cfg.Platform.VMManager.ListenPort()
	candidates := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	if gateway != "" {
		candidates = append(candidates, net.JoinHostPort(gateway, strconv.Itoa(port)))
	}
	for _, addr := range candidates {
		version, ok := detectVMManager("http://" + addr)
		if !ok {
			continue
		}
		found := &HostVMManager{Version: version, Addr: addr}
		if gateway != "" {
			found.HostReachesGateway = tcpAnswers(net.JoinHostPort(gateway, strconv.Itoa(port)))
		}
		if gateway != "" && clusterExists {
			host, err := podReachableHost(cfg.ControlPlaneNode(), gateway, port)
			if err != nil {
				found.ReachErr = err
			} else {
				found.Probed, found.PodHost = true, host
			}
		}
		return found
	}
	return nil
}

// preflightVMManager proves the host vm-manager answers from INSIDE the
// cluster before muster is pointed at it, the way preflightHostServer does
// for a model server: a probe pod fetches the metrics document and the same
// fingerprint the loopback discovery used decides what answered.
func preflightVMManager(cfg *config.Config, endpoint string) error {
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	ctx, cancel := context.WithTimeout(context.Background(), probePodTimeout)
	defer cancel()
	out, err := runProbePod(ctx, platformNamespace, vmManagerMCPServer+"-preflight", probeImage,
		[]string{"wget", "-qO-", "-T", strconv.Itoa(int(probeHeaderTimeout.Seconds())), endpoint + vmManagerMetricsPath},
		probePodTimeout)
	if err == nil {
		if version, ok := vmManagerIdent([]byte(out)); ok {
			note("host vm-manager answers from inside the cluster: %s", version)
			return nil
		}
		return fmt.Errorf("something at %s answered the probe, but it is not vm-manager: the document at %s carries no %s series.\n"+
			"  Pods reached the address, so platform.vmManager.endpoint (or .port) names another server,\n"+
			"  or that vm-manager runs with --metrics-enabled=false, which the lab needs on to recognise it.\n"+
			"  Answer: %.300s", endpoint, vmManagerMetricsPath, vmManagerBuildInfo, strings.TrimSpace(out))
	}
	out = strings.TrimSpace(out)
	reason := "the probe pod could not fetch it"
	switch {
	case strings.Contains(out, "refused"):
		reason = "connection refused — nothing listens on the bridge address (a vm-manager bound to 127.0.0.1,\n  or none running)"
	case strings.Contains(out, "timed out"), strings.Contains(out, "timeout"):
		reason = "connection timed out — the host firewall drops pod->host traffic on the docker bridge"
	case strings.Contains(out, "No route to host"), strings.Contains(out, "unreachable"):
		reason = "no route to host — the host firewall REJECTS pod->host traffic to this port (an ICMP admin-prohibited answer:\n  a default-deny INPUT chain that admits the model servers' ports and not this one)"
	}
	return fmt.Errorf("host vm-manager is not reachable from pods at %s: %s.\n"+
		"  Run it with this lab's settings (they bind the address pods dial and trust the lab Dex):\n%s"+
		"  allow TCP %d from the docker bridge subnets (172.16.0.0/12) through the host firewall if it drops them.\n"+
		"  Then re-run `agentlab platform`, or turn platform.vmManager.enabled off\n"+
		"  (`agentlab configure --defaults` follows what answers on this machine).\n"+
		"  Probe output: %.300s", endpoint, reason, vmManagerRunHint(), cfg.Platform.VMManager.ListenPort(), out)
}

// ensureVMManager brings muster's registration of the host vm-manager to what
// platform.vmManager says. On: applies the MCPServer (vm-manager.yaml.tmpl,
// its URL the endpoint the preflight just proved) and waits until muster
// reports it reachable — Auth Required until the first session signs in,
// Connected afterwards; Failed is a vm-manager that stopped between the
// preflight and now. Off: removes the registration a lab created while the
// key was on. After the platform install like the fixtures: the MCPServer CRD
// ships with muster.
func ensureVMManager(cfg *config.Config, endpoint string) error {
	ctx := context.Background()
	if !cfg.VMManagerEnabled() {
		return removeVMManager(ctx)
	}
	step("Registering the host vm-manager with muster (MCPServer %s -> %s)", vmManagerMCPServer, vmManagerURL(endpoint))
	rendered, _, err := renderManifestWith(cfg, vmManagerTemplate, func(d *tmplData) { d.VMManagerURL = vmManagerURL(endpoint) })
	if err != nil {
		return err
	}
	if _, err := applyManifests(ctx, rendered); err != nil {
		return err
	}
	return waitMCPServerReachable(vmManagerMCPServer)
}

// removeVMManager deletes the lab's vm-manager registration — what `agentlab
// platform` does while platform.vmManager is off. Idempotent.
func removeVMManager(ctx context.Context) error {
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return err
	}
	existing, err := listObjects(ctx, gvr, platformNamespace, vmManagerSelector)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // no muster, no CRD: nothing registered
		}
		return err
	}
	for _, member := range existing {
		note("removing the vm-manager registration %s (platform.vmManager is off)", member.GetName())
		if err := deleteObject(ctx, gvr, platformNamespace, member.GetName(), fixtureDeleteWait); err != nil {
			return err
		}
	}
	return nil
}

// vmManagerEnv renders the environment `vm-manager serve` needs for THIS
// lab: a listen address pods can dial (every interface: the address that
// reaches this machine from a pod is the kind gateway on a native runtime
// and the runtime's host alias where it runs in a VM, and vm-manager's
// default loopback is neither — its API is guarded, so nothing answers a
// caller without a token from this lab's Dex), and OAuth against the lab Dex
// — the one issuer URL valid from the host, the lab CA (a private
// certificate, a loopback issuer), the platform client (vm-manager's dex
// provider wants a client; the same one muster and Backstage use) and the
// platform client's audience as the trusted one, since that is what muster
// forwards. VM_MANAGER_OAUTH_BASE_URL names vm-manager's own OAuth server,
// which the forwarded tokens never touch; a loopback http URL is what its
// OAuth 2.1 check admits without TLS.
func vmManagerEnv(cfg *config.Config) (string, error) {
	vmm := cfg.Platform.VMManager
	listen := fmt.Sprintf("0.0.0.0:%d", vmm.ListenPort())
	caPath, err := filepath.Abs(caCertPath)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# vm-manager's environment for the agentlab at %s, written by `agentlab platform`.\n", cfg.MusterBaseURL())
	b.WriteString("# Every `vm-manager serve` flag has a variable; flags on the command line win.\n")
	b.WriteString("# Load it into a shell (set -a; source <this file>; set +a) or point a systemd\n")
	b.WriteString("# unit's EnvironmentFile= at it. Lab-only throwaway credentials, by design.\n")
	fmt.Fprintf(&b, "VM_MANAGER_LISTEN=%s\n", listen)
	b.WriteString("VM_MANAGER_OAUTH_ENABLED=true\n")
	fmt.Fprintf(&b, "VM_MANAGER_OAUTH_BASE_URL=http://localhost:%d\n", vmm.ListenPort())
	b.WriteString("VM_MANAGER_OAUTH_PROVIDER=dex\n")
	fmt.Fprintf(&b, "DEX_ISSUER_URL=%s\n", cfg.Issuer())
	fmt.Fprintf(&b, "DEX_CLIENT_ID=%s\n", config.AgentPlatformClientID)
	fmt.Fprintf(&b, "DEX_CLIENT_SECRET=%s\n", config.AgentPlatformClientSecret)
	fmt.Fprintf(&b, "DEX_CA_FILE=%s\n", caPath)
	b.WriteString("VM_MANAGER_OAUTH_ALLOW_PRIVATE_URLS=true\n")
	b.WriteString("SSO_ALLOW_PRIVATE_IPS=true\n")
	fmt.Fprintf(&b, "OAUTH_TRUSTED_AUDIENCES=%s\n", config.AgentPlatformClientID)
	return b.String(), nil
}

// writeVMManagerEnv writes vmManagerEnv to state/vm-manager.env (0600: it
// carries the lab's client secret) and returns the absolute path.
func writeVMManagerEnv(cfg *config.Config) (string, error) {
	content, err := vmManagerEnv(cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return "", err
	}
	if err := os.WriteFile(vmManagerEnvFile, []byte(content), 0o600); err != nil {
		return "", err
	}
	return filepath.Abs(vmManagerEnvFile)
}

// VMManagerEnv prints the environment `vm-manager serve` needs for this lab
// (`agentlab vm-manager-env`) — the same content `agentlab platform` writes
// to state/vm-manager.env, for a shell that wants to eval it. Nothing here
// needs the cluster: the settings describe the lab, not the node.
func VMManagerEnv(cfg *config.Config, w io.Writer) error {
	content, err := vmManagerEnv(cfg)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, content)
	return err
}

// vmManagerRunHint is the two lines that start a vm-manager for this lab,
// indented for the summary and the preflight's fixes.
func vmManagerRunHint() string {
	path, err := filepath.Abs(vmManagerEnvFile)
	if err != nil {
		path = vmManagerEnvFile
	}
	return fmt.Sprintf("    set -a; source %s; set +a   # written by `agentlab platform`, printed by `agentlab vm-manager-env`\n"+
		"    vm-manager serve --image-dir <the directory `make -C images` in a vm-manager checkout built>\n", path)
}

// vmManagerHint is the platform-up summary for the vm-manager wiring.
func vmManagerHint(cfg *config.Config, endpoint string) string {
	if !cfg.VMManagerEnabled() {
		return "  vm-manager is not wired (platform.vmManager in agentlab.yaml; `agentlab configure` turns it on when one answers on this machine)."
	}
	return fmt.Sprintf("  vm-manager: the host's VM provisioner at %s, registered as x_%s_* through muster\n"+
		"  (tool group %s; the portal lists it under Agent Platform). It runs on this host from\n"+
		"  the lab's settings (`agentlab vm-manager-env` prints them):\n%s",
		vmManagerURL(endpoint), vmManagerMCPServer, toolGroupAgentPlatform, vmManagerRunHint())
}

// tcpAnswers reports whether something accepts a TCP connection at addr,
// dialed from this machine — the host's side of the reachability question,
// which podReachableHost never asks (the two vantage points are kept apart
// on purpose there); vmManagerUnreachableCause asks both to tell a firewall
// from a bind.
func tcpAnswers(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
