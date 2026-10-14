#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Automated NeedsRecovery drill (docs/runbook-recovery-drill.md, trigger A).
# Starts a live migration, waits for Running, firewalls the destination's
# migration control-plane port so Commit() fails deterministically, waits for
# NeedsRecovery, lifts the block, runs `kaironctl recover`, and verifies the
# outcome:
#
#   --action=confirm-not-committed (default)  retried commit succeeds; the
#       Machine ends Running on the target.
#   --action=force-abort  the migration ends without cutover and the Machine
#       is still Running on the source (the source runtime is never touched).
#
# Usage:
#   recovery-drill.sh --machine=NAME --target=NODE [--namespace=ns]
#       [--dest-ssh=user@host] [--action=confirm-not-committed|force-abort]
#       [--port=9443] [--timeout=900]
#
# --dest-ssh defaults to KAIRON_HW_TARGET_SSH; that user needs passwordless
# sudo for iptables on the destination host. Exits non-zero on any deviation.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
MACHINE=""; TARGET=""; DEST_SSH="${KAIRON_HW_TARGET_SSH:-}"
ACTION="confirm-not-committed"; PORT="${KAIRON_MIGRATION_PORT:-9443}"; TIMEOUT=900
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --machine=*) MACHINE="${arg#*=}" ;;
    --target=*) TARGET="${arg#*=}" ;;
    --dest-ssh=*) DEST_SSH="${arg#*=}" ;;
    --action=*) ACTION="${arg#*=}" ;;
    --port=*) PORT="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$MACHINE" && -n "$TARGET" ]] || { echo "usage: $0 --machine=NAME --target=NODE [--dest-ssh=user@host]" >&2; exit 1; }
[[ -n "$DEST_SSH" ]] || { echo "set --dest-ssh or KAIRON_HW_TARGET_SSH (destination host for the iptables block)" >&2; exit 1; }
case "$ACTION" in confirm-not-committed|force-abort) ;; *) echo "unknown --action $ACTION" >&2; exit 1 ;; esac

SOURCE="$(machine_node "$NAMESPACE" "$MACHINE")"
[[ -n "$SOURCE" && "$SOURCE" != "$TARGET" ]] || { echo "Machine $MACHINE is on '${SOURCE:-<none>}'; pick a different --target" >&2; exit 1; }

drop_rule() { lab_ssh "$DEST_SSH" "sudo iptables -C INPUT -p tcp --dport $PORT -j DROP 2>/dev/null || sudo iptables -I INPUT -p tcp --dport $PORT -j DROP"; }
remove_rule() { lab_ssh "$DEST_SSH" "while sudo iptables -D INPUT -p tcp --dport $PORT -j DROP 2>/dev/null; do :; done" || true; }
trap remove_rule EXIT

MIG="$(start_migration "$NAMESPACE" "$MACHINE" "$TARGET" live drill)"
lab_log "migration $MIG: $MACHINE $SOURCE -> $TARGET"

wait_until "$TIMEOUT" "$MIG Running" migration_in "$NAMESPACE" "$MIG" Running Failed Blocked Succeeded
phase="$(migration_phase "$NAMESPACE" "$MIG")"
[[ "$phase" == "Running" ]] || { lab_log "migration reached $phase before Running; nothing to drill"; exit 1; }

lab_log "arming DROP on $DEST_SSH tcp/$PORT"
drop_rule
wait_until "$TIMEOUT" "$MIG NeedsRecovery" migration_in "$NAMESPACE" "$MIG" NeedsRecovery Failed Succeeded Blocked
phase="$(migration_phase "$NAMESPACE" "$MIG")"
[[ "$phase" == "NeedsRecovery" ]] || { lab_log "landed $phase instead of NeedsRecovery"; exit 1; }
lab_log "NeedsRecovery reached; lifting the block"
remove_rule

if [[ "$ACTION" == "force-abort" ]]; then
  "$KAIRONCTL" recover "$MIG" --namespace "$NAMESPACE" --action=ForceAbort --diagnosis=Unknown \
    --reason="automated recovery drill: ForceAbort must leave the source untouched"
  wait_until "$TIMEOUT" "$MIG terminal" migration_in "$NAMESPACE" "$MIG" Failed Cancelled Succeeded
  phase="$(migration_phase "$NAMESPACE" "$MIG")"
  [[ "$phase" != "Succeeded" ]] || { lab_log "ForceAbort ended Succeeded"; exit 1; }
  wait_until 300 "$MACHINE healthy on source $SOURCE" machine_healthy "$NAMESPACE" "$MACHINE" "$SOURCE"
  lab_log "PASS: ForceAbort left $MACHINE running on $SOURCE (migration $phase)"
else
  "$KAIRONCTL" recover "$MIG" --namespace "$NAMESPACE" --action=ConfirmDestinationNotCommitted \
    --diagnosis=DestinationNotCommitted \
    --reason="automated recovery drill: control-plane port firewalled during Running"
  wait_until "$TIMEOUT" "$MIG Succeeded" migration_in "$NAMESPACE" "$MIG" Succeeded Failed
  phase="$(migration_phase "$NAMESPACE" "$MIG")"
  [[ "$phase" == "Succeeded" ]] || { lab_log "retried commit ended $phase"; exit 1; }
  wait_until 300 "$MACHINE healthy on target $TARGET" machine_healthy "$NAMESPACE" "$MACHINE" "$TARGET"
  lab_log "PASS: ConfirmDestinationNotCommitted completed cutover to $TARGET"
fi
