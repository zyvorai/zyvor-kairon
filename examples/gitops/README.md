# GitOps health checks for Kairon

Argo CD and Flux decide whether a synced resource is healthy by looking at
its status. Neither knows Kairon's `status.phase` values, so without these
files a `Machine` stuck in `Failed`, or a `MachineMigration` parked in
`NeedsRecovery`, shows up as Healthy/Current and a rollout reports success.

| File | What it is |
|---|---|
| [`argocd-cm-health.yaml`](argocd-cm-health.yaml) | `argocd-cm` keys `resource.customizations.health.kairon.zyvor.dev_<Kind>` with Lua health checks |
| [`flux-kustomization.yaml`](flux-kustomization.yaml) | A Flux `Kustomization` with `healthChecks` and CEL `healthCheckExprs` (Flux 2.5+) |
| [`argocd-application.yaml`](argocd-application.yaml) | An Argo CD `Application` installing the chart with the observability values on |

## Argo CD

Merge `argocd-cm-health.yaml` into your `argocd-cm` (or put the keys under
`configs.cm` in the argo-cd Helm chart values), then restart or wait for
the application controller to pick up the change.

Kinds covered and how a status maps:

| Kind | Healthy | Progressing | Degraded | Suspended |
|---|---|---|---|---|
| `Machine` | `Running` | `Pending`, `Starting`, `Stopping`, `Restarting`, `Freezing`, `Thawing`, `Adopting`, no phase yet | `Failed`, `Blocked`, or a `NodeUnreachable=True` condition | `Stopped`, `Paused`, `Hibernated`, `Halted` |
| `MachineMigration` | `Succeeded` | any other phase | `Failed`, `Cancelled`, `NeedsRecovery` | |
| `MachineSnapshot` | `readyToUse: true` or `Succeeded` | everything else | `Failed` | |
| `MachineSnapshotRestore`, `MachineBackup`, `MachineBackupRestore` | `Succeeded` | everything else | `Failed` | |
| `MachineExport` | `Ready`, `Expired` | everything else | `Failed` | |
| `MachineProtectionPlan` | `Active`, `Standby` | `FailingOver`, everything else | `Invalid` | |
| `MachineImage` | `Ready` | everything else | `Failed` | |
| `MachineSet` | `readyReplicas`, `updatedReplicas` and `replicas` all equal `spec.replicas` | otherwise | | |

A stopped or hibernated Machine is `Suspended`, not `Degraded`: it is the
state somebody asked for. Change the mapping in the Lua if you want a
stopped Machine to count as Healthy in your setup.

## Flux

`flux-kustomization.yaml` uses `spec.healthCheckExprs`, which needs
Flux 2.5 or newer. `healthChecks` names the objects to wait for; the CEL
expressions say what `current`, `inProgress` and `failed` mean for each kind.

## What was and was not verified

- The Argo CD Lua was run under LuaJIT (Lua 5.1 semantics, as in Argo CD)
  against 27 hand-written status objects, one table of expected results per
  kind. It was not loaded into a real Argo CD.
- The phase names come from this repository's Go constants and CRD
  descriptions, not from objects read off a running cluster. The CRDs declare
  `status.phase` as a free string, so a phase this list does not know falls
  back to `Progressing`.
- The Flux CEL and the Flux and Argo CD YAML were checked for YAML syntax
  only. Flux's `healthCheckExprs` was not evaluated by a real Flux.
- `argocd-application.yaml` pins `0.10.6`, which does not yet contain the
  `metrics.serviceMonitor` and `metrics.grafanaDashboards` values; use the
  first release that does, or a chart built from this repository.
