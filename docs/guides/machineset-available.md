# MachineSet Available condition

The MachineSet controller sets an `Available` condition on every reconcile.

- `Available=True` (reason `MinimumReplicasAvailable`) when at least the desired number of replicas are `Phase=Running`.
- `Available=False` (reason `MinimumReplicasUnavailable`) otherwise. The message reports how many are ready, for example `1 of 3 ready`.

This is observational only. It does not change scale or rollout behavior.
