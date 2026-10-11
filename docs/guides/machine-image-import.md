# User guide: Machine image import

How to boot a Machine from a remote qcow2/raw image URL instead of a file an
operator already placed on the node, and what today's real limits are.

## `spec.image.source` vs. `spec.image.path`

| Field | What it means | Node-placement requirement |
|---|---|---|
| `spec.image.path` (default) | A plain file path already present on the node the Machine is scheduled to | An operator/image pipeline has to put the file there first |
| `spec.image.source.httpURL` | A plain `http(s)://` URL to a qcow2/raw image | `kairon-node` downloads it itself into a node-local, digest-keyed cache -- see below |
| `spec.image.source.oci` | A containerDisk registry reference (`quay.io/containerdisks/fedora:40`) | `kairon-node` pulls it by manifest digest into the same cache -- see [OCI containerDisks](#oci-containerdisks) |
| `spec.image.source.format` | `qcow2`/`raw` (default, booted as downloaded) or `ova`, `vmdk`, `vhd`, `vhdx` | Converted through FluxVM's image import before boot -- see [Converting and repairing](#converting-and-repairing) |
| `spec.image.source.repair` | Offline virtio repair for VMs from VMware or another hypervisor | Same |

`spec.image.source` and `spec.volumes` are mutually exclusive with each
other in practice (a Machine only has one boot disk); when `spec.volumes` is
set it still takes priority, exactly as it already does over
`spec.image.path` -- see [`machine-storage.md`](machine-storage.md).

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web-1
spec:
  image:
    source:
      httpURL: https://cloud-images.example.com/noble-server-cloudimg-amd64.img
    digest: sha256:8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa
  resources: {cpu: "2", memory: "4Gi"}
  runtime: {backend: qemu}
  powerState: Running
```

`spec.image.digest` is **required** whenever `spec.image.source` is set --
it's the cache key: without it, two Machines pointing at the same (mutable)
URL would have no way to know whether they mean the same bytes. Format is
`sha256:<64 hex characters>`, the same convention Kairon already uses for
`spec.image.digest` on a plain `spec.image.path` Machine.

## Setup

Set `node.imageCacheDir` in the Helm chart (empty/off by default):

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set node.imageCacheDir=/var/lib/fluxvm/images/cache
```

This mounts a writable `hostPath` volume into `kairon-node`'s container at
exactly that path (unlike `--image-root`, which only ever validates a path
*string* -- `kairon-node` itself never reads or writes an operator-placed
image file today). The downloaded bytes land at this same absolute path on
the host, which is what makes them visible to FluxVM (a separate process on
the same host) once `spec.image.path` is set to the cache path.

## Self-signed image servers

`source.insecureSkipTLSVerify: true` downloads `httpURL` without verifying
the server certificate, for in-cluster image stores with a self-signed
certificate (Veyron's upload store, for example). The sha256 digest is still
checked before the file enters the cache, so the bytes can't be swapped;
only the transfer's confidentiality is lost. Leave it off for anything on a
public network.

Named, versioned images (`spec.image.imageRef`) live in the
[`MachineImage`](machine-images.md) catalog.

## How it works

On the first reconcile of a `spec.image.source` Machine, `kairon-node`:

1. Streams the URL into a temp file under `<imageCacheDir>/sha256/`, hashing
   as it downloads.
2. Verifies the result against `spec.image.digest` -- a mismatch deletes the
   temp file and the Machine's reconcile fails with a clear error, retried
   next tick.
3. Atomically renames the verified file to `<imageCacheDir>/sha256/<hex digest>`.
4. Sets `spec.image.path` to that cache path in memory for the rest of this
   reconcile -- `resolveBootDiskPath`/FluxVM's own `CreateWithVFIO` see an
   ordinary path, exactly as if an operator had placed it there.

A second Machine (or a later reconcile tick of the same Machine) naming the
same digest skips straight to step 4 -- no re-download. This is the whole
answer to "50 Machines booting the same golden image": the cache path is
content-addressed and immutable once written, so it's shared for free.

## Converting and repairing

With `format: ova` (or `vmdk`, `vhd`, `vhdx`) or `repair: true`, kairon-node
downloads and verifies the file as above, then calls FluxVM's
`POST /v1/images/import` with the cached path. FluxVM converts it to raw and,
with `repair`, fixes the guest offline: VMware tools disabled, virtio modules
added to the initramfs and the initramfs rebuilt, `/dev/sdX` moved to
`/dev/vdX` in fstab and grub, persistent NIC rules dropped and a DHCP fallback
added. A Windows guest gets the virtio-win drivers (viostor, vioscsi,
NetKVM, vioserial) injected instead, when FluxVM has `virtio_win_dir` set
(see FluxVM's `docs/import-vmware.md`). The resulting disk paths are
recorded in `<imageCacheDir>/imported/kairon-<digest prefix>[-repaired].json`,
so the import runs once per digest per node.

Every disk of a multi-disk OVA is attached. The first is the boot disk; each
further disk N becomes data disk `import-diskN` (SCSI serial `import-diskN`),
created by FluxVM as a per-Machine qcow2 overlay on the imported file before
the first boot. Machines booted from the same import never share writes.
`spec.disks` names may not start with `import-disk`.

FluxVM reads the file from the same path kairon-node downloaded it to, so
`imageCacheDir` must be the same host directory for both. The chart mounts
`node.imageCacheDir` from the host at the same path, so this holds whenever
it is set. `kaironctl import ova` builds such a Machine from an
OVA; see [migrate-from-vmware.md](migrate-from-vmware.md).

```yaml
spec:
  image:
    source:
      httpURL: https://files.example.com/web01.ova
      format: ova
      repair: true
    digest: sha256:...
```

## OCI containerDisks

`spec.image.source.oci` boots the same containerDisk images KubeVirt uses:
a container image whose layers hold the disk under `/disk/` (for example
`quay.io/containerdisks/fedora`). No container runtime is involved;
kairon-node speaks the registry v2 API directly.

```yaml
spec:
  image:
    source:
      oci: quay.io/containerdisks/fedora:40
    digest: sha256:<manifest or index digest>
```

- `spec.image.digest` is the **manifest (or multi-arch index) digest**,
  for example from `crane digest quay.io/containerdisks/fedora:40` or
  `skopeo inspect`. The pull is always by that digest; the tag is a label.
  A reference that carries its own `@sha256:` must match it.
- For an index, the `linux/<node arch>` manifest is used. Layers are
  searched top-down for the first regular file under `disk/`. Every
  manifest and layer is checked against its digest while streaming, and
  nothing reaches the cache unless all checks pass.
- The disk lands at `<imageCacheDir>/oci/sha256/<manifest digest>` and is
  shared by every Machine on the node naming that digest. `format` and
  `repair` work as for `httpURL`.
- HTTPS only, anonymous pulls only (including the anonymous bearer-token
  flow Docker Hub, quay.io and ghcr.io use for public images). gzip and
  uncompressed layers; zstd layers are rejected.

## Uploading images

For an image that isn't already on a web server or registry, upload it to
kairon-ui's image store and boot it through the same `httpURL` path:

The routes are `GET /api/v1/images` (any session), `PUT` and `DELETE
/api/v1/images/{name}` (administrator only) and `GET /images/sha256/{digest}`,
which `kairon-node` uses to download the image and is unauthenticated: it
verifies the bytes against `spec.image.digest`, and anyone who can reach
kairon-ui and knows a digest can fetch that image. See the
[kairon-ui API reference](kairon-ui-api.md).

```bash
helm upgrade kairon ./charts/kairon -n kairon-system --reuse-values \
  --set ui.enabled=true --set ui.imageStore.enabled=true \
  --set node.imageCacheDir=/var/lib/fluxvm/images/cache

export KAIRON_UI_URL=https://kairon-ui.example.com KAIRON_UI_TOKEN=<admin session token>
kaironctl image upload ./noble-server-cloudimg-amd64.qcow2 --name ubuntu-24.04
```

```yaml
image:
  source:
    httpURL: https://kairon-ui.kairon-system.svc:18082/images/sha256/8f43...
    format: qcow2
    insecureSkipTLSVerify: true   # only when kairon-ui serves a self-signed certificate
  digest: sha256:8f43...
```

- kaironctl hashes the file first and sends the digest; kairon-ui rejects
  the upload if the bytes it received hash differently. Blobs are stored
  once per digest on a PVC (`ui.imageStore.size`, `storageClassName`, or
  `existingClaim`); names are pointers, and deleting the last name pointing
  at a blob deletes it.
- Upload and delete need an admin account; listing needs any login.
- `GET /images/sha256/<digest>` is **unauthenticated** so kairon-node can
  fetch without kairon-ui credentials (it verifies the digest itself).
  Anyone who can reach kairon-ui and knows a digest can download that
  image; keep secrets out of uploaded images or restrict network access.
- `ui.imageStore.publicURL` sets the URL nodes download from. The default
  `https://kairon-ui.<namespace>.svc:<port>` needs cluster DNS from the node
  host (kairon-node runs with `hostNetwork`). kairon-ui serves HTTPS, usually
  with a self-signed certificate, so `kaironctl image upload` prints
  `insecureSkipTLSVerify: true` in the snippet whenever the dashboard's
  certificate is self-signed: the transfer is still encrypted and the pinned
  `sha256` still guarantees the bytes, only the server's identity is not
  verified. Give the dashboard a certificate your nodes trust
  (`ui.tls.secretName`) and the flag disappears.
- One upload is capped at `ui.imageStore.maxBytes` (64Gi by default).

## Real limits today (v1 of this feature)

- **Imported data disks live and die with the runtime.** Like the boot
  overlay, `import-diskN` loses its changes when the Machine is stopped
  (`spec.powerState: Stopped`) and started again, because the runtime and
  its overlays are recreated. Use `spec.disks` with a PVC for data that must
  survive that.
- **No private sources.** No credentials-Secret mechanism for either
  `httpURL` or `oci` (a URL with embedded credentials works the same as any
  other `http.Client` request; private registries are refused).
- **No cache eviction.** `<imageCacheDir>` only ever grows; freeing disk
  space is an operator concern (`du`/manual cleanup), not something Kairon
  automates.
- **Single-node cache, not cluster-wide.** Two Machines scheduled to
  *different* nodes each download their own copy into their own node's
  `imageCacheDir` -- there's no cluster-wide image registry/replication.
- **Validated at admission time only when `webhook.enabled`.** With the
  admission webhook off (the default), a malformed `spec.image.source` (no
  digest, non-`http(s)` URL) still surfaces -- just later, as a stuck
  Machine reconcile error instead of an immediate `kubectl apply` rejection.
