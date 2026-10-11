# User guide: Machine placement

How `spec.placement` steers which node a Machine lands on, and what today's
real limits are. NUMA, GPU and NIC locality inside a node is
[topology-aware placement](machine-topology.md).

## Fields

| Field | What it does | Hard or soft |
|---|---|---|
| `architecture` | Only consider nodes whose `kubernetes.io/arch` label matches | Hard |
| `nodeSelector` | Only consider nodes matching every given label | Hard |
| `affinity` | Require co-location with at least one other Machine matching a selector | Hard |
| `antiAffinity` | Require separation from every other Machine matching a selector | Hard |
| `preferredAffinity` | Weighted preference to co-locate with a matching Machine | Soft |
| `preferredAntiAffinity` | Weighted preference to separate from a matching Machine | Soft |
| `topologySpreadConstraints` | Weighted preference to spread matching Machines evenly across a topology label's domains; hard-caps skew too if `whenUnsatisfiable: DoNotSchedule` | Soft, or hard per-constraint |
| `tolerations` | Lets this Machine schedule onto a node carrying a matching `Taint` it would otherwise be excluded from (`NoSchedule`/`NoExecute`), or avoid the score penalty for one it would otherwise (`PreferNoSchedule`) | Hard for `NoSchedule`/`NoExecute`, soft for `PreferNoSchedule` |

The first four fields are always-hard filters evaluated at scheduling time
by `internal/scheduler`'s `eligible` check: a node failing one is never a
candidate at all. `preferredAffinity`/`preferredAntiAffinity` never reject
a node -- they only influence which *eligible* node wins, via a real
weighted scoring pass (`internal/scheduler.score`).
`topologySpreadConstraints` is scored the same way regardless, but each
constraint can *also* hard-filter (see below) if you ask it to.
`tolerations` doesn't filter or score anything by itself -- it only ever
cancels out a *node's own* taint (see below), the opposite direction from
every other field in this table, which all filter/score based on the
Machine's own placement preferences. None of these fields influence an
already-scheduled Machine (`spec.nodeName` is set once, at scheduling
time, same as `spec.image`/`spec.resources`).

When every node is filtered out, the resulting error names which
constraint(s) actually did it -- aggregated per distinct reason across
every excluded node (e.g. `2 node(s): nodeSelector zone=us-east not
satisfied; 1 node(s): cpuPinning requests 8 vCPU(s), only 4 free`) --
rather than one undifferentiated "no nodes match" message, so debugging a
stuck `Machine` doesn't require guessing which of `architecture`/
`nodeSelector`/affinity/anti-affinity/`cpuPinning` capacity is the actual
blocker.

Two node-level checks sit alongside these: a node whose
`KaironMemoryPressure` condition is True is never a candidate, and a node
annotated `kairon.zyvor.dev/memory-overcommit-ratio` admits elastic-memory
Machines by their guaranteed minimum. See
[elastic memory](machine-elastic-memory.md).

## Affinity and anti-affinity

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: cache
spec:
  placement:
    affinity:
      - labelSelector: {tier: web}
        topologyKey: kubernetes.io/hostname
    antiAffinity:
      - labelSelector: {role: db-primary}
        topologyKey: kubernetes.io/hostname
```

Each term is `{labelSelector, topologyKey}` — deliberately shaped like
Kubernetes Pod affinity terms, so it's immediately familiar:

- **`affinity`**: the candidate node is eligible only if at least one other
  Machine matching `labelSelector` is currently scheduled to a node sharing
  the candidate's value for the `topologyKey` label.
- **`antiAffinity`**: the candidate node is eligible only if **no** other
  Machine matching `labelSelector` shares the candidate's `topologyKey`
  value.
- `topologyKey` names any node label -- `kubernetes.io/hostname` for
  same/different-node, or a rack/zone/region label your nodes carry, for
  broader placement domains.
- A Machine never matches its own affinity/anti-affinity terms against
  itself (relevant when re-evaluating an already-scheduled Machine, e.g. as
  a live-migration target search).

## Preferred (soft) affinity and topology spread

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: cache
spec:
  placement:
    preferredAffinity:
      - weight: 20
        labelSelector: {tier: web}
        topologyKey: kubernetes.io/hostname
    preferredAntiAffinity:
      - weight: 10
        labelSelector: {role: db-primary}
        topologyKey: kubernetes.io/hostname
    topologySpreadConstraints:
      - topologyKey: topology.kubernetes.io/zone
        labelSelector: {tier: web}
```

Each eligible node's score starts at `-(Machines already assigned to it)`
(the whole of the old load-balancing behavior) and then:

- `preferredAffinity`/`preferredAntiAffinity` add/subtract that term's
  `weight` when satisfied (same `{labelSelector, topologyKey}` semantics
  as the hard fields above, just non-rejecting).
- `topologySpreadConstraints` subtracts however many other Machines
  matching `labelSelector` are already in that node's `topologyKey`
  domain -- more existing matches there, lower score, favoring the
  emptier domain.
- The highest-scoring eligible node wins; a Machine with none of these
  three fields set schedules identically to before this scoring pass
  existed (least-loaded, deterministic hash tie-break on exact ties).

There's no fixed range for `weight` -- it's compared directly against the
1-point-per-Machine load penalty, so pick it relative to how much load
imbalance you want the preference to be able to outweigh (weight 20
comfortably beats several Machines' worth of imbalance; weight 1 only
matters when load is already tied).

## Hard-enforcing `topologySpreadConstraints.maxSkew`

```yaml
    topologySpreadConstraints:
      - topologyKey: topology.kubernetes.io/zone
        labelSelector: {tier: web}
        maxSkew: 1
        whenUnsatisfiable: DoNotSchedule
```

`whenUnsatisfiable: DoNotSchedule` (mirroring the Kubernetes field exactly)
turns this one constraint into a real filter, on top of the scoring it
already gets: a candidate node is dropped outright if placing this
Machine there would push that domain's skew (the matching-Machine count in
that domain versus the least-populated domain among candidates) over
`maxSkew`. Every domain still eligible after *all* `DoNotSchedule`
constraints are applied moves on to the same weighted scoring as before;
`Choose` returns an error if none are left. Omitting `whenUnsatisfiable`,
or setting it to `ScheduleAnyway` (the default, and every
`topologySpreadConstraints` entry before this field existed), never
rejects a node over skew -- `maxSkew` stays purely informational for
scoring, exactly as before.

`maxSkew`'s zero value (unset, same as writing `0` explicitly -- there's
no way to tell those apart through this field alone) is treated as a real
"domains must stay perfectly balanced," not as disabling enforcement --
the stricter of the two readings, deliberately, matching this project's
general fail-closed posture elsewhere. A real Kubernetes cluster's API
server would reject `maxSkew: 0` outright as invalid; Kairon has no
equivalent admission-time field validation to hook a rejection into, so
if you meant to write a positive number, write one.

## Taints and tolerations

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: gpu-job
spec:
  placement:
    tolerations:
      - key: dedicated
        operator: Equal
        value: gpu
        effect: NoSchedule
```

A `Node` here is the real Kubernetes `Node` object `kairon-controller`
already lists every reconcile tick -- `internal/scheduler` now also reads
its `spec.taints` (previously decoded by nothing in this project at all)
the same way a real `kube-scheduler` does. Kairon itself never sets a
taint; this is entirely about respecting whatever's already on the node --
a manual `kubectl taint`, a cloud provider's spot-instance taint, a
`node.kubernetes.io/unreachable` taint from a lost kubelet, or anything
else already there before Kairon looked.

- **`NoSchedule`/`NoExecute`** taints are a hard filter, same enforcement
  point as `architecture`/`nodeSelector`/`affinity`: a node carrying one
  is never a candidate for a Machine that doesn't tolerate it. When every
  node is filtered out, the aggregated error names it exactly like any
  other blocking reason, e.g. `1 node(s): untolerated NoSchedule taint
  dedicated=gpu`.
- **`PreferNoSchedule`** taints are soft, same enforcement point as
  `preferredAffinity`: an untolerated one only subtracts a fixed penalty
  from the node's score (`internal/scheduler`'s
  `preferNoScheduleTaintPenalty`, comparable in size to the DRA-preference
  bonus) -- it never removes the node from candidacy.
- A `Toleration` matches a `Taint` the same way Kubernetes' own
  `Toleration.ToleratesTaint` does: `key` must match (or be empty, which
  matches any key), `effect` must match if set (empty tolerates every
  effect for that key), and `operator: Equal` (the default when omitted)
  additionally requires `value` to match while `operator: Exists` ignores
  `value` entirely. An empty `key` with `operator: Exists` is Kubernetes'
  own "tolerate everything" wildcard toleration -- schedules onto any
  node regardless of what's tainted there.
- A Machine with no `tolerations` set schedules identically to before this
  field existed on an untainted fleet: nothing here changes what happens
  when `spec.taints` is empty on every node.

Before writing a `tolerations` entry you first need to know what's actually
tainted -- `kaironctl get nodes` lists every real Kubernetes `Node`
alongside its `Ready` condition, `spec.unschedulable`, and a `key[=value]:
Effect` summary of `spec.taints` (`-` when a node carries none), and
`kaironctl describe node NAME` dumps one node's full object, taints
included, the same raw-JSON way `describe` already works for every other
kind. Neither existed at all before this change -- an operator debugging
"why won't my Machine schedule onto NODE" once this section's own filtering
kicked in had no way to see a node's taints short of `kubectl get node
NODE -o yaml`, even though `kaironctl` already had full `get`/`describe`
for every kairon.zyvor.dev kind. `Node` is cluster-scoped, unlike
everything else `kaironctl get`/`describe` supports, so `-n`/`--namespace`
is silently ignored for it; there's no `kaironctl delete node`, since
deleting a cluster Node is squarely `kubectl`'s job, not Kairon's.

The dashboard now shows the same information: `kairon-ui`'s **Nodes** page
lists every node's `Ready` condition, `Unschedulable`, taints, and
addresses, refreshed every 5s like every other list page. `GET
/api/v1/nodes` already existed (it backed the Overview tile's node count),
but nothing in the dashboard rendered the list itself until this page --
an operator working only from the browser, not `kaironctl`/`kubectl`, had
no way to see a node's taints at all. Read-only, matching `kaironctl get
nodes`' own scope exactly: no create/edit/delete button, since Node's
lifecycle stays kubectl's job here too.

**Real limits today**: an untolerated `NoExecute` taint only ever blocks
*new* scheduling -- unlike real Kubernetes, Kairon has no eviction pass
that migrates or deletes an *already-running* Machine off a node that
becomes `NoExecute`-tainted after that Machine landed there (no
`tolerationSeconds` countdown either, since there's nothing to time out).
If you need a currently-running Machine off a freshly-tainted node,
`kaironctl evacuate NODE` (or cordoning the node, if `CordonEvacuation` is
enabled -- see `docs/guides/machine-disruption-budgets.md`) still works;
a taint alone doesn't trigger it automatically the way a cordon can.
Kairon also has no admission-time validation of `operator`/`effect`
values (same real limit `maxSkew`'s own section above already documents
for this project's placement fields generally) -- an unrecognized
`operator` fails closed, matching nothing, rather than being silently
treated as `Equal` or `Exists`.

## Scheduling priority

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: urgent-vm
spec:
  priority: 10
```

Every field above steers *which node* a Machine lands on. `spec.priority`
answers a different question: *when several Machines are all still
unscheduled at once and can't all fit, which gets tried first?*

Every reconcile tick, `kairon-controller` collects every Machine that isn't
scheduled yet (no `spec.nodeName`), sorts that list by `spec.priority`
descending, then runs each one through placement (this page's own
eligibility/scoring pass) and, if a node is found, `MachineQuota` admission
-- in that order. A higher-`priority` Machine is attempted, and can claim
scarce node capacity or the last unit of `MachineQuota` headroom, before a
lower-priority one — even one that was created earlier, or that the API
happened to list first.

There's no fixed range for `priority`, same as `preferredAffinity`'s own
`weight` — pick any integer, positive or negative; the default is `0`. Ties
(the overwhelming common case: every Machine that doesn't set this field)
keep whatever order they'd have had anyway — the sort used is stable, so a
fleet that never sets `priority` schedules in exactly the order it always
did.

```
kaironctl create urgent-vm --image /base.qcow2 --priority 10
kaironctl create machineset web --image /base.qcow2 --replicas 5 --priority 5
kaironctl edit machine urgent-vm --priority 20
```

`kaironctl create`/`create machineset` both take `--priority N` (a
MachineSet's replicas all inherit its template's priority); `kaironctl edit
machine NAME --priority N` changes it on an existing Machine without a
delete/recreate — the one Machine-spec field this project's `edit` verb
supports patching after creation, since it only ever affects a *future*
tick's admission order.

The dashboard (kairon-ui) has the same capability: each row on the
Machines page carries a Priority field with a "Set" button that appears
once you change it, which `POST`s to
`/api/v1/machines/{namespace}/{name}/priority`
(`internal/uiapi/machines.go`'s `handleSetMachinePriority`) — the same
narrow, single-field `spec.priority` merge-patch `kaironctl edit machine`
issues, reusing the `patch` verb the ClusterRole already grants kairon-ui
on `machines` for power actions (no new RBAC). Any whole number is
accepted, including negative, same as the CLI.

**By default this is not preemption.** A high-`priority` Machine created
after a lower-`priority` one is already running never evicts, migrates, or
otherwise disturbs it — `priority` only orders Machines that are *already*
competing to be scheduled in the same tick. It also never changes *which*
eligible node a Machine lands on — that's still this page's own
load/affinity/topology-spread scoring, untouched.

Preemption is opt-in on both sides: a pending Machine with
`kairon.zyvor.dev/preempt=true` that cannot be placed may halt one
same-namespace, lower-priority Machine annotated
`kairon.zyvor.dev/preemption-policy=Halt`, and the victim resumes once the
preemptor is gone. Nothing is evicted or migrated. See
[`preemption.md`](preemption.md).

## Real limits today

- **DRA topology-awareness is a best-effort hint unless the Machine sets a
  hard `spec.topology.policy`** (`restricted` or `single-numa-node`; see
  [topology-aware placement](machine-topology.md#dra-device-claims)), which
  makes the claim's node and NUMA node binding. Otherwise
  `kairon-controller` has no role in DRA device allocation
  itself -- for a Machine with `spec.deviceClaims`, it only reads back an
  already-allocated `ResourceClaim`'s device pool and, if that pool maps to
  a specific node via a `ResourceSlice`, adds a small fixed score bonus
  toward that node. An unallocated claim, or a pool with no single owning
  node (a network-attached device pool), contributes no hint at all --
  scheduling falls back to the other signals.
- **Evaluated against the reconcile-time Machine list, not a live watch.**
  Placements made earlier in the same reconcile tick are visible to later
  Machines in it (node, pinned CPUs and topology allocation), so they no
  longer collide on cores. Machines are still placed one at a time in
  priority order, so a Machine whose affinity target is placed later in the
  same tick waits for the next tick (every `controller.interval`, 5s by
  default). This applies to `DoNotSchedule` topology-spread domain counts
  too.
- **`DoNotSchedule` domains are only counted among candidate nodes, not
  every node in the cluster** -- a domain with matching Machines but no
  currently-eligible node in it (filtered out by `architecture`/
  `nodeSelector`/affinity/anti-affinity first) isn't part of the skew
  calculation at all, the same "candidate nodes matching the pod's own
  node affinity" scope real Kubernetes topology spread uses.
