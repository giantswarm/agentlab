package lab

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// ateletImageCacheArgs is the lab's atelet image-cache policy, rendered into
// substrate.atelet.extraArgs (agent-platform-values.yaml.tmpl) and asserted
// on the DaemonSet by `agentlab platform-test` (proveAteletImageCachePolicy).
//
// atelet evicts cached images on every GC pass (--image-cache-gc-period, 5
// min) while the cache volume is at or above --image-cache-high-percent (85
// by default) and spares only records younger than --image-cache-min-age. In
// the lab the cache dir (/var/lib/ateom-gvisor/image-cache) sits on the kind
// node's root overlay, which is the host's disk — on a laptop above 85 % the
// Harness image is gone five minutes after every turn, the next turn pays a
// cold pull and unpack that overruns the atenet router's parked-request
// budget, and the agent stays "Working…" forever (agentlab#169). The lab
// therefore takes the host disk out of the policy: the watermark moves to
// 100 (eviction only when the host has < 1 % free, where nothing runs
// anyway; low must stay below high) and an absolute cap bounds the cache
// instead — 4 GiB holds some twenty Harness digests (~50 MiB compressed, 3
// layers), evicted oldest-first past that. The period and min-age keep the
// chart's defaults so the cap is enforced. The Substrate line's fix
// (eviction sparing live images, a budget that covers a cold start) retires
// this policy once the meta chart pins it.
var ateletImageCacheArgs = []string{
	"--image-cache-high-percent=100",
	"--image-cache-low-percent=99",
	"--image-cache-max-bytes=4294967296",
}

// ateletDaemonSet is the substrate chart's per-node agent (templates/atelet.yaml).
const ateletDaemonSet = "atelet"

// proveAteletImageCachePolicy asserts the atelet DaemonSet carries every flag
// of ateletImageCacheArgs and that its pods have rolled to that spec — the
// live half of the policy: values that render but never reach the node
// would leave the eviction in place. Returns the number of ready pods.
func proveAteletImageCachePolicy(ctx context.Context) (int64, error) {
	gvr, err := gvrFor("daemonsets.apps")
	if err != nil {
		return 0, err
	}
	ds, err := getObject(ctx, gvr, substrateNamespace, ateletDaemonSet)
	if err != nil {
		return 0, fmt.Errorf("reading the %s DaemonSet in %s: %w", ateletDaemonSet, substrateNamespace, err)
	}
	if missing := ateletArgsMissing(ds); len(missing) > 0 {
		return 0, fmt.Errorf("the %s DaemonSet lacks the lab's image-cache policy %v — a platform.valuesFiles overlay that sets substrate.atelet.extraArgs replaces the list (Helm merges maps, not lists); add the flags to the overlay's list or drop the key, then `agentlab platform`",
			ateletDaemonSet, missing)
	}
	desired, _, _ := unstructured.NestedInt64(ds.Object, "status", "desiredNumberScheduled")
	updated, _, _ := unstructured.NestedInt64(ds.Object, "status", "updatedNumberScheduled")
	ready, _, _ := unstructured.NestedInt64(ds.Object, "status", "numberReady")
	if desired == 0 || updated < desired || ready < desired {
		return 0, fmt.Errorf("the %s DaemonSet carries the policy but has not rolled to it (desired %d, updated %d, ready %d) — `kubectl -n %s rollout status ds/%s`",
			ateletDaemonSet, desired, updated, ready, substrateNamespace, ateletDaemonSet)
	}
	return ready, nil
}

// ateletArgsMissing is the policy flags the DaemonSet's atelet container does
// not carry, in policy order; empty when it carries them all.
func ateletArgsMissing(ds *unstructured.Unstructured) []string {
	var args []any
	containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
	for _, c := range containers {
		container, _ := c.(map[string]any)
		if container["name"] == ateletDaemonSet {
			args, _ = container["args"].([]any)
			break
		}
	}
	var missing []string
	for _, want := range ateletImageCacheArgs {
		if !slices.Contains(args, any(want)) {
			missing = append(missing, want)
		}
	}
	return missing
}
