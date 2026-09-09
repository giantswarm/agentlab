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
- a user identifier hash: SHA-256 over OS, architecture, host name, OS user
  and group IDs, user name and the MAC addresses — enough to count distinct
  users, not to identify one (see the
  [library source](https://github.com/giantswarm/telemetrydeck-go/blob/main/telemetrydeck.go))
- a random session UUID, unique per command execution

Nothing from `agentlab.yaml`, `state/`, `certs/`, the cluster, the users, the
models or the model servers is ever sent. Help output (`-h`, `--help`) and
shell completion do not count. The signal goes out in the background while the command runs
and is dropped when the network is unavailable — it never blocks or fails a
command.

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
