package lab

import (
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"

	"github.com/giantswarm/agentlab/pkg/project"
)

// labRESTClientGetter is the client-go plumbing every embedded Kubernetes
// client of the lab is built on — Helm's SDK (helm.go) today, the API clients
// that replace kubectl next: a genericclioptions.RESTClientGetter pinned to
// the lab-owned kubeconfig (labKubeconfig(), absolute — the kind cluster's
// own as exported by useClusterKubeconfig) and to the given namespace ("" for
// the kubeconfig's). The shell's KUBECONFIG and current-context play no part,
// the rule exec.go applies to kubectl; a lab that is not up fails on the
// missing file, by name. The REST config it yields is tuned for a boot that
// touches many objects (client-go's default of five requests a second
// throttles an install of the platform's size) and identifies the lab in the
// apiserver's audit log.
func labRESTClientGetter(namespace string) genericclioptions.RESTClientGetter {
	kubeconfig := labKubeconfig()
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &kubeconfig
	if namespace != "" {
		ns := namespace
		flags.Namespace = &ns
	}
	return flags.WithWrapConfigFn(func(c *rest.Config) *rest.Config {
		c.QPS = 50
		c.Burst = 100
		c.UserAgent = "agentlab/" + project.Version()
		return c
	})
}
