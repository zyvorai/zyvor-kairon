# Runbook: fencing a dead node and stale evacuation

Use this when a node running Machines is gone and you want its Machines
placed on other nodes. Background and the design reasoning are in
[`docs/guides/machine-fencing.md`](guides/machine-fencing.md); this page is
the procedure.

Kairon never reschedules a Machine off an unreachable node by itself. If the
node is only partitioned, FluxVM may still be running the VM, and a second
copy elsewhere is split-brain (two writers on one disk). Fencing is
therefore an **operator attestation**: you tell Kairon, with `--reason`, that
the node is truly gone. Kairon cannot verify it.

## Step 0: make the node really dead

Do this outside Kairon first. Power the host off through IPMI/iDRAC/BMC or
your cloud API, or confirm a hardware failure or deliberate decommission.
A reboot, a network partition that may clear, or a guess is not a reason.

## Step 1: confirm Kairon sees the node as unreachable

```bash
kubectl get node worker-3
kubectl get machine web-1 -n NS -o jsonpath='{.status.conditions[?(@.type=="NodeUnreachable")]}'
```

`kairon-controller` sets `NodeUnreachable=True` on a Machine when its
`spec.nodeName` is not a Ready, present Node (reason `NodeNotReadyOrMissing`),
or, with the liveness Lease enabled, when the Node is Ready but kairon-node's
Lease is stale (reason `AgentLivenessStale`). `kaironctl fence` refuses to run
unless this condition is `True`.

## Option A: fence one Machine

```bash
kaironctl fence web-1 --namespace NS \
  --reason "ipmi power-off confirmed 14:02, ticket OPS-412"
```

`--reason` is required and is recorded verbatim in a `Fenced` condition
(reason `OperatorAttested`).

What it does: clears `spec.nodeName` and the status fields tied to the old
runtime (phase, node name, runtime ID, guest IPs, network, applied CPU and
memory) and drops the `kairon.zyvor.dev/runtime-cleanup` finalizer, which only
the dead node's kairon-node could have removed. On the next controller tick
the scheduler treats the Machine as new and places it.

Optional extra gate, if you run kairon-node liveness Leases
(`node.livenessLease.enabled`):

```bash
kaironctl fence web-1 --namespace NS --reason "..." \
  --liveness-lease-namespace kairon-system
```

`fence` then refuses if that node's kairon-node renewed its Lease recently.
A missing Lease or a read error lets it proceed with a warning.
`--force-ignore-liveness` overrides a refusal; use it only if you have
independent proof the agent is down.

## Option B: attest the node and let the controller evacuate (stale evacuation)

Preconditions (all must hold before the controller acts):

1. `kairon-controller` runs with `--stale-evacuation` (env
   `KAIRON_STALE_EVACUATION`; Helm `controller.staleEvacuation.enabled`,
   default off). `--stale-evacuation-max-per-tick` (Helm
   `controller.staleEvacuation.maxPerTick`) defaults to 10.
2. The Node carries a non-empty `kairon.zyvor.dev/node-fenced` annotation.
3. The Node is not Ready, or (with `--node-liveness-lease-namespace` set) its
   liveness Lease is stale. A fresh Lease, or a Lease read error, refuses.
   A Ready node with no stale Lease is ignored.
4. Each Machine to move has `kairon.zyvor.dev/evacuate=true`, shows
   `NodeUnreachable=True`, is not being deleted and has no unfinished
   MachineMigration.
5. Every MachineDisruptionBudget selecting it still allows a disruption.

Commands:

```bash
# once per dead node, after the power-off is confirmed
kaironctl node fence worker-3 --reason "ipmi power-off confirmed, OPS-412"

# once per Machine that may move without a human
kubectl annotate machine web-1 -n NS kairon.zyvor.dev/evacuate=true
```

The controller then applies the same transition as `kaironctl fence`, with
`Fenced` reason `StaleEvacuation` and message
`stale-evacuation: node "worker-3" attested dead: <your reason>`. Machines on
a fenced node without the `evacuate` annotation are logged once and left for
`kaironctl fence`. Nothing is migrated; the VM died with its node.

## Verify

```bash
kubectl get machine web-1 -n NS -o wide              # new node, phase moving to Running
kubectl get machine web-1 -n NS -o jsonpath='{.status.conditions[?(@.type=="Fenced")]}'
kubectl get node worker-3 -o jsonpath='{.metadata.annotations.kairon\.zyvor\.dev/node-fenced}'
```

The controller emits no Kubernetes Events for stale evacuation. Look in the
controller log for `stale evacuation: fenced machine off dead node`, `blocked
by disruption budget`, `per-tick limit reached` or `fence failed`.

## Undo

Fencing a Machine cannot be reverted: the old runtime state is cleared. What
you can undo is the node attestation, which stops further evacuations:

```bash
kaironctl node fence worker-3 --clear
```

Do this once the node is repaired or retired. Also remove the
`kairon.zyvor.dev/evacuate` annotation from Machines that should again wait
for a human.

## Split-brain warning

If the old node comes back with the FluxVM instance still running after you
fenced and the Machine was rescheduled, two runtimes are alive for one
Machine and may both write the same disk. Kairon does not detect or stop
this. Keep the old host powered off, or wipe/reimage it, before it rejoins.

Whoever can patch Nodes can set `kairon.zyvor.dev/node-fenced` and so make
opted-in Machines restart elsewhere. Restrict `patch` on nodes and on
Machine annotations to the operators and tooling that do the power fencing.
The behaviour has not been exercised against a real multi-host failure in this
repository's CI.
