# User guide: real CPU pinning (`spec.resources.cpuPinning`)

Real, exclusive host-core allocation -- a Machine actually owns specific
physical CPUs, with Kairon's own scheduler tracking and enforcing
non-overlapping allocation across every Machine competing for cores on a
node. The capability [`machine-cpu-numa.md`](machine-cpu-numa.md)'s own
`cpuSet` field explicitly does **not** provide (that one is a
guest-visible topology hint only).

## Why this exists

FluxVM already supports real `cgroup v2 cpuset.cpus` pinning via its own
resize API -- the missing piece was never the low-level primitive, it was
that *allocating specific, non-overlapping host CPU numbers* across every
Machine competing for them on one node is a real capacity-allocation
problem, and Kairon's scheduler used to do pure Machine-*count*
bin-packing with no notion of CPU capacity at all. This closes that gap.

## Prerequisites: which CPUs are pinnable

The node label `kairon.zyvor.dev/pinnable-cpus` names the host CPUs a
Machine may own. Either an operator sets it, or kairon-node discovers it
(see [Discovered set](#discovered-set) below). An operator label always
wins; discovery never overwrites it. To set it by hand, the same pattern
`kairon.zyvor.dev/storage-domain`/`network-domain`/`vfio-devices` use:

```bash
kubectl label node worker-1 kairon.zyvor.dev/pinnable-cpus="2-15"
```

The value uses Linux's `cpuset.cpus` list syntax with `_` between entries
(`"2-15"` or `"2_3_4-8_20"`), because a label value cannot contain `,` and
is limited to 63 characters. Start from
`cat /sys/fs/cgroup/cpuset.cpus.effective`, replace `,` with `_`, after excluding
whatever cores you want reserved for the OS, `kairon-node` itself, and any
kubelet-managed (Guaranteed-QoS) Pods already running real workloads on
that node. A hand-set label is not checked against kubelet: if it includes
a core kubelet later grants exclusively to a Pod, nothing detects the
collision. Discovery reads kubelet's `cpu_manager_state`, which is
kubelet-internal and not a stable API, so it refuses rather than guesses
when the file is missing or unexpected. **A node with no `pinnable-cpus`
label has zero pinnable CPUs -- fail-closed**, the same posture an empty
`KAIRON_VFIO_ALLOWLIST` already has.

## Requesting real pinning

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: latency-sensitive-db}
spec:
  image: {path: /var/lib/fluxvm/images/db.qcow2}
  resources:
    cpu: "4"
    memory: 8Gi
    cpuPinning: true
  runtime: {backend: qemu}
  powerState: Running
```

At scheduling time, `kairon-controller` filters out any candidate node
without at least `spec.resources.cpu` free, currently-unclaimed cores from
its `pinnable-cpus` label, then deterministically picks the cores (a
Machine `Pending` with a clear message if no node has enough). The chosen
core numbers land in `spec.resources.allocatedCpuSet` -- **system-computed,
the same relationship `spec.nodeName` itself already has to the
scheduler: not something you set yourself.** `kairon-node`
applies it to the live FluxVM cgroup the same reconcile pass that already
applies `spec.resources.limits`
([`machine-resource-limits.md`](machine-resource-limits.md)) -- both merge
into one FluxVM call, never two competing writes.

## Real limits today (first cut)

- **QEMU only**, matching every other NUMA/cpuSet/hugepages field.
- **Not NUMA-topology-aware on its own.** Add `spec.topology` (see
  [topology-aware placement](machine-topology.md)) to have the scheduler
  pick cores per NUMA node and FluxVM bind guest memory to them. Without
  it, core selection is deterministic first-fit
  over the `pinnable-cpus` label's own ascending order -- it does not
  cross-validate against `spec.resources.numaNode` even when both are set
  on the same Machine. If you need pinned cores to also fall within a
  specific NUMA node, the `pinnable-cpus` label itself must already be
  scoped to that node's own cores (an operator responsibility).
- **Creation-time allocation only.** `spec.resources.allocatedCpuSet` is
  computed once, at initial scheduling -- there's no live re-allocation if
  a Machine's `spec.resources.cpu` changes later (hotplug doesn't grow the
  pinned set) or if the node's own `pinnable-cpus` label changes after the
  fact.
- **Races across reconcile ticks are possible in principle.** There is no
  distributed lock in this codebase -- two Machines scheduled in the same
  `kairon-controller` reconcile pass are allocated from the same
  consistent snapshot (safe), but `kairon-controller` itself doesn't run
  more than one replica actively reconciling at once (leader election
  already guarantees this, see
  [`kairon-controller-ha.md`](kairon-controller-ha.md)), so this is a
  theoretical, not a practically-observed, risk today.
- **No enforcement against kubelet's own CPU Manager beyond what the
  operator excludes from the label.** If the operator's `pinnable-cpus`
  label includes a core kubelet's static CPU Manager later exclusively
  grants to a new Guaranteed-QoS Pod, nothing here detects or prevents
  that collision -- keeping the label accurate as node workloads change is
  an ongoing operator responsibility, not something Kairon verifies.
- **No live-migration compatibility checking** -- the same posture
  `machine-cpu-numa.md`'s own NUMA fields already have; a pinned Machine
  can attempt migration to a target with a completely different
  `pinnable-cpus` label, and whether that's sensible is between you and
  the target node's own topology.
- **No dashboard support** -- `spec.resources.cpuPinning` is spec-only.

## Discovered set

kairon-node reads `/sys/devices/system/cpu/online` and kubelet `cpu_manager_state`. It publishes `kairon.zyvor.dev/pinnable-cpus` only when it can prove the set, and marks it with `kairon.zyvor.dev/pinnable-cpus-source=discovered`. An operator label without that annotation is left alone. If discovery later fails, only a discovered label is cleared, so a Machine that asks for `cpuPinning` fails closed on a node that cannot prove its cores.

The set is online CPUs minus `--reserved-cpus` minus CPUs kubelet assigned exclusively to pods (`entries` in `cpu_manager_state`). `defaultCpuSet` is not subtracted: under the `static` policy it is the shared pool, so subtracting it would leave nothing.

Discovery refuses, and publishes no label, when:

- `--reserved-cpus` (`KAIRON_RESERVED_CPUS`) is empty. kubelet's own reserved CPUs are not in `cpu_manager_state`, so kairon-node cannot infer them. Set it to at least kubelet's `reservedSystemCPUs`.
- `cpu_manager_state` cannot be read or parsed (`--cpu-manager-state`, default `/var/lib/kubelet/cpu_manager_state`). kubelet writes it `0600 root`, and kairon-node runs unprivileged in both install layouts, so it needs a readable copy; see "Turning on discovery" below.
- The online set or any cpuset does not parse.
- The result does not fit the 63-character label limit.

A refusal is logged once per change, not every reconcile. With `--reserved-cpus` unset, discovery is simply off and kairon-node logs that at info level.

Under kubelet's `none` policy there are no exclusive pod CPUs, so every
online CPU outside `--reserved-cpus` is published, including cores that
ordinary Pods also run on. Reserve generously on nodes that also run Pods.

## Turning on discovery

kubelet's state file is root-only and replaced by rename on every write.
Both install layouts copy it with a small root-side helper instead of
giving kairon-node extra privileges.

**systemd host (`scripts/deploy-remote.sh`):**

```bash
./scripts/deploy-remote.sh HOST USER --reserved-cpus=0-1 --cpu-pinning
```

`--reserved-cpus` sets `KAIRON_RESERVED_CPUS` in
`/etc/kairon/kairon-node.env`, even if the file already exists (other keys
are left alone). `--cpu-pinning` installs `kairon-cpustate.path`, which
watches the kubelet file, and `kairon-cpustate.service`, which copies it
atomically to `/var/lib/kairon-node/cpu_manager_state` (`root:kairon`,
`0640`); it also sets `KAIRON_CPU_MANAGER_STATE` to that copy.
`--no-cpu-pinning` removes the units, the copy and both env keys again.
kairon-node removes a label it published on its own once discovery
refuses; clear an operator-set label yourself.

The units live in `systemd/`. `kairon-cpustate.path` fires
`kairon-cpustate.service` whenever `/var/lib/kubelet/cpu_manager_state`
changes (`PathChanged=`). The service is a oneshot: it creates
`/var/lib/kairon-node` (`0700`, `kairon:kairon`), copies the kubelet file to
`.cpu_manager_state.tmp` there as `root:kairon` `0640`, and renames it over
`cpu_manager_state`, so a reader never sees a missing or half-written file.
The service has `ConditionPathExists=/var/lib/kubelet/cpu_manager_state`, so
on a host without kubelet's CPU manager state file it silently does nothing:
systemd skips the start (visible only as a skipped condition in
`systemctl status kairon-cpustate.service`), no copy is made, and discovery
keeps refusing. `deploy-remote.sh` warns at install time if the file is not
there yet.

**Helm:**

```yaml
node:
  cpuPinning:
    enabled: true
    reservedCPUs: "0-1"   # kubelet's reserved CPUs; required
```

This adds a native sidecar (Kubernetes 1.29+) running as root with all
capabilities dropped. It mounts the state file's directory read-only
(`/var/lib/kubelet` by default, which also holds other pods' volume data,
so treat the sidecar image like any other privileged node component) and
copies the file every `copyIntervalSeconds` into a pod-private in-memory
volume. kairon-node gets `--reserved-cpus` and `--cpu-manager-state`
pointing at the copy and stays non-root. Its startup probe keeps
kairon-node from starting until the first copy exists. Set `stateFile` if
kubelet uses a non-default root directory.

Check the result with
`kubectl get node NODE -L kairon.zyvor.dev/pinnable-cpus`.
