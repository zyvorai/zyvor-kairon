# User guide: MachineDisruptionBudget

Caps how many Machines `kaironctl evacuate` is willing to disrupt at once.

## Why this exists, and what it isn't

By default, `node.spec.unschedulable` only blocks *new* placements --
nothing moves already-running Machines off a node being decommissioned just
because it's cordoned, the same as real Kubernetes (`kubectl cordon` alone
never evicts anything either). The operator-invoked action that actually
moves Machines off a node is `kaironctl evacuate NODE`, which creates a
`MachineMigration` for each one. `MachineDisruptionBudget` makes `evacuate`
throttle itself against those, instead of firing everything simultaneously;
`evacuate --wait` (below) additionally keeps retrying until the node is
actually empty.

**Opt-in exception**: `controller.cordonEvacuation.enabled` (off by
default) makes `kairon-controller` itself do this automatically -- every
reconcile tick, any Machine on a newly-cordoned node gets the same
`MachineDisruptionBudget`-throttled `MachineMigration` `evacuate` would
create, mirroring KubeVirt's `LiveMigrateIfPossible` eviction strategy.
See "Automatic cordon-triggered evacuation" below.

**Enforcement is still client-side by default, not a server-side admission
guarantee.** With the admission webhook below disabled (the default),
nothing stops a `MachineMigration` created directly through the Kubernetes
API (bypassing `kaironctl evacuate`) from ignoring it entirely. Treat it
the same way you'd treat any other `kaironctl`-enforced convention -- real
for anyone using the CLI as intended, not a hard multi-tenant guarantee --
unless you enable the webhook.

`kairon-controller` *does* now reconcile this object on a timer, but only
for `status` -- see "Status" below. That reconciliation is purely
observational; it doesn't change the enforcement story above at all.

### Admission webhook (`webhook.enabled`)

An opt-in validating admission webhook on `kairon-controller`
(`webhook.enabled`, off by default -- see the Helm chart's `webhook` values
and SECURITY.md) closes the direct-API bypass above: it rejects a
`MachineMigration` create outright if disrupting its target Machine would
violate a matching budget, reusing the exact same `LoadBudgetStates`/
`AdmitDisruption` decision `kaironctl evacuate` already makes
(`internal/controller/disruption.go`) rather than a second implementation.
It only evaluates `CREATE` -- a `MachineMigration`'s target never changes
after creation, so there's nothing for `UPDATE` to re-check. Requires an
operator-supplied TLS certificate (`webhook.tlsSecretName`,
`webhook.caBundle`); like `migration.tlsSecretName`, this chart doesn't
mint one for you.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineDisruptionBudget
metadata:
  name: web-tier
spec:
  selector: {tier: web}
  minAvailable: "2"      # or maxUnavailable: "1" -- exactly one of the two
```

`minAvailable`/`maxUnavailable` are a plain integer or a `"N%"` string,
evaluated against however many Machines currently match `selector` --
same shape and rounding (percentages round up) as a real Kubernetes
`PodDisruptionBudget`.

Or, via `kaironctl`:

```console
$ kaironctl create budget web-tier --selector tier=web --min-available 2
budget/web-tier created
$ kaironctl edit budget web-tier --min-available 3
budget/web-tier updated
```

`--selector` is repeatable for a multi-label selector. `create` requires
exactly one of `--min-available`/`--max-unavailable` -- mirroring
`DesiredHealthy`'s own "exactly one, not both or neither" contract above,
so a budget that would fail that check on every future reconcile tick is
never created in the first place. `edit` only patches the flags you
actually pass, and also refuses both bound flags in the same call; passing
`--selector` on its own `edit` call replaces the entire selector map, not a
per-key merge. Switching an existing budget from `minAvailable` to
`maxUnavailable` (or back) still needs a follow-up `kubectl`/YAML edit to
clear the old field -- a plain merge patch can set a new field but can't
also unset a different one in the same `edit` call, a real first-cut limit
of this verb.

## Status

Every reconcile tick, `kairon-controller` computes each budget's
`status.expectedMachines`/`currentHealthy`/`desiredHealthy`/
`disruptionsAllowed` from the exact same `LoadBudgetStates` logic
`evacuate`/the admission webhook use, and patches it onto the object --
`kubectl get mdb`/`kaironctl get budgets` now report real numbers instead
of an empty `{}` -- as does `kairon-ui`'s own "Disruption budgets" dashboard
page (read-only, `GET /api/v1/disruption-budgets`). This is purely
observational: `evacuate` and the
webhook still each recompute their own allowance fresh at decision time
rather than trusting this status, since it can be up to one reconcile
interval (`controller.interval`, 5s default) stale -- a real-time decision
should never be made against a cached number that old.

```
$ kaironctl get budgets
NAME       MINAVAILABLE  MAXUNAVAILABLE  EXPECTED  HEALTHY  DESIRED  ALLOWED
web-tier   2             -               3         3        2        1
```

## Previewing who counts right now (`kaironctl describe`)

`describe` for almost every other kind in this project prints the raw
object as JSON and nothing else. `kaironctl describe budget` is one of only
five deliberate exceptions (the others are [`kaironctl describe
snapshotschedule`](machine-snapshot-schedules.md#previewing-what-would-fire-right-now-kaironctl-describe),
[`kaironctl describe migrationpolicy`](migration-policies.md#previewing-what-a-migration-would-get-right-now-kaironctl-describe),
`kaironctl describe quota`, see
[machine-quotas.md](machine-quotas.md#previewing-usage-right-now-kaironctl-describe),
and `kaironctl describe machine`, which only appends a one-line
`spec.tenant` note when that field is set).
Unlike `MachineQuota`'s status, this object's `status.expectedMachines`/
`currentHealthy`/`desiredHealthy`/`disruptionsAllowed` are already four
clearly-labeled, already-resolved counts in the same unit (Machines) --
reading the raw JSON tells you the *numbers* perfectly well on its own. What
it can't tell you is *which* Machines are counted, and specifically *why*
any of them isn't currently healthy:

```console
$ kaironctl describe budget web-tier
{
  "apiVersion": "kairon.zyvor.dev/v1",
  "kind": "MachineDisruptionBudget",
  ...
}

Status (recomputed fresh from Machines/MachineMigrations right now): 3 matching, 2 healthy, 2 desired healthy, 0 disruptions allowed
Matching machines (3):
  web-1                    healthy
  web-2                    healthy
  web-3                    NOT healthy (non-terminal MachineMigration in flight)
```

Each non-healthy Machine says whether it's because `status.phase` isn't
`Running`, or because a non-terminal `MachineMigration` already has it in
flight -- the same "don't just report a number, show the receipts"
precedent the other `describe` exceptions already established (the
`quota` preview's counted-Machines list, and the `snapshotschedule`/
`migrationpolicy` previews' own matching-Machines lists).

Status here is recomputed fresh via the same `LoadBudgetStates` function
`kaironctl evacuate` and the opt-in admission webhook both call to decide
the same thing -- not read back from `status.*`, which (like every other
reconciled status on this page) can be up to one reconcile interval stale.
A selector matching zero Machines prints the same explicit hint every other
`describe` exception uses instead of an empty, unexplained list.

## Prometheus metrics and alerting

Until now, this status was only visible by polling `kubectl get mdb`/
`kaironctl get budgets` (or the dashboard) -- there was no proactive
signal that a budget had dropped to 0 disruptions allowed until an
`evacuate`/webhook-admitted disruption was actually refused.
`kairon-controller` now also exposes
`kairon_disruption_budget_status{namespace, budget, field}` (`field` is
`expected_machines`/`current_healthy`/`desired_healthy`/
`disruptions_allowed`), the exact same `status.*` values above, recorded
once per `Reconcile` right alongside that status patch so the two can
never disagree. `charts/kairon/alerts.yaml`'s new
`kairon-disruption-budgets` group (`KaironDisruptionBudgetExhausted`)
fires once `disruptions_allowed` has stayed at `0` for 10 minutes on any
namespace/budget -- `severity: info`, since a budget sitting at 0 is
often working-as-intended (mid rolling node drain) rather than a problem.
See `docs/guides/observability.md` for which component(s) expose which
metrics overall -- this one is `kairon-controller`-only, mirroring the
reconcile loop's own cluster-wide `ListMachineDisruptionBudgets`/
`LoadBudgetStates` visibility (neither `kairon-node` nor `kairon-ui` has
an equivalent view to report from).

## How `evacuate` uses it

For every Machine on the node being evacuated:

1. Find every `MachineDisruptionBudget` whose `selector` matches that
   Machine's labels.
2. For each one, count `total` (all Machines cluster-wide matching that
   selector) and `currentHealthy` (`status.phase == Running` **and** not
   already targeted by a non-terminal `MachineMigration` -- so a disruption
   already in flight is never counted as available headroom twice).
3. If `currentHealthy - desiredHealthy <= 0` for any matching budget, that
   Machine is skipped (reported as `machine ns/name skipped: ...`, not
   silently dropped) rather than migrated.
4. Queuing a Machine spends one allowance from every budget it matches, for
   the rest of that one `evacuate` pass -- recomputed from scratch on each
   invocation, nothing is persisted.

`kaironctl evacuate` exits non-zero if anything was skipped, so it's safe to
script/alert on -- unless you pass `--wait` (below), which only exits
non-zero on an actual timeout.

### `--wait`: retrying until the node is actually empty

Plain `evacuate` makes exactly one pass: anything `MachineDisruptionBudget`
blocks that tick is skipped for good, left on the node, reported, and
`evacuate` exits -- even though that Machine might become disruptable a
minute later, once an earlier migration from the same pass actually
finishes and frees up the budget's headroom again.

```
kaironctl evacuate worker-3 --wait [--timeout 15m] [--poll-interval 10s]
```

`--wait` re-runs the same pass on `--poll-interval` (default 10s) until
every Machine has actually left the node (`spec.nodeName != worker-3`) or
`--timeout` (default 15m) elapses, matching `kubectl drain`'s own retry
model. This remains an **operator-invoked** action, not something that
starts on its own the moment `node.spec.unschedulable` is set -- unless
`controller.cordonEvacuation.enabled` is also on (see below), the same
"operator attests, Kairon then follows through automatically" shape as
`kaironctl fence`/`recover`.

Each pass also now skips (never duplicates) a Machine that already has a
non-terminal `MachineMigration` in flight -- a real gap `--wait` needed
closed to avoid creating a second, conflicting migration for the same
Machine on its next retry, and one plain (non-`--wait`) `evacuate` shares
the fix too.

## Automatic cordon-triggered evacuation

`controller.cordonEvacuation.enabled` (Helm value; `-cordon-evacuate` on
the `kairon-controller` binary, off by default) makes `kairon-controller`
itself create a `MachineMigration` for every Machine on a Node whose
`spec.unschedulable` transitions to true -- no `kaironctl evacuate`
invocation needed, mirroring KubeVirt's `LiveMigrateIfPossible` eviction
strategy. It reuses the exact `MachineDisruptionBudget` check `evacuate`
and the admission webhook already share (`internal/controller/cordon.go`):

- A Machine a budget currently blocks is skipped, not force-migrated --
  the same throttling `evacuate` already does, just triggered by the
  cordon itself instead of an operator running a command.
- A blocked or already-attempted Machine is retried at most once per
  `controller.cordonEvacuation.minRetryInterval` (default `2m`), recorded
  via the `kairon.zyvor.dev/cordon-evacuate-attempted-at` annotation --
  otherwise a controller reconciling every few seconds would attempt a
  new migration every tick.
- `controller.cordonEvacuation.strategy` (default `cold`) is `cold` or
  `auto` -- deliberately never `live`: automatic *live* migration
  triggered with no operator watching is a bigger step than automatic
  cold, so it isn't offered here.
- Turning this on requires the `kairon-controller` ServiceAccount to be
  granted `create` on `machinemigrations` -- the Helm chart adds this
  automatically when `controller.cordonEvacuation.enabled` is true, since
  today only `kaironctl`/`kairon-ui` ever create one.
- A node in maintenance mode (`kairon.zyvor.dev/maintenance=true`) is left
  to maintenance, which applies the same budget check with its own
  strategy and fallback -- see
  [node maintenance mode](node-maintenance-drs.md).

## Real limits today (v1 of this feature)

- Client-side only unless `webhook.enabled` (see above) -- a
  `MachineMigration` created some other way isn't gated at all.
- `status` is observational only, not authoritative for any real-time
  decision (see "Status" above) -- up to one reconcile interval stale, and
  reconciling it is best-effort (a patch failure is logged, not retried
  until the next tick).
- A Machine matching zero `MachineDisruptionBudget`s is never blocked --
  budgets are opt-in per selector, not a cluster-wide default.
- No background/automatic drain triggered by `node.spec.unschedulable`
  unless `controller.cordonEvacuation.enabled` is turned on (see
  "Automatic cordon-triggered evacuation" above) -- off by default,
  `evacuate --wait` remains an explicit operator action to start.
- `--wait`'s timeout is wall-clock, not "N more retries" -- a
  `MachineDisruptionBudget` that can genuinely never be satisfied (e.g.
  `minAvailable` higher than the selector will ever match) just retries
  uselessly until `--timeout` regardless of how the budget is
  misconfigured; `evacuate --wait` has no way to detect that case and
  fail fast instead.
