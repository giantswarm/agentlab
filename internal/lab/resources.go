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
// as the agent turn and fails there. Nothing along the way says "out of CPU".
// preflightRuntimeResources asks the container runtime what it has BEFORE any
// cluster work and refuses (CPU, a hard limit) or warns (memory, a soft one)
// with the fix — in the spirit of preflightHostServer: a host-side shortfall
// fails here, with its remedy, not after a five-minute boot and a ten-minute
// install wait.
//
// Two columns per group of pods, both measured on a live lab: what the group
// REQUESTS of the scheduler (`kubectl describe node`'s Allocated resources —
// the hard column: a request that does not fit never schedules) and what it
// USES (the containers' memory working set, summed from the kubelet's
// cAdvisor metrics — the column RAM is actually spent on; several groups
// declare far less than they use, Substrate's WorkerPool reserves far more).
// The 4.x line was measured on 2026-09-11 (agent-platform 4.7.11: kagent
// 0.11.0-gs.3, Substrate 0.0.27-gs.5, the full default lab with model-manager
// on kind v1.37.0, a node up for 19 hours): 3340m / 4388Mi requested — the
// node's Allocated resources read exactly that — about 4.4 GiB in use.
// docs/getting-started.md "Docker resources" is the human
// copy of these constants — its table lists the same groups — so a change
// here is a change there.

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

// platformTopology is what the chart the lab installs brings along with the
// components agentlab.yaml switches on — read off the chart's rendered roster
// (platformRoster), never off a version string, so a lab on a chart that
// ships less budgets less.
type platformTopology struct {
	// Substrate: the chart delivers Agent Substrate (the 4.x line, with
	// kagent on) — the control plane in ate-system and the WorkerPool's
	// gVisor workers, every agent an actor on one of them.
	Substrate bool
	// CNPG: the chart delivers the CloudNativePG operator and, with it, the
	// platform's Postgres Cluster in the kagent namespace.
	CNPG bool
}

// topologyOf reads the topology off a rendered roster; a nil roster (the
// render failed) is the line the lab is verified with — the 4.x line ships
// both with the agents — so a boot that cannot render still budgets for
// what the default chart installs.
func topologyOf(cfg *config.Config, roster *platformRoster) platformTopology {
	if roster == nil {
		agents := cfg.Platform.Enabled && cfg.Platform.Agents
		return platformTopology{Substrate: agents, CNPG: agents}
	}
	return platformTopology{Substrate: roster.shipsSubstrate(), CNPG: roster.shipsCNPG()}
}

// resourceGroup is one group of pods the lab may schedule: what it requests,
// what it was measured to use (memory only — CPU use idles near zero and
// bursts are the headroom's business), and the configuration and topology
// that install it.
type resourceGroup struct {
	Name     string
	Requests resourceRequests
	// UseMem is the group's measured memory working set in MiB.
	UseMem  int
	Enabled func(*config.Config, platformTopology) bool
}

// The figures, one pair of constants per group (see the package comment for
// where they were measured).
const (
	// kind's static pods: kube-apiserver 250m, controller-manager 200m,
	// scheduler 100m, etcd 100m/100Mi, kindnet 100m/50Mi, two CoreDNS at
	// 100m/70Mi. Always there — it is the node. In use: the apiserver alone
	// held 1.6 GiB after a day of platform churn, controller-manager 147Mi,
	// etcd 124Mi, scheduler 55Mi, CoreDNS 66Mi, kube-proxy 35Mi, kindnet
	// 32Mi, the local-path provisioner 18Mi.
	reqKindControlPlaneCPU = 950
	reqKindControlPlaneMem = 290
	useKindControlPlaneMem = 2070
	// The lab's own Dex. Always deployed by Up.
	reqDexCPU = 50
	reqDexMem = 64
	useDexMem = 40
	// The chart without its optional parts: muster 100m/128Mi and its valkey
	// 150m/192Mi, agentgateway 100m/128Mi and its controller 50m/128Mi,
	// mcp-kubernetes 105m/144Mi, agent-manager 55m/80Mi. In use: muster 91Mi,
	// valkey 28Mi, the data plane 20Mi and its controller 63Mi,
	// mcp-kubernetes 24Mi, agent-manager 17Mi.
	reqPlatformCoreCPU = 510
	reqPlatformCoreMem = 736
	usePlatformCoreMem = 245
	// The agents runtime: the kagent controller 100m/128Mi and the UI
	// 100m/256Mi (70Mi and 2Mi in use).
	reqAgentsRuntimeCPU = 200
	reqAgentsRuntimeMem = 384
	useAgentsRuntimeMem = 75
	// kagent's bundled PostgreSQL, 250m/256Mi (47Mi in use) — on a chart
	// without the platform Postgres (the 0.10 product's 3.x line); on the
	// 4.x line the controller's database is the CNPG Cluster below.
	reqKagentPostgresCPU = 250
	reqKagentPostgresMem = 256
	useKagentPostgresMem = 50
	// model-manager in front of the host model servers (14Mi in use).
	reqModelManagerCPU = 55
	reqModelManagerMem = 80
	useModelManagerMem = 15
	// Agent Substrate (the 4.x line, from the chart): the control plane in
	// ate-system — ate-api-server ×2, ate-controller, atelet, the atenet
	// router, egress and dns, RustFS — and the podcertificate-controller
	// declare nothing; the WorkerPool's four gVisor workers request the
	// chart's 250m/512Mi each (limits 2 CPU / 2Gi: one worker hosts one
	// actor, the limit bounds its sandbox), so the pool is the whole
	// request. In use, idle: the control plane 425Mi (RustFS 175Mi, atelet
	// 51Mi, dns 45Mi, egress 35Mi, router 32Mi, ate-api-server 2 × 28Mi,
	// ate-controller 27Mi), the podcertificate-controller 17Mi, a worker
	// 9Mi (four: 37Mi) — the reservation is capacity for the actors, not
	// what idles.
	reqSubstrateCPU = 1000
	reqSubstrateMem = 2048
	useSubstrateMem = 480
	// The platform Postgres (the 4.x line, from the chart): the CloudNativePG
	// operator and the one-instance Cluster the lab renders declare nothing.
	// In use, right after the bootstrap: the operator 63Mi, the instance
	// (kagent-pg-1, both databases created) 114Mi.
	reqCNPGCPU = 0
	reqCNPGMem = 0
	useCNPGMem = 180
	// Backstage requests almost nothing and uses several times its 250Mi
	// (400Mi measured).
	reqBackstageCPU = 20
	reqBackstageMem = 250
	useBackstageMem = 400
	// The chart's bundled Flux engine (the lab shape): the Flux Operator plus
	// the FluxInstance's source-controller and helm-controller — the delivery
	// engine of every platform component, so it is always part of the platform.
	// flux-operator 100m/64Mi (145Mi in use), helm-controller 100m/64Mi
	// (124Mi), source-controller 50m/64Mi (52Mi).
	reqFluxCPU = 250
	reqFluxMem = 192
	useFluxMem = 320
	// observability: kube-state-metrics 200m/200Mi, mcp-prometheus
	// 105m/144Mi. The Prometheus server, the operator and node-exporter
	// declare no requests at all — and the server alone uses 564Mi (the
	// operator 46Mi, kube-state-metrics 49Mi, node-exporter 25Mi,
	// mcp-prometheus 24Mi).
	reqObservabilityCPU = 305
	reqObservabilityMem = 344
	useObservabilityMem = 710

	// The run-time headroom the CPU floor keeps free beyond the requests, for
	// what the platform runs AFTER the boot. On the 4.x line an agent is an
	// actor inside one of the WorkerPool's pre-provisioned workers (their
	// requests are counted above), so the headroom is what one Go ADK turn
	// bursts: about 0.3 core for a second or two on the worker while the
	// actor answers (its memory stays inside the worker's request).
	// On a chart without Substrate every kagent agent is one more pod at the
	// 100m the platform's own small pods request, and models-test and
	// agents-test each create one: six of them.
	runtimeHeadroomCPUTurn      = 300
	runtimeHeadroomCPUAgentPods = 600

	// memUseHeadroom is the room the memory floor keeps above the measured
	// use: a turn's working set on a worker (55–70Mi), Backstage and
	// Prometheus growing under load. A quarter over the idle measurement.
	memUseHeadroomNum, memUseHeadroomDen = 5, 4
)

// labResourceGroups lists every group in boot order, gated the way Up and
// platformUp install them. Groups that hang off the platform are inert when
// the platform itself is disabled, like their config fields; the two the
// chart brings with the agents on the 4.x line follow the topology.
var labResourceGroups = []resourceGroup{
	{"kind control plane", resourceRequests{reqKindControlPlaneCPU, reqKindControlPlaneMem}, useKindControlPlaneMem,
		func(*config.Config, platformTopology) bool { return true }},
	{"Dex", resourceRequests{reqDexCPU, reqDexMem}, useDexMem,
		func(*config.Config, platformTopology) bool { return true }},
	{"agent platform", resourceRequests{reqPlatformCoreCPU, reqPlatformCoreMem}, usePlatformCoreMem,
		func(c *config.Config, _ platformTopology) bool { return c.Platform.Enabled }},
	{"agents runtime", resourceRequests{reqAgentsRuntimeCPU, reqAgentsRuntimeMem}, useAgentsRuntimeMem,
		func(c *config.Config, _ platformTopology) bool { return c.Platform.Enabled && c.Platform.Agents }},
	{"kagent's bundled Postgres", resourceRequests{reqKagentPostgresCPU, reqKagentPostgresMem}, useKagentPostgresMem,
		func(c *config.Config, t platformTopology) bool {
			return c.Platform.Enabled && c.Platform.Agents && !t.CNPG
		}},
	{"model-manager", resourceRequests{reqModelManagerCPU, reqModelManagerMem}, useModelManagerMem,
		func(c *config.Config, _ platformTopology) bool { return c.ModelManagerEnabled() }},
	{"Substrate", resourceRequests{reqSubstrateCPU, reqSubstrateMem}, useSubstrateMem,
		func(c *config.Config, t platformTopology) bool {
			return c.Platform.Enabled && c.Platform.Agents && t.Substrate
		}},
	{"platform Postgres", resourceRequests{reqCNPGCPU, reqCNPGMem}, useCNPGMem,
		func(c *config.Config, t platformTopology) bool {
			return c.Platform.Enabled && c.Platform.Agents && t.CNPG
		}},
	{"Backstage", resourceRequests{reqBackstageCPU, reqBackstageMem}, useBackstageMem,
		func(c *config.Config, _ platformTopology) bool { return c.Platform.Enabled && c.Backstage.Enabled }},
	{"Flux engine", resourceRequests{reqFluxCPU, reqFluxMem}, useFluxMem,
		func(c *config.Config, _ platformTopology) bool { return c.Platform.Enabled }},
	{"observability", resourceRequests{reqObservabilityCPU, reqObservabilityMem}, useObservabilityMem,
		func(c *config.Config, _ platformTopology) bool { return c.Platform.Enabled && c.Platform.Observability }},
}

// runtimeHeadroomCPU is the CPU the floor keeps free for run-time work on a
// topology: a Go ADK turn inside a pre-provisioned worker with Substrate,
// six agent pods without.
func runtimeHeadroomCPU(topo platformTopology) int {
	if topo.Substrate {
		return runtimeHeadroomCPUTurn
	}
	return runtimeHeadroomCPUAgentPods
}

// resourceNeeds is what one configuration asks of the container runtime.
type resourceNeeds struct {
	// Requests is the sum over the enabled groups: what the boot schedules.
	Requests resourceRequests
	// UseMem is the sum of the groups' measured memory use, in MiB.
	UseMem int
	// Groups names the enabled groups with their requests, for the
	// breakdown a refusal prints.
	Groups []resourceGroup
	// MinCPUs is the hard floor: the requests plus the run-time headroom,
	// rounded up to whole CPUs — the unit docker is given them in, and the
	// node's allocatable CPU is exactly the runtime's count.
	MinCPUs int
	// MinMemMiB is the soft floor: the measured use plus its headroom, never
	// below the requests. Below Requests.Mem the pods do not even schedule;
	// between the two they schedule and then get evicted or OOM-killed under
	// real use.
	MinMemMiB int
}

// labResourceNeeds computes what cfg asks of the runtime from the enabled
// groups — never one hard-coded number, so a lab without Backstage and
// observability is held to its own, smaller floor.
func labResourceNeeds(cfg *config.Config, topo platformTopology) resourceNeeds {
	var n resourceNeeds
	for _, g := range labResourceGroups {
		if !g.Enabled(cfg, topo) {
			continue
		}
		n.Groups = append(n.Groups, g)
		n.Requests = n.Requests.add(g.Requests)
		n.UseMem += g.UseMem
	}
	n.MinCPUs = ceilCPUs(n.Requests.CPU + runtimeHeadroomCPU(topo))
	n.MinMemMiB = max(n.Requests.Mem, n.UseMem*memUseHeadroomNum/memUseHeadroomDen)
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
func judgeRuntimeResources(m runtimeMeasure, cfg *config.Config, topo platformTopology, needs resourceNeeds) (warning string, err error) {
	if m.CPUs < needs.MinCPUs {
		return "", fmt.Errorf("%s has %d CPUs; this lab configuration needs %d.\n"+
			"  Its pods request %s CPUs of the single kind node, plus room for the pods the\n"+
			"  platform creates at run time, and the kube-scheduler refuses a pod whose CPU\n"+
			"  request does not fit: the boot would wait on an agentgateway that stays\n"+
			"  Pending forever (docs/getting-started.md \"Docker resources\"). The requests:\n%s\n%s",
			m.engine(), m.CPUs, needs.MinCPUs, fmtCPUs(needs.Requests.CPU),
			wrap(requestsBreakdown(needs), 84, "    "), resourceFixes(m, cfg, topo, needs))
	}
	if m.MemMiB < needs.Requests.Mem {
		return "", fmt.Errorf("%s has %s of memory; this lab configuration needs %s.\n"+
			"  Its pods request %s of the single kind node, which does not fit, so they do not\n"+
			"  even all schedule — and they use about %s (docs/getting-started.md \"Docker resources\").\n%s",
			m.engine(), fmtGiB(m.MemMiB), fmtGiB(needs.MinMemMiB), fmtGiB(needs.Requests.Mem),
			fmtGiB(needs.UseMem), resourceFixes(m, cfg, topo, needs))
	}
	if m.MemMiB < needs.MinMemMiB {
		warning = fmt.Sprintf("%s has %s of memory; this lab configuration wants %s.\n"+
			"    Its pods request %s, so they schedule — but they use about %s (Backstage, Prometheus\n"+
			"    and the kind apiserver use several times what they declare): expect evictions\n"+
			"    and OOM kills under load (docs/getting-started.md \"Docker resources\").\n%s",
			m.engine(), fmtGiB(m.MemMiB), fmtGiB(needs.MinMemMiB), fmtGiB(needs.Requests.Mem), fmtGiB(needs.UseMem),
			indent(resourceFixes(m, cfg, topo, needs), "  "))
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
func resourceFixes(m runtimeMeasure, cfg *config.Config, topo platformTopology, needs resourceNeeds) string {
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
	// The smaller lab is offered where it lowers a floor: with Substrate the
	// WorkerPool is the CPU of requests that Backstage and observability
	// never were, so the offer may only buy memory.
	if flags, small := smallerLabFlags(cfg), labResourceNeeds(smallerLab(cfg), topo); flags != "" && (small.MinCPUs < needs.MinCPUs || small.MinMemMiB < needs.MinMemMiB) {
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
func preflightRuntimeResources(cfg *config.Config, topo platformTopology) error {
	needs := labResourceNeeds(cfg, topo)
	step("Checking the container runtime's CPUs and memory against this lab's requests")
	cpus, memBytes, err := runtimeResources()
	if err != nil {
		note("could not read them (%v); going on. This lab requests %s CPUs / %s, uses about %s, and needs %d CPUs / %s (docs/getting-started.md \"Docker resources\")",
			err, fmtCPUs(needs.Requests.CPU), fmtGiB(needs.Requests.Mem), fmtGiB(needs.UseMem), needs.MinCPUs, fmtGiB(needs.MinMemMiB))
		return nil
	}
	m := runtimeMeasure{
		CPUs:     cpus,
		MemMiB:   int(memBytes / (1024 * 1024)),
		Podman:   dockerIsPodman(),
		GOOS:     runtime.GOOS,
		HostCPUs: runtime.NumCPU(),
	}
	note("%s: %d CPUs, %s memory; the lab requests %s CPUs / %s, uses about %s, and needs %d CPUs / %s",
		m.engine(), m.CPUs, fmtGiB(m.MemMiB), fmtCPUs(needs.Requests.CPU), fmtGiB(needs.Requests.Mem), fmtGiB(needs.UseMem), needs.MinCPUs, fmtGiB(needs.MinMemMiB))
	warning, err := judgeRuntimeResources(m, cfg, topo, needs)
	if err != nil {
		return err
	}
	if warning != "" {
		warn("%s", warning)
	}
	// Above the floor but with less than a CPU to spare: say so, so a
	// models-test that later fails at the agent turn has its cause on record
	// in the boot log.
	if spare := m.CPUs*1000 - needs.Requests.CPU; spare < 1000 {
		note("tight: %s CPUs left beyond the requests for the agents' turns (and, without Substrate, their pods)", fmtCPUs(spare))
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
