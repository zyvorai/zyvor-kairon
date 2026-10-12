# User guide: node maintenance mode and DRS rebalancing

Two kairon-controller features keep Machines where they should be:

- **Maintenance mode** empties a node before you patch, reboot or replace it.
  kairon-controller cordons it, migrates its Machines to other nodes, parks
  the ones no other node can take, and puts everything back when you are done.
- **DRS** (distributed resource scheduling) keeps nodes evenly loaded. It plans
  migrations from busy nodes to quiet ones and either recommends them or
  carries them out.

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
| `kairon_drs_imbalance_percent` | Imbalance in the last round |
| `kairon_drs_node_utilization_percent{node,resource}` | Per-node `cpu`, `memory` and `max` utilization in the last round |
| `kairon_drs_moves_total{result}` | `recommended`, `started`, `blocked` |
| `kairon_node_maintenance{node,state}` | 1 while a node is `Entering` or `InMaintenance` |
| `kairon_node_maintenance_remaining_machines{node}` | Machines still holding a VMM on a node in maintenance |

## Limits

- DRS balances reservations, not measured CPU, memory, NUMA or network
  pressure. A node full of idle Machines looks busy.
- Maintenance does not drain Pods. Run `kubectl drain` for those; it does not
  touch Machines.
- Exit restores only what maintenance changed. It does not move Machines back
  to the node.
- Migrating Machines to another node during maintenance, and automatic DRS
  moves, have only run in unit tests, not on hardware.
