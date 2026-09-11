package lab

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/agentlab/internal/config"
)

// The platform's images come from charts the meta chart never renders: it
// renders one Flux OCIRepository + HelmRelease per component, and the bundled
// helm-controller pulls and renders each component chart at reconcile time.
// The preload therefore resolves the same thing ahead of the install — a
// `helm template`-style offline render of the meta chart (helmTemplate),
// then, per rendered HelmRelease, the same render of the component chart at
// the version its OCIRepository resolves to (resolveComponentVersion: the
// highest tag within the semver range, narrowed by the semverFilter where
// the OCIRepository carries one — source-controller's pick), with the
// HelmRelease's inlined values — and scrapes the images out of those renders.

// fluxRelease is one HelmRelease of a rendered Flux manifest joined with the
// OCIRepository it pulls its chart from.
type fluxRelease struct {
	// Name is the HelmRelease's (= the chart's) name.
	Name string
	// Namespace is where the release's workloads land (targetNamespace, else
	// the object's namespace).
	Namespace string
	// URL is the OCIRepository's oci:// chart URL, Version its semver range
	// and Filter its semverFilter: the regexp that narrows the range's tags
	// to a channel (a branch's dev builds); empty on the stable channel.
	URL, Version, Filter string
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
			Semver       string `yaml:"semver"`
			SemverFilter string `yaml:"semverFilter"`
			Tag          string `yaml:"tag"`
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
	type source struct{ url, version, filter string }
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
		case kindOCIRepository:
			version := doc.Spec.Ref.Semver
			if version == "" {
				version = doc.Spec.Ref.Tag
			}
			sources[doc.Metadata.Namespace+"/"+doc.Metadata.Name] = source{doc.Spec.URL, version, doc.Spec.Ref.SemverFilter}
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
		rel := fluxRelease{Name: hr.Spec.ReleaseName, Namespace: hr.Spec.TargetNamespace, URL: src.url, Version: src.version, Filter: src.filter}
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
// pulls whatever the preload misses. The versions picked through a
// semverFilter come back as "<name> <version>" lines, sorted — the channel
// evidence the boot log shows.
func fluxReleaseImages(releases []fluxRelease, apiVersions []string) (images, filtered []string, errs []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, rel := range releases {
		wg.Go(func() {
			rendered, version, err := renderFluxRelease(rel, apiVersions)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (%s %s): %w", rel.Name, rel.URL, rel.Version, err))
				return
			}
			if rel.Filter != "" {
				filtered = append(filtered, rel.Name+" "+version)
			}
			images = append(images, scrapeImages(rendered)...)
		})
	}
	wg.Wait()
	slices.Sort(images)
	slices.Sort(filtered)
	return slices.Compact(images), filtered, errs
}

// renderFluxRelease renders one component chart offline as helm-controller
// is about to: the chart the OCIRepository names at the version it resolves
// to, with the HelmRelease's inlined values. Reports the version rendered.
func renderFluxRelease(rel fluxRelease, apiVersions []string) (rendered, version string, err error) {
	vals, err := helmValues(rel.Values)
	if err != nil {
		return "", "", err
	}
	version, err = resolveComponentVersion(rel.URL, rel.Version, rel.Filter)
	if err != nil {
		return "", "", err
	}
	rendered, err = helmTemplate(rel.Namespace, rel.Name, rel.URL, version, vals, apiVersions)
	return rendered, version, err
}

// listChartTags lists a chart repository's tags for the component
// resolution (helmChartTags); a variable so tests answer for the registry.
var listChartTags = helmChartTags

// resolveComponentVersion is the version a component chart is rendered at,
// resolved the way source-controller resolves the OCIRepository. Without a
// semverFilter the range goes to Helm as it is: LocateChart picks the
// highest tag within it, source-controller's pick too. With one, Helm cannot
// help — it knows no filter and would pick the range's highest RELEASE over
// the channel's dev build, whose schema the dev values then fail — so the
// pick is made here: the repository's tags, those the filter's regexp
// matches, those the range admits, the highest. No such tag is an error the
// caller reports and skips: the node pulls that component's images itself.
func resolveComponentVersion(url, versionRange, filter string) (string, error) {
	if filter == "" {
		return versionRange, nil
	}
	tags, err := listChartTags(url)
	if err != nil {
		return "", err
	}
	return pickFilteredTag(tags, versionRange, filter)
}

// pickFilteredTag is source-controller's getTagBySemver: the highest version
// among the tags the filter's regexp matches (over the tag as spelled, like
// Flux) and the range's constraint admits — a range without a `-0` floor
// admits no prerelease, as in Flux.
func pickFilteredTag(tags []string, versionRange, filter string) (string, error) {
	re, err := regexp.Compile(filter)
	if err != nil {
		return "", fmt.Errorf("semverFilter %q: %w", filter, err)
	}
	constraint, err := semver.NewConstraint(versionRange)
	if err != nil {
		return "", fmt.Errorf("version range %q: %w", versionRange, err)
	}
	tag, ok := highestVersion(tags, func(v *semver.Version) bool {
		return re.MatchString(v.Original()) && constraint.Check(v)
	})
	if !ok {
		return "", fmt.Errorf("no tag matches semverFilter %s within %s (among %d)", filter, versionRange, len(tags))
	}
	return tag, nil
}

// platformRoster is the meta chart as it is about to be installed — its
// offline render with the lab's values and the component releases read out
// of it. One render per command: the roster says what the chart ships (the
// components the lab must budget for and must not install itself), the
// preload derives the images from it.
type platformRoster struct {
	chart    platformChart
	manifest string
	releases []fluxRelease
}

// renderPlatformRoster renders the meta chart offline with the values the
// install is about to use and joins its component releases. An error is the
// chart's (not found, a values guard) — the callers degrade to notes: the
// install itself reports the same error, with Helm's wording.
func renderPlatformRoster(chart platformChart, values map[string]any) (*platformRoster, error) {
	meta, err := helmTemplate(platformNamespace, platformRelease, chart.ref, chart.version, values, nil)
	if err != nil {
		return nil, err
	}
	releases, err := fluxReleases(meta)
	if err != nil {
		return nil, err
	}
	return &platformRoster{chart: chart, manifest: meta, releases: releases}, nil
}

// has reports whether the roster carries a component release of that name.
func (r *platformRoster) has(release string) bool {
	if r == nil {
		return false
	}
	return slices.ContainsFunc(r.releases, func(rel fluxRelease) bool { return rel.Name == release })
}

// shipsSubstrate reports whether the chart delivers Agent Substrate itself —
// the `substrate` component release in its roster (the 4.x line: it follows
// components.kagent) — read off the render, never off a version string. A
// chart that ships it is Substrate's one Helm owner: the lab installs none,
// budgets for the control plane and the WorkerPool's workers, and checks the
// apiserver gates Substrate needs before the install.
func (r *platformRoster) shipsSubstrate() bool { return r.has(substrateRelease) }

// shipsCNPG reports whether the chart delivers the CloudNativePG operator
// (components.cloudnative-pg) — with it the connectivity chart's Postgres
// Cluster and databases run here (postgres.enabled), the lab budgets for
// them and the preload fetches the operator's and the Cluster's images.
func (r *platformRoster) shipsCNPG() bool { return r.has(cnpgRelease) }

// cnpgRelease is the CloudNativePG operator's component release name.
const cnpgRelease = "cloudnative-pg"

// platformImages derives the platform's image refs exactly as it is about to
// be installed: the meta chart's own objects (the Flux Operator, the hook
// Jobs) from the roster's render, then every component chart at the version
// its OCIRepository resolves to (fluxReleaseImages), then the lab's own
// mcp-prometheus HelmRelease the same way. The images the engine composes at
// run time — the FluxInstance's source- and helm-controller — are in no
// render; the snapshot manifest covers them from the second boot on.
// Best-effort throughout: failures are notes, the node pulls the rest.
func platformImages(cfg *config.Config, roster *platformRoster) []string {
	if roster == nil {
		return nil
	}
	images := scrapeImages(roster.manifest)
	releases := roster.releases
	if cfg.Platform.Observability {
		if rendered, _, err := renderManifest(cfg, mcpPrometheusTemplate); err == nil {
			if own, err := fluxReleases(string(rendered)); err == nil {
				releases = append(slices.Clone(releases), own...)
			}
		}
	}
	apiVersions := offlineAPIVersions
	if cfg.Platform.Observability {
		apiVersions = append(slices.Clone(apiVersions), "monitoring.coreos.com/v1")
	}
	componentImages, filtered, errs := fluxReleaseImages(releases, apiVersions)
	for _, err := range errs {
		note("component render skipped: %s", excerpt(err.Error(), 300))
	}
	note("rendered %d of %d component charts%s", len(releases)-len(errs), len(releases), filteredNote(filtered))
	images = append(images, componentImages...)
	slices.Sort(images)
	images, byDigest := splitDigestRefs(slices.Compact(images))
	if len(byDigest) > 0 {
		note("%d digest-pinned refs are the node's to pull (a saved archive of a digest-only reference imports as an unnamed image the CRI cannot start a pod from):\n      %s", len(byDigest), strings.Join(byDigest, "\n      "))
	}
	return images
}

// splitDigestRefs separates the refs a side-load can carry — tagged
// references, which `docker save` writes with their name — from the
// digest-pinned ones (<repository>@sha256:…, no tag). A `docker save` of a
// digest-only reference writes an archive without a name; `ctr images import`
// records it as `import-<date>@sha256:…` and the kubelet, asked for the pod's
// <repository>@sha256 reference, fails the container with "failed to check if
// this is a checkpoint image … not found" (seen on Substrate's RustFS after a
// reinstall). Those refs are left to the kubelet: a digest pull is
// deterministic and small (RustFS, the bucket-init CLI); the Go ADK Harness
// image is pulled by atelet into its own cache, never by the kubelet.
func splitDigestRefs(images []string) (tagged, byDigest []string) {
	for _, img := range images {
		if strings.Contains(img, "@sha256:") {
			byDigest = append(byDigest, img)
			continue
		}
		tagged = append(tagged, img)
	}
	return tagged, byDigest
}

// filteredNote words the versions the component renders picked through a
// semverFilter — the dev channel's evidence of which builds the components
// are on — as the tail of the render count; empty on the stable channel.
func filteredNote(filtered []string) string {
	if len(filtered) == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d through a semverFilter: %s)", len(filtered), strings.Join(filtered, ", "))
}

// sideloadPlatformImages pulls the platform images on the host and side-loads
// them into the node, naming what landed: the boot log is the record that
// nothing of the topology is left to the kubelet under the install's wait.
func sideloadPlatformImages(cfg *config.Config, images []string) {
	if len(images) == 0 {
		return
	}
	res := sideloadImages(cfg, hostPullImages(images))
	loaded := ""
	if res.n > 0 {
		loaded = ":\n      " + strings.Join(res.refs, "\n      ")
	}
	switch {
	case res.err != nil && res.n > 0:
		note("side-loaded %d of %d platform images (%s); the rest failed (%v) and the node pulls them%s", res.n, len(images), res.d, res.err, loaded)
	case res.err != nil:
		note("side-loading failed (%v); the node pulls anything missing", res.err)
	case res.n > 0:
		note("side-loaded %d of %d platform images (%s)%s", res.n, len(images), res.d, loaded)
	default:
		note("all %d platform images are already on the node", len(images))
	}
}
