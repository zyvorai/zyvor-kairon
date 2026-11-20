# User guide: MachineQuota

Caps how many Machines, and how much total CPU/memory, a namespace may have
scheduled at once -- Kairon's namespace-scoped equivalent of a Kubernetes
`ResourceQuota`.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineQuota
metadata:
  name: team-payments
  namespace: payments
spec:
  maxMachines: 20
  maxTotalCpu: "40"
  maxTotalMemory: "160Gi"
```

Each field is optional (unset means no cap on that dimension); set at least
one for the quota to do anything. Multiple `MachineQuota` objects in the
same namespace all apply independently -- same as multiple Kubernetes
`ResourceQuota` objects in one namespace, every one of them must be
satisfied.

Or, via `kaironctl`:

```console
$ kaironctl create quota team-payments --max-machines 20 --max-total-cpu 40 --max-total-memory 160Gi
quota/team-payments created
$ kaironctl edit quota team-payments --max-machines 30
quota/team-payments updated
```

`kaironctl create quota` refuses to create one with every dimension left
unset -- a `MachineQuota` capping nothing is never useful, so this is
caught up front rather than silently shipping a no-op object. `edit` only
patches the flags you actually pass -- omitting `--max-total-cpu` on an
`edit` call never clears an already-configured cap back to unset.

## How it's enforced

`kairon-controller`'s existing scheduling loop checks quota, immediately
before it would otherwise assign a Machine to a node:

1. Every reconcile tick, tally how much each `MachineQuota`'s namespace is
   already using: count and sum `spec.resources.cpu`/`.memory` across every
   Machine in that namespace that's already scheduled (`spec.nodeName` set)
   and not desired-`Stopped`/`Halted` -- **not** yet-unscheduled Machines, since
   those aren't actually consuming anything yet.
2. For each not-yet-scheduled Machine, first find whether it has an
   eligible node at all (placement/affinity/anti-affinity, unrelated to
   quota). Only once a node is otherwise eligible does quota get checked;
   checking it earlier would reserve capacity for a Machine that couldn't
   have been scheduled anyway, starving something else in the same tick
   that could.
3. If admitting this Machine would push any applicable quota's Machine
   count, total CPU, or total memory over its cap, the Machine is left
   `Pending` with a clear message (`MachineQuota ns/name: maxMachines N
   reached`, etc.) instead of being scheduled. It's retried automatically
   on the next reconcile tick -- there's no special unblocking step needed
   once capacity frees up (a Machine is deleted, a quota is raised).
4. `status.usedMachines`/`usedTotalCpuCores`/`usedTotalMemoryMiB` are
   patched onto every `MachineQuota` every tick, mirroring what a real
   `ResourceQuota`'s `status.used` reports -- `kaironctl get quotas` shows
   this alongside the configured caps, and so does `kairon-ui`'s own
   "Quotas" dashboard page (read-only, `GET /api/v1/quotas`).

Already-scheduled Machines are never evicted retroactively if a quota is
lowered below what's already running -- quota only blocks *new* scheduling.

## Previewing usage right now (`kaironctl describe`)

`describe` for every other kind in this project uniformly prints the raw
object as JSON and nothing else. `kaironctl describe quota` is one of only
four deliberate exceptions (the others are [`kaironctl describe
snapshotschedule`](machine-snapshot-schedules.md#previewing-what-would-fire-right-now-kaironctl-describe),
[`kaironctl describe migrationpolicy`](migration-policies.md#previewing-what-a-migration-would-get-right-now-kaironctl-describe),
and `kaironctl describe budget`, see
[machine-disruption-budgets.md](machine-disruption-budgets.md#previewing-who-counts-right-now-kaironctl-describe)).
It's warranted here for a reason the other two don't share: a `MachineQuota`'s
raw JSON puts `spec.maxTotalCpu`/`maxTotalMemory` (human strings like `"8Gi"`)
and `status.usedTotalCpuCores`/`usedTotalMemoryMiB` (already-normalized
numbers) in two separate top-level objects, in two different unit systems --
reading "how close is this namespace to its cap" out of the raw dump means
mentally converting a `Gi` string to `MiB` and cross-referencing it against a
separate field by eye:

```console
$ kaironctl describe quota team-payments
{
  "apiVersion": "kairon.zyvor.dev/v1",
  "kind": "MachineQuota",
  ...
}

Usage (recomputed fresh from Machines counted right now -- same MachineCountsTowardQuota predicate kairon-controller's own scheduling loop uses):
  machines  12 / 20
  cpu       28 vCPU / 40 vCPU (spec.maxTotalCpu "40"; 12 vCPU headroom)
  memory    98304 MiB / 163840 MiB (spec.maxTotalMemory "160Gi"; 65536 MiB headroom)

Counted machines (12):
  db-1                     4 vCPU  16384 MiB
  db-2                     4 vCPU  16384 MiB
  ...
```

This lines used up against limit in matching units (like `kubectl describe
resourcequota`'s own Used/Hard columns), computes remaining headroom instead
of leaving that subtraction to the reader, and lists exactly which Machines
are counted -- the same "don't just report a number, show the receipts"
precedent the other three `describe` exceptions already established.

Usage is recomputed fresh from the namespace's current Machines via the
controller's own exported `MachineCountsTowardQuota`/`MachineFootprint` (the
identical predicate and footprint calculation the scheduling loop uses to
admit or block a new Machine), not read back from `status.used*` -- which is
only ever as fresh as the last reconcile tick's patch. That means this
preview's totals can very occasionally read a Machine or two ahead of
`status.used*` if run between a Machine's admission and the next tick's
status patch; that's a real, honest gap, which is why the header says
"recomputed fresh" rather than implying it's reading `status` verbatim. A
dimension with no configured cap prints `(no limit)` rather than a bogus
`0 / 0`, and zero counted Machines prints an explicit hint instead of an
empty, unexplained list.

## Prometheus metrics and alerting

Until now, seeing a namespace approach its limit meant either watching
`status.used*` by hand or waiting for a Machine to actually land `Pending`
with a `MachineQuota ... reached` message -- there was no proactive signal.
`kairon-controller` now also exposes `kairon_quota_resource{namespace,
quota, resource, type}` (`resource` is `machines`/`cpu_cores`/`memory_mib`,
`type` is `used`/`hard`), the exact same tallies `status.used*` gets every
tick, recorded once per `Reconcile` right alongside that status patch so
the two can never disagree. A dimension a `MachineQuota` doesn't cap at all
(e.g. no `maxTotalCpu`) never gets a `type="hard"` series for it -- only
`type="used"`, never a misleading "capped at zero". `charts/kairon/alerts.yaml`'s
new `kairon-quotas` group (`KaironQuotaNearLimit`) fires once
`used`/`hard` has stayed at or above 90% for 10 minutes on any
namespace/quota/resource, well before the limit is actually reached and
new scheduling starts blocking. See `docs/guides/observability.md` for
which component(s) expose which metrics overall -- this one is
`kairon-controller`-only, mirroring the reconcile loop's own cluster-wide
`ListMachineQuotas` visibility (neither `kairon-node` nor `kairon-ui` has
an equivalent view to report from).

## Admission webhook (`webhook.enabled`)

Unlike a real Kubernetes `ResourceQuota` (enforced by the API server at
admission time), the reconcile-loop check above only runs *after* a
`Machine` already exists -- an over-quota namespace could always create as
many as it wanted, they just stayed `Pending` forever. An opt-in validating
admission webhook on `kairon-controller` (`webhook.enabled`, off by
default -- see the Helm chart's `webhook` values and SECURITY.md) closes
two gaps:

- **`Machine` `CREATE`** is rejected outright instead of just staying
  `Pending`, reusing the exact same `buildQuotaTrackers`/`admitQuota`
  decision the reconcile loop above already makes. This only evaluates
  `CREATE`, matching the reconcile loop's own scope exactly -- deliberately
  never stricter than what it backstops.
- **`Machine` `UPDATE`** is also rejected when it grows an
  already-scheduled Machine's `spec.resources` (a hotplug resize) past
  quota, via `admitQuotaResize` -- the one check on this page with no
  reconcile-loop equivalent to backstop it at all: `kairon-node`'s hotplug
  reconciliation is per-node and has no cluster-wide `MachineQuota`
  visibility of its own. A shrink, a no-change update, or a resize of a
  Machine that isn't yet scheduled are all always allowed (the last case is
  covered by the reconcile loop's own scheduling-time check instead, the
  same as a fresh `CREATE` would be).

Requires an operator-supplied TLS certificate (`webhook.tlsSecretName`,
`webhook.caBundle`) -- like `migration.tlsSecretName`, this chart doesn't
mint one for you.

The same webhook also closes a third, syntax-level gap: `Machine`
`CREATE`/resize `UPDATE` now rejects a `spec.resources.cpu`/`memory`/
`maxCpu`/`maxMemory` that isn't a valid quantity at all (`"abc"`, `"4x"`) --
previously that sailed through `kubectl apply` and only ever failed once
`kairon-controller` tried to actually create the VM, and in the meantime
counted as a *zero* CPU/memory footprint against every `MachineQuota` in
its namespace the whole time it sat stuck (see `MachineFootprint`,
`internal/controller/quota.go`). A `MachineQuota` `CREATE`/`UPDATE` gets
the same treatment for its own `maxTotalCpu`/`maxTotalMemory` (a new
`/validate-machinequota` route) -- worth calling out separately, since a
malformed value here doesn't just break its own namespace: `BuildQuotaTrackers`
parses every `MachineQuota` in the cluster on every reconcile tick, so one
bad object anywhere used to abort quota enforcement *and Machine
scheduling* cluster-wide. See `SECURITY.md`'s "Machine / MachineQuota
resource-quantity admission" section for the full detail.

## Real limits today (v1 of this feature)

- Scoped by Kubernetes namespace, not `Machine.spec.tenant`. That field
  only drives the opt-in east-west [tenant fence](tenant-fence.md);
  namespace is still the quota and RBAC boundary.
- The admission webhook above is opt-in; with it off (the default),
  nothing stops a namespace from having far more `Machine` objects created,
  or an existing one hotplugged further, than its quota allows -- creates
  just stay `Pending` forever instead of being rejected up front, and a
  resize past quota isn't caught anywhere at all (not even by the
  reconcile loop, which only ever evaluates quota once, at initial
  scheduling).
- CPU/memory quantities use the same fractional-CPU-rounds-up parsing as
  `Machine.spec.resources` (`internal/model.ParseVCPUs`/`ParseMemoryMiB`) --
  a `maxTotalCpu: "4.5"` cap behaves like `5`.
- The syntax-level checks above are admission-time prevention only, same
  opt-in gate as everything else on this page: an already-existing
  malformed `MachineQuota` (created before this existed, or with the
  webhook disabled) still aborts cluster-wide quota enforcement exactly as
  before -- containing that blast radius for an object that already
  slipped through isn't something this v1 does.
