# User guide: runtime diagnostics (capabilities, pressure, cpuset, freeze/thaw)

Four small FluxVM diagnostics, API-only, no dashboard yet: a node's real
capability manifest, a Machine's PSI pressure stats, its effective host
CPU pinning, and a host-kernel-level force-freeze distinct from
`spec.powerState: Paused`.

## Runtime capabilities

**`GET /api/v1/nodes/{node}/capabilities`** returns a node's real, current
FluxVM capability manifest -- per-backend migration support (live,
pre-copy, post-copy, multifd, whether shared storage is required, which
transports) and per-backend VM-state snapshot support (memory, disk,
portable). Any authenticated operator; static, non-sensitive config.

**This is a diagnostic to cross-check against, not (yet) a replacement
for Kairon's own hardcoded backend assumptions.** Several facts Kairon's
own code already asserts today -- `spec.resources.numaNode`/`.cpuSet`/
`.hugepages` requiring the `qemu` backend
([`machine-cpu-numa.md`](machine-cpu-numa.md)), Firecracker not
supporting VM-state snapshot
([`machine-vm-state-snapshot.md`](machine-vm-state-snapshot.md)) -- are
also things this endpoint reports authoritatively. Kairon doesn't query
it before making those decisions today; the hardcoded checks were
verified directly against FluxVM's own source and remain correct as of
this writing, so this endpoint exists for operators and tooling to
confirm what a specific node actually supports (useful when nodes run
different FluxVM versions, or as a build block for a future admission-time
check), not as a currently-load-bearing part of Kairon's own decisions.

## Pressure (PSI)

**`GET /api/v1/machines/{ns}/{name}/pressure`** returns a Machine's real,
cgroup-derived [PSI](https://docs.kernel.org/accounting/psi.html)
(Pressure Stall Information) -- `cpu_some`, `memory_some`/`memory_full`,
`io_some`/`io_full`, each an `{avg10, avg60, avg300, total}` reading
straight from the kernel. Complements `status.resourceUsage`
([`machine-resource-limits.md`](machine-resource-limits.md)'s own
"Watching live usage" section, raw CPU%/memory/disk-I/O usage): pressure
reports how often tasks in
the Machine's cgroup are actually *stalled* waiting on a resource, a more
direct signal of real contention than a usage percentage alone -- a VM
can show high CPU% without ever being stalled (no contention), or show
moderate CPU% while badly stalled (real contention from a neighbor).

Any authenticated operator -- read-only, same posture as viewing a
Machine's own status.

## Effective CPU set

**`GET /api/v1/machines/{ns}/{name}/cpuset`** returns the real, current
set of host CPU numbers (`{"cpus": [...]}`) the Machine's cgroup is
allowed to run on right now. Read-only ground truth, not a Kairon-managed
allocation -- see [`spec.resources.cpuSet`](machine-cpu-numa.md)'s own
documented limit: Kairon doesn't allocate or guarantee non-overlapping
host cores across competing Machines on a node today, so this simply
reports whatever the host (and, if configured, `spec.resources.cpuSet`
itself) actually produced.

## Freeze / thaw

**`POST /api/v1/machines/{ns}/{name}/freeze`** / **`.../thaw`** /
**`GET .../frozen`** halt (and resume) a Machine's *entire cgroup* --
every process in it, the VMM itself and any of its own helper threads --
at the kernel scheduler level, via cgroup v2's freezer controller.
Admin-only for freeze/thaw (frozen status itself is read-only, any
operator).

**Genuinely different from [`spec.powerState: Paused`](machine-pause-resume.md)**,
despite both making a Machine appear frozen from the outside:

| | `Paused` | Freeze |
|---|---|---|
| Mechanism | QMP `stop`/backend equivalent | cgroup v2 freezer (kernel scheduler) |
| Scope | Guest vCPU execution only | Every process in the VM's cgroup, including host-side I/O/emulation threads |
| Requires the hypervisor's own control channel to be responsive | Yes | No -- pure kernel operation |
| Typical use | Routine suspend/resume | Diagnosing a hung VM, or a last-resort freeze when QMP itself won't respond |

Because freeze works even when a VM's own control channel is
unresponsive, it's the tool to reach for when a VM seems stuck and
`spec.powerState: Paused` itself isn't taking effect -- not a
recommended routine pause/resume replacement.

## Real limits today (first cut)

- **No dashboard yet.** All four capabilities here are API-only.
- **Freeze/thaw have no interaction guardrails.** There's no check against
  a Machine mid-migration or mid-hotplug before freezing it -- the same
  "you and FluxVM's own semantics" posture `Paused` already documents for
  analogous cases.
- **Pressure/cpuset are point-in-time snapshots**, not a time series --
  for history, use Prometheus/external monitoring against the node's own
  cgroup filesystem, not these endpoints.

For a full snapshot of a misbehaving Machine (these readings plus QMP state,
console log, dataplane, events and an optional memory dump in one tar.gz),
use `kaironctl diagnose machine NAME --deep`; see
[machine-incident-bundle.md](machine-incident-bundle.md).
