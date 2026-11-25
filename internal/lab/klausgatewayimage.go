package lab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agentlab/internal/config"
)

// klausGatewayImageRepository is the klaus-gateway chart's image
// (image.registry/image.name); the release pipeline stamps the chart's
// version as its appVersion, so the image tag is the chart version.
const klausGatewayImageRepository = "gsoci.azurecr.io/giantswarm/klaus-gateway"

// chartVersionField is the chart version of a Flux HelmRelease's history entry.
const chartVersionField = "chartVersion"

// gatewayImage is the host gateway's image and where its version came from.
type gatewayImage struct {
	ref, source string
}

// componentGatewayImage is the klaus-gateway the lab's meta chart runs as
// its component, the default of every proof that starts a gateway on the
// host — so one chart bump moves the host gateway and the in-cluster one
// together. The component's HelmRelease names the version it applied; with
// the component off (or not installed yet) the meta chart the lab installs
// is rendered offline with the component on and its range resolved against
// the registry the way source-controller would.
func componentGatewayImage(cfg *config.Config) (gatewayImage, error) {
	if v, err := klausGatewayReleaseVersion(); err != nil {
		return gatewayImage{}, err
	} else if v != "" {
		return gatewayImage{ref: klausGatewayImageRepository + ":" + v,
			source: fmt.Sprintf("the version HelmRelease %s/%s applied", platformNamespace, klausGatewayComponent)}, nil
	}
	chart := platformChartFor(cfg)
	v, versionRange, err := chartGatewayVersion(cfg, chart)
	if err != nil {
		return gatewayImage{}, fmt.Errorf("resolving the klaus-gateway %s runs (name one with --gateway-image): %w", chart, err)
	}
	return gatewayImage{ref: klausGatewayImageRepository + ":" + v,
		source: fmt.Sprintf("the newest release %s's components.%s range %s admits", chart, klausGatewayComponent, versionRange)}, nil
}

// klausGatewayReleaseVersion is the chart version the component's HelmRelease
// last applied (status.history[0].chartVersion) without the build metadata
// Flux appends for an OCI chart (`4.6.1+<digest>`), which no image tag
// carries; empty when there is no such HelmRelease or it applied nothing yet.
func klausGatewayReleaseVersion() (string, error) {
	gvr, err := gvrFor(fluxHelmReleaseResource)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	hr, err := getObject(ctx, gvr, platformNamespace, klausGatewayComponent)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading HelmRelease %s/%s: %w", platformNamespace, klausGatewayComponent, err)
	}
	return chartImageTag(appliedChartVersion(hr)), nil
}

// chartImageTag is the image tag of a chart version: the version without
// its build metadata.
func chartImageTag(version string) string {
	tag, _, _ := strings.Cut(version, "+")
	return tag
}

// appliedChartVersion reads status.history[0].chartVersion off a Flux
// HelmRelease; empty before its first revision.
func appliedChartVersion(hr *unstructured.Unstructured) string {
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return ""
	}
	snapshot, _ := history[0].(map[string]any)
	v, _, _ := unstructured.NestedString(snapshot, chartVersionField)
	return v
}

// chartGatewayVersion renders the meta chart offline with the lab's values,
// the klaus-gateway component switched on, and resolves the component's
// range (and semverFilter, an overlay's channel) to the newest tag of its
// chart repository. Returns the version and the range it came from.
func chartGatewayVersion(cfg *config.Config, chart platformChart) (version, versionRange string, err error) {
	values, err := platformValuesWith(cfg, func(t *tmplData) { t.KlausGatewayEnabled = true })
	if err != nil {
		return "", "", err
	}
	roster, err := renderPlatformRoster(chart, values)
	if err != nil {
		return "", "", err
	}
	for _, rel := range roster.releases {
		if rel.Name != klausGatewayComponent {
			continue
		}
		tags, err := listChartTags(rel.hostURL(), rel.PlainHTTP)
		if err != nil {
			return "", "", err
		}
		v, err := pickFilteredTag(tags, rel.Version, rel.Filter)
		return v, rel.Version, err
	}
	return "", "", fmt.Errorf("its render has no %s component release", klausGatewayComponent)
}

// platformValuesWith is the meta chart's values as the lab installs them —
// the values template, then platform.valuesFiles on top — with the template
// data changed by mutate, merged from a temporary file so state/ keeps what
// the last install rendered.
func platformValuesWith(cfg *config.Config, mutate func(*tmplData)) (map[string]any, error) {
	content, err := renderTemplate(cfg, platformValuesTemplate, mutate)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "agentlab-values-*.yaml")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return platformValuesFiles(f.Name(), cfg.Platform.ValuesFiles)
}

// resolveGatewayImage fills an empty image with the component's (a binary
// needs none) and says which gateway the host runs.
func resolveGatewayImage(cfg *config.Config, image, binary string) (string, error) {
	switch {
	case binary != "":
		note("host gateway: the local build %s", binary)
		return image, nil
	case image != "":
		note("host gateway: %s (--gateway-image)", image)
		return image, nil
	}
	img, err := componentGatewayImage(cfg)
	if err != nil {
		return "", err
	}
	note("host gateway: %s — %s", img.ref, img.source)
	return img.ref, nil
}

// hostGatewayTrustProblem words why a gateway binary (--gateway-binary)
// cannot call muster on this host, "" when it can. The lab gives a host
// process the lab CA through SSL_CERT_FILE, and a Go program on macOS
// ignores it (a module whose go directive is below 1.27 keeps
// x509sslcertoverrideplatform=0) and verifies against the system trust
// store alone: the muster call fails "x509: certificate signed by unknown
// authority" unless `agentlab trust` put the lab CA there. The image runs
// on Linux, where SSL_CERT_FILE holds.
func hostGatewayTrustProblem(goos, binary string, labCATrusted bool) string {
	if binary == "" || goos != goosDarwin || labCATrusted {
		return ""
	}
	return fmt.Sprintf("--gateway-binary %s: on macOS the gateway calls muster through the system trust store, which does not hold the lab CA, so its muster calls fail with an unknown authority; run the image instead (--gateway-image), or put the lab CA in the system trust store first (`agentlab trust`)", binary)
}

// refuseUntrustedHostGateway is hostGatewayTrustProblem for this host, as
// an error: a proof whose gateway calls muster asks it before it starts.
func refuseUntrustedHostGateway(binary string) error {
	if binary == "" {
		return nil
	}
	if problem := hostGatewayTrustProblem(runtime.GOOS, binary, systemTrusted()); problem != "" {
		return errors.New(problem)
	}
	return nil
}
