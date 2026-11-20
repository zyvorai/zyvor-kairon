# User guide: NUMA topology, CPU set, and hugepages

Three opt-in `spec.resources` fields, each a direct passthrough to a real,
already-existing FluxVM (QEMU backend only) capability -- not something
Kairon invents or emulates itself.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db-latency-sensitive
spec:
  image: {path: /var/lib/fluxvm/images/db.qcow2}
  resources:
    cpu: "4"
    memory: 8Gi
    numaNode: 0
    cpuSet: "0-3"
    hugepages: true
  runtime: {backend: qemu}
  powerState: Running
```

- **`resources.numaNode`** — binds the guest's virtual NUMA topology to
  host NUMA node `0` (FluxVM's own `numa_node`, QEMU `-numa node,...`).
- **`resources.cpuSet`** — a `-numa cpu=...`-style CPU expression (FluxVM's
  own `cpuset`) describing which vCPUs belong to that virtual NUMA node --
  a **guest-visible topology hint**, not host-level pinning (see "What
  this is not," below).
- **`resources.hugepages`** — backs the guest's memory with host
  hugepages (FluxVM's own `hugepages`, QEMU
  `-object memory-backend-file,...,mem-path=/dev/hugepages,prealloc=on`)
  instead of regular anonymous memory -- real value for memory-bandwidth-
  sensitive workloads (DPDK, in-memory databases), at the cost of
  requiring the host to actually have hugepages configured and free
  (`/proc/sys/vm/nr_hugepages` or a boot-time `hugepagesz=`/`hugepages=`
  kernel parameter) -- Kairon doesn't configure or reserve host hugepages
  for you, the same "you already needed this infrastructure" posture
  `csiNode`'s `target_core_mod` kernel module requirement already has.

All three also work on a `MachineInstanceType` (see
[`machine-instance-types.md`](machine-instance-types.md)) -- bundle them
into a named, reusable shape the same way `cpu`/`memory` already are.

## Backend requirement

**QEMU only.** FluxVM only implements NUMA/cpuset/hugepages for its QEMU
backend -- Cloud Hypervisor and Firecracker Machines get nothing from
these fields today. Rather than silently ignoring them the way FluxVM
itself does, Kairon refuses the Machine outright with a clear error
(`spec.resources.numaNode/cpuSet/hugepages require the qemu backend`) if
any of the three are set and the Machine's `spec.runtime.backend` resolves
to anything other than `qemu` -- you get a startup-time error, not a
silently-ineffective setting.

## What `cpuSet` is not -- and where real pinning now lives

**`resources.cpuSet` itself is still not exclusive host-core pinning.** It's
a *guest-visible* NUMA topology hint (what the guest's own OS/scheduler is
told about its vCPUs), not a host-level guarantee that those vCPU threads
run on specific physical cores with nothing else scheduled onto them.

For the real capability telco/NFV workloads usually mean by "dedicated CPU
placement" (KubeVirt's `dedicatedCpuPlacement`), see
**`resources.cpuPinning`** in [`machine-cpu-pinning.md`](machine-cpu-pinning.md)
instead -- a separate, real, exclusive host-core allocator built on top of
the exact same FluxVM `cgroup cpuset.cpus` resize API this field used to
say was unreachable. `cpuSet` and `cpuPinning` are independent: setting
`cpuPinning` doesn't imply or set `cpuSet` (the scheduler's own core
selection isn't NUMA-topology-aware in its first cut, a named limit in
that guide), and combining both doesn't cross-validate that the pinned
cores actually fall within the requested `numaNode`.

## Scheduler-chosen NUMA placement

`numaNode` and `cpuSet` are fixed by hand. To have the scheduler choose the
NUMA node(s), host CPUs and local devices, and FluxVM bind each guest NUMA
node's memory to them, use `spec.topology` instead: see
[topology-aware placement](machine-topology.md). The two cannot be
combined on one Machine (FluxVM refuses `numa` with `numa_node`/`cpuset`).

## Real limits today (first cut)

- QEMU-only, enforced with a clear error (see above) -- not a silent
  no-op for other backends.
- No host hugepage reservation/discovery -- an operator's own
  responsibility, same posture as other host-level prerequisites this
  project already documents (see SECURITY.md).
- `resources.cpuSet` only ever describes guest-visible topology, not real
  exclusive host-core pinning -- see
  [`machine-cpu-pinning.md`](machine-cpu-pinning.md) for the field that
  actually does that.
- Creation-time-only, like `spec.resources.cpu`/`.memory` themselves --
  editing these fields on an already-running Machine has no effect
  (CPU/memory hotplug is a separate, already-existing mechanism -- see
  [`machine-hotplug.md`](machine-hotplug.md) -- and doesn't carry NUMA/
  cpuset/hugepages changes either).
- No live-migration compatibility checking -- unlike `spec.deviceClaims`
  (see [`machine-fencing.md`](machine-fencing.md)'s VFIO preflight
  section), a Machine using these fields can attempt live migration to a
  target with a completely different (or absent) NUMA topology; whether
  that's safe is between you and QEMU, not something Kairon checks.
