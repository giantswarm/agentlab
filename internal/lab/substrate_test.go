package lab

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agentlab/internal/config"
)

// Substrate's WorkerPool as the fakes serve it (newFakeLab registers it with
// its mapper and the dynamic fake's list kinds); the version is the fakes'.
var (
	substrateGroupVersion = schema.GroupVersion{Group: "ate.dev", Version: "v1alpha1"}
	gvkWorkerPool         = substrateGroupVersion.WithKind("WorkerPool")
	gvrWorkerPools        = substrateGroupVersion.WithResource("workerpools")
)

// A cluster without the certificates.k8s.io/v1beta1 API is refused before
// anything installs, with the only fix — a new cluster — spelled out.
func TestSubstratePreflightWithoutTheAPI(t *testing.T) {
	newFakeLab(t)
	err := preflightPodCertificateAPI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "agentlab down && agentlab up") {
		t.Errorf("want the recreate hint, got %v", err)
	}
}

// archRenders is the kagent component render carrying a WorkerPool pinned to
// arch — what the preflight reads, since that is the object the chart is
// about to create. An empty arch renders the pool without a nodeSelector.
func archRenders(arch string) map[string]string {
	selector := ""
	if arch != "" {
		selector = fmt.Sprintf("\n    nodeSelector:\n      %s: %q", workerPoolArchLabel, arch)
	}
	return map[string]string{componentKagent: fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: kagent-controller
  namespace: kagent
---
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: kagent-default
  namespace: kagent
spec:
  replicas: 4
  template:%s
    resources:
      requests:
        cpu: 250m
`, selector)}
}

func archNode(name, arch string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{workerPoolArchLabel: arch}}}
}

// The pool's CPU feature-set pin is checked against the node its workers
// would land on: a pin no node carries is refused before the install, since
// the workers would only ever sit Pending and nothing else would say so.
func TestWorkerPoolArchPreflight(t *testing.T) {
	ctx := context.Background()

	newFakeLab(t, archNode("agentlab-control-plane", "arm64"))
	if err := preflightWorkerPoolArch(ctx, archRenders("arm64")); err != nil {
		t.Errorf("the node carries the pin: %v", err)
	}
	// A pool with no pin, and no pool at all — the 3.x line, or a component
	// whose render failed (noted, not fatal): nothing to check either way.
	if err := preflightWorkerPoolArch(ctx, archRenders("")); err != nil {
		t.Errorf("a pool without a pin: %v", err)
	}
	if err := preflightWorkerPoolArch(ctx, nil); err != nil {
		t.Errorf("without the kagent render: %v", err)
	}
	// The chart's own default on the pool is caught the same way — the point
	// of reading the render and not the lab's values.
	err := preflightWorkerPoolArch(ctx, archRenders("amd64"))
	if err == nil {
		t.Fatal("a pin no node carries must be refused")
	}
	for _, want := range []string{"amd64", "agentlab-control-plane (arm64)", "platform.valuesFiles"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in the refusal, got %v", want, err)
		}
	}

	// One node that carries it is enough — that is where the workers land.
	newFakeLab(t, archNode("amd", "amd64"), archNode("arm", "arm64"))
	if err := preflightWorkerPoolArch(ctx, archRenders("arm64")); err != nil {
		t.Errorf("a mixed cluster with a matching node: %v", err)
	}
}

// The arch the values pin comes from the node, never the binary: the devctl
// Makefile cross-builds amd64 even on an arm64 host, so GOARCH would pin the
// very architecture this refuses. Without a cluster it is the fallback.
func TestClusterWorkerPoolArch(t *testing.T) {
	newFakeLab(t, archNode("agentlab-control-plane", "arm64"))
	if got := clusterWorkerPoolArch(context.Background()); got != "arm64" {
		t.Errorf("clusterWorkerPoolArch() = %s, want the node's arm64", got)
	}
	newFakeLab(t)
	if got := clusterWorkerPoolArch(context.Background()); got != runtime.GOARCH {
		t.Errorf("without a node: clusterWorkerPoolArch() = %s, want the fallback %s", got, runtime.GOARCH)
	}
}

// testRegistryFlag is the lab registry rewrite the values render ahead of
// the image-cache policy (registryFlag on the default config).
const testRegistryFlag = "--localhost-registry-replacement=agentlab-registry:5000"

// The fakes' atelet DaemonSet: a sidecar next to the atelet container, and
// the Substrate release the proofs' images are from.
const (
	testSidecarContainer = "sidecar"
	testSubstrateRelease = "1.0"
)

// anySlice is a []string as the YAML decoder hands a list back.
func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func TestAteletArgsMissing(t *testing.T) {
	ds := func(args ...string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			fieldSpec: map[string]any{"template": map[string]any{fieldSpec: map[string]any{
				"containers": []any{
					map[string]any{nameKey: testSidecarContainer, argsKey: anySlice(ateletImageCacheArgs)},
					map[string]any{nameKey: ateletDaemonSet, argsKey: anySlice(args)},
				},
			}}},
		}}
	}
	registry := testRegistryFlag
	// The rendered list: the registry flag, then the policy.
	if got := ateletArgsMissing(ds(append([]string{registry}, ateletImageCacheArgs...)...)); len(got) != 0 {
		t.Errorf("a DaemonSet carrying the policy: missing = %v, want none", got)
	}
	// A platform.valuesFiles overlay that replaced the list: every policy flag
	// is reported (another container's args do not count), in policy order.
	if got := ateletArgsMissing(ds(registry)); !reflect.DeepEqual(got, ateletImageCacheArgs) {
		t.Errorf("a DaemonSet without the policy: missing = %v, want %v", got, ateletImageCacheArgs)
	}
	// A partial list names what is left.
	if got := ateletArgsMissing(ds(registry, ateletImageCacheArgs[0])); !reflect.DeepEqual(got, ateletImageCacheArgs[1:]) {
		t.Errorf("a DaemonSet with one policy flag: missing = %v, want %v", got, ateletImageCacheArgs[1:])
	}
}

// The Substrate release of an image is its tag's major.minor — a patch of
// the line shares it, a dev build of the line reads as the release it derives
// from; an image that names no version cannot be placed and is refused by
// name.
func TestSubstrateReleaseOf(t *testing.T) {
	for image, want := range map[string]string{
		"gsoci.azurecr.io/giantswarm/substrate/atelet:1.0.3":                                                                         testSubstrateRelease,
		"gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.0.0":                                                                   testSubstrateRelease,
		"gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.1.0":                                                                   "1.1",
		"localhost:5000/substrate/ateom-gvisor:1.0.1-dev.giantswarm.2026-09-19.10-00-00.h1234567":                                    testSubstrateRelease,
		"gsoci.azurecr.io/giantswarm/substrate/atelet:1.0.3@sha256:0000000000000000000000000000000000000000000000000000000000000000": testSubstrateRelease,
		"gsoci.azurecr.io/giantswarm/substrate/atelet:v1.2.3":                                                                        "1.2",
	} {
		if got, err := substrateReleaseOf(image); err != nil || got != want {
			t.Errorf("substrateReleaseOf(%q) = %q, %v; want %q", image, got, err, want)
		}
	}
	for _, image := range []string{
		"gsoci.azurecr.io/giantswarm/substrate/atelet@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"gsoci.azurecr.io/giantswarm/substrate/atelet:latest",
		"localhost:5000/atelet",
	} {
		if _, err := substrateReleaseOf(image); err == nil || !strings.Contains(err.Error(), image) {
			t.Errorf("substrateReleaseOf(%q): want a refusal naming the image, got %v", image, err)
		}
	}
}

// ateletDS is the atelet DaemonSet as the apiserver hands it back, its atelet
// container on the given image.
func ateletDS(image string) *unstructured.Unstructured {
	ds := &unstructured.Unstructured{Object: map[string]any{
		fieldSpec: map[string]any{"template": map[string]any{fieldSpec: map[string]any{
			"containers": []any{
				map[string]any{nameKey: testSidecarContainer, imageKey: "gsoci.azurecr.io/giantswarm/substrate/sidecar:9.9.9"},
				map[string]any{nameKey: ateletDaemonSet, imageKey: image, argsKey: anySlice(ateletImageCacheArgs)},
			},
		}}},
	}}
	ds.SetGroupVersionKind(schema.GroupVersion{Group: "apps", Version: "v1"}.WithKind("DaemonSet"))
	ds.SetNamespace(substrateNamespace)
	ds.SetName(ateletDaemonSet)
	return ds
}

// workerPool is a WorkerPool of the kagent namespace whose workers run the
// given image.
func workerPool(name, image string) *unstructured.Unstructured {
	pool := customObject(gvkWorkerPool, kagentNamespace, name, nil)
	_ = unstructured.SetNestedField(pool.Object, image, fieldSpec, "workerImage")
	return pool
}

// The atelet and the WorkerPool's workers on one Substrate release pass, with
// both images reported; the line's patch releases may differ. Two releases
// are refused naming both images, the symptom and the remedy; a lab without a
// WorkerPool cannot boot an actor either and says so.
func TestProveSubstrateLine(t *testing.T) {
	const (
		atelet = "gsoci.azurecr.io/giantswarm/substrate/atelet:1.0.3"
		worker = "gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.0.0"
		skewed = "gsoci.azurecr.io/giantswarm/substrate/ateom-gvisor:1.1.0"
		remedy = "agentlab configure --defaults --chart-version 9.9.9 && agentlab platform"
	)
	ctx := context.Background()

	newFakeLab(t, ateletDS(atelet), workerPool(testWorkerPool, worker))
	images, err := proveSubstrateLine(ctx, remedy)
	if err != nil {
		t.Fatalf("one release: %v", err)
	}
	want := []substrateImages{{atelet: atelet, pool: kagentNamespace + "/kagent-default", worker: worker, release: testSubstrateRelease}}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("images = %+v, want %+v", images, want)
	}
	if got := images[0].String(); !strings.Contains(got, "Substrate 1.0") || !strings.Contains(got, atelet) || !strings.Contains(got, worker) {
		t.Errorf("the report names the release and both images, got %q", got)
	}

	newFakeLab(t, ateletDS(atelet), workerPool(testWorkerPool, worker), workerPool("kagent-other", skewed))
	_, err = proveSubstrateLine(ctx, remedy)
	if err == nil {
		t.Fatal("two releases: want a refusal")
	}
	for _, want := range []string{atelet, "Substrate 1.0", skewed, "Substrate 1.1", kagentNamespace + "/kagent-other", "no golden actor boots", "kubectl -n " + substrateNamespace + " logs ds/" + ateletDaemonSet, remedy} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q:\n%v", want, err)
		}
	}

	newFakeLab(t, ateletDS(atelet))
	if _, err := proveSubstrateLine(ctx, remedy); err == nil || !strings.Contains(err.Error(), "no WorkerPool in "+kagentNamespace) {
		t.Errorf("no WorkerPool: want the refusal, got %v", err)
	}

	newFakeLab(t, workerPool(testWorkerPool, worker))
	if _, err := proveSubstrateLine(ctx, remedy); err == nil || !strings.Contains(err.Error(), ateletDaemonSet) {
		t.Errorf("no atelet DaemonSet: want a refusal naming it, got %v", err)
	}
}

// The remedy follows how the lab selects its chart: a release pin off the
// default is sent to the default, the default itself to a release of the
// person's choosing, a checkout or the dev channel to the chart.
func TestSubstrateSkewRemedy(t *testing.T) {
	cfg := config.Default()
	cfg.Platform.ChartVersion = "4.15.2"
	if got := substrateSkewRemedy(cfg); !strings.Contains(got, "agentlab configure --defaults --chart-version "+config.DefaultChartVersion+" && agentlab platform") || !strings.Contains(got, "4.15.2") {
		t.Errorf("a pin off the default is sent to the default, got %q", got)
	}
	cfg.Platform.ChartVersion = config.DefaultChartVersion
	if got := substrateSkewRemedy(cfg); !strings.Contains(got, "--chart-version <version>") || !strings.Contains(got, config.ChartRepository) {
		t.Errorf("the default itself asks for a release of the person's choosing, got %q", got)
	}
	cfg.Platform.ChartBranch = "feat/skew"
	if got := substrateSkewRemedy(cfg); !strings.Contains(got, "feat/skew") || strings.Contains(got, "--chart-version") {
		t.Errorf("the dev channel is the branch's to align, got %q", got)
	}
	cfg.Platform.ChartPath = "/src/agent-platform/helm/agent-platform"
	if got := substrateSkewRemedy(cfg); !strings.Contains(got, cfg.Platform.ChartPath) || strings.Contains(got, "--chart-version") {
		t.Errorf("a checkout is the chart's to align, got %q", got)
	}
}
