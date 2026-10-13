# User guide: node maintenance mode and DRS rebalancing

Two kairon-controller features keep Machines where they should be:

- **Maintenance mode** empties a node before you patch, reboot or replace it.
  kairon-controller cordons it, migrates its Machines to other nodes, parks
  the ones no other node can take, and puts everything back when you are done.
- **DRS** (distributed resource scheduling) keeps nodes evenly loaded. It plans
  migrations from busy nodes to quiet ones and either recommends them or
  carries them out. It balances either reservations or measured pressure
  (PSI).

Experimental: maintenance mode on one node (cordon, the wait, stop and
hibernate fallbacks, exit) and DRS in recommend mode have run on real hardware
(see [`COMPATIBILITY.md`](../COMPATIBILITY.md#node-maintenance-and-drs)).
Migrating Machines to another node during maintenance, and DRS moves, have
not: they need a second node.

## Maintenance mode

```sh
kaironctl node maintenance enter worker-2                  # migrate, wait for the rest
kaironctl node maintenance enter worker-2 --fallback stop --wait
kaironctl node maintenance status
kaironctl node maintenance exit worker-2
```

`enter` sets annotations on the Node. You can set them yourself instead:

| Annotation | Values | Default |
|---|---|---|
| `kairon.zyvor.dev/maintenance` | `true` | |
| `kairon.zyvor.dev/maintenance-fallback` | `wait`, `stop`, `hibernate` | `wait` |
| `kairon.zyvor.dev/maintenance-strategy` | `live`, `auto`, `cold` | `live` |

### What happens on enter

1. kairon-controller cordons the node (`spec.unschedulable: true`) and marks it
   `kairon.zyvor.dev/maintenance-cordoned: "true"`, so exit knows it owns the
   cordon. A node you cordoned yourself is not marked, and exit leaves it
   cordoned. Every node entering maintenance is cordoned before any target is
   chosen, so two nodes draining at once never pick each other.
2. For each Machine on the node whose power state holds a VMM (`Running`,
   `Paused`), the controller looks for a target with the same scheduler and
   preflight checks a `MachineMigration` uses.
   - **A target exists:** it creates `MachineMigration`
     `maintenance-<node>-<machine>-<time>` with the node's strategy. With
     `live`, Machines live migration refuses (passthrough devices, non-qemu
     backends) are migrated `cold` instead. At most
     `-node-maintenance-max-parallel` (default 2) of these run per node.
     `MachineDisruptionBudget`s are honored. A Machine whose migration was
     blocked or ended without moving it is retried after
     `-node-maintenance-min-retry-interval` (default 2m).
   - **No target:** the fallback applies. `wait` leaves the Machine running and
     reports it. `stop` sets `powerState: Stopped`. `hibernate` sets
     `powerState: Hibernated` (qemu backend; other backends are stopped). The
     Machine gets `kairon.zyvor.dev/maintenance-parked`, recording the node and
     the change (`{"node":"worker-2","from":"Running","to":"Stopped"}`), and a
     `MaintenanceParked` Event. Disruption budgets apply to parking too.
3. The node reports progress in `kairon.zyvor.dev/maintenance-state` and
   `kairon.zyvor.dev/maintenance-message`:
   - `Entering`, for example "2 Machine(s) still on this node: 1 migrating, 1
     with no eligible target node (...)".
   - `InMaintenance` once no Machine on it holds a VMM. A `MaintenanceReady`
     Event is recorded on the Node.

`kaironctl node maintenance enter --wait` polls until `InMaintenance`.

### What happens on exit

Removing `kairon.zyvor.dev/maintenance` (`kaironctl node maintenance exit`)
reverses only what maintenance did:

- The node is uncordoned if the controller cordoned it.
- Each Machine parked from this node gets its old power state back, but only if
  its power state is still the one maintenance set. A Machine you changed in the
  meantime keeps your change; only its annotation is removed. A hibernated
  Machine resumes the same boot.
- The state annotations are removed and a `MaintenanceExited` Event is recorded.

Machines that migrated away stay where they are. DRS (below) can spread them
back out.

### Notes

- Maintenance is on by default (`-node-maintenance`, Helm
  `controller.nodeMaintenance.enabled`). It never acts on a node nobody
  annotated. It needs `patch` on nodes and `create` on machinemigrations, which
  the chart grants while it is enabled.
- With `-cordon-evacuate` also on, cordon evacuation leaves nodes in
  maintenance alone.
- An invalid fallback or strategy annotation leaves the node untouched apart
  from the cordon, and reports the error in the message and a
  `MaintenanceInvalid` Event.
- Starting a Machine that is assigned to a node in maintenance runs it there.
  The next pass migrates or parks it again.

## DRS rebalancing

```sh
kaironctl drs plan                      # what DRS would do now; changes nothing
kaironctl drs plan --threshold-percent 10 --max-moves 5 -o json
```

```text
NODE      CPU%  MEMORY%  UTILIZATION%  MACHINES
worker-1  75    75       75            3
worker-2  0     0        0             0

imbalance: 75% -> 25% after 1 move(s)

MACHINE        FROM      TO        IMBALANCE AFTER
prod/vm1       worker-1  worker-2  25%
```

A node's **utilization** is the larger of its reserved CPU and reserved memory,
as a share of allocatable. This is the same accounting the scheduler places
Machines with: requested vCPUs and memory, the memory overcommit ratio and
elastic-memory minimums. It is not measured guest demand. The **imbalance** is
the gap between the most and least utilized node.

When the imbalance reaches `-drs-threshold-percent` (default 20), the planner
looks for the single move that lowers it most. It only accepts a move that:

- lowers the imbalance by at least `-drs-min-improvement-percent` (default 5);
- leaves the target cooler than the source was, so the next round cannot move
  the Machine straight back;
- passes every hard filter the scheduler applies to the target: capacity,
  nodeSelector, required affinity and anti-affinity, taints, and
  `DoNotSchedule` spread constraints.

It repeats up to `-drs-max-moves` (default 1) moves per round.

The planner considers only Ready, schedulable nodes with allocatable capacity,
no `KaironMemoryPressure` and no maintenance. It never moves a Machine that:

- is not Running;
- is already migrating;
- has passthrough devices or DRA device claims;
- has pinned CPUs or a topology request;
- was moved by DRS within `-drs-cooldown` (default 1h);
- is annotated `kairon.zyvor.dev/drs: disabled`.

With `-drs-strategy live` (the default), only Machines live migration can move
are considered.

### Pressure signal

With `-drs-signal pressure` (Helm `controller.drs.signal`), DRS plans from
what the hosts measure instead of what Machines reserved. Every 30 s
(`--pressure-report-interval`) kairon-node asks FluxVM for the host's PSI
(`GET /v1/host/pressure`, from `/proc/pressure`) and each local Machine's own
cgroup PSI, and writes the `some avg60` values to its Node:

```sh
kubectl get node worker-1 -o jsonpath='{.metadata.annotations.kairon\.zyvor\.dev/pressure}'
```

```json
{"sampledAt":"2026-10-13T05:35:12Z","host":{"cpu":25.9,"memory":0.3,"io":55.9},
 "machines":{"prod/vm1":{"cpu":14.2,"memory":0,"io":0.4}}}
```

Each value is the share of the last minute in which at least one task waited
for that resource. A node's pressure is the worst of the three.

- A node is **hot** when its pressure reaches
  `-drs-pressure-threshold-percent` (default 25), and **calm** below half of
  that. Nodes in between are neither a source nor a target, which keeps a
  Machine from bouncing back.
- From each hot node, hottest first, DRS moves the Machine whose own cgroup
  stalls most to the calmest node that passes the same hard filters and
  capacity checks as above. Each calm node takes at most one Machine per
  round, and `-drs-max-moves` still caps the round.
- A node whose sample is older than `-drs-pressure-max-age` (default 2m), or
  that has none, is left out: its pressure is unknown. A host whose kernel
  has no PSI (macOS, or Linux booted with `psi=0`) publishes nothing rather
  than zeros.
- The plan does not predict pressure after a move. Each move carries a
  reason instead, which the Events repeat:

```sh
kaironctl drs plan --signal pressure
```

```text
NODE      CPU PSI%  MEMORY PSI%  IO PSI%  UTILIZATION%  MACHINES
worker-1  41.0      0.3          4.2      38            6
worker-2  2.1       0.0          0.8      25            4

pressure spread: 39 points

MACHINE   FROM      TO        WHY
prod/vm1  worker-1  worker-2  cpu pressure on worker-1 is 41.0% (threshold 25%); worker-2 is at 2.1%; this Machine stalls 14.2%
```

kairon-node needs a FluxVM with `GET /v1/host/pressure`. With an older one it
logs one warning and publishes nothing, and DRS treats the node as
unmeasured. Turn the publisher off with `--pressure-report=false`.

### Modes

`-drs-mode` (Helm `controller.drs.mode`), checked every `-drs-interval`
(default 5m):

| Mode | Effect |
|---|---|
| `off` (default) | Nothing. |
| `recommend` | Logs the plan, exports metrics, and records a `DRSRecommendation` Event on each Machine it would move, once per distinct plan. |
| `automatic` | Also creates `MachineMigration` `drs-<machine>-<time>` with `spec.targetNode` set and label `kairon.zyvor.dev/drs: "true"`, stamps the Machine `kairon.zyvor.dev/drs-moved-at`, and records a `DRSMigration` Event. `MachineDisruptionBudget`s are honored. No new round starts while a DRS migration is still in flight. |

### Metrics

| Metric | Meaning |
|---|---|
| `kairon_drs_imbalance_percent` | Imbalance in the last round (pressure spread with `-drs-signal pressure`) |
| `kairon_drs_node_utilization_percent{node,resource}` | Per-node `cpu`, `memory` and `max` utilization in the last round, plus `pressure` with `-drs-signal pressure` |
| `kairon_node_psi_some_avg60_percent{node,resource}` | Host PSI kairon-node last published (`cpu`, `memory`, `io`) |
| `kairon_drs_moves_total{result}` | `recommended`, `started`, `blocked` |
| `kairon_node_maintenance{node,state}` | 1 while a node is `Entering` or `InMaintenance` |
| `kairon_node_maintenance_remaining_machines{node}` | Machines still holding a VMM on a node in maintenance |

## Limits

- With the default `reserved` signal, DRS balances reservations: a node full
  of idle Machines looks busy. The `pressure` signal measures CPU, memory and
  IO stalls, but not NUMA locality or network saturation, and it moves one
  Machine per hot node per round without predicting the result.
- Maintenance does not drain Pods. Run `kubectl drain` for those; it does not
  touch Machines.
- Exit restores only what maintenance changed. It does not move Machines back
  to the node.
- Migrating Machines to another node during maintenance, and automatic DRS
  moves, have only run in unit tests, not on hardware. The pressure signal
  ran on one real node against a synthetic calm node, in recommend mode
  only (see [`COMPATIBILITY.md`](../COMPATIBILITY.md#node-maintenance-and-drs)).
