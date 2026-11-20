# User guide: VM-state snapshot and restore (hibernate/checkpoint)

How to checkpoint a running Machine's full hypervisor state -- RAM, CPU,
and device state -- and restore it later, in place. API-only today (no
dashboard button yet), the same first-cut posture
[guest exec's own fsfreeze-status/firewall endpoints](machine-guest-exec.md#related-smaller-api-only-endpoints)
shipped with.

## What this is, and what it isn't

This is **not** [`MachineSnapshot`](machine-snapshot-restore.md) --
that one is Kairon's CSI-backed, disk-**content**-only snapshot (a
point-in-time copy of the backing disk file, restored into a brand-new
PVC/Machine). This feature is a full **hypervisor-level checkpoint** of a
Machine's *running* state via FluxVM's own real QEMU `savevm` (or a Cloud
Hypervisor snapshot directory) -- everything CSI's snapshot can't capture:
RAM contents, CPU register state, and device state, restored back into the
*same* Machine, in place.

Two REST endpoints, admin-gated (the same posture as
[guest exec](machine-guest-exec.md) and
[guest file access](machine-guest-agent-files.md) -- this is at least as
disruptive as either):

- **`POST /api/v1/machines/{ns}/{name}/vm-snapshot`** (body:
  `{"tag": "before-upgrade"}`) -- saves a checkpoint tagged `tag`. The
  Machine must be `Running` or `Paused`; it keeps running (or stays
  paused) throughout -- this never stops or restarts anything by itself.
- **`POST /api/v1/machines/{ns}/{name}/vm-restore-snapshot`** (body:
  `{"tag": "before-upgrade"}`) -- restores the Machine to exactly that
  checkpoint. **This always stops the Machine and starts it back up from
  the snapshot**, whatever state it was in going in -- FluxVM's own
  `start-from-snapshot` silently ignores the tag entirely on an
  already-running VM, so Kairon orchestrates a real stop, then
  start-from-snapshot, itself. Expect a genuine (brief) interruption, not
  a live in-place swap.

Under the hood: `browser/API client -> kairon-ui -> kairon-node
(internal/consoleproxy) -> FluxVM's own POST /v1/vms/{id}/snapshot`,
`/start-from-snapshot`, and `/stop`. No new `spec` field -- eligibility is
based on the Machine's current `status.phase`/`status.runtimeID`, not
anything declared up front.

## Real limits today (first cut)

- **Firecracker doesn't support this at all.** FluxVM's own clear error
  ("snapshot not supported for backend Firecracker") surfaces unmodified;
  Kairon doesn't pre-check `spec.runtime.backend` before asking.
- **No dashboard button yet** -- REST API only (`kaironctl` has none
  either). A UI panel is a reasonable future addition once real usage
  shows what workflow people actually want (list saved tags? one-click
  restore?) -- speculative to build ahead of that.
- **No snapshot listing or deletion.** FluxVM itself owns tag storage;
  Kairon has no endpoint to enumerate or prune tags on a Machine. Keeping
  track of what you've tagged, and cleaning up old ones, is on you today.
- **Restoring genuinely stops the Machine first**, even if it's currently
  `Running` -- there is no live, zero-interruption path back to a
  checkpoint. If the restore's own `start-from-snapshot` step fails after
  the stop already succeeded, the Machine is left FluxVM-stopped with its
  disk/record intact (not deleted) -- Kairon's own reconcile loop notices
  the mismatch against `spec.powerState` on its next tick and starts the
  Machine back up from its plain last-known-good disk state (**not** a
  second automatic restore attempt) as a safety net. Retry the restore
  call yourself if you actually wanted the snapshot back.
- **No admission-time guard against snapshotting/restoring mid-migration**
  -- the same posture [pause/resume](machine-pause-resume.md) documents
  for the same reason: `spec.powerState`-driven operations and
  `MachineMigration` are reconciled independently.
