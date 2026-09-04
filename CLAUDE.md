# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A local lab for the **Giant Swarm agent platform** in one Go binary
(`agentlab`): muster + the Kubernetes MCP server (and optionally Giant Swarm
Backstage) on a throwaway kind cluster, so the platform can be tested and
demoed end to end. The platform needs an identity provider, so the lab bundles
its own Dex — throwaway users that exist nowhere else, RBAC driven by the
`groups` claim, the apiserver, muster and Backstage all trusting the same
issuer. All configuration lives in `agentlab.yaml` (created by `agentlab
configure`); every manifest renders from templates embedded in the binary into
`state/` (gitignored). There is no YAML to hand-edit and no shell scripts.

## Always use the lab and its MCP

Testing the agent platform is this repo's purpose, and `.mcp.json` registers
the **`musterkind`** MCP server (`https://muster.127.0.0.1.nip.io/mcp`) —
muster running *inside* the lab cluster, reached through the agentgateway
edge. Interacting with the cluster through it is the point: it exercises the
whole Claude Code → agentgateway → muster → mcp-kubernetes → apiserver chain,
with Dex doing the logins.

- The edge serves a lab-CA certificate. Either the CA is in the system trust
  store (one-time `./agentlab trust`; then launch Claude Code with
  `NODE_USE_SYSTEM_CA=1`, Node >= 22.15) or launch with
  `NODE_EXTRA_CA_CERTS=<repo>/certs/ca.crt` — without one of the two the
  connection fails on TLS. (Fallback for a shell without either: the direct,
  edge-bypassing `http://localhost:8090/mcp`.) Never install trust silently:
  `agentlab trust` is the user's explicit, sudo-gated step.
- If `musterkind` is unreachable or unauthenticated, the lab is down — bring
  it up instead of switching tools: `./agentlab configure --defaults` (once;
  the platform and Backstage are enabled by default), then `./agentlab up`,
  then authenticate via `/mcp` (Dex browser login; users and passwords are in
  `agentlab.yaml`, default `admin@lab.local` / `password`).
- The Kubernetes tools come from the umbrella's bundled `mcp-kubernetes`
  MCPServer and use muster's per-server prefixing: `x_mcp-kubernetes_<tool>`
  (e.g. `x_mcp-kubernetes_list`), no `management_cluster` argument.
- muster's OAuth *client* role is on (`oauth.mcpClient`), and the lab ships
  one `Auth Required` downstream to sign in to: the MCPServer
  `lab-oauth-fixture`, which points muster at its own protected `/mcp`. It
  exists for the per-server sign-in path (`core_auth_login`, the portal's
  Sign in button); `platform-test` and `backstage-test` assert the challenge.
  Not a real integration — never "fix" its Auth Required state, and after a
  muster pod roll it reads `Failed` for about a minute by design.
- With `platform.observability: true` (the default), a minimal Prometheus
  (the GS kube-prometheus-stack constituent of the observability bundle, with
  the server re-enabled) and mcp-prometheus install too; the tools surface as
  `x_mcp-prometheus_<tool>` (e.g. `x_mcp-prometheus_execute_query`) — the way
  to answer CPU/memory questions about the lab. Chart pins are Go consts in
  `internal/lab/observability.go`; the bundle itself is deliberately NOT
  installed (MC-shaped: Flux HelmReleases, Alloy -> Mimir, no local PromQL).
  Backstage's Clusters/Deployments metrics work too: the lab serves the
  Mimir-shaped endpoint (`observability.<domain>/prometheus` on the edge →
  the lab Prometheus) and overrides the umbrella's `mimirEnabled: false` in
  its app-config overlay (backstage-catalog.yaml.tmpl).
- The agents runtime (kagent) installs with the platform by default but is
  optional (`platform.agents` in `agentlab.yaml`) — on real clusters agent
  delivery runs through Flux/GitOps, which the lab does not run as a GitOps
  loop. Backstage's agent create flow (`/agents/new`) deploys by kube:applying
  Flux CRs through the scaffolder Template `agent-deployment` (embedded into
  the lab catalog from `templates/static/`), so `agentlab backstage` also
  installs Flux's source+helm controllers as the delivery engine when agents
  are enabled — nothing watches git. Its default
  ModelConfig and Backstage's ai-chat both use `aiModel` from `agentlab.yaml`
  (Anthropic only); the API key comes from `$ANTHROPIC_API_KEY` on the host at
  deploy time and lives only in the Secrets `kagent/kagent-anthropic` and
  `backstage/backstage-anthropic` — never in `agentlab.yaml` or `state/`.
  Never inline a real key in config, templates, or rendered values.
  `platform.extraModels` adds further ModelConfigs (self-hosted
  OpenAI-compatible endpoints, OpenRouter, Gemini, Ollama) with the same
  env-var -> Secret key handling; entries removed from the config are pruned
  on the next run (see README "Extra model configs").
- `agentlab configure` **discovers this machine on every run** (fresh or
  existing `agentlab.yaml`): the tools `up` shells out to, whether this
  configuration's kind node exists and which host ports it publishes (never
  conflicts; while no node exists, occupied ports move to free ones), the
  host model servers — an Ollama on 11434, a Lemonade Server on 13305 —
  with their downloaded tool-calling models, a standalone `flm serve`
  (report-only), and `$ANTHROPIC_API_KEY`. What answers becomes
  `platform.modelManager.backends` (Ollama first); `--model-manager[=false]`
  and `--model-manager-backends` pin it. Never hand-edit that list to
  describe the machine — re-run `configure --defaults`.
- `platform.modelManager` installs the umbrella's model-manager component
  with the FIRST backend of the list (model-manager fronts one backend per
  instance today): that server's models become manageable from the portal
  and as `x_model-manager_<tool>` through muster, every pulled model
  auto-wired into kagent (native keyless `Ollama` provider; `OpenAI` on
  `/api/v1` for Lemonade). Every FURTHER backend is wired statically by
  `agentlab platform`: its downloaded tool-calling models become
  lab-labeled ModelConfigs `<backend>-<model>` (hostmodels.go), refreshed and
  pruned on every run — the interim until model-manager is multi-backend
  (agentlab#60, HACKS.md U14). Endpoints are autodetected (`docker network
  inspect kind` gateway + the server's default port) and every server is
  proven reachable from a pod before the install; the API sits behind the
  agentgateway route `https://agentgateway.<domain>/model-manager` with JWT
  validation on (a Dex token is required; 401 without). Proof:
  `./agentlab models-test` (see README "Managed models"), which ends with an
  agent turn on a statically wired model when a further backend is listed.
- For verifying RBAC as a specific user, use `./agentlab login <email>` and
  `kubectl --kubeconfig kubeconfig.oidc` — that is the OIDC path.
- The kind admin context (`kind-agentlab`) bypasses the platform and OIDC
  entirely; use it only to debug the lab's own plumbing, never to demonstrate
  platform behavior. The lab's own `kubectl`/`helm` never read the shell's
  kubeconfig: every cluster-facing command exports the kind cluster's
  kubeconfig to `state/kubeconfig` and pins `KUBECONFIG` to it (exec.go), so
  the proofs are deterministic about the cluster whatever the current-context
  is — `KUBECONFIG=state/kubeconfig kubectl ...` is the same view from a shell.

## Commands

```bash
make build                 # go build -o agentlab .
make test                  # go test ./...
go test ./internal/forms/ -run TestMinimalFormDrive -count=1 -v   # single test

./agentlab configure       # interactive form; --defaults keeps/writes the canonical lab
./agentlab up              # certs, kind cluster, Dex, RBAC, the agent platform — verified
./agentlab platform-test   # headless Dex -> muster -> mcp-kubernetes proof
./agentlab models-test     # managed models: 401 -> pull -> ModelConfig -> agent turn -> MCP -> unload -> delete
./agentlab test            # RBAC assertions for every configured user
./agentlab backstage-test  # headless Backstage sign-in for every user
./agentlab down            # delete the cluster (certs/ kept, trust stores untouched)
./agentlab trust           # install the lab CA into the system + NSS trust stores (sudo)
./agentlab untrust         # remove exactly the lab CA from those stores
./agentlab reload          # re-render + re-apply Dex after editing agentlab.yaml
./agentlab logs <dex|muster|backstage>
./agentlab render          # write every manifest to state/ without applying
```

The lab's own e2e checks are the `*-test` subcommands, not `go test`.

## Architecture

- `main.go` — cobra wiring only; no logic.
- `internal/config` — the `agentlab.yaml` schema, validation, and the
  cross-cutting constants: the fixed group vocabulary
  (`platform-admins`/`developers`/`viewers`, bound to cluster-admin /
  edit-in-demo / view) and the static OAuth client IDs/secrets. **These
  constants also appear in the embedded templates and must stay in
  agreement.** Password bcrypt hashes are cached in `agentlab.yaml` on purpose
  so renders stay byte-identical (no spurious pod rolls).
- `internal/forms` — the huh configuration form; tests drive it with scripted
  keystrokes.
- `internal/lab` — the lifecycle. Templates in `templates/` are embedded and
  rendered via the `manifests` table in `render.go`; stamped manifests (dex,
  backstage) carry a checksum over render + certs, so unchanged re-applies are
  pure no-ops and config/cert edits roll the pod exactly once. The platform
  install (`platform.go`) uses the binary itself as a Helm post-renderer
  (`postrender.go`: hostNetwork, the DCR chart-bug fix, HTTPRoute strip).

Load-bearing invariants (details in README.md):

- **The agent platform is on by default** — it is what the lab tests. Dex,
  kind and the RBAC exist to serve it; muster is the single auth enforcement
  point, and `mcp-kubernetes` is deliberately unauthenticated on the cluster
  network. kagent (the agents runtime) is an optional part of the platform
  install (on by default), with `controller.auth.mode: unsecure` because the
  lab runs no JWT-validating front proxy.
- **One issuer URL from every vantage point**: `https://localhost:<dexPort>/dex`
  works from the Mac, inside the node, and inside hostNetwork pods because the
  Dex NodePort equals the kind host port. The issuer must be spelled
  `localhost`, not `127.0.0.1` — muster rejects IP-literal loopback issuers.
- The agent-platform chart has no release yet, so `agentlab platform` vendors
  it from git at the SHA pinned in `agentlab.yaml` into `.vendor/` (not
  `vendor/`, which would flip the Go toolchain into vendored-build mode).
- The lab's credentials are throwaway by design; plaintext passwords in
  `agentlab.yaml` are fine.

`HACKS.md` is the audit journal of every hack/workaround and its status —
record new ones there, one commit per resolved item.
