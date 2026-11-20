# User guide: pause and resume (`spec.powerState: Paused`)

Suspend a running Machine's guest CPUs without tearing anything down --
Kairon's equivalent of `virtctl pause`/`unpause`. Distinct from `Stopped`
(which deletes the FluxVM runtime entirely, freeing its RAM) and from
[guest quiesce](machine-snapshot-quiesce.md) (which freezes the guest's
own *filesystem* I/O via qemu-guest-agent, not its CPUs, and needs guest
cooperation).

## What this does

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: db}
spec:
  powerState: Paused   # was Running
  # ... unchanged otherwise
```

Or via `kaironctl pause db` / `kaironctl resume db` (`kubectl kairon pause
db` works identically), or the dashboard's **Pause**/**Resume** buttons.

Pausing calls FluxVM's real QMP `stop` (or the Cloud Hypervisor/
Firecracker equivalent -- **backend-agnostic**, unlike CPU/memory
hotplug's QEMU-only restriction) to suspend guest CPU execution. RAM,
device state, and network attachment all stay fully resident and
unchanged -- resuming (`spec.powerState: Running`, `kaironctl resume`, or
the dashboard button) continues exactly where the guest left off, with no
boot, no re-attach, nothing lost. `status.phase` reports `Paused` while
suspended.

## Real limits today (first cut)

- **Requires an existing runtime.** Setting `powerState: Paused` on a
  Machine that's never been realized against FluxVM (no prior `Running`
  state) doesn't create one paused-from-the-start -- `status.phase` stays
  `Pending` with a clear message instead. Set `powerState: Running` first,
  let it actually start, then pause it.
- **A Paused Machine still counts against the scheduler's per-node
  capacity**, the same as `Running` -- unlike `Stopped` (which tears the
  runtime down and genuinely frees the node), a paused VMM process and
  its resident RAM are still there. Don't expect pausing a Machine to
  make room for another one on the same node.
- **No timeout or auto-resume.** A Machine can stay `Paused` indefinitely;
  nothing here ever resumes it automatically.
- **Interaction with migration/hotplug is untested.** Pausing a Machine
  mid-migration, or attempting `spec.resources` hotplug while paused, is
  real FluxVM/QMP behavior this guide doesn't make any claims about --
  treat it as unsupported until verified, not silently safe.
- **No admission-time guard against pausing a Machine mid-migration** --
  `spec.powerState` and `MachineMigration` are reconciled independently;
  changing one while the other is mid-flight is between you and FluxVM's
  own QMP semantics, not something Kairon checks first.
