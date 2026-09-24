# vm-manager

The platform's VM provisioner ([giantswarm/vm-manager](https://github.com/giantswarm/vm-manager))
in the lab: KVM virtual machines with an instance metadata service, a vTPM
with measured boot, an immutable image and attestation, exposed to muster and
the agents as `x_vm-manager_<tool>` and to the portal as one more server of
the Agent Platform group. vm-manager is the sibling of agent-manager (agents)
and model-manager (models) — the write surface for VMs — and runs the way they
do: as a **pod**, the agent-platform chart's `components.vm-manager`. The kind
node is a privileged docker container, so the host's `/dev/kvm` and
`/dev/vhost-vsock` are in it, and the runtime hands them to the privileged pod
(nothing is mounted from the node; the chart has no hostPath); QEMU and swtpm
are the pod's children. The guest image the pod boots is an OCI artifact its
init container fetches into the pod's state volume: the one every vm-manager
release publishes (`gsoci.azurecr.io/giantswarm/vm-manager-guest-image:<version>`,
the chart default), or a local build the lab pushes into its registry.

## What the lab does

`platform.vmManager` in `agentlab.yaml`:

```yaml
platform:
  vmManager:
    enabled: true
    imageDir: /home/you/projects/giantswarm/vm-manager/images/build   # optional: a local guest image build (`make -C images`); empty boots the release's
  devImages:
    vm-manager: vm-manager:dev                                        # optional: a build of the checkout's binary (`make docker-build`)
```

- **`agentlab configure`** reports whether this machine has the KVM devices
  (`KVM  /dev/kvm and /dev/vhost-vsock present — platform.vmManager on, the
  release's guest image` or `… a local guest image build from …`). A machine
  without them turns the key off with the reason; a machine with them keeps
  what the file says — the pod is a heavier piece of the lab than a model
  server, so it is never turned on by itself. `--vm-manager[=false]` pins it,
  `--vm-manager-image-dir <dir>` names a local build.
- **`agentlab up`** / **`agentlab platform`** refuse the key on a node without
  the devices, with the fix. With `imageDir` set, `platform` pushes the build
  into the lab registry (`<cluster>-registry:5000`, the one the Harness dev
  image uses; created when missing) as `vm-manager-guest-image:dev` with
  `vm-manager image push` — the same code the release pipeline runs — and
  records the manifest digest in `state/vm-manager-guest-image.json`. A
  rebuilt image is a new digest: the next `platform` rolls the pod, whose init
  container replaces the directory. Nothing is fixed at `kind create` any more.
- **`agentlab platform`** turns `components.vm-manager` on in the chart's
  values: the meta chart's own `vm-manager:` block brings the pinned Service
  name, OAuth against `global.identity` (the lab Dex, private URLs and IPs)
  and the muster `MCPServer` with the `agent-platform` tool group and
  forward-token auth; the lab adds `guestImage` (the lab registry, by digest,
  plain HTTP) when a local build is configured and a state claim
  (`persistence.create: true`, the node's local-path storage) so the fetched
  image, its golden PCR values and the lab's VM records survive a pod restart.
  A `platform.devImages.vm-manager` build is side-loaded into the node and
  swapped in like model-manager's (both containers of the pod run it). The run
  waits until muster reports the registration reachable.
- **`agentlab vm-manager-test`** is the proof (below).

An agentlab before 0.44 registered a vm-manager running on the host
(`state/vm-manager.env`, a lab-created `MCPServer`); `agentlab platform`
removes that registration before the install — the chart renders one of the
same name — and the environment file with it.

## Recording the image's golden PCR values

A released guest image artifact (vm-manager 0.23.0 and later, the pod's
default when `platform.vmManager.imageDir` is unset) carries the golden
values its release pipeline recorded for the same release's firmware
(`golden.sha256` and `golden_firmware` in its `policy.json`): the pod
verifies both quotes of its first VM, nothing to record. A local build
(`imageDir`) carries PCR 11 (the UKI) and PCR 13 (the Kubernetes sysext) but
no golden values for the firmware PCRs, and vm-manager's verifier rejects
every quote until they exist — so the proof boots its VM with
`require_attestation: false` and says so. The firmware is the **pod's** OVMF
(Ubuntu 26.04's, inside the vm-manager image), not the host's: values
recorded for another firmware fail every quote (`golden mismatch` on PCRs 0
and 7, naming the build they were recorded for) and the VM never releases
its user-data. For a local build, record them once per guest image and
vm-manager image, through the lab's identity, inside the pod — the image
directory is the pod's state volume, and the pod's environment makes
`vm-manager image golden` default to the pod's server and directory:

```sh
# 1. one boot in learn mode: the pod accepts the golden PCRs it has no value for
#    (an overlay in platform.valuesFiles, then `agentlab platform`)
cat > learn.yaml <<'YAML'
vm-manager:
  vm:
    learnGolden: true
YAML
agentlab login admin@lab.local      # writes .token
kubectl --kubeconfig state/kubeconfig -n agent-platform port-forward svc/vm-manager 18080:8080 &
curl -s -H "Authorization: Bearer $(cat .token)" -H 'Content-Type: application/json' \
  -d '{"name":"golden-bringup","cpus":1,"memory_mib":1024,"disk_gib":8,"require_attestation":true,"wait_for":"ready"}' \
  http://127.0.0.1:18080/api/v1/vms | jq -r .id
# 2. write the VM's verified ready-stage quote into the image's policy.json, inside the pod
kubectl --kubeconfig state/kubeconfig -n agent-platform exec deploy/vm-manager -- \
  vm-manager image golden giantswarm-vm-base_0.1.0 --from-vm <id> --token "$(cat .token)"
# 3. delete the VM, drop the overlay, `agentlab platform` (the pod restarts and reads the policy): every later boot is compared
curl -s -X DELETE -H "Authorization: Bearer $(cat .token)" http://127.0.0.1:18080/api/v1/vms/<id>
```

Recording *again*, over values that exist, has one step more: learn mode
accepts only a PCR the policy has no value for, so a stale value is still a
`golden mismatch` in the learn boot. Clear the image's values on the state
claim first; the learn-mode `agentlab platform` of step 1 then restarts the
pod, which reads the policy at start (the artifact digest is unchanged, so
the directory stays):

```sh
kubectl --kubeconfig state/kubeconfig -n agent-platform exec deploy/vm-manager -- \
  vm-manager image golden giantswarm-vm-base_0.1.0 --clear
```

From then on `list_images` reports the golden values, `agentlab
vm-manager-test` boots its VM with `require_attestation: true` and reads both
quotes verified. The values live in the pod's state claim: they survive pod
restarts, and a *new* guest image (another digest — a rebuilt local build, or
a vm-manager release with another image) replaces the directory, so `image
golden` again. To keep them for a local build, copy the pod's `policy.json`
back into the checkout's `images/build` (`kubectl cp` from
`/var/lib/vm-manager/images/policy.json`) before the next push: it travels
inside the artifact.

**The firmware PCRs follow the vm-manager release, not the guest image.**
PCRs 0, 2, 3 and 7 measure the OVMF the pod boots with: `ovmf-generic` of the
vm-manager image, pinned in its `Dockerfile` since vm-manager 0.22.12. A
release that moves the pin is a pull request and a release note of its own,
`update OVMF to <version>, re-record golden PCRs`. It changes PCR 0 while the
guest image, its digest and the values on the state claim stay as they
were, and every quote then fails with `golden mismatch: pcr 0`
(`vm-manager-test` names this at `get_vm_attestation`, with this recipe as
the fix). `get_host` reports the build the pod boots with as `firmware` (the
code image's SHA-256, the package and version), and `vm-manager-test` prints
it next to the host. A release that moves the pin ships values for the new
build in its guest image artifact; a local build's values are recorded again
after every change of it.
Before the pin, 0.20.2 → 0.21.0 moved the unpinned `ovmf` from
`2025.11-3ubuntu7` to `2025.11-3ubuntu7.2` and changed PCR 0 alone (PCRs 2,
3, 4, 7 and 13 kept their values).

## The proof

```
agentlab vm-manager-test [email] [--skip-vm] [--vm-timeout 6m]
```

1. **The identity boundary, against the pod's Service from inside the
   cluster** (a probe pod, the way muster dials it). `GET /api/v1/host`
   without a token answers 401; with the person's Dex id_token — the very
   token muster forwards — it answers the capability report (`ready`,
   `missing`, the kernel, CPUs, memory). A vm-manager that answers
   anonymously (OAuth off in the release's values), or refuses the lab's token
   (another issuer, CA or audience), fails here with the fix.
2. **Through muster as the person.** The `x_vm-manager_*` tools are
   aggregated — the core set of nineteen — with vm-manager's annotations
   intact (`get_host` read-only, `delete_vm` destructive: what a model reads
   before it calls); `get_host` names the same host as the direct call;
   `list_images` and `list_networks` answer (an image without golden PCR
   values is reported as such).
3. **The registration.** The chart's `MCPServer` carries the agent-platform
   tool group and reads Connected.
4. **The lifecycle** (unless `--skip-vm`, and only on a host that reports
   `ready` with an image): `create_vm` on the newest image (1 CPU, 1 GiB, an
   8 GiB disk, `wait_for: none`), the states followed with `get_vm` — the
   installer boot, the installed boot, `READY=1` over vsock — until `ready`,
   with the install and boot durations; `get_vm_attestation` with both stages
   verified when the image's policy carries golden values (until `vm-manager
   image golden` recorded them the VM boots with `require_attestation: false`,
   and the proof says so); the last console lines; `delete_vm`, and
   `list_vms` without it. A failed boot carries vm-manager's `lastError` and
   the console tail. A leftover of an interrupted run
   (`agentlab-vm-test-*`) is removed first.

The lab's `platform-test` keeps its tool-group check: the chart's servers
(agent-manager, model-manager, vm-manager) carry the platform group from their
templates; nothing lab-created may claim it (the fake fleet carries
`infrastructure`, the OAuth fixture no label).

## Interactive use

Through the lab muster as an MCP server (`https://muster.127.0.0.1.nip.io/mcp`):
`x_vm-manager_get_host` first, then `x_vm-manager_list_images` and
`x_vm-manager_list_networks`, then `x_vm-manager_create_vm` — the server's
own instructions say the order. In the portal, the MCP servers page lists
vm-manager under Agent Platform; an agent with a toolset that admits it can
provision VMs as the person who asked. `agentlab logs vm-manager` follows the
pod (`--verbose` is on in the lab's values for a dev image).

The pod's VMs end with the pod: a `agentlab platform` that rolls the
Deployment (a new dev image, a new guest image digest, changed values) stops
every VM; their records and disks stay on the state claim for the next
`start_vm`. What is **not** in the lab: a network policy around the pod (the
lab runs without policies).
