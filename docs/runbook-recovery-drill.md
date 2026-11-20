# Runbook: live `NeedsRecovery` drill

`docs/runbook-migration-failures.md` explains how to *respond* to
`NeedsRecovery`. This runbook is about *deliberately causing* it on a real
two-host setup, so an operator has actually run `kaironctl recover` under
pressure before the first time it matters for real.

**Not executed as part of shipping this runbook** -- like
`docs/runbook-multi-host-migration-test.md`, it needs the same second real
host, which this project's one lab host doesn't provide. Follow it once
that's available; do it on a **disposable Machine**, not anything you care
about -- these triggers deliberately break things.

Prerequisites: the two-host deployment from
`docs/runbook-multi-host-migration-test.md` (steps 1-3), already verified
with at least one successful real migration.

## Automated: Trigger A end to end

`scripts/recovery-drill.sh` runs Trigger A and the recovery in one go, and
is what the hardware matrix's `needs-recovery` case calls:

```
scripts/recovery-drill.sh --machine=MACHINE --target=vm-host-2 \
  --dest-ssh=root@10.0.1.12 --action=confirm-not-committed   # or force-abort
```

It starts a live migration, drops traffic to the destination's control
port (9443) once the migration is Running, waits for `NeedsRecovery`, lifts
the block, runs `kaironctl recover`, and checks the outcome:
`confirm-not-committed` must end `Succeeded` with the Machine healthy on the
target; `force-abort` must end `Failed`/`Cancelled` with the Machine still
healthy on the source. Trigger B stays manual (it races a sub-second window).

## Why this needs two different trigger mechanisms

`reconcileNeedsRecovery`'s three actions map to three different *ground
truths* about the destination (see `docs/runbook-migration-failures.md`'s
decision tree). Reliably drilling each one means reliably forcing that
specific ground truth, and the two truths that matter
(`DestinationNotCommitted` vs `DestinationCommitted`) come from different
places in the code with very different timing characteristics:

- **"Destination never committed"** is caused by `Commit()`'s RPC failing
  outright -- a plain network problem, straightforward to force
  deterministically from outside the destination host.
- **"Destination secretly committed"** requires the *inner* commit
  (`internal/migration.NetworkAwareDestination`'s first step, which
  actually activates the guest on FluxVM) to succeed while a *second*,
  independent step immediately after it fails -- both steps run
  back-to-back on the same host with no network hop between them. There's
  no clean way to inject a failure into just the second step from outside;
  forcing it means racing a sub-second local window.

Trigger A below is deterministic. Trigger B is honestly not -- see its own
header comment for exactly why, verified against
`internal/migration/network.go` and
`cmd/kairon-migration-adapter-fluxvm/main.go`'s real commit code paths.

## Trigger A (deterministic): `ConfirmDestinationNotCommitted` / `ForceAbort`

```
scripts/recovery-drill-trigger-not-committed.sh root@10.0.1.12 MACHINE_NAME vm-host-2
```

Waits for the migration to reach `Running` (so `Prepare()` has already
succeeded), then firewalls off the destination's migration control-plane
port (`iptables ... --dport 9443 -j DROP` by default) on the destination
host only -- transfer-status polling and the actual QEMU RAM stream don't
go through this port, so the transfer itself completes normally; only the
final `Commit()` RPC fails, landing the migration in `NeedsRecovery`
deterministically, every time.

**Verify `ForceAbort`'s safety guarantee** (do this run first, it's the
higher-stakes action): after the script prints the drill point, run

```
kaironctl recover MIG_NAME --namespace=NAMESPACE --action=ForceAbort --diagnosis=Unknown --reason="recovery drill: verifying source is never touched"
```

then confirm on the **source** host that the original guest is still
running, untouched (`agent.go`'s documented guarantee: the source runtime
is deliberately never touched on this path). This is the property most
worth actually rehearsing -- it's the one you're trusting blind under
real pressure.

**Then verify `ConfirmDestinationNotCommitted`** on a fresh drill run: same
script, but recover with

```
kaironctl recover MIG_NAME --namespace=NAMESPACE --action=ConfirmDestinationNotCommitted --diagnosis=DestinationNotCommitted --reason="recovery drill: control-plane port was firewalled off, destination never received Commit()"
```

Confirm the retried commit succeeds, the migration proceeds to
`Cutover`/`Succeeded`, and the **source** runtime is deleted afterward
(unlike `ForceAbort`, this path does delete it -- see the runbook's
Command reference table).

## Trigger B (best-effort, racy): `ConfirmDestinationCommitted`

```
scripts/recovery-drill-trigger-committed.sh root@10.0.1.12 MACHINE_NAME vm-host-2 --attempts=5
```

Runs up to `--attempts` full migrations, each racing to freeze the
destination's FluxVM process the instant the adapter logs `"fluxvm:
receiver activated"` (i.e., right after the guest actually starts running
on the destination, before the network-resume follow-up call that would
otherwise let the session store advance cleanly to `Committed`). **This
may take several attempts, or may not land at all on your hardware** -- the
window is sub-second and the script does not pretend otherwise.

When it does land, verify with

```
kaironctl recover MIG_NAME --namespace=NAMESPACE --action=ConfirmDestinationCommitted --diagnosis=DestinationCommitted --reason="recovery drill: destination confirmed running via live FluxVM lookup"
```

then confirm the destination guest was **never interrupted** during
recovery (no reboot, no pause visible from inside the guest) -- this
action's entire point is recognizing a guest that's already correctly
running and just finishing the paperwork, not touching it.

If all attempts miss: that's a valid, documented outcome for a sub-second
race, not a bug in the drill. `docs/runbook-migration-failures.md`'s
decision tree and the `ForceAbort`/`ConfirmDestinationNotCommitted` drills
above still cover the two actions an operator is far more likely to need
in practice.

## Related

- What to do when this happens for real, not as a drill:
  `docs/runbook-migration-failures.md`.
- Setting up the two-host environment these triggers run against:
  `docs/runbook-multi-host-migration-test.md`.
