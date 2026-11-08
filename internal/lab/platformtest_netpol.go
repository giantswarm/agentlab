package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
// under its own marker line.
func netpolProbeScript() string {
	return fmt.Sprintf(`sleep %d
echo '--- control'
wget -S -T %d -O /dev/null %s 2>&1
echo '--- backend'
wget -S -T %d -O /dev/null %s 2>&1
true`, netpolSettle, netpolProbeTimeout, netpolControlURL, netpolProbeTimeout, netpolBackendURL)
}

// netpolProbeVerdict judges the probe's output: the control must answer with
// an HTTP status, the backend with none. It returns the two answers' last
// lines for the note.
func netpolProbeVerdict(out string) (control, backend string, err error) {
	_, rest, ok := strings.Cut(out, "--- control\n")
	if !ok {
		return "", "", fmt.Errorf("the probe pod printed no control section: %.300s", strings.TrimSpace(out))
	}
	control, backend, ok = strings.Cut(rest, "--- backend\n")
	if !ok {
		return "", "", fmt.Errorf("the probe pod printed no backend section: %.300s", strings.TrimSpace(out))
	}
	control, backend = strings.TrimSpace(control), strings.TrimSpace(backend)
	if !httpStatusRe.MatchString(control) {
		return control, backend, fmt.Errorf("the probe pod in %s did not reach muster's Service (%s), whose policy admits every namespace — the probe's own network is broken, so a refusal below would prove nothing: %.300s", netpolProbeNamespace, netpolControlURL, control)
	}
	if httpStatusRe.MatchString(backend) {
		return control, backend, fmt.Errorf("a pod in %s reached agent-manager's Service (%s) although its ingress policy admits muster and the data plane only — the policy is not enforced: %.300s", netpolProbeNamespace, netpolBackendURL, backend)
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

// The Valkey store's policy (platform.networkPolicy): the chart ships it as
// a CiliumNetworkPolicy admitting muster and klaus-gateway only; the lab
// enforces it through its NetworkPolicy stand-in (ciliumstandin.go). Probe
// pods in the store's own namespace knock on the Valkey port: one carrying a
// client's labels is answered (the server's NOAUTH, no password needed),
// one without them is refused.

// valkeyService is the store's Service, valkeyPort its port; appNameLabel
// the pod label the store's policy admits its clients by.
const (
	appNameLabel  = "app.kubernetes.io/name"
	valkeyService = "muster-valkey"
	valkeyPort    = 6379
)

// valkeyProbe is one probe pod of the store proof: its name, the labels it
// carries and whether the policy must admit it.
type valkeyProbe struct {
	name   string
	labels map[string]string
	admit  bool
}

// valkeyProbes are the store proof's pods: a client of each kind the chart
// admits, and a third pod with neither's labels.
func valkeyProbes() []valkeyProbe {
	return []valkeyProbe{
		{name: "agentlab-valkey-probe-muster", labels: map[string]string{appNameLabel: componentMuster}, admit: true},
		{name: "agentlab-valkey-probe-klaus-gateway", labels: map[string]string{appNameLabel: klausGatewayComponent}, admit: true},
		{name: "agentlab-valkey-probe-other", labels: map[string]string{appNameLabel: "agentlab-valkey-probe"}, admit: false},
	}
}

// valkeyProbeScript sends PING to the store and prints what came back under
// a marker line. A reachable Valkey answers before AUTH (-NOAUTH); a
// refused connection times out with nothing.
func valkeyProbeScript() string {
	return fmt.Sprintf(`sleep %d
echo '--- valkey'
printf 'PING\r\n' | nc -w %d %s.%s.svc.cluster.local %d 2>&1
echo "--- exit $?"
true`, netpolSettle, netpolProbeTimeout, valkeyService, platformNamespace, valkeyPort)
}

// valkeyAnswered reports whether the probe's output carries a reply of the
// Valkey server (a RESP simple string or error).
func valkeyAnswered(out string) (string, bool, error) {
	_, rest, ok := strings.Cut(out, "--- valkey\n")
	if !ok {
		return "", false, fmt.Errorf("the probe pod printed no valkey section: %.300s", strings.TrimSpace(out))
	}
	reply, _, _ := strings.Cut(rest, "--- exit")
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "-NOAUTH") || strings.HasPrefix(line, "+PONG") || strings.HasPrefix(line, "-ERR") {
			return line, true, nil
		}
	}
	return strings.TrimSpace(reply), false, nil
}

// probeAdoptedBy names a Service or Deployment of ns whose selector would
// take a probe pod with these labels in — a Service would route traffic to
// it, a ReplicaSet adopt it — or "" when none would.
func probeAdoptedBy(ctx context.Context, ns string, labels map[string]string) (string, error) {
	k, err := labKube()
	if err != nil {
		return "", err
	}
	set := k8slabels.Set(labels)
	services, err := k.clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing the Services of %s: %w", ns, err)
	}
	for _, s := range services.Items {
		if len(s.Spec.Selector) > 0 && k8slabels.SelectorFromSet(s.Spec.Selector).Matches(set) {
			return "Service " + s.Name, nil
		}
	}
	deployments, err := k.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing the Deployments of %s: %w", ns, err)
	}
	for _, d := range deployments.Items {
		sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		if err != nil {
			return "", fmt.Errorf("the selector of Deployment %s: %w", d.Name, err)
		}
		if !sel.Empty() && sel.Matches(set) {
			return "Deployment " + d.Name, nil
		}
	}
	return "", nil
}

// liveStandIn checks the store's stand-in against the live policy: the
// CiliumNetworkPolicy the release installed, translated again, is the
// NetworkPolicy kindnetd enforces. It returns the stand-in.
func liveStandIn(ctx context.Context) (ciliumStandIn, error) {
	cnpGVR := schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
	obj, err := getObject(ctx, cnpGVR, platformNamespace, valkeyService)
	if err != nil {
		return ciliumStandIn{}, fmt.Errorf("the store's CiliumNetworkPolicy %s/%s: %w", platformNamespace, valkeyService, err)
	}
	raw, err := obj.MarshalJSON()
	if err != nil {
		return ciliumStandIn{}, err
	}
	var cnp ciliumPolicy
	if err := json.Unmarshal(raw, &cnp); err != nil {
		return ciliumStandIn{}, fmt.Errorf("the store's CiliumNetworkPolicy: %w", err)
	}
	nodeIPs, err := nodeAddresses(ctx)
	if err != nil {
		return ciliumStandIn{}, err
	}
	want, err := translateCiliumPolicy(&cnp, nodeIPs)
	if err != nil {
		return ciliumStandIn{}, fmt.Errorf("the store's CiliumNetworkPolicy: %w", err)
	}
	k, err := labKube()
	if err != nil {
		return ciliumStandIn{}, err
	}
	got, err := k.clientset.NetworkingV1().NetworkPolicies(platformNamespace).Get(ctx, want.policy.Name, metav1.GetOptions{})
	if err != nil {
		return ciliumStandIn{}, fmt.Errorf("the store's stand-in %s/%s (`agentlab platform` applies it): %w", platformNamespace, want.policy.Name, err)
	}
	if !equality.Semantic.DeepEqual(got.Spec, want.policy.Spec) {
		wantJSON, _ := json.Marshal(want.policy.Spec)
		gotJSON, _ := json.Marshal(got.Spec)
		return ciliumStandIn{}, fmt.Errorf("the stand-in %s is not the live CiliumNetworkPolicy's translation — rerun `agentlab platform`:\n  live: %s\n  want: %s", got.Name, gotJSON, wantJSON)
	}
	return want, nil
}

// proveValkeyStorePolicy is the store proof: the stand-in is the live
// policy's translation, a probe with a client's labels is answered by the
// store, a third pod in its namespace is refused, and the real clients
// work through it — muster serving a session from its stores, klaus-gateway
// (with platform.klausGateway) Ready, its readiness probe a routing-store
// read.
func proveValkeyStorePolicy(ctx context.Context, cfg *config.Config, token string) (string, error) {
	step("The store's policy: CiliumNetworkPolicy %s/%s and its NetworkPolicy stand-in", platformNamespace, valkeyService)
	standIn, err := liveStandIn(ctx)
	if err != nil {
		return "", err
	}
	var peers []string
	for _, r := range standIn.policy.Spec.Ingress {
		for _, p := range r.From {
			switch {
			case p.PodSelector != nil && len(p.PodSelector.MatchLabels) > 0:
				peers = append(peers, k8slabels.Set(p.PodSelector.MatchLabels).String())
			case p.IPBlock != nil:
				peers = append(peers, p.IPBlock.CIDR)
			}
		}
	}
	note("%s admits %s", standIn.policy.Name, strings.Join(peers, "; "))
	for _, u := range standIn.unheld {
		note("not proven in the lab: %s", u)
	}
	var answers []string
	for _, probe := range valkeyProbes() {
		if by, err := probeAdoptedBy(ctx, platformNamespace, probe.labels); err != nil {
			return "", err
		} else if by != "" {
			return "", fmt.Errorf("the probe labels %s would join %s: the proof would disturb the platform", k8slabels.Set(probe.labels), by)
		}
		step("A pod in %s labelled %s: PING to %s:%d", platformNamespace, k8slabels.Set(probe.labels), valkeyService, valkeyPort)
		pod := probePod(probe.name, probeImage, []string{"sh", "-c", valkeyProbeScript()})
		maps.Copy(pod.Labels, probe.labels)
		restrictPodSecurity(pod)
		out, err := runPod(ctx, platformNamespace, pod, probePodTimeout)
		if err != nil {
			return "", fmt.Errorf("the store probe %s: %w\n%.300s", probe.name, err, strings.TrimSpace(out))
		}
		reply, answered, err := valkeyAnswered(out)
		if err != nil {
			return "", err
		}
		switch {
		case probe.admit && !answered:
			return "", fmt.Errorf("a pod labelled %s, which the store's policy admits, got no answer from %s: %.300s", k8slabels.Set(probe.labels), valkeyService, reply)
		case !probe.admit && answered:
			return "", fmt.Errorf("a pod labelled %s reached %s (%s) although the store's policy admits muster and klaus-gateway only — the policy is not enforced", k8slabels.Set(probe.labels), valkeyService, reply)
		}
		if answered {
			note("answered: %s", reply)
			answers = append(answers, fmt.Sprintf("%s answered", probe.labels[appNameLabel]))
		} else {
			note("refused: no answer within %ds", netpolProbeTimeout)
			answers = append(answers, "a third pod refused")
		}
	}
	step("The real clients through the policy")
	session, err := openMusterSession(cfg, token, "platform-test-valkey-store")
	if err != nil {
		return "", fmt.Errorf("muster's session under the store's policy: %w", err)
	}
	if _, err := agentManagerModelConfig(session); err != nil {
		return "", fmt.Errorf("a tool call through muster under the store's policy: %w", err)
	}
	note("muster served a session and a tool call (its session and token stores are the Valkey's)")
	clients := "muster served a session"
	if cfg.KlausGatewayEnabled() {
		k, err := labKube()
		if err != nil {
			return "", err
		}
		d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, klausGatewayComponent, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("the klaus-gateway Deployment: %w", err)
		}
		if d.Status.ReadyReplicas < 1 {
			return "", fmt.Errorf("klaus-gateway has no Ready pod under the store's policy; its readiness probe reads the routing store")
		}
		note("klaus-gateway: %d Ready (its readiness probe reads the routing store); a Slack turn through it is klaus-gateway-test's step 9", d.Status.ReadyReplicas)
		clients += ", klaus-gateway Ready on its routing store"
	}
	return fmt.Sprintf("%s (%s); %s", strings.Join(answers, ", "), standIn.policy.Name, clients), nil
}
