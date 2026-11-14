package lab

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/xdg"
)

// The registry mirror (registryMirror, docs/registries.md): one zot on the
// machine, shared by every lab, that fetches each gsoci image once, when a
// node first asks for it, and serves it to every node after that. Each
// node's containerd pulls gsoci through it (a hosts.toml under the
// config_path the kind config sets) with gsoci itself as the fallback: a
// mirror that is missing, stopped or failing costs a failed request, and the
// pull goes on from gsoci. So nothing about the mirror ever fails a run.
//
// What it covers is the node lane: the images a component release brings
// between two runs, which each node's kubelet pulls itself (the boot's
// side-load from the host docker cache, preload.go, is shared by the labs
// already). What it does not: the host lane's own pulls, atelet's Harness
// images (its layer cache, not containerd) and Flux's chart pulls.

// mirrorName is the mirror's container, and its cache's named volume: one
// per machine, on the kind network next to every lab's nodes.
const mirrorName = "agentlab-mirror"

// mirrorUpstream is the registry the mirror serves the nodes.
const mirrorUpstream = "gsoci.azurecr.io"

// mirrorHostsFile is the containerd hosts file that sends a node's pulls
// from mirrorUpstream through the mirror; containerd reads it on every pull,
// so a file written into a running node applies from its next pull.
const mirrorHostsFile = "/etc/containerd/certs.d/" + mirrorUpstream + "/hosts.toml"

// mirrorEndpoint is the mirror as the nodes reach it on the kind network,
// by name through docker's embedded DNS.
var mirrorEndpoint = "http://" + mirrorName + ":" + strconv.Itoa(zotContainerPort)

// mirrorHosts is a node's hosts.toml: the mirror first, for tags and blobs
// both (resolve and pull), and gsoci as the server it falls back to — kind's
// local-registry recipe's shape. With resolve on the mirror it holds tags,
// which its retention keeps by when they were last pulled; zot checks every
// tag request against gsoci, so a tag cannot go stale.
var mirrorHosts = []byte(`server = "https://` + mirrorUpstream + `"

[host."` + mirrorEndpoint + `"]
  capabilities = ["pull", "resolve"]
`)

// mirrorConfig is the mirror's zot configuration. What each non-default
// setting is for:
//
//   - sync onDemand without a pollInterval: zot fetches what a node asks for
//     and never crawls gsoci; only the platforms asked for of a multi-arch
//     index (sparse sync), and manifestCheckInterval left unset, so every
//     tag request is checked against gsoci.
//   - stream: a manifest is answered at once and blob reads are fed from
//     the download in flight, which every client asking for the image
//     shares — no first pull waiting for zot to copy a whole image (CRI
//     cancels a pull that reads nothing for five minutes).
//   - writeTimeout: zot's default of 60 s is a deadline per response and
//     would cut a streamed blob off on a slow link.
//   - compat docker2s2: a quarter of gsoci's images are Docker manifest
//     lists, which sync refuses without it.
//   - accessControl read-only for anonymous clients, so nothing on the kind
//     network (a pod included) can push a tag into a repository every node
//     trusts.
//   - retention: a tag not pulled for two weeks goes, and GC with it, which
//     bounds the cache to what the labs pulled lately; zot has no size cap.
//   - no search extension: no CVE scanning and no Trivy downloads.
var mirrorConfig = []byte(`{
  "distSpecVersion": "1.1.1",
  "storage": {
    "rootDirectory": "` + zotStorageDir + `",
    "gc": true,
    "retention": {
      "delay": "24h",
      "policies": [{
        "repositories": ["**"],
        "deleteReferrers": true,
        "deleteUntagged": true,
        "keepTags": [{ "pulledWithin": "336h" }]
      }]
    }
  },
  "http": {
    "address": "0.0.0.0",
    "port": "` + strconv.Itoa(zotContainerPort) + `",
    "compat": ["docker2s2"],
    "writeTimeout": "30m",
    "accessControl": { "repositories": { "**": { "anonymousPolicy": ["read"] } } }
  },
  "log": { "level": "info" },
  "extensions": {
    "sync": {
      "enable": true,
      "registries": [{
        "urls": ["https://` + mirrorUpstream + `"],
        "onDemand": true,
        "stream": true,
        "tlsVerify": true,
        "content": [{ "prefix": "/giantswarm/**" }]
      }]
    }
  }
}
`)

// mirrorProbeAttempts bounds the wait for a freshly created mirror to answer
// a node (one second apart); an existing one is probed once. A dead mirror
// costs a fallback, never the run, so this only decides what the run says.
const mirrorProbeAttempts = 15

// mirrorContainer is the mirror as a zot container: no published port (only
// the nodes use it), its configuration in agentlab's state beside the labs,
// and the newest zot release whatever a lab's zotVersion says.
func mirrorContainer() (zotContainer, error) {
	state, err := xdg.StateHome()
	if err != nil {
		return zotContainer{}, err
	}
	return zotContainer{
		name:       mirrorName,
		what:       "the registry mirror",
		configPath: filepath.Join(state, "mirror", "config.json"),
		config:     mirrorConfig,
	}, nil
}

// ensureRegistryMirror applies registryMirror to the cluster's nodes: on,
// the mirror running (created, upgraded or started; zotContainer.ensure) and
// every node's hosts.toml written — substrateNodes workers too; off, the
// hosts.toml removed and the mirror left to the other labs. A failure is a
// warning: the nodes then pull from gsoci as they would without it.
func ensureRegistryMirror(cfg *config.Config) {
	nodes, err := kindNodeNames(cfg.ClusterName)
	if err != nil {
		warn("the registry mirror: %v; the nodes pull from %s directly", err, mirrorUpstream)
		return
	}
	if !cfg.RegistryMirrorOn() {
		if removed := removeMirrorHosts(nodes); len(removed) > 0 {
			note("registryMirror is off: the %s pulls of %s go to it directly again (%s stays for the other labs; `agentlab down --mirror` removes it)", mirrorUpstream, strings.Join(removed, ", "), mirrorName)
		}
		return
	}
	step("Ensuring the registry mirror %s", mirrorName)
	z, err := mirrorContainer()
	if err != nil {
		warn("the registry mirror: %v; the nodes pull from %s directly", err, mirrorUpstream)
		return
	}
	image, fresh, err := z.ensure()
	if err != nil {
		warn("%v; the nodes pull from %s directly", err, mirrorUpstream)
		return
	}
	var wrote []string
	for _, node := range nodes {
		if err := writeNodeFile(node, mirrorHostsFile, mirrorHosts); err != nil {
			warn("writing %s into %s: %v; that node pulls from %s directly", mirrorHostsFile, node, err, mirrorUpstream)
			continue
		}
		wrote = append(wrote, node)
	}
	if len(wrote) == 0 {
		return
	}
	attempts := 1
	if fresh {
		attempts = mirrorProbeAttempts
	}
	if waitFor(attempts, time.Second, func() bool { return mirrorAnswers(wrote[0]) }) {
		note("the %s pulls of %s go through %s (zot %s), %s itself the fallback", mirrorUpstream, strings.Join(wrote, ", "), mirrorName, zotTag(image), mirrorUpstream)
		return
	}
	warn("%s does not answer %s on %s/v2/ (`docker logs %s`); its pulls fall back to %s until it does", mirrorName, wrote[0], mirrorEndpoint, mirrorName, mirrorUpstream)
}

// mirrorAnswers probes the mirror's API from inside a node, the way its
// containerd reaches it (the kind node image has curl).
func mirrorAnswers(node string) bool {
	_, err := outputQuiet(dockerBin, "exec", node, "curl", "-fsS", "-m", "3", "-o", "/dev/null", mirrorEndpoint+"/v2/")
	return err == nil
}

// writeNodeFile writes a file into a node container, its directory created:
// it lives in the node's own layer, so it survives a node restart, and goes
// with the node.
func writeNodeFile(node, file string, raw []byte) error {
	cmd := command(dockerBin, "exec", "-i", node, "sh", "-c", `mkdir -p "$1" && cat > "$2"`, "sh", path.Dir(file), file)
	cmd.Stdin = strings.NewReader(string(raw))
	if out, err := cmd.CombinedOutput(); err != nil {
		return cmdError(dockerBin, cmd.Args[1:], err, out)
	}
	return nil
}

// removeMirrorHosts removes the hosts.toml from the nodes that carry it and
// names them.
func removeMirrorHosts(nodes []string) []string {
	var removed []string
	for _, node := range nodes {
		if _, err := outputQuiet(dockerBin, "exec", node, "test", "-e", mirrorHostsFile); err != nil {
			continue
		}
		if err := runQuiet(dockerBin, "exec", node, "rm", "-f", mirrorHostsFile); err != nil {
			warn("removing %s from %s: %v", mirrorHostsFile, node, err)
			continue
		}
		removed = append(removed, node)
	}
	return removed
}

// RemoveRegistryMirror removes the mirror and its cache, for `agentlab down
// --mirror`. The other labs' nodes keep their hosts.toml and pull from gsoci
// meanwhile; their next `up` or `platform` creates the mirror again.
func RemoveRegistryMirror() {
	removeZot(mirrorName, "the registry mirror")
}

// MirrorStatus is the mirror as `agentlab status` reports it.
type MirrorStatus struct {
	// On is the lab's registryMirror.
	On bool `json:"on"`
	// State is the container's docker state, "missing" when there is none.
	State string `json:"state"`
	// Release is the zot release it runs; "" when missing.
	Release string `json:"release,omitempty"`
	// Size is the size docker reports for its cache volume; "" when it
	// reports none.
	Size string `json:"size,omitempty"`
}

// readMirrorStatus reads the mirror's state on this machine.
func readMirrorStatus(cfg *config.Config) *MirrorStatus {
	s := &MirrorStatus{On: cfg.RegistryMirrorOn(), State: registryMissing}
	have, ok := inspectZot(mirrorName)
	if !ok {
		return s
	}
	s.State, s.Release = have.state, zotTag(have.image)
	s.Size = zotVolumeSize(mirrorName)
	return s
}

// String is the status line on the mirror.
func (s *MirrorStatus) String() string {
	if s.State == registryMissing {
		if s.On {
			return mirrorName + " missing — the nodes pull from " + mirrorUpstream + " directly; `agentlab platform` creates it"
		}
		return "off (registryMirror: false)"
	}
	line := fmt.Sprintf("%s %s (zot %s", mirrorName, s.State, s.Release)
	if s.Size != "" {
		line += ", " + s.Size + " cached"
	}
	line += ")"
	switch {
	case !s.On:
		line += " — this lab is off it (registryMirror: false)"
	case s.State != stateRunning:
		line += " — the nodes pull from " + mirrorUpstream + " directly; `agentlab platform` starts it"
	}
	return line
}
