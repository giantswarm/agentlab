package lab

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// Fixture refs, hoisted so the linter's constant check stays quiet.
const (
	refPostgres     = "docker.io/library/postgres:18.3-alpine"
	refBackstageDev = "docker.io/library/backstage-dev:multi-backend-022f5b7e"
	refMusterDev    = "gsoci.azurecr.io/giantswarm/muster:dev"
	refGolangADK    = "gsoci.azurecr.io/giantswarm/golang-adk:0.10.0"
	refSocat        = "docker.io/alpine/socat:1.8.1.3"
)

func TestScrapeImages(t *testing.T) {
	rendered := `
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - image: gsoci.azurecr.io/giantswarm/muster:5.7.2
        - image: "gsoci.azurecr.io/giantswarm/mcp-kubernetes:1.0.9"
      initContainers:
        - image: 'docker.io/library/postgres:18.3-alpine'
---
kind: ConfigMap
data:
  # a bare word under an image key is config, not a pullable ref
  image: muster
  settings: |
    image: not-yaml-context:but-tagged
---
kind: Pod
spec:
  containers:
    - image: gsoci.azurecr.io/giantswarm/muster:5.7.2
    - image: gsoci.azurecr.io/giantswarm/valkey@sha256:abcdef0123456789
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayParameters
spec:
  # the data-plane image, structured — the controller composes the ref
  image:
    registry: gsoci.azurecr.io
    repository: giantswarm/agentgateway
    tag: v1.4.1
`
	got := scrapeImages(rendered)
	want := []string{
		refPostgres,
		"gsoci.azurecr.io/giantswarm/agentgateway:v1.4.1",
		"gsoci.azurecr.io/giantswarm/mcp-kubernetes:1.0.9",
		"gsoci.azurecr.io/giantswarm/muster:5.7.2",
		"gsoci.azurecr.io/giantswarm/valkey@sha256:abcdef0123456789",
		"not-yaml-context:but-tagged",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("scrapeImages:\n got  %v\n want %v", got, want)
	}
}

// TestRegistryBacked: the snapshot keeps what the next boot can pull — refs
// the host cache does not know (the node pulled them itself) and refs it
// knows with a registry digest — and drops what it knows without one: images
// built or tagged here and side-loaded, which docker spells
// `docker.io/library/<name>` in the node although no registry has them.
func TestRegistryBacked(t *testing.T) {
	prov := parseImageProvenance("backstage-dev:multi-backend-022f5b7e\t<none>\n" +
		"postgres:18.3-alpine\tsha256:54451ecb8ab38c24c3ec123f2fd501303a3a1856a5c66e98cecf2460d5e1e9d7\n" +
		"alpine/socat:1.8.1.3\tsha256:5f275aa1b6e9889c851f61097142ee050fc6ac4615b4ea64ac1f2b0e81ff8d7f\n" +
		"gsoci.azurecr.io/giantswarm/muster:dev\t<none>\n" +
		"gsoci.azurecr.io/giantswarm/muster:5.10.2\tsha256:b97b80cd922c4aa2b6aa61e3fca50194d211b1b5a90b5931ce1eef5ff74d35a5\n" +
		"<none>:<none>\t<none>\n")
	for ref, want := range map[string]bool{
		refBackstageDev: false, // built here
		refPostgres:     true,  // pulled from Docker Hub
		refSocat:        true,  // pulled, non-library namespace
		refMusterDev:    false, // a dev build tagged into a registry repo
		"gsoci.azurecr.io/giantswarm/muster:5.10.2": true,
		refGolangADK: true, // unknown to the host: the node pulled it
	} {
		if got := registryBacked(ref, prov); got != want {
			t.Errorf("registryBacked(%q) = %v, want %v", ref, got, want)
		}
	}
	if _, dangling := prov["<none>:<none>"]; dangling {
		t.Error("untagged rows must not enter the provenance map")
	}
	for ref, want := range map[string]string{
		refPostgres:                  "postgres:18.3-alpine",
		refSocat:                     "alpine/socat:1.8.1.3",
		"ghcr.io/dexidp/dex:v2.45.1": "ghcr.io/dexidp/dex:v2.45.1",
	} {
		if got := shortRef(ref); got != want {
			t.Errorf("shortRef(%q) = %q, want %q", ref, got, want)
		}
	}
}

// TestSnapshotPreloadImagesSkipsLocalBuilds runs the snapshot against a
// stand-in docker. The node's crictl list holds infra images, a pulled
// official image, an image the node pulled itself, a dev image built on the
// host and a dev build tagged into a registry repo: the pull set gets the two
// pullable refs, the two local ones are remembered as such. Then the host
// prunes the dev image and pulls that repo:tag for real: the memory
// keeps the pruned one out, the real pull brings the other in.
func TestSnapshotPreloadImagesSkipsLocalBuilds(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	hostImages := filepath.Join(dir, "host-images")
	t.Setenv("FAKE_HOST_IMAGES", hostImages)
	installFakeTool(t, dir, "docker", `
case "$1" in
exec) cat <<'JSON'
{"images":[
 {"repoTags":["docker.io/library/backstage-dev:multi-backend-022f5b7e"]},
 {"repoTags":["docker.io/library/postgres:18.3-alpine"]},
 {"repoTags":["gsoci.azurecr.io/giantswarm/golang-adk:0.10.0"]},
 {"repoTags":["gsoci.azurecr.io/giantswarm/muster:dev"]},
 {"repoTags":["registry.k8s.io/etcd:3.6.4-0","docker.io/kindest/kindnetd:v20250512-df8de77b"]}
]}
JSON
;;
images) cat "$FAKE_HOST_IMAGES" ;;
esac`)
	writeHost := func(rows string) {
		if err := os.WriteFile(hostImages, []byte(rows), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(phase string, wantPull, wantLocal []string) {
		t.Helper()
		snapshotPreloadImages(config.Default())
		if got := readPreloadManifest(); !slices.Equal(got, wantPull) {
			t.Errorf("%s: pull set\n got  %v\n want %v", phase, got, wantPull)
		}
		if got := readLocalOnlyRefs(); !slices.Equal(got, wantLocal) {
			t.Errorf("%s: local-only memory\n got  %v\n want %v", phase, got, wantLocal)
		}
	}

	writeHost("backstage-dev:multi-backend-022f5b7e\t<none>\n" +
		"postgres:18.3-alpine\tsha256:54451e\n" +
		"gsoci.azurecr.io/giantswarm/muster:dev\t<none>\n")
	check("host knows both local images",
		[]string{refPostgres, refGolangADK},
		[]string{refBackstageDev, refMusterDev})

	writeHost("postgres:18.3-alpine\tsha256:54451e\n" +
		"gsoci.azurecr.io/giantswarm/muster:dev\tsha256:0ff1ce\n")
	check("dev image pruned on the host, muster:dev pulled for real",
		[]string{refPostgres, refGolangADK, refMusterDev},
		[]string{refBackstageDev})
}

// Podman gives local builds a digest like any pull but spells them
// `localhost/<name>`; that name marks them local, and podman's fully
// qualified rows key the map the way docker spells them.
func TestParseImageProvenancePodman(t *testing.T) {
	prov := parseImageProvenance("localhost/backstage-dev:t1\tsha256:0537\n" +
		"docker.io/library/postgres:18.3-alpine\tsha256:5445\n" +
		"docker.io/alpine/socat:1.8.1.3\tsha256:5f27\n")
	for ref, want := range map[string]bool{
		"localhost/backstage-dev:t1":             false,
		"docker.io/library/postgres:18.3-alpine": true,
		refSocat:                                 true,
	} {
		if got := registryBacked(ref, prov); got != want {
			t.Errorf("registryBacked(%q) = %v, want %v", ref, got, want)
		}
	}
	if _, known := prov["postgres:18.3-alpine"]; !known {
		t.Error("podman's docker.io/library/ rows must key the map as docker spells them")
	}
}

// withSavePlatform pins whether `docker save --platform` counts as available
// (Docker 28+) for the test's duration.
func withSavePlatform(t *testing.T, has bool) {
	t.Helper()
	prev := dockerSaveHasPlatform
	dockerSaveHasPlatform = func() bool { return has }
	t.Cleanup(func() { dockerSaveHasPlatform = prev })
}

// The two side-loads that still go through kind's own `kind load docker-image`.
// Podman: kind's multi-image `docker save` is single-image there, so the batch
// would land one image under every tag — one call per image. Docker older
// than 28: no `docker save --platform`, so kind's own batched save it is (right
// under the classic graph driver such an engine runs).
func TestKindLoadImagesOneCallPerImageUnderPodman(t *testing.T) {
	images := []string{refPostgres, refGolangADK, refMusterDev}
	for name, tc := range map[string]struct {
		podman    bool
		wantCalls []string
	}{
		"docker without save --platform batches": {false, []string{
			"kind load docker-image --name agentlab " + refPostgres + " " + refGolangADK + " " + refMusterDev,
		}},
		"podman loads one at a time": {true, []string{
			"kind load docker-image --name agentlab " + refPostgres,
			"kind load docker-image --name agentlab " + refGolangADK,
			"kind load docker-image --name agentlab " + refMusterDev,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			calls := installFakeTool(t, dir, "kind", "exit 0")
			dockerCalls := installFakeTool(t, dir, "docker", "exit 0")
			withPodman(t, tc.podman)
			withSavePlatform(t, false)
			cfg := config.Default()
			loaded, err := kindLoadImages(cfg, images)
			if err != nil {
				t.Fatalf("kindLoadImages: %v", err)
			}
			if loaded != len(images) {
				t.Errorf("loaded %d images, want %d", loaded, len(images))
			}
			got := strings.Split(strings.TrimSpace(readCalls(t, calls)), "\n")
			if !slices.Equal(got, tc.wantCalls) {
				t.Errorf("calls\n got  %v\n want %v", got, tc.wantCalls)
			}
			if d := readCalls(t, dockerCalls); d != "" {
				t.Errorf("kind's own load must not shell out to docker itself, got:\n%s", d)
			}
		})
	}
}

// tarPathRe matches the temporary archive's path in a stand-in's call log.
var tarPathRe = regexp.MustCompile(`\S+\.tar`)

// kindLoadArchiveCall is the one kind call the docker side-load makes, as the
// stand-in logs it with the archive path normalised.
const kindLoadArchiveCall = "kind load image-archive --name agentlab <tar>"

// callLines splits a stand-in's log into lines with the archive path
// normalised to <tar>, so the argv can be compared exactly.
func callLines(t *testing.T, path string) []string {
	t.Helper()
	raw := strings.TrimSpace(readCalls(t, path))
	if raw == "" {
		return nil
	}
	return strings.Split(tarPathRe.ReplaceAllString(raw, "<tar>"), "\n")
}

// installSideloadFakes puts a docker and a kind on PATH for the docker
// side-load: docker answers `image inspect` with a platform per ref (muster
// amd64, everything else arm64) and creates the archive `save` is asked for;
// kind's `load image-archive` insists the archive exists when it runs. The
// archives land in dir (TMPDIR), where the test can see them gone again.
func installSideloadFakes(t *testing.T, dir, saveBody string) (dockerCalls, kindCalls string) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	dockerCalls = installFakeTool(t, dir, "docker", `case "$1 $2" in
"image inspect") case "$5" in
  *muster*) echo linux/amd64 ;;
  *socat*) echo "Error response from daemon: No such image: $5" >&2; exit 1 ;;
  *) echo linux/arm64/v8 ;;
  esac ;;
"save --platform") `+saveBody+` ;;
*) echo "unexpected docker $*" >&2; exit 2 ;;
esac`)
	kindCalls = installFakeTool(t, dir, "kind", `[ "$1 $2" = "load image-archive" ] || { echo "unexpected kind $*" >&2; exit 2; }
[ -f "$5" ] || { echo "archive $5 is not there" >&2; exit 1; }`)
	withPodman(t, false)
	withSavePlatform(t, true)
	return dockerCalls, kindCalls
}

// leftoverArchives lists the temporary archives still in dir.
func leftoverArchives(t *testing.T, dir string) []string {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, "agentlab-images-*.tar"))
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// Under docker the side-load is `docker save --platform` + `kind load
// image-archive`, one archive per platform the host holds the images in,
// never kind's own `kind load docker-image`: its plain `docker save` breaks
// under the containerd image store (kind#3795). Every ref is inspected for
// its platform, the archives are written where docker was told to and are
// gone afterwards.
func TestDockerLoadImagesSavesOneArchivePerPlatform(t *testing.T) {
	dir := t.TempDir()
	dockerCalls, kindCalls := installSideloadFakes(t, dir, `: > "$5"`)
	images := []string{refPostgres, refGolangADK, refMusterDev}
	loaded, err := kindLoadImages(config.Default(), images)
	if err != nil {
		t.Fatalf("kindLoadImages: %v", err)
	}
	if loaded != len(images) {
		t.Errorf("loaded %d images, want %d", loaded, len(images))
	}
	var inspects, saves []string
	for _, c := range callLines(t, dockerCalls) {
		if strings.HasPrefix(c, "docker image inspect ") {
			inspects = append(inspects, c)
		} else {
			saves = append(saves, c)
		}
	}
	slices.Sort(inspects)
	wantInspects := []string{
		"docker image inspect -f " + imagePlatformFormat + " " + refPostgres,
		"docker image inspect -f " + imagePlatformFormat + " " + refGolangADK,
		"docker image inspect -f " + imagePlatformFormat + " " + refMusterDev,
	}
	slices.Sort(wantInspects)
	if !slices.Equal(inspects, wantInspects) {
		t.Errorf("inspects\n got  %v\n want %v", inspects, wantInspects)
	}
	wantSaves := []string{
		"docker save --platform linux/amd64 -o <tar> " + refMusterDev,
		"docker save --platform linux/arm64/v8 -o <tar> " + refPostgres + " " + refGolangADK,
	}
	if !slices.Equal(saves, wantSaves) {
		t.Errorf("saves\n got  %v\n want %v", saves, wantSaves)
	}
	wantKind := []string{kindLoadArchiveCall, kindLoadArchiveCall}
	if got := callLines(t, kindCalls); !slices.Equal(got, wantKind) {
		t.Errorf("kind calls\n got  %v\n want %v", got, wantKind)
	}
	if left := leftoverArchives(t, dir); len(left) > 0 {
		t.Errorf("temporary archives left behind: %v", left)
	}
}

// A platform group whose save fails, and a ref the host cannot inspect, must
// not stop the other groups: the count says how many landed, the error names
// what did not, and no archive is left behind either way.
func TestDockerLoadImagesPartialFailure(t *testing.T) {
	dir := t.TempDir()
	dockerCalls, kindCalls := installSideloadFakes(t, dir, `case "$3" in
  linux/amd64) echo "Error response from daemon: no suitable export target found" >&2; exit 1 ;;
  *) : > "$5" ;;
  esac`)
	loaded, err := kindLoadImages(config.Default(), []string{refPostgres, refSocat, refGolangADK, refMusterDev})
	if err == nil {
		t.Fatal("a failed save and an uninspectable ref must surface as an error")
	}
	for _, want := range []string{"No such image", "no suitable export target"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must carry %q, got:\n%v", want, err)
		}
	}
	if loaded != 2 {
		t.Errorf("loaded = %d, want 2 (the arm64 group must land despite the rest)", loaded)
	}
	var saves []string
	for _, c := range callLines(t, dockerCalls) {
		if strings.HasPrefix(c, "docker save ") {
			saves = append(saves, c)
		}
	}
	wantSaves := []string{
		"docker save --platform linux/amd64 -o <tar> " + refMusterDev,
		"docker save --platform linux/arm64/v8 -o <tar> " + refPostgres + " " + refGolangADK,
	}
	if !slices.Equal(saves, wantSaves) {
		t.Errorf("saves\n got  %v\n want %v", saves, wantSaves)
	}
	if got, want := callLines(t, kindCalls), []string{kindLoadArchiveCall}; !slices.Equal(got, want) {
		t.Errorf("kind calls\n got  %v\n want %v (only the group whose save succeeded)", got, want)
	}
	if left := leftoverArchives(t, dir); len(left) > 0 {
		t.Errorf("temporary archives left behind: %v", left)
	}
}

// saveHasPlatform reads both API versions `docker version` prints: `docker
// save --platform` needs 1.48 (Docker 28) on the client and the daemon.
func TestSaveHasPlatform(t *testing.T) {
	for out, want := range map[string]bool{
		"1.53 1.53\n": true,
		"1.48 1.48":   true,
		"2.0 2.1":     true,
		"1.47 1.47":   false,
		"1.53 1.47":   false,
		"1.47 1.53":   false,
		"1.48":        false,
		"":            false,
		"x.y 1.53":    false,
	} {
		if got := saveHasPlatform(out); got != want {
			t.Errorf("saveHasPlatform(%q) = %v, want %v", out, got, want)
		}
	}
}

// Under podman a failed image must not stop the rest, and the count must say
// how many landed so the caller can report a partial load as one.
func TestKindLoadImagesPartialFailureUnderPodman(t *testing.T) {
	dir := t.TempDir()
	installFakeTool(t, dir, "kind", `case "$5" in
`+refGolangADK+`) echo "no such image" >&2; exit 1 ;;
*) exit 0 ;;
esac`)
	withPodman(t, true)
	loaded, err := kindLoadImages(config.Default(), []string{refPostgres, refGolangADK, refMusterDev})
	if err == nil {
		t.Fatal("a failed image must surface as an error")
	}
	if loaded != 2 {
		t.Errorf("loaded = %d, want 2 (the failure must not stop the rest)", loaded)
	}
}
