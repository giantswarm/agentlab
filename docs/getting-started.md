# Getting started

Everything from an empty machine to a Claude Code session driving the
platform. The [README](../README.md) has the short version; this page is the
full walkthrough, including what `agentlab configure` discovers about the host
and how to exercise the identity on its own.

## Requirements

`docker` (or Podman >= 4's docker-compatible CLI), `kubectl`, `helm` (**>= 4**
— the platform install relies on Helm 4's `--wait`, which waits on the chart's
Flux custom resources so the command returns with every component Ready; Helm
3's does not, and the agent-platform chart documents a Helm 3 install as not
measured). `go` (>= 1.26) only to build from source.

kind is not on the list: it is built into `agentlab` as a Go dependency
(`sigs.k8s.io/kind`), which creates and deletes the cluster and side-loads
images through kind's own packages. The Kubernetes version the lab boots is
that kind release's default node image — `agentlab configure` names both
(`kind v0.32.0 (embedded, kindest/node:v1.36.1)`), and a new agentlab release
moves them together. kind drives the container engine through its CLI, so
`docker` (or `podman`) is the prerequisite it does not remove.

Under **rootless Podman** the lab publishes its ports from your own network
namespace, which cannot bind anything below
`net.ipv4.ip_unprivileged_port_start` (1024 by default). `agentlab configure`
detects this and moves the agentgateway edge off its default 443 — to 8443,
so the public URLs gain `:8443` — and reports the move. Run Podman as root, or
lower the sysctl, to keep 443.

## Docker resources

The lab runs the whole platform on a single kind node, so whatever backs
docker has to fit it — a Docker Desktop VM, Colima or a podman machine on a
Mac, the host itself under rootless Podman on Linux. **CPU is the binding
constraint, and it is a hard one**: the kube-scheduler refuses
a pod whose CPU *request* does not fit, so a node that is 100m short simply
leaves pods `Pending` forever. It does not degrade, it stalls.

What a full default lab requests (measured from the chart renders at the
pinned versions, plus kind's own control plane on a live node):

| | CPU | Memory |
|---|---|---|
| kind's Kubernetes: apiserver, controller-manager, scheduler, etcd, CNI, CoreDNS | 950m | ~290 MiB |
| the agent platform: muster + valkey, agentgateway + controller, mcp-kubernetes, agent-manager, model-manager, kagent + UI + postgres, Backstage | 1080m | ~1750 MiB |
| Dex | 50m | 64 MiB |
| the chart's Flux engine: the Flux Operator plus the `FluxInstance`'s source-controller and helm-controller (the lab shape brings it with the platform — it delivers every component and the agents) | 250m | 192 MiB |
| observability: kube-state-metrics + mcp-prometheus | 300m | 328 MiB |
| **total requests** | **≈ 2.6 CPU** | **≈ 2.5 GiB** |

The memory column *understates* real use, and by a lot: the Prometheus server
(its CR sets no `resources`), the prometheus-operator and node-exporter
declare nothing at all, and Backstage requests 250 MiB while its Node process
uses several times that. A node running the platform with Backstage and
observability **off** was observed at 2.4 GiB actual — i.e. the whole
requests budget — so a full lab wants roughly twice that.

Give docker at least:

| | CPUs | Memory |
|---|---|---|
| the full default lab (platform + agents + observability + Backstage) | **4** | **6 GiB** (the floor is 5.1 GiB; whole GiB) |
| platform + agents only (`configure --backstage=false --observability=false`) | 3 | 4 GiB (3.9 GiB) |

Those are the floors `agentlab up` enforces, and they already include room for
the pods the platform creates at run time: every kagent agent is another pod,
and `models-test` and `agents-test` each create one. `agentlab up` checks the
runtime before any cluster work — it prints the measured CPUs and memory next
to this configuration's requests and floor on every boot, refuses below the
CPU floor and warns below the memory one. Give it 6 CPUs and 8 GiB if you have
them: the lab is then comfortable rather than exactly large enough.

The second row is the one that has actually been measured on a live node: it
requests ~2.3 CPU (no Backstage, no observability; the chart's Flux engine is
always part of it) and sat at 2.4 GiB of real use — measured before the engine
joined the platform, which adds 250m / 192 MiB of requests on top.

Two CPUs — what a small Docker Desktop or Colima VM gives you — is not enough
for any of it. The symptoms are specific, and worth recognising because
nothing says "out of CPU": `agentgateway` (or any late pod) sits `Pending` while
`kubectl describe node` shows CPU requests at 95%+ of allocatable; the
platform install then waits on a workload that will never start and times
out. With the platform up but the node full, `models-test` gets as far as the
agent turn and fails there — the agent's own pod cannot be scheduled, so the
Agent CR never goes `Ready`.

Disk is not usually the constraint. A first boot pulls a few GiB of images —
on the *host*, always, and side-loads them into the node — and that host
cache survives `agentlab down`, so a re-boot does not pay for them again.

## Quick start

Install the `agentlab` binary one of three ways:

```bash
# From source (clone this repo first):
go build -o agentlab .

# Via go install:
go install github.com/giantswarm/agentlab@latest

# From a GitHub release — assets are named agentlab-<os>-<arch>:
curl -Lo agentlab https://github.com/giantswarm/agentlab/releases/latest/download/agentlab-linux-amd64
chmod +x agentlab
```

Keep it current with `agentlab self-update`. Every command also starts with a
one-line hint on stderr while a newer release is out — a hint, never a gate:
an outdated agentlab keeps working (see [Keeping agentlab current](cli.md#keeping-agentlab-current)).

Then bring the lab up:

```bash
export ANTHROPIC_API_KEY=sk-ant-...   # optional: powers the agents + Backstage AI chat
./agentlab configure       # interactive form: cluster, users, components
./agentlab up              # certs, kind cluster, Dex, RBAC, the agent platform — verified
./agentlab platform-test   # headless proof: Dex -> muster -> mcp-kubernetes -> apiserver, + the per-server OAuth sign-in challenge
./agentlab models-test     # with an Ollama on the host: pull -> ModelConfig -> agent turn -> delete, through the platform
./agentlab agents-test     # agent-manager as the signed-in user: create -> ready -> update -> delete via muster; a viewer's create is Forbidden; the ServiceAccount holds no RBAC
./agentlab toolsets-test   # declared toolsets end to end: agent-manager requires one, the Agent carries the header, muster resolves and refuses per request, agents see their toolset, a per-server sign-in scopes tools to the token, the portal's Tools step and apply path
```

Then trust the lab CA once and point Claude Code at the platform (`.mcp.json`
in this repo already does the latter):

```bash
./agentlab trust             # once per machine: green locks everywhere (one sudo prompt)
export NODE_USE_SYSTEM_CA=1  # Node >= 22.15 (older Node: export NODE_EXTRA_CA_CERTS=$PWD/certs/ca.crt)
claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp
# in Claude Code:  /mcp  ->  authenticate  ->  Dex login page  ->  done
```

The trust step is optional — see [TLS: one lab CA, trusted
explicitly](tls.md) for what it does, how to
revert it (`agentlab untrust`), and the untrusted fallback.

`agentlab configure --defaults` skips the form and writes the canonical lab:
the **agent platform on** (it is what the lab exists to test) behind the
agentgateway edge, Backstage on, three users, Dex on 32000.
`--platform=false` gives a bare kind+Dex OIDC sandbox;
`--backstage=false` skips the portal. On a plain terminal or screen reader,
`agentlab configure --accessible` runs the form as one prompt per question.

## `configure` discovers this machine — on every run

Before it asks anything (or, with `--defaults`, writes anything), `agentlab
configure` probes the machine and prints what it found, then applies it to the
configuration — a fresh one and an existing `agentlab.yaml` alike, so the file
follows the host instead of freezing the first run's view of it:

```
Discovering this machine:
  tools             docker 29.7.2, kind v0.32.0 (embedded, kindest/node:v1.36.1), kubectl v1.36.4, helm v4.2.2
  cluster           kind "agentlab" exists — its port mappings are fixed at node creation (`agentlab down && agentlab up` to change them)
  Ollama            0.33.2 on :11434 — answers on 172.21.0.1 (the address pods dial): yes; 10 downloaded, 4 tool-calling
  Lemonade Server   11.9.0 on :13305 — answers on 172.21.0.1 (the address pods dial): yes; 4 downloaded, 3 tool-calling
  Anthropic key     $ANTHROPIC_API_KEY is set — the agents' default ModelConfig and Backstage's AI chat get the real key at deploy time

Applied to the configuration:
  platform.modelManager.backends: [ollama] -> [ollama, lemonade]
```

- **Tools**: `docker`, `kubectl`, `helm` are looked up and their versions
  shown, next to the kind embedded in the binary and the node image — the
  Kubernetes — it boots. A missing tool or a Helm 3 **refuses `configure`
  right here** — before the first question, with why the lab needs it and
  where to get it — rather than after the whole form or minutes into
  `agentlab up`. `helm` is only required while the platform is on
  (`--platform=false`, the bare kind+Dex sandbox, does without it).
- **Ports**: every host-side port is probed on 127.0.0.1 — the address all
  kind port mappings bind. While **no kind node of this configuration
  exists** (a fresh lab, or after `agentlab down`), an occupied port is moved
  to a nearby free one, with a message saying what moved where (443 falls
  back to 8443). When the edge leaves 443, every public URL in these docs
  gains that port suffix (`https://backstage.127.0.0.1.nip.io:8443`); the
  lab's edge Service serves that port in-cluster as well, so the ported URLs
  resolve from pods too and every proof still passes. Once the cluster exists
  its mappings are fixed at node creation, so the ports it publishes never
  count as occupied and a foreign listener on one of them is **reported**, not
  renumbered around (free it, or `agentlab down`, re-run `configure`,
  `agentlab up`).
- **Host model servers**: an Ollama on `:11434` and a Lemonade Server on
  `:13305` (their default ports) are detected with version, whether they
  listen on the kind docker gateway (pods' path to the host — the bind-address
  fix is named when they do not) and their downloaded models, counting the
  tool-calling ones. What answers becomes `platform.modelManager.backends`
  (Ollama first) and turns managed models on; a server that vanished drops
  out and, with none left, managed models go off — see [Managed
  models](models.md#managed-models-model-manager--the-host-model-servers). A
  standalone `flm serve` (FastFlowLM's own server, default `:52625`) is
  reported but not wired: it has no management API and lists its catalog
  rather than what is downloaded — the lab drives FLM through Lemonade.
- **`$ANTHROPIC_API_KEY`**: whether it is exported, since the agents'
  default ModelConfig and Backstage's AI chat take it at deploy time.

Pins override the discovery for that run: `--model-manager[=false]` decides
the flag regardless of what answers, `--model-manager-backends ollama,lemonade`
sets the list (and its order) outright; `--platform`, `--agents`,
`--observability` and `--backstage` toggle the components as before, with or
without `--defaults`. Nothing else in an existing file is touched — users,
extra models and the pinned chart ref stay as they are.

To exercise the identity itself:

```bash
./agentlab test                    # asserts RBAC for every configured user
./agentlab login dev@lab.local     # headless, instant
./agentlab browser                 # real Dex login page in the browser
export KUBECONFIG=$PWD/kubeconfig.oidc
kubectl auth whoami
```

Tear down with `./agentlab down`.
