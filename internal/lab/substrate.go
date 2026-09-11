package lab

import (
	"context"
	"fmt"
)

// Agent Substrate is kagent API v2's actor runtime: WorkerPools of sandboxed
// (gVisor) worker pods that ate-controller schedules actors onto,
// ate-api-server as their control plane, atelet as the per-node agent and
// atenet as the actors' ingress/egress data plane. The agent-platform chart
// ships it — the `substrate-crds` and `substrate` components follow
// `components.kagent`, the connectivity release's hook Job mints the CA/JWT
// pools and ate-api-server's authentication config the substrate chart mounts
// but does not render, the kagent chart creates the WorkerPool, the
// connectivity chart the platform Harness — so the lab installs nothing of
// it: one Helm owner, the same objects an installation has (docs/platform.md
// "The 4.x line"). The lab's part is what the chart cannot do: the kind
// cluster carries the apiserver gates Substrate needs (kind-config.yaml.tmpl)
// and the boot refuses a cluster that lacks them before anything installs.

// The namespaces the substrate chart is fixed to (ate-controller's Role names
// ate-system; the podcertificate-controller runs in its own) and the release
// name under which the chart installs the control plane — the roster entry
// the lab reads to know a chart ships Substrate (platformRoster.shipsSubstrate).
const (
	substrateNamespace         = "ate-system"
	podCertControllerNamespace = "podcertificate-controller-system"
	substrateRelease           = "substrate"
)

// podCertificateAPIPath is the API Substrate cannot start a pod without: the
// PodCertificateRequest API behind the certificates.k8s.io/v1beta1 gate.
const podCertificateAPIPath = "/apis/certificates.k8s.io/v1beta1"

// preflightPodCertificateAPI refuses a cluster whose apiserver does not
// serve certificates.k8s.io/v1beta1 — one created before the lab's kind
// config turned the gates on. Feature gates are fixed at `kind create`, so
// the only fix is a new cluster; said here, before anything is installed (the
// connectivity chart's live render refuses the same cluster, minutes later
// and inside a failed HelmRelease).
func preflightPodCertificateAPI(ctx context.Context) error {
	if _, err := rawGet(ctx, podCertificateAPIPath); err != nil {
		return fmt.Errorf("the apiserver does not serve certificates.k8s.io/v1beta1 (%v):\n"+
			"Agent Substrate needs the PodCertificateRequest and ClusterTrustBundle gates the lab's kind config turns on, and this\n"+
			"cluster predates them — feature gates are fixed at kind create, so run `agentlab down && agentlab up`", err)
	}
	return nil
}
