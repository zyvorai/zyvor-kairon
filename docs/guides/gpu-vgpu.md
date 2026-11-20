# User guide: vGPU profiles, MIG, GPU memory and GPU health (experimental)

A Machine can ask for a slice of a GPU instead of a whole one. The slice
is a **mediated device** (mdev): the host's vGPU driver carves the GPU into
instances of a **profile** (for example NVIDIA `GRID A100-3-20C`), and each
instance is passed to QEMU like a VFIO device. Kairon schedules profiles
against the node's free instances and GPU memory, creates the instance
through FluxVM when the Machine starts, and removes it when the Machine
stops or is deleted.

Whole-GPU passthrough is unchanged; see
[topology-aware placement](machine-topology.md).

## How it fits together

1. FluxVM lists the host's mdev parents, their profiles (with framebuffer
   size and MIG slices when the driver says so) and existing instances at
   `GET /v1/host/mdev`, and judges each GPU's health at
   `GET /v1/host/gpus/health` (PCIe AER counters, link width, and
   `nvidia-smi` ECC, retired pages and temperature when it is installed).
2. kairon-node publishes both in `NodeTopology`: `status.mediatedDevices`
   (parents, profiles, free instances, VRAM) and `health` on each device.
   Only `vfio-pci` profiles are published; `--mdev-parents` (chart
   `node.mdevParents`) limits which parents Kairon may use.
3. The scheduler picks a parent with a free instance of the profile and
   writes it to `spec.topologyAllocation.devices[]` with `parent`,
   `profile`, `framebufferMiB` and a `uuid` that is stable for the Machine.
4. kairon-node creates the instance (`POST /v1/host/mdev`) and passes its
   UUID in FluxVM's `vfio_devices`. QEMU gets
   `-device vfio-pci,sysfsdev=/sys/bus/mdev/devices/<uuid>`.
5. Stopping or deleting the Machine removes the instance, so its share of
   the GPU goes back to the pool. Starting again re-creates the same UUID.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: inference
spec:
  image: {path: /var/lib/fluxvm/images/ubuntu-noble-base.img}
  resources: {cpu: "4", memory: 16Gi}
  runtime: {backend: qemu}
  devices:
    - name: gpu
      class: gpu
      vendor: nvidia
      profile: GRID A100-3-20C
      count: 1
```

`profile` matches a profile's ID (`nvidia-558`) or its name, ignoring
case. It works for `gpu` and `accelerator` entries and cannot be combined
with `virtualFunction`. `vendor`, `model`, `minMemory` (per instance) and
`alignment` work as for whole devices. A `MachineAcceleratorProfile` can
name the same request for reuse; see
[accelerator profiles](accelerator-profiles.md).

## Accounting

- **Instances.** A profile's free count is what the driver reports minus
  instances Kairon has allocated but not created yet.
- **GPU memory.** When profiles have a framebuffer size, profiles on one
  parent share a pool: placing `n` instances of a profile needs
  `n x framebuffer` free from `available x framebuffer` of that profile,
  less what other pending allocations on that parent take. The driver
  remains the final word: FluxVM refuses a create the GPU cannot take
  (`503`), and drivers that allow one profile per GPU at a time are only
  enforced there.
- **MIG.** MIG-backed profiles carry `migSlices` and `migMemoryGiB` in
  `NodeTopology`. Kairon schedules them like any other profile; it does not
  partition GPUs into MIG instances itself (use the NVIDIA tooling).
- **Unhealthy GPUs** are skipped: an `Unhealthy` device is never handed out
  whole, and an `Unhealthy` parent offers no profiles. `Degraded` devices
  are still used and are shown as such.

When nothing fits, the Machine stays `Pending` with
`TopologyUnsatisfiable` and a message naming the profile and how many are
free:

```text
1 node(s): TopologyUnsatisfiable: needs 4 gpu profile mbochs-large, 3 free
```

## Inspecting

`kaironctl node topology <node>` shows device health, a "vGPU parents"
section with each profile's free instances and the Machines holding them,
and a "GPU memory" line with allocated and total VRAM.

Metrics (controller):

| Metric | Labels | Meaning |
|---|---|---|
| `kairon_gpu_vram_mib` | `node`, `state` | GPU memory per node: `total` from `NodeTopology`, `allocated` to Machines' whole GPUs and vGPU framebuffers |
| `kairon_gpu_devices` | `node`, `health` | GPUs per health state (`Healthy`, `Degraded`, `Unhealthy`, `Unknown`) |
| `kairon_vgpu_available_instances` | `node`, `parent`, `profile` | Free instances per profile as last reported by the node |

## Host setup

- Install the vendor's vGPU host driver (for NVIDIA, the vGPU manager)
  and check that `/sys/class/mdev_bus/` lists the GPU.
- On nodes that also run KubeVirt, stop KubeVirt from managing mediated
  devices, or its virt-handler removes instances it did not create. On
  KubeVirt 1.8 add the `DisableMDEVConfiguration` feature gate (deprecated
  upstream, still honoured in the lab).
- FluxVM needs `/dev/vfio/vfio` and the instance's `/dev/vfio/<group>`;
  its per-VM device cgroup allows both.

## Real limits today

- **No live migration.** A Machine with a vGPU is VFIO-backed and can only
  cold-migrate, like whole-device passthrough.
- **Lab coverage.** The end-to-end lab uses the kernel's `mbochs` sample
  driver (a virtual display device with three profiles sharing a pool), not
  an NVIDIA GPU. NVIDIA vGPU, MIG-backed profiles and `nvidia-smi` health
  are unit-tested only; see [`COMPATIBILITY.md`](../COMPATIBILITY.md#gpu-and-vgpu).
- **Health is read, not acted on.** A GPU that turns `Unhealthy` under a
  running Machine is reported; the Machine is not moved.
