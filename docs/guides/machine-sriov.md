# User guide: SR-IOV NIC passthrough via DRA/VFIO reuse

A dedicated, high-performance network interface passed straight through
to a Machine's guest -- reusing the exact same mechanism GPU passthrough
already uses, not a new subsystem.

## Why this isn't "SR-IOV via Multus"

KubeVirt (and most of the Kubernetes SR-IOV ecosystem) gets multi-NIC/SR-IOV
support via Multus, a CNI meta-plugin that wires extra network interfaces
into a **Pod's** network namespace. Kairon Machines have no backing Pod at
all, so there's no attachment point for Multus to use -- that whole
integration path simply doesn't apply here. Multus stays non-primary.
The supported way onto the **cluster Cilium network** (identity and IPAM,
not a VF) is `spec.network.ciliumAttach` plus a `CiliumExternalWorkload`,
documented in [network-fabric.md](../network-fabric.md). That is a
different path from the PCI passthrough below.

What Kairon already has instead: real GPU/VFIO PCI passthrough via
Kubernetes DRA (`spec.deviceClaims[]` -> a `ResourceClaim` -> a PCI BDF ->
FluxVM's own `vfio_devices`). That path is **deliberately
device-type-agnostic** -- it never inspects what kind of PCI device a BDF
names. An SR-IOV Virtual Function (VF) is just another PCI device once
it's created and bound to `vfio-pci` -- so it passes through via this
exact same mechanism, with **no new Kairon code required**.

## Prerequisites (host/operator setup, same posture as GPU passthrough)

Kairon manages neither GPU driver binding nor SR-IOV VF lifecycle itself
-- both are host/operator setup, out of band, the same way an
administrator already prepares a GPU for passthrough today:

1. Enable IOMMU on the host (`intel_iommu=on`/`amd_iommu=on` kernel
   parameters).
2. Create VFs on the physical function: `echo N > /sys/class/net/<pf>/device/sriov_numvfs`.
3. Bind the VF you intend to pass through to `vfio-pci` instead of its
   normal driver (`echo <VF's PCI address> > /sys/bus/pci/drivers/vfio-pci/bind`,
   after unbinding it from its default driver).
4. Add the VF's normalized BDF (`0000:xx:yy.z`) to the node's
   `KAIRON_VFIO_ALLOWLIST` -- **fail-closed**: an empty allowlist denies
   all VFIO passthrough, GPU or NIC alike.

## Exposing the VF to a Machine

Same as GPU passthrough: reference a `ResourceClaim` from `spec.deviceClaims[]`.

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: high-throughput-vm}
spec:
  deviceClaims:
    - name: nic-vf-0
  # ... unchanged otherwise
```

The `ResourceClaim` needs to resolve to the VF's own PCI BDF, either way:

- **A real DRA driver** that publishes `ResourceSlice`s for SR-IOV VFs, if
  your cluster has one installed. The Kubernetes SR-IOV DRA ecosystem is
  real but meaningfully less mature than GPU DRA drivers as of this
  writing -- confirm one actually exists and is stable for your hardware
  before relying on it.
- **The existing manual fallback**, already implemented and requiring no
  new code: annotate the `ResourceClaim` with
  `kairon.zyvor.dev/vfio-bdf: "0000:xx:yy.z"` directly. This is the same
  fallback GPU passthrough already uses when there's no DRA driver for
  the device.

A Machine can combine a GPU claim and a NIC-VF claim in the same
`spec.deviceClaims[]` list -- FluxVM's own `CreateVmRequest.vfio_devices`
already loops over multiple BDFs with no per-device-type logic.

## Guest side

The guest needs its own driver for the passed-through NIC hardware (e.g.
`ixgbevf`/`mlx5_core`, depending on the physical NIC) -- this is no
different from passing a VF through to any other VM, Kairon or otherwise.

## `status.guestIP` and the passthrough NIC

A passthrough VF keeps its own real hardware MAC address (never one
FluxVM assigns, unlike the primary virtio-net NIC) and can carry its own
DHCP lease. `status.guestIP` now prefers the interface matching FluxVM's
own known primary-NIC MAC when one is known, so a VF's own address won't
nondeterministically clobber the primary management address just because
the guest happened to enumerate it first. If FluxVM has no primary-NIC
MAC on record for a Machine (e.g. `spec.network.mode: none`), address
selection falls back to the original first-IPv4-found behavior.

## Real limits today (first cut)

- **QEMU backend only.** `vfio_devices` is a QEMU-only FluxVM field per
  its own doc comment -- no attempt made here to extend Cloud
  Hypervisor/Firecracker support, since that's FluxVM-repo hypervisor
  work, out of scope for this guide.
- **No VF lifecycle management.** Kairon doesn't create, destroy, or
  rebind VFs itself -- entirely an operator/host responsibility, same as
  GPU passthrough.
- **No VFIO-through-live-migration**, the same tracked, named upstream
  gap `ROADMAP.md` already documents for GPU passthrough -- a Machine
  with `spec.deviceClaims` is refused live migration outright today.
- **No dashboard support** for device claims of any kind (GPU or NIC) --
  `spec.deviceClaims` is spec-only, set via `kubectl apply`/`kaironctl create`.
