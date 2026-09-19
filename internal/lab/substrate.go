package lab

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
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

// workerPoolArchLabel is the node label the WorkerPool's CPU feature-set pin
// names (kagent.substrateWorkerPool.template.nodeSelector). One pool runs one
// CPU feature set — a gVisor checkpoint restores only where the CPU carries
// every feature it recorded — so the chart pins the pool, at the fleet's
// amd64; the lab is the arm64 installation its UPGRADE.md describes, and on
// an Apple Silicon host that default matches no node at all.
const workerPoolArchLabel = "kubernetes.io/arch"

// workerPoolArch is the binary's architecture: the fallback for a render
// without a cluster, and not the pin itself — the devctl Makefile's `make
// build` cross-builds amd64 even on an arm64 Mac (Makefile.gen.go.mk), so a
// pin taken from it would recreate the very mismatch this avoids.
func workerPoolArch() string { return runtime.GOARCH }

// clusterWorkerPoolArch is the architecture of the node the pool's workers
// would land on — what `agentlab platform` renders the pin from. A node the
// lab cannot read leaves the binary's, which on a cross-built agentlab is the
// wrong one, so the fallback says so rather than pinning in silence.
func clusterWorkerPoolArch(ctx context.Context) string {
	arch, err := nodeArch(ctx)
	if err != nil {
		note("cannot read the node's architecture (%v); the WorkerPool is pinned to this binary's %s, and `agentlab platform` refuses the install if the node disagrees", err, workerPoolArch())
		return workerPoolArch()
	}
	return arch
}

// nodeArch is the architecture the cluster's nodes carry, or an error saying
// why none could be read.
func nodeArch(ctx context.Context) (string, error) {
	gvr, err := gvrFor("nodes")
	if err != nil {
		return "", err
	}
	nodes, err := listObjects(ctx, gvr, "", "")
	if err != nil {
		return "", err
	}
	for _, node := range nodes {
		if arch := node.GetLabels()[workerPoolArchLabel]; arch != "" {
			return arch, nil
		}
	}
	return "", fmt.Errorf("no node carries the label %s", workerPoolArchLabel)
}

// renderedWorkerPoolArch is the architecture the WorkerPool will carry ON THE
// CLUSTER, read off the kagent component's render — the object helm-controller
// is about to apply, so it answers for the chart's own default and for what the
// chart does with the lab's values, not merely for what the lab asked. That
// matters: the kagent values are a free-form map, so a chart that renames or
// stops honouring the key leaves the lab's pin inert and its amd64 default on
// the pool, which no reading of the lab's values could ever notice.
//
// Empty when the pool is not in the render — the 3.x line, a component whose
// render failed (noted, not fatal), a chart that creates no pool.
func renderedWorkerPoolArch(renders map[string]string) string {
	objs, err := decodeManifests([]byte(renders[componentKagent]))
	if err != nil {
		return ""
	}
	for _, obj := range objs {
		if obj.GetKind() != workerPoolKind {
			continue
		}
		arch, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "nodeSelector", workerPoolArchLabel)
		if arch != "" {
			return arch
		}
	}
	return ""
}

// workerPoolKind is the Substrate object the kagent chart renders the pool as.
const workerPoolKind = "WorkerPool"

// preflightWorkerPoolArch refuses a cluster whose nodes do not carry the
// architecture the WorkerPool is about to be created with. Nothing else would
// say it: the pool's workers are ate-controller's, not the Helm release's, so
// the release goes Ready while they sit Pending and the wedge only surfaces
// minutes later as agent turns that never finish.
func preflightWorkerPoolArch(ctx context.Context, renders map[string]string) error {
	pinned := renderedWorkerPoolArch(renders)
	if pinned == "" {
		return nil
	}
	gvr, err := gvrFor("nodes")
	if err != nil {
		return err
	}
	nodes, err := listObjects(ctx, gvr, "", "")
	if err != nil {
		return err
	}
	// A node that carries the pin is enough — that is where the workers land.
	var carry []string
	for _, node := range nodes {
		arch := node.GetLabels()[workerPoolArchLabel]
		if arch == pinned {
			return nil
		}
		carry = append(carry, fmt.Sprintf("%s (%s)", node.GetName(), orNone(arch)))
	}
	if len(carry) == 0 {
		return nil
	}
	return fmt.Errorf("the WorkerPool the chart is about to create is pinned to %s=%s, which no node of this cluster carries: %s\n"+
		"Its gVisor workers would stay Pending and every agent turn would wait for a worker that never comes. The lab pins the\n"+
		"pool to the node's own architecture, so either a platform.valuesFiles overlay sets\n"+
		"kagent.substrateWorkerPool.template.nodeSelector, or this chart version no longer takes that key and left its own\n"+
		"default on the pool — check `kagent.substrateWorkerPool.template` against the chart, then `agentlab platform`",
		workerPoolArchLabel, pinned, strings.Join(carry, ", "))
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

// imageKey is a container's image field as the apiserver hands it back.
const imageKey = "image"

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
	args, _ := ateletContainer(ds)["args"].([]any)
	var missing []string
	for _, want := range ateletImageCacheArgs {
		if !slices.Contains(args, any(want)) {
			missing = append(missing, want)
		}
	}
	return missing
}

// ateletContainer is the DaemonSet's atelet container as the apiserver hands
// it back; nil when the pod template has none.
func ateletContainer(ds *unstructured.Unstructured) map[string]any {
	containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
	for _, c := range containers {
		if container, _ := c.(map[string]any); container[nameKey] == ateletDaemonSet {
			return container
		}
	}
	return nil
}

// The two halves of Agent Substrate, held to one release (agentlab#187). The
// atelet is the chart's `components.substrate`; the WorkerPool's worker image
// (`spec.workerImage`, ateom-gvisor) is the kagent chart's stamp — kagent's
// own Substrate pin, forwarded by the meta chart. A meta chart whose kagent
// range admits a kagent from another Substrate release than its substrate
// range installs green and boots no golden actor: a worker looks for the
// actor bundles laid out as its own release writes them, the atelet writes
// its release's layout, every compile the atelet asks of the worker fails
// inside the worker, the AgentTemplate sits at ActorTemplatePending ("golden
// snapshot compiling"), `agentlab up` and the platform releases stay green,
// `agents-test` burns its five minutes and only the atelet log says why. The
// release compared is the image tag's major.minor: the Substrate line
// releases stable semver of its own, a patch is carried patches or a rebuild
// on the same upstream pin and never changes the worker/atelet contract, a
// re-pin onto another upstream release is at least a minor.

// workerPoolsResource is Substrate's WorkerPool API, resolved through
// discovery like every custom kind the lab reads (never pinned here).
const workerPoolsResource = "workerpools.ate.dev"

// substrateImages is one WorkerPool's half of the pair next to the atelet's.
type substrateImages struct {
	atelet  string // the atelet container's image on the DaemonSet
	pool    string // the WorkerPool, namespace/name
	worker  string // its spec.workerImage
	release string // the Substrate release both are on
}

func (s substrateImages) String() string {
	return fmt.Sprintf("Substrate %s: atelet %s, WorkerPool %s workers %s", s.release, s.atelet, s.pool, s.worker)
}

// proveSubstrateLine reads the atelet's image off its DaemonSet and the
// worker image off every WorkerPool in the kagent namespace and refuses a
// lab whose halves are different Substrate releases, naming both images and
// the remedy (substrateSkewRemedy). Cheap — two reads — so `agentlab
// platform` runs it after the install and `platform-test` asserts it.
func proveSubstrateLine(ctx context.Context, remedy string) ([]substrateImages, error) {
	gvr, err := gvrFor("daemonsets.apps")
	if err != nil {
		return nil, err
	}
	ds, err := getObject(ctx, gvr, substrateNamespace, ateletDaemonSet)
	if err != nil {
		return nil, fmt.Errorf("reading the %s DaemonSet in %s: %w", ateletDaemonSet, substrateNamespace, err)
	}
	atelet, _ := ateletContainer(ds)[imageKey].(string)
	if atelet == "" {
		return nil, fmt.Errorf("the %s DaemonSet in %s has no %s container", ateletDaemonSet, substrateNamespace, ateletDaemonSet)
	}
	ateletRelease, err := substrateReleaseOf(atelet)
	if err != nil {
		return nil, fmt.Errorf("the %s DaemonSet: %w", ateletDaemonSet, err)
	}
	poolGVR, err := gvrFor(workerPoolsResource)
	if err != nil {
		return nil, err
	}
	pools, err := listObjects(ctx, poolGVR, kagentNamespace, "")
	if err != nil {
		return nil, fmt.Errorf("listing the WorkerPools in %s: %w", kagentNamespace, err)
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("no WorkerPool in %s — the kagent chart creates it (substrateWorkerPool.create) and the platform Harness runs its actors on it; `kubectl -n %s get helmrelease kagent`", kagentNamespace, kagentNamespace)
	}
	var images []substrateImages
	for _, pool := range pools {
		name := pool.GetNamespace() + "/" + pool.GetName()
		worker, _, _ := unstructured.NestedString(pool.Object, "spec", "workerImage")
		if worker == "" {
			return nil, fmt.Errorf("WorkerPool %s names no spec.workerImage", name)
		}
		release, err := substrateReleaseOf(worker)
		if err != nil {
			return nil, fmt.Errorf("WorkerPool %s: %w", name, err)
		}
		if release != ateletRelease {
			return nil, fmt.Errorf("the two halves of Agent Substrate are different releases: atelet %s (Substrate %s, the chart's components.substrate) and the workers of WorkerPool %s %s (Substrate %s, the worker image the kagent chart stamps) — "+
				"a worker from another release than its atelet looks for actor bundles the atelet does not write, so no golden actor boots and every AgentTemplate stays ActorTemplatePending (`kubectl -n %s logs ds/%s` has the failing compiles). %s",
				atelet, ateletRelease, name, worker, release, substrateNamespace, ateletDaemonSet, remedy)
		}
		images = append(images, substrateImages{atelet: atelet, pool: name, worker: worker, release: release})
	}
	return images, nil
}

// substrateReleaseOf is the Substrate release an image is from: its tag's
// major.minor (1.0 from gsoci.azurecr.io/giantswarm/substrate/atelet:1.0.3,
// and from a dev build 1.0.1-dev.… of the line). An image with no tag or a
// tag that is no version cannot be placed and is refused by name.
func substrateReleaseOf(image string) (string, error) {
	ref, _, _ := strings.Cut(image, "@")
	name := ref[strings.LastIndex(ref, "/")+1:]
	_, tag, ok := strings.Cut(name, ":")
	if !ok || tag == "" {
		return "", fmt.Errorf("image %s names no tag to read its Substrate release off", image)
	}
	v, err := semver.NewVersion(tag)
	if err != nil {
		return "", fmt.Errorf("image %s: tag %q is not a version (%v)", image, tag, err)
	}
	return fmt.Sprintf("%d.%d", v.Major(), v.Minor()), nil
}

// substrateSkewRemedy is the fix a skewed lab is told, for the way this lab
// selects its chart: a release pin moves to a chart that pins kagent and
// Substrate together — the default when the lab is not on it, otherwise a
// release of the person's choosing; a checkout or the dev channel is the
// chart's own to align.
func substrateSkewRemedy(cfg *config.Config) string {
	switch {
	case cfg.Platform.ChartPath != "":
		return fmt.Sprintf("The chart at %s pins them apart: align components.kagent with components.substrate (the worker image the kagent release stamps must be the release the substrate range installs), then `agentlab platform`.", cfg.Platform.ChartPath)
	case cfg.Platform.ChartBranch != "":
		return fmt.Sprintf("The dev channel of %s pins them apart: align components.kagent with components.substrate on the branch (the worker image the kagent release stamps must be the release the substrate range installs), then `agentlab platform`.", cfg.Platform.ChartBranch)
	case cfg.Platform.ChartVersion != config.DefaultChartVersion:
		return fmt.Sprintf("agent-platform %s pins them apart; the release this agentlab was verified with pins them together: `agentlab configure --defaults --chart-version %s && agentlab platform`.", cfg.Platform.ChartVersion, config.DefaultChartVersion)
	default:
		return fmt.Sprintf("agent-platform %s pins them apart: pick a release whose components.kagent range admits only kagent releases on the Substrate release components.substrate installs (`helm show values %s --version <version>`), then `agentlab configure --defaults --chart-version <version> && agentlab platform`.", cfg.Platform.ChartVersion, config.ChartRepository)
	}
}
