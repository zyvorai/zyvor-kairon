# Leaving Nutanix for Kairon

Transiva exports a VM out of Nutanix through the Prism v4 API. h2kvm converts it and can hand it to Kairon through Veyron (`--deploy-kairon`).

> **Status: the Kairon hand-off was not run in the Nutanix test.** What was run end to end is Nutanix export, h2kvm conversion, and a boot on libvirt/KVM and on KubeVirt, against Chimera's Nutanix simulator. Treat the Kairon step below as the documented h2kvm hand-off, not as something verified with a Nutanix source.

## The path

```text
Nutanix (Prism v4)  ->  hyperexport  ->  h2kvm  ->  Veyron  ->  Kairon Machine
```

1. **Export** with Transiva: `hyperexport --provider nutanix -vm <name> -resolve-containers -mounts <container>:<nfs-path>`. This writes a qcow2 and `artifact-manifest.json`; the source VM is untouched.
2. **Convert and land** with h2kvm using `--deploy-kairon`. h2kvm asks Veyron to create a Kairon Machine that points at the converted disk, pinned by sha256. See the [h2kvm README](https://github.com/zyvorai/zyvor-h2kvm#readme) for the flags.

## What was measured elsewhere in the path

Export, convert and boots on KVM and KubeVirt passed in an unattended run on an Ubuntu 24.04 cloud image: 7 of 7 stages, 747 s ([case study](https://github.com/zyvorai/zyvor-transiva/blob/main/docs/case-study-nutanix-exit-linux.md)).

## Not covered

- A Kairon landing from a Nutanix export.
- A real Nutanix cluster, Windows, UEFI, multi-disk or large VMs.
- Boot repair on a guest that needs it (the Ubuntu test image needed only h2kvm's DHCP network fallback).

Related: [runbook-vm-export.md](../runbook-vm-export.md), [WHAT_SHIPS.md](../WHAT_SHIPS.md).
