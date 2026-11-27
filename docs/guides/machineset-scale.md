# Scaling a MachineSet

```bash
kubectl scale machineset NAME --replicas=N
```

The controller respects `maxSurge` and any MachineDisruptionBudget.
Paused MachineSets do not scale until unpaused.
