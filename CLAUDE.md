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
  store (one-time `agentlab trust`; then launch Claude Code with
  `NODE_USE_SYSTEM_CA=1`, Node >= 22.15) or launch with
  `NODE_EXTRA_CA_CERTS=~/.local/state/agentlab/labs/<lab>/certs/ca.crt` — without one of the two the
  connection fails on TLS. (Fallback for a shell without either: the direct,
  edge-bypassing `http://localhost:8090/mcp`.) Never install trust silently:
  `agentlab trust` is the user's explicit, sudo-gated step.
- If `musterkind` is unreachable or unauthenticated, the lab is down — bring
  it up instead of switching tools: `agentlab configure --defaults` (once;
  the platform and Backstage are enabled by default), then `agentlab up`,
  then authenticate via `/mcp` (Dex browser login; users and passwords are in
  `agentlab.yaml`, default `admin@lab.local` / `password`).
- The lab registers its own cluster as the infrastructure families, the way
  an installation registers a management cluster: the chart's
  `mcp-kubernetes` component is the MCPServer `<clusterName>-mcp-kubernetes`,
  a member of muster's `kubernetes` family, so the tools are
  `x_kubernetes_<tool>` with `management_cluster: <clusterName>-mcp-kubernetes`
  (e.g. `x_kubernetes_list` with `management_cluster: agentlab-mcp-kubernetes`).
- muster's OAuth *client* role is on (`oauth.mcpClient`), and the lab ships
  one `Auth Required` downstream to sign in to: the MCPServer
  `lab-oauth-fixture`, which points muster at its own protected `/mcp`. It
  exists for the per-server sign-in path (`core_auth_login`, the portal's
  Sign in button); `platform-test` and `backstage-test` assert the challenge.
  Not a real integration — never "fix" its Auth Required state, and after a
  muster pod roll it reads `Failed` for about a minute by design.
- With `platform.observability: true` (the default), a minimal Prometheus
  (the GS kube-prometheus-stack constituent of the observability bundle, with
  the server re-enabled) and mcp-prometheus install too, registered as the
  `prometheus` family's member `<clusterName>-mcp-prometheus`; the tools
  surface as `x_prometheus_<tool>` with
  `management_cluster: <clusterName>-mcp-prometheus`
  (e.g. `x_prometheus_execute_query`) — the way
  to answer CPU/memory questions about the lab. Chart pins are Go consts in
  `internal/lab/observability.go` (`platform.mcpPrometheusChartVersion`
  overrides mcp-prometheus's, for a candidate whose chart changed); the bundle itself is deliberately NOT
  installed (MC-shaped: Flux HelmReleases, Alloy -> Mimir, no local PromQL).
  Backstage's Clusters/Deployments metrics work too: the lab serves the
  Mimir-shaped endpoint (`observability.<domain>/prometheus` on the edge →
  the lab Prometheus) and overrides the chart's `mimirEnabled: false` in
  its app-config overlay (backstage-catalog.yaml.tmpl). mcp-prometheus is a
  lab-rendered Flux HelmRelease through the platform's bundled engine
  (mcp-prometheus.yaml.tmpl), not a helm install of its own; its
  `platform.devImages.mcp-prometheus` swap rides that release's
  postRenderers. `platform.alertmanager` (off by default) adds the
  Alertmanager, an always-firing `AgentlabWatchdog` and `ALERTMANAGER_URL`,
  for the alert tools (docs/observability.md "Alertmanager"). Off, `platform`
  removes what an earlier run installed (`observabilityDown`; the operator
  CRDs stay) and `status` names a leftover on its `prometheus` line.
- The agents runtime (kagent) installs with the platform by default but is
  optional (`platform.agents` in `agentlab.yaml`). Agents are Flux
  HelmReleases of the Generic agent chart (1.x) on every installation:
  Backstage's agent create flow (`/agents/new`) deploys through
  agent-manager's `create_agent` over muster as the signed-in person, the
  same tool an MCP session drives, and the platform chart's bundled engine
  reconciles the HelmRelease — nothing watches git, and the lab installs no
  Flux of its own. Its default
  ModelConfig and Backstage's ai-chat both use `aiModel` from `agentlab.yaml`
  (Anthropic only); the API key lives only in the Secrets
  `kagent/kagent-anthropic` and the portal's `agent-platform/agentlab-anthropic`
  — never in `agentlab.yaml` or `state/` — which
  every `up`/`platform` fills (anthropic.go): from the reference
  `aiKey.source` records through `beekeeper secret copy --to-secret`
  (agentlab never reads the value; a source that cannot be placed fails the
  run), else from `$ANTHROPIC_API_KEY` on the host, else with a placeholder
  so the ModelConfig resolves (agent turns then fail until the key is
  placed). Never inline a real key in config, templates, or rendered values.
  `platform.extraModels` adds further ModelConfigs (self-hosted
  OpenAI-compatible endpoints, OpenRouter, Gemini, Ollama) with the same
  env-var -> Secret key handling; entries removed from the config are pruned
  on the next run (see docs/models.md "Extra model configs").
- `agentlab configure` **discovers this machine on every run** (fresh or
  existing `agentlab.yaml`): the tools `up` shells out to, whether this
  configuration's kind node exists and which host ports it publishes (never
  conflicts; while no node exists, occupied ports and those another
  registered lab records, running or stopped, move to free ones, and so does
  a `platform.domain` another lab records: lab n to `127.0.0.<n>:443` with
  `lab<n>.127.0.0.<n>.nip.io`), the
  host model servers — an Ollama on 11434, a Lemonade Server on 13305, an
  LM Studio on 1234 (0.4.0+) — with their downloaded tool-calling models,
  each recognised by the SHAPE of its answer and never by a status code
  (LM Studio answers 200 with an error body for every path outside its
  `/api/v1`, Ollama's `/api/version` included, and reports no version
  anywhere — its discovery line reads `api v1`), a standalone `flm serve`
  (report-only), and `$ANTHROPIC_API_KEY`. What answers becomes
  `platform.modelManager.backends` (Ollama first); whether managed models
  are on follows that on the first configure only — a later `configure`,
  `--defaults` included, keeps the lab's recorded choice — and
  `--model-manager[=false]` and `--model-manager-backends` pin both. Never
  hand-edit that list to describe the machine — re-run `configure --defaults`.
- `platform.modelManager` installs the chart's model-manager component in
  front of EVERY backend of the list — and off states
  `components.model-manager.enabled: false` in the rendered values (the chart
  runs the component by default since agent-platform 4.24.0, with no backend) (model-manager >= 0.17.0 fronts several
  per instance; the first is its default backend, where a request that names
  none goes): their models become manageable from the portal and as
  `x_model-manager_<tool>` through muster, every pulled model auto-wired into
  kagent (native keyless `Ollama` provider; `OpenAI` for the servers behind
  an OpenAI-compatible API). Every object the API returns names its backend,
  every request may name one, and each ModelConfig carries the
  `model-manager.giantswarm.io/backend` label. Endpoints are autodetected —
  the `docker network inspect kind` gateway, or the container runtime's host
  alias (`host.docker.internal`, podman's `host.containers.internal`) where
  that gateway is a bridge inside the runtime's VM, whichever answers when
  dialled from inside the node; the gateway is wired by the name
  `host.agentlab.internal` (a CoreDNS hosts entry), because kagent injects
  an OpenAI-provider ModelConfig's key only for a DNS hostname — and a
  server pods cannot reach on either is
  reported and left out of the backends list. Every server is proven reachable
  from a pod before the install; every
  client — the portal, the proofs, an agent — calls its tools through muster
  as the signed-in person (no edge route of its own; muster answers 401
  without a Dex token). Proof: `agentlab models-test` (see
  docs/models.md "Managed models"), one backend per run — `--backend <kind>` picks
  it, the default is the first of the list.
- The `lmstudio` backend is the one that cannot delete: LM Studio has no
  delete over its API (`lms rm` on the host only), so model-manager reports
  `delete: false` and `agentlab models-test --backend lmstudio` asserts the
  501 refusal, checks the model is still downloaded and still wired, then
  tears the ModelConfig down with `POST /models/unwire`. It is the one
  models-test run that leaves the pulled model behind, by design — never
  "fix" that into a skipped step. Its default proof model is
  `ibm/granite-4-micro`; keep LM Studio's just-in-time loading on, or the
  agent turn fails instead of waiting for the load.
- `platform.vmManager` runs **vm-manager as a pod of the node** (the
  platform's VM provisioner: KVM VMs with IMDS, vTPM and attestation), the
  chart's `components.vm-manager` — like agent-manager and model-manager,
  never a host service. The node is a privileged container, so the host's
  `/dev/kvm` and `/dev/vhost-vsock` are in it and the runtime hands them to
  the pod — nothing is mounted from the node, the chart has no hostPath. The
  guest image is an OCI artifact the pod's init container fetches: the one
  the chart's release published, or a local build (`platform.vmManager.imageDir`,
  a vm-manager checkout's `images/build`) that `agentlab platform` pushes into
  the lab registry (`<cluster>-registry:5000`) and pins by digest, so a
  rebuilt image rolls the pod on the next run — no `down && up`.
  `platform.devImages.vm-manager` swaps a local build of the binary in. The
  chart brings OAuth against the lab Dex and the MCPServer `vm-manager`
  (`x_vm-manager_<tool>`, tool group `agent-platform`, forward-token auth); the
  lab gives the pod a state claim, so the fetched image and the golden PCR
  values survive a restart. `configure` turns the key off on a machine without
  the devices and never on by itself (`--vm-manager`); `up`/`platform` refuse it
  on a node without the devices, with the fix. The proof: 401
  anonymous from inside the cluster, the person's token accepted, the tools
  through muster with annotations, then create_vm → ready → attestation →
  delete_vm (`--skip-vm` boots nothing). The golden PCR values are the pod's
  OVMF's (pinned per vm-manager release, `get_host`'s `firmware`), recorded
  once per firmware build and guest image inside the pod (`kubectl exec …
  vm-manager image golden`, `--clear` first over stale values,
  docs/vm-manager.md).
- `platform.klausGateway` runs **Swarmgeist (klaus-gateway) as the meta
  chart's in-cluster component** (`components.klaus-gateway`), the shape
  every installation runs next to the host-mode gateway
  `klaus-gateway-test` starts for the public leg: A2A on the in-cluster
  controller target, the Slack adapter on a placeholder Secret
  (`agentlab-klaus-gateway-slack`; no workspace answers, the lab's
  postRenderer points its Web API at the Service `agentlab-slack-api` the
  proof aims at its fake, a container on the kind network — never a port on
  the host, whose firewall is the owner's), the OBO link store in a Secret
  (`obo.store: secret`, keys in `agentlab-klaus-gateway-obo`, generated
  once — never regenerate them, the store key seals every link).
  `platform.devImages.klaus-gateway` swaps a build in. The proof's component
  half: the Role scoped to the link Secret, two links seeded through
  `pkg/auth/musterlink`, the pod deleted and its replacement Ready with the
  same links, a Slack turn through the pod. A real OBO sign-in is out of reach
  here (docs/klaus-gateway.md): the proof links the person with a record
  carrying the lab user's own Dex id_token — never "fix" that with a fake
  muster identity.
- For verifying RBAC as a specific user, use `agentlab login <email>` and
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
  admin kubeconfig to `state/kubeconfig` only. The one other file the lab
  writes is a copy of that kubeconfig the shell's `KUBECONFIG` names (every
  entry `kind-<clusterName>` — a lab lease holds one): the export refreshes
  it once the cluster behind it was recreated, so a copy taken before `down`
  and `up` reaches the new cluster without a manual step.

## Commands

```bash
make build                 # CGO_ENABLED=0 go build -o agentlab .: a static binary, which the lab's containers need
make test                  # go test ./...
make lint                  # golangci-lint as CI's pre-commit runs it (pinned version, .golangci.yaml, test files included)
go test ./internal/forms/ -run TestMinimalFormDrive -count=1 -v   # single test

agentlab configure       # interactive form; --defaults keeps/writes the canonical lab
agentlab up              # certs, kind cluster, Dex, RBAC, the agent platform — verified
                           # on a terminal it ends by offering `trust` and the portal (--trust/--open pre-answer)
agentlab open portal     # the portal (Backstage) in the browser; `open agents` the kagent UI, `open prometheus` the lab Prometheus
agentlab list            # the labs on this machine: state, components, URLs, CA trust
agentlab status          # the lab's live state: the chart in place (a dev chart named as such) vs agentlab.yaml's, every platform.valuesFiles overlay with what it pins, the platform HelmReleases
agentlab configure --released  # the restore step before handing a lab on: drops overlays, dev images, chart directory, dev channel
agentlab pods            # the lab's pods (kubectl get pods -A, without kubectl); -n <namespace>
agentlab platform-test   # headless Dex -> muster -> mcp-kubernetes proof
agentlab polex-test      # the chart's Kyverno PolicyExceptions dry-run against the lab's Kyverno (--chart-version <release> for a candidate)
agentlab models-test     # managed models: 401 -> pull -> ModelConfig -> dry runs, gitops_owned -> agent turn -> MCP -> unload -> delete (on lmstudio: the 501 refusal -> unwire, U23)
agentlab serving-test    # platform.serving: llm-d controller + models Gateway up -> 401 -> the lab preset fits the node -> load -> Ready on the CPU runtime -> ModelConfig -> a completion through the models Gateway -> agent turn -> unload
agentlab vm-manager-test # the vm-manager pod as the person: 401 anonymous -> tools via muster -> create_vm -> ready -> attestation -> delete_vm
agentlab klaus-gateway-test # Swarmgeist on the host against the edge, and with platform.klausGateway the in-cluster component: the OBO link store in a Secret across a pod loss
agentlab decisions-test  # klaus-gateway's POST /decisions as a ServiceAccount: answered by click, modal and thread reply, each a muster tool call as the user; a refusal, a close as defaulted
agentlab beekeeper-decisions-test  # beekeeper serve behind muster puts decisions to people through klaus-gateway: a click (another user's refused), a thread reply, a default at the due time, a withdrawal; the guide conversing with its person in a direct message
agentlab beekeeper-central-test --part notes  # beekeeper's central notes through two lab users' CLIs: note add --for the other, listed and answered on the other's machine, the answer printed once by the filer's watch across a restart; a withdrawal only its filer may make; a fold into and a park on a central note, the park settled with the answer once
agentlab test            # RBAC assertions for every configured user
agentlab backstage-test  # headless Backstage sign-in for every user, the portal's Agent Platform and Models pages (--agent kagent/<name>: on an existing agent, no create or delete)
agentlab tenants-test    # platform.tenants: two organization namespaces, an editor in each: the reconciler's copies with the first agent, the other organization refused, a turn on the namespace's own credentials, the edge's RBAC authorizer and klaus-gateway as two tenant users bound by group (own roster, the other organization refused), muster's MCP servers per tenant (listed as the person, another organization's answered as missing, a tenant sign-in kept in its namespace), the Dev Portal's routes as each person (roster per tenant, the create flow's tenants, the access reviews behind Edit and Delete, a refused session start, the creator), the workers held against a cluster-admin, the copies gone with the last agent
agentlab coding-test     # platform.coding: the coding Harness from the values, agent-manager's guards, a coding agent's sandbox floor (uid split, forwarder paths, a write tool refused, no credentials, DNS canary, egress), a workspace Session's MCP call audited by muster as the person, nothing in the sandbox running between turns, the instruction fixture, suspend/resume, the turn's usage, a share refused, no muster call of the actor accepted after delete_session
agentlab coding-test --part devctl  # platform.devctl: a coding agent opens a pull request as the person, waits for its CI (one pr_wait), merges it (pr_merge) and waits for its release (release_wait) through devctl's MCP server; one pr_wait with a progressToken beside it
agentlab coding-test --part toolchains  # agent-manager's toolchains: start_session with --toolchains (default go) mounts them pinned by digest; a turn finds each tool under /opt/kagent/tools/<name> on its PATH, with go builds a fixture module; the actor template mounts those digests; --other-version go@0.1.0 adds a second Session at that version and a resume of the first (--harness claude|codex)
agentlab egress-test     # an agent with one declared origin, probed from its actor and judged from the egress gateway's record, never the reply: the origin forwarded, an undeclared host 403 (actor egress policy denied), muster forwarded without an entry, raw TCP to github.com:22 held at the gate
agentlab chatgpt-test    # platform.chatgptSubscription: codex on a person's own ChatGPT login from the chart's values (--connect: OpenAI's device code), judged from the egress gateway's record: the turn on the login, placeholders in the sandbox, a second person and a non-codex agent refused, the disconnect
agentlab skills-test     # kagent API v2: an AgentTemplate with a git-pinned skill boots (the golden boot) and answers from the skill
agentlab substrate-step-test --to <release>  # a suspended conversation and its /data file survive the step to a newer Substrate; a new one starts from a golden in the layout it writes (taken again on a layout change, kept otherwise)
agentlab kagent-step-test --to <kagent> | --chart <agent-platform>  # a suspended conversation and a file on its durable volume survive: it resumes, takes a turn reading both back, suspends and deletes across a kagent step (a kagent pin, a platform candidate — chart-only included — or both)
agentlab substrate-tunnel-test  # a gRPC call through atenet-router to an HTTP/1.1-only actor fails at once, not with its client's timeout (--substrate <release> pins one for the run)
agentlab down            # delete the cluster (certs/ kept, trust stores untouched)
agentlab trust           # install the lab CA into the system + NSS trust stores (sudo)
agentlab untrust         # remove exactly the lab CA from those stores
agentlab reload          # re-render + re-apply Dex after editing agentlab.yaml
agentlab logs <dex|muster|backstage>
agentlab login <email>   # headless password grant; --browser for the real Dex login page
agentlab render          # write every manifest to state/ without applying
agentlab self-update     # replace the binary with the latest GitHub release (--check only reports; exit 125 when outdated; --pre follows release candidates)
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
- `internal/xdg` — the freedesktop folders: configuration
  (`$XDG_CONFIG_HOME/agentlab`), state (`$XDG_STATE_HOME/agentlab`), cache
  (`$XDG_CACHE_HOME/agentlab`) and the binary (`~/.local/bin`, where
  `self-update` installs it and moves a release installed elsewhere, leaving a
  link). Tests never reach the person's folders: `TestMain` points every one
  of them and `HOME` at a temporary directory in each package that enters,
  settles or installs.
- `internal/labs` — the registry of the labs on this machine
  (`~/.config/agentlab/labs.yaml`, cluster name → lab directory), the one
  order every command resolves its lab in (`--lab`, `./agentlab.yaml`, the one
  registered lab, a picker on a terminal) and `Settle`: a lab lives in
  `$XDG_STATE_HOME/agentlab/labs/<name>` (its directory: `certs/`, `state/`)
  with its `agentlab.yaml` in `$XDG_CONFIG_HOME/agentlab/labs/<name>/`, linked
  from the directory. A lab elsewhere moves there by renames on the first
  command that enters it, leaving links at the old paths — the kind node
  mounts `certs/` and `state/node` by absolute path, and an older release run
  there still finds the lab. An `agentlab.yaml` file of its own in the lab
  directory moves to the configuration folder, the link in its place; one
  differing from the configuration folder's is refused, both kept
  (`adoptConfig`). The command then changes into the lab
  directory, so the lab's cwd-relative paths hold. The read-only commands
  (`trust`, `status`, `open`, `pods`, `logs`, `list`) read an `agentlab.yaml`
  a newer release wrote (`config.LoadReadOnly`, `Peek`); every writer keeps
  the strict refusal.
- `internal/telemetry` — anonymous usage signals to TelemetryDeck
  (giantswarm/telemetrydeck-go). One per user-facing command, kubectl-gs's
  signal shape: `GiantSwarm.command` with the command path and the version
  (the version and commit also as `TelemetryDeck.AppInfo.version`/
  `.buildNumber`, which the dashboard's standard insights read); `main.go`
  wires it as the root `PersistentPreRun`; hidden commands (`__complete`),
  `completion` and `help` never count. And one per platform install,
  `GiantSwarm.agentlab.platform`, sent by `platformUp` (so `up` and
  `platform`, nothing else — a test in `internal/lab` pins the call sites)
  once the chart is resolved: `chartVersion`, `chartMajor`, `chartChannel`
  (stable/dev/path), `chartPinned`, `legacyShape` and the feature switches as
  booleans — never a path (a chart directory reports its Chart.yaml version),
  host name, user or token. Both signals share one client, so the one
  `Flush` covers both. The command signal waits for `enterLab`, which hands
  it the lab's deployment (`telemetry.EnterLab`; `Flush` sends it for a
  command without a lab): a random ID and salt the first `up` after a `down`
  records in `state/telemetry-deployment.json` (`lab.StartTelemetryDeployment`)
  and `down` removes. Every signal after it carries `deploymentID`, and the
  Backstage overlay's `app.telemetrydeck` reports the portal into the same app
  under that ID and salt — `null` with usage data off. Opt-outs:
  `AGENTLAB_TELEMETRY_OPTOUT`, `DO_NOT_TRACK=1`. When iterating on the lab,
  `AGENTLAB_TELEMETRY_TESTMODE=1` keeps the runs out of the production
  numbers; `AGENTLAB_TELEMETRY_DEBUG=1` alone logs delivery problems — a
  command's output never carries telemetry, and `Flush` stops waiting once no
  request has connected within `connectTimeout`. The user identifier is agentlab's own,
  not the library's: `internal/telemetry/machineid` reads the identifier the
  OS keeps for the computer (macOS `kern.uuid`, Linux `/etc/machine-id`,
  Windows `MachineGuid`) and `telemetry.go` hashes it with the OS user name
  and a fixed salt into `WithUserID`; a machine that exposes none falls back
  to the library's derived default. **Changing that salt or the layout of the
  hashed string resets every user in the TelemetryDeck dashboard** — the
  pinned digest in the tests is there to make that deliberate. Details in
  docs/telemetry.md.
- `internal/update` — `agentlab self-update` (creativeprojects/go-selfupdate
  against the GitHub releases; the command muster and mcp-kubernetes ship).
  A release binary is installed only after its cosign Sigstore bundle
  (`agentlab-<os>-<arch>.bundle`, signed by the architect orb in CircleCI)
  verifies through the shared `github.com/giantswarm/selfupdate-cosign`
  validator; a release without a bundle or a download that does not verify is
  refused and the installed binary stays. `--pre` (`Options.Pre`) counts
  GitHub prereleases too, newest in semver order (rc.6 < the release < the
  next rc.1), verified the same way; without it, and in the hint and its
  cache, only stable releases count. Also `Remind`, the newer-release
  hint (no bundle needed: it installs nothing) the root `PersistentPreRun` prints on
  stderr before every user-facing command except `self-update`. A hint,
  never a gate: the command runs whatever the version. GitHub is asked at
  most once an hour (`latest-release.json` under `os.UserCacheDir()/agentlab`)
  with a two-second cap, and a failed attempt is remembered for ten minutes,
  so an offline machine is not held up. `AGENTLAB_NO_UPDATE_CHECK=1`
  silences it; `dev` builds never check and cannot self-update. Details in
  docs/cli.md "Keeping agentlab current". `agentlab.yaml`'s header records
  the agentlab that wrote it (`# Written by agentlab <version>.`, a comment
  so older releases read past it), and `decodeStrict`'s refusal of an
  unknown field names that version and the self-update that installs it.
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
  install (on by default); its controller runs in `trusted-proxy` mode behind
  the edge's JWT policy on the current line (`unsecure` on the released 3.x
  line, which runs no JWT-validating front proxy).
- **One issuer URL from every vantage point**: `https://localhost:<dexPort>/dex`
  works from the Mac, inside the node, and inside hostNetwork pods because the
  Dex NodePort equals the kind host port. The issuer must be spelled
  `localhost`, not `127.0.0.1` — muster rejects IP-literal loopback issuers.
  With an externally provisioned pair (`platform.tls`) it is
  `https://dex.<domain>:<dexPort>/dex` instead, served with that pair: the
  apiserver resolves it through its own hosts file, pods through a CoreDNS
  rewrite to the `dex-issuer` Service (no sidecar) — docs/tls.md "Bring your
  own certificate".
- **muster runs on the host network at one replica** by default, the issuer
  reached as the host does. `platform.musterPodNetwork` (`configure
  --muster-pod-network`) is the installation's shape instead: two replicas
  on the pod network behind a PodDisruptionBudget of minAvailable 1, the
  issuer bridged by the `dex-localhost` sidecar, proven by `platform-test`'s
  `muster replicas` stage (one pod deleted mid-session) — docs/platform.md
  "muster on the pod network".
- **The lab shape of the agent-platform chart**: the bundled Flux engine ON
  (`components.flux.enabled: true` — the lab has no Flux of its own, and the
  chart refuses a second one) and self-management OFF (`gitops.self.enabled:
  false` — the lab installs unreleased charts and dev images, which the
  chart's own HelmRelease would replace with the published release; the
  lab's embedded Helm stays the one writer of the release, and the Helm CLI
  its day-2 tool). The chart is pinned to an exact
  release (`platform.chartVersion`, default `config.DefaultChartVersion`);
  `platform.chartPath` installs a local checkout instead; `platform.chartBranch`
  is the dev channel — the branch's newest dev build, resolved into
  `chartVersion` on every `configure`/`up`/`platform` (`chartbranch.go`,
  `platform --pin` freezes it). Agent Substrate and the platform Postgres
  come with the chart (the 4.x line: the `substrate` and `cloudnative-pg`
  component releases) — the lab installs neither; it reads the chart's
  rendered roster (`platformRoster`) to know what to budget for, check the
  apiserver gates for and preload. Never emit `gitops.namespace` with the
  engine on.
- **The fleet's admission is in the lab**: `agentlab up` installs Kyverno
  (the fleet's kyverno-crds and kyverno app charts from gsoci, Kyverno
  3.7.2, admission controller only)
  with the fleet's `flux-multi-tenancy` ClusterPolicy in Enforce, verbatim
  from management-cluster-bases (`internal/lab/templates/flux-multi-tenancy.yaml`),
  the platform namespace exempt in code next to the fleet's three, and the
  org namespace `org-lab` with the tenant ServiceAccount `automation`
  (`admission.go`). A HelmRelease in an org namespace without
  `serviceAccountName`/`kubeConfig.secretRef` is denied as on an
  installation; `platform-test` proves the matrix. Never "fix" a denial by
  exempting a namespace or turning the policy to Audit — the denial is the
  finding. The chart's own Kyverno objects stay pinned off
  (`kyvernoPolicies.enabled: false`); its PolicyExceptions are judged
  anyway: `polex-test` (and the `platform-test` stage of that name)
  renders them with `kyvernoPolicies.enabled: true` and the tenants on and
  dry-runs each against the lab's Kyverno, PolicyExceptions on as on the
  fleet (`polextest.go`).
- The lab's credentials are throwaway by design; plaintext passwords in
  `agentlab.yaml` are fine.

`HACKS.md` is the audit journal of every hack/workaround and its status —
record new ones there, one commit per resolved item.
