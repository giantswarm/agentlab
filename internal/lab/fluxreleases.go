package lab

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

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
	// ValuesFrom reports that the HelmRelease also draws values from a
	// ConfigMap or Secret on the cluster (spec.valuesFrom), which an offline
	// render cannot see: a key the render found missing may be there.
	ValuesFrom bool
	// SchemaValidationOff reports that the HelmRelease tells helm-controller
	// not to validate the values against the chart's schema
	// (spec.install.disableSchemaValidation, spec.upgrade's): a verdict the
	// offline render gets is one the cluster never asks for.
	SchemaValidationOff bool
	// LabOwned marks a release the lab renders itself rather than one the
	// meta chart ships (mcp-prometheus): its chart version is a Go const and
	// its values the lab's template, so platform.chartVersion has no say over
	// what it accepts — a refusal is worded for agentlab, not for the chart.
	LabOwned bool
	// LocalChart is the directory the release's chart is pushed into the lab
	// registry from (the checkout's connectivity chart of a chartPath lab,
	// connectivity.go): the offline render reads the chart there, since the
	// URL names the registry by its kind-network name, out of the host's
	// reach. Empty for a chart the registry serves to the host too.
	LocalChart string
}

// chartLabel names the chart a render of the release was for: the
// OCIRepository's URL and range, and the version the range resolved to where
// the render got that far.
func (rel fluxRelease) chartLabel(resolved string) string {
	if rel.LocalChart != "" {
		return "the local chart at " + rel.LocalChart + ", pushed as " + rel.URL + ":" + rel.Version
	}
	label := rel.URL + " " + rel.Version
	if resolved != "" && resolved != rel.Version {
		label += ", resolved to " + resolved
	}
	return label
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
		ReleaseName     string      `yaml:"releaseName"`
		ValuesFrom      []yaml.Node `yaml:"valuesFrom"`
		TargetNamespace string      `yaml:"targetNamespace"`
		Install         struct {
			DisableSchemaValidation bool `yaml:"disableSchemaValidation"`
		} `yaml:"install"`
		Upgrade struct {
			DisableSchemaValidation bool `yaml:"disableSchemaValidation"`
		} `yaml:"upgrade"`
		ChartRef struct {
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
		rel.ValuesFrom = len(hr.Spec.ValuesFrom) > 0
		rel.SchemaValidationOff = hr.Spec.Install.DisableSchemaValidation || hr.Spec.Upgrade.DisableSchemaValidation
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

// componentAPIVersions are the API groups the component renders are given as
// .Capabilities (offlineAPIVersions), plus the Prometheus Operator's when the
// observability stack is on.
func componentAPIVersions(cfg *config.Config) []string {
	if cfg.Platform.Observability {
		return append(slices.Clone(offlineAPIVersions), "monitoring.coreos.com/v1")
	}
	return offlineAPIVersions
}

// renderFailure is one component render that produced no manifest, kept with
// the release it was for: whether a failure predicts the install depends on
// facts about the release, not only on what the chart said.
type renderFailure struct {
	rel fluxRelease
	err error
}

// refusesTheInstall reports whether this failure is a chart refusing values
// helm-controller will put to it the same way — the one class that predicts
// the install's outcome (an unreachable registry stops the boot for another
// reason, judgeRenderFailures) — and, where the chart did refuse but the
// install is not predicted, why.
//
// The chart must have returned a validation verdict (schemaRejection); a
// render the registry denied says nothing about the install.
// The HelmRelease must not switch helm-controller's validation off
// (SchemaValidationOff): that verdict is never asked for on the cluster. And
// a HelmRelease with a valuesFrom was rendered on an incomplete set: Flux
// merges the ConfigMap or Secret first and spec.values on top, so a
// reference can add keys the render found missing — a `missing property`
// verdict may well be answered there — but it cannot take a key away, so
// `additional properties … not allowed` on keys the render did see is
// refused on the cluster too. Whose release it is (LabOwned) changes the
// advice, never the verdict: the lab's own mcp-prometheus is installed and
// waited for like any component.
func (f renderFailure) refusesTheInstall() (refuses bool, reason string) {
	var rejection *schemaRejection
	if !errors.As(f.err, &rejection) {
		return false, ""
	}
	if f.rel.SchemaValidationOff {
		return false, "its HelmRelease turns helm-controller's schema validation off"
	}
	if f.rel.ValuesFrom && !onlyAdditionalProperties(rejection) {
		return false, "its HelmRelease also draws values from the cluster, which may hold what the render found missing"
	}
	return true, ""
}

// onlyAdditionalProperties reports whether every violation in a verdict is an
// `additional properties … not allowed` — the closed-schema class a
// valuesFrom cannot rescue, since the keys it names are in spec.values and
// stay there whatever the reference adds. Any other violation (a missing
// property, a type) may be about a key the reference supplies, so one is
// enough to keep the verdict a note.
func onlyAdditionalProperties(rejection *schemaRejection) bool {
	violations := 0
	for line := range strings.Lines(rejection.Error()) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		violations++
		if !strings.Contains(line, "additional propert") {
			return false
		}
	}
	return violations > 0
}

// fluxReleaseImages templates every release's chart at the resolved version
// with its values and scrapes the images, concurrently (one registry pull
// each; a render the registry did not answer is tried again,
// retryUnreachable). A release that does not render comes back as a failure
// with its release, for the caller to sort into notes and stops
// (judgeRenderFailures). The versions picked through a semverFilter
// come back as "<name> <version>" lines, sorted — the channel evidence the
// boot log shows. The renders themselves come back keyed by release name:
// what the dev-image swap reads a component's image name off
// (resolveDevImageNames).
func fluxReleaseImages(releases []fluxRelease, apiVersions []string) (images, filtered []string, renders map[string]string, errs []renderFailure) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	renders = map[string]string{}
	for _, rel := range releases {
		wg.Go(func() {
			var rendered, version string
			err := retryUnreachable("rendering "+rel.Name, func() (err error) {
				rendered, version, err = renderFluxRelease(rel, apiVersions)
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, renderFailure{rel: rel, err: fmt.Errorf("%s (%s): %w", rel.Name, rel.chartLabel(version), err)})
				return
			}
			if rel.Filter != "" {
				filtered = append(filtered, rel.Name+" "+version)
			}
			renders[rel.Name] = rendered
			images = append(images, scrapeImages(rendered)...)
		})
	}
	wg.Wait()
	slices.Sort(images)
	slices.Sort(filtered)
	return slices.Compact(images), filtered, renders, errs
}

// renderFluxRelease renders one component chart offline as helm-controller
// is about to: the chart the OCIRepository names at the version it resolves
// to — or the directory the lab pushes that chart from (LocalChart) — with
// the HelmRelease's inlined values. Reports the version rendered — the
// loaded chart's, so a range Helm resolved itself comes back as the tag, on
// a failed render too once the chart was loaded.
func renderFluxRelease(rel fluxRelease, apiVersions []string) (rendered, version string, err error) {
	vals, err := helmValues(rel.Values)
	if err != nil {
		return "", "", err
	}
	ref := rel.LocalChart
	if ref == "" {
		ref = rel.URL
		if version, err = resolveComponentVersion(rel.URL, rel.Version, rel.Filter); err != nil {
			return "", "", err
		}
	}
	rendered, resolved, err := helmTemplate(rel.Namespace, rel.Name, ref, version, vals, apiVersions)
	if resolved != "" {
		version = resolved
	}
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
// install is about to use and joins its component releases; a render the
// registry did not answer is tried again (retryUnreachable). An error is for
// the caller to judge: a refusal (isSchemaRejection) and a registry still
// unreachable (registryUnreachable) stop the boot, anything else of the
// chart's (not found, a values guard) is a note — the install itself
// reports the same error, with Helm's wording.
func renderPlatformRoster(chart platformChart, values map[string]any) (*platformRoster, error) {
	var meta string
	if err := retryUnreachable(fmt.Sprintf("rendering %s", chart), func() (err error) {
		meta, _, err = helmTemplate(platformNamespace, platformRelease, chart.ref, chart.version, values, nil)
		return err
	}); err != nil {
		return nil, err
	}
	releases, err := fluxReleases(meta)
	if err != nil {
		return nil, err
	}
	localizeConnectivity(releases, chart)
	return &platformRoster{chart: chart, manifest: meta, releases: releases}, nil
}

// localizeConnectivity marks the connectivity release of a chart directory
// (platform.chartPath) as rendered from the checkout's own connectivity
// chart, the one the lab pushes into the lab registry (connectivity.go):
// the release's OCIRepository names the registry by its kind-network name,
// which resolves in pods and nowhere on the host. A registry chart's
// releases are left alone — its connectivity is published with it.
func localizeConnectivity(releases []fluxRelease, chart platformChart) {
	dir := chart.connectivityDir()
	if dir == "" {
		return
	}
	for i := range releases {
		if releases[i].Name == config.ConnectivityChartName {
			releases[i].LocalChart = dir
		}
	}
}

// has reports whether the roster carries a component release of that name.
func (r *platformRoster) has(release string) bool {
	if r == nil {
		return false
	}
	return slices.ContainsFunc(r.releases, func(rel fluxRelease) bool { return rel.Name == release })
}

// unrendered names the roster's releases that have no render among renders
// (keyed by release name, platformImages) — the skipped ones — sorted; none
// without a roster.
func (r *platformRoster) unrendered(renders map[string]string) []string {
	if r == nil {
		return nil
	}
	var names []string
	for _, rel := range r.releases {
		if _, ok := renders[rel.Name]; !ok {
			names = append(names, rel.Name)
		}
	}
	slices.Sort(names)
	return names
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
// Best-effort for the images — a render that is skipped is a note, and the
// node pulls those images itself — with two exceptions it returns an error
// for (judgeRenderFailures): a chart that REFUSES the values its HelmRelease
// carries (refusesTheInstall), which is not a preload problem but the
// install's outcome, known early; and a render the registry did not answer
// through its retries (registryUnreachable), since the renders are more
// than the images: the lab's patches are read off them. Stopping here costs
// seconds instead of the install's whole wait, and nothing is side-loaded
// for an install that will not start. The component renders come back too,
// keyed by release name (nil without a roster): the dex-localhost sidecar's
// targets and the dev-image swap's image names are read off them
// (dexLocalhostTargets, resolveDevImageNames).
func platformImages(cfg *config.Config, roster *platformRoster) ([]string, map[string]string, error) {
	if roster == nil {
		return nil, nil, nil
	}
	images := scrapeImages(roster.manifest)
	releases := roster.releases
	if cfg.Platform.Observability {
		if rendered, _, err := renderManifest(cfg, mcpPrometheusTemplate); err == nil {
			if own, err := fluxReleases(string(rendered)); err == nil {
				// The lab's own HelmRelease, not the meta chart's (see
				// fluxRelease.LabOwned): a refusal must say so, since no
				// platform.chartVersion can answer it.
				for i := range own {
					own[i].LabOwned = true
				}
				releases = append(slices.Clone(releases), own...)
			}
		}
	}
	componentImages, filtered, renders, errs := fluxReleaseImages(releases, componentAPIVersions(cfg))
	if err := judgeRenderFailures(roster.chart, errs); err != nil {
		return nil, nil, err
	}
	note("rendered %d of %d component charts%s", len(releases)-len(errs), len(releases), filteredNote(filtered))
	images = append(images, componentImages...)
	// The lab preset's runtime (serving.go): named by no chart render a pod
	// of it is in — the preset's image is a value the connectivity chart
	// publishes as data, and the well-known configs' images are templates
	// (scrapeImages leaves them out).
	images = append(images, servingImages(cfg)...)
	slices.Sort(images)
	images, byDigest := splitDigestRefs(slices.Compact(images))
	if len(byDigest) > 0 {
		note("%d digest-pinned refs are the node's to pull (a saved archive of a digest-only reference imports as an unnamed image the CRI cannot start a pod from):\n      %s", len(byDigest), strings.Join(byDigest, "\n      "))
	}
	return images, renders, nil
}

// judgeRenderFailures sorts the component render failures into the three
// outcomes of a render that produced no manifest. Two stop the boot before
// the install, each as one error naming every release it holds for:
//
//   - unreachable: the registry did not answer through every retry
//     (registryUnreachable). The lab's patches for a component — the
//     dex-localhost sidecar, platform.devImages — are read off its render,
//     so the install would run it unpatched, and the symptom (a server
//     crash-looping on a Dex it cannot reach, the install failing minutes
//     later on its HelmRelease) points nowhere near the network.
//   - refused: the chart refuses the values its HelmRelease carries
//     (refusesTheInstall), so the install would fail after its wait.
//
// Everything else is a skip, noted with why where the chart did refuse: a
// render the offline render cannot do the way the cluster does (a
// kubeVersion guard, a verdict helm-controller never asks for or the
// cluster's values may answer), or a registry that answered no (a tag that
// is not there), which the install reports in helm-controller's words.
func judgeRenderFailures(chart platformChart, errs []renderFailure) error {
	var unreachable, rejected []renderFailure
	for _, failure := range errs {
		if registryUnreachable(failure.err) {
			unreachable = append(unreachable, failure)
			continue
		}
		refuses, reason := failure.refusesTheInstall()
		switch {
		case refuses:
			rejected = append(rejected, failure)
		case reason != "":
			note("component render skipped (%s): %s", reason, excerptEnds(failure.err.Error(), 300))
		default:
			note("component render skipped: %s", excerptEnds(failure.err.Error(), 300))
		}
	}
	byName := func(a, b renderFailure) int { return strings.Compare(a.rel.Name, b.rel.Name) }
	var stops []error
	if len(unreachable) > 0 {
		slices.SortFunc(unreachable, byName)
		stops = append(stops, unreachableComponentsError(unreachable))
	}
	if len(rejected) > 0 {
		slices.SortFunc(rejected, byName)
		stops = append(stops, rejectedComponentsError(chart, rejected))
	}
	return errors.Join(stops...)
}

// recheckReleases puts values that changed after the preload's renders (the
// dev-image swap) to the charts once more — the meta chart, and every
// component whose spec.values moved — for the verdict only: the images and
// the renders stay the first pass's. A chart that refuses the new values
// refuses the install, exactly as in the first pass.
func recheckReleases(cfg *config.Config, chart platformChart, before *platformRoster, values map[string]any) error {
	after, err := renderPlatformRoster(chart, values)
	if err != nil {
		if isSchemaRejection(err) {
			return chartRefusesValuesError(chart, err)
		}
		if registryUnreachable(err) {
			return chartUnreachableError(chart, err, "agentlab platform")
		}
		// Not a verdict: the first pass noted the same failure.
		return nil
	}
	var changed []fluxRelease
	for _, rel := range after.releases {
		if before != nil {
			i := slices.IndexFunc(before.releases, func(b fluxRelease) bool { return b.Name == rel.Name })
			if i >= 0 && bytes.Equal(before.releases[i].Values, rel.Values) {
				continue
			}
		}
		changed = append(changed, rel)
	}
	if len(changed) == 0 {
		return nil
	}
	_, _, _, errs := fluxReleaseImages(changed, componentAPIVersions(cfg))
	return judgeRenderFailures(chart, errs)
}

// rejectedComponentsError words the component charts that refuse the values
// their HelmReleases carry.
//
// A statement about the install, not about the preload: the chart is the one
// the OCIRepository resolves to and the values are the HelmRelease's own, so
// helm-controller coalesces and validates the same pair on the cluster
// (refusesTheInstall says when that holds) and the install would spend its
// whole wait discovering the verdict. Printed whole, never through excerpt:
// the offending paths are the last lines of Helm's message and the only part
// worth reading. The advice follows the release — the chart source for the
// meta chart's components (platformChart.remedy), agentlab for its own.
func rejectedComponentsError(chart platformChart, rejected []renderFailure) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d component chart(s) refuse the values their HelmReleases carry, so the install would fail after its wait:\n", len(rejected))
	var labOwned []string
	meta := false
	for _, f := range rejected {
		fmt.Fprintf(&b, "\n%s\n", indent(strings.TrimSpace(f.err.Error()), "  "))
		if f.rel.LabOwned {
			labOwned = append(labOwned, f.rel.Name)
		} else {
			meta = true
		}
	}
	if meta {
		fmt.Fprintf(&b, "\nThe meta chart and a component it pins disagree. %s", chart.remedy())
	}
	if len(labOwned) > 0 {
		fmt.Fprintf(&b, "\n%s: the lab's own release, outside the meta chart — its chart version and its values are agentlab's, so no chart pin in agentlab.yaml answers this. Update agentlab, or turn platform.observability off.", strings.Join(labOwned, ", "))
	}
	return errors.New(b.String())
}

// renderRetryDelays are the waits before each retry of a render the
// registry did not answer (registryUnreachable): three retries over some
// twenty seconds — what a resolver restart or a network change takes to
// settle — before the boot stops on it. A variable so tests do not sleep.
var renderRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

// renderAttempts is how often a render is tried before the registry counts
// as unreachable: once, then once per retry delay.
func renderAttempts() int { return 1 + len(renderRetryDelays) }

// retryUnreachable runs a render until it succeeds, fails for any reason but
// an unreachable registry, or has spent its retries, and returns its last
// error. A cause the transport has already retried (retriedInTransport: a
// timeout, a 5xx or 429) is returned at once — its retries are spent. Each
// retry is noted with the cause (unreachableCause), so the boot log shows
// the network failing before the boot stops on it.
func retryUnreachable(what string, render func() error) error {
	err := render()
	for i, delay := range renderRetryDelays {
		cause := unreachableCause(err)
		if cause == nil || retriedInTransport(cause) {
			return err
		}
		note("%s: the registry did not answer (%v); retry %d of %d in %s", what, cause, i+1, len(renderRetryDelays), delay)
		time.Sleep(delay)
		err = render()
	}
	return err
}

// unreachableComponentsError words the component charts the registry did
// not answer for through every retry, and why that stops the boot: the
// renders are what the lab reads its patches off (judgeRenderFailures).
// Printed whole: each release's error ends with the resolver's or the
// dialer's own words, the cause to act on.
func unreachableComponentsError(unreachable []renderFailure) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d component chart(s) could not be rendered, the registry did not answer through the retries:\n", len(unreachable))
	var hosts []string
	for _, f := range unreachable {
		fmt.Fprintf(&b, "\n%s\n", indent(strings.TrimSpace(f.err.Error()), "  "))
		hosts = append(hosts, registryHost(f.rel.URL))
	}
	slices.Sort(hosts)
	fmt.Fprintf(&b, "\nThe install is not started: the lab's patches for a component (the %s sidecar, platform.devImages) are read off its render, and without one it would run unpatched. Check this host's network and DNS for %s, then run `agentlab platform` again",
		dexLocalhostContainer, strings.Join(slices.Compact(hosts), ", "))
	return errors.New(b.String())
}

// chartUnreachableError words a meta chart the registry did not answer for
// through every retry: the component releases, and with them every render
// the lab's patches are read off, come out of that render. rerun is the
// command that picks the boot up again where it stopped.
func chartUnreachableError(chart platformChart, err error, rerun string) error {
	return fmt.Errorf("cannot render %s, the registry did not answer through the retries:\n\n%s\n\nThe install is not started. Check this host's network and DNS for %s, then run `%s` again",
		chart, indent(strings.TrimSpace(err.Error()), "  "), registryHost(chart.ref), rerun)
}

// registryHost is the registry an oci:// chart reference names — the host
// to check when it does not answer; the reference itself when it names none.
func registryHost(ref string) string {
	if u, err := url.Parse(ref); err == nil && u.Host != "" {
		return u.Host
	}
	return ref
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
