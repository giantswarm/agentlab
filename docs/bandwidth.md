# Bandwidth: the labs' downloads yield to yours

Labs run on laptops, and a laptop's uplink is whatever you are on: fiber at
the office, a phone hotspot, a train's Wi-Fi. Every component release inside
a chart's range makes each lab's nodes pull its images, and on a busy release
day that happens many times, unattended, whenever Flux reconciles. On a weak
link those pulls open dozens of TCP flows against your handful and fill the
modem's buffer: your own traffic crawls, a video call breaks up, `git push`
stalls. On a fast link the opposite is wanted: the faster the pulls, the
sooner the agents can test. A fixed limit is wrong on every link but one.

The bandwidth shaper caps what the [registry mirror](registries.md#the-registry-mirror)
downloads by the uplink's latency: the labs take what you leave idle and give
it back within seconds.

- [What it shapes](#what-it-shapes)
- [Turning it on](#turning-it-on)
- [How it decides](#how-it-decides)
- [This machine's override](#this-machines-override)
- [Status](#status)
- [What it does not shape](#what-it-does-not-shape)
- [Platforms](#platforms)
- [What could go wrong](#what-could-go-wrong)
- [The namespace proof](#the-namespace-proof)

## What it shapes

Every lab's nodes pull gsoci through one zot on the machine,
`agentlab-mirror`. Everything the mirror downloads reaches it through one
interface on the host: the host side of its veth on the `kind` network.
Packets towards the mirror leave the host there; the mirror's answers to the
nodes go the other way. So a CAKE qdisc as the root of that one veth caps the
labs' node lane and nothing else: no marks, no filters on your own
interfaces, and the qdisc goes away with the mirror's container.

The shaper is a container of its own, `agentlab-shaper`, one per machine
beside the mirror, shared by every lab like it. It runs this agentlab
(`agentlab shaper run`) in the lab's probe image, with:

- the host's network namespace, where the veth is;
- the mirror's PID namespace: the mirror's own process, PID 1 there, names
  the mirror's network namespace, and the shaper asks the kernel which veth
  leads there. No docker socket, and a recreated mirror's new veth is found
  the same way;
- `NET_ADMIN` for the qdisc and `NET_RAW` for its latency probes (Podman's
  default capabilities lack it). The lab's nodes run privileged, so this asks
  for nothing new;
- a read-only root and `~/.local/state/agentlab/shaper/` mounted, where it
  publishes its state (`state.json`) and reads this machine's override
  (`mode`).

It talks to the kernel over rtnetlink itself, so the image needs no `tc`.
CAKE runs as `besteffort dual-dsthost nat ingress`; the shaper changes only
its bandwidth from then on, and takes CAKE off again when it stops.

## Turning it on

`bandwidthShaper` in `agentlab.yaml` is off by default while it proves
itself on a few machines:

```bash
agentlab configure --bandwidth-shaper
agentlab platform        # or up
```

With it on, every `up` and `platform` ensures the shaper right after the
mirror, the way it ensures the mirror: created if missing, started if
stopped, recreated when this agentlab or the mirror changed. A second lab on
the machine reuses it. The run says what it does:

```text
==> [00:03] Ensuring the bandwidth shaper agentlab-shaper
    created agentlab-shaper (this agentlab in gsoci.azurecr.io/giantswarm/alpine:3.22.1, on the host network, joined to agentlab-mirror)
    agentlab-shaper shapes agentlab-mirror's downloads on vethfa4e51a: the mirror capped at 8.0 Mbit/s (auto), getting nothing, the link +0 ms over its baseline: the mirror is below its cap
```

`--bandwidth-shaper=false` stops a lab from ensuring it; a shaper in place
stays for the other labs. `agentlab shaper remove` removes it, and
`agentlab down --mirror` removes it with the mirror. The shaper shapes the
mirror only, so a lab with `registryMirror: false` has nothing for it to
shape.

## How it decides

Every 500 ms the shaper pings four public resolvers (1.1.1.1, 9.9.9.9,
8.8.8.8, 208.67.222.222), each against its own 5-minute minimum. The link's
queue is the lowest of the four, read over the last one and two seconds: a
full link raises every probe, Wi-Fi contention and a busy resolver raise
only some.

| The queue over the baseline | The mirror's cap |
|---|---|
| under +15 ms on the newest tick and over the last second, while CAKE holds the mirror back (drops or marks its packets, or delivers 90 % of the cap) | grows 15 % per tick, 2 % near where the link filled up last |
| one tick above +15 ms | holds if CAKE was dropping (noise, someone's burst); settles at once if the link was what held it back |
| above +15 ms over the last second | settles at 97 % of what the mirror got lately |
| above +30 ms over two seconds | is cut to 75 % of what the mirror got |
| above +90 ms over the last second, or every reflector that answered before lost its probe | is halved at once |

The floor is 256 kbit/s, the ceiling 2.5 Gbit/s, and a new network (default
route, gateway, gateway MAC) starts over at 8 Mbit/s. A cut of an idle
mirror only shrinks its unused cap, so its next pull does not start from the
floor.

**When there is no signal**: on a network that drops ICMP, no reflector ever
answers, and the latency says nothing. The shaper then leaves the mirror
unlimited, as without it, and says so in `status`. Once a reflector answers,
the controller starts from 8 Mbit/s.

The controller is a pure function of the probes and CAKE's counters
(`internal/shaper/controller.go`). Its tests run it against a fluid model of
3, 20 and 300 Mbit/s links with a 500 ms FIFO and against a recorded trace of
a contended Wi-Fi: the mirror fills an idle link, leaves a call's 75 % alone,
is at its floor 2–9 s after your own big download starts and has most of the
link back 15–30 s after it ends (55 s on the noisy Wi-Fi). The trace is the
regression test of the lowest-over-a-window signal: a median read that
Wi-Fi's noise as a full link and held the downloads at their floor.

## This machine's override

One shaper serves every lab, so the override is the machine's, not a lab's:

| Command | Effect |
|---|---|
| `agentlab shaper auto` | follow the link (the default) |
| `agentlab shaper max 5mbit` | follow the link, never above 5 Mbit/s (a metered hotspot) |
| `agentlab shaper pause` | 128 kbit/s: the pulls crawl but don't time out |
| `agentlab shaper off` | the mirror is not limited; the shaper keeps running |
| `agentlab shaper remove` | stop the shaper (its cap taken off) and remove its container |

Each writes `~/.local/state/agentlab/shaper/mode`, waits for the running
shaper to take it and prints what it does then. A mode set while no shaper
runs is taken when one starts.

## Status

`agentlab status` has a `shaper` line while the lab has it on or a shaper is
in place:

```text
    mirror      agentlab-mirror running (zot v2.1.23, 1.83GB cached)
    shaper      agentlab-shaper running: the mirror capped at 42.0 Mbit/s (auto), getting 39.8 Mbit/s, the link +6 ms over its baseline: link has room, the mirror may take more
```

The line names the cap, what the mirror got over the last second, the
link's queue over its baseline and why the cap is where it is; or why there
is no shaper (`skipped: …`), what brings a missing or stopped one back, and
when its state is older than ten seconds. In JSON it is `shaper` (`on`,
`state`, `skipped`, `shaper` with what it published, `age`).

`agentlab shaper` (or `agentlab shaper status`, `-o json`) shows the whole
state:

```text
shaper    auto · the mirror capped at 42.0 Mbit/s · getting 39.8 Mbit/s · CAKE on vethfa4e51a
latency   +6 ms over baseline (backs off above +30 ms) · link has room, the mirror may take more
network   wlan0 via 192.168.178.1
probes    1.1.1.1 18/12 ms · 9.9.9.9 21/15 ms · 8.8.8.8 19/13 ms · 208.67.222.222 25/19 ms (last/baseline)
```

`docker logs agentlab-shaper` logs the veth it shapes, network changes, mode
changes, problems and at most one back-off line per 30 s. To see CAKE
itself: `tc -s qdisc show dev <veth>` with the veth `status` names.

## What it does not shape

- **The host's preload.** A first boot's preload (1.83 GiB, once per machine
  since the docker cache is shared) is pulled on the host, unshaped. After
  that the host pulls only tags some node of the lab lacks: a component
  release the nodes pulled through the mirror is not pulled on the host again
  ([The lab's registries](registries.md#the-registry-mirror)).
- **Your own `docker pull`**, which does not go through the mirror, and your
  own downloads: they still bloat a weak link. The shaper gets the labs out
  of the way; it does not shape your traffic.
- **Uploads** (the labs' are small), and pulls from registries other than
  gsoci: the kind node image, Docker Hub, ghcr, quay, atelet's Harness images
  and Flux's chart pulls.
- **A mirror that fails**: the nodes fall back to gsoci directly, unshaped.

## Platforms

- **Docker on Linux**: as above.
- **Docker Desktop, colima, OrbStack with a Linux agentlab**: the shaper and
  the mirror both live in the engine's VM, so the design carries over: the
  veth is in the VM and the probes leave through the VM's NAT. Whether the
  VM's kernel ships `sch_cake` and how its network stack answers ICMP is
  untested. Without CAKE the shaper says so in `status` and leaves the
  mirror as it is.
- **An agentlab built for macOS**: skipped with one line. The shaper runs
  agentlab in a Linux container, and a macOS binary cannot.
- **Rootless Podman and rootless Docker**: skipped with one line. The engine
  has no `NET_ADMIN` in the host's network namespace.
- **A dynamically linked agentlab** (a `go build` with cgo): skipped with one
  line naming `make build`, which builds the static binary the probe image
  runs.

## What could go wrong

- **A stale low cap**: a shaper that dies while the cap is low leaves the
  mirror crawling. The shaper takes CAKE off on SIGTERM (`docker stop`, which
  `shaper remove` and every recreate use), and a shaper that starts takes
  over the veth from whatever it finds there, so a killed one's cap is reset
  by the next `up` or `platform`, which starts it again. A shaper removed
  while it was not running gets a one-shot `agentlab shaper cleanup` first.
- **The mirror recreated**: the shaper shares the mirror's PID namespace, so
  it stops with the mirror; the veth and its CAKE go with the mirror's
  container. The `up` or `platform` that recreated the mirror recreates the
  shaper next.
- **Two shapers**: a host that also runs a host-level shaper for docker's
  traffic caps the labs twice. Both yield on the same signal, so nothing
  breaks, but one of them is enough.
- **Shaping on a veth instead of the uplink**: what you see is the uplink's
  queue. Measured: a cap on the veth keeps that queue down as well as a cap
  on an IFB behind the uplinks does.
- **The latency tail**: the controller's upward probes show as short queues.
  The median latency halves; the p90 stays near the unshaped level.

## The namespace proof

`agentlab shaper-test` runs the shaper against a bloated link without a lab,
without root and without touching the host: an unprivileged user and
network namespace of its own (iproute2's `ip` and `tc`, the kernel's
`sch_netem` and `sch_cake`).

```text
internet ─ wan1 ══ 20 Mbit/s, 20 ms, ~420 ms FIFO ══ wan0 ─ host ─ br-lab ─ veth-mirror ═ eth0 ─ mirror
```

A fake internet sits behind 20 Mbit/s with 20 ms of delay and a netem FIFO of
700 packets (~420 ms), standing in for a bloated DSL modem. A "mirror"
namespace behind a bridge pulls four streams for 98 s; you, in the proof's
own namespace, download one stream from second 30 for 25 s and ping the far
side every 0.5 s. The real shaper shapes the mirror's veth, found the way it
finds `agentlab-mirror`'s. agentlab generates the traffic itself, no iperf3.

It passes when your download gets at least half of the link while the mirror
pulls, and your median latency while the mirror pulls alone (leaving out the
cap's first 5 s and the 10 s after your download) stays within +20 ms of the
idle round trip. `--baseline` runs the link without the shaper first:

| Run | Your download while the mirror pulls (of 20 Mbit/s) | Your latency while the mirror pulls alone, median / p90 (base 20 ms) | The mirror alone | The mirror while you download |
|---|---|---|---|---|
| Without the shaper | 5.0 | 72 / 76 ms | 18.9 | 14.1 |
| With the shaper | 16.4 | 33 / 72 ms | 16.6 | 1.9 |

Throughput in Mbit/s, one run each on a laptop; an earlier shaped run had a
median of 25 ms, the mirror at 16.0 alone and 2.7 beside you. The median
latency drops by more than half; the p90 does not, since each upward probe that fills the link
shows as a short queue. The shaper's log lines follow the table, so a run
shows when it backed off.
