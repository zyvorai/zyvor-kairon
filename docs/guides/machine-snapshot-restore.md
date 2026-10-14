# User guide: restoring a MachineSnapshot

How to get a usable disk back out of a `MachineSnapshot`, and why this is a
two-step flow rather than one "restore my VM" button.

## Why there's no in-place restore

A Kubernetes `PersistentVolumeClaim`'s `spec.dataSource` can't be changed
after the PVC is bound -- there is no such thing as "restore this existing
PVC back to an earlier snapshot" in the Kubernetes/CSI storage model.
Restoring **always** means creating a **new** PVC whose `dataSource` points
at the snapshot. That also means "restore" and "clone-from-snapshot" are
the same operation here -- there's no meaningful difference to model
separately.

`Machine.spec.volumes` is creation-time-only (same as `spec.image`/
`spec.resources`) -- editing it on an already-realized Machine has no
effect. So restoring into an existing running Machine isn't possible either;
what you actually get is a fresh PVC you point a **new** `Machine` at.

## The two steps

**1. Restore the snapshot into a new PVC:**

```bash
kaironctl restore my-snapshot --target-claim db-restored-pvc
```

or as a CRD:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineSnapshotRestore
metadata:
  name: db-restore-1
spec:
  snapshotName: my-snapshot
  targetClaimName: db-restored-pvc
  # volumeName: root       # required only if the snapshot covers more than one volume
  # storageClassName: ""   # empty uses the cluster default
  # storageSize: ""        # empty defaults to the VolumeSnapshot's own reported restoreSize
```

`kairon-controller` creates a real `PersistentVolumeClaim` named
`db-restored-pvc` with `spec.dataSource` pointing at the snapshot's
underlying `VolumeSnapshot`. Watch it with `kaironctl get restores` --
`status.phase` goes `Pending` → `Succeeded` once the PVC is `Bound`.
Under a `WaitForFirstConsumer` `StorageClass` (the common case, e.g.
Rancher's `local-path-provisioner`), the PVC stays `Pending` until step 2
gives it a consumer -- that's normal, not a failure.

Or from the dashboard: the **Restores** page lists every
`MachineSnapshotRestore` in the `default` namespace with its snapshot,
target claim, phase, and (once `Succeeded`) the restored claim name, and a
form creates a new one without needing `kaironctl`/`kubectl` for that one
action. The **Snapshots** page's own table grows a **Restore** button on
each row once that snapshot is `readyToUse` -- it jumps to the Restores
page with `spec.snapshotName` pre-filled, the same "prefill and switch
page" pattern the **Snapshot** button on the Machines page already uses
for creating a `MachineSnapshot`. A **Delete** button is also available on
each restore row -- deleting a `MachineSnapshotRestore` only ever removes
that bookkeeping object; the `PersistentVolumeClaim` it already created is
a normal, independent object once `status.phase` reaches `Succeeded` and
is never cascade-deleted with it (same posture `kubectl delete` on a
completed `Job` takes toward what the `Job` produced).

**2. Point a new Machine at it** (this is just the existing PVC-backed boot
disk feature -- see [docs/guides/machine-storage.md](machine-storage.md)):

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db-restored
spec:
  volumes:
    - name: root
      claimName: db-restored-pvc
  resources: {cpu: "2", memory: "4Gi"}
  runtime: {backend: qemu}
  powerState: Running
```

## Letting the restore create the Machine (`spec.machine`)

Step 2 is optional. Set `spec.machine` and the controller creates the
Machine too, built from the spec the source Machine had when it was
snapshotted:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineSnapshotRestore
metadata: {name: web-restore, namespace: default}
spec:
  snapshotName: web-snap
  targetClaimName: web-root-restored
  machine:
    name: web-restored
    powerState: Stopped   # default; Running boots it as soon as the claim binds
```

Or from the CLI: `kaironctl restore web-snap --target-claim web-root-restored
--machine web-restored [--start]`. The dashboard's Restores form has the
same fields.

How it works:

- Each `MachineSnapshot` now records `status.sourceSpec` (a copy of
  `Machine.spec` at snapshot time), so this works after the original
  Machine is deleted. For snapshots taken before this field existed, the
  controller falls back to the live source Machine, and fails the restore
  if that Machine is gone.
- The new Machine boots only from the restored claim: `spec.volumes` is
  replaced with that claim, and `spec.disks` is cleared.
  `nodeName`, the topology allocation, MAC addresses, the tap name, and
  fixed VirtualNetwork IPs are cleared too, so the copy can run next to
  the original without clashing.
- The Machine is created right away, before the PVC binds, because
  `WaitForFirstConsumer` storage classes only bind once a consumer is
  scheduled. It gets the labels `kairon.zyvor.dev/restored-by=<restore>`
  and `kairon.zyvor.dev/restored-from-snapshot=<snapshot>`. If a Machine
  with that name already exists without the matching label, the restore
  fails rather than taking it over.
- Only the boot volume (`volumes[0]`) can be restored as a Machine. Any
  other volume is refused; restore those without `spec.machine` and attach
  them yourself.
- Atlas snapshots are not supported with `spec.machine`.

## Real limits today (v1 of this feature)

- One volume restored per `MachineSnapshotRestore` -- for a Machine with
  multiple snapshotted volumes, create one restore object per volume you
  need back.
- No admission-time validation beyond what the CRD schema enforces -- there's
  no earlier warning at `kubectl apply` time if `spec.snapshotName` points at
  a `MachineSnapshot` that isn't `Succeeded`/ready yet, or doesn't exist at
  all yet. Neither is treated as an immediate failure: `status.phase` parks
  at `Pending` (message names exactly what it's waiting on) and retries
  automatically every reconcile tick, the same "waiting on an external
  condition" posture step 1's own PVC-`Pending` case above already has --
  so creating the restore slightly before its snapshot finishes, or even
  slightly before the snapshot object itself exists (both Machines and
  MachineSnapshots created together in one GitOps apply, ordering not
  guaranteed), just works once the snapshot catches up, no need to delete
  and recreate it. The "doesn't exist at all yet" case is bounded to a
  30-second grace period (from the restore's own `creationTimestamp`) --
  long enough to absorb an ordinary apply-ordering race, short enough that
  a genuinely missing/misspelled `spec.snapshotName` still surfaces as
  `Failed` within a bounded, human-noticeable time rather than sitting
  `Pending` forever. `status.phase: Failed` is reserved for what actually
  can't resolve itself: `spec.snapshotName` still unresolved past that
  grace period, a missing/misspelled `spec.targetClaimName`, an
  ambiguous/unknown `spec.volumeName`, or a real PVC-creation error.
