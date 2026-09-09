package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// sideloadImages side-loads the given host-cached refs into the node,
// skipping what the node already has — on a re-run over a live cluster that
// skip usually covers everything.
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
	loaded, err := kindLoadImages(cfg, images)
	return preloadResult{n: loaded, d: time.Since(start).Round(time.Second), err: err}
}

// kindLoadImages side-loads images and reports how many landed. Every load is
// a `docker save` into a temporary archive that the embedded kind imports
// into the node (kindLoadArchive) — what `kind load image-archive` does, and
// the recipe kind's maintainers give consumers (HACKS.md U21). Under docker
// the batch is saved per platform (dockerLoadImages). Under podman it is one
// archive per image: podman's multi-image save folds the batch into one
// image under every tag (runtime.go, HACKS.md U16), and a single-image save
// is always right. A failed image does not stop the rest, so the count and
// the error are both reported and a partial load reads as one.
func kindLoadImages(cfg *config.Config, images []string) (int, error) {
	if !dockerIsPodman() {
		return dockerLoadImages(cfg, images)
	}
	loaded := 0
	var errs []error
	for _, img := range images {
		if err := saveAndLoadArchive(cfg, "", []string{img}); err != nil {
			errs = append(errs, err)
			continue
		}
		loaded++
	}
	return loaded, errors.Join(errs...)
}

// dockerLoadImages is the docker side-load: `docker save --platform <what the
// host holds>` into an archive per platform, imported into the node. A plain
// `docker save` — what `kind load docker-image` runs — breaks under Docker's
// containerd image store (the default on Docker Desktop and on new Docker 29
// installs): that archive carries the image's whole multi-platform index
// while only the host platform's blobs were ever pulled, and the node's `ctr
// images import --all-platforms` fails on the first missing digest
// (kubernetes-sigs/kind#3795, HACKS.md U21). Saving only the platform the
// host has — `docker image inspect` says which — writes an archive whose
// index references nothing it does not carry. `docker save --platform`
// arrived with Docker 28 (API 1.48); an older engine gets one plain archive
// of the whole batch, which is right under the classic graph driver — the
// only store such an engine is likely to run.
func dockerLoadImages(cfg *config.Config, images []string) (int, error) {
	if !dockerSaveHasPlatform() {
		if err := saveAndLoadArchive(cfg, "", images); err != nil {
			return 0, err
		}
		return len(images), nil
	}
	groups, errs := hostImagePlatforms(images)
	loaded := 0
	for _, platform := range slices.Sorted(maps.Keys(groups)) {
		imgs := groups[platform]
		if err := saveAndLoadArchive(cfg, platform, imgs); err != nil {
			errs = append(errs, err)
			continue
		}
		loaded += len(imgs)
	}
	return loaded, errors.Join(errs...)
}

// imagePlatformFormat is the `docker image inspect` template that spells the
// platform the host holds an image in the way `docker save --platform` wants
// it: os/arch, with the variant where there is one (linux/arm/v7).
const imagePlatformFormat = "{{.Os}}/{{.Architecture}}{{if .Variant}}/{{.Variant}}{{end}}"

// hostImagePlatforms groups refs by the platform the host cache holds them in,
// keeping each group in input order. A ref the host cannot inspect is not in
// the cache: it is reported and left out, and the node pulls it itself.
func hostImagePlatforms(images []string) (map[string][]string, []error) {
	platforms := make([]string, len(images))
	errs := make([]error, len(images))
	var wg sync.WaitGroup
	for i, img := range images {
		wg.Go(func() {
			out, err := outputQuiet("docker", "image", "inspect", "-f", imagePlatformFormat, img)
			platforms[i], errs[i] = strings.TrimSpace(out), err
		})
	}
	wg.Wait()
	groups := map[string][]string{}
	var failed []error
	for i, img := range images {
		if errs[i] != nil {
			failed = append(failed, errs[i])
			continue
		}
		groups[platforms[i]] = append(groups[platforms[i]], img)
	}
	return groups, failed
}

// saveAndLoadArchive writes the images — their <platform> variants when one
// is named, else as `docker save` has them — to a temporary archive and loads
// it into the cluster's nodes. The archive is removed either way; the kind
// CLI stages its own the same way, so the footprint is the one `kind load
// docker-image` always had. docker's one-line stderr (the ref and platform it
// could not export) belongs in the error the caller reports, as does kind's
// (kindError carries the node-side command's output).
func saveAndLoadArchive(cfg *config.Config, platform string, images []string) error {
	f, err := os.CreateTemp("", "agentlab-images-*.tar")
	if err != nil {
		return err
	}
	tar := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(tar) }()
	args := []string{"save"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	args = append(append(args, "-o", tar), images...)
	if _, err := outputQuiet("docker", args...); err != nil {
		return err
	}
	return kindLoadArchive(cfg.ClusterName, tar)
}

// dockerSaveHasPlatform reports whether `docker save --platform` works here:
// the flag arrived with Docker 28 (API 1.48) and both the client and the
// daemon have to speak that API. Cached for the process; a variable so tests
// can pin the answer.
var dockerSaveHasPlatform = sync.OnceValue(func() bool {
	out, err := outputQuiet("docker", "version", "-f", "{{.Client.APIVersion}} {{.Server.APIVersion}}")
	return err == nil && saveHasPlatform(out)
})

// saveHasPlatform reads `docker version -f '{{.Client.APIVersion}}
// {{.Server.APIVersion}}'` ("1.53 1.53"): both at 1.48 or newer. The client
// reports the version it negotiated with the daemon, so an old daemon shows on
// both sides; an old client never prints a version it does not know.
func saveHasPlatform(out string) bool {
	versions := strings.Fields(out)
	if len(versions) != 2 {
		return false
	}
	for _, v := range versions {
		if !apiAtLeast(v, 1, 48) {
			return false
		}
	}
	return true
}

// apiAtLeast compares a docker API version ("1.48") against major.minor.
func apiAtLeast(v string, major, minor int) bool {
	before, after, ok := strings.Cut(strings.TrimSpace(v), ".")
	if !ok {
		return false
	}
	gotMajor, err := strconv.Atoi(before)
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(after)
	if err != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
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
	case res.err != nil && res.n > 0:
		note("preloaded %d cached images into the node (%s); the rest failed (%v) and their pods pull from the network", res.n, res.d, res.err)
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
	slices.Sort(images)
	images = slices.Compact(images)
	// Images built here were side-loaded by whoever built them and have no
	// registry to be pulled from on the next boot. They are remembered as
	// such, so a later `docker image prune` on the host does not make them
	// look like images the node pulled itself (localOnly).
	prov, provErr := hostImageProvenance()
	remembered := readLocalOnlyRefs()
	var local []string
	images = slices.DeleteFunc(images, func(img string) bool {
		if !localOnly(img, prov, provErr == nil, remembered) {
			return false
		}
		local = append(local, img)
		return true
	})
	if len(images) == 0 && len(local) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("# Images the last successful boot ran; the next `agentlab up` pulls\n" +
		"# them on the host and side-loads them into the fresh node. Regenerated\n" +
		"# after every boot — safe to delete.\n")
	for _, img := range images {
		b.WriteString(img + "\n")
	}
	if len(local) > 0 {
		b.WriteString("# Built or tagged on this host and side-loaded: no registry serves them,\n" +
			"# so they stay out of the pull set even once the local copy is pruned.\n")
		for _, img := range local {
			b.WriteString(localOnlyMarker + img + "\n")
		}
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return
	}
	_ = os.WriteFile(preloadImagesFile, []byte(b.String()), 0o600)
}

// localOnlyMarker prefixes the manifest lines that remember local-only refs —
// comments to readPreloadManifest, so they never enter the pull set.
const localOnlyMarker = "# local-only: "

// localOnly reports whether a node image is a local build the next boot must
// not try to pull. The host cache's current knowledge decides whenever it has
// any (registryBacked: a real pull of the same repo:tag makes it pullable
// again); for a ref the host no longer knows — the local copy was pruned — the
// manifest's memory decides.
func localOnly(ref string, prov map[string]bool, provOK bool, remembered []string) bool {
	if provOK {
		if _, known := prov[shortRef(ref)]; known {
			return !registryBacked(ref, prov)
		}
	}
	return slices.Contains(remembered, ref)
}

// readLocalOnlyRefs returns the refs the manifest remembers as local builds;
// no manifest, no memory.
func readLocalOnlyRefs() []string {
	raw, err := os.ReadFile(filepath.FromSlash(preloadImagesFile))
	if err != nil {
		return nil
	}
	var refs []string
	for line := range strings.Lines(string(raw)) {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(line), localOnlyMarker); ok && ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
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

// hostImageProvenance maps every repo:tag in the host docker cache to whether
// a registry digest backs it: `docker images --digests` prints the digest of
// the manifest a pulled image came from, and <none> for an image built or
// tagged here. A pulled image re-tagged into ANOTHER repository shows <none>
// too (the digest belongs to the repository it was pulled as); re-tagged within
// the same repository it keeps the digest, and so still reads as pulled. Refs
// are spelled the way docker prints them (shortRef). Podman prints a digest
// for local builds too, but spells them `localhost/<name>`: that name is
// what marks them local there.
func hostImageProvenance() (map[string]bool, error) {
	out, err := outputQuiet("docker", "images", "--digests", "--format", "{{.Repository}}:{{.Tag}}\t{{.Digest}}")
	if err != nil {
		return nil, err
	}
	return parseImageProvenance(out), nil
}

// parseImageProvenance reads hostImageProvenance's "repo:tag<TAB>digest"
// lines; untagged rows are skipped, and a ref listed more than once is backed
// if any of its rows is.
func parseImageProvenance(out string) map[string]bool {
	prov := map[string]bool{}
	for line := range strings.Lines(out) {
		ref, digest, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || strings.HasSuffix(ref, ":<none>") {
			continue
		}
		digest = strings.TrimSpace(digest)
		ref = shortRef(ref)
		backed := digest != "" && digest != "<none>" && !strings.HasPrefix(ref, "localhost/")
		prov[ref] = prov[ref] || backed
	}
	return prov
}

// shortRef spells a fully qualified ref (as crictl lists it) the way docker
// prints it: the docker.io registry and its library/ namespace are implicit.
func shortRef(ref string) string {
	ref = strings.TrimPrefix(ref, "docker.io/library/")
	return strings.TrimPrefix(ref, "docker.io/")
}

// registryBacked reports whether a node image can be pulled again on the next
// boot. A ref the host cache does not know was pulled by the node itself —
// pullable by construction; one the host knows with a registry digest was
// pulled on the host. One the host knows WITHOUT a digest was built or tagged
// here and side-loaded (a dev-image swap): no registry has it —
// `docker.io/library/backstage-dev:<tag>` is what docker makes of a bare
// `backstage-dev:<tag>` — and recording it means every boot after the local
// copy is pruned asks Docker Hub for it and dockerd logs "denied: requested
// access to the resource is denied" per ref.
func registryBacked(ref string, prov map[string]bool) bool {
	backed, known := prov[shortRef(ref)]
	return !known || backed
}
