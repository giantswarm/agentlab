package lab

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's image rule: images are ALWAYS pulled on the HOST and side-loaded
// into the node, never pulled by the kubelet. The host docker cache survives
// `agentlab down`; the node's containerd dies with it. An in-node pull is
// therefore paid again on every boot — and worse, a pull the kubelet started
// never adopts a concurrently side-loaded image, so in-node pulls also race
// every rollout timeout on a slow network. Three lanes feed the node:
//
//   - the Dex image (pullDexImage/sideloadDexImage): known from config,
//     side-loaded synchronously before the Dex Deployment lands;
//   - the snapshot manifest (pullLabImages/loadLabImages): everything the
//     last successful boot ran, loaded in the background while Dex and the
//     OIDC verification run, joined before the platform install;
//   - the chart-derived set (platformImages in platform.go): what the charts
//     about to be installed actually reference, so even a FIRST boot (no
//     manifest yet) pulls on the host.
//
// A failed host pull or side-load is never fatal: the affected image just
// degrades to the in-node pull it would have been anyway.

// preloadImagesFile is the self-maintaining image cache manifest: after a
// successful boot the node's workload images are snapshotted here. It backs
// the manifest lane and catches images the chart render cannot see (e.g. the
// ADK runtime images kagent composes at run time).
const preloadImagesFile = StateDir + "/preload-images.txt"

// infraImagePrefixes are baked into the kindest/node image already — nothing
// to preload, so the snapshot skips them.
var infraImagePrefixes = []string{"registry.k8s.io/", "docker.io/kindest/"}

// preloadResult is what a side-load reports back.
type preloadResult struct {
	n   int // images actually side-loaded into the node
	d   time.Duration
	err error
}

// hostPullImages ensures every ref is in the host docker cache, pulling the
// missing ones concurrently, and returns the refs that are available there
// (already cached or freshly pulled), sorted. Failed pulls are dropped:
// callers side-load what exists and the node pulls the rest itself.
func hostPullImages(images []string) []string {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var have []string
	for _, img := range images {
		wg.Go(func() {
			if _, err := outputQuiet("docker", "image", "inspect", img); err != nil {
				// Pull with stderr swallowed: the chart-derived lane scrapes
				// the odd unpullable ref out of config blobs (e.g. a bare
				// `giantswarm/valkey`, which docker reads as a Docker Hub
				// repo), and dropping it here IS the filter — not an error
				// worth showing.
				if _, err := outputQuiet("docker", "pull", "-q", img); err != nil {
					return
				}
			}
			mu.Lock()
			have = append(have, img)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.Sort(have)
	return have
}

// sideloadImages side-loads the given host-cached refs into the node in one
// `kind load`, skipping what the node already has — on a re-run over a live
// cluster that skip usually covers everything.
func sideloadImages(cfg *config.Config, images []string) preloadResult {
	start := time.Now()
	if have, err := nodeImageTags(cfg.ControlPlaneNode()); err == nil {
		images = slices.DeleteFunc(images, func(img string) bool {
			return slices.Contains(have, img)
		})
	}
	if len(images) == 0 {
		return preloadResult{}
	}
	args := append([]string{"load", "docker-image", "--name", cfg.ClusterName}, images...)
	if err := runQuiet("kind", args...); err != nil {
		return preloadResult{err: err}
	}
	return preloadResult{n: len(images), d: time.Since(start).Round(time.Second)}
}

// pullLabImages starts pulling the snapshot manifest's images into the host
// docker cache in the background and yields the refs available on the host.
// Independent of the cluster, so it overlaps with cluster creation. A missing
// manifest (first boot) is not an error — the chart-derived lane covers the
// platform images and the node pulls any leftovers.
func pullLabImages(cfg *config.Config) <-chan []string {
	done := make(chan []string, 1)
	images := readPreloadManifest()
	// The Dex image rides its own lane (pullDexImage/sideloadDexImage): it
	// must be in the node before the Dex Deployment lands, not whenever the
	// bulk load finishes. Dropped here so the lanes never double-pull or
	// double-load it.
	images = slices.DeleteFunc(images, func(img string) bool { return img == cfg.DexImage })
	if len(images) == 0 {
		done <- nil
		return done
	}
	go func() { done <- hostPullImages(images) }()
	return done
}

// loadLabImages waits for the host pulls and side-loads the images into the
// node. Quiet: it runs concurrently with the Dex/OIDC phase and Up reports
// the result when it joins the channel.
func loadLabImages(cfg *config.Config, pulled <-chan []string) <-chan preloadResult {
	done := make(chan preloadResult, 1)
	go func() { done <- sideloadImages(cfg, <-pulled) }()
	return done
}

// pullDexImage ensures the Dex image is in the host docker cache in the
// background, independent of the snapshot manifest: the ref is known from
// config, so even a first boot (no manifest yet) caches it for every boot
// after this one. The buffered channel yields availability.
func pullDexImage(cfg *config.Config) <-chan bool {
	done := make(chan bool, 1)
	img := cfg.DexImage
	go func() { done <- len(hostPullImages([]string{img})) == 1 }()
	return done
}

// sideloadDexImage joins the host-side Dex pull and side-loads the image into
// the node BEFORE the Deployment lands. This one image cannot ride the bulk
// side-load: that runs concurrently with the Dex deploy, and the kubelet
// starts its own network pull the moment the pod exists — on a slow network
// that pull alone has outlasted ApplyDex's rollout timeout with the image
// sitting in the host cache the whole time.
func sideloadDexImage(cfg *config.Config, ready <-chan bool) {
	var ok bool
	select {
	case ok = <-ready:
	default:
		note("waiting for the host pull of %s", cfg.DexImage)
		ok = <-ready
	}
	if !ok {
		note("%s is neither cached on the host nor pullable; the node pulls it instead", cfg.DexImage)
		return
	}
	res := sideloadImages(cfg, []string{cfg.DexImage})
	if res.err != nil {
		note("side-loading %s failed (%v); the node pulls it instead", cfg.DexImage, res.err)
		return
	}
	if res.n > 0 {
		note("side-loaded %s from the host cache (%s)", cfg.DexImage, res.d)
	}
}

// reportPreload joins the manifest lane's load stage and notes the outcome;
// failures are informational — the pods pull from the network exactly as they
// would have without the cache.
func reportPreload(loaded <-chan preloadResult) {
	res := <-loaded
	switch {
	case res.err != nil:
		note("image preload failed (%v); pods will pull from the network", res.err)
	case res.n > 0:
		note("preloaded %d cached images into the node (%s)", res.n, res.d)
	}
}

// imageLineRe matches the image fields of rendered Kubernetes manifests.
var imageLineRe = regexp.MustCompile(`(?m)^\s*(?:-\s*)?image:\s*["']?([^\s"']+)["']?\s*$`)

// structuredImageRe matches the structured image block of an
// AgentgatewayParameters CR (registry / repository / tag as separate keys) —
// the data-plane image the agentgateway controller deploys at RUN TIME, so it
// appears in no rendered pod spec the plain scraper could see.
var structuredImageRe = regexp.MustCompile(`(?m)image:\s*\n\s*registry:\s*["']?([^\s"']+)["']?\s*\n\s*repository:\s*["']?([^\s"']+)["']?\s*\n\s*tag:\s*["']?([^\s"']+)["']?`)

// scrapeImages extracts the image refs from rendered manifests. Only refs
// carrying a tag or digest count: a bare word under some config blob's
// `image:` key is not pullable and would poison the pull set.
func scrapeImages(rendered string) []string {
	var imgs []string
	for _, m := range imageLineRe.FindAllStringSubmatch(rendered, -1) {
		if ref := m[1]; strings.ContainsAny(ref, ":@") {
			imgs = append(imgs, ref)
		}
	}
	for _, m := range structuredImageRe.FindAllStringSubmatch(rendered, -1) {
		imgs = append(imgs, fmt.Sprintf("%s/%s:%s", m[1], m[2], m[3]))
	}
	slices.Sort(imgs)
	return slices.Compact(imgs)
}

// snapshotPreloadImages records the node's current workload images into the
// preload manifest for the next boot. Best-effort: a failed snapshot only
// costs the next boot its cache.
func snapshotPreloadImages(cfg *config.Config) {
	tags, err := nodeImageTags(cfg.ControlPlaneNode())
	if err != nil {
		return
	}
	var images []string
	for _, tag := range tags {
		infra := false
		for _, p := range infraImagePrefixes {
			if strings.HasPrefix(tag, p) {
				infra = true
				break
			}
		}
		if !infra {
			images = append(images, tag)
		}
	}
	if len(images) == 0 {
		return
	}
	slices.Sort(images)
	images = slices.Compact(images)
	content := "# Images the last successful boot ran; the next `agentlab up` pulls\n" +
		"# them on the host and side-loads them into the fresh node. Regenerated\n" +
		"# after every boot — safe to delete.\n" +
		strings.Join(images, "\n") + "\n"
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return
	}
	_ = os.WriteFile(preloadImagesFile, []byte(content), 0o600)
}

// readPreloadManifest returns the cached image list, tolerating comments and
// blank lines; missing file means no cache yet.
func readPreloadManifest() []string {
	raw, err := os.ReadFile(filepath.FromSlash(preloadImagesFile))
	if err != nil {
		return nil
	}
	var images []string
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			images = append(images, line)
		}
	}
	return images
}

// nodeImageTags lists every repo:tag known to the kind node's containerd.
func nodeImageTags(node string) ([]string, error) {
	out, err := outputQuiet("docker", "exec", node, "crictl", "images", "-o", "json")
	if err != nil {
		return nil, err
	}
	var imgs struct {
		Images []struct {
			RepoTags []string `json:"repoTags"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(out), &imgs); err != nil {
		return nil, fmt.Errorf("parsing crictl images: %w", err)
	}
	var tags []string
	for _, img := range imgs.Images {
		tags = append(tags, img.RepoTags...)
	}
	return tags, nil
}
