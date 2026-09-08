package lab

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The platform's images come from charts the meta chart never renders: it
// renders one Flux OCIRepository + HelmRelease per component, and the bundled
// helm-controller pulls and renders each component chart at reconcile time.
// The preload therefore resolves the same thing ahead of the install —
// `helm template` of the meta chart, then, per rendered HelmRelease, `helm
// template` of the component chart at the version its OCIRepository's semver
// range resolves to (Helm resolves a range against the registry's tags the way
// source-controller does: the highest matching version), with the HelmRelease's
// inlined values — and scrapes the images out of those renders.

// fluxRelease is one HelmRelease of a rendered Flux manifest joined with the
// OCIRepository it pulls its chart from.
type fluxRelease struct {
	// Name is the HelmRelease's (= the chart's) name.
	Name string
	// Namespace is where the release's workloads land (targetNamespace, else
	// the object's namespace).
	Namespace string
	// URL is the OCIRepository's oci:// chart URL, Version its semver range.
	URL, Version string
	// Values is the HelmRelease's inlined spec.values, as YAML.
	Values []byte
}

// fluxDoc is the subset of an OCIRepository or HelmRelease the join reads.
type fluxDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		URL string `yaml:"url"`
		Ref struct {
			Semver string `yaml:"semver"`
			Tag    string `yaml:"tag"`
		} `yaml:"ref"`
		ReleaseName     string `yaml:"releaseName"`
		TargetNamespace string `yaml:"targetNamespace"`
		ChartRef        struct {
			Kind      string `yaml:"kind"`
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"chartRef"`
		Values yaml.Node `yaml:"values"`
	} `yaml:"spec"`
}

// fluxReleases joins the HelmReleases of a multi-document manifest with their
// OCIRepositories (chartRef by name and namespace). A HelmRelease whose
// source is not an OCIRepository in the manifest is skipped: nothing to
// resolve offline.
func fluxReleases(manifests string) ([]fluxRelease, error) {
	dec := yaml.NewDecoder(strings.NewReader(manifests))
	type source struct{ url, version string }
	sources := map[string]source{}
	var releases []fluxDoc
	for {
		var doc fluxDoc
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing the rendered Flux manifests: %w", err)
		}
		switch doc.Kind {
		case "OCIRepository":
			version := doc.Spec.Ref.Semver
			if version == "" {
				version = doc.Spec.Ref.Tag
			}
			sources[doc.Metadata.Namespace+"/"+doc.Metadata.Name] = source{doc.Spec.URL, version}
		case "HelmRelease":
			releases = append(releases, doc)
		}
	}
	var out []fluxRelease
	for _, hr := range releases {
		if hr.Spec.ChartRef.Kind != "OCIRepository" {
			continue
		}
		ns := hr.Spec.ChartRef.Namespace
		if ns == "" {
			ns = hr.Metadata.Namespace
		}
		src, ok := sources[ns+"/"+hr.Spec.ChartRef.Name]
		if !ok {
			continue
		}
		rel := fluxRelease{Name: hr.Spec.ReleaseName, Namespace: hr.Spec.TargetNamespace, URL: src.url, Version: src.version}
		if rel.Name == "" {
			rel.Name = hr.Metadata.Name
		}
		if rel.Namespace == "" {
			rel.Namespace = hr.Metadata.Namespace
		}
		if !hr.Spec.Values.IsZero() {
			raw, err := yaml.Marshal(&hr.Spec.Values)
			if err != nil {
				return nil, fmt.Errorf("re-encoding the values of HelmRelease %s: %w", rel.Name, err)
			}
			rel.Values = raw
		}
		out = append(out, rel)
	}
	return out, nil
}

// offlineAPIVersions are the API groups the component charts consult with
// .Capabilities when rendered without a cluster: the guards that fail without
// Flux (agent-manager's) and the Gateway API the routes render into.
var offlineAPIVersions = []string{
	"helm.toolkit.fluxcd.io/v2",
	"source.toolkit.fluxcd.io/v1",
	"gateway.networking.k8s.io/v1",
}

// fluxReleaseImages templates every release's chart at the resolved version
// with its values and scrapes the images, concurrently (one registry pull
// each). A release that does not render is reported and skipped: the node
// pulls whatever the preload misses.
func fluxReleaseImages(releases []fluxRelease, apiVersions []string) ([]string, []error) {
	dir, err := os.MkdirTemp("", "agentlab-preload-")
	if err != nil {
		return nil, []error{err}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var mu sync.Mutex
	var wg sync.WaitGroup
	var images []string
	var errs []error
	for i, rel := range releases {
		valuesPath := filepath.Join(dir, fmt.Sprintf("%d-%s.yaml", i, rel.Name))
		if err := os.WriteFile(valuesPath, rel.Values, 0o600); err != nil {
			errs = append(errs, err)
			continue
		}
		wg.Go(func() {
			args := []string{"template", rel.Name, rel.URL, "--version", rel.Version, "-n", rel.Namespace, "-f", valuesPath}
			for _, v := range apiVersions {
				args = append(args, "--api-versions", v)
			}
			rendered, err := outputQuiet("helm", args...)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (%s %s): %w", rel.Name, rel.URL, rel.Version, err))
				return
			}
			images = append(images, scrapeImages(rendered)...)
		})
	}
	wg.Wait()
	slices.Sort(images)
	return slices.Compact(images), errs
}

// platformImages derives the platform's image refs exactly as it is about to
// be installed: the meta chart's own objects (the Flux Operator, the hook
// Jobs) from its render with the rendered lab values, then every component
// chart at the version its OCIRepository resolves to (fluxReleaseImages),
// then the lab's own mcp-prometheus HelmRelease the same way. The images the
// engine composes at run time — the FluxInstance's source- and
// helm-controller, the ADK runtime tags kagent builds from its ConfigMap —
// are not in any render; healADKImages and the snapshot manifest cover them.
// Best-effort throughout: failures are notes, the node pulls the rest.
func platformImages(cfg *config.Config, chart platformChart, valuesPath string) []string {
	args := append([]string{"template", platformRelease}, chart.args()...)
	args = append(args, "-n", platformNamespace, "-f", valuesPath)
	meta, err := outputQuiet("helm", args...)
	if err != nil {
		note("cannot render %s (%v); the node pulls the platform images itself", chart, excerpt(err.Error(), 300))
		return nil
	}
	images := scrapeImages(meta)
	releases, err := fluxReleases(meta)
	if err != nil {
		note("cannot read the component releases out of the render (%v); the node pulls their images itself", err)
		return images
	}
	if cfg.Platform.Observability {
		if rendered, _, err := renderManifest(cfg, mcpPrometheusTemplate); err == nil {
			if own, err := fluxReleases(string(rendered)); err == nil {
				releases = append(releases, own...)
			}
		}
	}
	apiVersions := offlineAPIVersions
	if cfg.Platform.Observability {
		apiVersions = append(slices.Clone(apiVersions), "monitoring.coreos.com/v1")
	}
	componentImages, errs := fluxReleaseImages(releases, apiVersions)
	for _, err := range errs {
		note("component render skipped: %s", excerpt(err.Error(), 300))
	}
	images = append(images, componentImages...)
	slices.Sort(images)
	return slices.Compact(images)
}

// sideloadPlatformImages pulls the platform images on the host and side-loads
// them into the node, reporting the outcome in one line.
func sideloadPlatformImages(cfg *config.Config, images []string) {
	if len(images) == 0 {
		return
	}
	switch res := sideloadImages(cfg, hostPullImages(images)); {
	case res.err != nil && res.n > 0:
		note("side-loaded %d of %d platform images (%s); the rest failed (%v) and the node pulls them", res.n, len(images), res.d, res.err)
	case res.err != nil:
		note("side-loading failed (%v); the node pulls anything missing", res.err)
	case res.n > 0:
		note("side-loaded %d of %d platform images (%s)", res.n, len(images), res.d)
	default:
		note("all %d platform images are already on the node", len(images))
	}
}
