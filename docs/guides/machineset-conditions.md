# MachineSet conditions

kairon-controller writes these conditions on every successful MachineSet
reconcile (`internal/controller/machineset.go` and `machineset_mdb.go`).
They are observational: none of them change scale or rollout behavior by
themselves. `status.conditions` is also the field `kubectl wait
--for=condition=...` reads.

To see them together with replica counts and generation:

```console
$ kaironctl machineset status web -n prod
NAME  NAMESPACE  DESIRED  READY  UPDATED  GENERATION  OBSERVED  PAUSED
web   prod       3        2      2        5           5         false

TYPE              STATUS   REASON                       MESSAGE                   AGE
Progressing       True     Updating                     2 of 3 replicas updated   3m
Available         False    MinimumReplicasUnavailable   2 of 3 ready              3m
ReplicaFailure    False    FailedReplicas               0 failed                  -
Paused            False    NotPaused                    -                         2h
ScaleDownBlocked  False    ScaleDownAllowed             -                         10m
```

The header is `spec.replicas` (DESIRED), `status.readyReplicas` (READY:
current-template Machines in `Phase=Running`), `status.updatedReplicas`
(UPDATED: current-template Machines of any phase), `metadata.generation`
versus `status.observedGeneration`, and `status.paused`. There is no
separate `availableReplicas` field; the Available condition uses READY.

The table always prints a row for each condition below, even if the
controller has not recorded it yet (`STATUS=Unknown`, the other columns
`-`). AGE is the time since `lastTransitionTime`; `-` means that field
was never set (see [AGE and lastTransitionTime](#age-and-lasttransitiontime)).

`-n` is the root namespace flag, the same as `kaironctl machineset history`.

## Progressing

Set on every successful reconcile after the create/delete step.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `Updating` | Any owned Machine is still on an older template, or the number of current-template replicas is below `spec.replicas`. Message: `N of M replicas updated`. |
| `False` | `Complete` | Every owned Machine is on the current template and the desired count has been reached. Message: `all replicas are on the current template`. |

While `spec.paused` is true the controller still refreshes this condition
but does not create or delete replicas, so Progressing stays `True` /
`Updating` if a rollout or scale is waiting on the pause. See
[Pausing a MachineSet](machine-sets.md#pausing-a-machineset).

## Available

Set on every successful reconcile. "Ready" here means a current-template
replica whose `status.phase` is `Running` — the same count as
`status.readyReplicas`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `MinimumReplicasAvailable` | At least `spec.replicas` current-template Machines are `Running`, and `spec.replicas` is greater than zero. Message: `N ready`. |
| `False` | `MinimumReplicasUnavailable` | Otherwise, including when `spec.replicas` is `0`. Message: `N of M ready`. |

## ReplicaFailure

Set on every successful reconcile. It counts every owned Machine whose
`status.phase` is `Failed`, including outdated ones.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `FailedReplicas` | One or more owned Machines are `Phase=Failed`. Message: `N failed`. |
| `False` | `FailedReplicas` | None are Failed. Message: `0 failed`. |

## Paused

Mirrors `spec.paused`. Written through `model.SetCondition`, so
`lastTransitionTime` is the moment pause actually flipped (not every
reconcile). `status.paused` is the same echo, and the `PAUSED` column in
`kubectl get machinesets` reads `spec.paused`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `Paused` | `spec.paused` is true. Message: `spec.paused is true; no replicas are created or deleted`. |
| `False` | `NotPaused` | `spec.paused` is false. No message. |

`kubectl wait machineset/web --for=condition=Paused=false` works against
this condition. Pause freezes creates and deletes only; deleting the
MachineSet still cascades to its replicas. See
[Pausing a MachineSet](machine-sets.md#pausing-a-machineset).

## ScaleDownBlocked

Set after the create/delete step. It applies only to a pure scale-down
of excess current-template replicas. RollingUpdate / Recreate replacement
of outdated replicas is paced by `maxUnavailable` / `maxSurge` and is not
gated by a `MachineDisruptionBudget`.

| Status | Reason | When |
| --- | --- | --- |
| `True` | `DisruptionBudget` | A matching budget in the namespace refused at least one healthy replica this tick. The message is the budget's reason. A Warning Event is recorded only when the condition turns True, not every tick it stays True. |
| `False` | `ScaleDownAllowed` | No scale-down was held back this tick (including when the MachineSet is not scaling down). No message. |

Held-back replicas are retried on the next reconcile tick. See
[MachineSet autoscaling](machineset-autoscaling.md#real-limits-today).

## AGE and lastTransitionTime

Paused and ScaleDownBlocked go through `model.SetCondition`, which stamps
`lastTransitionTime` when Status, Reason or Message change. Progressing,
Available and ReplicaFailure are written with a simpler upsert that does
not set `lastTransitionTime`, so their AGE column is often `-` today.
That is a display of what the controller wrote, not a bug in the command.

## See also

- [Machine sets](machine-sets.md) — rollout, pause, replica counts and `observedGeneration`
- [Template history and rollback](machineset-history.md)
- [CLI reference](../CLI.md)
