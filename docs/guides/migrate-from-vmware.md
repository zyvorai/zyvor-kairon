# Migrating VMs from VMware

Export a VM from vSphere as an OVA, put it on any http(s) server the nodes
can reach, and create a Machine from it with one command. kairon-node
downloads it, FluxVM converts the VMDK to raw and repairs the guest so it
boots on virtio, and the Machine starts.

```bash
# vSphere: export the powered-off VM (ovftool, govc, or the UI)
govc export.ovf -vm web01 ./exports
tar -C exports/web01 -cf web01.ova web01.ovf web01-disk1.vmdk
# publish it (any static file server works)
aws s3 cp web01.ova s3://exports/ && URL=$(aws s3 presign s3://exports/web01.ova --expires-in 86400)

kaironctl import ova "$URL" --name web01 --network tap
kaironctl get machines
```

`kaironctl import ova` streams the OVA once without saving it, hashes it for
`spec.image.digest` and reads vCPUs and memory from the OVF. It creates:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web01
  labels: {kairon.zyvor.dev/imported-from: ova}
spec:
  image:
    source: {httpURL: "https://...", format: ova, repair: true}
    digest: sha256:...
  resources: {cpu: "4", memory: 8192Mi}
  runtime: {backend: qemu}
  network: {mode: tap}
  powerState: Running
```

For a local file pass the URL it will be served from:
`kaironctl import ova ./web01.ova --url https://files.example.com/web01.ova`.
`--dry-run` prints the Machine instead of creating it. `--cpu`, `--memory`
and the other `kaironctl create` flags override the OVF. With `--sha256`,
`--cpu` and `--memory` all set, kaironctl doesn't read the OVA at all.

## OVAs with several VMs

An OVF with a `VirtualSystemCollection` (a vApp) or several `VirtualSystem`
entries is handled too. `kaironctl import ova` prints one Machine per VM as a
multi-document YAML stream on stdout and never creates anything, so review it
and pipe it to `kaironctl apply -f -`. Names come from the VM names, made
DNS-safe and unique (`Web 01` and `web-01` become `web-01` and `web-01-2`);
`--name` is rejected. vCPUs and memory come from each VM's own hardware
section and `--cpu`/`--memory` override all of them. Every Machine points at
the same OVA and digest. Anything not converted is printed as a `warning:`
line on stderr: startup ordering, vApp properties, shared disks, VMs with more
than one disk, UEFI guests, and the fact that kairon-node does not pick a
per-VM disk out of the OVA. This path has only been unit-tested against
hand-written OVF fixtures; it has not been run against a real VMware export or
booted on a node.

## What the repair changes

| Area | Change |
| --- | --- |
| VMware tools | `vmtoolsd`, `open-vm-tools`, `vmware-tools`, `vgauth` units disabled |
| Boot | virtio drivers added to dracut or initramfs-tools; initramfs rebuilt for the newest kernel |
| Disks | `/dev/sdX` and `/dev/hdX` in `/etc/fstab`, `/etc/default/grub` and `grub.cfg` become `/dev/vdX` |
| Network | `70-persistent-net.rules` removed; DHCP fallback for `en*` (netplan or NetworkManager) |

The repair report (actions and warnings) is in the kairon-node log for the
Machine and in `<imageCacheDir>/imported/*.json` on the node.

Tested on the lab: a stream-optimized VMDK OVA of Ubuntu 22.04 imports in
about a minute and boots on virtio-blk and virtio-net to a login prompt.

## Checklist

1. **Static IPs.** VMware NIC names (`ens192`, `ens160`) don't exist on
   virtio. The repair warns about configs that use them and adds a DHCP
   fallback. Re-apply a static address with cloud-init or the guest's own
   tooling, or use a `MachineNetworkPolicy` and DHCP.
2. **UEFI guests.** If the OVF says `firmware=efi`, kaironctl prints a note.
   Run the Machine on a node whose FluxVM config sets `qemu_ovmf_code`
   (use a node selector or label).
3. **Multiple disks.** Only the boot disk is attached. The other converted
   disks are on the node under FluxVM's `images/imported/` directory; copy
   them to PVCs and add them as `spec.volumes`.
4. **Windows.** Install the virtio-win drivers (viostor, vioscsi, netkvm) in
   the VM before exporting. Offline driver injection is not done.
5. **Node cache.** Set the chart's `node.imageCacheDir` (off by default).
   The chart mounts that host directory into kairon-node at the same path,
   which is what FluxVM needs to read the downloaded OVA.

## Compared with KubeVirt

KubeVirt's import path is CDI (a DataVolume that converts the disk) plus
virt-v2v or the Forklift/MTV operator for the guest repair, each with its own
CRDs and pods. In Kairon the download, conversion and repair are part of the
Machine's first reconcile on the node, driven by two fields on
`spec.image.source`.
