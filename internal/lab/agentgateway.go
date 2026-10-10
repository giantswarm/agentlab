package lab

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/giantswarm/agentlab/internal/config"
)

// The agentgateway component — the platform's edge and MCP data plane — as
// the dev-image swap sees it. Its chart renders ONE Deployment, the
// controller (config.DevImageAgentgatewayController, a Deployment target of
// devImageTargets). The data-plane proxy, Deployment `agentgateway`, is the
// controller's: created at run time for the lab's Gateway from the image the
// chart hands the controller as three values — proxy.image.registry,
// .repository and .tag (its AGW_PROXY_IMAGE_* environment) — so no
// post-renderer on the component's release can reach it. The `agentgateway`
// target (config.DevImageAgentgateway) therefore travels as those values
// (agent-platform-values.yaml.tmpl), split off the ref the build is
// side-loaded under (labImageRef) so the proxy's ref and the node's copy
// agree, and the controller's proxies pull IfNotPresent. After the install
// the boot waits for the controller to roll the proxy onto it
// (reportAgentgatewayDevImage).

// agentgatewayComponent is the meta chart's component and release name, and
// the proxy's Deployment and container name alike.
const agentgatewayComponent = config.DevImageAgentgateway

// agentgatewayRollTimeout bounds the wait for the controller to roll the
// proxy Deployment onto a dev image: the controller's own roll is behind the
// HelmRelease's wait, the proxy's follows its reconcile.
const agentgatewayRollTimeout = 3 * time.Minute

// The keys of the chart's proxy.image block, as the merged values are
// checked against (checkValues).
const (
	proxyImageRegistryKey   = "registry"
	proxyImageRepositoryKey = "repository"
	proxyImageTagKey        = "tag"
)

// agentgatewayProxyImage is the `agentgateway` dev image in the shape the
// chart's proxy.image values take.
type agentgatewayProxyImage struct {
	Registry, Repository, Tag string
}

// ref is the image the proxy names from the three fields, the way the
// controller composes it.
func (p agentgatewayProxyImage) ref() string {
	return p.Registry + "/" + p.Repository + ":" + p.Tag
}

// agentgatewayDevImage is the configured `agentgateway` dev image as the
// proxy.image values carry it, nil without the target. The ref is the one
// the build is side-loaded under: its first path component names the
// registry (labImageRef puts a build of this host under localhost/), the
// rest is the repository, the tag follows the last colon after the last
// slash (a digest ref is refused by the config).
func agentgatewayDevImage(cfg *config.Config) *agentgatewayProxyImage {
	ref, ok := cfg.Platform.DevImages[agentgatewayComponent]
	if !ok {
		return nil
	}
	full := labImageRef(ref)
	registry, path, _ := strings.Cut(full, "/")
	repository, tag := path, ""
	if colon := strings.LastIndex(path, ":"); colon > strings.LastIndex(path, "/") {
		repository, tag = path[:colon], path[colon+1:]
	}
	return &agentgatewayProxyImage{Registry: registry, Repository: repository, Tag: tag}
}

// checkValues asserts the merged values still carry the proxy image: a
// platform.valuesFiles overlay that sets agentgateway.proxy.image itself
// wins the merge and the swap would be a silent no-op.
func (p *agentgatewayProxyImage) checkValues(values map[string]any) error {
	if p == nil {
		return nil
	}
	for key, want := range map[string]string{proxyImageRegistryKey: p.Registry, proxyImageRepositoryKey: p.Repository, proxyImageTagKey: p.Tag} {
		if got := nestedString(values, agentgatewayComponent, "proxy", "image", key); got != want {
			return fmt.Errorf("platform.devImages.%s: the merged values set agentgateway.proxy.image.%s to %q, not to the dev image's %q — a platform.valuesFiles overlay sets it; drop that key from the overlay while the dev image is configured", agentgatewayComponent, key, got, want)
		}
	}
	return nil
}

// reportAgentgatewayDevImage waits, after the install, for the controller to
// run the proxy Deployment on the dev image — the controller rolled onto the
// new proxy values with its release, and re-renders the proxy from them —
// and for that rollout to complete, then says so. A proxy still on another
// image when the wait runs out is the error, naming both.
func reportAgentgatewayDevImage(ctx context.Context, p *agentgatewayProxyImage) error {
	k, err := labKube()
	if err != nil {
		return err
	}
	want := p.ref()
	var last string
	err = wait.PollUntilContextTimeout(ctx, pollInterval, agentgatewayRollTimeout, true, func(ctx context.Context) (bool, error) {
		d, err := k.clientset.AppsV1().Deployments(platformNamespace).Get(ctx, agentgatewayComponent, metav1.GetOptions{})
		if err != nil {
			return false, nil // the controller has not created it yet
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == agentgatewayComponent {
				last = c.Image
				return c.Image == want, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("the proxy Deployment %s/%s runs %q, not the dev image %s, after %s: the agentgateway controller did not re-render it from agentgateway.proxy.image (check the controller's AGW_PROXY_IMAGE_* environment in `kubectl -n %s get deploy %s -o yaml` and the merged values in %s)", platformNamespace, agentgatewayComponent, last, want, agentgatewayRollTimeout, platformNamespace, config.DevImageAgentgatewayController, StateDir+"/agent-platform-values.yaml")
	}
	if err := waitDeploymentRolledOut(ctx, platformNamespace, agentgatewayComponent, agentgatewayRollTimeout); err != nil {
		return err
	}
	note("the agentgateway proxy (Deployment %s) runs %s", agentgatewayComponent, want)
	return nil
}
