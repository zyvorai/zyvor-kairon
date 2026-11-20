# User guide: third-party CSI Machine storage

`kairon-node` can act as the CSI client for an allowlisted third-party
driver, so a Machine boots from any `PersistentVolume` that driver serves:

- **Attach:** drivers whose `CSIDriver` sets `attachRequired: true`
  (EBS-CSI, PD-CSI, Azure Disk, most SAN drivers) get a `VolumeAttachment`,
  exactly what kubelet's attach/detach controller creates for a Pod. The
  driver's own external-attacher runs `ControllerPublishVolume`, and its
  `attachmentMetadata` is passed as the `PublishContext` to
  `NodeStageVolume`/`NodePublishVolume`.
- **Secrets:** `nodeStageSecretRef` / `nodePublishSecretRef` are resolved,
  but only from one operator-chosen namespace.

Request shapes are covered by unit tests against a fake CSI server and a
fake API server playing the external-attacher; no live third-party driver
has been run end to end yet (see "Real limits").

This is a separate mechanism from
[Kairon's own iSCSI CSI driver](machine-storage-csi.md)
(`internal/csinode`, deployed via `csiNode.enabled`): that guide covers
*driving* a CSI plugin Kairon itself ships. This guide covers
`kairon-node` acting as a generic CSI **client** against a third-party
driver's socket -- a different, larger piece of surface with its own real
limits.

## Why this exists

KubeVirt-class platforms can attach any CSI-backed `PersistentVolume` to a
VM because kubelet already acts as the CSI client for every Pod. A
`Machine` has no Pod, so nothing calls `NodeStageVolume`/
`NodePublishVolume` on its behalf. `kairon-node` already does this for its
own driver (`internal/agent/csi.go`); this feature lets it do the same
against an **operator-configured allowlist** of other drivers' sockets,
instead of only its own.

## Why no automatic driver discovery

A real CSI driver normally registers itself with kubelet via the
upstream `node-driver-registrar` sidecar, which populates
`/var/lib/kubelet/plugins_registry/` specifically expecting *kubelet* to
be the one dialing it (kubelet's own `pluginregistration.v1` service).
Reusing that discovery path would mean `kairon-node` re-implementing
kubelet's own plugin-registration listener. This first cut skips that
entirely: you name each driver and its socket path explicitly in
`node.thirdPartyCSIDrivers`, the same fail-closed-allowlist shape
`KAIRON_VFIO_ALLOWLIST` already has for VFIO passthrough. A driver not in
the list is refused outright -- `kairon-node` never dials an
unauthorized socket.

## Attach (`ControllerPublishVolume`)

Before staging, `kairon-node` reads the driver's `CSIDriver` object:

- `attachRequired: true`: it creates (or reuses) a cluster-scoped
  `VolumeAttachment` named `kairon-<sha256(volumeHandle+driver+node)>` with
  `attacher` = driver, `nodeName` = this node and
  `source.persistentVolumeName` = the PV. Until the external-attacher sets
  `status.attached`, the Machine waits and the node retries each reconcile
  tick; an `attachError` is reported as the reconcile error. The `kairon-`
  prefix keeps it apart from kubelet's own `csi-` attachments, which the
  attach/detach controller would delete because no Pod uses them.
- `attachRequired: false`, or **no `CSIDriver` object**: no attach step.
  (Kubernetes treats a missing `CSIDriver` as "attach required"; Kairon
  doesn't, because without one nothing says an external-attacher exists to
  act on the `VolumeAttachment`.)

On teardown (Machine delete, or `spec.volumes[0]` pointing elsewhere) the
node unpublishes and unstages, then deletes the `VolumeAttachment`; the
external-attacher runs `ControllerUnpublishVolume`. For a live migration of
a `ReadWriteOnce` volume, the destination's attach succeeds only once the
driver allows it (multi-attach support varies by driver).

The driver's controller deployment (with its `csi-attacher` sidecar) must
be running, as it would be for Pods. Setting `node.thirdPartyCSIDrivers`
grants `kairon-node` `get` on `csidrivers` and `get`/`create`/`delete` on
`volumeattachments`.

## Secrets

Letting any Machine author point a PV's secret ref at an arbitrary Secret
would be privilege escalation, so resolution is namespace-allowlisted, the
same rule [Kairon's iSCSI CHAP](machine-storage-csi.md) uses:

```yaml
node:
  thirdPartyCSISecrets:
    enabled: true   # --third-party-csi-secret-namespace=<release namespace>
```

With this on, `kairon-node` gets `get` on Secrets in the release namespace
only. A PV's `nodeStageSecretRef` / `nodePublishSecretRef` must name a
Secret there (an empty `namespace` means that namespace); every key of the
Secret is passed as the request's `secrets`, as kubelet does. A ref
anywhere else, or any ref while this is off, fails before `NodeStageVolume`
is called. Create PVs that carry secret refs with the same care as the
Secrets themselves: whoever can create a PV can name any Secret in that
namespace.

## Setup

1. Deploy the third-party driver's own DaemonSet as you normally would
   (outside Kairon entirely -- e.g. Ceph-CSI's own Helm chart), so its node
   plugin socket exists on each Kairon node, typically under
   `/var/lib/kubelet/plugins/<driver-name>/csi.sock`.

2. Allowlist it for `kairon-node`:

```yaml
# values.yaml
node:
  thirdPartyCSIDrivers:
    rbd.csi.ceph.com: /var/lib/kubelet/plugins/rbd.csi.ceph.com/csi.sock
```

This does two things: passes `--third-party-csi-drivers=rbd.csi.ceph.com=/var/...`
to `kairon-node`, and mounts the host's entire `/var/lib/kubelet/plugins`
directory read-only into the `kairon-node` container at the identical
path -- so whatever socket path you configure here is reachable
unmodified inside the container, with no per-driver volume/mount
templating needed. `kairon-node` only ever dials the one socket path
configured for a given driver name; it never lists or touches anything
else under that mount.

3. Create a `PersistentVolume` naming the third-party driver directly, the
   same shape it would take for a real Kubernetes Pod:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: db-root-rbd
spec:
  capacity: {storage: 20Gi}
  volumeMode: Filesystem
  accessModes: [ReadWriteOnce]
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: rbd.csi.ceph.com
    volumeHandle: "0001-0024-...-rbd-image-1"
    fsType: ext4
    volumeAttributes:
      clusterID: "my-ceph-cluster"
      pool: "kubevirt-pool"
      imageFeatures: "layering"
    # Needs node.thirdPartyCSISecrets.enabled; the Secret must live in
    # the Kairon release namespace (see "Secrets" above).
    nodeStageSecretRef: {name: csi-rbd-secret, namespace: kairon-system}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: db-root-pvc
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 20Gi}}
  volumeName: db-root-rbd
  storageClassName: ""
```

Then reference it from a Machine exactly as any other PVC-backed boot
disk:

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

On first reconcile, `kairon-node` dials `rbd.csi.ceph.com`'s socket
directly and calls `NodeStageVolume`/`NodePublishVolume`, then boots from
`<published path>/disk.img` -- the same `disk.img` convention every other
boot-disk source uses.
`status.volumeStagingPath`/`status.volumePublishPath`/`status.volumeHandle`/
`status.volumeDriver` record the result so this only happens once, not
every reconcile tick, and so teardown routes back to the correct driver
later even after the PV/PVC is already gone. `status.volumeDriver` stays
empty for a Machine using Kairon's own driver (or no CSI volume at all) --
existing Machines created before this field existed keep working
unchanged.

Editing `spec.volumes[0].claimName` away from a third-party-backed PVC (to
a different one, or removing `spec.volumes` entirely) tears down the old
volume via *its* driver's socket (`status.volumeDriver`, read before it's
overwritten) before the new one is ever recorded in status -- the same
"don't leak the volume being replaced" behavior
[`docs/guides/machine-storage-csi.md`](machine-storage-csi.md) describes
in full for Kairon's own driver; it applies identically here.

## Real limits today (first cut)

- **Attach requires the driver's external-attacher.** Kairon creates the
  `VolumeAttachment`; it never calls `ControllerPublishVolume` itself.
- **Secrets only from one namespace** (see "Secrets" above).
- **Explicit allowlist, not automatic discovery.** A driver not listed in
  `node.thirdPartyCSIDrivers` is refused with a clear error -- there is no
  fallback to kubelet's own `plugins_registry/` discovery.
- **Staging/publish paths are Kairon-synthesized, not kubelet's Pod-UID
  convention.** Built from the Machine's own deterministic `RuntimeName()`
  instead. Most CSI drivers treat these paths as opaque and don't care,
  but a driver that specifically depends on kubelet's own path shape
  (some drivers key internal state off it) is a named, real compatibility
  risk here, not a hidden one.
- **Only validated against fakes, not a live driver.** The request
  shapes, attach flow, secret allowlist and idempotency have unit tests
  against a fake gRPC CSI server and fake API server
  (`internal/agent/csi_thirdparty_test.go`, `csi_attach_test.go`), but no live Ceph-CSI/RBD (or
  any other third-party driver) cluster exists in this project's test
  environment to validate end-to-end. Treat this as an opt-in, first-cut
  capability until validated against your own real driver deployment.
- **Raw block mode for the boot volume.** A `volumeMode: Block` PV is
  staged and published with a `Block` volume capability; the publish
  target is a device node the driver creates, and the Machine boots from
  it directly instead of from `disk.img`.
- **No volume expansion, no snapshots** through this path. A driver's own
  `ControllerExpandVolume`/snapshot support (if any) isn't wired up here.
- **No health/stats reporting** (`NodeGetVolumeStats` is never called).
- **Read-only host mount, larger container surface.** Enabling this
  mounts the node's entire `/var/lib/kubelet/plugins` directory
  read-only into the `kairon-node` container -- broader than the single
  socket actually dialed, because per-driver dynamic volume mounts from
  an arbitrary operator-configured map aren't templated in this first
  cut. `kairon-node` itself only ever connects to the one socket path
  configured per driver name; nothing in this codebase lists or reads
  anything else under that mount. See [SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md).
