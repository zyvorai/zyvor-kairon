# Preemption

`spec.priority` still orders pending Machines. It halts a running Machine only when both sides opt in.

On the pending Machine:

```yaml
metadata:
  annotations:
    kairon.zyvor.dev/preempt: "true"
spec:
  priority: 10
```

On a Machine that may be halted:

```yaml
metadata:
  annotations:
    kairon.zyvor.dev/preemption-policy: Halt
spec:
  priority: 0
```

Rules:

- Same namespace. A victim in another namespace is ignored.
- Victim priority must be strictly lower.
- Victim must already be scheduled and desired Running.
- `Pause` is rejected. A Paused Machine still counts in node load, so it would not free the capacity the preemptor is waiting on. Halted does, and it also drops out of MachineQuota.
- One victim per preemptor per tick. The lowest priority eligible victim wins. Ties break by name.
- A non-terminal MachineMigration or a MachineDisruptionBudget with no allowance refuses the victim.
- The controller writes `kairon.zyvor.dev/preempted-by` and restores `powerState: Running` when the preemptor is gone, Halted, Stopped, or no longer pending and unscheduled.

## Annotations

| Annotation | On | Set by | Meaning |
| --- | --- | --- | --- |
| `kairon.zyvor.dev/preempt` | pending Machine | you | `"true"` lets this Machine halt a lower-priority victim. Any other value is off. |
| `kairon.zyvor.dev/preemption-policy` | candidate victim | you | `Halt` (or `Halted`) allows the Machine to be halted. Anything else means it is never a victim. |
| `kairon.zyvor.dev/preempted-by` | victim | kairon-controller | `namespace/name` of the preemptor that halted it. A Machine with this set is not chosen as a victim again, and it is what the controller looks for when deciding to resume. |
| `kairon.zyvor.dev/preempted-power` | victim | kairon-controller | The `spec.powerState` the victim had before the halt (`Running`; an empty or missing value is read as `Running`). On resume the controller sets `powerState` back to `Running` and removes both `preempted-*` annotations. A halted Machine whose value is anything other than `Running` or empty is left halted. |

Do not set or edit `preempted-by` / `preempted-power` yourself. To resume a victim by hand, set `spec.powerState: Running` and remove both annotations.

Placement is retried on the next tick, after the halt is visible to the scheduler.

See `examples/preemption.yaml`.

## Limits

- Rejecting `Pause` (or any other value) for `preemption-policy` happens in the admission webhook. Without it, an unknown value is simply ignored: that Machine is never a victim.
- Halt is the only action. Nothing is evicted, deleted, or migrated, and a preemptor never takes a victim from another namespace.
- A resumed victim uses FluxVM's kept config, so CPU or memory hotplugged after its last create is lost (see [`machine-hotplug.md`](machine-hotplug.md)).
- The scheduler still decides placement. Halting a victim frees its capacity, but the preemptor can still fail placement for another reason (affinity, quota, taints) and the victim stays halted until the preemptor is deleted, halted, or stopped.

## Inspect

```bash
kubectl -n NS get machines -o custom-columns=NAME:.metadata.name,PRIO:.spec.priority,POWER:.spec.powerState,BY:.metadata.annotations.kairon\.zyvor\.dev/preempted-by
journalctl -u kairon-controller | grep -E 'preempted machine|resumed preempted machine|preempt refused'
```

Verified on a live k3s host on 2026-10-06: a pending priority-10 Machine halted an opted-in priority-0 Machine, deleting the preemptor resumed it, and without `preemption-policy: Halt` nothing was halted.
