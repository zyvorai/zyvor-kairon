# User guide: the image catalog (`MachineImage`)

A `MachineImage` is a cluster-scoped, named and versioned image: a boot disk
(`kind: disk`) or install media (`kind: iso`). Machines reference it by
name instead of repeating a URL and a digest. It replaces KubeVirt's CDI
`DataSource` + golden PVC without a PVC per image: every node caches the
bytes once by digest and gives each Machine its own overlay.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineImage
metadata:
  name: windows-server-2022-v3
spec:
  displayName: Windows Server 2022 (sysprepped, May patches)
  family: windows-server-2022
  version: "3"
  os: windows
  kind: disk
  source: {httpURL: "https://images.example/ws2022-v3.qcow2", format: qcow2}
  digest: sha256:<64 hex>
  defaults: {cpu: "4", memory: 8Gi, diskSize: 80Gi}
```

A complete example with a disk image and an install ISO is in [`examples/machineimage.yaml`](https://github.com/zyvorai/zyvor-kairon/blob/main/examples/machineimage.yaml); its URLs and digests are placeholders.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: win01, namespace: prod}
spec:
  image: {imageRef: windows-server-2022-v3}
  security: {secureBoot: true, tpm: true}
```

## How a reference resolves

`kairon-controller` resolves `spec.image.imageRef` (and
`spec.cdroms[].imageRef`) **once**, before the Machine is scheduled:

1. It copies the image's `source` and `digest` into the Machine
   (`spec.image.source`, `spec.image.digest`). From then on the Machine is
   pinned to those bytes.
2. It fills `spec.image.diskSize` and an empty `spec.resources` (only when
   `spec.instanceTypeName` is unset) from `spec.defaults`. Values the
   Machine already sets win.
3. Until every reference resolves, the Machine stays `Pending` with a reason
   (`waiting for MachineImage "x"`, `MachineImage "x" is kind iso, need
   disk`, or a validation error) and no node sees it.

Because the bytes are pinned, publishing a new version (a new
`MachineImage`, or editing this one) changes only Machines created
afterwards. Deleting a `MachineImage` never affects Machines already using
it, as long as nodes keep the cached file.

## Validation

- `digest` is a sha256 digest of the downloaded file (or the OCI manifest),
  same as `spec.image.source`.
- `kind: iso` must be an `httpURL` to raw bytes: no `oci`, no `format`
  other than `raw`, no `repair`.
- `disk` images accept everything `spec.image.source` accepts, including
  `format: vmdk|vhdx|ova` conversion and `repair`.
- `insecureSkipTLSVerify: true` downloads from a self-signed server; the
  digest check still guarantees the bytes.

- Downloads share `kairon-node`'s size cap (`-image-max-download-bytes`, 256 GiB by default), and a qcow2 `disk` whose
  header names a backing file is refused on the node; flatten it with `qemu-img convert -O qcow2 in.qcow2 out.qcow2`
  (see [image import](machine-image-import.md#size-limit)).

Images are checked when a Machine references them, not at `kubectl apply`.

## Versions

`family` and `version` are labels for clients (Veyron's catalog groups by
family and offers the newest version); Kairon doesn't interpret them. One
`MachineImage` per version keeps old versions referenceable. Set
`deprecated: true` to hide a version from pickers without breaking anyone.

## RBAC

`kairon-controller` and `kairon-ui` read `machineimages`. Whoever publishes
images (a CI pipeline, Veyron) needs `create`/`update`/`delete` on
`machineimages.kairon.zyvor.dev`.

See also: [`machine-install-media.md`](machine-install-media.md) for ISO
installs, [`machine-image-import.md`](machine-image-import.md) for how the
node cache works.
