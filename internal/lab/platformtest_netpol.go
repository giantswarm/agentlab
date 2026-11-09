package lab

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The network-policy proof (platform.networkPolicy): the platform's policies
// in the plain NetworkPolicy flavour kindnetd enforces, with the selectors an
// installation runs. agent-manager is the backend: the connectivity chart's
// ingress policy admits muster by its pod labels (and the data plane), so a
// pod anywhere else is refused, while muster — on the pod network, carrying
// those labels — still calls it.

// netpolProbeNamespace is where the probe pod runs: a namespace outside
// muster's, which no platform policy names as a peer.
const netpolProbeNamespace = "default"

// netpolProbePod is the probe pod's name.
const netpolProbePod = "agentlab-netpol-probe"

// netpolProbeTimeout is how long the probe waits for each connection; a
// refused one times out, since a NetworkPolicy drops rather than resets.
const netpolProbeTimeout = 5

// netpolSettle is how long the probe pod waits before its first connection:
// a new pod's policy takes effect about a second after its IP is synced.
const netpolSettle = 5

// The probe's two targets: muster's Service, whose own policy admits every
// namespace on its port (the control: the probe's network works), and
// agent-manager's, which admits muster and the data plane only.
var (
	netpolControlURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:8090/health", componentMuster, platformNamespace)
	netpolBackendURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:8080/readyz", agentManagerMCPServer, platformNamespace)
)

// netpolProbeScript connects to both targets from the probe pod, each answer
// under its own marker line, the backend's followed by wget's exit status so
// the verdict reads how the connection ended, not only what it printed.
func netpolProbeScript() string {
	return fmt.Sprintf(`sleep %d
echo '--- control'
wget -S -T %d -O /dev/null %s 2>&1
echo '--- backend'
wget -S -T %d -O /dev/null %s 2>&1
echo "%s$?"`, netpolSettle, netpolProbeTimeout, netpolControlURL, netpolProbeTimeout, netpolBackendURL, netpolExitMarker)
}

// netpolExitMarker prefixes the backend wget's exit status in the probe's
// output.
const netpolExitMarker = "--- backend exit "

// netpolTimedOut is busybox wget's message when -T expires before the
// connection answers: what a NetworkPolicy's dropped packets look like.
const netpolTimedOut = "download timed out"

// netpolOtherFailures are the backend answers that are not a policy's drop —
// a name that does not resolve, a port nothing listens on, a reset — each
// with what it says about the probe.
var netpolOtherFailures = []struct{ text, means string }{
	{"bad address", "the Service name does not resolve"},
	{"Connection refused", "nothing listens on the Service's port, or it has no endpoints"},
	{"Connection reset", "the connection was reset, not dropped"},
	{"Network is unreachable", "the pod has no route"},
	{"No route to host", "the pod has no route"},
}

// netpolProbeVerdict judges the probe's output: the control must answer with
// an HTTP status; the backend must end in wget's own connection timeout with
// a failed exit and no HTTP status — a NetworkPolicy drops packets, so a
// name that does not resolve, a refused or reset connection, no output or
// anything unrecognised is no proof of the policy and fails. It returns the
// two answers for the note.
func netpolProbeVerdict(out string) (control, backend string, err error) {
	_, rest, ok := strings.Cut(out, "--- control\n")
	if !ok {
		return "", "", fmt.Errorf("the probe pod printed no control section: %.300s", strings.TrimSpace(out))
	}
	control, backend, ok = strings.Cut(rest, "--- backend\n")
	if !ok {
		return "", "", fmt.Errorf("the probe pod printed no backend section: %.300s", strings.TrimSpace(out))
	}
	backend, exitText, ok := strings.Cut(backend, netpolExitMarker)
	if !ok {
		return "", "", fmt.Errorf("the probe pod printed no exit status for the backend: %.300s", strings.TrimSpace(out))
	}
	control, backend = strings.TrimSpace(control), strings.TrimSpace(backend)
	if !httpStatusRe.MatchString(control) {
		return control, backend, fmt.Errorf("the probe pod in %s did not reach muster's Service (%s), whose policy admits every namespace — the probe's own network is broken, so a refusal below would prove nothing: %.300s", netpolProbeNamespace, netpolControlURL, control)
	}
	if httpStatusRe.MatchString(backend) {
		return control, backend, fmt.Errorf("a pod in %s reached agent-manager's Service (%s) although its ingress policy admits muster and the data plane only — the policy is not enforced: %.300s", netpolProbeNamespace, netpolBackendURL, backend)
	}
	for _, f := range netpolOtherFailures {
		if strings.Contains(backend, f.text) {
			return control, backend, fmt.Errorf("agent-manager's Service (%s) answered %q from %s: %s — no proof that a policy dropped the connection: %.300s", netpolBackendURL, f.text, netpolProbeNamespace, f.means, backend)
		}
	}
	if exit := strings.TrimSpace(exitText); exit == "0" || exit == "" {
		return control, backend, fmt.Errorf("wget on agent-manager's Service (%s) exited %q without an HTTP status — neither an answer nor a timeout: %.300s", netpolBackendURL, exit, backend)
	}
	if !strings.Contains(backend, netpolTimedOut) {
		return control, backend, fmt.Errorf("agent-manager's Service (%s) did not time out from %s (wget %q): unrecognised failure, no proof that a policy dropped the connection: %.300s", netpolBackendURL, netpolProbeNamespace, netpolTimedOut, backend)
	}
	return control, backend, nil
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// proveNetworkPolicyDenial is the denial proof: a pod outside muster's
// namespace times out on agent-manager's Service while it reaches muster's,
// and muster's own call to agent-manager (list_model_configs as the
// caller) still answers.
func proveNetworkPolicyDenial(ctx context.Context, cfg *config.Config, token string) (string, error) {
	step("A pod in %s: %s (the control) and %s (the backend, admitting muster by its pod labels)", netpolProbeNamespace, netpolControlURL, netpolBackendURL)
	out, err := runProbePod(ctx, netpolProbeNamespace, netpolProbePod, probeImage, []string{"sh", "-c", netpolProbeScript()}, probePodTimeout)
	if err != nil {
		return "", fmt.Errorf("the network-policy probe pod: %w\n%.300s", err, strings.TrimSpace(out))
	}
	control, backend, err := netpolProbeVerdict(out)
	if err != nil {
		return "", err
	}
	statuses := httpStatusRe.FindAllString(control, -1)
	note("control, muster's Service: %s", statuses[len(statuses)-1])
	note("backend, agent-manager's Service: %s", lastLine(backend))
	step("muster's call to agent-manager: %slist_model_configs as the caller", agentManagerToolPrefix)
	session, err := openMusterSession(cfg, token, "platform-test-network-policy")
	if err != nil {
		return "", err
	}
	modelConfig, err := agentManagerModelConfig(session)
	if err != nil {
		return "", fmt.Errorf("muster's call to agent-manager under the policies: %w", err)
	}
	note("agent-manager answered muster: the default ModelConfig is %s", modelConfig)
	return fmt.Sprintf("a pod in %s timed out on agent-manager's Service and reached muster's; muster's %slist_model_configs answered (%s)", netpolProbeNamespace, agentManagerToolPrefix, modelConfig), nil
}
