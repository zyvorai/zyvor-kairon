# User guide: elastic memory and safe overcommit (`spec.resources.elasticMemory`)

Lets a node run more Machine memory than it physically has. Each elastic
Machine has a guaranteed minimum. The scheduler packs Machines by those
minimums, up to a node overcommit ratio. When the host runs short,
kairon-node inflates a virtio-balloon in each elastic guest toward its
minimum and gives the memory back when pressure passes. When there is
nothing left to reclaim, the node reports `KaironMemoryPressure`. The
scheduler then avoids it, and the controller can optionally live-migrate
Machines away.

Experimental: see [Real limits today](#real-limits-today) and the lab rows
in [`COMPATIBILITY.md`](../COMPATIBILITY.md#elastic-memory).

## How it fits together

1. **FluxVM** adds a `virtio-balloon-pci` device to QEMU VMs created with a
   `balloon` request (`deflate-on-oom=on`, `free-page-reporting=on` by
   default). With free-page reporting, the guest kernel (5.8+) returns
   freed pages to the host on its own. FluxVM serves `GET
   /v1/host/memory` (MemAvailable and memory PSI), `POST
   /v1/vms/{id}/balloon` and `GET /v1/vms/{id}/memory`.
2. **kairon-controller** schedules by guaranteed minimum and overcommit
   ratio (below), and avoids nodes with `KaironMemoryPressure=True`.
3. **kairon-node** creates elastic Machines with the balloon. Every 10s it
   samples host memory and moves balloon targets. It writes
   `status.memory` on each Machine and the `KaironMemoryPressure` condition
   on its Node.
4. **kairon-controller** (opt-in, `--memory-pressure-evacuate`) migrates
   Machines off a node whose condition stays True.

## Making a Machine elastic

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web-1
spec:
  runtime:
    backend: qemu
  resources:
    cpu: "2"
    memory: 8Gi
    elasticMemory:
      min: 2Gi              # never ballooned below this
      freePageReporting: true   # default
  image:
    path: /var/lib/fluxvm/images/ubuntu-24.04.qcow2
```

Rules (webhook and CRD):

- The backend must be `qemu`, or `auto`/unset resolving to QEMU on the
  node. kairon-node refuses to create it on any other backend.
- No `hugepages`, `devices` or `deviceClaims`. Pinned or passed-through
  guest memory can't be ballooned.
- `min` must be at most `memory`.
- `elasticMemory` can't be added to or removed from an existing Machine.
  Recreate it instead. `min` itself can change.

The guest needs the `virtio_balloon` driver, which is built into most
distribution kernels. Without it the balloon never inflates and
`status.memory.balloonActualMiB` stays 0.

## Node overcommit ratio

```bash
kubectl annotate node worker-1 kairon.zyvor.dev/memory-overcommit-ratio=1.5
```

With a ratio above 1, the scheduler admits a Machine when both of these
hold:

- the sum of guaranteed memory fits allocatable. Guaranteed is
  `elasticMemory.min` for elastic Machines and the full `memory` for the
  others;
- the sum of full `memory` is at most ratio × allocatable.

The ratio is clamped to 1–4. Without the annotation it is 1, and
scheduling is exactly what it was before: full memory against allocatable.
The dashboard's Nodes page shows the ratio, guaranteed memory and
pressure. `GET /api/v1/nodes/capacity` returns them as
`memoryOvercommitRatio`, `guaranteedMemoryMiB` and `memoryPressure`.

Set the ratio no higher than your elastic Machines can actually give back.
Non-elastic Machines never shrink, so a node full of them gains nothing
from a ratio.

## Reclaim

kairon-node's loop, per node:

| Host state | Action |
| --- | --- |
| MemAvailable below the low watermark, or PSI `some avg10` above the limit | Inflate balloons toward the high watermark, in proportion to each Machine's headroom, at most 25% of its reclaimable memory per pass |
| Pressure, and every balloon already at its minimum | Set `KaironMemoryPressure=True` (reason `ReclaimExhausted`) |
| MemAvailable above the high watermark and PSI calm | Deflate, at most one step per Machine and no more in total than the memory above the watermark |

Flags and environment (kairon-node; Helm `node.memoryReclaim`):

| Flag | Env | Default |
| --- | --- | --- |
| `--memory-reclaim` | `KAIRON_MEMORY_RECLAIM` | `true` |
| `--memory-low-watermark-mib` | `KAIRON_MEMORY_LOW_WATERMARK_MIB` | 10% of MemTotal |
| `--memory-high-watermark-mib` | `KAIRON_MEMORY_HIGH_WATERMARK_MIB` | 20% of MemTotal (2× low when only low is set) |
| `--memory-psi-some-avg10` | `KAIRON_MEMORY_PSI_SOME_AVG10` | `10` |
| `--memory-reclaim-interval` | - | `10s` |

The loop runs only when FluxVM reports `host.balloon` in
`/v1/runtime/capabilities`. It writes the node condition even on a node
with no elastic Machines. There, "exhausted" just means the host is under
pressure, so the scheduler stops placing new Machines on it.

`status.memory` on each elastic Machine:

```yaml
status:
  memory:
    currentMiB: 8192        # includes hot-added DIMMs
    minMiB: 2048
    balloonTargetMiB: 3072  # asked to give back
    balloonActualMiB: 3072  # given back so far
    guestAvailableMiB: 1830 # the guest's MemAvailable, when its driver reports stats
    reason: HostPressure    # HostPressure | Relief | Restart
    lastChangeTime: "2026-10-12T10:04:31Z"
```

`kaironctl describe machine web-1` prints a one-line summary under the
JSON. A restart resets the balloon to 0, and the next pass records
`reason: Restart`.

## Evacuation (opt-in)

```bash
kairon-controller --memory-pressure-evacuate \
  --memory-pressure-evacuate-strategy=live \
  --memory-pressure-evacuate-grace=2m \
  --memory-pressure-evacuate-min-retry-interval=5m
```

Helm: `controller.memoryPressureEvacuation.enabled=true`. This also grants
the controller `machinemigrations` create.

Once `KaironMemoryPressure` has been True for the grace period, the
controller creates one `MachineMigration` per pressured node at a time.
It picks the lowest-priority, then largest, Running Machine, and respects
[MachineDisruptionBudgets](machine-disruption-budgets.md). It skips a node
that already has a migration in flight, and waits the retry interval
before trying the same Machine again (annotation
`kairon.zyvor.dev/memory-evacuate-attempted-at`). With `strategy: live`,
Machines with passthrough devices are skipped.

## Metrics

| Metric | Labels |
| --- | --- |
| `kairon_node_memory_pressure` | `node`, `state` (`pressure`, `exhausted`) |
| `kairon_machine_balloon_mib` | `namespace`, `machine`, `kind` (`target`, `actual`) |
| `kairon_memory_reclaim_actions_total` | `direction` (`inflate`, `deflate`) |

See [observability](observability.md).

## Real limits today

- QEMU backend only. Cloud Hypervisor and Firecracker VMs keep FluxVM's
  own idle-reclaim balloon, which Kairon doesn't drive. FluxVM's idle
  reclaim, in turn, doesn't cover QEMU VMs.
- FluxVM keeps the balloon target in a workspace file. A FluxVM restart
  keeps it; a VM relaunch resets it to 0.
- Hot-added memory DIMMs are never unplugged. Reclaim is balloon-only.
- The overcommit ratio is a node annotation only. There is no cluster-wide
  default flag.
- NUMA/topology accounting (`spec.topology`) still counts each Machine's
  full memory against its NUMA node.
- No KSM (same-page merging).
- Guest statistics need the guest balloon driver. Free-page reporting needs
  guest kernel 5.8+.
- `elasticMemory` is not part of `MachineInstanceType`. Set it on the
  Machine.
