package lab

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

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
---
# the images pods get at run time from an operator: the WorkerPool's worker,
# the CNPG Cluster's operand and its ImageVolume extension
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
spec:
  workerImage: ghcr.io/giantswarm/substrate/ateom-gvisor:0.0.27-gs.5
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
spec:
  imageName: "gsoci.azurecr.io/giantswarm/postgresql-cnpg:18.3"
  postgresql:
    extensions:
      - name: pgvector
        image:
          reference: gsoci.azurecr.io/giantswarm/pgvector:0.8.2-18-bookworm
---
apiVersion: kagent.dev/v1alpha3
kind: Harness
spec:
  workload:
    image: "ghcr.io/giantswarm/kagent/golang-adk@sha256:a2d23f5eb9c01e1903459a6e742f7d4aaa5e950d7e9aa6f07f8982761be0163a"
  substrate:
    snapshotPolicy:
      # a URL under a scraped key is no image
      location: s3://ate-snapshots/kagent
`
	got := scrapeImages(rendered)
	want := []string{
		refPostgres,
		"ghcr.io/giantswarm/kagent/golang-adk@sha256:a2d23f5eb9c01e1903459a6e742f7d4aaa5e950d7e9aa6f07f8982761be0163a",
		"ghcr.io/giantswarm/substrate/ateom-gvisor:0.0.27-gs.5",
		"gsoci.azurecr.io/giantswarm/agentgateway:v1.4.1",
		"gsoci.azurecr.io/giantswarm/mcp-kubernetes:1.0.9",
		"gsoci.azurecr.io/giantswarm/muster:5.7.2",
		"gsoci.azurecr.io/giantswarm/pgvector:0.8.2-18-bookworm",
		"gsoci.azurecr.io/giantswarm/postgresql-cnpg:18.3",
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
	installFakeTool(t, dir, "docker", `[ "$1" = images ] || { echo "unexpected docker $*" >&2; exit 2; }
cat "$FAKE_HOST_IMAGES"`)
	// What the pods run, spelled as their manifests spell it; the node's
	// own images (kindnet, etcd) are baked into the node image.
	stubLabKube(t, &kubeClients{clientset: kubefake.NewClientset(
		podOf("backstage", "backstage-dev:multi-backend-022f5b7e"),
		podOf("kagent-pg", "postgres:18.3-alpine"),
		podOf("harness", refGolangADK),
		podOf("muster", refMusterDev),
		podOf("kindnet", "docker.io/kindest/kindnetd:v20250512-df8de77b", "registry.k8s.io/etcd:3.6.4-0"),
	)})
	writeHost := func(rows string) {
		if err := os.WriteFile(hostImages, []byte(rows), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(phase string, wantPull, wantLocal []string) {
		t.Helper()
		snapshotPreloadImages()
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

// The two side-loads without `docker save --platform`. Podman: its multi-image
// save is single-image, so the batch would land one image under every tag —
// one plain save per image. Docker older than 28: no `--platform`, so one
// plain save of the whole batch (right under the classic graph driver such an
// engine runs). Either way the stream goes into the node through the embedded
// kind's import — never through a kind on PATH, never through a file.
func TestKindLoadImagesArchivesWithoutSavePlatform(t *testing.T) {
	images := []string{refPostgres, refGolangADK, refMusterDev}
	for name, tc := range map[string]struct {
		podman    bool
		wantSaves []string
	}{
		"docker without save --platform batches": {false, []string{
			"docker save " + refPostgres + " " + refGolangADK + " " + refMusterDev,
		}},
		"podman saves one at a time": {true, []string{
			"docker save " + refPostgres,
			"docker save " + refGolangADK,
			"docker save " + refMusterDev,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dockerCalls := installFakeTool(t, dir, "docker", `[ "$1" = save ] || { echo "unexpected docker $*" >&2; exit 2; }
echo "$*"`)
			kindCalls := stubKindLoadArchive(t, dir)
			withPodman(t, tc.podman)
			withSavePlatform(t, false)
			loaded, err := kindLoadImages(config.Default(), images)
			if err != nil {
				t.Fatalf("kindLoadImages: %v", err)
			}
			if loaded != len(images) {
				t.Errorf("loaded %d images, want %d", loaded, len(images))
			}
			if got := callLines(t, dockerCalls); !slices.Equal(got, tc.wantSaves) {
				t.Errorf("saves\n got  %v\n want %v", got, tc.wantSaves)
			}
			if got, want := callLines(t, kindCalls), kindLoads(tc.wantSaves...); !slices.Equal(got, want) {
				t.Errorf("kind loads\n got  %v\n want %v", got, want)
			}
		})
	}
}

// stubKindLoadArchive stands in for the embedded kind's archive import
// (kindLoadArchive): it drains the stream and logs each call — the cluster
// and what came down the stream — to a file under dir, returned for
// callLines. The stand-in docker echoes its argv as the archive, so the log
// proves which save fed which import; an import fed nothing (its save
// failed) logs the bare call.
func stubKindLoadArchive(t *testing.T, dir string) string {
	t.Helper()
	calls := filepath.Join(dir, "kind-calls")
	prev := kindLoadArchive
	kindLoadArchive = func(clusterName string, archive io.Reader) error {
		got, err := io.ReadAll(archive)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- the call log under t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(f, strings.TrimSpace("kindLoadArchive "+clusterName+" "+string(got)))
		_ = f.Close()
		return nil
	}
	t.Cleanup(func() { kindLoadArchive = prev })
	return calls
}

// kindLoads is the import log the stand-ins write for the given saves: one
// import per save, fed that save's argv ("docker save ..." as the docker
// stand-in logs it, "save ..." as it echoes it into the stream).
func kindLoads(saves ...string) []string {
	var lines []string
	for _, save := range saves {
		lines = append(lines, "kindLoadArchive agentlab "+strings.TrimPrefix(save, "docker "))
	}
	return lines
}

// kindLoadFedNothing is the import log of a save that failed before it wrote
// a byte: the import still ran, on an empty stream.
const kindLoadFedNothing = "kindLoadArchive agentlab"

// callLines splits a stand-in's log into lines, so the argv can be compared
// exactly.
func callLines(t *testing.T, path string) []string {
	t.Helper()
	raw := strings.TrimSpace(readCalls(t, path))
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// installSideloadFakes puts a docker on PATH and a stand-in behind kind's
// archive import for the docker side-load: docker answers `image inspect`
// with a platform per ref (muster amd64, everything else arm64) and runs
// saveBody for `save --platform`; the import drains and logs what it is fed.
func installSideloadFakes(t *testing.T, dir, saveBody string) (dockerCalls, kindCalls string) {
	t.Helper()
	dockerCalls = installFakeTool(t, dir, "docker", `case "$1 $2" in
"image inspect") case "$5" in
  *muster*) echo linux/amd64 ;;
  *socat*) echo "Error response from daemon: No such image: $5" >&2; exit 1 ;;
  *) echo linux/arm64/v8 ;;
  esac ;;
"save --platform") `+saveBody+` ;;
*) echo "unexpected docker $*" >&2; exit 2 ;;
esac`)
	kindCalls = stubKindLoadArchive(t, dir)
	withPodman(t, false)
	withSavePlatform(t, true)
	return dockerCalls, kindCalls
}

// Under docker the side-load is `docker save --platform` streamed into kind's
// archive import, one stream per platform the host holds the images in,
// never a plain `docker save`: that breaks under the containerd image store
// (kind#3795). Every ref is inspected for its platform, and each import is
// fed exactly its platform's save.
func TestDockerLoadImagesSavesOneArchivePerPlatform(t *testing.T) {
	dir := t.TempDir()
	dockerCalls, kindCalls := installSideloadFakes(t, dir, `echo "$*"`)
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
		"docker save --platform linux/amd64 " + refMusterDev,
		"docker save --platform linux/arm64/v8 " + refPostgres + " " + refGolangADK,
	}
	if !slices.Equal(saves, wantSaves) {
		t.Errorf("saves\n got  %v\n want %v", saves, wantSaves)
	}
	if got, want := callLines(t, kindCalls), kindLoads(wantSaves...); !slices.Equal(got, want) {
		t.Errorf("kind loads\n got  %v\n want %v", got, want)
	}
}

// A platform group whose save fails, and a ref the host cannot inspect, must
// not stop the other groups: the count says how many landed, and the error
// names what did not in docker's words — not the cut stream's import error.
func TestDockerLoadImagesPartialFailure(t *testing.T) {
	dir := t.TempDir()
	dockerCalls, kindCalls := installSideloadFakes(t, dir, `case "$3" in
  linux/amd64) echo "Error response from daemon: no suitable export target found" >&2; exit 1 ;;
  *) echo "$*" ;;
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
		"docker save --platform linux/amd64 " + refMusterDev,
		"docker save --platform linux/arm64/v8 " + refPostgres + " " + refGolangADK,
	}
	if !slices.Equal(saves, wantSaves) {
		t.Errorf("saves\n got  %v\n want %v", saves, wantSaves)
	}
	want := []string{kindLoadFedNothing, kindLoads(wantSaves[1])[0]}
	if got := callLines(t, kindCalls); !slices.Equal(got, want) {
		t.Errorf("kind loads\n got  %v\n want %v (the arm64 group's import fed its save, the amd64 group's nothing)", got, want)
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

// Under podman a failed image must not stop the rest, the count must say how
// many landed so the caller can report a partial load as one, and the error
// must carry docker's words.
func TestKindLoadImagesPartialFailureUnderPodman(t *testing.T) {
	dir := t.TempDir()
	installFakeTool(t, dir, "docker", `case "$2" in
`+refGolangADK+`) echo "Error response from daemon: No such image: $2" >&2; exit 1 ;;
*) echo "$*" ;;
esac`)
	kindCalls := stubKindLoadArchive(t, dir)
	withPodman(t, true)
	loaded, err := kindLoadImages(config.Default(), []string{refPostgres, refGolangADK, refMusterDev})
	if err == nil {
		t.Fatal("a failed image must surface as an error")
	}
	if !strings.Contains(err.Error(), "No such image") {
		t.Errorf("error must carry docker's words, got:\n%v", err)
	}
	if loaded != 2 {
		t.Errorf("loaded = %d, want 2 (the failure must not stop the rest)", loaded)
	}
	want := []string{
		"kindLoadArchive agentlab save " + refPostgres,
		kindLoadFedNothing,
		"kindLoadArchive agentlab save " + refMusterDev,
	}
	if got := callLines(t, kindCalls); !slices.Equal(got, want) {
		t.Errorf("kind loads\n got  %v\n want %v", got, want)
	}
}

// podOf is a pod running the given images as its containers, in a namespace
// of its own so a listing that finds it is provably cluster-wide.
func podOf(name string, images ...string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: name}}
	for i, img := range images {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: fmt.Sprintf("c%d", i), Image: img})
	}
	return pod
}

// podImages reads what the pods reference — init and ephemeral containers
// included, across namespaces, each ref once — spelled as the node spells it;
// a digest-pinned or bare ref is left to the kubelet.
func TestPodImages(t *testing.T) {
	app := podOf("app", "alpine/k8s:1.37.0", "busybox")
	app.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "postgres:18.3-alpine"}}
	app.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: refSocat},
	}}
	stubLabKube(t, &kubeClients{clientset: kubefake.NewClientset(app,
		podOf("pinned", "gsoci.azurecr.io/giantswarm/rustfs@sha256:0123abcd", refMusterDev),
		podOf("again", refMusterDev),
	)})
	got, err := podImages()
	if err != nil {
		t.Fatalf("podImages: %v", err)
	}
	want := []string{"docker.io/alpine/k8s:1.37.0", refSocat, refPostgres, refMusterDev}
	if !slices.Equal(got, want) {
		t.Errorf("pod images\n got  %v\n want %v", got, want)
	}
}
