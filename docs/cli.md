# Command reference

`agentlab --help` and `agentlab <command> --help` are authoritative; this page
is the map. Every command reads `agentlab.yaml` (written by `configure`; on a
terminal, a missing file starts the form) and talks to the kind cluster
through the cluster's own exported kubeconfig, `state/kubeconfig` — never
your shell's current-context.

## Lifecycle

| Command | What it does |
|---|---|
| `configure` | Discover this machine, then ask for the lab configuration (or keep it with `--defaults`) and save `agentlab.yaml`. Flags below. |
| `up` | Check docker's CPUs and memory against this configuration's floors, then create the kind cluster, deploy Dex and the enabled components, and verify the OIDC chain end to end. Idempotent: unchanged re-runs are no-ops. |
| `down` | Destroy the kind cluster. `certs/` is kept and the trust stores are untouched. |
| `reload` | Re-render and re-apply the Dex config after editing `agentlab.yaml` (users, passwords, groups). |
| `render` | Render every manifest from `agentlab.yaml` into `state/` without applying anything. |
| `certs` | Generate the lab CA and Dex server cert, re-minting only what config or policy require. `--force` regenerates everything and breaks a running cluster's trust. |
| `trust` | Install the lab CA into the system and browser trust stores (one sudo prompt; reversible). See [TLS](tls.md). |
| `untrust` | Remove exactly the lab CA from those stores. |
| `platform` | Install the agent platform on a running cluster: one idempotent upgrade-or-install of the agent-platform chart in its lab shape through the embedded Helm (the `helm upgrade --install --wait` of Helm 4, in-process), then wait for every component. A re-run that would install the same chart version with the same values writes no Helm revision. `up` runs this when the platform is enabled. On the [dev channel](platform.md#dev-channel) it first re-resolves the branch's newest build (and installs Substrate); `--pin` freezes the recorded build instead, `--pin=false` follows the branch again. |
| `platform-down` | Remove the agent platform in the chart's ordered teardown, leaving Dex and the cluster alone. |
| `logs <component>` | Tail a component's logs: `backstage`, `dex`, `mcp-prometheus`, `muster` or `prometheus`. |
| `self-update` | Replace the binary with the latest GitHub release, once its cosign Sigstore bundle verifies (see below). `--check` only reports the running and the latest version, exit status 125 when a newer one exists. |

`backstage` is retired: Backstage deploys with the platform (`backstage.enabled`
in `agentlab.yaml` and `agentlab up`).

## Identity

| Command | What it does |
|---|---|
| `login [email]` | Headless login (password grant); writes `.token` and `kubeconfig.oidc` for that user. `--password` overrides the one in `agentlab.yaml`. |
| `browser` | Log in through the real Dex login page in a browser (authorization-code flow). |
| `test` | Assert RBAC for every configured user: a token from Dex, then one SelfSubjectAccessReview per expectation of each group — `kubectl auth can-i`, asked of the apiserver in-process with that token alone. |

`kubectl --kubeconfig kubeconfig.oidc` after `login` is the OIDC path — the
way to verify what a specific user can do. The kind admin context bypasses the
platform and OIDC entirely.

## Proofs

The lab's end-to-end checks. Each one logs in to Dex headlessly, drives the
real components through the same paths a person would, asserts the result and
leaves nothing behind. Where a command takes `[email]`, that user runs the
proof (default: the admin). They trust `certs/ca.crt` directly, so they need
neither `agentlab trust` nor any Node setting. The agent proofs follow the
kagent API the cluster serves — the released kagent's Agent CRs on the stable
channel, kagent API v2's AgentTemplates on the [dev channel](platform.md#dev-channel)
— and say which at the start.

| Command | What it proves |
|---|---|
| `platform-test [email]` | Dex → muster → mcp-kubernetes → apiserver, the per-server OAuth sign-in challenge, the tool-group label on the fake fleet and, with observability on, mcp-prometheus and Backstage's metrics endpoint. See [The agent platform](platform.md). |
| `models-test [email]` | Managed models: 401 at the gateway without a token, then pull → ModelConfig → agent turn → MCP via muster → unload → delete, one backend per run. `--backend` picks one of `platform.modelManager.backends` (default: the first); `--model` a small, tool-calling capable model. See [Models](models.md). |
| `agents-test [email]` | agent-manager through muster as the signed-in user: create → ready → update → delete; a viewer's create is Forbidden by the apiserver; the ServiceAccount holds no RBAC of its own. On kagent API v2 the AgentTemplate carries the user's field manager, is Ready on the Harness and its toolset rides on the per-agent muster carrier. |
| `toolsets-test [email]` | Declared toolsets end to end: agent-manager requires one, the agent carries the header (the released kagent: on its Agent CR; kagent API v2: on the RemoteMCPServer its AgentTemplate binds), muster resolves and refuses per request, agents see their toolset through a turn on kagent, a per-server sign-in scopes a server's tools to the token, the portal's Tools step and apply path. `--model-config` picks the kagent ModelConfig the throwaway agents run on; `--skip-chat` skips the turns that need a model to answer; `--skip-portal` (kagent API v2) skips the portal's apply path. See [Toolsets](platform.md#toolsets-declared-tool-access). |
| `backstage-test [email...]` | The headless Backstage sign-in and the muster hop with that user's own forwarded token, including the per-server Sign in challenge, the MCP servers page's grouping and, on kagent API v2 with agents on, the Agent Platform pages — the proof brings a throwaway AgentTemplate along (a fresh lab has none), every user's agents list must show it, a chat turn on it for the first (default: every user). See [Backstage](backstage.md). |

## `configure` flags

`configure` probes the machine on every run and applies what it finds before
asking anything — see [what it
discovers](getting-started.md#configure-discovers-this-machine--on-every-run).
The flags pin a value regardless of the discovery, with or without
`--defaults`; nothing else in an existing file is touched.

| Flag | Effect |
|---|---|
| `--defaults` | Skip the form; keep the current values (or write the canonical lab) plus what the discovery finds. |
| `--accessible` | Prompt-per-question form mode, for screen readers and plain terminals. |
| `--platform[=false]` | Enable or disable the agent platform. Off gives a bare kind + Dex OIDC sandbox. |
| `--agents[=false]` | Enable or disable the agents runtime (kagent, part of the platform install). |
| `--observability[=false]` | Enable or disable the observability stack (Prometheus + mcp-prometheus). |
| `--backstage[=false]` | Enable or disable Backstage (implies the platform). |
| `--chart-version <x.y.z>` | The agent-platform chart release to install, an exact version (default: the release this agentlab was verified with, `config.DefaultChartVersion`). |
| `--chart-path <dir>` | Install the agent-platform chart from a local checkout's `helm/agent-platform` directory instead of the pinned release; `--chart-path ""` clears it. See [Installing an unreleased chart](platform.md#installing-an-unreleased-chart). |
| `--chart-branch <branch>` | The dev channel: follow this agent-platform branch's newest dev build — resolved now and on every `up`/`platform`, written to `chartVersion`; `--chart-branch ""` returns to the stable channel. Mutually exclusive with `--chart-path`; implies Substrate. See [Dev channel](platform.md#dev-channel). |
| `--substrate[=false]` | Pin Substrate (kagent's actor runtime) on or off instead of following the chart channel (on with `--chart-branch`, off otherwise). |
| `--model-manager[=false]` | Pin managed models on or off instead of following the host model servers the discovery finds (needs agents). |
| `--model-manager-backends ollama,lemonade` | Pin the host model servers, in order; the first is model-manager's default backend. |

## Environment variables

| Variable | Read by | Effect |
|---|---|---|
| `ANTHROPIC_API_KEY` | `up`, `platform` | Becomes the Secrets `kagent/kagent-anthropic` and `backstage/backstage-anthropic` at deploy time; never written to `agentlab.yaml` or `state/`. See [Agents](agents.md). |
| `<name>` per `extraModels[].apiKeyEnv` | `up`, `platform` | The key for that model config, same handling. See [Models](models.md). |
| `NODE_USE_SYSTEM_CA=1` | Node >= 22.15, Claude Code | Makes Node honor the system trust store after `agentlab trust`. Older Node: `NODE_EXTRA_CA_CERTS=$PWD/certs/ca.crt`. See [TLS](tls.md). |
| `KUBECONFIG` | your shell | Ignored by the lab: its embedded Helm and Kubernetes client are built from `state/kubeconfig` alone. `KUBECONFIG=state/kubeconfig kubectl ...` is the lab's view from a shell. |
| `AGENTLAB_TELEMETRY_OPTOUT`, `DO_NOT_TRACK=1` | every command | Disable the anonymous usage signal. See [Usage data](telemetry.md). |
| `AGENTLAB_TELEMETRY_TESTMODE=1` | every command | File the signals as test data and log delivery errors, for work on the lab itself. |
| `AGENTLAB_NO_UPDATE_CHECK=1` | every command | Silence the newer-release hint (below). |
| `GITHUB_TOKEN` | the update check | Lifts GitHub's anonymous rate limit; nothing else. |

## Keeping agentlab current

`agentlab self-update` replaces the running binary with the latest GitHub
release for your OS and architecture — the command muster and mcp-kubernetes
have too. `agentlab self-update --check` only reports the running and the
latest version, with exit status 125 when a newer one exists (for scripts). A
binary without a release version (`agentlab --version` says `dev`) is
refused: reinstall it from a release or with `go install`. A `go build` from
a checkout carries Go's pseudo-version (`v0.19.3-0.20260908…-8536d36`) and
is treated as what it is: after the tag before it, before the tag after it.

**Every release binary is verified before it is installed.** CI (the
architect orb) signs each `agentlab-<os>-<arch>` with cosign — keyless, the
CircleCI pipeline's identity, recorded in the Rekor transparency log — and
publishes the signature next to it as `agentlab-<os>-<arch>.bundle`.
`self-update` downloads both and installs the binary only after the bundle
verifies against the Sigstore public-good trust root for a CircleCI build of
`github.com/giantswarm/agentlab` (the shared
[`selfupdate-cosign`](https://github.com/giantswarm/selfupdate-cosign)
validator, the one muster and the other Giant Swarm CLIs use). A release
without a bundle for your platform is refused before anything is downloaded; a
download that does not match its signature is refused before anything is
written. Either way the installed binary stays as it is, and the error says
why. The hint below installs nothing, so it does not need the bundle.

Every command also starts with a one-line hint on stderr while a newer
release is out — devctl's per-command check, with two deliberate differences:

- **A hint, never a gate.** An outdated agentlab runs every command the same;
  nothing waits for you to update.
- **It gives up fast.** The GitHub round trip is capped at two seconds, and
  its answer is cached for an hour under your user cache directory
  (`~/.cache/agentlab/latest-release.json` on Linux,
  `~/Library/Caches/agentlab/` on macOS). A failed attempt is remembered for
  ten minutes, so a machine without internet is not held up on every command,
  and the last known answer keeps being shown meanwhile. A `GITHUB_TOKEN` in
  the environment is used when present; it only lifts GitHub's anonymous
  rate limit.

`AGENTLAB_NO_UPDATE_CHECK=1` silences the hint (`self-update` itself always
works); `dev` builds never check.
