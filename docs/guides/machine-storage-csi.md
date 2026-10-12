# User guide: CSI (network-block) Machine storage

`csiNode.enabled` (Helm chart, off by default) deploys Kairon's own
first-cut CSI node plugin, so a Machine's `spec.volumes[0]` can boot from
a real network-block volume (iSCSI, or NVMe-oF as described in
[machine-storage-nvmeof.md](machine-storage-nvmeof.md)) instead of only a
`hostPath`/`local` `PersistentVolume` that already names an attach-ready
directory -- see [`docs/guides/machine-storage.md`](machine-storage.md)
for the rest of the PVC-backed-boot-disk feature this extends.

## Why this breaks Go-stdlib-only, deliberately

Alongside `kairon-ui`'s optional OIDC/SSO, this is the other deliberate
exception to Kairon's Go-stdlib-only design guarantee. The reason is
structural, not a convenience trade: the CSI (Container Storage
Interface) protocol is a gRPC/protobuf wire contract kubelet speaks to a
plugin's Unix socket -- there is no stdlib-only way to implement a real
CSI plugin at all, full stop. `golang.org/x/oauth2`/`go-oidc`'s inclusion
for OIDC was "hand-rolling this would be unwise"; `google.golang.org/grpc`
and `github.com/container-storage-interface/spec`'s inclusion here is
"there is no other way to speak this protocol." `kairon-controller`,
`kairon-node`, and `kaironctl` pull in none of this regardless.

## What kind of CSI driver this is

**Static provisioning by default; dynamic provisioning is opt-in
(`csiController.enabled`).** By default this is a "pre-provisioned"
driver in CSI's own terminology: you (or your storage tooling) create the
`PersistentVolume` directly, naming this driver and carrying the iSCSI
target's connection info in `spec.csi.volumeAttributes` -- there's no
`ControllerPublishVolume`/attach step either way (`attachRequired: false`
on the `CSIDriver` object this chart installs, regardless of
`csiController.enabled`). Separately, enabling `csiController` deploys a
single-replica Controller service (`internal/csinode.ControllerServer`,
driven by a real Linux LIO/`targetcli` backend) so a `StorageClass`
naming this driver can dynamically provision a fresh iSCSI target/LUN per
PVC instead -- see "Setup" below for the static path and
`charts/kairon/values.yaml`'s `csiController` block for the dynamic one.
The two are independent: `csiNode` alone (the original, still fully
supported path) never requires `csiController` at all.

**Two protocols: iSCSI and NVMe-oF.** iSCSI goes through `open-iscsi`'s
`iscsiadm` CLI, the most widely available network-block protocol with a
stable, scriptable Linux initiator. NVMe-oF (`nvme-tcp`, `nvme-rdma`)
uses the kernel target and initiator directly, with no CLI; see
[machine-storage-nvmeof.md](machine-storage-nvmeof.md). A StorageClass
picks one with its `transport` parameter. Both export sparse files from
one storage node. Ceph RBD, EBS or any other backend would need its own
separate implementation; nothing here is a generic multi-backend framework.

**Kairon's own Machine boot-disk path never goes through kubelet's Pod
volume machinery.** A `Machine` is a CRD object, not a Pod -- there's no
Pod for kubelet to trigger `NodeStageVolume`/`NodePublishVolume` against.
Instead, `kairon-node` dials `kairon-csi-node`'s local Unix socket
directly, acting as its own CSI client (see `internal/agent/csi.go`) --
the same way it already reads a `hostPath`/`local` PV's directory
directly, without any Pod involved. The upstream
[`csi-node-driver-registrar`](https://github.com/kubernetes-csi/node-driver-registrar)
sidecar this chart still deploys registers the plugin with kubelet
regardless, so a real Kubernetes Pod (unrelated to Kairon, with a matching
PVC) can also use this driver normally, the standard way.

## Setup

1. Enable the driver:

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system --set csiNode.enabled=true
```

This deploys a privileged `kairon-csi-node` DaemonSet (registrar sidecar +
the driver itself) and a `CSIDriver` object named `csi.kairon.zyvor.dev`.

2. Point `kairon-node` at it -- also opt-in, independent of `csiNode.enabled`
   (a cluster might run the driver for regular Kubernetes Pods without ever
   wanting Kairon's own Machines to use it): set `node.csi.socketDir` to
   match `csiNode`'s own socket directory (they share a default and don't
   normally need changing) and make sure `kairon-node`'s `--csi-socket` flag
   is wired -- the chart does this automatically once `csiNode.enabled` is
   true, nothing extra to set for a standard install.

3. Create a static `PersistentVolume` naming an iSCSI target directly:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: db-root-iscsi
spec:
  capacity: {storage: 20Gi}
  volumeMode: Filesystem
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: csi.kairon.zyvor.dev
    # Kairon's own volume_id encoding -- see internal/csinode/iscsi.go's
    # encodeVolumeID. Must match exactly, pipe-delimited:
    # iscsi|<portal host:port>|<target IQN>|<LUN>
    volumeHandle: "iscsi|10.0.0.50:3260|iqn.2026-01.dev.zyvor:disk-1|0"
    fsType: ext4
    volumeAttributes:
      portal: "10.0.0.50:3260"
      iqn: "iqn.2026-01.dev.zyvor:disk-1"
      lun: "0"        # optional, defaults to "0"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: db-root-pvc
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 20Gi}}
  volumeName: db-root-iscsi
  storageClassName: ""
```

Then reference `db-root-pvc` from a Machine exactly as you already would
for a `hostPath`/`local`-backed one:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: db}
spec:
  volumes: [{name: root, claimName: db-root-pvc}]
  resources: {cpu: "2", memory: "4Gi"}
  runtime: {backend: qemu}
  powerState: Running
```

The first reconcile stages and publishes the volume (logs in to the iSCSI
target, formats it if it has no existing filesystem, mounts it), then
boots from `<published path>/disk.img` -- same `disk.img` convention every
other boot-disk source uses. `status.volumeStagingPath`/
`status.volumePublishPath`/`status.volumeHandle` record the result so this
only happens once, not every reconcile tick.

**Editing `spec.volumes[0].claimName` (or removing `spec.volumes`
entirely) on an existing Machine tears down the volume it's replacing.**
Every reconcile tick re-resolves `spec.volumes[0]` and compares the
result against what `status.volume*` already recorded from a previous
tick; if a still-running Machine now resolves to a genuinely different
volume (a different `claimName`, or none at all, falling back to plain
`spec.image.path`) than the one already staged/published for it,
`kairon-node` calls `NodeUnpublishVolume`/`NodeUnstageVolume` against the
*old* one before ever recording the new one in status. Without this, the
old iSCSI login/mount would simply be abandoned -- not even reachable at
Machine delete time, since by then `status.volume*` already names the
new volume -- leaking a live iSCSI session against the old target
indefinitely, and, for a `ReadWriteOnce` volume, permanently blocking it
from ever being attached elsewhere. Fails closed like every other
teardown step here: an error tearing down the old volume is returned
before the new one's staging path is ever committed to status, so the
old (still-accurate) status keeps being reported and this is retried
next tick rather than silently losing track of which volume is actually
attached.

**CHAP authentication**: supported by the driver itself (for a real
Kubernetes Pod using it via kubelet, which resolves a `nodeStageSecretRef`
Secret with its own properly-scoped RBAC) but **not** for Kairon's own
Machine-boot-disk path -- see "Real limits" below for why. This now also
applies to dynamically provisioned volumes (`csiController.enabled`):
point a `StorageClass`'s `csi.storage.k8s.io/provisioner-secret-name`/
`-namespace` and `csi.storage.k8s.io/node-stage-secret-name`/`-namespace`
parameters at the same pre-created Secret (keys `username`/`password`) to
have `CreateVolume` configure real LIO CHAP instead of demo mode -- but a
volume provisioned from that `StorageClass` can then only ever be
consumed by a real Kubernetes Pod via kubelet, never by a Kairon Machine,
for exactly the same reason. Leaving the `StorageClass` with no secret
parameters (the default) preserves demo mode exactly as before.

## Real limits today (first cut)

- **iSCSI and NVMe-oF only.** No Ceph RBD, EBS, or any other network-block
  backend.
- **Dynamic provisioning (`csiController.enabled`) is single-storage-node
  only.** One Controller replica, pinned via `nodeSelector` to whichever
  node is this cluster's storage node -- no topology-aware placement
  across multiple storage nodes. Leaving `csiController` disabled (the
  default) keeps every PV static, hand-created (or created by your own
  tooling) -- no `StorageClass`, no `CreateVolume`.
  `csiController.leaderElection.enabled` (on by default) turns on the
  `csi-provisioner`/`csi-resizer` sidecars' own Lease-based leader
  election as defense-in-depth against more than one Controller replica
  racing the same LIO target -- not real HA/failover on its own, since
  this Controller's `hostPath`/`hostNetwork`/LIO configfs are still tied
  to one physical storage node regardless of replica count.
- **Raw block mode** works on both protocols. A `volumeMode: Block`
  volume is published as its device node. A Machine boots from that
  device in place (`storage: shared` in FluxVM), with the image written
  onto it on first boot, or attaches it as a data disk. Raw block for
  iSCSI is unit-tested only; the NVMe-oF path ran in the lab
  (see [machine-storage-nvmeof.md](machine-storage-nvmeof.md)).
- **Volume expansion is supported for dynamically provisioned volumes**
  (`csiController.enabled`, `StorageClass.allowVolumeExpansion: true`):
  growing a PVC's `spec.resources.requests.storage` grows the LIO
  backstore (`ControllerExpandVolume`) and then the on-disk filesystem
  (`NodeExpandVolume`, ext2/3/4 via `resize2fs`, xfs via `xfs_growfs`) --
  grow-only, no shrink (the CSI spec has no shrink verb). Statically
  provisioned volumes still don't support this -- there's no
  `ControllerExpandVolume` call without a `StorageClass`/PVC driving it.
- **Volume stats are reported; volume health is not.** `NodeGetVolumeStats`
  is implemented: it `statfs(2)`s whatever path the CO passed to
  `NodeStageVolume`/`NodePublishVolume` (pure Go, no `df` binary) and
  reports real total/used/available bytes *and* inodes -- a filesystem can
  run out of either independently, so both are reported, not just bytes.
  This is what feeds `kubectl describe pod`'s per-volume "Used" line and
  ephemeral-storage-based eviction/metrics for a real Kubernetes Pod using
  this driver via kubelet; Kairon's own Machine-boot-disk consumption path
  (`internal/agent`) doesn't call it today (kubelet's periodic volume-stats
  collector is what normally drives this RPC, and kairon-node bypasses
  kubelet entirely for its own volumes -- see `NodeServer`'s own doc
  comment). Fails closed rather than fabricating a number: a
  `volume_path` that isn't currently a mount point returns `NotFound`, and
  a real `statfs(2)` error returns `Internal` -- never a guessed value.
  For NVMe-oF volumes, kairon-node reports path health itself, as the
  Machine's `StoragePaths` condition.
  `NodeGetVolumeHealth` remains unimplemented (the CSI spec's own
  `Unimplemented` response) -- it's a newer, far-less-adopted alpha RPC
  with no comparable orchestrator-side consumer to justify it yet.
- **Volume snapshots are supported for dynamically provisioned volumes**
  (`csiController.enabled` plus the separately-gated
  `csiController.snapshotter.enabled`, off by default): `CreateSnapshot`/
  `DeleteSnapshot` clone a volume's backing file via `cp --reflink=auto`
  (an instant, metadata-only copy-on-write clone on a filesystem that
  supports it -- btrfs, XFS with `reflink=1`, overlayfs on either -- a
  real byte-for-byte copy otherwise, coreutils' own silent, automatic
  fallback), and `CreateVolume`'s `volume_content_source` can restore a
  new volume from one. Requires your cluster to already have the
  `snapshot.storage.k8s.io` CRDs and the (separate, cluster-wide,
  not-installed-by-this-chart) `snapshot-controller` running -- the
  `csi-snapshotter` sidecar this enables fails to start at all without
  them, which is exactly why this is a second, deliberate opt-in rather
  than bundled into `csiController.enabled` automatically. No
  cross-volume dedup and no incremental/differential snapshots -- each
  is an independent full clone, so N snapshots of the same volume cost
  (at minimum, before any CoW savings) N times the space if the
  filesystem can't reflink.
- **No CHAP support on Kairon's own consumption path unless opted in.** `kairon-node`
  acts as its own CSI client. By default it never resolves a
  `nodeStageSecretRef` — doing so cluster-wide would mean granting it `get`
  RBAC on Secrets named by whatever a Machine's PV happens to reference.
  Opt-in `node.csi.chap.enabled` grants `get` on Secrets **only in the
  release namespace** and passes `--csi-chap-secret-namespace`; a PV's
  `spec.csi.nodeStageSecretRef` must name a Secret in that namespace (or omit
  namespace to default there). Real Kubernetes Pods using this driver via
  kubelet aren't affected — kubelet resolves secrets with its own,
  already-scoped RBAC.
- **Concurrent consume of a stale volume_id across replicas isn't
  guarded** the way `kairon-ui`'s console tickets are -- this driver
  assumes one Machine per volume, and Kairon never runs two `kairon-node`
  reconcile loops against the same node.
- **Privileged, larger-attack-surface container.** `kairon-csi-node` is
  the one Kairon-built image not based on the minimal distroless base
  every other component uses (it needs a real `iscsiadm`/`blkid`/`mkfs.*`
  environment around it) and the one that runs `privileged: true` -- both
  inherent to actually attaching/mounting network block devices from
  inside a container, not specific to this implementation. See
  [SECURITY.md](https://github.com/zyvorai/kairon/blob/main/SECURITY.md).
- **iSCSI's own operational requirements aren't Kairon's to solve.**
  Network reachability to the target, multipath (if you need it), and
  target-side ACLs are all your storage infrastructure's responsibility,
  same as they'd be for any other iSCSI consumer.
- **If the node already runs its own `iscsid` (e.g. `open-iscsi` installed
  at the OS level, common on storage-capable hosts), `kairon-csi-node`
  detects and reuses it instead of starting a redundant one** --
  confirmed necessary against a real host: `iscsid`'s IPC socket lives in
  the *abstract* Unix socket namespace, which `hostNetwork: true` shares
  with the node directly, so binding a second one over an
  already-running instance fails outright. The container's entrypoint
  probes (`iscsiadm -m iface`) before deciding whether to start its own.
- **The actual iSCSI attach/mount path is still not verified against a
  real target.** What *has* been confirmed on a real Kubernetes cluster:
  the plugin starts, the upstream registrar sidecar registers it with
  kubelet (`kubectl get csinodes` shows `csi.kairon.zyvor.dev` listed for
  the node), and `internal/csinode`'s orchestration logic (login/logout
  sequencing, idempotency, format-if-needed, error handling) has real
  unit test coverage against a faked command runner and mount state. But
  the actual
  OS-level behavior of `iscsiadm`/`mount`/`mkfs` can only be verified by
  running the built `kairon-csi-node` image for real.
