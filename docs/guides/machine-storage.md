# User guide: Machine storage

How to boot a Machine's disk from a `PersistentVolumeClaim` instead of a
hand-placed image file on the node, and what today's real limits are.

## The two ways to give a Machine a disk

| Field | What it means | Node-placement requirement |
|---|---|---|
| `spec.image.path` (default) | A plain file path already present on the node the Machine is scheduled to | An operator/image pipeline has to put the file there first, and it must live under `--image-root` if the node enforces one |
| `spec.volumes[0]` | The Machine boots from a file named `disk.img` inside the directory a Bound `PersistentVolumeClaim` resolves to | The bound `PersistentVolume` must already be a real, attach-ready directory on that node -- see limits below |

`spec.volumes[0]` takes priority when set; `spec.image.path` is only used as
a fallback when `spec.volumes` is empty. Both are creation-time-only, same
as `spec.resources`/`spec.network.forwards` — changing them on an existing
Machine has no effect.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  volumes:
    - name: root
      claimName: db-root-pvc
  resources: {cpu: "2", memory: "4Gi"}
  runtime: {backend: qemu}
  powerState: Running
```

`kairon-node` resolves `db-root-pvc` → its `status.phase == Bound` check →
`spec.volumeName` → the matching `PersistentVolume` → that PV's real host
directory, and boots from `<directory>/disk.img`. If you're hand-provisioning
storage for a test, that means placing (or copying) your qcow2/raw image at
exactly `<PersistentVolume path>/disk.img` before the Machine reconciles.

## Seeding a boot volume

An empty `spec.volumes[0]` no longer has to be filled by hand. When the
Machine also sets `spec.image.source` (or `imageRef`), kairon-node copies
the cached image into `<volume>/disk.img` before the first boot; with
`spec.image.blank` it creates a sparse empty `disk.img` of
`spec.image.diskSize` instead (an ISO install onto the volume, see
[`machine-install-media.md`](machine-install-media.md)).

```yaml
spec:
  image: {imageRef: ubuntu-24-04}
  volumes: [{name: root, claimName: db-root-pvc}]
```

Seeding only happens when `disk.img` is missing or empty: an existing disk
is never overwritten, so republishing the image or restarting the Machine
can't clobber it. A Block-mode volume of Kairon's own CSI driver gets the
image written onto the device instead, only while its first MiB is still
zero, and boots from the device in place. A qcow2 image needs `qemu-img`
on kairon-node's `PATH` for this, so the kairon-node container image
takes raw images only
(see [machine-storage-nvmeof.md](machine-storage-nvmeof.md)).

## Shared hostPath volumes (live migration)

A `hostPath` PersistentVolume annotated `kairon.zyvor.dev/shared-storage: "true"`
says its directory is one shared filesystem (NFS, for example) mounted at the
same path on every node. A Machine booting from it opens `disk.img` in place
(FluxVM `storage: shared`) instead of through a node-local overlay, which is
what lets it live-migrate between nodes. Kairon cannot verify the claim: a
path that is not really shared corrupts the disk on the first migration.

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: db-root
  annotations: {kairon.zyvor.dev/shared-storage: "true"}
spec:
  capacity: {storage: 20Gi}
  accessModes: [ReadWriteMany]
  storageClassName: ""
  hostPath: {path: /srv/kairon-shared/db}     # contains disk.img
  claimRef: {namespace: default, name: db-root}
```

Pair it with a Bound claim and `spec.volumes[0].claimName`. `spec.image.path`
Machines are never live-migrated: the migration is `Blocked` with "live
migration needs the disk on shared storage". Every node's FluxVM must support
`storage: shared`, and its systemd unit (which runs `ProtectSystem=strict`)
needs the shared directory in `ReadWritePaths` (for example a drop-in with
`ReadWritePaths=-/srv/kairon-shared`). See
[runbook-multi-host-migration-test.md](../runbook-multi-host-migration-test.md).

## Real limits today

- **Boot volume:** `spec.volumes[0]` is the boot disk (`disk.img` inside the
  PVC directory), same as before.
- **Additional volumes (`spec.volumes[1+]`):** mapped as FluxVM **virtiofs**
  shared folders (QEMU only). Default guest mount is `/mnt/<name>` (override
  with `guestPath`). FluxVM has no multi-block-disk create API yet; this is
  the supported first cut — not a raw virtio-blk data disk.
- **`Block`-mode boot volumes.** A `Filesystem`-mode PV boots from
  `disk.img` inside it; a `Block`-mode PV boots from the raw device itself
  (FluxVM opens it as the base image, format detected with `qemu-img`).
  For `hostPath`/`local` PVs the PV path must name the device (for example
  `/dev/disk/by-id/...`). For third-party CSI drivers kairon-node requests
  a `Block` volume capability and boots from the published device node.
  Kairon's own iSCSI driver serves `Filesystem` volumes only, and
  `spec.volumes[1+]` (virtiofs shares) still require `Filesystem` mode.
- **`hostPath`, `local`, or Kairon's own CSI-backed volume sources.**
  `hostPath`/`local` already name a real, present-today directory on a
  specific node, resolved directly, no attach/mount step. A `csi`-backed PV
  is also resolvable now, but *only* when it names Kairon's own first-cut
  network-block (iSCSI) driver (`csi.kairon.zyvor.dev`) — see
  [`docs/guides/machine-storage-csi.md`](machine-storage-csi.md) for setup,
  and note this needs `csiNode.enabled` in the Helm chart plus
  `kairon-node`'s own `--csi-socket` flag pointed at it, neither of which
  is on by default. A PV naming any *other* CSI driver is still refused —
  Kairon only ever attaches/mounts network storage through its own driver,
  never an arbitrary third-party one (unless listed in
  `node.thirdPartyCSIDrivers`).
- **The PVC must already be `Bound`.** A `Pending` claim fails reconcile
  with a clear "not Bound yet" error rather than retrying silently forever —
  check `kubectl get pvc` if a Machine referencing one gets stuck.
- **The PVC-resolved path is exempt from `--image-root`.** That allowlist
  exists to fence an arbitrary string in `spec.image.path`; a PVC name has
  already gone through a stronger gate (the claim had to exist and be bound
  by the cluster's own storage machinery), so the same node-local directory
  restriction doesn't apply to it.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  volumes:
    - name: root
      claimName: db-root-pvc
    - name: data
      claimName: db-data-pvc
      guestPath: /var/lib/postgresql/data   # virtiofs inside guest
  resources: {cpu: "2", memory: "4Gi"}
  runtime: {backend: qemu}
  powerState: Running
```

## What this unlocks vs. what's still missing

This closes the gap between "Machines only boot from files an operator
manually placed on a specific node" and "Machines can boot from Kubernetes
storage" — for local/directory-backed storage classes (e.g. Rancher's
`local-path-provisioner`) directly, and for real network-block storage via
Kairon's own first-cut iSCSI CSI driver — see
[`docs/guides/machine-storage-csi.md`](machine-storage-csi.md). It does
**not** yet provide: clone-from-snapshot into a new running Machine
(`MachineSnapshotRestore` restores a snapshot to a new PVC only — see
[machine-snapshot-restore.md](machine-snapshot-restore.md)), CPU/memory
hotplug via this path, or support for any network-block backend other than
iSCSI through Kairon's own driver (Ceph RBD/EBS/etc. would each need their
own driver backend, not just config; third-party drivers can be allowlisted,
see [machine-storage-thirdparty-csi.md](machine-storage-thirdparty-csi.md)).
