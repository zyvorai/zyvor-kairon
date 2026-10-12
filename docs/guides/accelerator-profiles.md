# User guide: accelerator profiles (`MachineAcceleratorProfile`, experimental)

A `MachineAcceleratorProfile` is a named accelerator shape that a cluster
admin defines once. It says how many GPUs or accelerators, which vendor or
vGPU profile, how much memory each needs, whether they must share a PCIe
root, and whether an RDMA NIC goes with them. Machines then ask for it by
name with `spec.accelerators`, instead of spelling out `spec.devices`.

The profile is a template. Before the Machine is scheduled,
kairon-controller copies it into the Machine's `spec.devices`. Placement
then works exactly as for hand-written devices; see
[topology-aware placement](machine-topology.md) and
[vGPU profiles](gpu-vgpu.md). Editing or deleting the profile later does
not change Machines already expanded from it.

## Example

[`examples/acceleratorprofile.yaml`](https://github.com/zyvorai/kairon/blob/main/examples/acceleratorprofile.yaml):

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineAcceleratorProfile
metadata:
  name: a100-pair-rdma
spec:
  class: gpu
  vendor: nvidia
  count: 2
  topology:
    p2pRequired: true
    sameNuma: true
  memory:
    minimumPerDevice: 40Gi
  nic:
    vendor: mellanox
    virtualFunction: true
  migration:
    required: false
---
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: trainer
spec:
  image: {path: /var/lib/fluxvm/images/ubuntu-noble-base.img}
  resources: {cpu: "16", memory: 64Gi, cpuPinning: true}
  topology: {policy: single-numa-node}
  runtime: {backend: qemu}
  accelerators:
    - name: train
      profile: a100-pair-rdma
```

## Fields

| Field | Meaning |
|---|---|
| `class` | `gpu` (default) or `accelerator` |
| `vendor`, `model` | PCI vendor ID (`10de`) or `nvidia`, `amd`, `intel`; PCI device ID |
| `profile` | vGPU (mediated device) instances of this type instead of whole devices: an mdev type ID or the vendor's profile name |
| `count` | Devices or instances, 1-16 (default 1) |
| `topology.p2pRequired` | Every device under one PCIe root. Instances on a vGPU parent that is not a PCI device (such as `mbochs`) must share that parent. |
| `topology.sameNuma` | The devices on one NUMA node. With a Machine `spec.topology.policy` other than `none`, that is also the node of the Machine's CPUs. |
| `memory.minimumPerDevice` | Least GPU memory per device, or framebuffer per vGPU instance (class `gpu` only) |
| `nic` | `vendor`, `model`, `virtualFunction`, `count`: NICs under the same PCIe root as the accelerators, for GPUDirect RDMA |
| `migration.required` | Asks for live migration. Passthrough and vGPU devices cannot live-migrate yet, so such a profile is not `Ready` and Machines naming it wait. |

## What the Machine gets

Each `spec.accelerators` entry `{name, profile}` becomes:

- a `spec.devices` entry named `name` with the profile's class, vendor,
  model, profile, count and `minMemory`;
- with `nic`, a second entry named `name-nic`;
- the `sameNuma` alignment on both when `topology.sameNuma` is set, and
  `samePcieRoot` on both when `topology.p2pRequired` is set or there is a
  `nic`.

kairon-controller records what it expanded in the annotation
`kairon.zyvor.dev/accelerators`, as `name=profile@generation` pairs (for
example `train=a100-pair-rdma@3`). The expansion and the annotation land in
one patch, before scheduling.

`samePcieRoot` applies to the whole Machine: all its `samePcieRoot`
devices share one root. Two `p2pRequired` accelerators on one Machine
therefore end up under the same root.

## Rules

- `spec.accelerators` takes at most 4 entries. Names are DNS labels of at
  most 28 characters and must not clash with `spec.devices` names
  (including the `-nic` suffix). After expansion, the Machine can have at
  most 8 `spec.devices` entries.
- Accelerators need the qemu backend and the scheduler: a Machine created
  with `spec.nodeName` set or with the annotation already set is refused
  by the admission webhook.
- `spec.accelerators` cannot change after the Machine is scheduled, like
  `spec.devices`.
- Machines with accelerators cannot use `spec.resources.elasticMemory`,
  because VFIO pins guest memory.

## When a Machine waits

While a profile is missing or not usable, the Machine stays `Pending`
with condition `Scheduled=False`, reason `AcceleratorProfileUnavailable`:

```text
waiting for MachineAcceleratorProfile "a100-pair-rdma"
MachineAcceleratorProfile "live" cannot be used: migration.required: passthrough and vGPU devices cannot live-migrate yet
```

Once expanded, a Machine that no node can place gets `TopologyUnsatisfiable`
as usual, for example `needs 1 gpu profile mbochs-large with 1Gi, 0 free`.

## Profile status

kairon-controller writes each profile's status every reconcile:

- **`Ready`**: `True` (`Valid`), or `False` with `Invalid` or
  `LiveMigrationUnsupported`.
- **`Available`**: `True` (`Fits`) when at least one more Machine with the
  profile fits on some node, otherwise `False` (`NoCapacity`) with the first
  node's reason.
- **`nodes[]`**: for each schedulable node with a `NodeTopology`, how many
  more Machines with this profile its free devices could take (`fits`, at
  most 16), and why none fit when `fits` is 0.
- **`capacity`**: the sum of `nodes[].fits`.
- **`machines`**: how many Machines were expanded from the profile.

Capacity counts devices only. It ignores CPUs, memory, quotas and the
Machine's own topology policy, so a Machine can still wait for CPUs on a
node with free GPUs.

```bash
kaironctl get acceleratorprofiles
kaironctl describe acceleratorprofile a100-pair-rdma
```

```text
NAME             CLASS  COUNT  PROFILE  READY  CAPACITY  MACHINES
a100-pair-rdma   gpu    2      -        True   3         1
```

Metrics (controller):

| Metric | Labels | Meaning |
|---|---|---|
| `kairon_accelerator_profile_capacity` | `profile`, `node` | `status.nodes[].fits` |
| `kairon_accelerator_profile_machines` | `profile` | `status.machines` |

## Setup

The CRD ships in `deploy/crd.yaml` and the chart. kairon-controller needs
`get`, `list`, `watch` and `patch` on `machineacceleratorprofiles` and
`machineacceleratorprofiles/status`, which the chart grants. Profiles are
cluster-scoped, so create them with cluster-admin rights and let tenants
read them. Host setup for vGPU profiles is in [vGPU profiles](gpu-vgpu.md#host-setup).
On nodes that also run KubeVirt, turn off its mdev management there: its
virt-handler removes instances it did not create, and the profile's
capacity then counts them as still pending.

## Real limits today

- **P2P is approximated by the PCIe root.** Kairon does not detect NVLink,
  PCIe switches below the root, or ACS settings. Two GPUs under one root
  can still be unable to talk peer to peer.
- **No driver or firmware matching.** A profile cannot ask for a driver or
  firmware version.
- **No live migration** of passthrough or vGPU devices, so
  `migration.required: true` only produces a profile that is not Ready.
- **Lab coverage.** The lab ran the profile flow with the kernel's `mbochs`
  sample vGPU driver on one single-NUMA node. Whole GPUs with
  `p2pRequired`, an RDMA NIC next to GPUs, `sameNuma` across several NUMA
  nodes and NVIDIA profiles are unit-tested only; see
  [`COMPATIBILITY.md`](../COMPATIBILITY.md#accelerator-profiles).
