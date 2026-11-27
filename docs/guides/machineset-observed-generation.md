# MachineSet observedGeneration

MachineSet status now includes `observedGeneration`. The controller sets it to the MachineSet's `metadata.generation` on every successful reconcile.

Operators can compare `status.observedGeneration` with `metadata.generation` to know whether the controller has processed the latest spec change.
