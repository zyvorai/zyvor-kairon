# Runbook: migration failures and NeedsRecovery

## When you land here

You were paged by `KaironMigrationNeedsRecoveryStuck` or `KaironMigrationFailureRateHigh` (see `charts/kairon/alerts.yaml`), or you noticed `phase: NeedsRecovery` while inspecting a `MachineMigration` directly:

```
kubectl get machinemigration -A
```

## Why `NeedsRecovery` exists

Kairon never auto-resolves an ambiguous commit. From `internal/agent/agent.go`'s `reconcileNeedsRecovery`:

> `reconcileNeedsRecovery` never auto-resolves a NeedsRecovery migration -- every tick it only refreshes a live diagnosis of what's actually known (`status.Recovery`'s diagnosis fields), and acts *only* when the operator has set `spec.recovery` with an `AcknowledgedDiagnosis` that matches the requested `Action` and a non-empty `Reason`.

This happens when the source's transfer completed but the commit RPC to the destination failed or timed out -- Kairon genuinely cannot tell, on its own, whether the destination ended up running the guest or not. Guessing wrong risks either two live copies of the same guest, or none. See `docs/architecture.md` and `SECURITY.md` for the full invariant.

## Diagnose

```
kubectl get machinemigration -n NAMESPACE NAME -o yaml
```

Read `status.recovery` -- refreshed by the source node's agent every reconcile tick, no action needed to force an update:

- `sourceRuntimeStatus` -- the source FluxVM runtime's current status (queried directly).
- `destinationSessionPhase` -- the destination's own durable session-store phase (`Prepared`/`Committed`/...). **Not proof on its own** that the destination didn't commit -- see the caveat below.
- `destinationRuntimeFound` / `destinationRuntimeStatus` -- ground truth from a live lookup of the destination's FluxVM runtime by its deterministic name.
- `diagnosedAt` -- when this snapshot was last refreshed.

These are the exact same fields `kaironctl recover NAME` prints before it requires `--action`/`--diagnosis`/`--reason` -- you can just run that command to see them without any `kubectl` YAML-wrangling.

**Caveat**: `destinationSessionPhase == "Prepared"` is *not* proof the destination never committed. The destination's session store only advances to `"Committed"` after both the inner adapter commit *and* a subsequent network-migration-resume step both succeed -- if the adapter-level commit actually succeeded but the network step failed afterward, the destination guest can be genuinely running while the store still says `"Prepared"`. Always cross-check `destinationRuntimeFound`/`destinationRuntimeStatus`, not just `destinationSessionPhase`, before choosing an action.

## Decision tree

| What you observed | Action | Acknowledged diagnosis |
|---|---|---|
| `destinationRuntimeFound: true` and `destinationRuntimeStatus` looks healthy/running | `ConfirmDestinationCommitted` | `DestinationCommitted` |
| `destinationRuntimeFound: false`, or clearly never provisioned | `ConfirmDestinationNotCommitted` | `DestinationNotCommitted` |
| Signals contradictory, or you had to inspect the host directly (QMP/FluxVM API) to be sure | `ForceAbort` | `Unknown` |

`reconcileNeedsRecovery` rejects the action outright (no state change, `status.message` explains why) unless `AcknowledgedDiagnosis` matches `Action` exactly and `Reason` is non-empty -- it will not accept a guess. `Reason` must be a real description of what you observed, not a placeholder.

`ConfirmDestinationCommitted` and `ConfirmDestinationNotCommitted` also refuse while the diagnosis shows the destination runtime as `failed`, `stopped`, `error` or `exited`: both delete the source runtime, which may then be the only copy that can still run the guest. The migration stays in `NeedsRecovery` with a `recovery refused` message and the action is not recorded as applied. For `ConfirmDestinationNotCommitted` the destination is diagnosed again after the retried commit; if it is failed then, the migration stays in `NeedsRecovery` with a `recovery paused` message and the source is kept. Inspect the destination host, then fix it and retry, or use `ForceAbort`.

## Command reference

```
kaironctl recover MIGRATION --action ACTION --diagnosis DIAGNOSIS --reason REASON [-n NAMESPACE]
```

- `ACTION`: `ConfirmDestinationCommitted` | `ConfirmDestinationNotCommitted` | `ForceAbort`
- `DIAGNOSIS`: `DestinationCommitted` | `DestinationNotCommitted` | `Unknown`

Effects:
- `ConfirmDestinationCommitted` / `ConfirmDestinationNotCommitted` (after a successful retried commit): the source runtime is deleted, migration proceeds to `Cutover` and on through the normal state machine.
- `ConfirmDestinationNotCommitted` (if the retried commit fails again): migration lands `Failed`; the source runtime is **not** deleted -- inspect it manually before reuse.
- `ForceAbort`: the destination session is aborted (a 409 here is expected/harmless if it secretly did commit); migration lands `Failed`. The source runtime is **deliberately never touched** in this path -- when ground truth is unknown, destroying the only possibly-live copy of the guest is out of bounds. Verify the source host's actual VM state manually before reusing the Machine.

## Verify recovery took effect

```
kubectl get machinemigration -n NAMESPACE NAME -o yaml
```

Confirm `status.recovery.appliedAction`/`appliedReason`/`appliedAcknowledgedDiagnosis`/`appliedAt` are populated (a permanent audit record, independent of later `spec.recovery` edits), and that `status.phase` progressed to `Cutover`/.../`Succeeded` or `Failed` as expected.

## Failed after post-copy started

A live migration that switched to post-copy (immediate `mode: post-copy`, or an adaptive migration's `PostCopy` stage) and then failed also lands in `NeedsRecovery`, with `status.adaptive.postCopyStarted: true` and a message saying the source was not resumed. Here the guest's newest memory is on the destination and the rest on the source, so neither copy is complete. Kairon deliberately left both runtimes alone, and cancel is refused from the moment post-copy starts.

Inspect both hosts before acting. If the destination guest is running and healthy (QEMU completed post-copy despite the reported failure), use `ConfirmDestinationCommitted`. If the destination guest is gone or paused mid-post-copy, the guest state is lost: restore from the most recent snapshot or backup rather than resuming the source, whose memory is stale. Use `ForceAbort` with diagnosis `Unknown` to close the migration out, then repair the Machine.

## Stuck in flight (`KaironMigrationStuckInFlight`) -- cancelling instead of waiting

Unlike `NeedsRecovery`, a migration merely stuck in `Starting`/`Running` for a long time (the alert's 30-minute default) is not ambiguous -- the destination has not committed yet, so it is always safe to abort. Rather than waiting it out or leaving it to eventually fail on its own:

```
kaironctl cancel-migration NAME [-n NAMESPACE]
```

This patches `spec.cancel: true`; the source node's agent aborts the in-flight transfer (both the source adapter's transfer and the destination's prepared session) on its next reconcile tick and marks the migration `Cancelled` -- the source runtime is left running, untouched, on its current node throughout. The dashboard's Migration detail panel offers the same action as a "Cancel migration" button whenever a migration is in `Starting`/`Running` with `effectiveStrategy: live`.

Cancel only works in that window, and not at all once post-copy has started (`status.adaptive.postCopyStarted`). Both `kaironctl cancel-migration` and the dashboard refuse locally once phase has reached `Cutover` or later (the destination has already committed by then -- aborting would recreate the same split-brain risk `NeedsRecovery` exists to avoid) and for cold-strategy migrations (nothing in-flight to abort). If a migration is stuck past `Cutover`/`Adopting` instead, that is a different problem -- investigate the target node's own health rather than trying to cancel.

## Unencrypted migration data-plane

`KaironMigrationDataPlaneUnencrypted` fires when `status.dataPlaneEncrypted` is `false` for an in-flight live migration -- the QEMU RAM/state stream is crossing the network in cleartext (the control-plane RPCs between kairon-node peers are always mTLS-encrypted regardless; this is specifically about the guest memory transfer itself). This is `info`-severity, not necessarily an incident: `migration.dataplaneTls` defaults to `false` in this chart.

To actually turn encryption on, two things must both be true simultaneously across the fleet:
1. `migration.dataplaneTls: true` in Helm (stages cert material onto every node's `adapterHostPath`). Also set `migration.dataplaneTlsSecretName` for real per-node identity there instead of falling back to the shared control-plane cert -- see [`docs/guides/machine-migration-tls.md`](guides/machine-migration-tls.md).
2. Every node's `kairon-migration-adapter-fluxvm` systemd unit passes `-migration-data-tls=true` with valid `-migration-ca/-migration-cert/-migration-key` -- this is **outside Helm's control**, set directly in the unit's `ExecStart`.

Before flipping either, query whether every currently-active live migration already reports `dataPlaneEncrypted: true` (or watch the `kairon_migration_dataplane_encrypted` metric go to 1 across the board) -- that's the actual signal that every node's adapter is upgraded and configured, not just a guess based on when you think the rollout finished.

Once every node reports encrypted, start `kairon-node` with `--migration-require-encrypted`: a live migration whose destination reports an unencrypted data plane is then aborted before any guest memory moves and parks in `Blocked` (the source is left untouched), instead of only raising the alert.

## Abandoned pre-commit sessions (destination-side, opt-in)

A source `kairon-node` crashing or being fenced *between* a successful `Prepare` and ever calling `Commit`/`Abort` is a different, narrower failure than everything above: `NeedsRecovery` only ever covers an *ambiguous commit result* on a transfer that got far enough to try committing. This case never gets that far -- the destination's reserved incoming-QEMU process just sits there with no source left to release it, and no `MachineMigration` object is stuck in any visible phase to alert on (the source-side object is usually gone or unreachable along with its node).

`kairon-node`'s `--migration-heartbeat-ttl` flag (zero by default, off) closes this: the source heartbeats a live transfer's destination session once per reconcile tick (piggybacked on the same tick that already polls transfer status), and a destination whose `HeartbeatTTL` is configured self-aborts any `Prepared` session that's gone quiet for longer than that -- logged as `session.Reason: "heartbeat timeout: no renewal for over <TTL>, presumed source failure"`, visible via `GET /internal/v1/migrations/{id}` or `.../diagnosis` against the *destination* node directly (not through the Kubernetes API -- there's no CRD-level surface for this yet). Set `--migration-heartbeat-ttl` comfortably larger than several multiples of `--interval` (the safety resync interval; flag default 5m, `deploy-remote.sh` bakes in 3s) to tolerate a transient network blip without reaping a migration that's still actually healthy.

## A Machine is `Blocked`: "incoming FluxVM runtime was not found"

Seen in a repeat test after a live migration reported its transfer complete
and then sat in `Adopting`:

```
phase: Blocked
message: adopt-only cutover guard: incoming FluxVM runtime was not found; refusing to create a duplicate VM
```

The controller had already pointed the Machine at the target and set the
annotations `kairon.zyvor.dev/adopt-only` and `kairon.zyvor.dev/migration`;
the target's node agent looked for the migrated runtime, did not find it, and
(correctly) refused to create a second VM from the same disk.

First establish that the VM really is gone, on **both** nodes, because
removing the guard while a runtime still exists risks two writers:

```bash
# on source and target: is any QEMU / FluxVM runtime for this Machine alive?
ssh NODE 'curl -s localhost:7788/v1/vms | grep -i MACHINE; ps -eo args | grep [q]emu-system | grep MACHINE'
# is the disk open anywhere?
ssh NODE 'sudo lsof /path/to/the/disk.img'
```

If nothing is running and nothing holds the disk, the data on the shared
disk is intact (FluxVM refuses to start a second VM on a disk another host
has open, which is what protects it). Clear the guard so the node agent
recreates the VM from that disk:

```bash
kubectl annotate machine NAME -n NS kairon.zyvor.dev/adopt-only- kairon.zyvor.dev/migration-
```

The VM boots cold from its disk: the guest sees a crash-consistent start.
Then delete or leave the stuck MachineMigration; it is already terminal in
practice.

If you find a runtime on either node, do not clear the annotations. Stop the
duplicate on the node that should not own it first.

## Escalation

- A single `NeedsRecovery` migration, diagnosable and resolvable via the decision tree above: handle it yourself, no escalation needed.
- `KaironMigrationFailureRateHigh` (more than 3 failures in 30 minutes) or multiple simultaneous `NeedsRecovery` migrations: likely a systemic issue (network partition between nodes, a broken adapter deploy, FluxVM instability) rather than isolated bad luck -- escalate and investigate the shared cause before recovering each migration individually.

## Related

- Real multi-host testing and a live `NeedsRecovery` drill (deliberately forcing this state on demand to rehearse the above) are covered separately in `docs/runbook-multi-host-migration-test.md` and `docs/runbook-recovery-drill.md` -- not duplicated here.
