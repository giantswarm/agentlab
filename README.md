<div align="center">

# agentlab

**The Giant Swarm agent platform on your laptop: one binary, one throwaway
kind cluster, every chain proven end to end.**

[![Release](https://img.shields.io/github/v/release/giantswarm/agentlab?label=release)](https://github.com/giantswarm/agentlab/releases)
[![CI](https://dl.circleci.com/status-badge/img/gh/giantswarm/agentlab/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/agentlab/tree/main)
[![Go](https://img.shields.io/github/go-mod/go-version/giantswarm/agentlab)](go.mod)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

</div>

---

**agentlab** runs the [Giant Swarm agent
platform](https://github.com/giantswarm/agent-platform) —
[muster](https://github.com/giantswarm/muster) as the MCP gateway, the
Kubernetes MCP server, the [kagent](https://github.com/kagent-dev/kagent)
agents runtime and Giant Swarm's
[Backstage](https://github.com/giantswarm/backstage) portal — on a
[kind](https://kind.sigs.k8s.io/) cluster on your machine, so the whole
platform can be tested and demoed without a management cluster.

Two kinds of client meet at one gateway. MCP clients such as Claude Code talk
to muster directly; people use **Backstage, the human frontend to the whole
platform** — servers, workflows, tools, agents, models — with their own token.
The agents Backstage creates run on kagent and are MCP clients of muster too,
calling tools as the person who invoked them.

The platform needs an identity provider, so the lab bundles its own
[Dex](https://dexidp.io/): users that exist nowhere but this cluster, RBAC
driven by the `groups` claim, and the apiserver, muster and Backstage all
trusting the same issuer.

<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/architecture-dark.svg">
    <img alt="agentlab architecture: Claude Code and Backstage, the two interfaces on the left, reach the platform through the agentgateway TLS edge into muster. muster fans out to mcp-kubernetes (the kube-apiserver), mcp-prometheus (Prometheus), model-manager (Ollama, Lemonade and LM Studio on the host) and agent-manager. The kagent runtime holds the Agent CRs, run as ADK pods, and the ModelConfigs for Anthropic, Ollama, Lemonade, LM Studio and OpenAI-compatible endpoints; agent-manager creates the Agent CRs, model-manager wires the ModelConfigs, the agents call tools back through muster with the caller's token and toolset, and Backstage chats with them over A2A. The bundled Dex at https://localhost:32000/dex is the one issuer the browser, muster, Backstage and the apiserver trust" src="docs/img/architecture-light.svg" width="900">
  </picture>
</div>

## What's in the box

- **One Go binary.** `agentlab configure` asks every option through an
  interactive form and writes `agentlab.yaml`; every manifest renders from
  embedded templates. No YAML to hand-edit, no scripts to source.
- **The agent platform.** The agent-platform chart every Giant Swarm
  management cluster runs, installed in its lab shape: muster behind an
  agentgateway TLS edge, the single OAuth enforcement point in front of an
  unauthenticated in-cluster `mcp-kubernetes`. Per-server sign-in, declared
  toolsets and a fake multi-cluster fleet are all exercised.
- **Backstage, the human frontend.** Giant Swarm's Backstage is how a person
  works the whole platform: browse and sign in to MCP servers, run workflows,
  explore tools, create agents and chat with them, manage models. Every call
  carries the signed-in user's own token, so the portal shows exactly what
  the platform grants that person.
- **Agents.** kagent runs the agents Backstage or agent-manager creates. They
  are MCP clients of muster like Claude Code is, forwarding the caller's token
  and bounded further by declared toolsets, so they see the same catalogue
  under the same rules.
- **Models.** An Anthropic default; extra model configs for OpenAI-compatible,
  Gemini and Ollama endpoints; managed models through model-manager fronting
  the model servers on the host — an Ollama, a Lemonade Server, an LM Studio.
- **Observability.** A minimal Prometheus plus mcp-prometheus, so PromQL
  questions about the lab go through MCP too.
- **Identity you can reason about.** Three throwaway users, a fixed group
  vocabulary, one issuer URL valid from the host, the node and every
  hostNetwork pod, and a lab CA you trust explicitly.
- **Proofs, not hope.** Headless `*-test` commands drive every chain — Dex
  login, muster, MCP tools, agents, toolsets, models, RBAC, Backstage — and
  fail loudly.

## Getting started

Requirements: `docker` (or Podman >= 4's docker-compatible CLI) — nothing
else; `go` >= 1.26 only to build from source. kind, Helm and the Kubernetes
client are built into the binary: the cluster runs the Kubernetes of the
embedded kind release's default node image, the platform installs through
Helm 4's SDK, and every call to the apiserver goes through client-go. Give
docker at least 4 CPUs and 6 GiB for the full lab; `agentlab up` checks before
it touches anything (see [Docker resources](docs/getting-started.md#docker-resources)).

```bash
go install github.com/giantswarm/agentlab@latest   # or a release binary, or `go build -o agentlab .`

export ANTHROPIC_API_KEY=sk-ant-...   # optional: powers the agents and Backstage's AI chat
agentlab configure --defaults         # drop --defaults for the interactive form
agentlab up                           # certs, kind cluster, Dex, RBAC, the platform — verified
agentlab open portal                  # the portal in your browser (the URL is printed too)
agentlab platform-test                # headless proof: Dex -> muster -> mcp-kubernetes -> apiserver
```

On a terminal, `up` ends by asking whether to trust the lab CA (one sudo
prompt, so browsers get a green lock) and whether to open the portal — `open
portal` is the command for later, and `open agents` opens the kagent UI.

Then point Claude Code at the platform:

```bash
agentlab trust                # if you said no above: one sudo prompt, `agentlab untrust` reverts it
export NODE_USE_SYSTEM_CA=1   # Node >= 22.15; older Node: NODE_EXTRA_CA_CERTS=$PWD/certs/ca.crt
claude mcp add --transport http muster https://muster.127.0.0.1.nip.io/mcp
# in Claude Code:  /mcp  ->  authenticate  ->  Dex login page  ->  done
```

The portal (<https://backstage.127.0.0.1.nip.io>) signs in the default users
`admin@lab.local`, `dev@lab.local` and `viewer@lab.local`, password
`password`. `agentlab down` deletes the cluster.

The full walkthrough, including what `configure` discovers about your
machine, is in [Getting started](docs/getting-started.md).

## Documentation

| Page | What it covers |
|---|---|
| [Getting started](docs/getting-started.md) | Requirements, docker CPU and memory, install, what `configure` discovers, the first `up`, connecting Claude Code |
| [Command reference](docs/cli.md) | Every `agentlab` command and flag, the environment variables, keeping the binary current |
| [TLS](docs/tls.md) | The lab CA, `trust` and `untrust`, Node and browsers, bringing your own certificate |
| [The agent platform](docs/platform.md) | muster + mcp-kubernetes: the request path, per-server sign-in, the fake fleet, toolsets, deviations from a real management cluster, the dev channel (a branch's dev builds), Agent Substrate and the platform Postgres from the chart |
| [Agents](docs/agents.md) | The kagent runtime, the default ModelConfig and the API key Secret |
| [Models](docs/models.md) | Extra model configs, model servers on the host, managed models through model-manager |
| [Observability](docs/observability.md) | Prometheus + mcp-prometheus, and Backstage's metrics views |
| [Backstage](docs/backstage.md) | The human frontend: the muster plugin, agents and models in the portal, the agent create flow |
| [Identity](docs/identity.md) | Users and groups, the shared issuer, the Dex version, wiring another app, `trustedPeers` |
| [Troubleshooting](docs/troubleshooting.md) | The gotchas that cost time |
| [Usage data](docs/telemetry.md) | The one anonymous signal per command, and how to opt out |
| [Development](docs/development.md) | Repository layout, building and testing, the hack journal |

## Development

```bash
make build     # go build -o agentlab .
make test      # go test ./...
```

The lab's end-to-end checks are its own `*-test` subcommands, not `go test`.
Every hack and workaround, with its status, is recorded in
[HACKS.md](HACKS.md); [CLAUDE.md](CLAUDE.md) briefs coding agents on the
repository. The layout is in [Development](docs/development.md).

## License

[Apache 2.0](LICENSE).
