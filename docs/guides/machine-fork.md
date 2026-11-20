# Forking a running Machine

`kaironctl fork` turns one Running Machine into N live copies on the same
node. FluxVM snapshots the parent once (a pause of a few milliseconds) and
restores every child from that snapshot, so each child starts with the
parent's memory, CPU state and a copy-on-write copy of its disk. Processes
that were running in the parent keep running in every child.

Use it to branch an agent sandbox before a risky step, fan a warmed-up
workload out to N trials, or explore several actions from one state.

## Example

```bash
kaironctl fork base --count 4 --prefix trial
# ✓ forked machine/base into trial-1, trial-2, trial-3, trial-4
kaironctl get machines --selector kairon.zyvor.dev/forked-from=base
kaironctl delete machine trial-3
```

The MCP server (`kaironctl mcp serve --allow-write`) exposes the same
operation as `fork_machine`.

## How it works

`kaironctl fork` (or `fork_machine`) creates the children as ordinary
Machines:

| Field | Value |
| --- | --- |
| `spec` | Copy of the parent's spec |
| `spec.nodeName` | The parent's node |
| `metadata.annotations["kairon.zyvor.dev/fork-from"]` | Parent name |
| `metadata.labels["kairon.zyvor.dev/forked-from"]` | Parent name |

The parent's own labels are not copied, so a child never counts as a
MachineSet, MachinePool or MachineClaim member. Children go through
admission like any other Machine and count against `MachineQuota`.

When kairon-node reconciles a child that has no runtime yet, it calls
FluxVM's `POST /v1/vms/{id}/fork` on the parent's runtime with
`count: 1`, instead of booting a new VM. FluxVM names the child runtime
`kairon-<namespace>-<child>-1`. If a status update is lost after the fork,
kairon-node finds that runtime by name on the next tick and adopts it
instead of forking twice.

## Requirements and limits

- The parent must be `Running` and use FluxVM's `flux-vm` backend
  (`spec.runtime.backend: flux-vm` or a `spec.sandbox` Machine). Other
  backends can't be forked.
- Children keep the parent's MAC and guest IP, so the parent must use user
  networking, no network, or a tap in a per-VM network namespace, with no
  extra NICs. FluxVM rejects the fork otherwise.
- Machines with `spec.volumes`, `spec.disks` or `spec.deviceClaims` can't be
  forked: only the boot disk is copied.
- Hostname and machine-id are the parent's. Reset them through the guest
  agent if the children need distinct identities.
- Children are pinned to the parent's node; migrate them afterwards if
  needed.
- Stopping a child discards its runtime. Starting it again forks the
  parent's current state, not the child's old one.
- At most 32 children per call. Each child forks separately, so N children
  take N snapshots of the parent.
