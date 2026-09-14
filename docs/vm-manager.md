# vm-manager

The platform's VM provisioner ([giantswarm/vm-manager](https://github.com/giantswarm/vm-manager))
wired into the lab: KVM virtual machines with an instance metadata service, a
vTPM with measured boot, an immutable image and attestation, exposed to muster
and the agents as `x_vm-manager_<tool>` and to the portal as one more server
of the Agent Platform group. vm-manager is the sibling of agent-manager
(agents) and model-manager (models) — the write surface for VMs — and, like
the host model servers, it runs on the **lab host**, not in a pod: it needs
`/dev/kvm`, `/dev/vhost-vsock`, QEMU, swtpm and a service manager for the VMs
it boots. The lab therefore does for it what it does for an Ollama: discovers
it on this machine, proves pods can reach it, and points muster at it.

## What the lab does

`platform.vmManager` in `agentlab.yaml`:

```yaml
platform:
  vmManager:
    enabled: true   # `agentlab configure` follows whether one answers on this machine
    port: 8100      # the host port vm-manager listens on (its own default 8080 is often taken)
    # endpoint: http://host.docker.internal:8100   # the URL pods dial, when the autodetection is wrong
```

- **`agentlab configure`** looks for a vm-manager on `platform.vmManager.port`
  (loopback, then the kind gateway) and recognises it by its Prometheus
  exposition — the `vm_manager_build_info` series, with the version it
  carries. With a running node it also asks, from inside the node, which
  address pods reach it on: the kind docker network's gateway, or the
  container runtime's host alias where that gateway is inside the runtime's
  VM. What it found turns `platform.vmManager.enabled` on; nothing answering
  turns it off; `--vm-manager[=false]` pins it either way and
  `--vm-manager-port` moves the port.
- **`agentlab platform`** writes `state/vm-manager.env` (below), proves the
  vm-manager answers from a probe pod at the detected address, and applies
  the muster `MCPServer` `vm-manager` in `agent-platform`: `streamable-http`
  to `http://<address>:<port>/mcp`, `auth.type: oauth` with
  `forwardToken: true`, the label `agent-platform.giantswarm.io/tool-group:
  agent-platform` (the platform's own management surface, next to
  agent-manager and model-manager — the portal's MCP servers page lists it
  under Agent Platform). Off, the run removes the registration a lab created
  while the key was on. On a management cluster the agent-platform chart
  renders the same CR for the host it names; the lab renders it for the
  machine that runs the lab.
- **`agentlab vm-manager-test`** is the proof (below).

## Running vm-manager for the lab

Identity is the whole point of the wiring, and it works the way it does for
every downstream the lab aggregates: muster forwards the person's Dex
id_token byte-identical and vm-manager validates it against the lab Dex's
JWKS, trusting the platform client's audience. vm-manager has to be told the
lab's issuer, CA, client and audience for that, and the lab is what knows
them — so `agentlab platform` writes them to `state/vm-manager.env`
(`agentlab vm-manager-env` prints the same):

```sh
VM_MANAGER_LISTEN=0.0.0.0:8100                # pods dial the gateway or the runtime's host alias, never loopback
VM_MANAGER_OAUTH_ENABLED=true
VM_MANAGER_OAUTH_BASE_URL=http://localhost:8100
VM_MANAGER_OAUTH_PROVIDER=dex
DEX_ISSUER_URL=https://localhost:32000/dex    # the one issuer URL valid from the host
DEX_CLIENT_ID=agent-platform                  # the platform client muster and Backstage use
DEX_CLIENT_SECRET=agent-platform-lab-secret
DEX_CA_FILE=/…/agentlab/certs/ca.crt          # the lab CA: a private certificate on a loopback issuer
VM_MANAGER_OAUTH_ALLOW_PRIVATE_URLS=true
SSO_ALLOW_PRIVATE_IPS=true
OAUTH_TRUSTED_AUDIENCES=agent-platform        # the audience of the tokens muster forwards
```

Every `vm-manager serve` flag has such a variable, and flags win. Build
vm-manager from its checkout (`make build`), build an image once
(`make -C images`, mkosi 27 — the directory is the contract of `--image-dir`),
and run it from the environment:

```sh
set -a; source ~/projects/giantswarm/agentlab/state/vm-manager.env; set +a
./vm-manager serve --image-dir ./images/build
```

or as a systemd user service with `EnvironmentFile=` pointing at the file —
that is how the VMs survive vm-manager restarts (they run as transient units
of the user's service manager). vm-manager discovers the lab Dex at start,
so the lab must be up when it starts (`Restart=on-failure` covers a lab that
comes up later); `agentlab up` after `agentlab down` re-mints nothing the
file names (the CA and the client are stable), so the same environment keeps
working across labs.

The listen address is every interface on purpose: the address that reaches
this machine from a pod is the kind gateway on a native runtime and the host
alias where the runtime runs in a VM, and vm-manager's own default, loopback,
is neither. Its API is guarded — a caller without a token from this lab's Dex
gets 401 — and the lab Dex itself is reachable from this machine only.

A vm-manager running **without** `--enable-oauth` answers every call
anonymously. muster would still connect (it forwards a token nobody checks),
but every identity would be meaningless; `agentlab vm-manager-test` fails on
it first thing.

## Recording the image's golden PCR values

A freshly built image's `policy.json` carries PCR 11 (the UKI) and PCR 13
(the Kubernetes sysext) but no golden values for the firmware PCRs, and
vm-manager's verifier rejects every quote until they exist — so the proof
boots its VM with `require_attestation: false` and says so. Record them once
per image and host firmware, through the lab's own identity (the `image
golden` command takes the bearer token the lab's login writes):

```sh
# 1. one boot in learn mode: the server accepts the golden PCRs it has no value for
systemctl --user edit vm-manager    # [Service] Environment=VM_MANAGER_ATTESTATION_LEARN_GOLDEN=true
systemctl --user restart vm-manager
agentlab login admin@lab.local      # writes .token
curl -s -H "Authorization: Bearer $(cat .token)" -H 'Content-Type: application/json' \
  -d '{"name":"golden-bringup","cpus":1,"memory_mib":1024,"disk_gib":8,"require_attestation":true,"wait_for":"ready"}' \
  http://127.0.0.1:8100/api/v1/vms | jq -r .id
# 2. write the VM's verified ready-stage quote into the image's policy.json
vm-manager image golden giantswarm-vm-base_0.1.0 --from-vm <id> \
  --server http://127.0.0.1:8100 --token "$(cat .token)" --image-dir <image dir>
# 3. delete the VM, drop the learn setting, restart: every later boot is compared
curl -s -X DELETE -H "Authorization: Bearer $(cat .token)" http://127.0.0.1:8100/api/v1/vms/<id>
```

From then on `list_images` reports the golden values, `agentlab
vm-manager-test` boots its VM with `require_attestation: true` and reads both
quotes verified. A different OVMF build (a host upgrade) changes PCRs 0 and
2-4: `image golden` again.

## The proof

```
agentlab vm-manager-test [email] [--skip-vm] [--vm-timeout 6m]
```

1. **The identity boundary, directly against the host.** `GET
   /api/v1/host` without a token answers 401; with the person's Dex id_token
   — the very token muster forwards — it answers the capability report
   (`ready`, `missing`, the kernel, CPUs, memory). A vm-manager that answers
   anonymously, or refuses the lab's token (its environment names another
   issuer, CA or audience), fails here with the fix.
2. **Through muster as the person.** The `x_vm-manager_*` tools are
   aggregated — the core set of nineteen — with vm-manager's annotations
   intact (`get_host` read-only, `delete_vm` destructive: what a model reads
   before it calls); `get_host` names the same host as the direct call;
   `list_images` and `list_networks` answer (an image without golden PCR
   values is reported as such).
3. **The registration.** The `MCPServer` carries the agent-platform tool
   group and reads Connected.
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

The lab's `platform-test` keeps its tool-group check: the registration is a
lab-created server that carries the platform tool group, which the check
admits for a host-service registration and for nothing else lab-created (the
fake fleet carries `infrastructure`, the OAuth fixture no label).

## Interactive use

Through the lab muster as an MCP server (`https://muster.127.0.0.1.nip.io/mcp`):
`x_vm-manager_get_host` first, then `x_vm-manager_list_images` and
`x_vm-manager_list_networks`, then `x_vm-manager_create_vm` — the server's
own instructions say the order. In the portal, the MCP servers page lists
vm-manager under Agent Platform; an agent with a toolset that admits it can
provision VMs as the person who asked.

What is **not** in the lab: the agent-platform chart's values block for a
vm-manager host and its agentgateway route (the wiring on a management
cluster), and vm-manager's own OAuth flow (`VM_MANAGER_OAUTH_BASE_URL` names a
server the forwarded tokens never touch).
