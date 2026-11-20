# User guide: halt (`spec.powerState: Halted`)

Power a Machine's VMM process off while keeping FluxVM's own VM record
(and disk) intact -- distinct from both `Stopped` (full teardown, rebuilt
from `spec` on next `Running`) and [`Paused`](machine-pause-resume.md)
(guest CPUs suspended, but the process and its RAM stay resident).

## What this does

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: db}
spec:
  powerState: Halted   # was Running
  # ... unchanged otherwise
```

Or via `kaironctl halt db` (`kubectl kairon halt db` works identically), or
the dashboard's **Halt** button. Resume with `spec.powerState: Running`,
`kaironctl start db`, or the dashboard's **Resume** button -- the same
verb `Stopped` and `Paused` both already use to come back.

Halting calls FluxVM's real `POST /v1/vms/{id}/stop`: the VMM process
terminates and guest RAM is lost (the guest reboots on resume, same as
`Stopped`), but FluxVM keeps its own VM record and disk in place, so
resuming (`POST /v1/vms/{id}/start`) doesn't need Kairon to fully
re-derive the runtime from `spec` the way `Stopped` -> `Running` does.
`status.phase` reports `Halted` while off -- deliberately not `Stopped`,
since the two mean genuinely different things for this Machine's
resumability and node-capacity accounting (a `Halted` Machine, like
`Stopped`, frees the node capacity it held -- see
[`machine-disruption-budgets.md`](machine-disruption-budgets.md) and
`MachineQuota`'s own accounting, both of which now treat `Halted` the
same way they already treat `Stopped`).

## Real limits today (first cut)

- **Requires an existing runtime.** Setting `powerState: Halted` on a
  Machine that's never been realized against FluxVM doesn't create one
  halted-from-the-start -- `status.phase` stays `Pending` with a clear
  message instead, the same convention `Paused` already established. Set
  `powerState: Running` first, let it actually start, then halt it.
- **A spec edit made while Halted does not apply on resume.** Unlike
  `Stopped` -> `Running` (which always fully rebuilds the FluxVM runtime
  from the current Machine `spec`), resuming from `Halted` calls FluxVM's
  plain `start`, which reuses FluxVM's own last-applied VM config as-is.
  A `spec.resources`, `spec.cloudInit`, or VFIO device-claim change made
  while Halted has no effect until a real `Stopped` -> `Running` cycle --
  the same "read only once, editing is a silent no-op" caveat
  `spec.cloudInit`/`spec.network.forwards` already carry for an
  already-running Machine (see
  [`docs/architecture.md`](../architecture.md#day-2-operations-on-a-running-machine)),
  not a new kind of gap. Hotplugged CPU and memory are the exception:
  kairon-node hotplugs them back right after the resume (see
  [`machine-hotplug.md`](machine-hotplug.md)).
- **Preemption uses Halt.** A Machine annotated
  `kairon.zyvor.dev/preemption-policy=Halt` can be halted by a
  higher-priority pending Machine and is set back to `Running` when the
  preemptor goes away (see [`preemption.md`](preemption.md)).
- **Only a single-hop resume back to `Running` is supported.** Going
  straight from `Halted` to `Paused` isn't handled directly -- FluxVM's
  own `Pause` requires the VM to already be running, so set
  `powerState: Running` first, then `Paused`, the same two-step
  convention `Paused` -> `Running` -> `Paused` already implies elsewhere.
- **No timeout or auto-resume.** A Machine can stay `Halted` indefinitely;
  nothing here ever resumes it automatically.
- **Interaction with migration is untested**, the same caveat
  [`machine-pause-resume.md`](machine-pause-resume.md) already carries for
  `Paused` -- cold migration always uses `Stopped`, never `Halted` (moving
  nodes needs a genuinely new runtime on the target, not FluxVM's kept
  per-node record), and there's no admission-time guard against halting a
  Machine mid-migration.
