# User guide: topology-aware placement (`spec.topology`, `spec.devices`)

Places a Machine's vCPUs, memory and PCI devices by host locality: NUMA
node, PCIe root and SR-IOV parent. The scheduler picks the node and the
exact CPUs, NUMA memory and device BDFs together. FluxVM then binds each
guest NUMA node's memory to its host node and pins its vCPU threads.

Experimental: see [Real limits today](#real-limits-today) and the lab rows
in [`COMPATIBILITY.md`](../COMPATIBILITY.md#topology-aware-placement).

## How it fits together

1. **FluxVM** reads sysfs and serves `GET /v1/host/topology`: NUMA nodes
   (CPUs, memory, hugepage pools, distances) and PCI devices (class,
   vendor/device ID, NUMA node, PCIe root, IOMMU group, SR-IOV PF/VFs,
   netdevs, link speed, NVMe size, GPU memory).
2. **kairon-node** publishes it as a cluster-scoped `NodeTopology` named
   after the node. It checks once a minute and writes when the graph
   changes, or every ten minutes. A device is `allocatable` only if it is
   in `KAIRON_VFIO_ALLOWLIST`.
3. **kairon-controller** filters and scores nodes with the topology
   allocator. It writes `spec.nodeName`, `spec.resources.allocatedCpuSet`
   and `spec.topologyAllocation` in one patch.
4. **kairon-node** creates the VM with FluxVM's `numa` request field built
   from `spec.topologyAllocation`. It binds the allocated devices to
   `vfio-pci` (`POST /v1/host/pci/bind`) and passes them through. It then
   records what FluxVM actually applied in `status.appliedTopology`.

```bash
kaironctl get nodetopologies
kaironctl node topology worker-1
```

```text
worker-1 (numa binding: yes)
├── numa0  cpus 0-23  mem 192000MiB (150311MiB free)  hugepages 1Gi 40/64 free
│   ├── gpu 0000:3b:00.0 10de:2330 root pci0000:3a 80GiB -> ml/trainer
│   ├── nic 0000:3c:00.2 15b3:101e root pci0000:3a vf-of 0000:3c:00.0 -> ml/trainer
│   └── machine ml/trainer cpus=2-17 mem=65536MiB
└── numa1  cpus 24-47  mem 192000MiB (190002MiB free)
    └── nvme 0000:d8:00.0 8086:0a54 root pci0000:d7 [not in VFIO allowlist]
```

The dashboard's **Nodes** page shows the same tree in the node inspector
(`GET /api/v1/nodes/{node}/topology`; the SDKs' `topology(node)`).

## Example

[`examples/topology-gpu-nic.yaml`](https://github.com/zyvorai/kairon/blob/main/examples/topology-gpu-nic.yaml):

```yaml
spec:
  resources:
    cpu: "16"
    memory: 64Gi
    cpuPinning: true
    hugepages: true
  topology:
    policy: single-numa-node
    hugepageSize: 1Gi
  devices:
    - name: gpu
      class: gpu
      vendor: nvidia
      minMemory: 40Gi
      alignment: [samePcieRoot]
    - name: nic
      class: nic
      vendor: mellanox
      virtualFunction: true
      alignment: [samePcieRoot]
  runtime: {backend: qemu}
```

## Policies

| `spec.topology.policy` | Meaning |
|---|---|
| `none` (default) | No NUMA placement. `spec.devices` are still allocated from `NodeTopology`, ignoring locality. |
| `best-effort` | Prefer one NUMA node, then two, then any. Never refuses a node over locality. A node with no `NodeTopology` stays eligible when the Machine has no devices. |
| `restricted` | The fewest NUMA nodes that fit (one or two); devices must be local to one of them. |
| `single-numa-node` | All vCPUs, memory and devices on one NUMA node, or the node is ineligible. |

Any policy other than `none` requires `spec.resources.cpuPinning: true`.
The vCPUs are pinned 1:1 to the allocated host CPUs, which are taken from
the node's `kairon.zyvor.dev/pinnable-cpus` label (see
[CPU pinning](machine-cpu-pinning.md)). `restricted` and `single-numa-node`
also need a node whose FluxVM reports `numaBinding: true`.

With several NUMA nodes, vCPUs are split evenly and memory proportionally,
rounded to `hugepageSize`. Guest node *i* gets the next block of vCPU
indexes, so the guest sees the same NUMA layout as the host placement.

## Devices

Each `spec.devices` entry asks for `count` (default 1) devices of a
`class` (`gpu`, `nic`, `nvme`, `accelerator`):

- **`vendor`** is a PCI vendor ID (`10de`) or `nvidia`, `amd`, `intel`,
  `mellanox`. **`model`** is a PCI device ID.
- **`minMemory`** is the least GPU memory per device.
- **`virtualFunction: true`** takes SR-IOV VFs only; otherwise whole
  devices (never VFs).
- **`alignment`**: `sameNuma` keeps the entry's devices on one NUMA node
  with the Machine's CPUs. `samePcieRoot` puts every `samePcieRoot` device
  of the Machine under one PCIe root, for GPU-to-NIC peer to peer.

A device is never handed out twice: devices in another Machine's
`spec.topologyAllocation`, or already in a FluxVM VM, are skipped.
`spec.devices` need the qemu backend. They live next to DRA
`spec.deviceClaims`, and the passed-through BDFs are merged.

## DRA device claims

With `restricted` or `single-numa-node`, an allocated `ResourceClaim` is a
hard constraint, not a hint. The Machine can only land on the claim's node,
and its CPUs go on the NUMA node of the claim's device (the BDF from the
claim's device result or `kairon.zyvor.dev/vfio-bdf`). While a claim has
no allocation the Machine stays Pending with reason `DRAAllocationPending`.
Under `none` and `best-effort`, DRA stays the small scoring hint described
in [placement](machine-placement.md).

## When it does not fit

The Machine stays `Pending` with condition `Scheduled=False`, reason
`TopologyUnsatisfiable`, and a message naming each NUMA node's shortfall:

```text
TopologyUnsatisfiable: no single NUMA node fits (numa0: 3 free pinnable CPUs, needs 6; numa1: 3 free pinnable CPUs, needs 6)
```

`kairon_topology_allocation_failures_total{reason}` counts these, and
`kairon_node_numa_free_cpus{node,numa}` shows pinnable CPUs left per NUMA
node.

## What was applied

`status.appliedTopology` reports FluxVM's `GET /v1/vms/{id}/numa`: each
guest node's host node, vCPUs, the host CPUs its vCPU threads may run on,
memory, and the memory policy QEMU reports. `message` explains any
difference from `spec.topologyAllocation`.

## Migration

- **Cold migration** re-runs the allocator on the target when the Machine
  is reassigned, so it gets fresh CPUs, NUMA memory and devices there.
- **Live migration** keeps the guest's NUMA shape: each guest node needs a
  free host NUMA node with as many CPUs and as much memory on the target.
  The source passes the target's binding to the receiver. If no shape
  fits, the migration is blocked with `TopologyShapeUnavailable` and the
  source is not touched. A Machine with `spec.devices` cannot live-migrate
  (VFIO); use cold migration.

## Real limits today

- **vCPU pinning is best-effort.** FluxVM sets each vCPU thread's affinity
  after QEMU starts; a failure is logged and shows up in
  `status.appliedTopology.message`. Memory binding is enforced by QEMU
  (`policy=bind`), and the cgroup cpuset still confines the VM to its
  allocated CPUs.
- **Hotplugged vCPUs and DIMMs are not NUMA-bound.** A bound Machine always
  boots at its allocated size, not a persisted hotplug size.
- **Memory accounting counts Kairon allocations only.** A NUMA node's free
  memory is its capacity minus other topology Machines' allocations. VMs
  without `spec.topology` are not counted per NUMA node.
- **FluxVM's bare migration receiver has no NUMA.** The target's binding
  only reaches the receiver through the source record Kairon relays.
- Only single-NUMA hosts have run in this repository's lab. Multi-NUMA,
  GPU with NIC, and SR-IOV placement are unit-tested only.
