# User guide: live host resource limits (`spec.resources.limits`)

A host-side cap on an already-running Machine's own VMM process cgroup --
distinct from `spec.resources.cpu`/`.memory` (which describe what the
*guest* sees) and from [CPU/memory hotplug](machine-hotplug.md) (which
grows what the guest sees, one-way only).

## What this is

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: noisy-neighbor
spec:
  image: {path: /var/lib/fluxvm/images/app.qcow2}
  resources:
    cpu: "4"
    memory: 8Gi
    limits:
      cpuQuotaPercent: 300      # cap at 3 full cores' worth of CPU time
      memoryMaxBytes: 9663676416 # 9Gi hard ceiling (some headroom over the guest's own 8Gi)
      ioWeight: 50              # lower priority against other cgroups on the same disk
      pidsMax: 4096
  runtime: {backend: qemu}
  powerState: Running
```

This wraps FluxVM's real `POST /v1/vms/{id}/resources` -- a partial patch
to the VMM process's own cgroup v2 controllers (`cpu.max`, `memory.max`,
`io.weight`, `pids.max`), enforced by the Linux kernel itself, not
something Kairon or FluxVM polices in userspace. **Backend-agnostic**:
unlike `hugepages`/`numaNode`/`cpuSet` (QEMU-only), this works for every
backend FluxVM supports, since it operates on the cgroup the VMM process
runs in, not a backend-specific device or API.

`kairon-node` reconciles this continuously (`internal/agent/resourcelimits.go`),
not just at creation -- editing `spec.resources.limits` on a `Running`
Machine takes effect on the next reconcile tick, and unlike hotplug it can
be **raised or lowered freely at any time**: a cgroup limit change has none
of hotplug's "can't unplug a vCPU" one-way asymmetry.

## Watching live usage (`status.resourceUsage`)

Every reconcile tick, `kairon-node` also reads FluxVM's own
`GET /v1/vms/{id}/stats` (the same cgroup-derived, backend-agnostic
mechanism `limits` above enforces against) and projects it into
`status.resourceUsage`:

```yaml
status:
  resourceUsage:
    cpuPercent: 42.5       # % of one core, averaged over the process's whole lifetime
    memoryBytes: 2147483648
    diskReadBytes: 10485760
    diskWriteBytes: 5242880
```

The dashboard shows this inline next to each Machine's CPU/Memory
columns. It's a point-in-time snapshot refreshed every tick, not a time
series -- for historical graphs, use `kairon_reconcile_duration_seconds`-
style Prometheus metrics or your own external monitoring against the
guest itself, not this field.

`kaironctl top machines`/`kaironctl top nodes` gives the same data a
`kubectl top`-style terminal view instead of reading it off individual
Machine objects one at a time: `top machines` prints every Machine's own
`status.resourceUsage` row (`--selector k=v`, repeatable, narrows which
ones count, exactly like `kaironctl get`'s own `--selector`); `top nodes`
rolls those same per-Machine samples up by `spec.nodeName`
(`model.AggregateUsageByNode`, `internal/model/usage.go`) into a
per-host CPU%/memory/Machine-count total -- the fleet-wide hotspot view
`kaironctl get machines` alone can't answer without an operator manually
grouping and summing rows. Deliberately no new metrics pipeline: both
verbs just render data every Machine already reports. `top nodes` is
cluster-wide by construction (`-n`/`--namespace` has no effect, the same
"unused for a cluster-scoped kind" precedent `kaironctl get nodes`
already sets); `top machines` defaults to `-n`/`--namespace` like `get
machines` does. The dashboard's **Nodes** page shows the identical
per-node rollup in its Machines/CPU/Memory columns, via the same
`model.AggregateUsageByNode` call behind `GET /api/v1/nodes/usage`.

## Why this exists

Every Machine here runs as a bare process on the host, not inside a Pod --
so unlike a KubeVirt VM (whose `virt-launcher` Pod gets Kubernetes' own
resource `limits` enforcement for free), a Kairon Machine had no host-side
enforcement of anything beyond the scheduler's own bin-packing count. This
closes that gap directly: a real, kernel-enforced ceiling on CPU/memory/IO/
process count, independent of whatever the guest itself believes it has.

## What this deliberately doesn't include

**No `cpuSetCPUs`.** FluxVM's own `ResourcePatch` also supports pinning a
VM to specific host CPU cores (`cpuset_cpus`), but Kairon doesn't expose
it here -- allocating *specific*, non-overlapping host CPU numbers across
every Machine competing for them on one node is a real capacity-allocation
problem `internal/scheduler`'s still pure count-based bin-packing doesn't
solve (the same reason `spec.resources.cpuSet` is a guest-visible hint
only, not real pinning -- see [`machine-cpu-numa.md`](machine-cpu-numa.md)'s
"What this is not" section). Exposing `cpuSetCPUs` here would let two
Machines silently claim overlapping host cores with nothing to stop them;
that's a bigger, separate scheduler feature, not attempted in this cut.

## Real limits today (first cut)

- **No validation that limits are internally consistent with
  `spec.resources.cpu`/`.memory`** -- setting `memoryMaxBytes` below what
  the guest was booted with, for instance, is accepted and forwarded to
  FluxVM as-is; whatever happens next (likely OOM-killing the VMM process)
  is between you and the kernel, not something Kairon checks first.
- **Requires the Machine to already be running.** FluxVM's own error
  ("VM has no cgroup: not running, or cgroup setup failed at launch")
  surfaces as-is if `spec.resources.limits` is set before the Machine ever
  reaches `Running` -- retried automatically on the next tick once it does.
- **No way to explicitly clear a limit back to "unset."** FluxVM's own
  `ResourcePatch` only ever touches fields present in the request; there's
  no "reset to no limit" call. Removing `spec.resources.limits` (or one of
  its fields) from the Machine leaves the last-applied cgroup value in
  place rather than lifting it -- `status.appliedResourceLimits` always
  reflects what's actually enforced, not what the current spec asks for,
  precisely so this isn't a silent surprise.
- **No admission-time bounds checking** -- an absurd value (e.g.
  `cpuQuotaPercent: 0`) is rejected by FluxVM's own cgroup write, not
  caught earlier by Kairon.
