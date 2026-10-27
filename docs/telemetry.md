# Usage data (telemetry)

Since v0.17.0, agentlab reports **one anonymous usage signal per command you
run** — the same integration [kubectl-gs has](https://docs.giantswarm.io/reference/kubectl-gs/telemetry/)
— so Giant Swarm can see which parts of the lab get used, on which versions
and platforms, and by roughly how many people. That shapes what gets built
next.

One signal contains:

- the command (`agentlab up`, `agentlab platform-test`, …) — never its
  arguments or flags
- the agentlab version (what `agentlab --version` prints) and the commit it
  was built from
- operating system and processor architecture
- the library and version that sent it (`telemetrydeck-go/…`)
- a user identifier hash: SHA-256 over the identifier your operating system
  keeps for this computer (macOS `kern.uuid`, Linux `/etc/machine-id`,
  Windows `MachineGuid`), your OS user name, and a fixed agentlab salt —
  enough to count distinct people on distinct machines, not to identify one,
  and not reversible (the derivation is
  [`internal/telemetry`](https://github.com/giantswarm/agentlab/tree/main/internal/telemetry))
- a random session UUID, unique per command execution

**Since v0.48.0 that identifier comes from the computer, not from the
network.** It used to be derived partly from every MAC address the machine
had, so a laptop that docks, joins a VPN or runs Docker drifted into several
"users" — and the host name and the processor architecture were part of it
too, so renaming the machine, moving between networks, or running the Intel
build under Rosetta each counted as somebody new. The identifier therefore
changed once, with that release: installations that reported before it appear
as new users from then on. What is *sent* did not change — still a one-way
hash, still nothing that says who you are. A machine that exposes no such
identifier (a container without `/etc/machine-id`, say) falls back to the one
the library derives for itself, which means the fallback cannot be counted:
in the dashboard it looks exactly like a pre-v0.48.0 user.

**How long it lasts differs by platform, and it is worth knowing which you
are on.** macOS `kern.uuid` is derived from the hardware, so it is the same
before and after a full reinstall and lasts for the life of the machine.
Linux `/etc/machine-id` and Windows `MachineGuid` are written when the system
is installed, so reinstalling gives you a new identifier and a new row.

In a container it depends on the base image, and neither case reports
honestly. `golang`, `debian` and `alpine` images ship no `/etc/machine-id`
and `ubuntu` ships an empty one, so agentlab takes the fallback. Images that
ship a non-empty one bake a single value into the image layer rather than
writing a fresh one per container — `cimg/go:1.24` gives
`b5c01751b9ee4ef182c026e943d33d30` to every container built from it — so
everyone running that image counts as one user rather than many.

A random identifier saved under your cache directory would be steadier than
any of this, and would settle the container case. The lab does not do that on
purpose: reporting usage should not write state to your disk as a side
effect, and it would fail on a read-only home directory.

Nothing from `state/`, `certs/`, the cluster, the users, the models or the
model servers is ever sent, and from `agentlab.yaml` only what the platform
signal below lists; the one exception is the random deployment ID below, which
exists only to be sent. Help output (`-h`, `--help`) and shell completion do
not count. A command that runs against a lab sends its signal as it enters the
lab, so that the signal can name the lab's deployment, and the signals go out
in the background while the command runs; a command that runs against no lab
sends its signal as it finishes. A command that finishes before its signals
have left the machine (`agentlab version`, say) waits for them at most half a
second, then exits regardless. When the
endpoint is out of reach (no connection within 300ms of the signal setting
out: offline, or egress blocked), the command does not wait for it at all
past that point. A signal is dropped when the network is unavailable — it
never fails a command and never prints anything on its output.

## The platform signal

Since v0.51.0, a lab that installs the platform also reports **which meta
chart it installs**: `agentlab up` and `agentlab platform` send one
`GiantSwarm.agentlab.platform` signal per run, once the chart is resolved and
ahead of the install, so a run that fails to install still counts. It
carries:

- `chartVersion` — the exact agent-platform chart version the run installs;
  a chart directory reports the version its `Chart.yaml` carries, never
  where it is
- `chartMajor` — its major, `3` or `4`
- `chartChannel` — `stable` for a pinned release, `dev` for a dev-channel
  build (`platform.chartBranch`), `path` for a chart directory
  (`platform.chartPath`)
- `chartPinned` — whether a dev-channel lab is frozen at its recorded build
- `legacyShape` — whether the lab renders the 3.x shape of the values
- the feature switches of `agentlab.yaml` as booleans: `agents`,
  `observability`, `modelManager`, `vmManager`, `klausGateway`,
  `backstage` — the effective ones (model-manager and klaus-gateway come with
  the agents, so they read `false` while the agents are off)

plus the version, platform and user-identifier parameters every signal
carries. Versions and booleans only: no path, host name, user or token.
`render`, the proofs and every other command send nothing beyond the command
signal. The signal exists so that the remaining use of the 3.x chart shape can
be measured before the lab drops it
([#211](https://github.com/giantswarm/agentlab/issues/211)); the opt-outs and
the test mode below apply to it exactly as to the command signal.

In the TelemetryDeck dashboard, labs by chart line and channel is this
Playground query (Explore → Playground → JSON Editor; the global time range
and the Test Mode toggle apply):

```json
{
  "queryType": "groupBy",
  "granularity": "all",
  "baseFilters": "noFilter",
  "filter": {
    "type": "and",
    "fields": [
      {"type": "selector", "dimension": "appID", "value": "89699F74-9A72-46BF-BF5A-7949901FBB36"},
      {"type": "selector", "dimension": "type", "value": "GiantSwarm.agentlab.platform"}
    ]
  },
  "dimensions": [
    {"type": "default", "dimension": "chartMajor", "outputName": "major"},
    {"type": "default", "dimension": "chartChannel", "outputName": "channel"}
  ],
  "aggregations": [{"type": "count", "name": "count"}]
}
```

Swap `chartMajor` for `chartVersion` to see the exact versions, or add a
`legacyShape` dimension to count the labs still rendering the 3.x shape.

## The deployment and the lab's portal

A lab's Backstage reports how it is used into agentlab's TelemetryDeck app
too, so that what happens in the portal can be read next to the commands that
built the lab. The first `agentlab up` after a `down` (or ever) draws a
**deployment**: a random ID and a random salt, recorded in
`state/telemetry-deployment.json` and removed again by `agentlab down` — with
the lab's other state, which `up` writes anyway, and never with usage data off.
Every
signal of a command that runs against the lab, the platform signal included,
carries the ID as `deploymentID`, and so does every signal of the lab's
portal.

What the portal sends is what every Giant Swarm portal sends — page views by
route template and the listed actions, never a name, prompt or free text; the
portal's [telemetry page](https://github.com/giantswarm/backstage/blob/main/docs/telemetry.md)
lists them. Its user identifier is a hash of the signed-in Backstage user and
the deployment's salt: every lab has the same Dex users, so without the salt
`admin@lab.local` would be one person across all labs. The salt stays in the
lab; sent next to the hashes, it would let a guessed GitHub login (with
`platform.githubSignIn`) be checked against them. A new deployment means a new
salt, so the same person counts as a new portal user after every `down`.

The lab writes the portal's `app.telemetrydeck` into its app-config overlay
on every `up` and `platform`. With usage data off (below), or before
the first `up`, it writes an empty app ID, and the portal sends nothing (its
browser console and its backend log say usage data is disabled); the decision
is taken when the overlay is rendered, so an opt-out set later takes effect
for the portal on the next `up`. Test mode applies to the portal's signals as
well. A Backstage release from before `deploymentID` and `testMode` existed
(giantswarm/backstage#3294) sends its signals without the ID and outside test
mode.

Data is stored at [TelemetryDeck](https://telemetrydeck.com/) on servers in
the EU; see their [privacy FAQ](https://telemetrydeck.com/docs/guides/privacy-faq/).

**Opting out** (of every signal, the lab portal's included): set `AGENTLAB_TELEMETRY_OPTOUT` to any
value, or the cross-tool [`DO_NOT_TRACK=1`](https://consoledonottrack.com/):

```bash
export AGENTLAB_TELEMETRY_OPTOUT=1
```

Working on the lab itself? `AGENTLAB_TELEMETRY_TESTMODE=1` files your signals
as test data (kept apart from the production numbers in the dashboard).
`AGENTLAB_TELEMETRY_DEBUG=1` logs what happened to them on stderr — a failed
or rejected delivery, an endpoint out of reach; without it telemetry prints
nothing, test mode included.
