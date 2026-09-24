package lab

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/giantswarm/agentlab/internal/config"
)

// The klaus-gateway wiring (platform.klausGateway in agentlab.yaml):
// Swarmgeist (github.com/giantswarm/klaus-gateway), the fleet's Slack bridge,
// runs as the meta chart's in-cluster component — components.klaus-gateway,
// turned on by the lab's values template — the way every installation runs
// it, next to the host-mode gateway `agentlab klaus-gateway-test` starts for
// the public leg (klausgatewaytest.go). The component's values are the
// installation's shape on a kind cluster:
//
//   - a2a on the in-cluster controller target: the agentgateway data-plane
//     Service over plaintext h2c (the JWT policy validates the forwarded
//     token there), AgentTemplates from the kagent namespace, the proof's
//     fixture as the default agent (the Slack adapter refuses to start
//     without one; the name is resolved on a turn, never at start);
//   - the Slack adapter — the gateway's only channel — in events mode on a
//     placeholder credentials Secret, its Web API base patched to the lab's
//     Service in front of the proof's fake Slack Web API (the chart has no
//     value for it; postrenderers.go): events mode makes no outbound call at
//     start, and no workspace ever answers, which is why a real OBO sign-in
//     is out of the lab's reach (klausgatewaytest_component.go);
//   - OBO with the link store in a Kubernetes Secret (obo.store: secret, the
//     store the fleet runs since klaus-gateway 1.3.0): the chart renders the
//     empty link Secret with a Role scoped to it by resourceNames, and the
//     lab pre-creates the keys Secret the chart mounts (obo.existingSecret)
//     — generated once and then left alone, because rotating store-key would
//     orphan every sealed link;
//   - the ServiceMonitor following platform.observability (the chart's
//     default `true` fails the HelmRelease on a lab without the Prometheus
//     Operator); the chart's edge route for the channels stays off — the
//     public leg is the host-mode proof's.
//
// A build of the checkout swaps in through the dev-image loop
// (platform.devImages.klaus-gateway, devimages.go). What the wiring proves is
// the component half of `agentlab klaus-gateway-test`.

// klausGatewayComponent is the meta chart's component name — also the
// HelmRelease, the Deployment, its container, the Service and the
// ServiceAccount the chart renders under it (fullnameOverride is unset).
const klausGatewayComponent = "klaus-gateway"

// klausGatewayInClusterTarget is the kagent controller's gRPC target as the
// component dials it: the agentgateway data-plane Service in the platform
// namespace, plaintext h2c, the chart's own default for the lab's layout.
const klausGatewayInClusterTarget = "grpc://agentgateway." + platformNamespace + ".svc.cluster.local:8080"

// The two Secrets the lab creates for the component in the platform
// namespace, and their keys as the chart mounts them.
const (
	// klausGatewaySlackPlaceholder carries the Slack adapter's credentials
	// (slack.secretName): placeholders no workspace answers. Neither value
	// is shaped like a Slack credential (no Slack token prefix), so a secret
	// scanner reading this file or the Secret sees a placeholder, not a key.
	klausGatewaySlackPlaceholder = "agentlab-klaus-gateway-slack"
	slackBotTokenKey             = "bot-token"
	slackSigningSecretKey        = "signing-secret"
	slackBotPlaceholder          = "agentlab-placeholder-bot-token"
	slackSigningPlaceholder      = "agentlab-placeholder-signing-secret"
	// klausGatewayOBOKeys carries the OBO keys (obo.existingSecret): the
	// HMAC key signing link state and the AES-256 key sealing the links.
	klausGatewayOBOKeys = "agentlab-klaus-gateway-obo"
	oboStateKeyKey      = "state-key"
	oboStoreKeyKey      = "store-key"
)

// The component's Slack Web API: the selector-less Service
// klausGatewaySlackAPIService, which `agentlab klaus-gateway-test` points at
// its fake's container on the kind network (pointFakeSlackService) while it
// runs. Outside a
// proof nothing answers behind it, and nothing calls it: the Events API
// adapter calls the Web API only for a Slack event, and the proof is the
// only one sending those.
const (
	klausGatewaySlackAPIService = "agentlab-slack-api"
	slackAPIBaseEnv             = "KLAUS_GATEWAY_SLACK_API_BASE"
	klausGatewaySlackAPIHost    = "http://" + klausGatewaySlackAPIService + "." + platformNamespace + ".svc.cluster.local"
	klausGatewaySlackAPIBase    = klausGatewaySlackAPIHost + slackAPIPath
)

// fakeSlackForPods points the component's Slack Web API Service at the fake's
// container and proves a pod reaches it through the Service before the proof
// starts anything else — found here in seconds, not as a failed turn at the
// end of the run. The returned func removes the Service.
func fakeSlackForPods(cfg *config.Config, ip string, port int) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), probePodTimeout)
	defer cancel()
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	remove, err := pointFakeSlackService(ctx, k, ip, port)
	if err != nil {
		return nil, err
	}
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	health := klausGatewaySlackAPIHost + slackHealthPath
	out, err := runProbePod(ctx, platformNamespace, klausGatewaySlackAPIService+"-preflight", probeImage,
		[]string{"wget", "-qO-", "-T", "5", health}, probePodTimeout)
	if err == nil && strings.TrimSpace(out) == "ok" {
		note("a pod reaches the fake Slack Web API through %s (%s:%d on the %s network)", health, ip, port, kindDockerNetwork)
		return remove, nil
	}
	remove()
	return nil, fmt.Errorf("pods cannot reach the fake Slack Web API the klaus-gateway component calls (%s -> %s:%d, a container on the %s network): %s; probe output: %.300s",
		health, ip, port, kindDockerNetwork, probeFailure(err, out), strings.TrimSpace(out))
}

// probeFailure words why a probe pod's fetch failed: its error, or what the
// fetch printed instead of the answer.
func probeFailure(err error, out string) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("the fetch answered %q, not ok", excerpt(strings.TrimSpace(out), 80))
}

// pointFakeSlackService creates the component's Slack Web API Service and the
// EndpointSlice behind it: the fake container's address and port. A leftover of an aborted run is replaced; the returned func removes
// both.
func pointFakeSlackService(ctx context.Context, k *kubeClients, hostIP string, port int) (func(), error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("the fake Slack Web API's port %d is no TCP port", port)
	}
	p := int32(port)
	services := k.clientset.CoreV1().Services(platformNamespace)
	endpointSlices := k.clientset.DiscoveryV1().EndpointSlices(platformNamespace)
	remove := func() {
		for _, err := range []error{
			endpointSlices.Delete(context.Background(), klausGatewaySlackAPIService, metav1.DeleteOptions{}),
			services.Delete(context.Background(), klausGatewaySlackAPIService, metav1.DeleteOptions{}),
		} {
			if err != nil && !apierrors.IsNotFound(err) {
				note("cleanup: the Slack Web API Service %s: %v", klausGatewaySlackAPIService, err)
			}
		}
	}
	remove()
	labels := map[string]string{managedByLabel: managedByAgentlabValue}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: klausGatewaySlackAPIService, Namespace: platformNamespace, Labels: labels},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
			Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(p),
		}}},
	}
	if _, err := services.Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating the Slack Web API Service %s/%s: %w", platformNamespace, klausGatewaySlackAPIService, err)
	}
	name, proto := "http", corev1.ProtocolTCP
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: klausGatewaySlackAPIService, Namespace: platformNamespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: klausGatewaySlackAPIService, managedByLabel: managedByAgentlabValue}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{hostIP}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &name, Protocol: &proto, Port: &p}},
	}
	if _, err := endpointSlices.Create(ctx, slice, metav1.CreateOptions{}); err != nil {
		remove()
		return nil, fmt.Errorf("creating the EndpointSlice of %s/%s: %w", platformNamespace, klausGatewaySlackAPIService, err)
	}
	note("Service %s/%s → %s:%d (the fake Slack Web API)", platformNamespace, klausGatewaySlackAPIService, hostIP, port)
	return remove, nil
}

// klausGatewayLinksSecret is the chart's link Secret of the Secret backend
// (`<release>-obo-links`, rendered empty with helm.sh/resource-policy: keep)
// and the name of the Role and RoleBinding granting the gateway get/update/
// patch on exactly that Secret.
const klausGatewayLinksSecret = klausGatewayComponent + "-obo-links"

// klausGatewayValues is what the values template's `klausGateway:` block
// needs beyond the config: the names above and the URLs the component is
// configured with.
type klausGatewayValues struct {
	// A2ATarget is klausGatewayInClusterTarget; AgentNamespace the namespace
	// whose AgentTemplates the component serves (kagent).
	A2ATarget      string
	AgentNamespace string
	// DefaultAgent is the AgentTemplate a channel turn runs on when it names
	// none: the proof's fixture (klausgatewaytest.go), which exists while
	// the proof runs.
	DefaultAgent string
	// SlackSecret and KeysSecret are the two Secrets above.
	SlackSecret string
	KeysSecret  string
	// MusterURL is muster's public URL, the OBO authorization server the
	// linker discovers lazily (RFC 8414) on the first link — never in the
	// lab, see the header.
	MusterURL string
	// CallbackBaseURL is the component's public base URL as the OBO redirect
	// URI and CIMD client_id derive from it: the agentgateway hostname
	// without a port, whatever platform.gatewayPort is. The connectivity
	// chart derives the hostname of the OBO HTTPRoute it renders on the edge
	// from this value, and a Gateway API hostname cannot carry a port; the
	// route matches the Host header's name part either way, and no link
	// ever completes here.
	CallbackBaseURL string
}

// klausGatewayValuesFor is the `klausGateway:` block's data for this lab.
func klausGatewayValuesFor(cfg *config.Config) klausGatewayValues {
	return klausGatewayValues{
		A2ATarget:       klausGatewayInClusterTarget,
		AgentNamespace:  kagentNamespace,
		DefaultAgent:    klausGatewayTestAgent,
		SlackSecret:     klausGatewaySlackPlaceholder,
		KeysSecret:      klausGatewayOBOKeys,
		MusterURL:       cfg.MusterBaseURL(),
		CallbackBaseURL: "https://agentgateway." + cfg.Platform.Domain,
	}
}

// ensureKlausGatewaySecrets creates the component's two Secrets once: the
// placeholder Slack credentials, and the OBO keys — random, generated here,
// and never regenerated while the Secret exists (the store key seals every
// link in the link Secret; a new key would make them unreadable). Before the
// install: the chart mounts both without `optional`.
func ensureKlausGatewaySecrets(ctx context.Context) error {
	for _, s := range []struct {
		name string
		data map[string][]byte
		what string
	}{
		{klausGatewaySlackPlaceholder, map[string][]byte{
			slackBotTokenKey:      []byte(slackBotPlaceholder),
			slackSigningSecretKey: []byte(slackSigningPlaceholder),
		}, "the placeholder Slack credentials (no workspace answers them)"},
		{klausGatewayOBOKeys, map[string][]byte{
			oboStateKeyKey: []byte(randHex(32)),
			oboStoreKeyKey: []byte(randBase64(32)),
		}, "the OBO keys (state-key, store-key; kept as long as the Secret exists)"},
	} {
		exists, err := objectExists(ctx, gvrSecrets, platformNamespace, s.name)
		if err != nil {
			return err
		}
		if exists {
			note("%s already exists, leaving it alone", s.name)
			continue
		}
		if err := ensureSecret(platformNamespace, s.name, corev1.SecretTypeOpaque, s.data); err != nil {
			return err
		}
		note("created %s: %s", s.name, s.what)
	}
	return nil
}

// klausGatewayHint is the platform-up summary for the klaus-gateway wiring.
func klausGatewayHint(cfg *config.Config) string {
	if !cfg.KlausGatewayEnabled() {
		return "  klaus-gateway is not wired (platform.klausGateway in agentlab.yaml; `agentlab configure --klaus-gateway` turns it on)."
	}
	dev := ""
	if ref, ok := cfg.Platform.DevImages[klausGatewayComponent]; ok {
		dev = fmt.Sprintf(" running the dev image %s", ref)
	}
	return fmt.Sprintf("  klaus-gateway: Swarmgeist as the meta chart's component%s — A2A on %s,\n"+
		"  Slack on a placeholder Secret, the OBO link store in Secret %s. Proof: `agentlab klaus-gateway-test`.",
		dev, klausGatewayInClusterTarget, klausGatewayLinksSecret)
}
