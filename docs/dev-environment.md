# agentlab as a development environment

The lab is the whole agent platform on one machine — Dex, muster, the MCP
servers, kagent, Backstage — so it is also the place to develop a platform
component against everything else it talks to, before anything is released or
pushed. This page is for that: working on Backstage, muster, kagent or the
chart with the lab as the environment around your change. (Developing
agentlab itself is [Development](development.md).)

There are three loops, from fastest to most faithful:

| Loop | What runs your code | Turnaround | Use it for |
|---|---|---|---|
| [Hot reload](#hot-reload-agentlab-portal-dev) (`agentlab portal dev`) | Your Backstage checkout on this machine, wired to the lab | Seconds | Day-to-day work on the portal, frontend and backend |
| [Dev images](#dev-images-agentlab-portal-build-and-platformdevimages) (`agentlab portal build`, `platform.devImages`) | An image you built, inside the cluster, in place of the release | Minutes | The check before a pull request: the artifact that ships, in the shape it ships in; any component other than the portal |
| [Unreleased charts](#unreleased-charts) (`platform.chartPath`, `platform.chartBranch`) | A chart checkout or a branch's dev builds | Minutes | Changes to the agent-platform chart itself |

They combine: a lab can run a branch's chart with a dev image of muster while
you work on the portal with hot reload.

## Hot reload: `agentlab portal dev`

Runs your [giantswarm/backstage](https://github.com/giantswarm/backstage)
checkout with `yarn start` on this machine, against the lab:

```bash
agentlab portal dev                          # the checkout at ../backstage, next to the directory you run this in
agentlab portal dev --checkout ~/src/backstage
```

Open `http://localhost:3000` and sign in as a lab user (`admin@lab.local`,
password `password`). The portal signs in through the lab Dex and calls the
lab's muster, kagent API and apiserver with the signed-in person's own token —
the same platform, the same RBAC, the same tools the in-cluster portal sees.

**What reloads how.**

- A **frontend** change (the app, a plugin's frontend) is swapped into the
  open page without a reload.
- A **backend** change (the backend, a backend plugin, a package it imports)
  restarts the backend process: `Change detected, restarting the development
  server…`, serving again on its port within about ten seconds. The frontend
  keeps running, and you stay signed in — the Backstage token and the Dex
  session survive the restart.
- An edit to the checkout's `app-config.yaml` restarts the backend too.

**What it is wired to.** The app-config is what the in-cluster portal runs on,
in the same order: the checkout's `app-config.yaml`, then the chart's own
app-config read from the cluster at start (so the run follows the installed
chart version), then a generated overlay that swaps every in-cluster address
for one this machine reaches — the frontend on `http://localhost:3000`, the
backend on `backstage.localDevPort` (both bound to localhost, nothing on the
network), the apiserver on its published port with
the cluster CA, muster and the kagent API through the edge. The OIDC client
and session secrets come from the lab's `agent-platform-secrets`, and the lab
CA through `NODE_EXTRA_CA_CERTS`. All of it is written to
`state/backstage-dev/`; the checkout is never written to.

The config read from the cluster is treated as data: a `$file`, `$include` or
`$env` include directive anywhere in it is refused rather than evaluated on
this machine (in the pod those resolve inside the pod; here they would read
your files and environment), and the dev process runs on a minimal
environment — the lab's secrets, the CA and what node and yarn need, not your
shell's. `${VAR}` string interpolation keeps working.

**The Dex callback.** Dex only redirects to callbacks registered on its
`agent-platform` client, so the local backend's
(`http://localhost:<localDevPort>/api/auth/oidc-agent-platform/handler/frame`)
is registered while `backstage.localDevPort` is set in `agentlab.yaml`. The
first `portal dev` records it — 7017, or `--port` — and re-applies Dex; later
runs reuse it. It cannot be `backstage.port` (7007), which kind publishes for
the in-cluster Backstage. Removing the key and running `agentlab reload`
unregisters it.

**Limits.**

- The database is in memory, so every backend restart starts it empty: the
  catalog re-ingests the lab users within seconds, but whatever a backend
  plugin stored during the session is gone.
- A new dependency (a `package.json` change) needs `yarn install` in the
  checkout and a fresh `portal dev`.
- The lab-derived config is generated when `portal dev` starts. After changing
  the lab — another chart version, `agentlab platform` with new values — start
  it again.
- It runs your code, not your build: the production bundle, the image, TLS on
  the edge and the in-cluster addresses are the next loop's job.

## Dev images: `agentlab portal build` and `platform.devImages`

A dev image replaces a component's released image inside the cluster, so your
code runs exactly where and how the release does.

For the portal, one command builds and rolls it in:

```bash
agentlab portal build              # build, record, roll in
agentlab portal build --no-apply   # build and record only; `agentlab platform` rolls it in
```

It runs the checkout's own build — `yarn install --immutable`, `yarn tsc`,
`yarn ci:build` (what its CI runs) and `docker build -f
packages/backend/Dockerfile` — tags the image
`backstage-dev:<commit>[-dirty]-<UTC time>` so no two builds share a tag,
records it as `platform.devImages.backstage` and runs the platform step, which
side-loads it into the node and rolls the pod. Under Podman the image build
runs with `--security-opt label=disable`: SELinux labelling would otherwise
hide the Dockerfile's bind-mounted bundles from the build's non-root user.

For every other component — muster, the kagent controller, mcp-kubernetes,
model-manager, agent-manager, vm-manager, klaus-gateway, the Harness — build
the image from its repository yourself and name it under `platform.devImages`,
then run `agentlab platform`:

```yaml
platform:
  devImages:
    muster: muster:dev-1a2b3c
```

Use a new tag per build (kind's containerd keeps running the old bits under a
reused one). Removing an entry and running `agentlab platform` puts the
released image back. The targets, how each is swapped and the Harness's
registry path are in [The agent platform: Dev images](platform.md#dev-images).

## Unreleased charts

For a change to the agent-platform chart itself:

- `platform.chartPath` installs a local checkout's chart directory —
  [Installing an unreleased chart](platform.md#installing-an-unreleased-chart).
- `platform.chartBranch` follows a branch's published dev builds — the
  [Dev channel](platform.md#dev-channel).

## Proving a change

The lab's `*-test` commands are the end-to-end checks; run the ones your change
touches against the dev image or chart before opening a pull request —
`agentlab backstage-test` for the portal, `agentlab platform-test` for the
request path, the rest per component ([Command reference](cli.md#testing)).
The Backstage repository's browser suite runs against the lab too
(`yarn test:e2e:agentlab`, its `docs/development.md`).
