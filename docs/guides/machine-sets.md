# User guide: MachineSet

A Deployment/ReplicaSet-shaped abstraction over `Machine`: `kairon-controller`
creates/deletes plain `Machine` objects to match `spec.replicas`, each built
from `spec.template` -- so a fleet of identical Machines doesn't mean
hand-creating each one and hand-tracking template drift yourself.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineSet
metadata:
  name: web
  namespace: prod
spec:
  replicas: 3
  strategy: RollingUpdate
  maxUnavailable: 1
  template:
    labels:
      app: web
    spec:
      image: {path: /var/lib/fluxvm/images/web.qcow2}
      resources: {cpu: "2", memory: "4Gi"}
      runtime: {backend: qemu}
      powerState: Running
```

Or, for the flag-friendly fields above, `kaironctl create`:

```console
$ kaironctl create machineset web --image /var/lib/fluxvm/images/web.qcow2 \
    --cpu 2 --memory 4Gi --replicas 3 --max-unavailable 1 --label app=web
machineset/web created
$ kaironctl scale machineset web --replicas 5
machineset/web scaled to 5 replicas
$ kaironctl edit machineset web --strategy Recreate --max-unavailable 2
machineset/web updated
$ kaironctl edit machineset web --paused=true
machineset/web updated
```

`create machineset` reuses the exact same Machine-spec flags `kaironctl
create` (a single Machine) already has for `spec.template.spec` -- anything
those flags don't cover (`spec.placement`, device claims, security,
per-volume claims) still needs `kubectl apply`/YAML, the same limit a plain
`create machine` already has for those fields. `scale` (replicas) and `edit`
(`--strategy`/`--max-unavailable`/`--paused`) are the only mutations these verbs
support beyond create -- editing the template itself still means `kubectl
edit`/`apply`.

`scale` also supports scaling every `MachineSet` a label selector matches to
the same replica count in one call (`kaironctl scale machineset --selector
env=staging --replicas 0`) instead of a single `NAME` -- see
[`kaironctl-scale-selector.md`](kaironctl-scale-selector.md).

`spec.template.spec` is exactly a `Machine`'s own `spec` -- anything a
hand-created `Machine` accepts (`image`, `volumes`, `deviceClaims`,
`network`, `cloudInit`, ...) works here unchanged. `spec.template.labels`
are applied to every replica in addition to (never replacing) the two
labels `kairon-controller` itself always adds:
`kairon.zyvor.dev/machineset=<name>` and
`kairon.zyvor.dev/machineset-template-hash=<hash of the current template>`.

Every replica is a completely ordinary `Machine` object once created --
`kubectl get machines`, `kaironctl evacuate`, `MachineDisruptionBudget`
selectors (match on `spec.template.labels`, e.g. `{app: web}`, to throttle
disruption across a MachineSet's own replicas the same way they'd throttle
any other Machines), and everything else that already operates on Machines
keeps working exactly as it does today. `kaironctl get machinesets` (or
`kubectl get machinesets`) lists the `MachineSet`s themselves --
`STRATEGY`/`REPLICAS`/`READY`/`UPDATED`/`PAUSED` at a glance, without having to
separately count matching Machines by hand -- `kairon-ui`'s own "Machine
sets" dashboard page (read-only, `GET /api/v1/machinesets`) shows the same
thing.

## Deleting a MachineSet deletes its replicas too

`kubectl delete machineset`/`kaironctl delete machineset` deletes every
Machine it owns along with it -- Kairon has no Kubernetes
ownerReference/garbage-collection mechanism to do this for free (no
`client-go` dependency anywhere in this project to drive one), so
`kairon-controller` does it explicitly: a `kairon.zyvor.dev/machineset-cleanup`
finalizer holds the `MachineSet` object in Kubernetes until every Machine
still carrying its `kairon.zyvor.dev/machineset` label is actually gone
(not merely deletion-requested -- each Machine's own runtime-cleanup
finalizer keeps it around for a few more ticks while `kairon-node` tears
down its FluxVM VM). A single stuck Machine delete leaves the `MachineSet`
in `Terminating` and is retried every tick from a fresh Machine listing,
the same fail-closed shape `NetworkSecurityGroup`/`MachineNetworkPolicy`
deletion already uses.

## How replicas are reconciled

Every reconcile tick, for each `MachineSet`:

1. Find its owned Machines (`kairon.zyvor.dev/machineset` label match,
   excluding any already being deleted).
2. Split them into **current** (template hash matches `spec.template`
   right now) and **outdated** (an earlier template hash -- meaning
   `spec.template` changed since they were created).
3. If the total (current + outdated) is below `spec.replicas`, create
   enough current-template replicas to fill the gap -- this always takes
   priority over any rollout concern, whether the shortfall is a fresh
   scale-up or a replica that died on its own.
4. Otherwise, advance the rollout by exactly one bounded step (see below),
   never the whole thing in one tick -- a MachineSet rollout is always
   paced by, and visible across, real reconcile intervals.

`status.replicas`/`.readyReplicas`/`.updatedReplicas` are patched every
tick from this same pass -- `readyReplicas` counts current-template
replicas whose `status.phase` is `Running`; `updatedReplicas` counts every
current-template replica regardless of phase.

## Metrics and alerts

`kairon-controller`'s `/metrics` also exposes
`kairon_machineset_status{namespace, machineset, field}`, one gauge per
`field` (`replicas`/`ready_replicas`/`updated_replicas`) -- mirroring
`kube-state-metrics`' own `kube_replicaset_status_replicas`/
`kube_replicaset_status_ready_replicas`/`kube_deployment_status_replicas_updated`
gauges, collapsed into a single vector the same way `kairon_quota_resource`/
`kairon_disruption_budget_status` already collapse their own dimensions.
Recorded once per `Reconcile` tick (`internal/metrics.Recorder.ObserveMachineSets`,
called from `reconcileMachineSets` above) from the exact same tally that
tick's `status` patch uses -- never a second, independently-computed number
that could drift from what `kubectl get machineset` shows for the same
tick. `kairon-controller`-only, for the same reason `kairon_quota_resource`
is: `kairon-node` never lists `MachineSet`s cluster-wide (it only
reconciles individual `Machine`s already assigned to it) and `kairon-ui`
has no reconcile loop at all, so neither has a meaningful per-tick tally to
report here. See `docs/guides/observability.md`.

`charts/kairon/alerts.yaml`'s `kairon-machinesets` group adds
`KaironMachineSetRolloutStuck`: `ready_replicas` below `replicas` for over
30 minutes on some `MachineSet`, the same "sustained gap, not a transient
blip" 30-minute threshold `KaironMigrationStuckInFlight` already uses for
an in-flight migration. Previously the only signal a rollout had stalled
was polling `kaironctl get machinesets`/`kubectl get machinesets` and
noticing `READY` never catches up to `REPLICAS` -- this fires proactively
instead. `severity: warn`, pointed at this page rather than a dedicated
runbook (check `kubectl get machines -l kairon.zyvor.dev/machineset=<name>`
for the specific stuck replica, per the "Real limits today" section below).

## Rollout strategy

- **`RollingUpdate`** (the default): replaces outdated replicas while
  respecting both `maxUnavailable` and `maxSurge`. With `maxSurge` > 0 the
  controller may create new (current-template) replicas first, up to
  `replicas + maxSurge` total, before deleting old ones. Without surge the
  classic behavior is preserved: delete only when it would not drop
  available below `replicas - maxUnavailable`, then create replacements on
  a later tick. A current-template replica only counts as available once
  its `status.phase` is `Running`. Both fields accept an integer or a
  percentage string (same parsing as `MachineDisruptionBudget`);
  `maxUnavailable` defaults to `1`, `maxSurge` defaults to `0`.
- **`Recreate`**: terminates every outdated replica first, and creates no
  replacements until none remain -- matches a real Kubernetes
  `Deployment`'s own `Recreate` strategy (full stop, then restart) exactly.
  Simpler than `RollingUpdate`, at the cost of a window with fewer than
  `replicas` Machines running during every template change.

Changing `spec.template` never touches an already-running replica directly
-- it only changes what *new* replicas look like; existing ones are
replaced (not edited in place) by the rollout logic above.

## Pausing a MachineSet

`spec.paused: true` freezes a MachineSet: kairon-controller creates and
deletes no replicas for it, whether for scale-up, scale-down or a
template rollout. Existing replicas keep running untouched.

```console
$ kaironctl edit machineset web --paused=true     # or create ... --paused
$ kubectl get machinesets
NAME   REPLICAS   READY   UPDATED   STRATEGY        PAUSED   AGE
web    3          3       3         RollingUpdate   true     2d
$ kaironctl edit machineset web --paused=false    # resume
```

While paused:

- `spec.replicas` and `spec.template` can still be edited (including by
  HPA/KEDA through `/scale`); the change is recorded but not acted on.
  On resume the controller steps toward whatever `spec` says then, with
  the usual `maxUnavailable`/`maxSurge` pacing.
- `status` is still refreshed every tick: replica counts, `status.paused`,
  and a `Paused` condition (`True`, reason `Paused`; `False`, reason
  `NotPaused`) whose `lastTransitionTime` is when pause last flipped. So
  `kubectl wait machineset/web --for=condition=Paused=false` works.
- Deleting the MachineSet still deletes its replicas (pause doesn't block
  the deletion cascade).
- A replica that dies is not replaced until you resume.

The `PAUSED` column in `kubectl get` reads `spec.paused` (shows your
intent immediately); `status.paused` is the controller's echo of it.

## Real limits today (first cut)

- **Gated on `status.phase == Running`, not on deeper application health.**
  `RollingUpdate` now withholds further replacement until each already-
  created current-template replica reaches `Running` (see above) -- but
  that's Kairon's own coarse VM-power-state signal, not a guest-level
  readiness probe (nothing here waits for a guest agent heartbeat, an HTTP
  health check, or any other application-defined signal). A template with
  a boot failure severe enough to never reach `Running` correctly stalls
  the rollout in place rather than cycling every replica through it; one
  that boots fine but is application-broken is not caught here.
- **`maxSurge` is best-effort.** Extra replicas are created during a
  rollout up to the surge limit, but the scheduler still decides placement.
  A surge Machine that cannot be placed stays `Pending`; the rollout does
  not fail closed on it.
- **Autoscaling is external to the MachineSet itself.** The CRD exposes a
  `/scale` subresource (`status.selector` is written by the controller), so
  `kubectl scale`, HPA (with prometheus-adapter) and KEDA can drive
  `spec.replicas`; the fleet `MachineAutoscaler` can too. The MachineSet
  controller only reconciles toward whatever `spec.replicas` says. See
  [machineset-autoscaling.md](machineset-autoscaling.md).
- **Pause, but no automatic undo.** `spec.paused` freezes a rollout
  partway (see [Pausing a MachineSet](#pausing-a-machineset)); to roll
  back, edit `spec.template` back to the previous shape yourself.
- **Replica names are randomly suffixed** (`<machineset-name>-<8 hex
  chars>`), not sequential/predictable -- don't script against a specific
  replica's name surviving a rollout.
