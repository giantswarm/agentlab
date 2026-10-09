# Workspaces

Storage for the actors' external volumes in the lab: with `platform.workspaces`
on, `agentlab up` installs a CSI driver with snapshots next to the platform,
and Agent Substrate provisions each actor's workspace from it, so a Session's
clone of a Workspace is a volume restored from a snapshot the way it is on an
installation. The switch is off by default; `agentlab configure --workspaces`
turns it on (the form asks too), and it needs the agents runtime: Substrate's
signers certify the driver's endpoint, and the volumes are Substrate's.

## What the lab installs

Everything but the CSIDriverConfig lives in the namespace `agentlab-workspaces`:

| Piece | What it is |
|---|---|
| The CSI snapshot CRDs and the `snapshot-controller` Deployment | The `VolumeSnapshot`, `VolumeSnapshotContent` and `VolumeSnapshotClass` kinds and the controller that binds them, from kubernetes-csi/external-snapshotter |
| The `csi-hostpathplugin` StatefulSet | The CSI hostpath driver `hostpath.csi.k8s.io` with its sidecars (provisioner, attacher, resizer, snapshotter, node-driver-registrar, livenessprobe): one pod on one node, its volumes under `/var/lib/csi-hostpath-data` of that node, Substrate's volumes directory `/var/lib/ate` mounted `Bidirectional` so the actors see what the driver publishes |
| The `csi-hostpath-controller` Service and the `csi-hostpath-proxy` StatefulSet | The controller endpoint: an Envoy in front of the driver's socket that requires a client certificate from Substrate's pod-identity CA with ate-api-server's SPIFFE ID, its own serving certificate a `PodCertificateRequest` the service-DNS signer answers (both come from the chart's podcertificate-controller, so the proxy is waited for after the platform install) |
| The StorageClass and VolumeSnapshotClass `agentlab-workspaces` | Both delete-reclaiming, `Immediate` binding; the StorageClass with the driver's topology, so a pod using a volume lands on the driver's node |
| The `CSIDriverConfig` `hostpath.csi.k8s.io` | Substrate's registration of the driver: the controller endpoint through the proxy, the node socket override, mTLS with the pod identity. The chart renders it from the lab's `workspaces:` values once its release takes the key; until then the lab applies its own after the install (`HACKS.md` U30) |

The driver serves one node. Without reserved substrate workers it runs on the
control plane; with `substrateNodes: 1` it runs on the worker, tolerating its
taint, since the actors run there; two or more workers are refused by
`agentlab configure`. The actors' volumes, snapshots and the grants Substrate
needs live in the `kagent` namespace with the actors: per-tenant placement
waits for the platform's permissions model.

`agentlab status` prints a `workspaces` line (the snapshot controller, the
driver and its proxy on their node, the classes, whose CSIDriverConfig
registers the driver); `agentlab platform-down` and `agentlab down` remove the
pieces and clean the driver's mounts and volume bytes off the node.

## Turning it on

```sh
agentlab configure --workspaces   # needs --agents (on by default)
agentlab up                       # or `agentlab platform` on a running lab
agentlab workspaces-test --storage-only
```

With the switch off, nothing of it is installed, and `agentlab platform` on a
lab that had it removes it.

## The proof

`agentlab workspaces-test --storage-only` is headless:

1. The storage in place: the classes, the CSIDriver, the snapshot controller
   rolled out, the driver and its proxy Ready.
2. A 100Mi PVC from the StorageClass, written by a pod: two random files and a
   text file, their sha256 sums printed.
3. A VolumeSnapshot of it from the VolumeSnapshotClass, `readyToUse`.
4. A PVC restored from the snapshot, read by a pod: the sums equal the
   source's.
5. The controller endpoint through the proxy, as a client without Substrate's
   identity: with no certificate and with a self-signed one that claims
   ate-api-server's SPIFFE ID, both refused (the TLS alert quoted).
6. Through ate-api-server as the `ate-client` ServiceAccount: an atespace, an
   actor template whose workspace is an external volume on the class, and an
   actor appending the date to `/workspace/heartbeat` every second. The
   volume's directory on the node grows; after `PauseActor` it stands still;
   after `ResumeActor` the paused lines are kept and the file grows again;
   after `DeleteActor` the actor is NotFound and the directory is gone.

The proof's PVCs, snapshot and pods live in the namespace
`agentlab-workspaces-test`, its template and actor in the atespace of the same
name; both are removed before the proof starts and when it ends.
`--storage-only` is required: a harness turn against a workspace is not part
of the proof yet. `--ready-timeout` bounds each wait (default 5m).

## By hand

The same template through `kubectl ate` (Substrate's CLI against
ate-api-server), once `agentlab login` gave you the lab's kubeconfig:

```sh
kubectl ate create atespace scratch
kubectl ate create actortemplate scratch/heartbeat -f template.yaml   # the proof's template
kubectl ate create actor scratch/heartbeat --template heartbeat
kubectl ate get actor scratch/heartbeat          # RUNNING, the volume id in its status
kubectl ate pause actor scratch/heartbeat
kubectl ate resume actor scratch/heartbeat
kubectl ate delete actor scratch/heartbeat
```

The volume's bytes are on the driver's node under
`/var/lib/csi-hostpath-data/<volume id>/` (`docker exec agentlab-control-plane
ls /var/lib/csi-hostpath-data`).
