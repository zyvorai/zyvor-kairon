# User guide: Atlas-provisioned Machine storage

[Atlas](https://github.com/zyvorai/atlas) is Zyvor's storage control plane:
one REST API in front of Ceph RBD, CephFS, NFS, and any CSI StorageClass,
with policy intents, snapshots, backups, and job tracking. With Atlas
enabled, a Machine asks for a disk by size and policy instead of naming a
PersistentVolumeClaim someone created by hand.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: web, namespace: default}
spec:
  volumes:
    - name: root
      atlas:
        size: 20Gi
        policy: gold            # Atlas policy intent (optional)
        storageClass: ceph-rbd  # optional; Atlas picks one from policy otherwise
    - name: data
      guestPath: /srv/data
      atlas:
        size: 100Gi
        retain: true            # keep the volume after the Machine is deleted
```

## What the controller does

1. Adds the `kairon.zyvor.dev/atlas-volumes` finalizer.
2. Calls Atlas `POST /volumes` with name `<machine>-<volume>-<hash>`, where
   the hash covers namespace/machine/volume so names never collide across
   namespaces (63 characters at most), tenant `--atlas-tenant`, and an
   owner of `kairon/machine/<namespace>/<name>`. Creates are idempotent, so a
   controller restart mid-create doesn't leak volumes.
3. Polls the returned job on later reconcile ticks. It never blocks a tick
   waiting on Atlas.
4. When the job succeeds, reads the volume back, records the PVC name, and
   writes it into `spec.volumes[].claimName`. From there kairon-node uses its
   existing PVC path ([machine-storage.md](machine-storage.md)).
5. Schedules the Machine only once every Atlas volume is Ready. Until then the
   Machine stays `Pending` with condition `Scheduled=False`,
   reason `VolumesNotReady`.

Per-volume state (Atlas volume id, job id, backend native id, phase) lives in
the `kairon.zyvor.dev/atlas-volumes` annotation:

```sh
kubectl get machine web -o jsonpath='{.metadata.annotations.kairon\.zyvor\.dev/atlas-volumes}' | jq
```

A volume whose Atlas job fails, or whose create Atlas rejects (bad policy,
unknown backend, size mismatch on an existing name), goes to phase `Failed`
and is not retried. Delete and recreate the Machine after fixing the spec.

## Deletion

When a Machine is deleted, kairon-node first tears down the VM and drops its
`runtime-cleanup` finalizer. Only then does the controller delete the Atlas
volumes (except `retain: true` ones) and release its own finalizer, so a disk
is never deleted under a running guest.

## Snapshots and restore

A MachineSnapshot of a Machine with Atlas volumes snapshots those volumes
through Atlas (`POST /volumes/{id}/snapshots`) instead of creating CSI
VolumeSnapshots; other volumes on the same Machine still use CSI. Each
volume's Atlas job and snapshot id appear in
`status.volumeSnapshots[].atlasJobID` / `atlasSnapshotID`. Guest quiesce
(`spec.guestAgent.enabled`) works the same as for CSI. Set
`spec.volumeNames` (or `kaironctl snapshot MACHINE --volume NAME`) to
snapshot only some volumes.

Deleting the MachineSnapshot deletes its Atlas snapshots (finalizer
`kairon.zyvor.dev/atlas-snapshots`). A snapshot that volumes were cloned
from is refused by Atlas; Kairon logs it, leaves it in Atlas and lets the
MachineSnapshot go.

A MachineSnapshotRestore of an Atlas snapshot calls Atlas
`POST /snapshots/{id}/restore` with `targetClaimName`, the namespace,
`storageClassName` and `storageSize`; Atlas creates the PVC. The restore is
Succeeded once that PVC is Bound.

```sh
kaironctl volumes web            # source, claim, size, Atlas phase, backend id
kaironctl snapshot web --volume root --name before-upgrade
kaironctl restore before-upgrade --target-claim web-root-restored
```

## Raw RBD mode

`atlas.mode: rbd` (boot disk, `volumes[0]`, only) creates a raw Ceph RBD
image (`POST /rbd-images`) instead of a PVC. kairon-node boots it with FluxVM
`storage: ceph-rbd-in-place`, where QEMU opens the image directly over librbd
with no clone and no CSI attach in the path. Every node opens the same image,
so live migration is shared-storage migration with no block copy.

- Ceph credentials stay in each node's FluxVM config (`[storage] ceph_user`,
  `ceph_conf`). The Machine carries only the `rbd:<pool>/<image>` id.
- The node only boots pools listed in `node.atlasRBDPools`
  (`--atlas-rbd-pools`); empty refuses rbd mode.
- The annotation holding the image id is editable by anyone who can edit the
  Machine, so the node recomputes the image name from the Machine's own
  namespace, name and volume and refuses any other image. A Machine can't be
  pointed at another tenant's disk.
- QEMU backend only. `spec.guestAgent.console` isn't supported with rbd boot
  (FluxVM can't inject the console agent token into an RBD image);
  `spec.guestAgent.enabled` (qemu-guest-agent) works.

```yaml
spec:
  runtime: {backend: qemu}
  volumes:
    - name: root
      atlas: {size: 40Gi, mode: rbd, pool: rbd-nvme-prod}
```

Atlas creates an empty image, so seed it with an OS before first boot (for
example `rbd import` or an Atlas restore from a backup) or boot an installer.

## Enabling

```yaml
# values.yaml
atlas:
  enabled: true
  url: http://atlas.atlas-system.svc:5110
  tenantID: kairon
  defaultPolicy: standard
  tokenSecret:
    name: atlas-kairon-token   # Secret with key "token" (Atlas HS256 JWT)
```

Or run kairon-controller with `--atlas-url`, `--atlas-tenant`,
`--atlas-default-policy`, and `--atlas-token-file`, or set `KAIRON_ATLAS_TOKEN`.

The Atlas Go client (`github.com/zyvorai/atlas/clients/go`) is Go
standard library only, so the controller keeps its stdlib-only dependency
boundary ([DEPENDENCIES.md](../DEPENDENCIES.md)). The client is
contract-tested against a live Atlas gateway in Atlas's own CI.

## Dashboard: Storage page

`kairon-ui` can show a read-only **Storage** page (Compute → Storage) backed by
the same Atlas gateway the controller provisions volumes through.

| Setting | systemd env (`/etc/kairon/kairon-ui.env`) | Helm value |
| --- | --- | --- |
| Gateway URL | `KAIRON_UI_ATLAS_URL=http://atlas:5110` | `ui.atlas.url` |
| Bearer token | `KAIRON_UI_ATLAS_TOKEN` or `KAIRON_UI_ATLAS_TOKEN_FILE` | `ui.atlas.tokenSecret` (key `token`) |
| "Open in Atlas" link | `KAIRON_UI_ATLAS_CONSOLE_URL` | `ui.atlas.consoleURL` |

- The browser never sees the Atlas token: `kairon-ui` proxies an allowlist of
  `GET` resources under `/api/v1/atlas/...` (metrics summary, pools, OSDs,
  alerts, volumes, snapshots, backups, schedules, jobs, events, DR mirror
  status).
- Volumes are filtered to owner prefix `kairon/` (the owner Kairon stamps as
  `kairon/machine/<ns>/<name>`), so a shared gateway's other products' volumes
  are not shown.
- Use a viewer-role Atlas token; the proxy is read-only either way.
- With `--namespace-scoping` on, only administrators can open the page (same
  rule as the node routes), because Atlas data spans tenants.
- Without `KAIRON_UI_ATLAS_URL` the routes answer `501` and the page explains
  how to enable it.
