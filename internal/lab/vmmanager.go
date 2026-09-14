package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/giantswarm/vm-manager/pkg/guestimage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The vm-manager wiring (platform.vmManager in agentlab.yaml): the platform's
// VM provisioner (github.com/giantswarm/vm-manager) runs as a POD of the KVM
// node, the shape of its siblings agent-manager and model-manager — the
// agent-platform chart's components.vm-manager, turned on by the lab's values
// template. The kind node is a privileged docker container, so the host's
// /dev/kvm and /dev/vhost-vsock are in it, and the runtime hands them to the
// privileged pod — nothing is mounted from the node. The guest image the pod
// boots is an OCI artifact its init container fetches: the one the chart's
// release published (the chart default), or a local build (`make -C images`
// in a vm-manager checkout, platform.vmManager.imageDir) that `agentlab
// platform` pushes into the lab registry and pins by digest, the way the
// Harness dev image travels. A build of the checkout's binary swaps in
// through the dev-image loop (platform.devImages.vm-manager, devimages.go).
//
// Identity is the chart's: the meta chart's vm-manager block turns OAuth on
// against the lab Dex through global.identity, and the chart's own MCPServer
// CR carries the agent-platform tool group and forward-token auth, so muster
// forwards the person's Dex id_token and vm-manager validates it. What the
// wiring proves is `agentlab vm-manager-test` (vmmanagertest.go).

// vmManagerMCPServer is the MCPServer CR name the chart renders; muster
// prefixes its tools with it: x_vm-manager_<tool>. Also the component, the
// release, the Deployment and the Service name (fullnameOverride).
const vmManagerMCPServer = "vm-manager"

// The lab registry's copy of a local guest image build: one repository, one
// tag, the chart pinned to the pushed digest (vmManagerGuestImageStateFile),
// so a rebuilt image is a new digest and a rolled pod.
const (
	vmManagerGuestImageRepository = "vm-manager-guest-image"
	vmManagerGuestImageTag        = "dev"
)

// vmManagerGuestImageStateFile records the last push of
// platform.vmManager.imageDir into the lab registry (guestImageRecord); the
// values template pins the chart to it.
const vmManagerGuestImageStateFile = StateDir + "/vm-manager-guest-image.json"

// guestImageRecord is the content of vmManagerGuestImageStateFile.
type guestImageRecord struct {
	// ImageDir the artifact was built from, as configured.
	ImageDir string `json:"imageDir"`
	// Reference the host pushed to (localhost:<devRegistryPort>/...).
	Reference string `json:"reference"`
	// Digest of the artifact manifest.
	Digest string `json:"digest"`
}

// vmManagerGuestImage is the chart's guestImage block for a local build: the
// lab registry as pods reach it, by digest, over plain HTTP.
type vmManagerGuestImage struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// vmManagerHostPath is the guarded capability report the proof calls; the
// MCP endpoint is the chart's mcp.path, dialed by muster alone.
const vmManagerHostPath = "/api/v1/host"

// kvmDevices are the node devices the privileged pod gets from the runtime:
// without them vm-manager starts, reports them under `missing`, and
// create_vm cannot work.
var kvmDevices = []string{"/dev/kvm", "/dev/vhost-vsock"}

// legacyHostServiceLabel marked the registration an earlier agentlab created
// for a vm-manager running on the host (0.43). The chart now renders an
// MCPServer of the same name, which Helm refuses to adopt while the lab's
// object stands, so `agentlab platform` removes the leftover before the
// install (removeLegacyVMManagerRegistration).
const legacyHostServiceLabel = "agentlab.giantswarm.io/host-service"

// legacyVMManagerEnvFile is the environment file the host wiring wrote;
// removed with the registration.
const legacyVMManagerEnvFile = StateDir + "/vm-manager.env"

// missingKVMDevices lists the KVM devices this machine lacks, checked as the
// node sees them: a character device that exists. Empty when the machine can
// run the platform's VM provisioner.
func missingKVMDevices() []string {
	var missing []string
	for _, dev := range kvmDevices {
		st, err := os.Stat(dev)
		if err != nil || st.Mode()&os.ModeCharDevice == 0 {
			missing = append(missing, dev)
		}
	}
	return missing
}

// kvmFixHint is the fix for a missing KVM device: the module and the device
// node the platform's VM provisioner needs.
func kvmFixHint(missing []string) string {
	var fixes []string
	for _, dev := range missing {
		switch dev {
		case "/dev/kvm":
			fixes = append(fixes, "/dev/kvm: hardware virtualization (kvm_intel / kvm_amd) — enable it in the firmware, `modprobe kvm_intel` (or kvm_amd)")
		case "/dev/vhost-vsock":
			fixes = append(fixes, "/dev/vhost-vsock: `modprobe vhost_vsock` (and `echo vhost_vsock >| /etc/modules-load.d/vhost-vsock.conf` to keep it)")
		}
	}
	return "    " + strings.Join(fixes, "\n    ")
}

// preflightVMManager refuses platform.vmManager on a machine that cannot run
// it, before the install would leave a pod that reports its devices missing:
// the KVM devices on the host, and the same devices inside the running node
// (a node created before the module was loaded has no /dev/vhost-vsock — the
// node's /dev is populated at `kind create`).
func preflightVMManager(cfg *config.Config) error {
	if missing := missingKVMDevices(); len(missing) > 0 {
		return fmt.Errorf("platform.vmManager is on but this machine has no %s: the platform's VM provisioner runs as a pod of the kind node and needs the node's KVM devices.\n%s\n  Or turn it off: `agentlab configure --vm-manager=false`",
			strings.Join(missing, " and "), kvmFixHint(missing))
	}
	node := cfg.ControlPlaneNode()
	for _, dev := range kvmDevices {
		if _, err := outputQuiet(dockerBin, "exec", node, "test", "-c", dev); err != nil {
			return fmt.Errorf("the kind node %s has no %s although this machine does: the node's /dev is populated when the node is created, so the device appeared afterwards.\n  Fix: `agentlab down && agentlab up`", node, dev)
		}
	}
	if cfg.Platform.VMManager.ImageDir == "" {
		note("platform.vmManager.imageDir is unset: the pod fetches the guest image its chart release published (`agentlab configure --vm-manager-image-dir <a vm-manager checkout's images/build>` boots a local build instead)")
	}
	return nil
}

// pushVMManagerGuestImage publishes platform.vmManager.imageDir into the lab
// registry as the guest image artifact and records the digest the values
// template pins the chart to (vmManagerGuestImageStateFile). Idempotent: the
// registry keeps what it has, an unchanged build is the same digest and the
// pod stays. Without a directory the record goes, and the chart's default —
// the release's published artifact — applies.
func pushVMManagerGuestImage(ctx context.Context, cfg *config.Config) error {
	dir := cfg.Platform.VMManager.ImageDir
	if dir == "" {
		if err := os.Remove(vmManagerGuestImageStateFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := ensureDevRegistry(cfg); err != nil {
		return err
	}
	ref := devRegistryHost(cfg) + "/" + vmManagerGuestImageRepository + ":" + vmManagerGuestImageTag
	desc, err := guestimage.Push(ctx, dir, ref, guestimage.Options{PlainHTTP: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		return fmt.Errorf("pushing the guest image of %s to the lab registry: %w", dir, err)
	}
	record := guestImageRecord{ImageDir: dir, Reference: ref, Digest: desc.Digest.String()}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(vmManagerGuestImageStateFile, append(data, '\n'), 0o600); err != nil {
		return err
	}
	note("the guest image of %s is in the lab registry as %s (%s); the pod pulls it by digest", dir, ref, desc.Digest)
	return nil
}

// vmManagerGuestImageFor is the chart's guestImage block for this lab: the
// lab registry's copy of the local build, by the digest the last push
// recorded, or nil for the chart default (the release's artifact) — when no
// build is configured, and while none has been pushed yet: `agentlab up`
// renders the templates before the cluster and its registry exist, and
// `agentlab platform` pushes before it renders, so the record is there for
// the values that reach the release. A malformed record is an error.
func vmManagerGuestImageFor(cfg *config.Config) (*vmManagerGuestImage, error) {
	if cfg.Platform.VMManager.ImageDir == "" {
		return nil, nil
	}
	record, err := readGuestImageRecord(vmManagerGuestImageStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &vmManagerGuestImage{
		Registry:   devRegistryEndpoint(cfg),
		Repository: vmManagerGuestImageRepository,
		Tag:        vmManagerGuestImageTag,
		Digest:     record.Digest,
	}, nil
}

func readGuestImageRecord(path string) (guestImageRecord, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the lab's own state file.
	if err != nil {
		return guestImageRecord{}, err
	}
	var record guestImageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return guestImageRecord{}, fmt.Errorf("%s: %w", path, err)
	}
	if record.Digest == "" {
		return guestImageRecord{}, fmt.Errorf("%s: no digest recorded", path)
	}
	return record, nil
}

// removeLegacyVMManagerRegistration deletes the MCPServer an agentlab before
// the pod wiring registered for a host vm-manager, and its environment file.
// The chart renders an MCPServer of the same name in the same namespace, and
// Helm refuses to install over an object another owner created — so the
// leftover goes before the install. Idempotent; nothing to do on a lab that
// never had it (or has no muster yet: no CRD, no registration).
func removeLegacyVMManagerRegistration(ctx context.Context) error {
	if err := os.Remove(legacyVMManagerEnvFile); err == nil {
		note("removed %s — vm-manager runs as a pod now, nothing on the host reads it", legacyVMManagerEnvFile)
	}
	gvr, err := gvrFor(musterMCPServerResource)
	if err != nil {
		return nil // no muster yet, no CRD: nothing was ever registered
	}
	existing, err := listObjects(ctx, gvr, platformNamespace, legacyHostServiceLabel)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, member := range existing {
		note("removing the host-service registration %s (vm-manager runs as a pod now; the chart renders its MCPServer)", member.GetName())
		if err := deleteObject(ctx, gvr, platformNamespace, member.GetName(), fixtureDeleteWait); err != nil {
			return err
		}
	}
	return nil
}

// vmManagerServiceURL is vm-manager's in-cluster base URL: the Service the
// chart renders under its pinned name, in the platform namespace.
func vmManagerServiceURL() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", vmManagerMCPServer, platformNamespace)
}

// vmManagerHint is the platform-up summary for the vm-manager wiring.
func vmManagerHint(cfg *config.Config) string {
	if !cfg.VMManagerEnabled() {
		return "  vm-manager is not wired (platform.vmManager in agentlab.yaml; `agentlab configure --vm-manager --vm-manager-image-dir <dir>` turns it on)."
	}
	images := fmt.Sprintf("booting the local guest image build of %s (pushed to the lab registry)", cfg.Platform.VMManager.ImageDir)
	if cfg.Platform.VMManager.ImageDir == "" {
		images = "booting the guest image its release published"
	}
	dev := ""
	if ref, ok := cfg.Platform.DevImages[vmManagerMCPServer]; ok {
		dev = fmt.Sprintf(" running the dev image %s", ref)
	}
	return fmt.Sprintf("  vm-manager: the platform's VM provisioner as a pod of the node%s, %s, registered as x_%s_*\n"+
		"  through muster (tool group %s; the portal lists it under Agent Platform). Proof: `agentlab vm-manager-test`.",
		dev, images, vmManagerMCPServer, toolGroupAgentPlatform)
}
