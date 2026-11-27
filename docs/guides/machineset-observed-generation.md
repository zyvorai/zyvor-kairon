# MachineSet observedGeneration

MachineSet status now includes `observedGeneration`. The controller sets it to the MachineSet's `metadata.generation` on every successful reconcile.

Operators can compare `status.observedGeneration` with `metadata.generation` to know whether the controller has processed the latest spec change.

The field is declared in the MachineSet CRD status schema (v1, v1beta1 and v1alpha1), so the API server keeps the value rather than pruning it.
