# vm-manager

The platform's VM provisioner ([giantswarm/vm-manager](https://github.com/giantswarm/vm-manager))
in the lab: KVM virtual machines with an instance metadata service, a vTPM
with measured boot, an immutable image and attestation, exposed to muster and
the agents as `x_vm-manager_<tool>` and to the portal as one more server of
the Agent Platform group. vm-manager is the sibling of agent-manager (agents)
and model-manager (models) — the write surface for VMs — and runs the way they
do: as a **pod**, the agent-platform chart's `components.vm-manager`. The kind
node is a privileged docker container, so the host's `/dev/kvm` and
`/dev/vhost-vsock` are in it, and the chart mounts them into the pod; QEMU and
swtpm are the pod's children.

## What the lab does

`platform.vmManager` in `agentlab.yaml`:

```yaml
platform:
  vmManager:
    enabled: true
    imageDir: /home/you/projects/giantswarm/vm-manager/images/build   # `make -C images` in a vm-manager checkout
  devImages:
    vm-manager: vm-manager:dev                                        # optional: a build of the checkout (`make docker-build`)
```

- **`agentlab configure`** reports whether this machine has the KVM devices
  (`KVM  /dev/kvm and /dev/vhost-vsock present — platform.vmManager on, images
  from …`). A machine without them turns the key off with the reason; a
  machine with them keeps what the file says — the pod is a heavier piece of
  the lab than a model server, so it is never turned on by itself.
  `--vm-manager[=false]` pins it, `--vm-manager-image-dir <dir>` names the
  image directory.
- **`agentlab up`** mounts `imageDir` into the kind node at
  `/var/lib/agentlab/vm-manager/images` (read-only, a kind `extraMount` —
  fixed at `kind create` like the port mappings, so a changed directory means
  `agentlab down && agentlab up`), and refuses the key on a node without the
  devices or the mount, with the fix.
- **`agentlab platform`** turns `components.vm-manager` on in the chart's
  values: the meta chart's own `vm-manager:` block brings the pinned Service
  name, OAuth against `global.identity` (the lab Dex, private URLs and IPs)
  and the muster `MCPServer` with the `agent-platform` tool group and
  forward-token auth; the lab adds `images.hostPath` (the node path above) and
  keeps the state an emptyDir (the lab's VMs are throwaway). A
  `platform.devImages.vm-manager` build is side-loaded into the node and
  swapped in like model-manager's. The run waits until muster reports the
  registration reachable.
- **`agentlab vm-manager-test`** is the proof (below).

An agentlab before 0.44 registered a vm-manager running on the host
(`state/vm-manager.env`, a lab-created `MCPServer`); `agentlab platform`
removes that registration before the install — the chart renders one of the
same name — and the environment file with it.

## Recording the image's golden PCR values

A freshly built image's `policy.json` carries PCR 11 (the UKI) and PCR 13
(the Kubernetes sysext) but no golden values for the firmware PCRs, and
vm-manager's verifier rejects every quote until they exist — so the proof
boots its VM with `require_attestation: false` and says so. The firmware is
the **pod's** OVMF (Ubuntu 26.04's, inside the vm-manager image), not the
host's: values recorded for another firmware fail every quote (`golden
mismatch` on PCRs 0 and 7) and the VM never releases its user-data. Record
them once per image and vm-manager image, through the lab's identity:

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
# 2. write the VM's verified ready-stage quote into the image's policy.json (on the host: the mount is read-only in the pod)
vm-manager image golden giantswarm-vm-base_0.1.0 --from-vm <id> \
  --server http://127.0.0.1:18080 --token "$(cat .token)" --image-dir <imageDir>
# 3. delete the VM, drop the overlay, `agentlab platform` (the pod restarts and reads the policy): every later boot is compared
curl -s -X DELETE -H "Authorization: Bearer $(cat .token)" http://127.0.0.1:18080/api/v1/vms/<id>
```

From then on `list_images` reports the golden values, `agentlab
vm-manager-test` boots its VM with `require_attestation: true` and reads both
quotes verified. A new vm-manager image with another OVMF build changes PCRs 0
and 2-4: `image golden` again.

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
Deployment (a new dev image, changed values) stops every VM; their records
stay in the emptyDir only as long as the pod does. What is **not** in the
lab: a persistent state claim (`vm-manager.persistence`) and a network policy
around the pod (the lab runs without policies).
