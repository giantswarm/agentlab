package lab

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"agentlab/internal/config"
)

// preloadImagesFile is the self-maintaining image cache manifest: after a
// successful boot the node's workload images are snapshotted here, and the
// next boot pulls them on the HOST (whose docker cache survives `agentlab
// down`) and side-loads them into the fresh node. `down` deletes the node and
// with it every image, so without this every boot re-pulls the whole platform
// from the network.
const preloadImagesFile = StateDir + "/preload-images.txt"

// infraImagePrefixes are baked into the kindest/node image already — nothing
// to preload, so the snapshot skips them.
var infraImagePrefixes = []string{"registry.k8s.io/", "docker.io/kindest/"}

// preloadResult is what the load stage reports back to Up.
type preloadResult struct {
	n   int // images actually side-loaded into the node
	d   time.Duration
	err error
}

// pullLabImages starts pulling the cached image list into the host docker
// cache in the background and yields the refs that are available on the host
// (freshly pulled or already cached). Independent of the cluster, so it
// overlaps with cluster creation. A missing manifest (first boot) or a failed
// pull is not an error — those images just fall back to in-node pulls.
func pullLabImages() <-chan []string {
	done := make(chan []string, 1)
	images := readPreloadManifest()
	if len(images) == 0 {
		done <- nil
		return done
	}
	go func() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		var have []string
		for _, img := range images {
			wg.Go(func() {
				if _, err := outputQuiet("docker", "image", "inspect", img); err != nil {
					if err := runQuiet("docker", "pull", "-q", img); err != nil {
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
		done <- have
	}()
	return done
}

// loadLabImages waits for the host pulls and side-loads the images into the
// kind node, so the pods find every layer already present instead of pulling
// over the network. Quiet: it runs concurrently with the Dex/OIDC phase and
// Up reports the result when it joins the channel.
func loadLabImages(cfg *config.Config, pulled <-chan []string) <-chan preloadResult {
	done := make(chan preloadResult, 1)
	go func() {
		start := time.Now()
		images := <-pulled
		// Skip what the node already has: on a fresh node this filters
		// nothing, on a re-run over a live cluster it skips the whole load.
		if have, err := nodeImageTags(cfg.ControlPlaneNode()); err == nil {
			images = slices.DeleteFunc(images, func(img string) bool {
				return slices.Contains(have, img)
			})
		}
		if len(images) == 0 {
			done <- preloadResult{}
			return
		}
		args := append([]string{"load", "docker-image", "--name", cfg.ClusterName}, images...)
		if err := runQuiet("kind", args...); err != nil {
			done <- preloadResult{err: err}
			return
		}
		done <- preloadResult{n: len(images), d: time.Since(start).Round(time.Second)}
	}()
	return done
}

// reportPreload joins the load stage and notes the outcome; failures are
// informational — the pods pull from the network exactly as they would have
// without the cache.
func reportPreload(loaded <-chan preloadResult) {
	res := <-loaded
	switch {
	case res.err != nil:
		note("image preload failed (%v); pods will pull from the network", res.err)
	case res.n > 0:
		note("preloaded %d cached images into the node (%s)", res.n, res.d)
	}
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
