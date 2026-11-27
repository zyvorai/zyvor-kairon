# Scaling a MachineSet

```bash
kubectl scale machineset NAME --replicas=N
```

Or with kaironctl (planned):

```bash
kaironctl machineset scale NAME N
```

The controller respects `maxSurge` and any MachineDisruptionBudget.
Paused MachineSets do not scale until unpaused.
