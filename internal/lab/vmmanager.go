package lab

import (
	"context"
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/giantswarm/agentlab/internal/config"
)

// The vm-manager wiring (platform.vmManager in agentlab.yaml): the platform's
// VM provisioner (github.com/giantswarm/vm-manager) runs as a POD of the KVM
// node, the shape of its siblings agent-manager and model-manager — the
// agent-platform chart's components.vm-manager, turned on by the lab's values
// template. The kind node is a privileged docker container, so the host's
// /dev/kvm and /dev/vhost-vsock are in it, and the chart mounts them into
// the pod; the image directory a vm-manager checkout built (`make -C
// images`) reaches the pod through a kind extraMount of
// platform.vmManager.imageDir (kind-config.yaml.tmpl, fixed at `kind
// create`) and the chart's images.hostPath. A build of the checkout swaps in
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

// vmManagerImageMount is where the kind node sees platform.vmManager.imageDir
// (the extraMount's containerPath) and what the chart's images.hostPath
// names — a short path with no host-specific part, so the rendered values
// stay byte-identical across machines.
const vmManagerImageMount = "/var/lib/agentlab/vm-manager/images"

// vm-manager's own paths: the MCP endpoint (the chart's mcp.path) and the
// guarded capability report the proof calls.
const (
	vmManagerMCPPath  = "/mcp"
	vmManagerHostPath = "/api/v1/host"
)

// kvmDevices are the node devices the pod mounts from the node: without them
// vm-manager starts, reports them under `missing`, and create_vm cannot work.
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
// it, before the install would leave a pod stuck in ContainerCreating on a
// hostPath device the node lacks: the KVM devices on the host, the same
// devices inside the running node (a node created before the module was
// loaded has no /dev/vhost-vsock — the node's /dev is populated at `kind
// create`), and the image directory's mount into the node, which is fixed at
// `kind create` too.
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
	if dir := cfg.Platform.VMManager.ImageDir; dir != "" {
		if _, err := outputQuiet(dockerBin, "exec", node, "test", "-d", vmManagerImageMount); err != nil {
			return fmt.Errorf("the kind node %s has no %s: platform.vmManager.imageDir (%s) is mounted into the node when it is created, so a directory set afterwards is not there yet.\n  Fix: `agentlab down && agentlab up` (the pod then finds the images at %s)", node, vmManagerImageMount, dir, vmManagerImageMount)
		}
		note("the image directory %s is in the node at %s", dir, vmManagerImageMount)
	} else {
		note("platform.vmManager.imageDir is unset: the pod starts with an empty image directory, so list_images is empty and the proof boots no VM (`agentlab configure --vm-manager-image-dir <a vm-manager checkout's images/build>`, then `agentlab down && up`)")
	}
	return nil
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
		if errors.IsNotFound(err) {
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

// vmManagerURL is the MCP endpoint muster dials.
func vmManagerURL() string { return vmManagerServiceURL() + vmManagerMCPPath }

// vmManagerHint is the platform-up summary for the vm-manager wiring.
func vmManagerHint(cfg *config.Config) string {
	if !cfg.VMManagerEnabled() {
		return "  vm-manager is not wired (platform.vmManager in agentlab.yaml; `agentlab configure --vm-manager --vm-manager-image-dir <dir>` turns it on)."
	}
	images := fmt.Sprintf("the images of %s", cfg.Platform.VMManager.ImageDir)
	if cfg.Platform.VMManager.ImageDir == "" {
		images = "no image directory (platform.vmManager.imageDir)"
	}
	dev := ""
	if ref, ok := cfg.Platform.DevImages[vmManagerMCPServer]; ok {
		dev = fmt.Sprintf(" running the dev image %s", ref)
	}
	return fmt.Sprintf("  vm-manager: the platform's VM provisioner as a pod of the node%s, %s, registered as x_%s_*\n"+
		"  through muster (tool group %s; the portal lists it under Agent Platform). Proof: `agentlab vm-manager-test`.",
		dev, images, vmManagerMCPServer, toolGroupAgentPlatform)
}

// vmManagerImageMountFor is the chart's images.hostPath for this lab: the
// node path platform.vmManager.imageDir is mounted at, or "" without one
// (the pod then has an empty image directory).
func vmManagerImageMountFor(cfg *config.Config) string {
	if cfg.Platform.VMManager.ImageDir == "" {
		return ""
	}
	return vmManagerImageMount
}
