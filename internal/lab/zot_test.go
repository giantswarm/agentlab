package lab

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// Zot release tags the tests pick among.
const (
	zotV2121 = "v2.1.21"
	zotV2122 = "v2.1.22"
	zotV2123 = "v2.1.23"
	zotV2124 = "v2.1.24"
	zotV300  = "v3.0.0"
)

// stubZotReleases stands in for gsoci's zot tags and the host docker cache's
// for one test.
func stubZotReleases(t *testing.T, latest string, listErr error, cached []string) {
	t.Helper()
	prevLatest, prevCached := latestZotRelease, cachedZotTags
	t.Cleanup(func() { latestZotRelease, cachedZotTags = prevLatest, prevCached })
	latestZotRelease = func() (string, error) { return latest, listErr }
	cachedZotTags = func() ([]string, error) { return cached, nil }
}

// TestPickZotImage: a container follows the newest release on gsoci; a lab's
// zotVersion holds the lab registry at its release; with gsoci unlisted a
// container in place keeps its version and a new one takes the newest zot
// at or above the floor in the host docker cache, and with none the run
// fails.
func TestPickZotImage(t *testing.T) {
	offline := errors.New("dial tcp: lookup gsoci.azurecr.io: no such host")
	old := config.ZotImage(zotV2121)
	for _, tc := range []struct {
		name      string
		latest    string
		listErr   error
		cached    []string
		pin       string
		running   string
		want      string
		wantWhy   string
		wantError string
	}{
		{name: "the newest release", latest: zotV2124, running: old, want: config.ZotImage(zotV2124)},
		{name: "a new container on the newest release", latest: zotV2123, want: config.ZotImage(zotV2123)},
		{name: "held at zotVersion", latest: zotV2124, pin: zotV2123, running: old, want: config.ZotImage(zotV2123)},
		{name: "held below the floor", latest: zotV2124, pin: zotV2121, wantError: "below v" + config.ZotFloor.String()},
		{name: "gsoci unlisted, a container in place keeps its version", listErr: offline, running: old, want: old, wantWhy: "keeps " + old},
		{
			name: "gsoci unlisted, a new container takes the newest in the cache", listErr: offline,
			cached: []string{zotV2121, zotV2124, zotV2123, zotV300, "<none>"},
			want:   config.ZotImage(zotV2124), wantWhy: "newest in the host docker cache",
		},
		{name: "gsoci unlisted, nothing usable in the cache", listErr: offline, cached: []string{zotV2121}, wantError: "no such host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubZotReleases(t, tc.latest, tc.listErr, tc.cached)
			got, why, err := pickZotImage(tc.pin, tc.running)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("pickZotImage = %q, %v; want an error containing %q", got, err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
			if !strings.Contains(why, tc.wantWhy) || (tc.wantWhy == "" && why != "") {
				t.Errorf("why = %q, want %q", why, tc.wantWhy)
			}
		})
	}
}

// TestZotChanges: a container is recreated when its release, its
// configuration or its volume differ from this run's, and each reason is
// named; a current one stays.
func TestZotChanges(t *testing.T) {
	cur := zotState{state: stateRunning, image: config.ZotImage(zotV2123), config: configDigest(devRegistryConfig), volume: "agentlab-registry"}
	if got := zotChanges(cur, cur.image, cur.config, cur.volume); len(got) != 0 {
		t.Fatalf("zotChanges of a current container = %v, want none", got)
	}
	shipped := zotState{state: stateRunning, image: config.ZotImage(zotV2121)}
	got := zotChanges(shipped, cur.image, cur.config, cur.volume)
	want := []string{"zot v2.1.21 → v2.1.23", "agentlab's configuration", "its storage into the volume agentlab-registry"}
	if !slices.Equal(got, want) {
		t.Fatalf("zotChanges of a v2.1.21 registry on the shipped configuration = %q, want %q", got, want)
	}
}

// TestParseZotState reads inspectZot's line, a container without the label
// or the volume included.
func TestParseZotState(t *testing.T) {
	got := parseZotState("running\tgsoci.azurecr.io/giantswarm/zot:v2.1.23\tabc\tagentlab-mirror\n")
	if got != (zotState{state: stateRunning, image: config.ZotImage(zotV2123), config: "abc", volume: mirrorName}) {
		t.Errorf("parseZotState = %+v", got)
	}
	got = parseZotState("exited\tgsoci.azurecr.io/giantswarm/zot:v2.1.21\t\t\n")
	if got != (zotState{state: stateExited, image: config.ZotImage(zotV2121)}) {
		t.Errorf("parseZotState of a pre-volume registry = %+v", got)
	}
}

// TestZotContainerArgs: both containers restart with the daemon, carry
// their configuration's digest, mount the storage volume of their own name
// and the configuration read-only, on the kind network; the lab registry
// publishes its port, the mirror nothing.
func TestZotContainerArgs(t *testing.T) {
	z := zotContainer{name: "agentlab-registry", configPath: "/state/registry/config.json", config: devRegistryConfig,
		flags: []string{"-p", "127.0.0.1:5001:5000"}}
	args := z.containerArgs("run", config.ZotImage(zotV2123), "d1g")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --name agentlab-registry --network kind",
		"-d --restart=always --label " + zotConfigLabel + "=d1g",
		"-v agentlab-registry:/var/lib/registry -v /state/registry/config.json:/etc/zot/config.json:ro",
		"-p 127.0.0.1:5001:5000 " + config.ZotImage(zotV2123),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker %s\nlacks %q", joined, want)
		}
	}
	if args[len(args)-1] != config.ZotImage(zotV2123) {
		t.Errorf("the image is not the last argument: %v", args)
	}
	if created := strings.Join(z.containerArgs("create", "img", "d1g"), " "); strings.Contains(created, " -d ") || !strings.HasPrefix(created, "create ") {
		t.Errorf("docker %s, want a create without -d", created)
	}
}

// TestZotConfigs: both configurations are JSON zot reads, on the port and
// storage the containers use, with Docker's manifest types and no search
// extension (no CVE scanning); the mirror syncs gsoci on demand with
// streaming, a write timeout fit for a streamed blob, anonymous clients
// read-only and a retention by last pull.
func TestZotConfigs(t *testing.T) {
	type zotConfig struct {
		Storage struct {
			RootDirectory string `json:"rootDirectory"`
			GC            bool   `json:"gc"`
			Retention     struct {
				Policies []struct {
					KeepTags []struct {
						PulledWithin string `json:"pulledWithin"`
					} `json:"keepTags"`
				} `json:"policies"`
			} `json:"retention"`
		} `json:"storage"`
		HTTP struct {
			Port          string   `json:"port"`
			Compat        []string `json:"compat"`
			WriteTimeout  string   `json:"writeTimeout"`
			AccessControl struct {
				Repositories map[string]struct {
					AnonymousPolicy []string `json:"anonymousPolicy"`
				} `json:"repositories"`
			} `json:"accessControl"`
		} `json:"http"`
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	read := func(name string, raw []byte) zotConfig {
		t.Helper()
		var c zotConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Storage.RootDirectory != zotStorageDir || c.HTTP.Port != "5000" || !slices.Equal(c.HTTP.Compat, []string{"docker2s2"}) {
			t.Errorf("%s: storage %q, port %q, compat %v", name, c.Storage.RootDirectory, c.HTTP.Port, c.HTTP.Compat)
		}
		if _, ok := c.Extensions["search"]; ok {
			t.Errorf("%s: the search extension is on (CVE scanning downloads the Trivy databases)", name)
		}
		return c
	}
	if reg := read("the lab registry", devRegistryConfig); len(reg.Extensions) != 0 {
		t.Errorf("the lab registry runs extensions %v, want none", slices.Sorted(maps.Keys(reg.Extensions)))
	}
	m := read("the mirror", mirrorConfig)
	if m.HTTP.WriteTimeout != "30m" {
		t.Errorf("the mirror's writeTimeout = %q, want 30m", m.HTTP.WriteTimeout)
	}
	if got := m.HTTP.AccessControl.Repositories["**"].AnonymousPolicy; !slices.Equal(got, []string{"read"}) {
		t.Errorf("the mirror's anonymous policy = %v, want read only", got)
	}
	if !m.Storage.GC || len(m.Storage.Retention.Policies) != 1 || m.Storage.Retention.Policies[0].KeepTags[0].PulledWithin != "336h" {
		t.Errorf("the mirror's GC and retention = %+v", m.Storage)
	}
	var sync struct {
		Enable     bool `json:"enable"`
		Registries []struct {
			URLs                  []string `json:"urls"`
			OnDemand              bool     `json:"onDemand"`
			Stream                bool     `json:"stream"`
			TLSVerify             bool     `json:"tlsVerify"`
			PollInterval          string   `json:"pollInterval"`
			ManifestCheckInterval string   `json:"manifestCheckInterval"`
			Content               []struct {
				Prefix string `json:"prefix"`
			} `json:"content"`
		} `json:"registries"`
	}
	if err := json.Unmarshal(m.Extensions["sync"], &sync); err != nil {
		t.Fatal(err)
	}
	if !sync.Enable || len(sync.Registries) != 1 {
		t.Fatalf("the mirror's sync = %+v", sync)
	}
	r := sync.Registries[0]
	if !slices.Equal(r.URLs, []string{"https://" + mirrorUpstream}) || !r.OnDemand || !r.Stream || !r.TLSVerify ||
		r.PollInterval != "" || r.ManifestCheckInterval != "" || len(r.Content) != 1 || r.Content[0].Prefix != "/giantswarm/**" {
		t.Errorf("the mirror's upstream = %+v", r)
	}
}

// TestMirrorHosts: a node's hosts.toml puts the mirror first for tags and
// blobs, gsoci as the server it falls back to.
func TestMirrorHosts(t *testing.T) {
	want := "server = \"https://gsoci.azurecr.io\"\n\n[host.\"http://agentlab-mirror:5000\"]\n  capabilities = [\"pull\", \"resolve\"]\n"
	if string(mirrorHosts) != want {
		t.Errorf("hosts.toml =\n%s\nwant\n%s", mirrorHosts, want)
	}
	if mirrorHostsFile != "/etc/containerd/certs.d/gsoci.azurecr.io/hosts.toml" {
		t.Errorf("hosts.toml path = %s", mirrorHostsFile)
	}
}

// TestMirrorContainerLivesBesideTheLabs: the mirror's configuration is
// host state beside the labs, not any lab's, and it follows the newest zot
// whatever a lab holds.
func TestMirrorContainerLivesBesideTheLabs(t *testing.T) {
	z, err := mirrorContainer()
	if err != nil {
		t.Fatal(err)
	}
	if z.name != mirrorName || z.pin != "" || len(z.flags) != 0 {
		t.Errorf("mirror container = %+v, want %s unpinned and unpublished", z, mirrorName)
	}
	if want := filepath.Join("agentlab", "mirror", "config.json"); !strings.HasSuffix(z.configPath, want) || !filepath.IsAbs(z.configPath) {
		t.Errorf("the mirror's configuration is %s, want an absolute path ending %s", z.configPath, want)
	}
}

// TestWriteIfChanged writes a file and its directory, and leaves a file
// that already holds the bytes untouched.
func TestWriteIfChanged(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mirror", "config.json")
	if err := writeIfChanged(file, mirrorConfig); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); string(got) != string(mirrorConfig) { // #nosec G304 -- the test's own temporary file
		t.Fatalf("wrote %q", got)
	}
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := writeIfChanged(file, mirrorConfig); err != nil {
		t.Fatalf("rewriting the same bytes into a read-only file = %v, want it left alone", err)
	}
}

// TestNothingToCopy reads docker's and podman's words for a path a
// container does not have, and nothing else.
func TestNothingToCopy(t *testing.T) {
	for msg, want := range map[string]bool{
		"Error response from daemon: Could not find the file /var/lib/registry in container agentlab-registry-moving":    true,
		`Error: "/var/lib/registry" could not be found on container agentlab-registry-moving: no such file or directory`: true,
		"Error response from daemon: No such container: agentlab-registry-moving":                                        false,
		"permission denied": false,
	} {
		if got := nothingToCopy(msg); got != want {
			t.Errorf("nothingToCopy(%q) = %v, want %v", msg, got, want)
		}
	}
}

// TestMirrorStatusLine words the mirror for `agentlab status`.
func TestMirrorStatusLine(t *testing.T) {
	for _, tc := range []struct {
		s    MirrorStatus
		want string
	}{
		{MirrorStatus{On: true, State: stateRunning, Release: zotV2123, Size: "1.2GB"}, "agentlab-mirror running (zot v2.1.23, 1.2GB cached)"},
		{MirrorStatus{On: true, State: registryMissing}, "agentlab-mirror missing — the nodes pull from gsoci.azurecr.io directly; `agentlab platform` creates it"},
		{MirrorStatus{On: true, State: stateExited, Release: zotV2123}, "agentlab-mirror exited (zot v2.1.23) — the nodes pull from gsoci.azurecr.io directly; `agentlab platform` starts it"},
		{MirrorStatus{State: stateRunning, Release: zotV2123}, "agentlab-mirror running (zot v2.1.23) — this lab is off it (registryMirror: false)"},
		{MirrorStatus{State: registryMissing}, "off (registryMirror: false)"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("%+v:\n got %s\nwant %s", tc.s, got, tc.want)
		}
	}
}

// TestRegistryLineNamesTheZotRelease: the lab registry's status line names
// the release it runs.
func TestRegistryLineNamesTheZotRelease(t *testing.T) {
	if got, want := registryLine("agentlab", stateRunning, zotV2123), "agentlab-registry running (zot v2.1.23)"; got != want {
		t.Errorf("registryLine = %q, want %q", got, want)
	}
	if got := registryLine("agentlab", stateExited, zotV2121); !strings.HasPrefix(got, "agentlab-registry exited (zot v2.1.21) — ") {
		t.Errorf("registryLine of a stopped registry = %q", got)
	}
}
