package lab

import (
	"context"
	"reflect"
	"strings"
	"testing"

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

// testRegistryFlag is the lab registry rewrite the values render ahead of
// the image-cache policy (registryFlag on the default config).
const testRegistryFlag = "--localhost-registry-replacement=agentlab-registry:5000"

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
					map[string]any{nameKey: "sidecar", argsKey: anySlice(ateletImageCacheArgs)},
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

// The Substrate release of an image is its tag's version without the
// prerelease; an image that names no version cannot be placed and is refused
// by name.
func TestSubstrateReleaseOf(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/giantswarm/substrate/atelet:0.0.30-gs.4":                                                                         "0.0.30",
		"ghcr.io/giantswarm/substrate/ateom-gvisor:0.0.27-gs.9":                                                                   "0.0.27",
		"localhost:5000/substrate/ateom-gvisor:0.0.30-gs.1":                                                                       "0.0.30",
		"ghcr.io/giantswarm/substrate/atelet:0.0.30-gs.4@sha256:0000000000000000000000000000000000000000000000000000000000000000": "0.0.30",
		"ghcr.io/giantswarm/substrate/atelet:v0.1.0":                                                                              "0.1.0",
	} {
		if got, err := substrateReleaseOf(image); err != nil || got != want {
			t.Errorf("substrateReleaseOf(%q) = %q, %v; want %q", image, got, err, want)
		}
	}
	for _, image := range []string{
		"ghcr.io/giantswarm/substrate/atelet@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"ghcr.io/giantswarm/substrate/atelet:latest",
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
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{nameKey: "sidecar", "image": "ghcr.io/giantswarm/substrate/sidecar:9.9.9"},
				map[string]any{nameKey: ateletDaemonSet, "image": image, argsKey: anySlice(ateletImageCacheArgs)},
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
	_ = unstructured.SetNestedField(pool.Object, image, "spec", "workerImage")
	return pool
}

// The atelet and the WorkerPool's workers on one Substrate release pass, with
// both images reported; the line's -gs.N patches may differ. Two releases are
// refused naming both images, the symptom and the remedy; a lab without a
// WorkerPool cannot boot an actor either and says so.
func TestProveSubstrateLine(t *testing.T) {
	const (
		atelet = "ghcr.io/giantswarm/substrate/atelet:0.0.30-gs.4"
		worker = "ghcr.io/giantswarm/substrate/ateom-gvisor:0.0.30-gs.2"
		skewed = "ghcr.io/giantswarm/substrate/ateom-gvisor:0.0.27-gs.9"
		remedy = "agentlab configure --defaults --chart-version 9.9.9 && agentlab platform"
	)
	ctx := context.Background()

	newFakeLab(t, ateletDS(atelet), workerPool("kagent-default", worker))
	images, err := proveSubstrateLine(ctx, remedy)
	if err != nil {
		t.Fatalf("one release: %v", err)
	}
	want := []substrateImages{{atelet: atelet, pool: kagentNamespace + "/kagent-default", worker: worker, release: "0.0.30"}}
	if !reflect.DeepEqual(images, want) {
		t.Errorf("images = %+v, want %+v", images, want)
	}
	if got := images[0].String(); !strings.Contains(got, "Substrate 0.0.30") || !strings.Contains(got, atelet) || !strings.Contains(got, worker) {
		t.Errorf("the report names the release and both images, got %q", got)
	}

	newFakeLab(t, ateletDS(atelet), workerPool("kagent-default", worker), workerPool("kagent-other", skewed))
	_, err = proveSubstrateLine(ctx, remedy)
	if err == nil {
		t.Fatal("two releases: want a refusal")
	}
	for _, want := range []string{atelet, "Substrate 0.0.30", skewed, "Substrate 0.0.27", kagentNamespace + "/kagent-other", "no golden actor boots", "kubectl -n " + substrateNamespace + " logs ds/" + ateletDaemonSet, remedy} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q:\n%v", want, err)
		}
	}

	newFakeLab(t, ateletDS(atelet))
	if _, err := proveSubstrateLine(ctx, remedy); err == nil || !strings.Contains(err.Error(), "no WorkerPool in "+kagentNamespace) {
		t.Errorf("no WorkerPool: want the refusal, got %v", err)
	}

	newFakeLab(t, workerPool("kagent-default", worker))
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
	cfg.Platform.ChartBranch = "feat/x"
	if got := substrateSkewRemedy(cfg); !strings.Contains(got, "feat/x") || strings.Contains(got, "--chart-version") {
		t.Errorf("the dev channel is the branch's to align, got %q", got)
	}
	cfg.Platform.ChartPath = "/src/agent-platform/helm/agent-platform"
	if got := substrateSkewRemedy(cfg); !strings.Contains(got, cfg.Platform.ChartPath) || strings.Contains(got, "--chart-version") {
		t.Errorf("a checkout is the chart's to align, got %q", got)
	}
}
