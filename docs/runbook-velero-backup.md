# Runbook: backing up Kairon with Velero

[Velero](https://velero.io) is a widely-used, generic Kubernetes backup
tool. This documents what actually works against Kairon's CRDs with
**plain Velero, no Kairon-specific plugin** -- and names the one real gap
plainly rather than glossing over it. Read this alongside
[`docs/runbook-backup-restore.md`](runbook-backup-restore.md) (Kairon's
own hand-rolled `scripts/backup-crds.sh`/`restore-crds.sh`) -- the two are
complementary, not competing: use whichever fits your existing tooling.

## What works today: CRD/object-level backup and restore

A generic Velero backup of Kairon's namespaces/CRDs works with no plugin,
confirmed by code inspection (not yet drilled against a real Velero
installation in this repo's own CI -- see "Real limits" below):

- **No finalizer blocks a restore.** `Machine`'s own
  `kairon.zyvor.dev/runtime-cleanup` finalizer (added the first time
  `kairon-node` reconciles a live Machine, removed only after a
  `DeletionTimestamp`-triggered cleanup succeeds) never affects a
  restore: Velero's restore path only ever `Create`s objects that don't
  already exist in the target, it never `Delete`s anything, so the
  finalizer's own removal path is simply never invoked by a restore.
- **A restored Machine re-adopts its FluxVM runtime automatically**, the
  exact same reconcile-loop behavior
  [`runbook-backup-restore.md`](runbook-backup-restore.md) already
  documents for Kairon's own restore script -- `kairon-node`'s `current()`
  falls back to looking up the runtime by its deterministic name
  (`RuntimeName()`) whenever `status.runtimeID` is empty. This is a
  property of the reconcile loop itself, triggered by any fresh Machine
  object with reset status, regardless of which tool created it. Velero's
  own default behavior of not restoring `status` (a real Kubernetes
  subresource, dropped on restore by default) is exactly the case this
  already handles.
- **Velero's own tracking labels survive.** Every Kairon `Patch*` call
  uses a JSON merge patch scoped to `metadata.finalizers`/`spec`/`status`
  only -- nothing ever replaces the whole `metadata.labels`/`.annotations`
  map, so labels Velero itself adds (`velero.io/backup-name`, etc.) are
  never wiped out by a later Kairon reconcile.

## The one real gap: disk content is not covered by Velero's standard CSI snapshot mechanism

**Velero's generic CSI plugin cannot back up a Machine's boot disk
content** for either storage path Kairon supports today:

- A `hostPath`/`local` `PersistentVolume` (the common case --
  [`machine-storage.md`](guides/machine-storage.md)) isn't a real CSI
  volume at all, so there's nothing for Velero's CSI plugin to snapshot.
- Kairon's own iSCSI CSI driver (`csi.kairon.zyvor.dev`,
  [`machine-storage-csi.md`](guides/machine-storage-csi.md)) *does* now
  implement `CreateSnapshot`/`DeleteSnapshot` for dynamically-provisioned
  volumes (`csiController.enabled` + `csiController.snapshotter.enabled`)
  -- Velero's generic CSI plugin should work against a `VolumeSnapshotClass`
  naming it, the same as any other CSI driver with snapshot support. Not
  yet drilled end-to-end against a real Velero backup/restore in this
  repo's own CI, and still doesn't help the far more common `hostPath`/
  `local` PV case above, which was never a CSI volume in the first place.

This is the **same limit** `MachineSnapshot`
([`machine-snapshot-restore.md`](guides/machine-snapshot-restore.md))
already documents: real disk-content snapshotting only works when your
`StorageClass`'s actual provisioner is a real CSI driver with snapshot
support (a cloud block-storage CSI driver, Ceph RBD, etc.) -- Velero's own
CSI plugin would work fine *there*, for exactly the same reason
`MachineSnapshot` already does. Neither Kairon's own restore script nor
Velero invents disk-snapshot capability your storage backend doesn't
already have.

**Practical takeaway**: a Velero backup restores your Machines/CRDs and
re-adopts still-alive FluxVM runtimes correctly, but does **not** get you
disk content back if the underlying storage is also gone -- exactly the
same boundary `docs/runbook-backup-restore.md`'s own "what this does and
doesn't protect" section already draws for Kairon's own script.

## Setup

```bash
velero install --provider <your-provider> --bucket <your-bucket> \
  --secret-file ./credentials-velero --plugins <your-provider-plugin>

velero backup create kairon-prod \
  --include-namespaces prod \
  --snapshot-volumes=true   # only meaningful if prod's PVCs use a real
                            # snapshot-capable CSI driver, see above
```

Restore:

```bash
velero restore create --from-backup kairon-prod
```

## Real limits today

- **Not yet drilled against a real Velero installation in this repo's own
  CI** -- the analysis above is a careful code-level review (finalizer
  lifecycle, patch semantics, reconcile-loop re-adoption logic), not a
  live end-to-end restore test, the same honest caveat
  `runbook-backup-restore.md` already carries for its own full-cluster-loss
  scenario.
- **Admission webhook + quota ordering, if `webhook.enabled`.** A bulk
  restore re-creates every Machine as an individual `CREATE`, each
  evaluated against whatever `MachineQuota` usage already exists in the
  target namespace *at that instant* -- not as one atomic, whole-namespace
  operation. A restore that would have been valid all-at-once can spuriously
  reject a later object in the batch purely due to ordering. If you hit
  this: scale `kairon-controller` to 0 (or temporarily disable the
  webhook) for the duration of the restore, then scale back up once every
  object has landed.
- **No disk-content backup** unless your storage backend's own CSI driver
  supports real snapshots -- see above.
- **No Kairon-specific Velero plugin exists** -- this is plain Velero
  against plain CRDs/PVCs, nothing more.
