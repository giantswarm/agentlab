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
- muster's `kubernetes` tools are a *family*: pass the MCPServer CR name,
  e.g. `management_cluster: "agentlab-mcp-kubernetes"`.
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
- For verifying RBAC as a specific user, use `./agentlab login <email>` and
  `kubectl --kubeconfig kubeconfig.oidc` — that is the OIDC path.
- The kind admin context (`kind-agentlab`) bypasses the platform and OIDC
  entirely; use it only to debug the lab's own plumbing, never to demonstrate
  platform behavior.

## Commands

```bash
make build                 # go build -o agentlab .
make test                  # go test ./...
go test ./internal/forms/ -run TestMinimalFormDrive -count=1 -v   # single test

./agentlab configure       # interactive form; --defaults keeps/writes the canonical lab
./agentlab up              # certs, kind cluster, Dex, RBAC, the agent platform — verified
./agentlab platform-test   # headless Dex -> muster -> mcp-kubernetes proof
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
