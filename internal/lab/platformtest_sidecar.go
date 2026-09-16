package lab

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agentlab/internal/config"
)

// dexLocalhostServer is one OAuth resource server platform-test proved the
// lab's Dex bridge on: its Deployment and the MCPServer that reads Connected
// for it ("" when it registers none).
type dexLocalhostServer struct {
	namespace, deployment, mcpServer string
}

func (s dexLocalhostServer) String() string {
	if s.mcpServer == "" {
		return s.namespace + "/" + s.deployment
	}
	return s.deployment
}

// proveDexLocalhostSidecars is platform-test's check of the lab's OAuth
// bridge, the sidecar rule read off the live objects this time: every
// Deployment in the platform namespace (and monitoring, with observability)
// whose containers are told the lab Dex's localhost address and that is not
// on the host network must carry the dex-localhost container; its rollout
// must be complete with every pod Ready, every container running and none
// ever restarted — an unreachable issuer is a crash loop, and that is what
// this catches; and the MCPServer of its name, when it registers one, must
// read Connected: muster reached it through the bridge with this session's
// token. A server the rule finds without the sidecar is the lab's gap — a
// component turned on by the chart or an overlay that `agentlab platform`
// did not patch.
func proveDexLocalhostSidecars(ctx context.Context, cfg *config.Config) ([]dexLocalhostServer, error) {
	k, err := labKube()
	if err != nil {
		return nil, err
	}
	mcpServers, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil, err
	}
	namespaces := []string{platformNamespace}
	if cfg.Platform.Observability {
		namespaces = append(namespaces, observabilityNamespace)
	}
	addr := dexLocalhostAddr(cfg)
	var proven []dexLocalhostServer
	for _, ns := range namespaces {
		list, err := k.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing Deployments in %s: %w", ns, err)
		}
		slices.SortFunc(list.Items, func(a, b appsv1.Deployment) int { return strings.Compare(a.Name, b.Name) })
		for i := range list.Items {
			d := &list.Items[i]
			if d.Spec.Template.Spec.HostNetwork || !podSpecMentions(&d.Spec.Template.Spec, addr) {
				continue
			}
			if err := checkDexLocalhostDeployment(ctx, k, d); err != nil {
				return nil, err
			}
			server := dexLocalhostServer{namespace: ns, deployment: d.Name}
			if registered, err := objectExists(ctx, mcpServers, platformNamespace, d.Name); err != nil {
				return nil, err
			} else if registered {
				server.mcpServer = d.Name
				if err := waitMCPServerState(d.Name, mcpServerStateConnected); err != nil {
					return nil, err
				}
			} else {
				note("%s/%s carries the sidecar and registers no MCPServer of its name", ns, d.Name)
			}
			proven = append(proven, server)
		}
	}
	if len(proven) == 0 {
		return nil, fmt.Errorf("no Deployment in %s is told the lab Dex address %s — the platform's OAuth resource servers (mcp-kubernetes at least) should be; check `kubectl -n %s get deploy` and `agentlab logs mcp-kubernetes`", strings.Join(namespaces, ", "), addr, platformNamespace)
	}
	return proven, nil
}

// checkDexLocalhostDeployment asserts one selected Deployment carries the
// sidecar, rolled out completely, with every pod Ready, every container
// running and none restarted.
func checkDexLocalhostDeployment(ctx context.Context, k *kubeClients, d *appsv1.Deployment) error {
	where := d.Namespace + "/" + d.Name
	if !slices.ContainsFunc(d.Spec.Template.Spec.Containers, func(c corev1.Container) bool { return c.Name == dexLocalhostContainer }) {
		return fmt.Errorf("the Deployment %s is told the lab Dex address but carries no %s sidecar — the lab did not patch this component; check the `%s sidecar on …` line of `agentlab platform` and components.%s.postRenderers in state/agent-platform-values.yaml",
			where, dexLocalhostContainer, dexLocalhostContainer, d.Name)
	}
	status, done, err := deploymentRolloutStatus(d)
	if err != nil {
		return fmt.Errorf("the Deployment %s: %w", where, err)
	}
	if !done {
		return fmt.Errorf("the Deployment %s has not rolled out: %s", where, status)
	}
	pods, err := k.targetPods(ctx, d.Namespace, "deploy/"+d.Name)
	if err != nil {
		return err
	}
	var restarts []string
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("pod %s/%s of Deployment %s is %s, want Running (%s)", pod.Namespace, pod.Name, d.Name, pod.Status.Phase, podStateSummary(pod))
		}
		for _, c := range pod.Status.ContainerStatuses {
			if !c.Ready || c.State.Running == nil {
				return fmt.Errorf("container %s of pod %s/%s is not running and ready (%s) — with %s the issuer is reachable, so look at `kubectl -n %s logs %s -c %s`",
					c.Name, pod.Namespace, pod.Name, podStateSummary(pod), dexLocalhostContainer, pod.Namespace, pod.Name, c.Name)
			}
			if c.RestartCount > 0 {
				restarts = append(restarts, fmt.Sprintf("%s/%s %s ×%d", pod.Namespace, pod.Name, c.Name, c.RestartCount))
			}
		}
	}
	if len(restarts) > 0 {
		return fmt.Errorf("the Deployment %s: containers restarted — %s; a server that could not reach the issuer crash-loops until the sidecar is there, so read the previous container's log (`kubectl -n %s logs -p …`)",
			where, strings.Join(restarts, ", "), d.Namespace)
	}
	note("%s: %s sidecar, %d pods Running, 0 restarts", where, dexLocalhostContainer, len(pods))
	return nil
}

// podSpecMentions reports whether any container of the pod spec carries the
// string in an argument or an environment variable's value — the live
// counterpart of renderedWorkload.mentions.
func podSpecMentions(spec *corev1.PodSpec, s string) bool {
	for _, c := range spec.Containers {
		if slices.ContainsFunc(c.Args, func(arg string) bool { return strings.Contains(arg, s) }) {
			return true
		}
		if slices.ContainsFunc(c.Env, func(env corev1.EnvVar) bool { return strings.Contains(env.Value, s) }) {
			return true
		}
	}
	return false
}
