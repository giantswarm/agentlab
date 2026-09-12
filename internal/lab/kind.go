package lab

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/kind/pkg/apis/config/defaults"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/cmd"
	kindversion "sigs.k8s.io/kind/pkg/cmd/kind/version"
	kindexec "sigs.k8s.io/kind/pkg/exec"
)

// kind is a Go dependency of the lab, not a tool on PATH: creating, listing
// and deleting the cluster, reading its kubeconfig and side-loading images go
// through sigs.k8s.io/kind's own packages — the code the kind CLI runs, minus
// the CLI. What follows from that:
//
//   - the kind release, and with it the default node image — the Kubernetes
//     the lab boots, since the rendered kind config pins none — is fixed by
//     go.mod and bumped by Renovate; the machine's kind, if any, is never
//     consulted and there is no version floor to check;
//   - kind drives the container engine through its CLI — `docker`, or
//     `podman` when the docker CLI is podman's (runtime.go) — so the engine
//     stays the one prerequisite embedding kind cannot remove;
//   - the admin kubeconfig is written to the lab's own state/kubeconfig
//     (CreateWithKubeconfigPath) and dropped from there again on delete;
//     the user's ~/.kube/config is never read or merged into.

// kindToolName names kind in the discovery report.
const kindToolName = "kind"

// kindClusterWait bounds how long cluster creation waits for the control
// plane to be Ready — the CLI's `--wait`.
const kindClusterWait = 120 * time.Second

// kindProvider is the cluster provider for the engine the lab drives, built
// once per process: docker, or podman when the docker CLI is podman's. It
// logs the way the kind CLI does — the spinner on a terminal, plain lines
// otherwise — on stderr, so a boot reads like `kind create cluster` did.
var kindProvider = sync.OnceValue(func() *cluster.Provider {
	engine := cluster.ProviderWithDocker()
	if dockerIsPodman() {
		engine = cluster.ProviderWithPodman()
	}
	return cluster.NewProvider(cluster.ProviderWithLogger(cmd.NewLogger()), engine)
})

// kindVersion is the embedded kind release, "v0.32.0".
func kindVersion() string {
	return "v" + kindversion.Version()
}

// kindNodeImage is the embedded kind's default node image — the Kubernetes
// the lab boots — without its digest: "kindest/node:v1.36.1".
func kindNodeImage() string {
	img, _, _ := strings.Cut(defaults.Image, "@")
	return img
}

// kindToolVersion is the kind entry of the discovery report's embedded
// tools: the release and the node image it boots, "v0.32.0 (kindest/node:v1.36.1)".
func kindToolVersion() string {
	return fmt.Sprintf("%s (%s)", kindVersion(), kindNodeImage())
}

// kindCreateCluster creates the cluster from the rendered kind config, waits
// up to kindClusterWait for its control plane and writes the admin kubeconfig
// to state/kubeconfig — the lab's file, never the user's. The CLI's closing
// "Set kubectl context" and "Have a nice day" are left out: the lab prints
// its own summary once everything is up.
func kindCreateCluster(name string, rawConfig []byte) error {
	err := kindProvider().Create(name,
		cluster.CreateWithRawConfig(rawConfig),
		cluster.CreateWithWaitForReady(kindClusterWait),
		cluster.CreateWithKubeconfigPath(labKubeconfig()),
		cluster.CreateWithDisplayUsage(false),
		cluster.CreateWithDisplaySalutation(false),
	)
	return kindError("creating cluster "+name, err)
}

// kindDeleteCluster removes the cluster's node containers and its entries
// from state/kubeconfig (a missing file is fine; Down removes it afterwards
// anyway).
func kindDeleteCluster(name string) error {
	return kindError("deleting cluster "+name, kindProvider().Delete(name, labKubeconfig()))
}

// kindClusters lists the clusters whose node containers exist — in any
// state, an exited node included (node.go).
func kindClusters() ([]string, error) {
	clusters, err := kindProvider().List()
	return clusters, kindError("listing clusters", err)
}

// kindKubeconfigRaw reads the cluster's admin kubeconfig off its
// control-plane node, with the host-side endpoint — independent of any
// kubeconfig on the host. A variable so tests can stand in for the cluster.
var kindKubeconfigRaw = func(name string) ([]byte, error) {
	raw, err := kindProvider().KubeConfig(name, false)
	if err != nil {
		return nil, kindError("reading the kubeconfig of cluster "+name, err)
	}
	return []byte(raw), nil
}

// kindLoadArchive streams an image archive into every node of the cluster:
// `ctr images import` over the engine's exec, what `kind load image-archive`
// runs (preload.go says why the archive, HACKS.md U21), reading the stream as
// it comes — never a file. The lab is a single-node cluster
// (config.ControlPlaneNode); a cluster with more nodes gets the one stream
// fanned out to each of them, a node whose import stopped dropping out
// without stalling the rest. A variable so tests can stand in for the node.
var kindLoadArchive = func(clusterName string, archive io.Reader) error {
	nodes, err := kindProvider().ListInternalNodes(clusterName)
	if err != nil {
		return kindError("listing the nodes of cluster "+clusterName, err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("kind: cluster %q has no nodes to load images into", clusterName)
	}
	if len(nodes) == 1 {
		return kindError("loading images into "+nodes[0].String(), nodeutils.LoadImageArchive(nodes[0], archive))
	}
	sinks := make([]*nodeSink, len(nodes))
	writers := make([]io.Writer, len(nodes))
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		r, w := io.Pipe()
		sinks[i] = &nodeSink{w: w}
		writers[i] = sinks[i]
		wg.Go(func() {
			errs[i] = kindError("loading images into "+node.String(), nodeutils.LoadImageArchive(node, r))
			_ = r.Close() // the sink's next write fails and it goes quiet
		})
	}
	_, copyErr := io.Copy(io.MultiWriter(writers...), archive)
	for _, s := range sinks {
		_ = s.w.CloseWithError(copyErr)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// nodeSink is one node's end of a fanned-out archive stream: a write the
// node's import no longer takes (its pipe closed) is dropped and reported
// complete, so io.MultiWriter keeps feeding the other nodes — the failed
// import speaks for itself through kindLoadArchive's error.
type nodeSink struct {
	w    *io.PipeWriter
	dead bool
}

func (s *nodeSink) Write(p []byte) (int, error) {
	if !s.dead {
		if _, err := s.w.Write(p); err != nil {
			s.dead = true
		}
	}
	return len(p), nil
}

// kindError is the error a failed kind operation reports: kind's own message
// and, when a command under it failed, that command's output — what the kind
// CLI prints as "Command Output", where the diagnosis usually is.
func kindError(what string, err error) error {
	if err == nil {
		return nil
	}
	if rerr := kindexec.RunErrorForError(err); rerr != nil {
		if out := strings.TrimSpace(string(rerr.Output)); out != "" {
			return fmt.Errorf("kind: %s: %w\n%s", what, err, out)
		}
	}
	return fmt.Errorf("kind: %s: %w", what, err)
}
