# User guide: per-disk I/O QoS

A Machine on the `qemu` backend can cap each disk's IOPS and bandwidth.
The limits sit in the disk's `io` block: `spec.image.io.throttle` for the
root disk, `spec.disks[].io.throttle` for a data disk. QEMU enforces them
in its block layer, so they hold no matter what the guest does or which
storage backs the disk. Changing or removing a limit on a running Machine
takes effect within one kairon-node reconcile, without a restart.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  runtime: {backend: qemu}
  image:
    path: /var/lib/fluxvm/images/ubuntu-noble-base.img
    io:
      throttle: {iopsTotal: 2000}
  resources: {cpu: "4", memory: 8Gi}
  disks:
  - name: data
    claimName: db-data
    io:
      bus: virtio-blk
      throttle:
        iopsRead: 5000
        iopsWrite: 3000
        bpsTotal: 209715200      # 200 MiB/s
        bpsTotalMax: 419430400   # bursts to 400 MiB/s
        burstSeconds: 10
```

## Fields

| Field | Meaning |
|---|---|
| `iopsTotal`, `iopsRead`, `iopsWrite` | Operations per second. `iopsTotal` excludes the read/write pair. |
| `bpsTotal`, `bpsRead`, `bpsWrite` | Bytes per second. `bpsTotal` excludes the read/write pair. |
| `iopsTotalMax`, `bpsTotalMax` | A burst ceiling above `iopsTotal`/`bpsTotal`; needs the base limit and must not be below it. |
| `burstSeconds` | How long a burst may last, 1-86400 (default 1). Needs a `*TotalMax`. |

Each limit is 1 to 10^15. The CRD refuses the combinations QEMU would
refuse, and a throttle on a `vhost-user-blk` disk: that disk is served by
`qemu-storage-daemon`, so its I/O never passes through QEMU's block layer.
`scsi` (the default) and `virtio-blk` data disks and the root disk can be
throttled.

QEMU allows a short burst even without `*TotalMax`: about a tenth of a
second's worth of I/O above the limit. A measurement over a second or two
can read 5-15% over the limit; over longer windows it converges on it.

## How it is applied

- At create, the root limit goes to FluxVM in `root_disk_io.throttle`, and a
  data disk's limit in the `io` it is attached with. FluxVM sets them with
  QMP `block_set_io_throttle` as soon as QEMU starts, and again on every
  later start of the VM, a migration target included (QEMU does not
  migrate them). If QEMU refuses, the launch fails rather than running
  unthrottled.
- On a running Machine, kairon-node compares spec with
  `status.appliedDiskThrottles` and calls FluxVM's
  `PUT /v1/vms/{id}/disks/{name}/throttle` for each disk that changed. A
  limit removed from spec is lifted, then dropped from status. FluxVM
  keeps the new values, so they survive a restart of the VM.
- A root limit on its own does not change the root disk's QEMU device, so
  adding one later does not move the guest's PCI layout.

```console
$ kubectl get machine db -o jsonpath='{.status.appliedDiskThrottles}'
[{"disk":"data","throttle":{"bpsTotal":209715200,...}},{"disk":"root","throttle":{"iopsTotal":2000}}]
```

On the node, `fluxctl disk throttle <vm> <disk|root> --iops-total 2000`
sets the same limits on any FluxVM QEMU VM, and `fluxctl disk list` shows
them.

## virtiofs DAX

Not available. DAX for virtiofs needs a shared-memory window on the device:
`cache-size` on QEMU's `vhost-user-fs-pci` or `dax=on` on Cloud
Hypervisor's `--fs`. Upstream QEMU never merged it (10.2 has no
`cache-size`), and Cloud Hypervisor v53 has no `dax` option either.
virtiofs shares (`spec.volumes[1+]`) keep using the page cache inside the
guest.

## Lab results

`scripts/lab-qos.sh --node=NAME --image=PATH` runs on one node. See
[`COMPATIBILITY.md`](../COMPATIBILITY.md#disk-qos) for the rows from the
last run: limits at boot, a live change with the same QEMU process, a lift,
a recreate, FluxVM's own stop/start, and CRD validation, each checked in
QEMU over QMP and measured from inside the guest.

## Limits today

- `qemu` backend only. Cloud Hypervisor and Firecracker have their own
  rate limiters, which FluxVM does not expose yet.
- Limits are per disk. There is no shared budget across a Machine's disks
  or across Machines (QEMU throttle groups), and no StorageClass-level
  policy.
- Limits across a live migration have not run in the lab (one node).
