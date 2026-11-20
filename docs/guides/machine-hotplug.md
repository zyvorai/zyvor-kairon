# User guide: CPU, memory, disk and NIC hotplug

How growing `spec.resources` on a running Machine works, and what today's
real limits are.

## How it works

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  resources: {cpu: "4", memory: "4Gi"}   # was "2" / "2Gi"
  # ... unchanged otherwise
```

## Requesting more hotplug headroom

By default FluxVM reserves roughly double `cpu`, and `memory + 2Gi` or
double `memory` (whichever is larger), as hotplug headroom at boot time --
enough for most cases, but a fixed ceiling you can't grow later (see "Real
limits" below). If you know upfront you'll want to grow a Machine well
beyond that default, request more headroom explicitly at creation:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: db
spec:
  resources:
    cpu: "4"
    memory: "4Gi"
    maxCpu: "32"       # reserve headroom for hotplugging up to 32 vCPUs
    maxMemory: "128Gi" # reserve headroom for hotplugging up to 128Gi
  # ... unchanged otherwise
```

`maxCpu`/`maxMemory` are creation-time-only, like `cpu`/`memory`'s own
boot-time sizing -- unlike them, editing these on an already-running
Machine has no effect at all, since the headroom is baked into the VM's
boot-time `-smp`/`-m` arguments (`maxcpus=`/`maxmem=`) and there's no way
to grow that after boot. If `maxCpu` is set lower than `cpu`, FluxVM
silently raises it back up to at least `cpu` rather than erroring; if
`maxMemory` is set lower than `memory`, that's an inconsistent QEMU
argument FluxVM/QEMU itself will reject with an error, not something
Kairon validates first.

Just edit `spec.resources` on an already-`Running` Machine and increase
`cpu`/`memory` — `kairon-node` picks up the difference on its next reconcile
tick and calls FluxVM's real QMP hotplug (`device_add`/`object-add`), not a
reboot. `status.appliedVCPUs`/`appliedMemoryMiB` report what's actually been
realized so far.

Unlike every other `spec.resources` field, this one **is** live-reconciled
-- `spec.image`, `spec.network`, `spec.cloudInit` etc. remain creation-time-only.

## Why Kairon tracks `status.appliedVCPUs`/`appliedMemoryMiB` itself

FluxVM has no endpoint to query a VM's *current* live vCPU/memory count --
hotplugged CPUs and DIMMs are pure QMP-runtime state, never written back
into FluxVM's own stored VM record. So `kairon-node` is the only source of
truth for how much of `spec.resources` has actually been realized: it
computes the delta between `spec.resources` and `status.applied*`, sends
only that delta to FluxVM's hotplug endpoints, and advances `status.applied*`
by the amount FluxVM reports actually landed.

## Real limits today (v1 of this feature)

- **Grow-only.** FluxVM has no CPU/DIMM unplug. Lowering `spec.resources`
  below what's already hotplugged is logged and ignored, not applied and
  not an error -- `status.applied*` simply stays at its current (higher)
  value. To actually shrink a Machine, stop and recreate it.
- **Bounded by headroom reserved at creation.** `spec.resources.maxCpu`/
  `.maxMemory` (see "Requesting more hotplug headroom" above) requests
  more than FluxVM's own default headroom, but it's still a fixed ceiling
  set once at creation, not something a running Machine can grow. Once
  that headroom is exhausted, a hotplug request fails clearly
  (`not enough hotplug headroom...`), surfaced in `status.message`, and
  retried automatically next tick (it won't succeed until the request is
  lowered back within headroom, or the Machine is stopped and recreated
  with a larger `maxCpu`/`maxMemory`).
- **Stop/start and Halted resume both keep the hotplugged size.**
  `Stopped` -> `Running` and cold migration recreate the VM at the realized
  size (see [Stop/start keeps the realized size](#stopstart-keeps-the-realized-size)).
  `Halted` -> `Running` boots from FluxVM's kept create-time config, so
  kairon-node takes that boot size as the baseline and hotplugs back up to
  max(`spec.resources`, the pre-halt `status.applied*`) right after the
  start. If that hotplug fails, `status.applied*` reports the boot size
  rather than the pre-halt one. Machines halted by
  [preemption](preemption.md) get their size back the same way.
- QEMU-only, since that's the only FluxVM backend with hotplug support.

## Disk hotplug: `spec.disks`

`spec.disks` attaches PersistentVolumeClaims to a running Machine as SCSI
block disks. Add an entry and kairon-node hot-adds the disk on its next tick;
remove it and the disk is unplugged. The PVC and its data are never touched.

```yaml
spec:
  disks:
  - name: data          # [a-z0-9-], up to 32 chars, not "root"
    claimName: db-data
```

or `kaironctl disk attach db data --claim db-data` /
`kaironctl disk detach db data` / `kaironctl disk list db`.

- The PV must be hostPath- or local-backed. A **Block**-mode PV attaches its
  device path (e.g. `/dev/vg0/data`); a **Filesystem**-mode PV attaches the
  `disk.img` inside its directory, as for boot volumes.
- A **CSI**-backed PV works too, with Kairon's own driver
  (`csi.kairon.zyvor.dev`, Filesystem or Block mode) or any driver on the node's
  third-party allowlist (Filesystem or Block mode, with a `VolumeAttachment`
  when the driver needs one). kairon-node stages and publishes the volume
  under `<publishDir>/disks/<driver>/<machine>/<disk>`, attaches it, and
  records it in `status.diskVolumes`. Removing the disk detaches it and then
  unpublishes and unstages the volume; deleting the Machine does the same.
  The node needs the CSI setup from
  [machine-storage-csi.md](machine-storage-csi.md).
- To point a CSI disk at another claim, remove the entry first and add it
  back under the new claim once `status.diskVolumes` has dropped it.
- Detach CSI disks before migrating a Machine. The volume is published on
  the source node only, and the destination does not take it over.
- In the guest the disk shows up as
  `/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_<name>`; device letters are not
  stable, the serial is.
- FluxVM's `policy.allowed_image_dirs`, when set, must include the PV
  directories; block devices must live under `/dev`.
- `status.attachedDisks` lists what kairon-node attached. Only those are ever
  detached, so a disk attached to the VM by other means is left alone. A VM
  recreated from scratch (stop/start, cold migration) gets every `spec.disks`
  entry re-attached.
- QEMU's image locking refuses a disk another running VM already has open.

## NIC hotplug and hot-unplug: `spec.network.extraInterfaces`

On a `mode: tap` Machine, `spec.network.extraInterfaces` adds up to three
bridged NICs next to the primary one, live:

```yaml
spec:
  network:
    mode: tap
    bridge: br0
    extraInterfaces:
    - name: lan
      bridge: br-lan
    - name: dmz
      bridge: br-dmz
      mac: 02:aa:bb:cc:dd:ee   # optional
```

or `kaironctl nic add fw lan --bridge br-lan` / `kaironctl nic remove fw lan`
/ `kaironctl nic list fw`.

- Without `mac`, the MAC is derived from the Machine UID and interface name,
  so it stays the same across restarts.
- Removing an entry hot-unplugs that NIC and deletes its host tap. The guest
  must acknowledge the PCIe unplug within 10 seconds; Linux guests do.
- The primary NIC (`spec.network.bridge`) can't be hot-removed.
- NICs keep their PCIe slot when one in the middle is removed, so guest
  interface names of the others don't change.
- `status.attachedInterfaces` records each NIC with the MAC used.

The MCP tools `machine_disk` and `machine_nic` (write, `--allow-write`) make
the same spec edits for agents.

## Stop/start keeps the realized size

FluxVM boots from the create request, not from QMP hotplug state. On the
next create, kairon-node sends max(`spec.resources`, `status.appliedVCPUs` /
`appliedMemoryMiB`), clamped to `maxCpu` / `maxMemory` when those are set.
A restart therefore comes back at the size that was actually realized.

Set `kairon.zyvor.dev/hotplug-persist: "true"` to also write that size
back into `spec.resources`. The write only raises CPU or memory. Without
the annotation, spec stays at the GitOps value and only the next boot
uses the realized size.

See `examples/hotplug-persist.yaml` for a Machine with the annotation and `maxCpu`/`maxMemory` headroom set.

Lowering `spec.resources` does not shrink the next boot either: it is
still max(spec, applied). When the boot memory reaches `maxMemory`,
kairon-node omits `max_memory_mib` and FluxVM picks its default headroom,
because QEMU refuses to start with DIMM slots and `maxmem` equal to the
boot size. Verified live on 2026-10-06: a 1 vCPU / 1Gi Machine hotplugged
to 2 / 2Gi (`maxCpu: "2"`, `maxMemory: 2Gi`) restarted with
`-smp cpus=2` and `-m 2048M`.
