# User guide: MachineBackup and MachineBackupRestore

A `MachineBackup` copies a Machine's disks somewhere that outlives the VM;
a `MachineBackupRestore` puts them back. Unlike `MachineSnapshot`, which
takes CSI or Atlas snapshots that live next to the volume, a backup is a
full, standalone copy.

Which disks get copied depends on how the Machine boots:

| Machine boots from | What a MachineBackup copies | Where it lands |
|---|---|---|
| `spec.image` (FluxVM owns the root disk) | the root disk and every FluxVM-owned data disk, as one consistent set | FluxVM's `state_dir/backups/` on the Machine's node |
| `spec.image` with `spec.target.s3` | the same disks, full or only the blocks changed since the parent | an S3 bucket, restorable on any node ([incremental S3 backups](machine-backup-incremental.md), experimental) |
| `spec.volumes[].atlas` (with `spec.atlas` set) | each Atlas volume, through an Atlas backup job | the Atlas S3 bucket |

On-node backups are lost with the node. For off-node, incremental and
scheduled backups with GFS retention and Object Lock, see
[machine-backup-incremental.md](machine-backup-incremental.md).

PVC disks attached through `spec.disks` belong to their storage, not to
FluxVM, and are skipped. Use `MachineSnapshot` or your storage's own
backups for plain PVC volumes.

## Creating a backup

```bash
kaironctl backup create web --name nightly
kaironctl backup create db --atlas --keep 7          # Atlas volumes to S3
kaironctl backup list
```

or as a CRD:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineBackup
metadata:
  name: nightly
spec:
  machineName: web
  quiesce: auto            # auto (default) | required | never
  # atlas:                 # back up Atlas volumes to S3 as well
  #   bucketID: ""         # default: the controller's --atlas-backup-bucket
  #   volumeNames: [data]  # default: every Atlas volume
  #   keep: 7              # Atlas prunes older backups of the same volume
  #   maxAgeSeconds: 0
```

The spec is immutable. To back up again, create a new MachineBackup.

See `examples/machinebackup.yaml` for a `MachineBackup` plus the matching in-place `MachineBackupRestore`.

### Consistency

kairon-node asks FluxVM to back up the VM. For a running VM, FluxVM takes
one short internal snapshot that covers every disk, so the copies are
consistent with each other. FluxVM then copies the disks while the guest
keeps running.

With `spec.guestAgent.enabled` and `qemu-guest-agent` running in the guest,
FluxVM freezes guest filesystems for the instant of that snapshot:

- `quiesce: auto` freezes when the agent answers and otherwise takes a
  crash-consistent copy. `status.disk.quiesced` and the `QUIESCED` column
  tell you which one you got.
- `quiesce: required` fails the backup when the guest can't be frozen.
- `quiesce: never` skips the freeze.

Atlas backups are crash-consistent: Atlas snapshots the volume and exports
the snapshot to S3.

A live backup fails when the VM has a raw block device attached. QEMU's
internal snapshot can't cover that device. Halt the Machine first, or
detach the device.

### Status

```bash
kubectl get machinebackups
NAME      MACHINE   NODE       PHASE       QUIESCED   AGE
nightly   web       worker-1   Succeeded   true       2m
```

`status.phase` moves from `Running` to `Succeeded` or `Failed`. The
`status.disk` and `status.volumes[]` fields report each half separately.
The FluxVM backup name is `status.disk.name`, and the Atlas backup ids are
in `status.volumes[].atlasBackupID`.

The copy runs in the background on kairon-node. If kairon-node restarts
mid-copy, it checks FluxVM on startup:

- If the backup finished, it is recorded as `Succeeded`.
- If it didn't, the partial copy is deleted and the backup is marked
  `Failed`. Create a new MachineBackup.

### Deleting

`kaironctl backup delete nightly` (or `kubectl delete machinebackup
nightly`) has kairon-node delete the FluxVM copy before the object goes
away, using the `kairon.zyvor.dev/fluxvm-backup` finalizer. If that node is
gone for good, remove the finalizer by hand:

```bash
kubectl patch machinebackup nightly --type=merge -p '{"metadata":{"finalizers":null}}'
```

Kairon never deletes Atlas backups. Set `spec.atlas.keep` or
`maxAgeSeconds` so Atlas prunes them.

## Restoring

The disk half restores **in place**: FluxVM copies the backup over the
VM's disks. The Machine must be halted (`spec.powerState: Halted`), which
powers the VM off but keeps it. `Stopped` would delete the VM, leaving
nothing to restore into.

```bash
kaironctl halt web
kaironctl backup restore nightly            # into the backed-up Machine
kubectl get machinebackuprestores
kaironctl start web
```

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineBackupRestore
metadata:
  name: nightly-restore
spec:
  backupName: nightly
  # machineName: web-clone   # default: the backed-up Machine
  # storageClassName: ceph   # for restored Atlas volumes
```

- **Order:** a restore created before its backup finishes stays `Pending`
  until the backup succeeds. A restore created before the Machine is halted
  keeps `status.disk.message` at "waiting for the Machine to be halted".
- **Node:** FluxVM backups stay on the node that wrote them. A restore into
  a Machine on a different node fails with a clear message.
- **Atlas volumes:** each one restores into a **new** Atlas volume, since a
  bound PVC can't be swapped in place. `status.volumes[].atlasVolumeID` and
  `claimName` name the new volume. Point a new Machine at it, as you would
  after a `MachineSnapshotRestore`.
- **Interrupted restore:** if kairon-node restarts during the copy, the
  restore is marked `Failed` because the disks may be partly restored.
  Restore again before starting the Machine.

## From an AI agent (MCP)

`kaironctl mcp` exposes:

- `list_backups`: read-only.
- `machine_backup`: write-gated, with action `create`, `restore` or
  `delete`.

See [ai-agents.md](../ai-agents.md).

## RBAC and configuration

- **RBAC:** both kairon-controller and kairon-node get `get/list/watch/patch`
  on `machinebackups` and `machinebackuprestores`, including their status.
- **Helm:** `atlas.backupBucketID` sets the default Atlas bucket
  (`--atlas-backup-bucket`).
- **FluxVM:** the backup endpoints are `POST /v1/vms/{id}/backup` (with
  `name`, `all_disks` and `quiesce`), `GET /v1/backups`,
  `DELETE /v1/backups/{name}` and `POST /v1/vms/{id}/restore-backup`.
