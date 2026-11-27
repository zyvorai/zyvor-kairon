# MachineSet events

The MachineSet controller emits Kubernetes events when the desired replica count changes.

- `ScaledUp` when the desired count increases
- `ScaledDown` when the desired count decreases

Events appear in `kubectl describe machineset NAME` under Events. They are observational and do not change rollout behavior.
- Pause/resume events
