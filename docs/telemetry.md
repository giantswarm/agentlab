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

A random identifier saved under your cache directory would be steadier still,
and would cover containers. The lab does not do that on purpose: reporting
usage should not write state to your disk as a side effect, and it would fail
on a read-only home directory.

Nothing from `agentlab.yaml`, `state/`, `certs/`, the cluster, the users, the
models or the model servers is ever sent. Help output (`-h`, `--help`) and
shell completion do not count. The signal goes out in the background while
the command runs; a command that finishes before the signal has left the
machine (`agentlab version`, say) waits for it at most half a second, then
exits regardless. The signal is dropped when the network is unavailable — it
never fails a command.

Data is stored at [TelemetryDeck](https://telemetrydeck.com/) on servers in
the EU; see their [privacy FAQ](https://telemetrydeck.com/docs/guides/privacy-faq/).

**Opting out:** set `AGENTLAB_TELEMETRY_OPTOUT` to any value, or the
cross-tool [`DO_NOT_TRACK=1`](https://consoledonottrack.com/):

```bash
export AGENTLAB_TELEMETRY_OPTOUT=1
```

Working on the lab itself? `AGENTLAB_TELEMETRY_TESTMODE=1` files your signals
as test data (kept apart from the production numbers in the dashboard) and
logs delivery errors to stderr.
