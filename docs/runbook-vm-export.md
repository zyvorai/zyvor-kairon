# Runbook: exporting a Machine's disk content out of the cluster

How to get a Machine's boot disk *bytes* off the cluster entirely (a
different site, an object-storage bucket, a laptop) -- for cross-cluster
clone, off-site backup, or migrating a Machine's data to a completely
different platform.

## Built in: `MachineExport` and `kaironctl export`

Like KubeVirt's `VirtualMachineExport`, but with no export Pod or
Service. The node that holds the disk serves it through its existing
relay, and kairon-ui proxies the download:

```bash
kaironctl stop web-1            # or: kaironctl halt web-1
kaironctl export machine web-1 -o web-1.qcow2 --format qcow2
```

`kaironctl export machine` creates a `MachineExport`, waits until it is
`Ready`, downloads it through kairon-ui (`KAIRON_UI_URL`,
`KAIRON_UI_TOKEN`), then deletes the export unless you pass `--keep`.
An interrupted download resumes when you rerun it with the same `--name`.
`-o -` writes to stdout. The dashboard's **Exports** page offers the same
flow, with a one-time download link.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineExport
metadata: {name: web-1-export, namespace: default}
spec:
  machineName: web-1
  format: qcow2     # or raw; empty keeps the disk's own format
  ttl: 2h           # default 2h, max 168h
```

How it works:

- kairon-node on the Machine's node resolves the boot disk. A `Halted`
  Machine keeps its FluxVM record, so its live disk path is used. A
  `Stopped` Machine has no FluxVM VM any more, so only a hostPath or
  local PersistentVolume boot disk can be exported.
- If `spec.format` differs from the disk's own format, kairon-node runs
  `qemu-img convert` into `--export-dir` (default
  `/var/lib/kairon/exports`); `status.phase` is `Preparing` meanwhile. The
  copy is removed when the export expires or is deleted.
- The node relay serves only disks its own agent registered for a `Ready`
  export. It never serves a path taken from a request or from object
  status. Downloads go through the shared node relay token, and support
  HTTP Range requests for resuming.
- kairon-ui serves `GET /api/v1/exports/{namespace}/{name}/download` (bearer
  auth) and `POST .../link`, which returns a single-use
  `/api/v1/export-links/{ticket}` URL valid for 30 seconds, for browsers.
  Both apply the console access rules: a Machine annotated
  `kairon.zyvor.dev/console-allowed-users` can only be exported by those
  users or an admin.
- If the Machine starts again, the export drops back to `Pending` and the
  node stops serving it. A download already in flight keeps reading the
  file, so don't start the Machine until the download is done.

Real limits of the built-in path:

- The source is a Machine. To export a `MachineSnapshot`, restore it into
  a new Machine first (`kaironctl restore SNAP --target-claim C --machine
  M`), then export that Machine.
- CSI-backed and Atlas rbd boot volumes can't be exported while the
  Machine is `Stopped`. Halt the Machine instead, or use the Pod pattern
  below.
- Needs the console relay (`console.enabled` in the chart), since that's
  the path the bytes take.
- kairon-node must be able to read the disk file. That holds for the host
  install (`deploy-remote.sh`, kairon-node as a systemd service). The
  Helm chart's DaemonSet runs kairon-node as non-root without the disk
  directories mounted, so exports fail there with a permission or
  not-found message until that changes.

## The Pod pattern, for what the built-in path doesn't cover

For CSI volumes, snapshot-only data, or pushing straight to object
storage from inside the cluster, mount the PVC in a short-lived Pod.

## Step 1: get a consistent copy first (don't export a live boot disk directly)

A Machine's boot disk is a real qcow2/raw file FluxVM has open while the
Machine is `Running` -- exporting it while live only ever gets you a
crash-consistent copy (the same risk as `cp`-ing any live database file).
Two ways to get something consistent instead:

- **Stop the Machine first** (`kaironctl stop MACHINE`), then export its
  existing `PersistentVolumeClaim` directly -- always available, no extra
  step, but takes the Machine offline for the duration.
- **Take a `MachineSnapshot` first** (see
  [`machine-snapshot-restore.md`](guides/machine-snapshot-restore.md)) and
  export the *snapshot*, restored into a new PVC
  (`kaironctl restore SNAPSHOT --target-claim export-pvc`) -- the Machine
  never stops, but this needs a real CSI snapshotter behind your
  `StorageClass` (the same requirement `MachineSnapshot` itself already
  has -- Rancher's `local-path-provisioner` doesn't have one).

## Step 2: export the PVC's content with a short-lived Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: export-db-disk
spec:
  restartPolicy: Never
  # Only needed for a hostPath/local PV -- pins the Pod to the same node
  # the PV actually lives on. Not needed for a real network-block/cloud
  # CSI volume, which any node can mount.
  nodeSelector:
    kubernetes.io/hostname: worker-1
  containers:
    - name: export
      image: alpine:3
      command: ["sh", "-c", "gzip -c /data/disk.img > /data/disk.img.gz && echo done"]
      volumeMounts:
        - {name: disk, mountPath: /data, readOnly: false}
  volumes:
    - name: disk
      persistentVolumeClaim:
        claimName: db-root-pvc
        readOnly: false
```

Then get the compressed file out however fits your environment:

```bash
kubectl wait --for=condition=Ready pod/export-db-disk --timeout=5m || kubectl logs pod/export-db-disk
kubectl cp export-db-disk:/data/disk.img.gz ./disk.img.gz
kubectl delete pod export-db-disk
```

For a large disk, push directly to object storage from inside the Pod
instead of `kubectl cp` (swap the container image/command for one with
`aws s3 cp`/`rclone`/etc. and the right credentials mounted) -- avoids
buffering the whole file through your own machine.

To go the other direction (import an exported image into a new cluster),
place the file where `spec.image.path`/`spec.image.source` can reach it
and boot a new Machine from it normally -- see
[`machine-storage.md`](guides/machine-storage.md)/
[`machine-image-import.md`](guides/machine-image-import.md).

## Real limits of the Pod pattern

- **No Kairon-specific tooling.** This is plain Kubernetes primitives
  (`Pod`, `PersistentVolumeClaim`, `kubectl cp`), with no progress tracking.
- **No built-in encryption or compression choice.** The example above
  gzips; swap in whatever your own security/bandwidth requirements need
  (the export Pod's command is yours to change).
- **hostPath/local PVs need node pinning** (`nodeSelector`) since the
  data only exists on that one node's local disk -- a real network-block/
  cloud CSI volume doesn't have this restriction.
- **Not a snapshot in itself.** Exporting a `Stopped` Machine's PVC
  directly is safe and consistent on its own; exporting a `Running`
  Machine's PVC without stopping it or snapshotting first only ever gets
  you a crash-consistent copy, the same risk as `cp`-ing any live disk
  image.
