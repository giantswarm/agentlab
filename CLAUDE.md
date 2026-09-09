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
- The Kubernetes tools come from the chart's `mcp-kubernetes` component
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
  the lab Prometheus) and overrides the chart's `mimirEnabled: false` in
  its app-config overlay (backstage-catalog.yaml.tmpl). mcp-prometheus is a
  lab-rendered Flux HelmRelease through the platform's bundled engine
  (mcp-prometheus.yaml.tmpl), not a helm install of its own.
- The agents runtime (kagent) installs with the platform by default but is
  optional (`platform.agents` in `agentlab.yaml`). Agents are Flux
  HelmReleases on every installation: Backstage's agent create flow
  (`/agents/new`) deploys by kube:applying Flux CRs through the scaffolder
  Template `agent-deployment` (embedded into the lab catalog from
  `templates/static/`), agent-manager writes the same objects, and the
  platform chart's bundled engine reconciles them — nothing watches git, and
  the lab installs no Flux of its own. Its default
  ModelConfig and Backstage's ai-chat both use `aiModel` from `agentlab.yaml`
  (Anthropic only); the API key comes from `$ANTHROPIC_API_KEY` on the host at
  deploy time and lives only in the Secrets `kagent/kagent-anthropic` and
  `backstage/backstage-anthropic` — never in `agentlab.yaml` or `state/`.
  Never inline a real key in config, templates, or rendered values.
  `platform.extraModels` adds further ModelConfigs (self-hosted
  OpenAI-compatible endpoints, OpenRouter, Gemini, Ollama) with the same
  env-var -> Secret key handling; entries removed from the config are pruned
  on the next run (see docs/models.md "Extra model configs").
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
- `platform.modelManager` installs the chart's model-manager component in
  front of EVERY backend of the list (model-manager >= 0.17.0 fronts several
  per instance; the first is its default backend, where a request that names
  none goes): their models become manageable from the portal and as
  `x_model-manager_<tool>` through muster, every pulled model auto-wired into
  kagent (native keyless `Ollama` provider; `OpenAI` for the servers behind
  an OpenAI-compatible API). Every object the API returns names its backend,
  every request may name one, and each ModelConfig carries the
  `model-manager.giantswarm.io/backend` label. Endpoints are autodetected
  (`docker network inspect kind` gateway + the server's default port) and
  every server is proven reachable from a pod before the install; the API
  sits behind the agentgateway route
  `https://agentgateway.<domain>/model-manager` with JWT validation on (a Dex
  token is required; 401 without). Proof: `./agentlab models-test` (see
  docs/models.md "Managed models"), one backend per run — `--backend <kind>` picks
  it, the default is the first of the list.
- For verifying RBAC as a specific user, use `./agentlab login <email>` and
  `kubectl --kubeconfig kubeconfig.oidc` — that is the OIDC path.
- The cluster's admin kubeconfig (`state/kubeconfig`, context `kind-agentlab`)
  bypasses the platform and OIDC entirely; use it only to debug the lab's own
  plumbing, never to demonstrate platform behavior. The lab never reads the
  shell's kubeconfig: every cluster-facing command exports the kind cluster's
  kubeconfig to `state/kubeconfig`, and its embedded Helm and Kubernetes
  client are built from that file alone (restclient.go, kube.go), so the
  proofs are deterministic about the cluster whatever the current-context is
  — `KUBECONFIG=state/kubeconfig kubectl ...` (or `helm ...`) is the same view
  from a shell. Your own `~/.kube/config` is never touched: kind is embedded
  (`internal/lab/kind.go`, `sigs.k8s.io/kind` as a pinned Go dependency — the
  Kubernetes version is its release's default node image), and it writes the
  admin kubeconfig to `state/kubeconfig` only.

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
./agentlab self-update     # replace the binary with the latest GitHub release (--check only reports; exit 125 when outdated)
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
- `internal/telemetry` — one anonymous usage signal per user-facing command
  to TelemetryDeck (giantswarm/telemetrydeck-go, kubectl-gs's signal shape:
  `GiantSwarm.command` with the command path and the version; the version
  and commit also as `TelemetryDeck.AppInfo.version`/`.buildNumber`, which
  the dashboard's standard insights read). `main.go`
  wires it as the root `PersistentPreRun`; hidden commands (`__complete`),
  `completion` and `help` never count. Opt-outs:
  `AGENTLAB_TELEMETRY_OPTOUT`, `DO_NOT_TRACK=1`. When iterating on the lab,
  `AGENTLAB_TELEMETRY_TESTMODE=1` keeps the runs out of the production
  numbers (and logs delivery errors). Details in docs/telemetry.md.
- `internal/update` — `agentlab self-update` (creativeprojects/go-selfupdate
  against the GitHub releases; the command muster and mcp-kubernetes ship).
  A release binary is installed only after its cosign Sigstore bundle
  (`agentlab-<os>-<arch>.bundle`, signed by the architect orb in CircleCI)
  verifies through the shared `github.com/giantswarm/selfupdate-cosign`
  validator; a release without a bundle or a download that does not verify is
  refused and the installed binary stays. Also `Remind`, the newer-release
  hint (no bundle needed: it installs nothing) the root `PersistentPreRun` prints on
  stderr before every user-facing command except `self-update`. A hint,
  never a gate: the command runs whatever the version. GitHub is asked at
  most once an hour (`latest-release.json` under `os.UserCacheDir()/agentlab`)
  with a two-second cap, and a failed attempt is remembered for ten minutes,
  so an offline machine is not held up. `AGENTLAB_NO_UPDATE_CHECK=1`
  silences it; `dev` builds never check and cannot self-update. Details in
  docs/cli.md "Keeping agentlab current".
- `pkg/project` — the build identity (`Version()`, `GitSHA()`,
  `BuildTimestamp()`): stamped by the devctl Makefile / architect CI through
  `-ldflags -X`, else Go's VCS build info (`v0.16.6`, a pseudo-version
  between tags, `+dirty`), else `dev`. `agentlab --version` prints it.
- `internal/lab` — the lifecycle. Templates in `templates/` are embedded and
  rendered via the `manifests` table in `render.go`; stamped manifests (dex,
  backstage) carry a checksum over render + certs, so unchanged re-applies are
  pure no-ops and config/cert edits roll the pod exactly once. The platform
  install (`platform.go`) is one upgrade-or-install with the kstatus wait of
  the agent-platform meta chart in its lab shape through the embedded Helm
  (`helm.go`: Helm 4's SDK in-process, no `helm` binary — it writes a regular
  release the CLI reads); the lab's patches on the
  component charts (hostNetwork, the dex-localhost sidecar, the kagent UI
  NodePort, dev images) are per-component `postRenderers` values the chart
  forwards to the component HelmReleases (`postrenderers.go`), and the image
  preload resolves the component charts from the rendered OCIRepositories
  (`fluxreleases.go`).
- `docs/` — the documentation, one page per topic (getting started, the
  command reference, TLS, the platform, agents, models, observability,
  Backstage, identity, troubleshooting, usage data, development).
  `README.md` is the short front door: what the lab is, the quick start and
  the page index — keep it that way and put detail in `docs/`. Code comments
  cite pages by path and section (`docs/models.md "Local backends on the lab
  host"`), so keep those section titles stable or update the citations.

Load-bearing invariants (details in docs/):

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
- **The lab shape of the agent-platform chart**: the bundled Flux engine ON
  (`components.flux.enabled: true` — the lab has no Flux of its own, and the
  chart refuses a second one) and self-management OFF (`gitops.self.enabled:
  false` — the lab installs unreleased charts and dev images, which the
  chart's own HelmRelease would replace with the published release; the
  lab's embedded Helm stays the one writer of the release, and the Helm CLI
  its day-2 tool). The chart is pinned to an exact
  release (`platform.chartVersion`, default `config.DefaultChartVersion`);
  `platform.chartPath` installs a local checkout instead. Never emit
  `gitops.namespace` with the engine on.
- The lab's credentials are throwaway by design; plaintext passwords in
  `agentlab.yaml` are fine.

`HACKS.md` is the audit journal of every hack/workaround and its status —
record new ones there, one commit per resolved item.
