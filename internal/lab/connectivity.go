package lab

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartv2util "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/registry"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/agentlab/internal/config"
)

// The connectivity chart of a checkout (platform.chartPath).
//
// The meta chart installs its wiring chart, agent-platform-connectivity, at
// its own exact version (components.agent-platform-connectivity.releasedWithChart):
// the two charts are published off one git tag, so an installation's
// connectivity never lags or leads the meta chart it wires. A checkout's
// meta chart carries the placeholder version of its Chart.yaml, which no
// registry publishes — the connectivity OCIRepository asks the release
// registry for it, resolves nothing (`no match found for semver`), and
// connectivity, Substrate, kagent and agent-manager never turn Ready while
// `up` waits out its deadline.
//
// So a chartPath lab installs the checkout's connectivity chart too, the way
// the chart's own acceptance tests do: packaged from the sibling directory at
// the meta chart's version — one version for the two charts of a working
// tree, as CI stamps them for a commit — pushed into the lab registry (the
// container on the kind network the `harness` dev image and the vm-manager
// guest image go through, devimages.go), and the meta chart pointed at that
// registry through the entry's `repository` and `insecure` knobs — the ones
// it admits for a chart pushed by hand. Both charts of one working tree, no
// commit, no push, no CI run: an edit to either is one `agentlab platform`
// away, and the lab's offline renders read the connectivity chart from the
// directory (fluxRelease.LocalChart), since the registry's kind-network name
// is out of the host's reach.

// localConnectivityChart is the connectivity component's source for a
// chartPath lab: what the values template renders into
// components.agent-platform-connectivity and what the push publishes.
type localConnectivityChart struct {
	// Dir is the checkout's chart directory (config.ConnectivityChartDir).
	Dir string
	// Repository is the chart repository as the OCIRepository names it: the
	// lab registry on the kind network, plain HTTP (spec.insecure).
	Repository string
	// Version is the version the chart is pushed as, the meta chart
	// directory's own; the OCIRepository's exact semver.
	Version string
}

// connectivityChartRepoPath is the repository path charts take in the lab
// registry, next to the images.
const connectivityChartRepoPath = "charts"

// connectivityCatchUpTimeout bounds the wait for the connectivity release to
// run the chart just pushed: source-controller's fetch after the reconcile
// request, then helm-controller's upgrade with its rollout waits.
const connectivityCatchUpTimeout = 5 * time.Minute

// localConnectivityChartFor is the connectivity source of a chartPath lab —
// nil for a registry chart, whose connectivity is published with it. The
// version is the meta chart directory's; a Chart.yaml without one cannot
// name the release and is an error.
func localConnectivityChartFor(cfg *config.Config) (*localConnectivityChart, error) {
	if cfg.Platform.ChartPath == "" {
		return nil, nil
	}
	version := chartDirVersion(cfg.Platform.ChartPath)
	if version == "" {
		return nil, fmt.Errorf("platform.chartPath: %s/Chart.yaml carries no version to install the connectivity chart at", cfg.Platform.ChartPath)
	}
	return &localConnectivityChart{
		Dir:        config.ConnectivityChartDir(cfg.Platform.ChartPath),
		Repository: registry.OCIScheme + "://" + devRegistryEndpoint(cfg) + "/" + connectivityChartRepoPath,
		Version:    version,
	}, nil
}

// pushRef is the chart as the host pushes it: the registry's loopback
// spelling, the same repository path pods pull from, tagged with the
// version.
func (c *localConnectivityChart) pushRef(cfg *config.Config) string {
	return devRegistryHost(cfg) + "/" + connectivityChartRepoPath + "/" + config.ConnectivityChartName + ":" + c.Version
}

// pushConnectivityChart publishes the checkout's connectivity chart into the
// lab registry at the meta chart's version and returns the manifest digest
// the registry computed for it — what the connectivity release records as
// its chart's OCI digest once it runs it (waitConnectivityCatchUp).
// Idempotent: an unchanged directory is the same archive and the same
// digest, and the release stays.
func pushConnectivityChart(cfg *config.Config, c *localConnectivityChart) (string, error) {
	if err := ensureDevRegistry(cfg); err != nil {
		return "", err
	}
	data, err := packageChart(c.Dir, c.Version)
	if err != nil {
		return "", fmt.Errorf("packaging the connectivity chart of %s: %w", c.Dir, err)
	}
	log := newHelmLog()
	rc, err := registry.NewClient(
		registry.ClientOptDebug(helmSettings().Debug),
		registry.ClientOptWriter(log),
		registry.ClientOptPlainHTTP(),
	)
	if err != nil {
		return "", fmt.Errorf("creating the registry client for the lab registry: %w", err)
	}
	ref := c.pushRef(cfg)
	res, err := rc.Push(data, ref)
	if err != nil {
		log.dump()
		return "", fmt.Errorf("pushing the connectivity chart to the lab registry as %s: %w", ref, err)
	}
	digest := res.Manifest.Digest
	note("the connectivity chart of %s is in the lab registry as %s (%s); the meta chart pulls it from %s", c.Dir, ref, digest, c.Repository)
	return digest, nil
}

// packageChart is `helm package <dir> --version <version>` into memory: the
// chart directory loaded (its .helmignore honoured), its version set — the
// meta chart's, so the two charts of a working tree carry one version — and
// saved as the archive a registry takes.
func packageChart(dir, version string) ([]byte, error) {
	ch, err := loader.Load(dir)
	if err != nil {
		return nil, err
	}
	ch.Metadata.Version = version
	tmp, err := os.MkdirTemp("", "agentlab-chart-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	path, err := chartv2util.Save(ch, tmp)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path) // #nosec G304 -- the archive Save just wrote into this function's own temp dir
}

// waitConnectivityCatchUp has the connectivity release run the chart just
// pushed. The meta chart's OCIRepository fetches its tag on gitops.interval
// (ten minutes), and a re-push under the same version changes the content
// behind the tag, not the object the install wrote — so it is asked to
// fetch now (the reconcile request annotation), and the release is waited
// for until its current revision names the pushed manifest digest
// (status.history[0].ociDigest, what helm-controller stamps into the chart
// version as <version>+<digest>) and it is Ready. A lab whose release
// already runs that digest — the first install, an unchanged directory —
// passes at the first read.
func waitConnectivityCatchUp(ctx context.Context, digest string) error {
	ociGVR, err := gvrFor(fluxOCIRepositoryResource)
	if err != nil {
		return err
	}
	hrGVR, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return err
	}
	name := config.ConnectivityChartName
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, fluxReconcileAnnotation, time.Now().UTC().Format(time.RFC3339Nano))
	if err := patchObject(ctx, ociGVR, platformNamespace, name, types.MergePatchType, []byte(patch)); err != nil {
		return err
	}
	var last string
	caught := waitFor(int(connectivityCatchUpTimeout/pollInterval), pollInterval, func() bool {
		hr, err := getObject(ctx, hrGVR, platformNamespace, name)
		if err != nil {
			last = err.Error()
			return false
		}
		current := currentReleaseOCIDigest(hr)
		ready := conditionStatus(hr, conditionReady)
		last = fmt.Sprintf("runs %s, Ready=%s %s", orNone(current), orNone(ready), conditionMessage(hr, conditionReady))
		return current == digest && ready == conditionTrue
	})
	if !caught {
		return fmt.Errorf("HelmRelease %s does not run the connectivity chart pushed as %s after %s (%s)\ncheck `kubectl -n %s describe ocirepository %s` and `kubectl -n %s describe helmrelease %s`",
			name, digest, connectivityCatchUpTimeout, last, platformNamespace, name, platformNamespace, name)
	}
	note("HelmRelease %s runs the connectivity chart of the checkout (%s)", name, digest)
	return nil
}

// currentReleaseOCIDigest is the OCI manifest digest of the chart a
// HelmRelease's current revision was installed from (status.history[0].ociDigest),
// "" while it has none.
func currentReleaseOCIDigest(hr *unstructured.Unstructured) string {
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return ""
	}
	current, ok := history[0].(map[string]any)
	if !ok {
		return ""
	}
	digest, _, _ := unstructured.NestedString(current, "ociDigest")
	return strings.TrimSpace(digest)
}
