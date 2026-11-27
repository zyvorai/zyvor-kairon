# MachineSet ReplicaFailure condition

The MachineSet controller sets a `ReplicaFailure` condition on every reconcile.

- `ReplicaFailure=True` (reason `FailedReplicas`) when one or more owned Machines are `Phase=Failed`. The message reports the count, for example `1 failed`.
- `ReplicaFailure=False` otherwise.

This is observational only. It does not change scale or rollout behavior.
