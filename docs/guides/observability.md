# User guide: metrics and alerts

Until now, Kairon's Prometheus metrics were `kairon-controller`-only and
migration-lifecycle-only (`internal/metrics`'s original scope) -- a real
observability gap this page closes the worst of: there was no signal at
all if `kairon-controller`/`kairon-node` themselves were unhealthy or
wedged, no metrics from `kairon-node`/`kairon-ui` at all, and every alert
in `charts/kairon/alerts.yaml` was migration-specific.

## What each component exposes

Every metric name and its exact meaning lives in `internal/metrics`'s
package doc and `Observe*` method comments -- this table is a map to that,
not a duplicate of it.

| Component | Port | New in this pass | Already existed |
|---|---|---|---|
| `kairon-controller` | `controller.healthPort`, default 32301 (`/metrics`) | `kairon_reconcile_duration_seconds`, `kairon_reconcile_errors_total`, `kairon_reconcile_item_errors_total`, `kairon_webhook_decisions_total` (only if `webhook.enabled`), `kairon_apiserver_request_duration_seconds` | `kairon_migration_phase_count`, `kairon_migration_phase_age_seconds`, `kairon_migration_completed_total`, `kairon_migration_transfer_duration_seconds`, `kairon_migration_cutover_downtime_seconds`, `kairon_migration_dataplane_encrypted` |
| `kairon-node` | `node.healthPort` , default 32302 (`/metrics`) | `kairon_reconcile_duration_seconds`, `kairon_reconcile_errors_total`, `kairon_reconcile_item_errors_total`, `kairon_apiserver_request_duration_seconds` | (none -- `internal/health.Server.Metrics` existed but was never wired to anything, so `/metrics` 404'd) |
| `kairon-ui` | its own listen port (`GET /metrics`, unauthenticated -- see `SECURITY.md`) | `kairon_ui_request_duration_seconds`, `kairon_apiserver_request_duration_seconds` | (none -- no health/metrics endpoint of any kind existed) |

`kairon_apiserver_request_duration_seconds` is the one metric all three
share, by design: every component talks to the Kubernetes API through the
same `internal/kube.Client`, so instrumenting it once (`Client.Observe`,
wired to each component's own `Recorder` in `cmd/*/main.go`) gives every
component apiserver call visibility for free, labeled `method`
(GET/POST/PATCH/PUT/DELETE) and `outcome` (ok/error).

`kairon-controller` also reports live migration telemetry from each MachineMigration's `status.progress` and `status.adaptive`. Adaptive migration is experimental until the lab migration matrix runs green.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kairon_migration_progress` | gauge | `namespace`, `name`, `field` | Telemetry for a migration in Starting or Running. `field` is `dirty_rate_mbps`, `ram_remaining_bytes`, `ram_transferred_bytes`, `throughput_mbps`, `expected_downtime_ms`, `cpu_throttle_pct`, `estimated_completion_seconds`, `compression_ratio_percent` or `duplicate_pages`. |
| `kairon_migration_adaptive_stage` | gauge | `namespace`, `name`, `stage` | 1 for the adaptive engine's current stage: `PreCopy`, `AutoConverge`, `RelaxDowntime`, `PostCopy`, `Abort` or `Unsupported`. |
| `kairon_migration_adaptive_actions_total` | counter | `action` | Adaptive engine actions, such as `auto-converge`, `throttle`, `relax-downtime`, `bandwidth`, `postcopy`, `abort`, `tune-failed` and `postcopy-refused`. |
| `kairon_topology_allocation_failures_total` | counter | `reason` | Scheduling attempts for `spec.topology`/`spec.devices` Machines that found no placement; `reason` is `TopologyUnsatisfiable`, `DRAAllocationPending` or `Unschedulable`. |
| `kairon_node_numa_free_cpus` | gauge | `node`, `numa` | Pinnable CPUs not held by a Machine, per NUMA node, from each `NodeTopology`. See [topology-aware placement](machine-topology.md). |
| `kairon_node_memory_pressure` | gauge | `node`, `state` | kairon-node only. 1 while host memory is under pressure (`state=pressure`), and while it is with every elastic-memory balloon at its minimum (`state=exhausted`, the `KaironMemoryPressure` condition). See [elastic memory](machine-elastic-memory.md). |
| `kairon_node_virtual_networks` | gauge | — | kairon-node only. Virtual networks applied to the local FluxVM. See [virtual networks](virtual-networks.md). |
| `kairon_node_virtual_network_endpoints` | gauge | `location` | kairon-node only. Endpoints of the applied networks on this node (`local`) and on other nodes (`remote`). |
| `kairon_node_virtual_network_sync_errors_total` | counter | — | kairon-node only. Failed virtual network applies or listings against FluxVM. |
| `kairon_node_bgp_session_up` | gauge | `peer` | kairon-node only. 1 while the BGP session to `peer` is Established. |
| `kairon_node_bgp_advertised_prefixes` | gauge | `peer` | kairon-node only. Prefixes announced to `peer`. |
| `kairon_machine_balloon_mib` | gauge | `namespace`, `machine`, `kind` | kairon-node only. Balloon size of an elastic-memory Machine: `kind=target` (asked) or `actual` (given back by the guest). |
| `kairon_memory_reclaim_actions_total` | counter | `direction` | kairon-node only. Balloon changes, `inflate` or `deflate`. |

`kairon-node` also registers the VM-edge metrics below (not registered on the controller or kairon-ui). Details, including how drops are read from FluxVM, are in [`docs/ebpf-edge.md`](../ebpf-edge.md#metrics).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kairon_net_drops_total` | counter | `namespace`, `machine`, `reason`, `policy` | VM-edge drops attributed to a MachineNetworkPolicy or dataplane guard; `policy` is `-` when not tied to a policy. No series exists until the first drop. |
| `kairon_net_conntrack_restored_total` | counter | none | Conntrack entries restored onto a destination during live migration. |
| `kairon_net_migration_blackhole_ms` | histogram | none | Milliseconds between conntrack export and restore. Buckets 5 to 2500 ms. |

`kairon-node`'s metrics needed no chart changes -- its health port was
already exposed as a container port, just never serving `/metrics`.
`kairon-ui`'s did: it has no separate health-check port at all, so its
`/metrics` route shares the same listen port as the dashboard and its
`/api/v1/...` routes, on the same "unauthenticated, like `/healthz`"
convention -- see `SECURITY.md`'s `kairon-ui` section for the trust
boundary that implies (aggregate request counts/latencies, not sensitive
data, but reachable wherever the dashboard itself is).

## `kairon_quota_resource` (`kairon-controller` only)

A later addition on top of the above: `kairon-controller`'s `/metrics` now
also exposes `kairon_quota_resource{namespace, quota, resource, type}`,
one gauge per `MachineQuota` dimension it caps or tracks (`resource` is
`machines`/`cpu_cores`/`memory_mib`; `type` is `used` or `hard`) --
mirroring `kube-state-metrics`' own `kube_resourcequota{resource, type}`
shape so the familiar `used / hard` ratio pattern applies directly.
Recorded once per `Reconcile` tick (`internal/metrics.Recorder.ObserveQuotas`),
from the exact same tallies that tick patches onto each `MachineQuota`'s
own `status.used*` fields -- never a second, independently-computed
number that could drift from what `kubectl get machinequota` shows for
the same tick. A dimension the `MachineQuota` doesn't cap at all (e.g. no
`maxTotalCpu` set) never gets a `type="hard"` series for it, only
`type="used"` -- there's no limit to report, not a limit of zero that
would misleadingly read as "already over quota."

Why `kairon-controller`-only, unlike `kairon_apiserver_request_duration_seconds`:
this needs the same cluster-wide `MachineQuota`/`Machine` listing the
reconcile loop's own quota enforcement already does (`internal/controller/quota.go`),
which neither `kairon-node` (no cluster-wide visibility, only ever
self-checks quota for its own node's hotplug resizes) nor `kairon-ui` (no
reconcile loop at all) has an equivalent of. See `docs/guides/machine-quotas.md`
for the full picture, including the `KaironQuotaNearLimit` alert below.

## `kairon_disruption_budget_status` (`kairon-controller` only)

The same follow-on applied to the other resource that already computed
real status every tick but never exposed it as a metric:
`kairon-controller`'s `/metrics` now also exposes
`kairon_disruption_budget_status{namespace, budget, field}`, one gauge per
`MachineDisruptionBudget` status field (`field` is `expected_machines`,
`current_healthy`, `desired_healthy`, or `disruptions_allowed`) --
mirroring `kube-state-metrics`' own `kube_poddisruptionbudget_status_*`
gauges (four separate metric names there, collapsed here into one vector
via the `field` label, the same collapsing `kairon_quota_resource` already
does with its own `type` label). Recorded once per `Reconcile` tick
(`internal/metrics.Recorder.ObserveDisruptionBudgets`, called from
`internal/controller/disruption.go`'s `reconcileDisruptionBudgetsStatus`),
from the exact same `BudgetState.Status()` values that tick patches onto
each `MachineDisruptionBudget`'s own `status.*` fields -- never a second,
independently-computed number that could drift from what
`kubectl get machinedisruptionbudget` shows for the same tick.

Why `kairon-controller`-only: same reason as `kairon_quota_resource` above
-- this needs the cluster-wide `MachineDisruptionBudget`/`Machine`/
`MachineMigration` listing `LoadBudgetStates` already does
(`internal/controller/disruption.go`), which neither `kairon-node` nor
`kairon-ui` has an equivalent of. See
`docs/guides/machine-disruption-budgets.md` for the full picture,
including the `KaironDisruptionBudgetExhausted` alert below.

## `kairon_machineset_status` (`kairon-controller` only)

The same follow-on applied to a third resource that already computed real
status every tick but never exposed it as a metric:
`kairon-controller`'s `/metrics` now also exposes
`kairon_machineset_status{namespace, machineset, field}`, one gauge per
`MachineSet` rollout status field (`field` is `replicas`, `ready_replicas`,
or `updated_replicas`) -- mirroring `kube-state-metrics`' own
`kube_replicaset_status_replicas`/`kube_replicaset_status_ready_replicas`/
`kube_deployment_status_replicas_updated` gauges (three separate metric
names there), collapsed here into one vector via the `field` label, the
same collapsing `kairon_quota_resource`/`kairon_disruption_budget_status`
already do with their own `type`/`field` labels. Recorded once per
`Reconcile` tick (`internal/metrics.Recorder.ObserveMachineSets`, called
from `internal/controller/machineset.go`'s `reconcileMachineSets`), from
the exact same tally that tick's `status.replicas`/`.readyReplicas`/
`.updatedReplicas` patch uses -- never a second, independently-computed
number that could drift from what `kubectl get machineset` shows for the
same tick. Unlike `kairon_quota_resource`/`kairon_disruption_budget_status`,
this is observed even when the tick's own create/delete step failed (the
tally is computed from the Machine snapshot before that step ever runs, so
it stays accurate regardless of whether the attempted mutation succeeded).

Why `kairon-controller`-only: same reason as `kairon_quota_resource` and
`kairon_disruption_budget_status` above -- `kairon-node` never lists
`MachineSet`s cluster-wide at all (it only reconciles individual `Machine`s
already assigned to it), and `kairon-ui` has no reconcile loop of its own,
so neither has an equivalent per-tick tally to report here. See
`docs/guides/machine-sets.md` for the full picture, including the
`KaironMachineSetRolloutStuck` alert below.

## Alerts

`charts/kairon/alerts.yaml` now has five rule groups:

- **`kairon-migrations`** (unchanged) -- the original four alerts, all
  pointing at `docs/runbook-migration-failures.md`.
- **`kairon-health`** (new) -- four cross-component alerts with no
  runbook of their own (check the named component's own logs):
  - `KaironReconcileErrorRateHigh` -- more than 5 failed reconcile ticks
    in 15 minutes, on `kairon-controller` or `kairon-node`.
  - `KaironReconcileItemErrorRateHigh` -- more than 5 per-item reconcile
    failures (`kairon_reconcile_item_errors_total`) in 15 minutes.
  - `KaironAPIServerErrorRateHigh` -- more than 10 failed apiserver calls
    in 15 minutes, from any component.
  - `KaironWebhookDenyRateHigh` -- more than 20 admission-webhook denials
    in 15 minutes (`webhook.enabled` only). `severity: info`, not `warn`
    -- a denial is the webhook doing its job; a spike usually means a
    `MachineQuota`/`MachineDisruptionBudget` was set stricter than
    expected, not that something is broken.
- **`kairon-quotas`** (new) -- one alert, `KaironQuotaNearLimit`: a
  `MachineQuota`'s `used`/`hard` ratio (`kairon_quota_resource`, see
  above) has stayed at or above 90% for 10 minutes on some
  namespace/quota/resource. Fires *before* the limit is actually hit --
  previously the only signal was a Machine already stuck `Pending` (or,
  with `webhook.enabled`, an outright denied create/resize) with a
  "MachineQuota ... reached" message. `severity: warn`, pointed at
  `docs/guides/machine-quotas.md` rather than a dedicated runbook (there's
  nothing to debug -- raise the limit or free capacity).
- **`kairon-disruption-budgets`** (new) -- one alert,
  `KaironDisruptionBudgetExhausted`: a `MachineDisruptionBudget`'s
  `disruptions_allowed` (`kairon_disruption_budget_status`, see above) has
  stayed at `0` for 10 minutes. Unlike `KaironQuotaNearLimit`, this fires
  *at* exhaustion rather than approaching it -- `disruptions_allowed`
  swings between `0` and a positive number as Machines matching the
  budget's selector come and go, so a "near" threshold on a small integer
  count would either never fire (rounding) or fire on every ordinary dip;
  `== 0` sustained for 10 minutes is the meaningful signal instead ("this
  budget is currently blocking every `kaironctl evacuate`/webhook-admitted
  disruption against it," not "it dipped to 0 for one reconcile tick").
  `severity: info`, same rationale as `KaironWebhookDenyRateHigh`: a
  disruption budget sitting at 0 is working-as-intended during, say, a
  rolling node drain, not necessarily a problem -- pointed at
  `docs/guides/machine-disruption-budgets.md` rather than a dedicated
  runbook.
- **`kairon-machinesets`** (new) -- one alert,
  `KaironMachineSetRolloutStuck`: a `MachineSet`'s `ready_replicas`
  (`kairon_machineset_status`, see above) has stayed below its `replicas`
  for 30 minutes. Previously the only signal a rollout had stalled was
  polling `kaironctl get machinesets`/`kubectl get machinesets` and
  noticing `READY` never catches up to `REPLICAS` -- this fires
  proactively instead, the same 30-minute "sustained, not a transient
  blip" threshold `KaironMigrationStuckInFlight` already uses for an
  in-flight migration. `severity: warn`, pointed at
  `docs/guides/machine-sets.md` rather than a dedicated runbook.

Neither rule file distinguishes *which* `kairon-controller` or
`kairon-node` instance fired, since `kairon_reconcile_*`/
`kairon_apiserver_*` use the same metric names on every instance's own
`/metrics` -- that's your scrape config's `job`/`instance` labels to add,
same as any other multi-target Prometheus rule.

If you run prometheus-operator, `metrics.prometheusRule.enabled=true`
renders `charts/kairon/alerts.yaml` as a real `PrometheusRule` named
`kairon-alerts` -- **renamed from `kairon-migrations`** now that it covers
more than migrations. On `helm upgrade` from an older release, the
old-named object is orphaned (Helm creates the new name rather than
renaming in place): `kubectl delete prometheusrule kairon-migrations -n
<namespace>` cleans it up. Otherwise, copy the rule groups into your own
Prometheus `rule_files` config directly.

## Real limits today (first cut)

- Distributed tracing is opt-in via `otel.enabled` / `KAIRON_OTEL_ENDPOINT`
  (`internal/oteltrace`): OTLP/HTTP JSON reconcile spans for
  `kairon-controller` and `kairon-node` item loops. Not the full
  OpenTelemetry Go SDK — a deliberate narrow exporter so enabling tracing
  does not pull a large dependency tree. Off by default.
- `kairon-ui`'s `kairon_ui_request_duration_seconds` now carries a
  `route` label -- the registered mux *pattern* a request matched (e.g.
  `/api/v1/machines/{namespace}/{name}`), never the raw request path, so
  cardinality stays bounded regardless of how many distinct
  Machines/namespaces are ever actually requested, the same way
  `status_class` already bounds it against the full HTTP status range
  instead of the raw numeric code. A request matching nothing registered
  under `/api/v1/` (a genuine 404) reports `route="unmatched"`.
- `kairon_reconcile_errors_total` only counts a whole reconcile tick
  returning an error outright (a `List*` call itself failing) -- the far
  more common case, one Machine/MachineMigration/MachineSnapshot/
  MachineSnapshotRestore among many failing its own reconcile step, is
  caught, logged, and status-patched individually without the tick
  itself failing, so it never incremented this counter at all.
  `kairon_reconcile_item_errors_total{kind}` now counts exactly those,
  `kind` being one of `machine`/`migration`/`snapshot`/`snapshotrestore`
  -- labeled by resource *kind* only, never a specific name/namespace, to
  keep cardinality bounded. Still not a substitute for a real trace: it
  can tell you "machine reconciliation is failing repeatedly," not *which*
  Machine -- check the accompanying "... reconcile failed" log line
  (which does carry namespace/name) for that, or enable `otel.enabled`.

## Migration concurrency caps

`migration.maxConcurrentPerNode`/`migration.maxConcurrentCluster` (both `0` = unlimited) bound how many non-terminal migrations may touch one node or the cluster at once — a lightweight admission control, mainly useful to cap the blast radius of a bulk `kaironctl evacuate`.

## Workload hardening defaults

All three workloads set CPU/memory `resources:` by default, `kairon-controller`/`kairon-ui` each get a `PodDisruptionBudget` (on by default — voluntary-eviction protection only, not HA), and an opt-in `NetworkPolicy` (`*.networkPolicy.enabled`) restricts their ingress once you tell it which other namespace needs to reach in (`*.networkPolicy.allowIngressFrom` — get this wrong and Prometheus scraping or your ingress controller breaks silently, so it's opt-in rather than default-on). Image supply-chain (digest pins, Trivy, cosign, SBOM) is documented in [SECURITY.md](https://github.com/zyvorai/kairon/blob/main/SECURITY.md)'s "Images" section.

Real two-host live-migration testing and a live `NeedsRecovery` drill are documented as runbooks with helper scripts, since they need hardware this repository's own CI doesn't have: [`runbook-multi-host-migration-test.md`](../runbook-multi-host-migration-test.md), [`runbook-recovery-drill.md`](../runbook-recovery-drill.md).
