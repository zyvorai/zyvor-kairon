# User guide: hibernate (`spec.powerState: Hibernated`)

Save a running Machine's RAM, vCPU and device state to disk, stop its VMM and
give the node's CPU and memory back. Setting `powerState: Running` again loads
the state and the guest carries on in the same boot: same processes, same
`/dev/shm`, same open files. Compare [`Halted`](machine-halt.md) (VMM off, the
guest reboots on resume) and [`Paused`](machine-pause-resume.md) (vCPUs stopped,
RAM stays resident).

Experimental: same-node hibernate and resume have run on real hardware (see
[`COMPATIBILITY.md`](../COMPATIBILITY.md#hibernate)); resume on another node has
not.

```yaml
spec:
  powerState: Hibernated   # was Running
```

Or `kaironctl hibernate db`, or the dashboard's **Hibernate** button. Resume with
`powerState: Running`, `kaironctl resume db` (or `start`), or **Resume**.

## What happens

1. kairon-node calls FluxVM `POST /v1/vms/{id}/hibernate`. FluxVM pauses the
   vCPUs, writes the QEMU migration stream and a compatibility manifest (QEMU
   version, q35 machine type, CPU vendor and flags) to its hibernation
   directory, then quits QEMU. See FluxVM's `docs/hibernation.md`.
2. The Machine reports `status.phase: Hibernated`, conditions `Ready=False`
   (`Hibernated`) and `Hibernated=True`, and `status.hibernation`:

   ```yaml
   hibernation:
     node: node-a
     runtimeID: b4e19f2b-...
     stateBytes: 479456925
     stateDir: /var/lib/fluxvm/hibernation/b4e19f2b-...
     hibernatedAt: "2026-10-12T04:58:00Z"
     sharedStorage: false        # root disk on shared storage
     host: {qemuVersion: 10.2.1, machineType: pc-q35-10.2, cpuVendor: GenuineIntel, cpuFlags: [...], stateDir: /var/lib/fluxvm/hibernation}
   ```

   Like `Halted` and `Stopped`, a hibernated Machine holds no node capacity in
   the scheduler or in `MachineQuota`, and leaves service-fabric membership.
3. On `powerState: Running`, kairon-controller picks the node that resumes it
   and writes `status.hibernation.resumeNode`. Only that node's kairon-node
   resumes it, so two nodes never load the same state. FluxVM starts QEMU with
   `-incoming defer`, loads the state and continues the guest; the saved state
   is then deleted and `status.hibernation` cleared (`Hibernated=False`,
   reason `Resumed`).

## Where it resumes

- **Its own node**, when that node is ready and has room again (the capacity
  was released, so another Machine may have taken it).
- **Another node**, only when its own node is gone, not ready or full, **and**
  `status.hibernation.sharedStorage` is true, **and** the other node's
  `NodeTopology` `status.hibernation` facts match the manifest: same QEMU
  version, machine type, CPU vendor and hibernation directory path, and a
  superset of the CPU flags. The controller moves `spec.nodeName`, records a
  `ResumeElsewhere` event, and the target node imports the state from FluxVM's
  hibernation directory before starting it.
- Otherwise the Machine stays `Hibernated` with `Ready=False`
  (`ResumeWaiting`) and a message naming why each node was refused.

For a move to work, every node must mount FluxVM's `hibernation_dir` from
shared storage at the same path, and the root disk must be on shared storage.
Through Kairon that means an Atlas RBD root disk (`ceph-rbd-in-place`); the
controller cannot verify that the directory is really shared, only that the
path matches. If the import fails, the Machine goes `Blocked` (`ResumeBlocked`).

## Throwing the saved state away

Annotate the Machine to boot from disk instead of resuming:

```sh
kubectl annotate machine db kairon.zyvor.dev/discard-hibernation=true
```

kairon-node deletes the saved state, removes the annotation and clears
`status.hibernation`; the next `Running` is a fresh boot. Setting
`powerState: Halted` or `Stopped` on a hibernated Machine also drops the state.

## Limits

- QEMU only. FluxVM refuses vTPM, VFIO devices (GPU, SR-IOV), virtiofs shares,
  confidential VMs, and Machines with CPU, memory or NICs hot-plugged since they
  started; the refusal shows as `Hibernated=False` (`HibernateFailed`).
- Moving to another node also needs no data disks, no attached install media and
  no direct-datapath or macvtap NIC.
- The guest is paused, not frozen: no `fsfreeze` and no separate disk snapshot.
  The root disk stays consistent with the saved RAM as long as nothing else
  writes it while hibernated.
- Hibernating takes about as long as writing the guest's touched RAM; resuming
  is fast. The guest clock jumps on resume; run NTP in guests that care.
- `Hibernated` requires an existing runtime: set `Running` first. A spec edit
  made while hibernated does not apply on resume (same as `Halted`).
- Kairon deletes a hibernated FluxVM record on the old node once the Machine is
  deleted or has resumed elsewhere (checked every minute).
