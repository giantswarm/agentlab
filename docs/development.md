# Development

## Building and testing

```bash
make build                 # go build -o agentlab .
make test                  # go test ./...
go test ./internal/forms/ -run TestMinimalFormDrive -count=1 -v   # one test
```

`go test` covers the pieces that run without a cluster: the config schema and
its discovery and port logic, the form (driven with scripted keystrokes), the
renderer, the `postRenderers` patches, the docker resource floors, the
fixtures' arithmetic and the proofs' parsers. The lab's end-to-end checks are
its own `*-test` subcommands — see [Proofs](cli.md#proofs) — run against a
live lab.

A `go build` from a checkout reports Go's pseudo-version (`agentlab
--version`), never checks for releases and cannot self-update; releases are
stamped by the devctl Makefile and the architect CI through `-ldflags -X`.
While iterating on the lab, `AGENTLAB_TELEMETRY_TESTMODE=1` keeps your runs
out of the production usage numbers (see [Usage data](telemetry.md)).

The rendered manifests are always in `state/` (`agentlab render` writes them
without applying), so `kubectl diff -f state/dex.yaml` and plain reading stay
possible. The lab's own `kubectl` and `helm` never read your shell's
kubeconfig: every cluster-facing command exports the kind cluster's kubeconfig
to `state/kubeconfig` and pins `KUBECONFIG` to it, so
`KUBECONFIG=state/kubeconfig kubectl ...` is the same view from a shell.

## The hack journal

[HACKS.md](../HACKS.md) is the audit journal of every hack and workaround in
the lab and its status — fixed, blocked upstream (with the exact upstream
change that unblocks it), or accepted with the reasoning. Record new ones
there; one commit per resolved item. Code that works around something cites
its entry (`HACKS.md U9`).

[CLAUDE.md](../CLAUDE.md) briefs coding agents on the repository and its
invariants; the constants it names (the group vocabulary, the static OAuth
client IDs) appear both in `internal/config` and in the embedded templates and
must stay in agreement.

## Layout

```
main.go                          the CLI (cobra): one subcommand per lifecycle step, no logic
internal/config/                 agentlab.yaml schema, defaults, validation; the fixed group vocabulary and the static OAuth clients
internal/forms/                  the interactive configuration form (huh); tests drive it with scripted keystrokes
internal/telemetry/              the one anonymous usage signal per command (TelemetryDeck)
internal/update/                 agentlab self-update + the newer-release hint before every command (go-selfupdate)
pkg/project/                     version, commit, build time: ldflags from make/CI, else Go's VCS build info
internal/lab/                    everything operational:
  up.go down.go                    lifecycle; checksum-stamped Dex apply
  discover.go                      what `configure` learns about this machine on every run
  node.go runtime.go               the kind node as a docker container; docker vs Podman
  preload.go                       images pulled on the host and side-loaded into the node, never by the kubelet
  certs.go trust.go                the name-constrained lab CA + 825-day leaf certs; trust/untrust (smallstep/truststore)
  oidc.go login.go browser.go      the lab's Dex clients: password grant, authorization-code flow
  test.go                          RBAC assertions for every configured user
  platform.go platformtest.go      agent platform install + the headless MCP proof
  postrenderers.go                 the lab's per-component postRenderers patches (hostNetwork, sidecar, nodePort, dev images)
  fluxreleases.go helm.go          image preload from the chart's rendered OCIRepositories/HelmReleases; the Helm >= 4 check
  resources.go                     docker CPU/memory: the requests table and the floors `up` enforces
  oauthfixture.go                  the Auth Required MCPServer fixture + the per-server sign-in proof
  fleetfixture.go servergroups.go  the fake-fleet MCPServers (families x fake clusters, tool-group label); the portal's grouping arithmetic
  toolsetstest*.go                 the toolset proof: muster, kagent, the portal
  agentmanager.go agentstest.go    the agent-manager MCPServer + the agents proof
  models.go hostmodels.go          ModelConfigs from extraModels (+ pruning); the host model servers' inventories
  modelmanager.go modelstest.go    managed models: host preflight, install, the models proof
  anthropic.go                     the API key: host environment -> Secret, never config or state/
  adk.go kagentcrd.go              kagent workarounds (HACKS.md U8, U11)
  observability.go                 the lab Prometheus (helm) and the mcp-prometheus HelmRelease through the platform's engine
  backstage.go backstagetest.go    Backstage deploy + headless sign-in proof
  portal.go mcpsession.go          one user's signed-in portal session; the proofs' MCP session against muster
  exec.go kubeconfig.go            running kubectl/helm with KUBECONFIG pinned to state/kubeconfig; the OIDC kubeconfig `login` writes
  render.go logs.go                template rendering into state/; `agentlab logs`
  templates/                       every manifest, rendered from agentlab.yaml (static/: the verbatim agent-deployment Template)
docs/                            this documentation; README.md is the front door
HACKS.md                         the hack journal
agentlab.yaml                    your configuration (gitignored; `agentlab configure`)
certs/                           the lab CA and leaf certs (gitignored; key 0600)
state/                           rendered manifests, for inspection (gitignored)
  kubeconfig                       the kind cluster's kubeconfig, exported per run — what the lab's own kubectl/helm use
  agent-platform-values.yaml       the chart's values in the lab shape, incl. the postRenderers
.mcp.json                        registers muster as an MCP server for Claude Code
```

Useful passthroughs: `agentlab logs dex|muster|backstage|prometheus|mcp-prometheus`
tails logs; the effective Dex config is
`kubectl -n dex get secret dex-config -o jsonpath='{.data.config\.yaml}' | base64 -d`,
muster's is `kubectl -n agent-platform get cm muster-config -o jsonpath='{.data.config\.yaml}'`
(both with `KUBECONFIG=state/kubeconfig`).
