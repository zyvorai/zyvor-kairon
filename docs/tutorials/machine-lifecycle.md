# Tutorial: The Full Machine Lifecycle

`docs/getting-started.md` lists create → migrate → snapshot → dashboard as a terse command sequence. This walkthrough is the narrated, single-thread version: one named Machine (`database`), taken from a PVC-backed boot disk through a cold relocation, a snapshot, and a restore into a brand-new volume — the same demo Machine `examples/snapshot-machine.yaml` uses, built up step by step instead of applied all at once. Live migration needs a second host and TLS material, so it isn't repeated here — see [Next](#next) for where that continues.

## Prerequisites

- A Kubernetes cluster with at least two nodes labeled `kairon.zyvor.dev/capable=true`, each running a reachable FluxVM (default `127.0.0.1:7788`).
- `kairon-controller`/`kairon-node` installed (`kaironctl install`, or `helm upgrade --install kairon ./charts/kairon -n kairon-system --create-namespace`, or the raw manifests — see the top-level README's Quick start).
- A qcow2 image already present on the node's filesystem, e.g. `/var/lib/fluxvm/images/ubuntu-24.04.qcow2` (see `docs/getting-started.md`'s Prerequisites for how FluxVM expects this laid out).
- A CSI driver with a real `VolumeSnapshot` implementation behind your default `StorageClass` — Rancher's `local-path-provisioner`, a common default, does **not** have one ("snapshotting non-CSI volumes is not supported"); check `kubectl get volumesnapshotclass` first. Everything up through step 4 works without this; steps 5-6 need it.

## 1. Provision a boot volume

Kairon boots a Machine from `spec.image.path` (a bare file path an operator places on the node) or from `spec.volumes[0]` (a Bound `PersistentVolumeClaim`) — this tutorial uses the PVC path throughout, since that's what makes the snapshot/restore steps later possible (`spec.image.path` Machines have nothing CSI can snapshot). Full field reference: [`docs/guides/machine-storage.md`](../guides/machine-storage.md).

On the node that will run this Machine:

```bash
mkdir -p /var/lib/fluxvm/volumes/database
cp /var/lib/fluxvm/images/ubuntu-24.04.qcow2 /var/lib/fluxvm/volumes/database/disk.img
```

Then, from wherever you run `kubectl`:

```yaml
# pv-pvc.yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: database-data-pv
spec:
  capacity: {storage: 10Gi}
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath: {path: /var/lib/fluxvm/volumes/database}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: database-data
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ""
  volumeName: database-data-pv
  resources: {requests: {storage: 10Gi}}
```

```bash
kubectl apply -f pv-pvc.yaml
kubectl get pvc database-data   # wait for STATUS=Bound before continuing
```

A `Pending` claim fails a Machine's reconcile with a clear error rather than retrying silently forever, so it's worth confirming `Bound` here rather than finding out from a stuck Machine.

## 2. Create the Machine

```yaml
# database-machine.yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: database
  namespace: default
spec:
  image:
    path: /var/lib/fluxvm/images/database.qcow2   # ignored: spec.volumes[0] takes priority
  resources: {cpu: "4", memory: 8Gi}
  runtime: {backend: qemu}
  powerState: Running
  volumes:
    - name: data
      claimName: database-data
```

```bash
kubectl apply -f database-machine.yaml
kaironctl get machines
```

```text
NAME       NODE       PHASE     CPU  MEMORY  IP
database   worker-1   Running   4    8Gi     10.0.2.15
```

`spec.volumes[0]` resolves `database-data` → its Bound `PersistentVolume` → that PV's real host directory → boots from `<directory>/disk.img`, the file you placed in step 1. `spec.image.path` above is accepted by the CRD but ignored once `spec.volumes` is set.

## 3. Inspect it

```bash
kaironctl describe database
```

Prints the full `Machine` object as JSON — `status.phase`, `status.nodeName`, and (if `spec.guestAgent.enabled` was set) `status.guestIP` resolved via the real guest agent rather than a DHCP lease. See [`docs/guides/machine-guest-agent.md`](../guides/machine-guest-agent.md) if you want that turned on.

## 4. Relocate it

```bash
kaironctl migrate database --strategy cold --target-node worker-2
kaironctl get machines
```

Cold migration is stop → reassign → restart — it needs nothing extra installed and works with any storage backing, PVC-backed included: `spec.volumes[0]`'s `PersistentVolumeClaim` reference doesn't change, so the Machine just gets a fresh `kairon-node` reading the same PV. `status.nodeName` (the `NODE` column above) should now read `worker-2`.

Live migration — the secure mTLS handshake that moves a *running* Machine's memory without a restart — needs a second host with the migration adapter deployed, so it isn't repeated here: see `docs/getting-started.md`'s "Enable the secure live-migration peer" section, or drive it end-to-end for real with [`docs/runbook-multi-host-migration-test.md`](../runbook-multi-host-migration-test.md).

## 5. Snapshot it

```bash
kaironctl snapshot database --name database-before-upgrade --class csi-snapclass
kaironctl get snapshots
```

```text
NAME                       MACHINE   PHASE      READY
database-before-upgrade   database  Succeeded  true
```

Substitute your cluster's real `VolumeSnapshotClass` name for `csi-snapclass` (`kubectl get volumesnapshotclass`). `MachineSnapshot` orchestrates a standard Kubernetes `VolumeSnapshot` object underneath — it's create-only, there's no in-place "revert this Machine to a snapshot."

## 6. Restore it into a new volume

```bash
kaironctl restore database-before-upgrade --target-claim database-restored
kaironctl get restores
```

```text
NAME                                SNAPSHOT                    CLAIM               PHASE
database-before-upgrade-20260101   database-before-upgrade    database-restored   Succeeded
```

This is the actual "clone from snapshot" operation — there's no such thing as restoring a PVC in place in Kubernetes, so `MachineSnapshotRestore` always produces a **new** `PersistentVolumeClaim` (here, `database-restored`) rather than touching `database-data`. It deliberately doesn't also create a Machine for you: point a *new* Machine's `spec.volumes[0].claimName` at `database-restored` (same shape as step 2's YAML) to actually boot from the restored data — see [`docs/guides/machine-snapshot-restore.md`](../guides/machine-snapshot-restore.md) for why that's a separate step, not an oversight.

## 7. Or do all of the above from the dashboard

Every action above has a point-and-click equivalent — Machines/Migrations/Snapshots tables with the same create/relocate/snapshot actions as `kaironctl`, no side channel or second source of truth:

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system --set ui.enabled=true
kubectl -n kairon-system port-forward svc/kairon-ui 18082:18082
# then open https://localhost:18082 (a self-signed certificate; accept the warning)
```

See the top-level README's [dashboard section](https://zyvorai.github.io/kairon/docs/getting-started#deploy-the-web-dashboard) for login setup.

## Cleanup

```bash
kubectl delete machine database
kubectl delete pvc database-restored database-data
kubectl delete pv database-data-pv
```

Deleting `database-data-pv` does not delete `/var/lib/fluxvm/volumes/database/disk.img` on the node (`persistentVolumeReclaimPolicy: Retain`) — remove that directory by hand if you were just testing.

## Next

- [`docs/guides/machine-storage.md`](../guides/machine-storage.md) — PVC-backed boot disk field reference and today's real limits (one volume per Machine, `Filesystem`-mode only, `hostPath`/`local` only)
- [`docs/guides/machine-snapshot-restore.md`](../guides/machine-snapshot-restore.md) — `MachineSnapshotRestore` reference
- [`docs/guides/machine-placement.md`](../guides/machine-placement.md) — affinity/anti-affinity, for relocating a Machine onto (or away from) specific other Machines
- `docs/getting-started.md`'s live-migration section, then [`docs/runbook-multi-host-migration-test.md`](../runbook-multi-host-migration-test.md) — the secure live handshake this tutorial deliberately skipped
- [`docs/runbook-migration-failures.md`](../runbook-migration-failures.md) — what to do if a migration lands in `NeedsRecovery` instead of completing
- [`ARCHITECTURE.md`](https://github.com/zyvorai/kairon/blob/main/ARCHITECTURE.md) — how the pieces exercised above actually fit together
