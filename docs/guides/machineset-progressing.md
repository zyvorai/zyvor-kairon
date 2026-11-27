# MachineSet Progressing condition

The MachineSet controller sets a `Progressing` condition on every reconcile.

- `Progressing=True` (reason `Updating`) while any owned Machine is still on an older template, or while the desired replica count has not yet been reached.
- `Progressing=False` (reason `Complete`) when every replica is on the current template.

The condition message reports how many replicas are updated, for example `2 of 3 replicas updated`.

This is observational only. It does not change rollout behavior.
