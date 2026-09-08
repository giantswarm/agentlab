# Getting started

Everything from an empty machine to a Claude Code session driving the
platform. The [README](../README.md) has the short version; this page is the
full walkthrough, including what `agentlab configure` discovers about the host
and how to exercise the identity on its own.

## Requirements

`go` (>= 1.25), `docker` (or Podman >= 4's docker-compatible CLI), `kind`
(>= 0.31), `kubectl`, `helm` (**>= 4** — Helm 3
cannot store the umbrella chart's release any more: the dependency archives put
the release Secret over etcd's 1 MiB cap, see
[agent-platform-standalone#21](https://github.com/giantswarm/agent-platform-standalone/issues/21);
Helm 4's plugin-only post-renderer contract is handled by a generated plugin,
see the [deviations table](platform.md#lab-specific-deviations-from-a-real-management-cluster)), `git`.

Under **rootless Podman** the lab publishes its ports from your own network
namespace, which cannot bind anything below
`net.ipv4.ip_unprivileged_port_start` (1024 by default). `agentlab configure`
detects this and moves the agentgateway edge off its default 443 — to 8443,
so the public URLs gain `:8443` — and reports the move. Run Podman as root, or
lower the sysctl, to keep 443.

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
an outdated agentlab keeps working (see "Keeping agentlab current").

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
  tools             docker 29.7.2, kind v0.32.0, kubectl v1.36.4, helm v4.2.2
  cluster           kind "agentlab" exists — its port mappings are fixed at node creation (`agentlab down && agentlab up` to change them)
  Ollama            0.33.2 on :11434 — answers on 172.21.0.1 (the address pods dial): yes; 10 downloaded, 4 tool-calling
  Lemonade Server   11.9.0 on :13305 — answers on 172.21.0.1 (the address pods dial): yes; 4 downloaded, 3 tool-calling
  Anthropic key     $ANTHROPIC_API_KEY is set — the agents' default ModelConfig and Backstage's AI chat get the real key at deploy time

Applied to the configuration:
  platform.modelManager.backends: [ollama] -> [ollama, lemonade]
```

- **Tools**: `docker`, `kind`, `kubectl`, `helm` are looked up and their
  versions shown; a missing one (or a Helm 3) is called out here rather than
  minutes into `agentlab up`.
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
