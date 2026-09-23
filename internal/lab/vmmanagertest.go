package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/agentlab/internal/config"
)

// VMManagerTestOptions tunes the vm-manager proof.
type VMManagerTestOptions struct {
	// SkipVM skips the VM lifecycle: the registration, the identity boundary
	// and the read tools are proven, no VM boots.
	SkipVM bool
	// VMTimeout bounds the boot of the proof's VM (installer boot plus the
	// installed boot to READY=1; about 30 s on a laptop).
	VMTimeout time.Duration
}

// The vm-manager tools the proof drives (its README's "API at a glance"),
// as muster names them: x_vm-manager_<tool>.
const (
	vmToolGetHost        = "get_host"
	vmToolListImages     = "list_images"
	vmToolListNetworks   = "list_networks"
	vmToolListVMs        = "list_vms"
	vmToolGetVM          = "get_vm"
	vmToolGetVMConsole   = "get_vm_console"
	vmToolGetAttestation = "get_vm_attestation"
	vmToolCreateVM       = "create_vm"
	vmToolDeleteVM       = "delete_vm"
)

// vmManagerCoreTools is the set the registration must surface: the reads a
// model calls first and the writes the lifecycle needs.
var vmManagerCoreTools = []string{vmToolGetHost, vmToolListImages, vmToolListNetworks, vmToolListVMs, vmToolGetVM,
	vmToolGetVMConsole, vmToolGetAttestation, vmToolCreateVM, "start_vm", "stop_vm", "reboot_vm", vmToolDeleteVM}

// vmManagerTestVMPrefix names the proof's throwaway VM; a leftover of an
// interrupted run is removed before a new one boots.
const vmManagerTestVMPrefix = "agentlab-vm-test"

// DefaultVMManagerTestVMTimeout is the proof's default VM boot budget: the
// installer boot (~12 s) plus the installed boot to READY=1 (11-15 s) with
// room for a cold host; vm-manager's own ceilings are 5 m and 4 m.
const DefaultVMManagerTestVMTimeout = 6 * time.Minute

// VM states of vm-manager's lifecycle the proof reads.
const (
	vmStateReady   = "ready"
	vmStateRunning = "running"
	vmStateFailed  = "failed"
	vmStateStopped = "stopped"
)

// vmHostInfo is the part of get_host / GET /api/v1/host the proof reads.
type vmHostInfo struct {
	Hostname    string      `json:"hostname"`
	Kernel      string      `json:"kernel"`
	CPUs        int         `json:"cpus"`
	MemoryBytes uint64      `json:"memoryBytes"`
	OVMFCode    string      `json:"ovmfCode"`
	Firmware    *vmFirmware `json:"firmware"`
	Ready       bool        `json:"ready"`
	Missing     []string    `json:"missing"`
}

// vmFirmware is get_host's build of the firmware the pod's VMs boot with,
// the build the golden value of PCR 0 belongs to (vm-manager 0.22.12 and later).
type vmFirmware struct {
	SHA256  string `json:"sha256"`
	Package string `json:"package"`
	Version string `json:"version"`
}

// firmware names the build the VMs boot with, for the notes and the
// golden-mismatch explanation.
func (h *vmHostInfo) firmware() string {
	if h == nil || h.Firmware == nil {
		return "a build this vm-manager does not report (get_host's `firmware` needs vm-manager 0.22.12 or later)"
	}
	sum := h.Firmware.SHA256
	if len(sum) > 12 {
		sum = sum[:12] + "…"
	}
	if h.Firmware.Package == "" {
		return fmt.Sprintf("%s (sha256 %s)", h.OVMFCode, sum)
	}
	return fmt.Sprintf("%s %s (%s, sha256 %s)", h.Firmware.Package, h.Firmware.Version, h.OVMFCode, sum)
}

// vmImage is the part of list_images the proof reads.
type vmImage struct {
	ID                 string          `json:"id"`
	Version            string          `json:"version"`
	KubernetesVersions []string        `json:"kubernetesVersions"`
	Policy             json.RawMessage `json:"policy"`
}

func (i vmImage) ref() string { return i.ID + "_" + i.Version }

// hasGolden reports whether the image's PCR policy carries golden firmware
// values: without them vm-manager's verifier rejects every quote, so a VM
// with require_attestation fails its boot by design until `vm-manager image
// golden` recorded them (the README's Attestation section).
func (i vmImage) hasGolden() bool {
	if len(i.Policy) == 0 {
		return false
	}
	var policy struct {
		Golden map[string]any `json:"golden"`
	}
	return json.Unmarshal(i.Policy, &policy) == nil && len(policy.Golden) > 0
}

// vmNetwork is the part of a network record the proof reads: the spec it
// was created from and the gateway the guests get.
type vmNetwork struct {
	Spec struct {
		Name       string `json:"name"`
		CIDR       string `json:"cidr"`
		EnableIMDS bool   `json:"enableIMDS"`
	} `json:"spec"`
	Gateway string `json:"gateway"`
}

// vmQuote is one attestation stage's verdict.
type vmQuote struct {
	Verified bool   `json:"verified"`
	Message  string `json:"message"`
	Learned  []int  `json:"learned"`
}

// vmRecord is the part of a VM record the proof reads.
type vmRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	State       string `json:"state"`
	IP          string `json:"ip"`
	LastError   string `json:"lastError"`
	Attestation struct {
		Required         bool     `json:"required"`
		UserDataReleased bool     `json:"userDataReleased"`
		Initrd           *vmQuote `json:"initrd"`
		Ready            *vmQuote `json:"ready"`
	} `json:"attestation"`
	CreatedAt   time.Time  `json:"createdAt"`
	InstalledAt *time.Time `json:"installedAt"`
	BootedAt    *time.Time `json:"bootedAt"`
	ReadyAt     *time.Time `json:"readyAt"`
}

// VMManagerTest is the headless proof of the vm-manager wiring: the
// platform's VM provisioner as a pod of the node, registered with muster by
// its chart, reached as the signed-in person.
//
// The identity boundary first, against the pod's Service from inside the
// cluster (a probe pod, the way muster dials it): vm-manager without a token
// answers 401 — a vm-manager running anonymously (oauth off) would make every
// forwarded identity meaningless, and the proof fails on it — and with the
// person's Dex id_token it answers the capability report, validated against
// the lab Dex through the chart's global.identity wiring. Then through
// muster: the x_vm-manager_* tools are aggregated with their
// annotations intact (get_host read-only, delete_vm destructive), get_host,
// list_images and list_networks answer as the person, and the MCPServer CR
// carries the agent-platform tool group and reads Connected. Then, unless
// --skip-vm, the lifecycle through muster: create_vm on the newest image,
// the states followed with get_vm until ready (the installer boot, the
// installed boot, READY=1 over vsock), the attestation verdicts read when
// the image's policy has golden values, the console tailed, and delete_vm
// leaving nothing behind. A host that is not ready (get_host's `missing`)
// or has no image skips the lifecycle with the reason.
func VMManagerTest(cfg *config.Config, email string, opts VMManagerTestOptions) error {
	if !cfg.VMManagerEnabled() {
		return fmt.Errorf("platform.vmManager is off in %s — `agentlab configure --vm-manager --vm-manager-image-dir <dir>` turns it on, then `agentlab platform` installs the component", config.File)
	}
	if err := useClusterKubeconfig(cfg); err != nil {
		return err
	}
	user := cfg.FindUser(email)
	if user == nil {
		return fmt.Errorf("no user %q in %s", email, config.File)
	}
	if opts.VMTimeout <= 0 {
		opts.VMTimeout = DefaultVMManagerTestVMTimeout
	}
	endpoint := vmManagerServiceURL()

	step("Calling vm-manager without a token from inside the cluster (GET %s%s)", endpoint, vmManagerHostPath)
	if err := proveVMManagerRefusesAnonymous(cfg); err != nil {
		return err
	}
	note("401 — vm-manager is an OAuth resource server; nothing answers without an identity")

	step("Logging in to Dex as %s", email)
	token, err := passwordGrant(cfg, config.AgentPlatformClientID, config.AgentPlatformClientSecret,
		user.Email, user.Password, musterLoginScopes)
	if err != nil {
		return err
	}
	note("got an id_token")

	step("vm-manager with the person's token, directly against the Service (the boundary muster's forwarding relies on)")
	direct, err := vmManagerHostDirect(cfg, token)
	if err != nil {
		return err
	}
	noteHost(direct)

	step("MCP tools through muster (x_%s_*)", vmManagerMCPServer)
	session, err := openMusterSession(cfg, token, "vm-manager-test")
	if err != nil {
		return err
	}
	prefix := "x_" + vmManagerMCPServer + "_"
	toolCount, err := proveVMManagerTools(session, prefix)
	if err != nil {
		return err
	}

	step("%s through muster as %s", vmToolGetHost, email)
	var viaMuster vmHostInfo
	if err := callVMTool(session, prefix+vmToolGetHost, nil, &viaMuster); err != nil {
		return err
	}
	if viaMuster.Hostname != direct.Hostname {
		return fmt.Errorf("get_host through muster reports host %q, the direct call %q — muster is not dialing the same vm-manager", viaMuster.Hostname, direct.Hostname)
	}
	note("the same host through muster: %s", viaMuster.Hostname)

	step("%s and %s through muster", vmToolListImages, vmToolListNetworks)
	var images []vmImage
	if err := callVMTool(session, prefix+vmToolListImages, nil, &images); err != nil {
		return err
	}
	for _, img := range images {
		golden := "no golden PCR values yet (`vm-manager image golden`)"
		if img.hasGolden() {
			golden = "golden PCR values recorded"
		}
		note("image %s — Kubernetes %s; %s", img.ref(), orNone(strings.Join(img.KubernetesVersions, ", ")), golden)
	}
	if len(images) == 0 {
		note("no image in vm-manager's image directory (platform.vmManager.imageDir: a vm-manager checkout's images/build after `make -C images`, mounted at `agentlab up`)")
	}
	var networks []vmNetwork
	if err := callVMTool(session, prefix+vmToolListNetworks, nil, &networks); err != nil {
		return err
	}
	if len(networks) == 0 {
		return fmt.Errorf("list_networks lists no network — vm-manager creates the default one at startup (--network-subnet)")
	}
	for _, n := range networks {
		note("network %s (%s, gateway %s, IMDS %v)", n.Spec.Name, n.Spec.CIDR, n.Gateway, n.Spec.EnableIMDS)
	}

	step("The registration: MCPServer %s carries %s=%s and reads Connected", vmManagerMCPServer, toolGroupLabel, toolGroupAgentPlatform)
	if err := proveVMManagerRegistration(); err != nil {
		return err
	}

	switch {
	case opts.SkipVM:
		note("VM lifecycle skipped (--skip-vm)")
	case !direct.Ready:
		note("VM lifecycle skipped: the host is not ready for create_vm — missing %s", strings.Join(direct.Missing, ", "))
	case len(images) == 0:
		note("VM lifecycle skipped: no image to boot")
	default:
		if err := proveVMLifecycle(session, prefix, images[0], direct, opts.VMTimeout); err != nil {
			return err
		}
	}

	fmt.Printf("PASS: the vm-manager pod (%s) refuses anonymous calls, answers %s's forwarded token, and muster aggregates x_%s_* (%d tools) for the person\n",
		direct.Hostname, email, vmManagerMCPServer, toolCount)
	return nil
}

// vmManagerDirect GETs the guarded capability report from inside the
// cluster — the Service the chart renders, dialed from a probe pod the way
// muster dials it — with the bearer token given (none for anonymous), and
// returns the status code and the body. The token is the lab's throwaway
// id_token and travels in the probe pod's command.
func vmManagerDirect(cfg *config.Config, name, token string) (int, string, error) {
	sideloadImages(cfg, hostPullImages([]string{probeImage}))
	ctx, cancel := context.WithTimeout(context.Background(), probePodTimeout)
	defer cancel()
	// -S prints the status line (to stderr, in the same log); `|| true` keeps
	// a 401 from failing the pod — the code is the answer, not an error.
	cmd := fmt.Sprintf("wget -q -S -O - -T %d", int(probeHeaderTimeout.Seconds())*5)
	if token != "" {
		cmd += " --header 'Authorization: Bearer " + token + "'"
	}
	cmd += " " + vmManagerServiceURL() + vmManagerHostPath + " 2>&1 || true"
	out, err := runProbePod(ctx, platformNamespace, name, probeImage, []string{"sh", "-c", cmd}, probePodTimeout)
	if err != nil {
		return 0, "", fmt.Errorf("probing vm-manager from inside the cluster: %w\n%.300s", err, strings.TrimSpace(out))
	}
	m := httpStatusRe.FindStringSubmatch(out)
	if m == nil {
		return 0, "", fmt.Errorf("vm-manager does not answer at %s from inside the cluster (no status line): is the pod running? `kubectl -n %s get pods -l app.kubernetes.io/name=%s`, `agentlab logs %s`\n  Probe output: %.300s",
			vmManagerServiceURL(), platformNamespace, vmManagerMCPServer, vmManagerMCPServer, strings.TrimSpace(out))
	}
	status, _ := strconv.Atoi(m[1])
	body := ""
	if i := strings.Index(out, "{"); i >= 0 {
		if j := strings.LastIndex(out, "}"); j > i {
			body = out[i : j+1]
		}
	}
	return status, body, nil
}

// httpStatusRe matches the status line wget -S prints; the last match is the
// final answer of a redirected request.
var httpStatusRe = regexp.MustCompile(`HTTP/1\.[01] (\d{3})`)

// proveVMManagerRefusesAnonymous GETs the guarded capability report with no
// token and wants the 401.
func proveVMManagerRefusesAnonymous(cfg *config.Config) error {
	status, body, err := vmManagerDirect(cfg, vmManagerMCPServer+"-test-anonymous", "")
	if err != nil {
		return err
	}
	switch status {
	case http.StatusUnauthorized:
		return nil
	case http.StatusOK:
		return fmt.Errorf("vm-manager answered the capability report WITHOUT a token: it runs anonymously (vm-manager.oauth.enabled is false), so every identity muster forwards is ignored.\n"+
			"  The meta chart's vm-manager block turns OAuth on; check the release's values: `kubectl -n %s get helmrelease %s -o yaml`", platformNamespace, vmManagerMCPServer)
	default:
		return fmt.Errorf("vm-manager answered %d to an anonymous call, want 401:\n%.300s", status, strings.TrimSpace(body))
	}
}

// vmManagerHostDirect GETs the capability report with the person's token —
// the very token muster forwards — and parses it.
func vmManagerHostDirect(cfg *config.Config, token string) (*vmHostInfo, error) {
	status, body, err := vmManagerDirect(cfg, vmManagerMCPServer+"-test-token", token)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("vm-manager refused the lab's Dex id_token (%d) — its OAuth settings do not match this lab "+
			"(issuer, CA, trusted audience: the chart reads global.identity; compare `kubectl -n %s get deploy %s -o yaml` with the lab's Dex):\n%.300s",
			status, platformNamespace, vmManagerMCPServer, strings.TrimSpace(body))
	}
	var info vmHostInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return nil, fmt.Errorf("parsing the capability report: %w\n%.300s", err, strings.TrimSpace(body))
	}
	return &info, nil
}

func noteHost(h *vmHostInfo) {
	ready := "ready for create_vm"
	if !h.Ready {
		ready = "NOT ready: missing " + strings.Join(h.Missing, ", ")
	}
	note("host %s — kernel %s, %d CPUs, %.1f GiB; %s", h.Hostname, h.Kernel, h.CPUs, float64(h.MemoryBytes)/(1<<30), ready)
	note("firmware the VMs boot with: %s — the build golden PCR 0 is recorded for", h.firmware())
}

// proveVMManagerTools asserts muster aggregates the core tool set under the
// server's prefix and forwards vm-manager's annotations: get_host read-only,
// delete_vm destructive — what a model reads before it calls. It returns how
// many tools the server surfaces.
func proveVMManagerTools(session *musterSession, prefix string) (int, error) {
	names, err := session.listTools()
	if err != nil {
		return 0, err
	}
	var found []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			found = append(found, strings.TrimPrefix(n, prefix))
		}
	}
	if len(found) == 0 {
		return 0, fmt.Errorf("muster aggregates no %s* tools; check `kubectl -n %s get mcpservers.muster.giantswarm.io %s` and `agentlab logs muster`",
			prefix, platformNamespace, vmManagerMCPServer)
	}
	var missing []string
	for _, want := range vmManagerCoreTools {
		if !slices.Contains(found, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("muster aggregates %d %s* tools but not %s (an older vm-manager?)", len(found), prefix, strings.Join(missing, ", "))
	}
	note("%d tools under %s (%s, …)", len(found), prefix, strings.Join(vmManagerCoreTools[:4], ", "))

	getHost, err := session.describeTool(prefix + vmToolGetHost)
	if err != nil {
		return 0, err
	}
	if getHost.Annotations == nil || !getHost.Annotations.readOnly() {
		return 0, fmt.Errorf("%s%s is not annotated read-only through muster (annotations: %+v)", prefix, vmToolGetHost, getHost.Annotations)
	}
	deleteVM, err := session.describeTool(prefix + vmToolDeleteVM)
	if err != nil {
		return 0, err
	}
	if deleteVM.Annotations == nil || deleteVM.Annotations.DestructiveHint == nil || !*deleteVM.Annotations.DestructiveHint {
		return 0, fmt.Errorf("%s%s is not annotated destructive through muster (annotations: %+v)", prefix, vmToolDeleteVM, deleteVM.Annotations)
	}
	note("annotations survive muster: %s read-only, %s destructive", vmToolGetHost, vmToolDeleteVM)
	return len(found), nil
}

// callVMTool runs one x_vm-manager_* tool through muster and decodes its
// JSON payload into out (nil to discard).
func callVMTool(session *musterSession, name string, args map[string]any, out any) error {
	text, err := session.callServerTool(name, args)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("parsing %s payload: %w\n%.300s", name, err, text)
	}
	return nil
}

// proveVMManagerRegistration reads the MCPServer CR: the tool-group label
// that puts it under Agent Platform on the portal's servers page, and the
// state — Connected now that a session called it.
func proveVMManagerRegistration() error {
	ctx, cancel := context.WithTimeout(context.Background(), kubeReadTimeout)
	defer cancel()
	cr, err := getMCPServer(ctx, platformNamespace, vmManagerMCPServer)
	if err != nil {
		return err
	}
	if v := cr.Labels[toolGroupLabel]; v != toolGroupAgentPlatform {
		return fmt.Errorf("MCPServer %s carries %s=%q, want %s", vmManagerMCPServer, toolGroupLabel, v, toolGroupAgentPlatform)
	}
	if err := waitMCPServerState(vmManagerMCPServer, "Connected"); err != nil {
		return err
	}
	return nil
}

// proveVMLifecycle boots one VM through muster and removes it: create_vm on
// the image, the states followed with get_vm until ready, the attestation
// read when the policy has golden values, the console tailed, delete_vm and
// list_vms without it. On any failure the VM is deleted before returning.
func proveVMLifecycle(session *musterSession, prefix string, img vmImage, host *vmHostInfo, timeout time.Duration) error {
	if err := removeStaleTestVMs(session, prefix); err != nil {
		return err
	}
	attest := img.hasGolden()
	name := fmt.Sprintf("%s-%s", vmManagerTestVMPrefix, randHex(3))
	how := "attestation required (the policy has golden PCR values)"
	if !attest {
		how = "attestation NOT required: the image's policy has no golden values yet (`vm-manager image golden` records them)"
	}
	step("%s %s on image %s (1 CPU, 1 GiB, 8 GiB disk) — %s", vmToolCreateVM, name, img.ref(), how)
	var created vmRecord
	if err := callVMTool(session, prefix+vmToolCreateVM, map[string]any{
		"name": name, "image": img.ref(), "cpus": 1, "memory_mib": 1024, "disk_gib": 8,
		"require_attestation": attest, "wait_for": "none",
		"metadata": map[string]string{"agentlab-proof": "vm-manager-test"}, // keys take no slashes
	}, &created); err != nil {
		return err
	}
	note("VM %s created (state %s)", created.ID, created.State)
	deleted := false
	defer func() {
		if deleted {
			return
		}
		if err := callVMTool(session, prefix+vmToolDeleteVM, map[string]any{"id": created.ID}, nil); err != nil {
			note("cleanup: %s %s failed: %v", vmToolDeleteVM, created.ID, err)
		}
	}()

	step("Following the boot with %s (installer boot -> installed boot -> READY=1 over vsock; up to %s)", vmToolGetVM, timeout)
	vm, err := followVMBoot(session, prefix, created.ID, timeout)
	if err != nil {
		return err
	}
	install, boot := "?", "?"
	if vm.InstalledAt != nil {
		install = vm.InstalledAt.Sub(vm.CreatedAt).Round(time.Second).String()
	}
	if vm.ReadyAt != nil && vm.BootedAt != nil {
		boot = vm.ReadyAt.Sub(*vm.BootedAt).Round(time.Second).String()
	}
	note("ready: ip %s — install %s, installed boot to READY=1 %s", vm.IP, install, boot)

	if attest {
		step("%s: both stages verified", vmToolGetAttestation)
		if err := proveVMAttestation(session, prefix, created.ID, img, host); err != nil {
			return err
		}
	}

	step("%s (the last lines of the serial console)", vmToolGetVMConsole)
	var console struct {
		Console string `json:"console"`
	}
	if err := callVMTool(session, prefix+vmToolGetVMConsole, map[string]any{"id": created.ID, "lines": 5}, &console); err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(console.Console), "\n") {
		if line = strings.TrimSpace(stripANSI(line)); line != "" {
			note("  | %.160s", line)
		}
	}

	step("%s %s", vmToolDeleteVM, created.ID)
	if err := callVMTool(session, prefix+vmToolDeleteVM, map[string]any{"id": created.ID}, nil); err != nil {
		return err
	}
	deleted = true
	var vms []vmRecord
	if err := callVMTool(session, prefix+vmToolListVMs, nil, &vms); err != nil {
		return err
	}
	for _, v := range vms {
		if v.ID == created.ID {
			return fmt.Errorf("%s still lists %s after %s", vmToolListVMs, created.ID, vmToolDeleteVM)
		}
	}
	note("gone: disk, vTPM state, lease and record removed")
	return nil
}

// followVMBoot polls get_vm until the VM is ready, printing every state
// change; failed (with the installer's console tail in lastError), running
// (no READY=1 within vm-manager's boot timeout) and the proof's own timeout
// are errors that carry the VM's own words.
func followVMBoot(session *musterSession, prefix, id string, timeout time.Duration) (*vmRecord, error) {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var vm vmRecord
		if err := callVMTool(session, prefix+vmToolGetVM, map[string]any{"id": id}, &vm); err != nil {
			return nil, err
		}
		if vm.State != last {
			note("state %s", vm.State)
			last = vm.State
		}
		switch vm.State {
		case vmStateReady:
			return &vm, nil
		case vmStateFailed, vmStateStopped:
			return nil, fmt.Errorf("VM %s ended %s: %s", id, vm.State, orNone(vm.LastError))
		case vmStateRunning:
			return nil, fmt.Errorf("VM %s is running but never sent READY=1 (vm-manager's --boot-timeout passed): %s", id, orNone(vm.LastError))
		}
		if time.Now().After(deadline) {
			var console struct {
				Console string `json:"console"`
			}
			_ = callVMTool(session, prefix+vmToolGetVMConsole, map[string]any{"id": id, "lines": 20}, &console)
			return nil, fmt.Errorf("VM %s did not reach ready within %s (last state %s)\n  console:\n%s", id, timeout, last, prefixLines(console.Console, "    | "))
		}
		time.Sleep(3 * time.Second)
	}
}

// proveVMAttestation reads the current boot's attestation and wants both
// quotes verified — the initrd stage that gates user-data and the ready
// stage with PCR 13.
func proveVMAttestation(session *musterSession, prefix, id string, img vmImage, host *vmHostInfo) error {
	var att struct {
		Required         bool     `json:"required"`
		UserDataReleased bool     `json:"userDataReleased"`
		Initrd           *vmQuote `json:"initrd"`
		Ready            *vmQuote `json:"ready"`
	}
	if err := callVMTool(session, prefix+vmToolGetAttestation, map[string]any{"id": id}, &att); err != nil {
		return err
	}
	for _, stage := range []struct {
		name  string
		quote *vmQuote
	}{{"initrd", att.Initrd}, {"ready", att.Ready}} {
		if stage.quote == nil {
			return fmt.Errorf("attestation: no %s quote recorded", stage.name)
		}
		if !stage.quote.Verified {
			return fmt.Errorf("attestation: the %s quote did not verify: %s", stage.name, explainQuoteVerdict(orNone(stage.quote.Message), img.ref(), host.firmware()))
		}
	}
	note("initrd and ready quotes verified; user-data released: %v", att.UserDataReleased)
	return nil
}

// goldenMismatchRe matches the verifier's verdict on a PCR whose quoted value
// differs from the golden value the image's policy.json carries.
var goldenMismatchRe = regexp.MustCompile(`golden mismatch: pcr (\d+)`)

// explainQuoteVerdict adds to the verifier's words what a golden mismatch
// means and what fixes it. The PCR names what changed under the recorded
// values: the firmware PCRs are the pod's OVMF — the build get_host reports,
// pinned per vm-manager release, so a release that changes it changes them
// while the guest image and its digest stay the same — and PCR 4 and 13 are
// the guest image's. Either way the policy's golden values were recorded for
// another build and are recorded again (docs/vm-manager.md). Any other
// verdict is returned as it is.
func explainQuoteVerdict(message, image, firmware string) string {
	m := goldenMismatchRe.FindStringSubmatch(message)
	if m == nil {
		return message
	}
	var changed string
	switch m[1] {
	case "0", "2", "3", "6", "7":
		changed = "PCR " + m[1] + " is measured by the firmware: the OVMF of the vm-manager pod image, " + firmware + ". vm-manager pins it; a release that changes it says `re-record golden PCRs` in its notes, with the same guest image"
	case "4":
		changed = "PCR 4 measures the boot loader and the UKI: the guest image changed"
	case "13":
		changed = "PCR 13 measures the Kubernetes sysext: the guest image or its Kubernetes version changed"
	default:
		changed = "PCR " + m[1] + " differs from the recorded value"
	}
	return message + "\n  " + changed + ".\n  The image policy's golden values were recorded for another build: record them again on this pod — `vm-manager image golden " + image + " --clear`, one learn-mode boot, `vm-manager image golden " + image + " --from-vm <id>` (docs/vm-manager.md \"Recording the image's golden PCR values\")."
}

// removeStaleTestVMs deletes VMs an interrupted run left behind.
func removeStaleTestVMs(session *musterSession, prefix string) error {
	var vms []vmRecord
	if err := callVMTool(session, prefix+vmToolListVMs, nil, &vms); err != nil {
		return err
	}
	for _, v := range vms {
		if !strings.HasPrefix(v.Name, vmManagerTestVMPrefix+"-") {
			continue
		}
		note("removing the stale proof VM %s (%s)", v.Name, v.ID)
		if err := callVMTool(session, prefix+vmToolDeleteVM, map[string]any{"id": v.ID}, nil); err != nil {
			return err
		}
	}
	return nil
}

// prefixLines prefixes every line of s, ANSI sequences removed.
func prefixLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + stripANSI(l)
	}
	return strings.Join(lines, "\n")
}

// ansiRe matches the escape sequences a serial console carries: CSI (colour,
// cursor), OSC (the terminal's title and semantic prompt marks, ended by BEL
// or ST) and the two-character ones.
var ansiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// stripANSI removes terminal escape sequences from a console line.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }
