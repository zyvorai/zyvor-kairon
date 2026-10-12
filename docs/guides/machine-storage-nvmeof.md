# User guide: NVMe-oF Machine storage

Kairon's own CSI driver (`csi.kairon.zyvor.dev`, see
[machine-storage-csi.md](machine-storage-csi.md)) can serve volumes over
NVMe over Fabrics as well as iSCSI. The storage node exports each volume
through the kernel NVMe target (`nvmet`). Machine nodes connect over
`nvme-tcp` or `nvme-rdma`. With several portals, native NVMe multipath
merges the paths into one `/dev/nvmeXnY`. A `volumeMode: Block` volume
reaches FluxVM as that raw device: a Machine boots from it in place, or
attaches it as a data disk with the bus and I/O path you choose.

No `nvme-cli` or `nvmetcli` is needed. The controller writes `nvmet`'s
configfs directly. The node plugin writes connect strings to
`/dev/nvme-fabrics` and reads path state from sysfs.

## Setup

On the storage node, load `nvmet` plus `nvmet_tcp` and/or `nvmet_rdma`. On
every Machine node, load `nvme_tcp` and/or `nvme_rdma`, and keep native
multipath on (`nvme_core.multipath=Y`, the default on most distributions).
A volume with several paths is refused on a node with multipath off.

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set csiNode.enabled=true \
  --set csiController.enabled=true \
  --set csiController.portal=10.0.0.50:3260 \
  --set csiController.nvmePortals='tcp:10.0.0.50:4420\,tcp:10.0.1.50:4420' \
  --set csiController.storageClass.create=true \
  --set csiController.storageClass.name=kairon-nvme \
  --set csiController.storageClass.parameters.transport=nvme-tcp
```

`csiController.nvmePortals` (`--nvme-portals` on `kairon-csi-controller`)
lists `tcp:IP:PORT` and `rdma:IP:PORT` addresses. Use IP addresses, not
host names: the kernel target binds to them. Every volume is exported on
every portal of its transport. Two `tcp:` portals on two storage NICs
therefore give every node two paths. A portal's `nvmet` port id is 7400
plus its position in the list, so keep the order stable. A port whose
address changes while volumes are exported on it is refused rather than
rewritten.

The StorageClass parameter `transport` picks the protocol per class:
`nvme-tcp`, `nvme-rdma`, or `iscsi` (the default when unset). One
controller serves all three. The chart creates one StorageClass; add
another by hand (provisioner `csi.kairon.zyvor.dev`) for each extra
transport.

The node plugin derives the host NQN from `/etc/nvme/hostnqn`, or else
from `/etc/machine-id`. It derives the host id from `/etc/nvme/hostid`,
or else from the NQN. The chart mounts both paths read-only, so the
identity survives plugin restarts.

## Using it

A Block-mode claim as the boot disk:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: db-root}
spec:
  storageClassName: kairon-nvme
  volumeMode: Block
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 20Gi}}
---
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: db}
spec:
  image:
    digest: sha256:...
    source: {httpURL: https://images.example/noble.raw, format: raw}
    io: {aio: io_uring, cache: none, iothread: true, queues: 4}
  volumes: [{name: root, claimName: db-root}]
  disks:
  - {name: data, claimName: db-data, io: {bus: virtio-blk, aio: io_uring, cache: none, iothread: true, queues: 4}}
  - {name: logs, claimName: db-logs, io: {bus: vhost-user-blk, queues: 2}}
  resources: {cpu: "4", memory: "8Gi"}
  runtime: {backend: qemu}
```

kairon-node stages and publishes the volume with a `Block` capability.
The publish target is a bind mount of the device. On the first boot, the
image named by `spec.image.source` is written onto the device, but only
while its first MiB is still zero. A device that already holds data is
never overwritten. A raw image is copied as is. A qcow2 image is
converted with `qemu-img`, which kairon-node must find on its `PATH`. The
kairon-node container image doesn't have it, so use raw images there. A
kairon-node installed on the host can use either. FluxVM then boots the
device in place
(`storage: shared`), so the guest writes straight to the volume and a
node-local overlay is never involved. A Filesystem-mode claim still boots
from `disk.img` on the mounted filesystem, as before.

`spec.image.io` tunes the root disk, which is always virtio-blk.
`spec.disks[].io` picks each data disk's bus: `scsi` (the default),
`virtio-blk`, or `vhost-user-blk`. It also sets `aio` (`io_uring`,
`native`, `threads`), `cache` (`none`, `writeback`, `writethrough`,
`directsync`, `unsafe`), `iothread` and `queues` (1-64). Unset fields keep
FluxVM's defaults. The options apply on the `qemu` backend only and are
passed to FluxVM unchanged. Its API guide covers what each one does in
QEMU.

A `vhost-user-blk` disk is served by its own `qemu-storage-daemon` rather
than by QEMU. When a Machine lists one, Kairon creates the VM with
shareable memory so the disk can be hot-added. Inside the guest,
virtio-blk disks appear as `/dev/disk/by-id/virtio-<name>`.
vhost-user-blk disks have no serial number, so find them by size or
order.

Static PVs work too. `volumeHandle` is
`nvme|<nvme-tcp|nvme-rdma>|<subsystem NQN>|<host:port>,<host:port>`. When
`volumeAttributes` omits `transport`, `nqn` and `portals`, they are taken
from the handle.

## Path health

kairon-node sets a `StoragePaths` condition on every Machine with an
NVMe-oF boot volume or data disk. The condition is `True`/`AllPathsLive`
while every portal has a live, usable path, and
`False`/`PathsDegraded` otherwise. A usable path is ANA optimized or
non-optimized; inaccessible or lost paths don't count. The message lists
each path, for example
`2 of 2 paths usable: nvme0 tcp 10.0.0.50:4420 live optimized; ...`.
Connections use `ctrl_loss_tmo=-1`, so a lost path keeps reconnecting.
When it comes back it rejoins on its own, and while no path is up I/O
queues instead of failing.

ANA states are `nvmet`'s defaults (group 1, optimized on every port).
Kairon reads them and does not set them. To steer I/O away from a portal,
set that port's ANA state on the target yourself.

## Lab results

[`scripts/lab-nvmeof.sh`](../../scripts/lab-nvmeof.sh) runs the whole
path on one node, which acts as both storage node and Machine host. It
uses two tcp portals on a dummy interface and an rdma portal over
soft-RoCE (`rdma_rxe`). See the NVMe-oF rows in
[COMPATIBILITY.md](../COMPATIBILITY.md) for what ran and what didn't.

## Real limits today

- **No access control.** Subsystems allow any host, and there is no
  DH-CHAP or in-band authentication. Anyone who can reach a portal can
  connect to any volume. Put the portals on a storage network only your
  nodes can reach. A StorageClass with CHAP secrets is refused for NVMe-oF.
- **One storage node, file-backed.** Volumes are sparse files in
  `csiController.volumeDir`, as for iSCSI. Expansion grows the file and
  revalidates the namespace, and nodes rescan it; this ran in the lab.
  Snapshots copy the file as for iSCSI; they are unit-tested only.
  `nvmet` logs `WRITE_ZEROES` as unsupported for file-backed namespaces;
  this is harmless.
- **RDMA has run only over soft-RoCE.** Real RDMA NICs (RoCE, iWARP,
  InfiniBand) have not been tested.
- **Block mode means one Machine per volume.** The boot volume is
  written by the guest directly, with no overlay, so it can't be shared
  or cloned through FluxVM. Use CSI snapshots instead.
- **vhost-user-blk disks:** FluxVM refuses live resize and hibernation,
  and leaves them out of live and incremental (CBT) backups. Adding one to
  a Machine that already runs without one needs a restart, because the
  shareable memory is set at boot.
- **Device allowlist.** On FluxVM's eBPF dataplane, each VM's device
  cgroup has 8 entries: three fixed devices, one per VFIO group, and one
  per block device. That leaves room for five NVMe-oF disks (boot
  included) on a VM without passthrough.
- **Migration.** Detach CSI data disks before migrating, as for iSCSI.
  Migrating a Machine that boots from a Block volume has not been tested.
