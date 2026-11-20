# User guide: guest quiesce for MachineSnapshot

How `MachineSnapshot` gets an application-consistent snapshot instead of
a merely crash-consistent one, for a Machine with `spec.guestAgent.enabled`
-- and what happens when the guest agent doesn't cooperate.

## What "quiesce" means here

Snapshotting a `PersistentVolumeClaim` while its filesystem has in-flight,
unflushed writes only ever gets you a **crash-consistent** copy -- the
same state you'd get from pulling the power cord, not corrupt, but not
guaranteed to reflect a clean shutdown either. Real qemu-guest-agent
support for `guest-fsfreeze-freeze`/`guest-fsfreeze-thaw` (the same
mechanism libvirt/virt-manager use) flushes and freezes every mounted,
writable filesystem inside the guest immediately before the snapshot is
taken, and thaws it again right after -- an **application-consistent**
snapshot, safe to restore without an implicit fsck/replay-journal step.

This is automatic and requires no extra fields on `MachineSnapshot`
itself -- it activates whenever the target Machine has
`spec.guestAgent.enabled: true`
([`machine-guest-agent.md`](machine-guest-agent.md)):

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: db}
spec:
  guestAgent: {enabled: true}
  volumes: [{name: root, claimName: db-root-pvc}]
  # ...
---
apiVersion: kairon.zyvor.dev/v1
kind: MachineSnapshot
metadata: {name: db-snap-1}
spec:
  machineName: db
```

Watch it with `kaironctl get snapshots` (or `kubectl get machinesnapshot
db-snap-1 -o yaml`) -- `status.phase` now passes through `Freezing` and
`Thawing` around the existing `Pending`/`Succeeded` states for a
guest-agent-enabled Machine.

## Why this needs a request/response protocol, not a direct call

`kairon-controller` (which reconciles `MachineSnapshot`) has no network
path to a Machine's FluxVM instance at all -- only `kairon-node`, running
on the same host, does. So freezing/thawing is coordinated through two
annotations on the **Machine** object itself, the same "durable request
in the API, not memory" pattern `kairon.zyvor.dev/adopt-only` already
uses for controller↔node coordination during a live migration cutover:

1. `kairon-controller` patches `kairon.zyvor.dev/quiesce-request` onto the
   Machine, naming this `MachineSnapshot` and a timestamp.
2. `kairon-node`'s own reconcile (already running per-tick for every
   Machine it owns) sees the request, calls FluxVM's real
   `guest-fsfreeze-freeze`, and confirms by setting
   `kairon.zyvor.dev/quiesce-status` to the same value.
3. `kairon-controller` sees the confirmation, creates the underlying CSI
   `VolumeSnapshot`(s), then immediately clears `quiesce-request` --
   *before* waiting for the `VolumeSnapshot`s to actually report ready
   (holding the guest frozen through CSI's own, potentially slow, async
   provisioning would be an unbounded, guest-visible stall).
4. `kairon-node` sees the request cleared while its own status annotation
   still shows frozen, calls `guest-fsfreeze-thaw`, and clears the status
   annotation to confirm.
5. `kairon-controller` only lets the snapshot reach `Succeeded` once the
   thaw is confirmed **and** every `VolumeSnapshot` is ready.

## What happens when the guest agent doesn't cooperate

- **Freeze never confirms** (guest not actually running the agent despite
  the spec flag, a network partition, a hung guest): `kairon-controller`
  waits up to 30 seconds, then gives up, clears the request, and proceeds
  with an unquiesced, crash-consistent snapshot anyway --
  `status.message` says so explicitly. A snapshot is never blocked
  forever by an uncooperative guest agent.
- **Thaw never confirms**: **no timeout** here, deliberately --
  `guest-fsfreeze-thaw` is safe to retry indefinitely (idempotent per
  QGA's own semantics), and a snapshot that silently reported `Succeeded`
  while the guest was still frozen would leave an operator with no
  explanation for what looks like a hung VM. `kairon-node` keeps retrying
  the thaw every reconcile tick until it succeeds, and the
  `MachineSnapshot` stays in `Thawing` until it does -- the same "never
  silently abandon" posture `MachineMigration`'s own `NeedsRecovery` phase
  already has.

## Deleting a `MachineSnapshot` while the guest is frozen

Deleting a `MachineSnapshot` mid-`Freezing`/`Thawing` is safe: the moment
`kairon-controller` first requests a freeze (step 1 above), it also adds
the `kairon.zyvor.dev/snapshot-quiesce` finalizer to the `MachineSnapshot`
itself. If the object is deleted before the guest is confirmed thawed,
`kairon-controller` requests the thaw (clearing `quiesce-request`, same as
the normal completion path) and holds the finalizer until `kairon-node`
confirms it -- retried indefinitely, the same "never silently abandon a
frozen guest" posture the thaw side already has above. Only once the
guest is confirmed not frozen on this snapshot's behalf (or its target
Machine is gone too, with nothing left to thaw) does deletion actually
proceed. A `MachineSnapshot` that never enabled guest quiesce, or hasn't
reached its first freeze request yet, never gets the finalizer and
deletes immediately, exactly as before this existed.

## Real limits today (first cut)

- **QEMU backend only** -- qemu-guest-agent is a QEMU-specific channel;
  Cloud Hypervisor/Firecracker Machines have no equivalent today.
- **All-or-nothing across every `spec.volumes` entry.** A Machine with
  multiple volumes gets one freeze covering all of them, not
  per-volume-selective quiescing.
- **No manual override.** There's no way to force a crash-consistent
  snapshot on a guest-agent-enabled Machine (or vice versa) short of
  toggling `spec.guestAgent.enabled` itself.
- **The freeze-timeout fallback is silent about *why* the guest didn't
  confirm** -- `status.message` says it timed out, not whether the guest
  agent is unreachable, not installed, or just slow. Check the guest
  itself (or `kairon-node`'s own logs) to tell those apart.
