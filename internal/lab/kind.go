package lab

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"sigs.k8s.io/kind/pkg/apis/config/defaults"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
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

// kindNodeRepository is where the lab pulls the kind node image from: the
// gsoci mirror of kindest/node (retagger copies it digest-identically), so
// every image the lab pulls comes from gsoci.
const kindNodeRepository = "gsoci.azurecr.io/giantswarm/kind-node"

// kindNodeImage is the node image the lab boots — the Kubernetes the embedded
// kind pins, at kind's own tag and digest, from the gsoci mirror:
// "gsoci.azurecr.io/giantswarm/kind-node:v1.37.0@sha256:…".
func kindNodeImage() string {
	_, versioned, _ := strings.Cut(defaults.Image, ":")
	return kindNodeRepository + ":" + versioned
}

// kindNodeImageTag is kindNodeImage without the digest, for the reports:
// "gsoci.azurecr.io/giantswarm/kind-node:v1.37.0".
func kindNodeImageTag() string {
	img, _, _ := strings.Cut(kindNodeImage(), "@")
	return img
}

// kindToolVersion is the kind entry of the discovery report's embedded
// tools: the release and the node image it boots.
func kindToolVersion() string {
	return fmt.Sprintf("%s (%s)", kindVersion(), kindNodeImageTag())
}

// kindCreateCluster creates the cluster from the rendered kind config, waits
// up to kindClusterWait for its control plane and writes the admin kubeconfig
// to state/kubeconfig — the lab's file, never the user's. The CLI's closing
// "Set kubectl context" and "Have a nice day" are left out: the lab prints
// its own summary once everything is up.
func kindCreateCluster(name string, rawConfig []byte) error {
	err := kindProvider().Create(name,
		cluster.CreateWithNodeImage(kindNodeImage()),
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
// `ctr images import` over the engine's exec (loadNodeArchive), what `kind
// load image-archive` runs minus its digest records (preload.go says why the
// archive, HACKS.md U21), reading the stream as
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
		return kindError("loading images into "+nodes[0].String(), loadNodeArchive(nodes[0], archive))
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
			errs[i] = kindError("loading images into "+node.String(), loadNodeArchive(node, r))
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

// loadNodeArchive imports an image archive into one node's containerd: the
// `ctr images import` of kind's nodeutils.LoadImageArchive without its
// `--digests`. That flag records every manifest a second time as
// `import-<date>@sha256:…`, a name without a registry: the CRI lists it as
// docker.io/library/import-…, which containerd does not have, and an image
// whose containers resolve to that name (one saved by digest, whose archive
// carries no other) fails every container create with "failed to check if
// this is a checkpoint image … not found", after a node restart too. Without
// it the images land under the names they were saved as, the references the
// manifests use; an entry without a name is left unrecorded and the kubelet
// pulls it. The snapshotter is the CRI's, read the way kind reads it.
func loadNodeArchive(node nodes.Node, archive io.Reader) error {
	dump, err := kindexec.Output(node.Command("containerd", "config", "dump"))
	if err != nil {
		return fmt.Errorf("detecting the containerd snapshotter: %w", err)
	}
	snapshotter, err := criSnapshotter(dump)
	if err != nil {
		return err
	}
	return node.Command("ctr", "--namespace=k8s.io", "images", "import", "--all-platforms", "--snapshotter="+snapshotter, "-").SetStdin(archive).Run()
}

// criSnapshotter reads the CRI's snapshotter off `containerd config dump`:
// config version 2 (containerd 1.3 on) names it under the CRI plugin, 3 and 4
// (containerd 2.0, 2.1) under the CRI images plugin.
func criSnapshotter(dump []byte) (string, error) {
	var cfg map[string]any
	if _, err := toml.Decode(string(dump), &cfg); err != nil {
		return "", fmt.Errorf("detecting the containerd snapshotter: %w", err)
	}
	var path []string
	switch version := cfg["version"]; version {
	case int64(2):
		path = []string{"plugins", "io.containerd.grpc.v1.cri", "containerd", "snapshotter"}
	case int64(3), int64(4):
		path = []string{"plugins", "io.containerd.cri.v1.images", "snapshotter"}
	default:
		return "", fmt.Errorf("detecting the containerd snapshotter: unknown containerd config version %v (2, 3 and 4 are known)", version)
	}
	var v any = cfg
	for _, key := range path {
		table, _ := v.(map[string]any)
		v = table[key]
	}
	snapshotter, _ := v.(string)
	if snapshotter == "" {
		return "", fmt.Errorf("detecting the containerd snapshotter: no %s in the containerd config", strings.Join(path, "."))
	}
	return snapshotter, nil
}
