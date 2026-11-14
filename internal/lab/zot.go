package lab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"oras.land/oras-go/v2/registry/remote"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab's zot containers (docs/registries.md): the lab registry of each
// lab (devimages.go) and the registry mirror every lab on the machine shares
// (mirror.go). Both run the zot release this run resolves, with a
// configuration agentlab writes and mounts read-only, their storage in a
// named volume, and both are recreated — the volume kept — when the release
// or the configuration differs from the container's.

// zotContainerPort is the port zot listens on inside both containers: the
// endpoint the node's containerd and atelet dial on the kind network.
const zotContainerPort = 5000

// zotStorageDir is zot's storage in both containers, a named volume of the
// container's name, so a recreated container keeps what it holds.
const zotStorageDir = "/var/lib/registry"

// zotConfigFile is where zot's image reads its configuration.
const zotConfigFile = "/etc/zot/config.json"

// zotConfigLabel is the docker label carrying the sha256 of the
// configuration a container was created with: the configuration is part of
// what the container is, and a changed one recreates it like a new release.
const zotConfigLabel = "agentlab.giantswarm.io/zot-config"

// zotMovingSuffix names an old container while its storage moves into the
// new one's volume (zotContainer.move); one left over is an interrupted move.
const zotMovingSuffix = "-moving"

// zotListTimeout bounds listing gsoci's zot tags: one paged list request.
const zotListTimeout = 30 * time.Second

// latestZotRelease is the zot both containers follow, resolved once per run:
// the highest release on gsoci at or above the floor (config.PickZotRelease).
// A seam the tests replace.
var latestZotRelease = sync.OnceValues(func() (string, error) {
	tags, err := listZotTags()
	if err != nil {
		return "", err
	}
	tag, ok := config.PickZotRelease(tags)
	if !ok {
		return "", fmt.Errorf("no zot %d release at or above v%s among the %d tags of %s", config.ZotMajor, config.ZotFloor, len(tags), config.ZotRepository)
	}
	return tag, nil
})

// listZotTags lists zot's tags on gsoci. Directly, not through Helm's
// registry client: that one keeps only tags its strict semver parser
// accepts, and every zot tag carries a v.
var listZotTags = func() ([]string, error) {
	repo, err := remote.NewRepository(config.ZotRepository)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), zotListTimeout)
	defer cancel()
	var tags []string
	if err := repo.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("listing the tags of %s: %w", config.ZotRepository, err)
	}
	return tags, nil
}

// cachedZotTags lists the zot tags in the host docker cache: the fallback
// for a container that has to be created while gsoci cannot be listed. A
// seam the tests replace.
var cachedZotTags = func() ([]string, error) {
	out, err := outputQuiet(dockerBin, "image", "ls", "--format", "{{.Tag}}", config.ZotRepository)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// pickZotImage is the zot image a container runs this run, and what the run
// says about the pick ("" when it follows the newest release): pin is the
// lab's zotVersion ("" for the mirror and a lab that follows releases),
// running the image of the container in place ("" when there is none).
// When gsoci cannot be listed a container in place keeps its image, and a
// new one takes the newest release at or above the floor in the host docker
// cache; with none the run fails as the pull would.
func pickZotImage(pin, running string) (image, why string, err error) {
	if pin != "" {
		if err := config.CheckZotVersion(pin); err != nil {
			return "", "", fmt.Errorf("zotVersion %s: %w: `agentlab configure --zot-version v%s` (or newer), or `--zot-version \"\"` to follow zot's releases", pin, err, config.ZotFloor)
		}
		return config.ZotImage(pin), "", nil
	}
	tag, listErr := latestZotRelease()
	if listErr == nil {
		return config.ZotImage(tag), "", nil
	}
	if running != "" {
		return running, fmt.Sprintf("zot's releases could not be resolved (%v), so it keeps %s", listErr, running), nil
	}
	cached, err := cachedZotTags()
	if err == nil {
		if tag, ok := config.PickZotRelease(cached); ok {
			return config.ZotImage(tag), fmt.Sprintf("zot's releases could not be resolved (%v); %s is the newest in the host docker cache", listErr, tag), nil
		}
	}
	return "", "", fmt.Errorf("zot's releases could not be resolved (%w), and the host docker cache holds no zot %d release at or above v%s", listErr, config.ZotMajor, config.ZotFloor)
}

// zotTag is the tag of a zot image ref, the release a status line names.
func zotTag(image string) string {
	if _, tag, ok := strings.Cut(strings.TrimPrefix(image, config.ZotRepository), ":"); ok {
		return tag
	}
	return image
}

// zotContainer is one zot container the lab keeps.
type zotContainer struct {
	// name is the container's, and its storage volume's.
	name string
	// what names it in messages: "the lab registry", "the registry mirror".
	what string
	// configPath is the host path of its configuration, mounted read-only
	// at zotConfigFile; config is what this run writes there.
	configPath string
	config     []byte
	// flags are its own `docker run` flags (the lab registry's published
	// port).
	flags []string
	// pin is the lab's zotVersion; "" follows zot's releases.
	pin string
}

// zotState is what the lab reads off a zot container in place.
type zotState struct {
	// state is its docker state.
	state string
	// image is the image ref it was created from.
	image string
	// config is its zotConfigLabel, "" for a container agentlab configured
	// before it wrote zot's configuration.
	config string
	// volume is the named volume at zotStorageDir, "" for a container whose
	// storage is in its own layer.
	volume string
}

// inspectZot reads a zot container in place; false when there is none.
func inspectZot(name string) (zotState, bool) {
	format := "{{.State.Status}}\t{{.Config.Image}}\t{{index .Config.Labels \"" + zotConfigLabel + "\"}}\t" +
		"{{range .Mounts}}{{if eq .Destination \"" + zotStorageDir + "\"}}{{.Name}}{{end}}{{end}}"
	out, err := outputQuiet(dockerBin, "inspect", "-f", format, name)
	if err != nil {
		return zotState{}, false
	}
	return parseZotState(out), true
}

// parseZotState reads inspectZot's tab-separated line.
func parseZotState(out string) zotState {
	f := strings.Split(strings.TrimRight(out, "\r\n"), "\t")
	for len(f) < 4 {
		f = append(f, "")
	}
	return zotState{state: f[0], image: f[1], config: f[2], volume: f[3]}
}

// configDigest is the label value of a configuration.
func configDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// zotChanges words why a container in place is recreated: its release, its
// configuration or its volume differ from this run's; none when it is
// current.
func zotChanges(have zotState, image, digest, volume string) []string {
	var why []string
	if have.image != image {
		why = append(why, fmt.Sprintf("zot %s → %s", zotTag(have.image), zotTag(image)))
	}
	if have.config != digest {
		why = append(why, "agentlab's configuration")
	}
	if have.volume != volume {
		why = append(why, "its storage into the volume "+volume)
	}
	return why
}

// ensure has the container running the zot this run resolves with the
// configuration this run writes, creating, moving, recreating, starting or
// unpausing it as needed, and returns the image it runs and whether it was
// created or recreated (its API then still comes up). A container whose
// storage is in its own layer moves it into the volume once (move).
func (z zotContainer) ensure() (image string, fresh bool, err error) {
	if err := writeIfChanged(z.configPath, z.config); err != nil {
		return "", false, fmt.Errorf("writing the configuration of %s %s: %w", z.what, z.name, err)
	}
	z.recoverMove()
	have, exists := inspectZot(z.name)
	image, why, err := pickZotImage(z.pin, have.image)
	if err != nil {
		return "", false, fmt.Errorf("%s %s: %w", z.what, z.name, err)
	}
	if why != "" {
		note("%s %s: %s", z.what, z.name, why)
	}
	digest := configDigest(z.config)
	release := "zot " + zotTag(image)
	if z.pin != "" {
		release += ", held at zotVersion"
	}
	switch {
	case !exists:
		if err := z.create(image, digest); err != nil {
			return "", false, err
		}
		note("created %s %s (%s, on the %s network)", z.what, z.name, release, kindDockerNetwork)
		return image, true, nil
	case have.volume == "":
		if err := z.move(have, image, release, digest); err != nil {
			return "", false, err
		}
		return image, true, nil
	}
	if changes := zotChanges(have, image, digest, z.name); len(changes) > 0 {
		// The image first: a pull that fails leaves the container in place.
		if err := z.pullZot(image); err != nil {
			return "", false, err
		}
		if err := runQuiet(dockerBin, "rm", "-f", z.name); err != nil {
			return "", false, fmt.Errorf("removing %s %s to recreate it: %w", z.what, z.name, err)
		}
		if err := z.create(image, digest); err != nil {
			return "", false, err
		}
		note("recreated %s %s on %s (%s), its volume kept", z.what, z.name, release, strings.Join(changes, ", "))
		return image, true, nil
	}
	switch have.state {
	case stateRunning:
	case statePaused:
		if err := runQuiet(dockerBin, verbUnpause, z.name); err != nil {
			return "", false, fmt.Errorf("resuming %s %s: %w", z.what, z.name, err)
		}
	default:
		if err := runQuiet(dockerBin, verbStart, z.name); err != nil {
			return "", false, fmt.Errorf("starting %s %s (%s): %w", z.what, z.name, have.state, err)
		}
		note("started %s %s (was %s)", z.what, z.name, have.state)
		return image, true, nil
	}
	return image, false, nil
}

// containerArgs is the `docker run` (or `docker create`) of the container on
// the kind network: its storage volume, its configuration read-only, the
// configuration's digest as a label, restarted with the docker daemon.
func (z zotContainer) containerArgs(verb, image, digest string) []string {
	flags := []string{"--restart=always", "--label", zotConfigLabel + "=" + digest,
		"-v", z.name + ":" + zotStorageDir,
		"-v", z.configPath + ":" + zotConfigFile + ":ro"}
	if verb == "run" {
		flags = append([]string{"-d"}, flags...)
	}
	flags = append(flags, z.flags...)
	return append(dockerContainer(verb, z.name, kindDockerNetwork, flags...), image)
}

// pullZot has the image in the host docker cache.
func (z zotContainer) pullZot(image string) error {
	if len(hostPullImages([]string{image})) == 0 {
		return fmt.Errorf("%s %s: pulling %s failed (`docker pull %s`)", z.what, z.name, image, image)
	}
	return nil
}

// create runs a new container. Another lab's run that created the shared
// mirror a moment earlier is as good as this one's: the name is taken by a
// zot container, which the next run brings to this run's release and
// configuration if it differs.
func (z zotContainer) create(image, digest string) error {
	if err := z.pullZot(image); err != nil {
		return err
	}
	if _, err := outputQuiet(dockerBin, z.containerArgs("run", image, digest)...); err != nil {
		if have, taken := inspectZot(z.name); taken && have.state == stateRunning {
			return nil
		}
		// A container docker made but could not start (a published port
		// another process holds) goes, so the next run starts clean.
		_, _ = outputQuiet(dockerBin, "rm", "-f", z.name)
		return fmt.Errorf("creating %s %s: %w", z.what, z.name, err)
	}
	return nil
}

// move replaces a container whose storage is in its own layer — the lab
// registry as agentlab created it before it mounted a volume — with one on
// the volume, its storage copied over first: what was pushed by hand stays,
// and a large build is not pushed again. The old container is stopped (it
// holds the published port) and renamed aside, the new one created stopped,
// the storage streamed from the one into the other (`docker cp`, which
// needs no shell in either image), the new one started and the old one
// removed. A failure puts the old container back as it was.
func (z zotContainer) move(have zotState, image, release, digest string) error {
	if err := z.pullZot(image); err != nil {
		return err
	}
	old := z.name + zotMovingSuffix
	if have.state == statePaused {
		_ = runQuiet(dockerBin, verbUnpause, z.name)
	}
	if have.state == stateRunning || have.state == statePaused {
		if err := runQuiet(dockerBin, "stop", z.name); err != nil {
			return fmt.Errorf("stopping %s %s to move its storage: %w", z.what, z.name, err)
		}
	}
	if err := runQuiet(dockerBin, "rename", z.name, old); err != nil {
		return fmt.Errorf("renaming %s %s aside to move its storage: %w", z.what, z.name, err)
	}
	restore := func(cause error) error {
		_, _ = outputQuiet(dockerBin, "rm", "-f", z.name)
		_, _ = outputQuiet(dockerBin, "volume", "rm", z.name)
		if err := runQuiet(dockerBin, "rename", old, z.name); err != nil {
			return fmt.Errorf("%w; putting the old container back failed too (%v): `docker rename %s %s`", cause, err, old, z.name)
		}
		if have.state == stateRunning || have.state == statePaused {
			_ = runQuiet(dockerBin, verbStart, z.name)
		}
		return fmt.Errorf("%w; %s %s is back as it was", cause, z.what, z.name)
	}
	if err := runQuiet(dockerBin, z.containerArgs("create", image, digest)...); err != nil {
		return restore(fmt.Errorf("creating the new %s %s: %w", z.what, z.name, err))
	}
	copied, err := copyZotStorage(old, z.name)
	if err != nil {
		return restore(fmt.Errorf("copying the storage of %s %s into its volume: %w", z.what, z.name, err))
	}
	if err := runQuiet(dockerBin, verbStart, z.name); err != nil {
		return restore(fmt.Errorf("starting the new %s %s: %w", z.what, z.name, err))
	}
	if err := runQuiet(dockerBin, "rm", "-f", old); err != nil {
		note("removing the old %s %s failed (%v); `docker rm -f %s` removes it", z.what, old, err, old)
	}
	what := "its storage copied into"
	if !copied {
		what = "nothing stored to copy into"
	}
	note("recreated %s %s on %s with agentlab's configuration, %s the volume %s", z.what, z.name, release, what, z.name)
	return nil
}

// recoverMove puts back a container an interrupted move left renamed aside:
// the half-made new container and its volume go, the old one takes its name
// again, and this run's ensure moves it from the start. Its last step
// removes the old container, so the old one still holds everything.
func (z zotContainer) recoverMove() {
	old := z.name + zotMovingSuffix
	if _, err := nodeState(old); err != nil {
		return
	}
	note("%s %s: an earlier move of its storage stopped half-way; starting it again from %s", z.what, z.name, old)
	_, _ = outputQuiet(dockerBin, "rm", "-f", z.name)
	_, _ = outputQuiet(dockerBin, "volume", "rm", z.name)
	_ = runQuiet(dockerBin, "rename", old, z.name)
}

// copyZotStorage streams zotStorageDir from one container into the other —
// `docker cp` out of the one as a tar on stdout, into the other from stdin
// — and reports whether there was anything to copy: a container that never
// started has no storage directory.
func copyZotStorage(from, to string) (bool, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return false, err
	}
	var srcErr, dstErr bytes.Buffer
	src := command(dockerBin, "cp", from+":"+zotStorageDir, "-")
	src.Stdout, src.Stderr = w, &srcErr
	dst := command(dockerBin, "cp", "-", to+":"+path.Dir(zotStorageDir))
	dst.Stdin, dst.Stderr = r, &dstErr
	if err := src.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return false, err
	}
	if err := dst.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		_ = src.Wait()
		return false, err
	}
	// The children hold their own ends: closed here, a reader that dies
	// ends the writer with a broken pipe and a writer that ends gives the
	// reader its EOF.
	_ = r.Close()
	_ = w.Close()
	srcWait, dstWait := src.Wait(), dst.Wait()
	if srcWait != nil {
		if nothingToCopy(srcErr.String()) {
			return false, nil
		}
		return false, cmdError(dockerBin, src.Args[1:], srcWait, srcErr.Bytes())
	}
	if dstWait != nil {
		return false, cmdError(dockerBin, dst.Args[1:], dstWait, dstErr.Bytes())
	}
	return true, nil
}

// nothingToCopy reads a failed `docker cp` out of a container as a path the
// container does not have, in docker's words and podman's.
func nothingToCopy(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "could not find") || strings.Contains(s, "could not be found") || strings.Contains(s, "no such file")
}

// writeIfChanged writes a file, its directory created, unless it already
// holds those bytes.
func writeIfChanged(file string, raw []byte) error {
	if have, err := os.ReadFile(file); err == nil && bytes.Equal(have, raw) { // #nosec G304 -- the lab's own zot configuration paths
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}
	return os.WriteFile(file, raw, 0o644) // #nosec G306 -- zot's configuration, no secret; zot reads it as root in its container
}

// removeZot removes a zot container and its volume; nothing to do when
// there is neither. Best-effort, for `agentlab down`.
func removeZot(name, what string) {
	if _, err := nodeState(name); err == nil {
		if err := runQuiet(dockerBin, "rm", "-f", name); err != nil {
			note("removing %s %s failed (%v); `docker rm -f %s && docker volume rm %s` removes it", what, name, err, name, name)
			return
		}
		note("removed %s %s", what, name)
	}
	if _, err := outputQuiet(dockerBin, "volume", "inspect", name); err != nil {
		return
	}
	if err := runQuiet(dockerBin, "volume", "rm", name); err != nil {
		note("removing the volume %s failed (%v); `docker volume rm %s` removes it", name, err, name)
	}
}

// zotVolumeSize is the size docker reports for a container's storage
// volume ("1.2GB"), "" when it reports none. `docker system df -v` sizes
// every volume on the host, seconds on a busy one: status only.
func zotVolumeSize(volume string) string {
	out, err := outputQuiet(dockerBin, "system", "df", "-v", "--format", "{{range .Volumes}}{{.Name}}\t{{.Size}}\n{{end}}")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(out) {
		if name, size, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && name == volume {
			return size
		}
	}
	return ""
}
