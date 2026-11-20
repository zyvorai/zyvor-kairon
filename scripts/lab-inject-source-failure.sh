#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Source failure during transfer: starts a live migration, waits for Running,
# stops kairon-node on the source host for --outage seconds, then restarts it.
# Whatever the migration's outcome, the Machine must end with exactly one
# healthy runtime: on the target if the migration Succeeded, on the source if
# it Failed/Cancelled, or after `kaironctl recover` if it landed in
# NeedsRecovery (reported, not auto-resolved -- the drill fails then, because
# a source-agent restart alone shouldn't need an operator).
#
# Usage:
#   lab-inject-source-failure.sh --machine=NAME --target=NODE [--namespace=ns]
#       [--source-ssh=user@host] [--unit=kairon-node] [--outage=20] [--timeout=900]
#
# --source-ssh defaults to KAIRON_HW_SOURCE_SSH; the user needs passwordless
# sudo for systemctl on the source host.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
MACHINE=""; TARGET=""; SOURCE_SSH="${KAIRON_HW_SOURCE_SSH:-}"
UNIT="${KAIRON_HW_NODE_UNIT:-kairon-node}"; OUTAGE=20; TIMEOUT=900
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --machine=*) MACHINE="${arg#*=}" ;;
    --target=*) TARGET="${arg#*=}" ;;
    --source-ssh=*) SOURCE_SSH="${arg#*=}" ;;
    --unit=*) UNIT="${arg#*=}" ;;
    --outage=*) OUTAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$MACHINE" && -n "$TARGET" ]] || { echo "usage: $0 --machine=NAME --target=NODE [--source-ssh=user@host]" >&2; exit 1; }
[[ -n "$SOURCE_SSH" ]] || { echo "set --source-ssh or KAIRON_HW_SOURCE_SSH" >&2; exit 1; }

SOURCE="$(machine_node "$NAMESPACE" "$MACHINE")"
[[ -n "$SOURCE" && "$SOURCE" != "$TARGET" ]] || { echo "Machine $MACHINE is on '${SOURCE:-<none>}'; pick a different --target" >&2; exit 1; }

restart_unit() { lab_ssh "$SOURCE_SSH" "sudo systemctl start $UNIT" || true; }
trap restart_unit EXIT

MIG="$(start_migration "$NAMESPACE" "$MACHINE" "$TARGET" live srcfail)"
lab_log "migration $MIG: $MACHINE $SOURCE -> $TARGET"
wait_until "$TIMEOUT" "$MIG Running" migration_in "$NAMESPACE" "$MIG" Running Failed Blocked Succeeded
[[ "$(migration_phase "$NAMESPACE" "$MIG")" == "Running" ]] || { lab_log "migration never reached Running"; exit 1; }

lab_log "stopping $UNIT on $SOURCE_SSH for ${OUTAGE}s"
lab_ssh "$SOURCE_SSH" "sudo systemctl stop $UNIT"
sleep "$OUTAGE"
restart_unit
lab_log "$UNIT restarted"

wait_until "$TIMEOUT" "$MIG terminal" migration_in "$NAMESPACE" "$MIG" Succeeded Failed Cancelled NeedsRecovery
phase="$(migration_phase "$NAMESPACE" "$MIG")"
case "$phase" in
  Succeeded) want="$TARGET" ;;
  Failed|Cancelled) want="$SOURCE" ;;
  *) lab_log "FAIL: migration landed $phase after a source-agent restart"; exit 1 ;;
esac
wait_until 300 "$MACHINE healthy on $want" machine_healthy "$NAMESPACE" "$MACHINE" "$want"
lab_log "PASS: migration $phase; $MACHINE healthy on $want"
