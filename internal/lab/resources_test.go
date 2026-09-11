package lab

import (
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// Nothing in this file touches a container runtime: the probe is split into
// the shell-out (runtimeResources, untested here) and the parsing and
// arithmetic below, which is where the decisions are.

// Both engines' `info` templates print "<cpus> <memory bytes>"; everything
// else — the other engine's template hitting missing fields, an empty
// answer, garbage, zeros — must be an error, which the preflight turns into
// a note rather than a refusal.
func TestParseRuntimeResources(t *testing.T) {
	for name, tc := range map[string]struct {
		out     string
		cpus    int
		mem     int64
		wantErr bool
	}{
		"docker":                       {out: "24 92417925120\n", cpus: 24, mem: 92417925120},
		"podman machine":               {out: "4 8589934592", cpus: 4, mem: 8589934592},
		"two cpus two gib":             {out: "2 2147483648\n", cpus: 2, mem: 2147483648},
		"template hit the wrong shape": {out: "<no value> <no value>\n", wantErr: true},
		"empty":                        {out: "", wantErr: true},
		"whitespace only":              {out: "  \n", wantErr: true},
		"one field":                    {out: "24\n", wantErr: true},
		"three fields":                 {out: "24 92417925120 extra", wantErr: true},
		"garbage":                      {out: "Cannot connect to the Docker daemon", wantErr: true},
		"zero cpus":                    {out: "0 92417925120", wantErr: true},
		"zero memory":                  {out: "24 0", wantErr: true},
		"negative":                     {out: "-1 -1", wantErr: true},
		"float cpus":                   {out: "2.5 2147483648", wantErr: true},
	} {
		cpus, mem, err := parseRuntimeResources(tc.out)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: parseRuntimeResources(%q) = %d, %d, want an error", name, tc.out, cpus, mem)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parseRuntimeResources(%q): %v", name, tc.out, err)
			continue
		}
		if cpus != tc.cpus || mem != tc.mem {
			t.Errorf("%s: parseRuntimeResources(%q) = %d, %d, want %d, %d", name, tc.out, cpus, mem, tc.cpus, tc.mem)
		}
	}
}

// labConfig builds a configuration from the four toggles the floor depends
// on (plus model-manager, which rides on agents).
func labConfig(platform, agents, observability, backstage, modelManager bool) *config.Config {
	cfg := config.Default()
	cfg.Platform.Enabled = platform
	cfg.Platform.Agents = agents
	cfg.Platform.Observability = observability
	cfg.Backstage.Enabled = backstage
	cfg.Platform.ModelManager.Enabled = modelManager
	return cfg
}

// The two topologies the floor is computed for: the 4.x line ships Agent
// Substrate and the platform Postgres with the agents (fourX); a chart that
// ships neither — the 0.10 product's 3.x line — budgets for kagent's bundled
// Postgres instead (stableLine).
var (
	fourX      = platformTopology{Substrate: true, CNPG: true}
	stableLine = platformTopology{}
)

// The floor follows the enabled components and the chart's topology. The
// full default lab on the 4.x line reproduces the live measurement the
// constants come from (3340m / 4388Mi requested, ~4.4 GiB in use on
// 2026-09-11) and lands on the Docker resources table's rows: 4 CPUs and
// 5.5 GiB (the WorkerPool's four workers are a CPU of requests by
// themselves, the apiserver and Prometheus most of the use). A chart without
// Substrate and the platform Postgres — the 0.10 product's 3.x line — budgets
// kagent's bundled Postgres and six agent pods instead: the 4 CPUs of old.
// The chart's Flux engine counts whenever the platform does; everything but
// kind and Dex is inert when the platform is off.
func TestLabResourceNeeds(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg     *config.Config
		topo    platformTopology
		cpu     int
		mem     int
		use     int
		minCPUs int
		minMem  int
		groups  string
	}{
		"the 4.x line, full default lab with model-manager (the measured lab)": {
			cfg: labConfig(true, true, true, true, true), topo: fourX,
			cpu: 3340, mem: 4388, use: 4535, minCPUs: 4, minMem: 5668,
			groups: "kind control plane,Dex,agent platform,agents runtime,model-manager,Substrate,platform Postgres,Backstage,Flux engine,observability",
		},
		"the 4.x line without agents: no runtime, nothing for Substrate or the Cluster to serve": {
			cfg: labConfig(true, false, true, true, false), topo: fourX,
			cpu: 2085, mem: 1876, use: 3785, minCPUs: 3, minMem: 4731,
			groups: "kind control plane,Dex,agent platform,Backstage,Flux engine,observability",
		},
		"the 4.x line, full default lab without model-manager": {
			cfg: labConfig(true, true, true, true, false), topo: fourX,
			cpu: 3285, mem: 4308, use: 4520, minCPUs: 4, minMem: 5650,
			groups: "kind control plane,Dex,agent platform,agents runtime,Substrate,platform Postgres,Backstage,Flux engine,observability",
		},
		"the 4.x line, platform + agents only (the docs' smaller lab)": {
			cfg: labConfig(true, true, false, false, true), topo: fourX,
			cpu: 3015, mem: 3794, use: 3425, minCPUs: 4, minMem: 4281,
			groups: "kind control plane,Dex,agent platform,agents runtime,model-manager,Substrate,platform Postgres,Flux engine",
		},
		"the 3.x line, full default lab with model-manager: the bundled Postgres, six agent pods of headroom": {
			cfg: labConfig(true, true, true, true, true), topo: stableLine,
			cpu: 2590, mem: 2596, use: 3925, minCPUs: 4, minMem: 4906,
			groups: "kind control plane,Dex,agent platform,agents runtime,kagent's bundled Postgres,model-manager,Backstage,Flux engine,observability",
		},
		"the 3.x line, platform + agents only": {
			cfg: labConfig(true, true, false, false, true), topo: stableLine,
			cpu: 2265, mem: 2002, use: 2815, minCPUs: 3, minMem: 3518,
			groups: "kind control plane,Dex,agent platform,agents runtime,kagent's bundled Postgres,model-manager,Flux engine",
		},
		"platform + Backstage without agents: no kagent, no model-manager": {
			cfg: labConfig(true, false, false, true, true), topo: fourX,
			cpu: 1780, mem: 1532, use: 3075, minCPUs: 3, minMem: 3843,
			groups: "kind control plane,Dex,agent platform,Backstage,Flux engine",
		},
		"platform alone": {
			cfg: labConfig(true, false, false, false, false), topo: fourX,
			cpu: 1760, mem: 1282, use: 2675, minCPUs: 3, minMem: 3343,
			groups: "kind control plane,Dex,agent platform,Flux engine",
		},
		"platform off: kind and Dex only, the other toggles inert": {
			cfg: labConfig(false, true, true, true, true), topo: fourX,
			cpu: 1000, mem: 354, use: 2110, minCPUs: 2, minMem: 2637,
			groups: "kind control plane,Dex",
		},
	} {
		n := labResourceNeeds(tc.cfg, tc.topo)
		if n.Requests.CPU != tc.cpu || n.Requests.Mem != tc.mem || n.UseMem != tc.use {
			t.Errorf("%s: requests = %dm / %dMi, use %dMi, want %dm / %dMi, %dMi", name, n.Requests.CPU, n.Requests.Mem, n.UseMem, tc.cpu, tc.mem, tc.use)
		}
		if n.MinCPUs != tc.minCPUs {
			t.Errorf("%s: MinCPUs = %d, want %d", name, n.MinCPUs, tc.minCPUs)
		}
		if n.MinMemMiB != tc.minMem {
			t.Errorf("%s: MinMemMiB = %d, want %d", name, n.MinMemMiB, tc.minMem)
		}
		got := make([]string, 0, len(n.Groups))
		for _, g := range n.Groups {
			got = append(got, g.Name)
		}
		if strings.Join(got, ",") != tc.groups {
			t.Errorf("%s: groups = %s, want %s", name, strings.Join(got, ","), tc.groups)
		}
	}
}

// The floor is the requests plus the run-time headroom rounded UP to whole
// CPUs: 3340m + 300m = 3640m needs 4, not 3.
func TestCeilCPUs(t *testing.T) {
	for in, want := range map[int]int{0: 0, 1: 1, 999: 1, 1000: 1, 1001: 2, 2540: 3, 3140: 4, 3000: 3} {
		if got := ceilCPUs(in); got != want {
			t.Errorf("ceilCPUs(%d) = %d, want %d", in, got, want)
		}
	}
}

const goosLinux = "linux"

// measure is a docker VM on a Mac unless a test says otherwise: the case the
// resize hints are written for.
func measure(cpus, memMiB int) runtimeMeasure {
	return runtimeMeasure{CPUs: cpus, MemMiB: memMiB, GOOS: "darwin", HostCPUs: 10}
}

// Below the CPU floor the boot is refused, and the refusal carries the
// measured value, this configuration's floor, the breakdown, the resize
// steps and the smaller lab that fits.
func TestJudgeRuntimeResourcesRefusesTooFewCPUs(t *testing.T) {
	cfg := labConfig(true, true, true, true, true)
	needs := labResourceNeeds(cfg, fourX)
	warning, err := judgeRuntimeResources(measure(2, 2048), cfg, fourX, needs)
	if err == nil {
		t.Fatal("2 CPUs for the full lab must be refused")
	}
	if warning != "" {
		t.Errorf("a refusal carries no separate warning, got %q", warning)
	}
	for _, want := range []string{
		"docker has 2 CPUs; this lab configuration needs 4",
		"request 3.34 CPUs",
		"kind control plane 0.95", "Dex 0.05", "agent platform 0.51", "agents runtime 0.20",
		"model-manager 0.06", "Substrate 1.00", "platform Postgres 0.00", "Backstage 0.02", "Flux engine 0.25", "observability 0.31",
		"Docker Desktop: Settings -> Resources",
		"colima start --cpu 4 --memory 6",
		"agentlab configure --backstage=false --observability=false",
		"needs 4 CPUs and 4.2 GiB",
		`docs/getting-started.md "Docker resources"`,
	} {
		// The breakdown is word-wrapped: compare on one line.
		if flat := strings.Join(strings.Fields(err.Error()), " "); !strings.Contains(flat, want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "podman") {
		t.Errorf("a docker refusal must not talk about podman:\n%s", err)
	}
}

// The floor is per configuration and topology: 3 CPUs refuse the full lab
// and pass the platform without agents (no WorkerPool to reserve a CPU); on
// the 3.x line, where an agent is a pod, 3 CPUs pass the platform + agents
// lab. The platform + agents lab's refusal at 2 CPUs offers no smaller lab
// (it is the smaller lab).
func TestJudgeRuntimeResourcesFloorFollowsConfiguration(t *testing.T) {
	full := labConfig(true, true, true, true, true)
	if _, err := judgeRuntimeResources(measure(3, 8192), full, fourX, labResourceNeeds(full, fourX)); err == nil {
		t.Error("3 CPUs must be refused for the full lab (floor 4)")
	}
	noAgents := labConfig(true, false, false, false, false)
	if warning, err := judgeRuntimeResources(measure(3, 8192), noAgents, fourX, labResourceNeeds(noAgents, fourX)); err != nil || warning != "" {
		t.Errorf("3 CPUs / 8 GiB must pass the platform without agents, got warning %q, err %v", warning, err)
	}
	small := labConfig(true, true, false, false, true)
	if warning, err := judgeRuntimeResources(measure(3, 8192), small, stableLine, labResourceNeeds(small, stableLine)); err != nil || warning != "" {
		t.Errorf("3 CPUs / 8 GiB must pass the platform + agents lab on the 3.x line, got warning %q, err %v", warning, err)
	}
	_, err := judgeRuntimeResources(measure(2, 8192), small, fourX, labResourceNeeds(small, fourX))
	if err == nil {
		t.Fatal("2 CPUs must be refused even for the platform + agents lab")
	}
	if strings.Contains(err.Error(), "agentlab configure --backstage=false") {
		t.Errorf("the smaller lab must not be offered to itself:\n%s", err)
	}
	if !strings.Contains(err.Error(), "already the smaller lab") {
		t.Errorf("the refusal should say this is already the smaller lab:\n%s", err)
	}
}

// Memory is soft: between the requests and the floor the boot goes on with a
// loud warning; below the requests nothing schedules, so it is refused.
func TestJudgeRuntimeResourcesMemory(t *testing.T) {
	cfg := labConfig(true, true, true, true, true)
	needs := labResourceNeeds(cfg, fourX) // 4388Mi requested, 4535Mi in use, floor 5668Mi

	warning, err := judgeRuntimeResources(measure(6, 5120), cfg, fourX, needs)
	if err != nil {
		t.Fatalf("5 GiB is below the floor but above the requests — a warning, not a refusal: %v", err)
	}
	for _, want := range []string{"docker has 5.0 GiB of memory", "wants 5.5 GiB", "request 4.3 GiB", "use about 4.4 GiB", "OOM", "colima start --cpu 4 --memory 6"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning lacks %q:\n%s", want, warning)
		}
	}

	if _, err := judgeRuntimeResources(measure(6, 4096), cfg, fourX, needs); err == nil {
		t.Fatal("4 GiB is below the 4.3 GiB of requests: the pods cannot schedule, refuse")
	} else if !strings.Contains(err.Error(), "docker has 4.0 GiB of memory") || !strings.Contains(err.Error(), "do not\n  even all schedule") {
		t.Errorf("memory refusal wording:\n%s", err)
	}

	if warning, err := judgeRuntimeResources(measure(6, 8192), cfg, fourX, needs); err != nil || warning != "" {
		t.Errorf("6 CPUs / 8 GiB is the Docker resources page's recommendation and must pass silently, got warning %q, err %v", warning, err)
	}
	if warning, err := judgeRuntimeResources(measure(4, 5668), cfg, fourX, needs); err != nil || warning != "" {
		t.Errorf("exactly the floor passes without a warning, got warning %q, err %v", warning, err)
	}
}

// The fix text names the runtime the user has. Podman on a Mac is a podman
// machine; rootless Podman on Linux, and docker's own daemon on Linux, are the
// host itself — nothing to resize, only the smaller lab to fall back to.
func TestResourceFixesPerRuntime(t *testing.T) {
	cfg := labConfig(true, true, true, true, true)
	needs := labResourceNeeds(cfg, fourX)
	podmanMac := runtimeMeasure{CPUs: 2, MemMiB: 2048, Podman: true, GOOS: "darwin", HostCPUs: 10}
	_, err := judgeRuntimeResources(podmanMac, cfg, fourX, needs)
	if err == nil {
		t.Fatal("2 CPUs must be refused")
	}
	if !strings.Contains(err.Error(), "podman has 2 CPUs") || !strings.Contains(err.Error(), "podman machine set --cpus 4 --memory 6144") {
		t.Errorf("podman machine refusal wording:\n%s", err)
	}
	if strings.Contains(err.Error(), "Colima") || strings.Contains(err.Error(), "Docker Desktop") {
		t.Errorf("a podman refusal must not send the user to Docker Desktop or Colima:\n%s", err)
	}

	podmanLinuxHost := runtimeMeasure{CPUs: 2, MemMiB: 2048, Podman: true, GOOS: goosLinux, HostCPUs: 2}
	_, err = judgeRuntimeResources(podmanLinuxHost, cfg, fourX, needs)
	if err == nil {
		t.Fatal("2 CPUs must be refused")
	}
	if !strings.Contains(err.Error(), "Rootless Podman on Linux runs the containers on this machine itself") ||
		strings.Contains(err.Error(), "podman machine set") {
		t.Errorf("rootless podman on Linux is the host itself, nothing to resize:\n%s", err)
	}

	dockerLinuxHost := runtimeMeasure{CPUs: 2, MemMiB: 2048, GOOS: goosLinux, HostCPUs: 2}
	_, err = judgeRuntimeResources(dockerLinuxHost, cfg, fourX, needs)
	if err == nil {
		t.Fatal("2 CPUs must be refused")
	}
	if !strings.Contains(err.Error(), "docker here is the host's own engine") || strings.Contains(err.Error(), "Colima") {
		t.Errorf("a native docker on Linux is the host itself, nothing to resize:\n%s", err)
	}

	// A Linux host running a VM (Docker Desktop for Linux) has more CPUs than
	// the VM: the resize hints apply.
	dockerLinuxVM := runtimeMeasure{CPUs: 2, MemMiB: 2048, GOOS: goosLinux, HostCPUs: 16}
	_, err = judgeRuntimeResources(dockerLinuxVM, cfg, fourX, needs)
	if err == nil || !strings.Contains(err.Error(), "Docker Desktop: Settings") {
		t.Errorf("a VM on a Linux host gets the resize hints:\n%v", err)
	}
}

func TestResourceFormatting(t *testing.T) {
	if got := fmtCPUs(2540); got != "2.54" {
		t.Errorf("fmtCPUs(2540) = %q", got)
	}
	if got := fmtCPUs(460); got != "0.46" {
		t.Errorf("fmtCPUs(460) = %q", got)
	}
	if got := fmtGiB(2532); got != "2.5 GiB" {
		t.Errorf("fmtGiB(2532) = %q", got)
	}
	if got := fmtGiB(88136); got != "86.1 GiB" {
		t.Errorf("fmtGiB(88136) = %q", got)
	}
	for in, want := range map[int]int{5064: 5, 4096: 4, 4097: 5, 3620: 4} {
		if got := ceilGiB(in); got != want {
			t.Errorf("ceilGiB(%d) = %d, want %d", in, got, want)
		}
	}
}
