# MachineSet conditions

The MachineSet controller sets these status conditions on every reconcile:

| Condition | True means | Reason |
|-----------|------------|--------|
| Progressing | Any replica is still on an older template, or the desired count has not been reached | Updating |
| Available | At least the desired number of replicas are Phase=Running | MinimumReplicasAvailable |
| ReplicaFailure | One or more owned Machines are Phase=Failed | FailedReplicas |
| Paused | spec.paused is true; no replicas are created or deleted | Paused |
| ScaleDownBlocked | A MachineDisruptionBudget is holding back a scale-down | DisruptionBudget |

All are observational except ScaleDownBlocked, which gates deletion of Running replicas.

`kaironctl machineset status NAME` prints them in one table.
