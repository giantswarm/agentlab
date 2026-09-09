package lab

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/giantswarm/agentlab/internal/config"
)

// The lab runs the whole platform on ONE kind node, and the kube-scheduler
// refuses a pod whose CPU request does not fit that node. Nothing degrades:
// a docker VM with two CPUs leaves agentgateway (100m) Pending forever, and
// the platform install waits on a workload that will never start until helm
// gives up; with the platform up but the node full, models-test gets as far
// as the agent turn and fails there, because the agent's own pod cannot be
// scheduled. Nothing along the way says "out of CPU". preflightRuntimeResources
// asks the container runtime what it has BEFORE any cluster work and refuses
// (CPU, a hard limit) or warns (memory, a soft one) with the fix — in the
// spirit of preflightHostServer: a host-side shortfall fails here, with its
// remedy, not after a five-minute boot and a ten-minute install wait.
//
// The figures below are the requests `kubectl describe node` reports for each
// group of pods, measured on a live full default lab (agents, observability,
// Backstage, model-manager) on 2026-09-08: 2540m / 2532Mi allocated in total.
// docs/getting-started.md "Docker resources" is the human copy of these constants — its
// table lists the same groups — so a change here is a change there.

// resourceRequests is what a group of pods asks the scheduler for: CPU in
// millicores, memory in MiB — the units describe node uses.
type resourceRequests struct {
	CPU int
	Mem int
}

// add returns the sum of two request pairs.
func (r resourceRequests) add(o resourceRequests) resourceRequests {
	return resourceRequests{r.CPU + o.CPU, r.Mem + o.Mem}
}

// resourceGroup is one group of pods the lab may schedule, with what it
// requests and the configuration that installs it.
type resourceGroup struct {
	Name     string
	Requests resourceRequests
	Enabled  func(*config.Config) bool
}

// The request figures, one constant per group (see the package comment for
// where they were measured).
const (
	// kind's static pods: kube-apiserver 250m, controller-manager 200m,
	// scheduler 100m, etcd 100m/100Mi, kindnet 100m/50Mi, two CoreDNS at
	// 100m/70Mi. Always there — it is the node.
	reqKindControlPlaneCPU = 950
	reqKindControlPlaneMem = 290
	// The lab's own Dex. Always deployed by Up.
	reqDexCPU = 50
	reqDexMem = 64
	// The umbrella chart without its optional parts: muster 100m/128Mi and
	// its valkey 150m/192Mi, agentgateway 100m/128Mi and its controller
	// 50m/128Mi, mcp-kubernetes 105m/144Mi, agent-manager 55m/80Mi.
	reqPlatformCoreCPU = 510
	reqPlatformCoreMem = 736
	// The agents runtime: kagent controller 100m/128Mi, UI 100m/256Mi, its
	// bundled PostgreSQL 250m/256Mi.
	reqAgentsRuntimeCPU = 450
	reqAgentsRuntimeMem = 640
	// model-manager in front of the host model servers.
	reqModelManagerCPU = 55
	reqModelManagerMem = 80
	// Backstage requests almost nothing and uses several times its 250Mi;
	// the memory floor below accounts for that with a factor, not here.
	reqBackstageCPU = 20
	reqBackstageMem = 250
	// The chart's bundled Flux engine (the lab shape): the Flux Operator plus
	// the FluxInstance's source-controller and helm-controller — the delivery
	// engine of every platform component, so it is always part of the platform.
	// Measured on the lab 2026-09-08 (agent-platform 3.20.0, flux-engine 0.1.0): flux-operator 100m/64Mi, helm-controller 100m/64Mi, source-controller 50m/64Mi.
	reqFluxCPU = 250
	reqFluxMem = 192
	// observability: kube-state-metrics 200m/200Mi, mcp-prometheus
	// 105m/144Mi. The Prometheus server, the operator and node-exporter
	// declare no requests at all, which is one reason real memory use runs
	// so far above this column.
	reqObservabilityCPU = 305
	reqObservabilityMem = 344

	// runtimeHeadroomCPU is what the CPU floor keeps free beyond the
	// requests for the pods the platform creates AFTER the boot: every kagent
	// agent is one more pod, and models-test and agents-test each create one.
	// Six of them at the 100m the platform's own small pods request; without
	// this room a lab that boots fine cannot run a single agent.
	runtimeHeadroomCPU = 600

	// memRealUseFactor is how far real memory use runs above the requests
	// column on a live lab: about twice (docs/getting-started.md "Docker resources" — a
	// platform+agents node sat at 2.4 GiB of real use against 1.8 GiB of
	// requests, and the full lab's Backstage and Prometheus use several
	// times what they declare). The memory floor is the requests times this.
	memRealUseFactor = 2
)

// labResourceGroups lists every group in boot order, gated the way Up and
// platformUp install them. Groups that hang off the platform are inert when
// the platform itself is disabled, like their config fields.
var labResourceGroups = []resourceGroup{
	{"kind control plane", resourceRequests{reqKindControlPlaneCPU, reqKindControlPlaneMem},
		func(*config.Config) bool { return true }},
	{"Dex", resourceRequests{reqDexCPU, reqDexMem},
		func(*config.Config) bool { return true }},
	{"agent platform", resourceRequests{reqPlatformCoreCPU, reqPlatformCoreMem},
		func(c *config.Config) bool { return c.Platform.Enabled }},
	{"agents runtime", resourceRequests{reqAgentsRuntimeCPU, reqAgentsRuntimeMem},
		func(c *config.Config) bool { return c.Platform.Enabled && c.Platform.Agents }},
	{"model-manager", resourceRequests{reqModelManagerCPU, reqModelManagerMem},
		func(c *config.Config) bool { return c.ModelManagerEnabled() }},
	{"Backstage", resourceRequests{reqBackstageCPU, reqBackstageMem},
		func(c *config.Config) bool { return c.Platform.Enabled && c.Backstage.Enabled }},
	{"Flux engine", resourceRequests{reqFluxCPU, reqFluxMem},
		func(c *config.Config) bool { return c.Platform.Enabled }},
	{"observability", resourceRequests{reqObservabilityCPU, reqObservabilityMem},
		func(c *config.Config) bool { return c.Platform.Enabled && c.Platform.Observability }},
}

// resourceNeeds is what one configuration asks of the container runtime.
type resourceNeeds struct {
	// Requests is the sum over the enabled groups: what the boot schedules.
	Requests resourceRequests
	// Groups names the enabled groups with their requests, for the
	// breakdown a refusal prints.
	Groups []resourceGroup
	// MinCPUs is the hard floor: the requests plus the run-time headroom,
	// rounded up to whole CPUs — the unit docker is given them in, and the
	// node's allocatable CPU is exactly the runtime's count.
	MinCPUs int
	// MinMemMiB is the soft floor: the requests times memRealUseFactor.
	// Below Requests.Mem the pods do not even schedule; between the two
	// they schedule and then get evicted or OOM-killed under real use.
	MinMemMiB int
}

// labResourceNeeds computes what cfg asks of the runtime from the enabled
// groups — never one hard-coded number, so a lab without Backstage and
// observability is held to its own, smaller floor.
func labResourceNeeds(cfg *config.Config) resourceNeeds {
	var n resourceNeeds
	for _, g := range labResourceGroups {
		if !g.Enabled(cfg) {
			continue
		}
		n.Groups = append(n.Groups, g)
		n.Requests = n.Requests.add(g.Requests)
	}
	n.MinCPUs = ceilCPUs(n.Requests.CPU + runtimeHeadroomCPU)
	n.MinMemMiB = n.Requests.Mem * memRealUseFactor
	return n
}

// ceilCPUs rounds millicores up to whole CPUs.
func ceilCPUs(millicores int) int {
	return (millicores + 999) / 1000
}

// smallerLabFlags is the configuration a refused boot is pointed at: the
// platform and its agents without Backstage and observability — the Docker
// resources page's "platform + agents only" row. Empty when cfg is already that (or smaller),
// so the message never suggests a change that changes nothing.
func smallerLabFlags(cfg *config.Config) string {
	if !cfg.Platform.Enabled || (!cfg.Backstage.Enabled && !cfg.Platform.Observability) {
		return ""
	}
	return "agentlab configure --backstage=false --observability=false"
}

// smallerLab is cfg with Backstage and observability off — what
// smallerLabFlags configures — for quoting its floor next to the flags.
func smallerLab(cfg *config.Config) *config.Config {
	c := *cfg
	c.Backstage.Enabled = false
	c.Platform.Observability = false
	return &c
}

// runtimeResources asks the container runtime behind `docker` for its CPU
// count and total memory in bytes — the VM's under Docker Desktop, Colima or
// a podman machine, the host's own under a native engine. Docker's `info`
// document has them at the top level (NCPU, MemTotal); podman's
// docker-compatible CLI answers `docker info` with podman's own document,
// where they live under Host (CPUs, MemTotal), and each engine's template
// engine errors on the other's field names. The engine's shape is tried
// first (dockerIsPodman), the other one second — a docker CLI in front of a
// podman API socket, and any future divergence, still get an answer.
func runtimeResources() (int, int64, error) {
	templates := []string{dockerInfoResourcesTemplate, podmanInfoResourcesTemplate}
	if dockerIsPodman() {
		templates[0], templates[1] = templates[1], templates[0]
	}
	var firstErr error
	for _, tmpl := range templates {
		out, probeErr := outputQuiet("docker", "info", "--format", tmpl)
		if probeErr == nil {
			cpus, memBytes, parseErr := parseRuntimeResources(out)
			if parseErr == nil {
				return cpus, memBytes, nil
			}
			probeErr = parseErr
		}
		if firstErr == nil {
			firstErr = probeErr
		}
	}
	return 0, 0, firstErr
}

// The two `docker info --format` templates for CPU count and memory bytes.
const (
	dockerInfoResourcesTemplate = "{{.NCPU}} {{.MemTotal}}"
	podmanInfoResourcesTemplate = "{{.Host.CPUs}} {{.Host.MemTotal}}"
)

// parseRuntimeResources reads the two fields the templates above print:
// "24 92417925120". Anything else — a template that hit the wrong engine
// ("<no value> <no value>"), an empty answer, a zero — is an error, and the
// caller degrades to a warning: an unreadable runtime is not a small one.
func parseRuntimeResources(out string) (cpus int, memBytes int64, err error) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected `docker info` answer %q, want \"<cpus> <memory bytes>\"", strings.TrimSpace(out))
	}
	cpus, err = strconv.Atoi(fields[0])
	if err != nil || cpus <= 0 {
		return 0, 0, fmt.Errorf("unexpected CPU count %q in `docker info` answer", fields[0])
	}
	memBytes, err = strconv.ParseInt(fields[1], 10, 64)
	if err != nil || memBytes <= 0 {
		return 0, 0, fmt.Errorf("unexpected memory total %q in `docker info` answer", fields[1])
	}
	return cpus, memBytes, nil
}

// runtimeMeasure is what the probe found, plus where it found it: which
// engine, on which OS, with how many CPUs the host itself has — the facts
// the fix text is built from.
type runtimeMeasure struct {
	CPUs     int
	MemMiB   int
	Podman   bool
	GOOS     string
	HostCPUs int
}

// engine names the runtime the way the user knows it.
func (m runtimeMeasure) engine() string {
	if m.Podman {
		return "podman"
	}
	return "docker"
}

// onHost reports whether the runtime IS this machine — a native engine on
// Linux (docker's daemon, or rootless podman running the containers in the
// user's own namespaces) — rather than a VM the user can resize. A VM on a
// Linux host with exactly the host's CPU count is misread as native, which
// only costs the resize hints; the smaller-lab alternative is always printed.
func (m runtimeMeasure) onHost() bool {
	return m.GOOS == "linux" && m.CPUs == m.HostCPUs
}

// judgeRuntimeResources compares what the runtime has with what the lab
// needs: an error below the CPU floor or below the memory requests (neither
// schedules), a warning between the memory requests and the memory floor
// (schedules, then starves), and otherwise a note only when the run-time
// pods have less than one CPU to themselves. Pure, so every branch is
// unit-tested without a container runtime.
func judgeRuntimeResources(m runtimeMeasure, cfg *config.Config, needs resourceNeeds) (warning string, err error) {
	if m.CPUs < needs.MinCPUs {
		return "", fmt.Errorf("%s has %d CPUs; this lab configuration needs %d.\n"+
			"  Its pods request %s CPUs of the single kind node, plus room for the pods the\n"+
			"  platform creates at run time, and the kube-scheduler refuses a pod whose CPU\n"+
			"  request does not fit: the boot would wait on an agentgateway that stays\n"+
			"  Pending forever (docs/getting-started.md \"Docker resources\"). The requests:\n%s\n%s",
			m.engine(), m.CPUs, needs.MinCPUs, fmtCPUs(needs.Requests.CPU),
			wrap(requestsBreakdown(needs), 84, "    "), resourceFixes(m, cfg, needs))
	}
	if m.MemMiB < needs.Requests.Mem {
		return "", fmt.Errorf("%s has %s of memory; this lab configuration needs %s.\n"+
			"  Its pods request %s of the single kind node, which does not fit, so they do not\n"+
			"  even all schedule — and real use runs about %dx the requests (docs/getting-started.md \"Docker resources\").\n%s",
			m.engine(), fmtGiB(m.MemMiB), fmtGiB(needs.MinMemMiB), fmtGiB(needs.Requests.Mem),
			memRealUseFactor, resourceFixes(m, cfg, needs))
	}
	if m.MemMiB < needs.MinMemMiB {
		warning = fmt.Sprintf("%s has %s of memory; this lab configuration wants %s.\n"+
			"    Its pods request only %s, so they schedule — but real use runs about %dx the requests\n"+
			"    (Backstage and Prometheus use several times what they declare): expect evictions\n"+
			"    and OOM kills under load (docs/getting-started.md \"Docker resources\").\n%s",
			m.engine(), fmtGiB(m.MemMiB), fmtGiB(needs.MinMemMiB), fmtGiB(needs.Requests.Mem), memRealUseFactor,
			indent(resourceFixes(m, cfg, needs), "  "))
	}
	return warning, nil
}

// requestsBreakdown lists the enabled groups with their CPU requests, so a
// refusal shows where the number comes from: "kind control plane 0.95, Dex
// 0.05, agent platform 0.51, ...". The names are the Docker resources table's rows.
func requestsBreakdown(needs resourceNeeds) string {
	parts := make([]string, 0, len(needs.Groups))
	for _, g := range needs.Groups {
		parts = append(parts, g.Name+" "+fmtCPUs(g.Requests.CPU))
	}
	return strings.Join(parts, ", ")
}

// resourceFixes is the "what to change" tail of a refusal or warning: the
// resize step for the runtime the user has, and the smaller lab that fits.
func resourceFixes(m runtimeMeasure, cfg *config.Config, needs resourceNeeds) string {
	var b strings.Builder
	cpus, memGiB := needs.MinCPUs, ceilGiB(needs.MinMemMiB)
	switch {
	case m.onHost() && m.Podman:
		fmt.Fprintf(&b, "  Rootless Podman on Linux runs the containers on this machine itself, so there is\n"+
			"  no VM to resize: this host has %d CPUs and %s of memory.\n", m.HostCPUs, fmtGiB(m.MemMiB))
	case m.onHost():
		fmt.Fprintf(&b, "  docker here is the host's own engine, so there is no VM to resize: this machine\n"+
			"  has %d CPUs and %s of memory.\n", m.HostCPUs, fmtGiB(m.MemMiB))
	default:
		fmt.Fprintf(&b, "  Give %s %d CPUs and %d GiB of memory:\n", m.engine(), cpus, memGiB)
		if m.Podman {
			fmt.Fprintf(&b, "  - podman machine: podman machine set --cpus %d --memory %d, then podman machine stop && podman machine start\n", cpus, memGiB*1024)
		} else {
			fmt.Fprintf(&b, "  - Docker Desktop: Settings -> Resources -> CPUs / Memory\n")
			fmt.Fprintf(&b, "  - Colima:         colima stop && colima start --cpu %d --memory %d\n", cpus, memGiB)
		}
	}
	if flags := smallerLabFlags(cfg); flags != "" {
		small := labResourceNeeds(smallerLab(cfg))
		fmt.Fprintf(&b, "  Or run the smaller lab, the platform and its agents without Backstage and observability\n"+
			"  (needs %d CPUs and %s):  %s", small.MinCPUs, fmtGiB(small.MinMemMiB), flags)
	} else {
		b.WriteString("  This is already the smaller lab (no Backstage, no observability); it needs a bigger machine.")
	}
	return b.String()
}

// preflightRuntimeResources is the boot's second check, after the Helm
// version and before any cluster work: what the container runtime has
// against what this configuration schedules. The measured values and the
// requests print on every boot, so the numbers are in every boot log. A
// probe that fails (no docker on PATH is caught later by kind, with its own
// message; an engine answering something unexpected) degrades to a note and
// lets the boot go on — an unreadable runtime is not a small one.
func preflightRuntimeResources(cfg *config.Config) error {
	needs := labResourceNeeds(cfg)
	step("Checking the container runtime's CPUs and memory against this lab's requests")
	cpus, memBytes, err := runtimeResources()
	if err != nil {
		note("could not read them (%v); going on. This lab requests %s CPUs / %s and needs %d CPUs / %s (docs/getting-started.md \"Docker resources\")",
			err, fmtCPUs(needs.Requests.CPU), fmtGiB(needs.Requests.Mem), needs.MinCPUs, fmtGiB(needs.MinMemMiB))
		return nil
	}
	m := runtimeMeasure{
		CPUs:     cpus,
		MemMiB:   int(memBytes / (1024 * 1024)),
		Podman:   dockerIsPodman(),
		GOOS:     runtime.GOOS,
		HostCPUs: runtime.NumCPU(),
	}
	note("%s: %d CPUs, %s memory; the lab requests %s CPUs / %s and needs %d CPUs / %s",
		m.engine(), m.CPUs, fmtGiB(m.MemMiB), fmtCPUs(needs.Requests.CPU), fmtGiB(needs.Requests.Mem), needs.MinCPUs, fmtGiB(needs.MinMemMiB))
	warning, err := judgeRuntimeResources(m, cfg, needs)
	if err != nil {
		return err
	}
	if warning != "" {
		warn("%s", warning)
	}
	// Above the floor but with less than a CPU for the run-time pods: say so,
	// so a models-test that later fails at the agent turn has its cause on
	// record in the boot log.
	if spare := m.CPUs*1000 - needs.Requests.CPU; spare < 1000 {
		note("tight: %s CPUs left for the pods the platform creates at run time (every kagent agent is one; models-test and agents-test create one each)", fmtCPUs(spare))
	}
	return nil
}

// fmtCPUs prints millicores as CPUs with two decimals, rounding half up in
// integers (305m is "0.31", which float formatting would make "0.30"):
// 2540 -> "2.54".
func fmtCPUs(millicores int) string {
	hundredths := (millicores + 5) / 10
	return fmt.Sprintf("%d.%02d", hundredths/100, hundredths%100)
}

// fmtGiB prints MiB as GiB with one decimal, rounding half up in integers:
// 2532 -> "2.5 GiB".
func fmtGiB(mib int) string {
	tenths := (mib*10 + 512) / 1024
	return fmt.Sprintf("%d.%d GiB", tenths/10, tenths%10)
}

// wrap breaks text at spaces into lines of at most width characters, each
// prefixed with indent — for the group breakdown, which is one long clause.
func wrap(text string, width int, indent string) string {
	var b strings.Builder
	line := indent
	for _, word := range strings.Fields(text) {
		if len(line) > len(indent) && len(line)+1+len(word) > width {
			b.WriteString(line + "\n")
			line = indent
		}
		if len(line) > len(indent) {
			line += " "
		}
		line += word
	}
	b.WriteString(line)
	return b.String()
}

// ceilGiB rounds MiB up to whole GiB — the unit the resize commands take.
func ceilGiB(mib int) int {
	return (mib + 1023) / 1024
}

// indent prefixes every line of s.
func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
