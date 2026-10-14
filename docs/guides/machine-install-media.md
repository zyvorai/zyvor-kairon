# User guide: installing from an ISO (`spec.cdroms`, `spec.image.blank`)

Boot a Machine from installer media onto an empty disk: Windows Setup, a
Linux installer, or any appliance ISO. No KubeVirt, no CDI, no PVC upload.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: win-builder, namespace: images}
spec:
  image: {blank: true, diskSize: 80Gi}
  cdroms:
  - {name: install, imageRef: windows-server-2022-iso}
  - {name: virtio, imageRef: virtio-win}
  resources: {cpu: "4", memory: 8Gi}
  security: {secureBoot: true, tpm: true}
  runtime: {backend: qemu}
```

## What happens

- `spec.image.blank` boots an empty root disk of `spec.image.diskSize`.
  kairon-node overlays a shared 1 MiB empty base in its image cache and
  FluxVM grows the overlay, so the disk costs nothing until the guest
  writes to it.
- Each `spec.cdroms` entry is an ISO attached **read-only** as a SATA CD-ROM
  on the q35 machine's built-in AHCI controller. Windows Setup sees it with
  no extra drivers. At most 4.
- No boot order is forced. Firmware skips the blank disk and boots the first
  CD-ROM. After installation the OS writes its own boot entry, so reboots
  come up from the disk.
- ISOs come from a `MachineImage` of `kind: iso` (`imageRef`) or directly
  from `source.httpURL` + `digest`. They land in the same digest-keyed node
  cache as boot images, so 50 installs from one ISO download it once.

## Windows

Windows Setup can't see a virtio disk until it loads the driver. Attach the
VirtIO driver ISO as a second CD-ROM (`virtio` above) and choose **Load
driver → `viostor\2k22\amd64`** when Setup shows no disk. Install the
VirtIO drivers and the QEMU guest agent from the same CD before you seal
the image.

UEFI ISOs show "Press any key to boot from CD or DVD" for a few seconds.
Open the console (VNC) right after creating the Machine to catch it.

## Turning an install into a golden image

1. Install, update, then `Sysprep.exe /generalize /shutdown /oobe /mode:vm`.
2. Copy the root disk off the node (a [`MachineBackup`](machine-backup.md),
   or the FluxVM overlay flattened with `qemu-img convert`), serve it over
   HTTP(S) and publish it as a `MachineImage` of `kind: disk`
   ([`machine-images.md`](machine-images.md)).
3. Create Machines with `spec.image.imageRef`.

## Ejecting media after the install

FluxVM won't live-migrate a VM that still has media in a CD-ROM, because the ISO is a file on the
node. Once the OS is installed, delete the entry from `spec.cdroms`:

```bash
# eject both ISOs from a running Machine
kubectl patch machine win2022 --type=json -p='[{"op":"remove","path":"/spec/cdroms"}]'
# or eject one (here the first entry, "install")
kubectl patch machine win2022 --type=json -p='[{"op":"remove","path":"/spec/cdroms/0"}]'
```

kairon-node notices a drive that still holds media but is no longer listed, and calls FluxVM's
`POST /v1/vms/{id}/cdroms/{name}/eject`. If the VM is running, the medium is removed live; the guest
sees an empty drive and nothing is unplugged. FluxVM records the drive as empty, so restarts and
migration targets bring it up without the ISO and drive letters stay put. Then the Machine can
live-migrate.

Once the Machine has a runtime, entries can only be **removed**: FluxVM can't insert media into an
existing VM. Adding or changing an entry is denied by the admission webhook; without the webhook,
kairon-node simply doesn't attach the new entry. A Machine held Stopped (no runtime) may change
`spec.cdroms` freely, and the drives are created when it next starts:

```bash
kaironctl stop win2022
kaironctl cdrom insert win2022 virtio --image virtio-win
kaironctl start win2022
kaironctl cdrom eject win2022 virtio   # live
```

A Stopped Machine keeps only disks on `spec.volumes`; one booting from `spec.image` starts again
from the image.

## Persistent installs on a volume

With `spec.volumes[0]` set, `blank: true` creates the empty disk **inside
the volume** (`disk.img`, sparse, `diskSize` bytes) instead of in the node
cache. See [`machine-storage.md`](machine-storage.md#seeding-a-boot-volume).

## Limits

- QEMU backend only.
- Requires a FluxVM with `CreateVmRequest.cdroms` (fluxvm PR #142 or later).
  An older FluxVM ignores the field and boots the blank disk with no media.
- After creation, `spec.cdroms` entries can only be removed, which ejects them (see above). Ejecting
  needs FluxVM with `POST /v1/vms/{id}/cdroms/{name}/eject` (fluxvm PR #149 or later).
- FluxVM refuses to live-migrate a VM while a CD-ROM still holds media. Eject it first.
