# MachineSet events

kairon-controller posts real core/v1 Events on the MachineSet itself
(`involvedObject` is the MachineSet, `source` is `kairon-controller`). They
show up in `kubectl describe machineset NAME` and `kaironctl events -n
<namespace>`. They are observational and do not change rollout or scale
behavior.

| Reason | Type | When |
| --- | --- | --- |
| `ScaledUp` | Normal | A replica was created (scale-up toward `spec.replicas`, or a rollout surge). Message: `created machine <name>, replicas a->b`. |
| `ScaledDown` | Normal | A replica was deleted (scale-down, or a rollout / Recreate replacement). Message: `deleted machine <name>, replicas a->b`. |
| `ScaleDownBlocked` | Warning | A scale-down was held back by a `MachineDisruptionBudget`. Emitted only when the `ScaleDownBlocked` condition turns `True`, not every tick it stays `True`. |
| `Paused` | Normal | `spec.paused` flipped to `true` (`status.paused` was `false`). |
| `Resumed` | Normal | `spec.paused` flipped to `false` (`status.paused` was `true`). |

A MachineSet sitting at its desired replica count, or left paused, does
not emit another Event each reconcile. Event failures are logged and never
stall a scale or rollout.

An earlier cut posted a single `ScaledUp` / `ScaledDown` Event whenever
`status.replicas` differed from `spec.replicas`. That fired every tick
while a scale was still in flight or blocked by a budget, skipped
replacement of a dead replica (desired unchanged), and never fired for
scale-to-zero. The events above replace that: one Event per Machine
actually created or deleted, plus pause/resume on transition only.

See also [machine-sets.md](machine-sets.md) and
[machineset-conditions.md](machineset-conditions.md).
