#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Source failure after the post-copy switch: starts an adaptive live
# migration that switches to post-copy after --post-copy-after, waits until
# status.adaptive.postCopyStarted, then SIGKILLs the source QEMU. At that
# point neither node holds the whole guest, so the only correct outcome is
# NeedsRecovery with the source left paused/dead and the destination left
# alone -- a Failed migration (source resumed, destination aborted) or a
# Running one that never resolves fails the drill.
#
# The guest is lost by design. --machine must be a disposable Machine that
# the lab recreates before each run; the script refuses without --disposable.
# With --recover it closes the migration out with `kaironctl recover
# --action ForceAbort --diagnosis Unknown` and waits for Failed.
#
# Usage:
#   lab-inject-postcopy-source-failure.sh --machine=NAME --target=NODE --disposable
#       [--namespace=ns] [--source-ssh=user@host] [--bandwidth-mbps=100]
#       [--post-copy-after=15s] [--load-mib=512] [--kill-cmd=CMD] [--recover]
#       [--timeout=900]
#
# --kill-cmd runs on the source host; {runtime} is replaced with the source
# FluxVM runtime ID (default: sudo pkill -9 -f '[q]emu-system.*{runtime}'; the
# bracket keeps pkill from matching the remote shell running it).
# Guest load needs KAIRON_UI_URL / KAIRON_UI_TOKEN; without it the transfer
# may finish before the kill, which the drill reports as a failure.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
MACHINE=""; TARGET=""; SOURCE_SSH="${KAIRON_HW_SOURCE_SSH:-}"
BANDWIDTH=100; POSTCOPY_AFTER=15s; LOAD_MIB=512; TIMEOUT=900
KILL_CMD="sudo pkill -9 -f '[q]emu-system.*{runtime}'"
DISPOSABLE=0; RECOVER=0
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --machine=*) MACHINE="${arg#*=}" ;;
    --target=*) TARGET="${arg#*=}" ;;
    --source-ssh=*) SOURCE_SSH="${arg#*=}" ;;
    --bandwidth-mbps=*) BANDWIDTH="${arg#*=}" ;;
    --post-copy-after=*) POSTCOPY_AFTER="${arg#*=}" ;;
    --load-mib=*) LOAD_MIB="${arg#*=}" ;;
    --kill-cmd=*) KILL_CMD="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --disposable) DISPOSABLE=1 ;;
    --recover) RECOVER=1 ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$MACHINE" && -n "$TARGET" ]] || { echo "usage: $0 --machine=NAME --target=NODE --disposable [--source-ssh=user@host]" >&2; exit 1; }
[[ "$DISPOSABLE" == "1" ]] || { echo "refusing: this drill destroys ${MACHINE}'s guest; pass --disposable" >&2; exit 2; }
[[ -n "$SOURCE_SSH" ]] || { echo "set --source-ssh or KAIRON_HW_SOURCE_SSH" >&2; exit 1; }

SOURCE="$(machine_node "$NAMESPACE" "$MACHINE")"
[[ -n "$SOURCE" && "$SOURCE" != "$TARGET" ]] || { echo "Machine $MACHINE is on '${SOURCE:-<none>}'; pick a different --target" >&2; exit 1; }

postcopy_started() {
  [[ "$(migration_field "$NAMESPACE" "$MIG" '((o.get("status") or {}).get("adaptive") or {}).get("postCopyStarted") or False' 2>/dev/null)" == "True" ]] ||
    migration_in "$NAMESPACE" "$MIG" Succeeded Failed Blocked Cancelled NeedsRecovery
}

if [[ -n "${KAIRON_UI_URL:-}" && -n "${KAIRON_UI_TOKEN:-}" ]]; then
  start_guest_load "$NAMESPACE" "$MACHINE" "$LOAD_MIB" || lab_log "could not start guest load; continuing idle"
  sleep 10
fi

MIG="$(start_migration "$NAMESPACE" "$MACHINE" "$TARGET" adaptive pcfail \
  --bandwidth-mbps="$BANDWIDTH" --post-copy --post-copy-after="$POSTCOPY_AFTER")"
lab_log "migration $MIG: $MACHINE $SOURCE -> $TARGET, post-copy after $POSTCOPY_AFTER"
wait_until "$TIMEOUT" "$MIG post-copy started" postcopy_started
phase="$(migration_phase "$NAMESPACE" "$MIG")"
if [[ "$phase" != "Running" ]]; then
  lab_log "FAIL: migration reached $phase before the kill (post-copy finished too fast or never started); lower --bandwidth-mbps or raise --load-mib"
  exit 1
fi

runtime="$(migration_field "$NAMESPACE" "$MIG" '(o.get("status") or {}).get("runtimeID") or ""')"
[[ -n "$runtime" ]] || { lab_log "FAIL: $MIG has no status.runtimeID"; exit 1; }
lab_log "post-copy started; killing source runtime $runtime on $SOURCE_SSH"
if ! lab_ssh "$SOURCE_SSH" "${KILL_CMD//\{runtime\}/$runtime}"; then
  lab_log "FAIL: kill command matched nothing on the source host"
  exit 1
fi

wait_until "$TIMEOUT" "$MIG terminal" migration_in "$NAMESPACE" "$MIG" Succeeded Failed Cancelled NeedsRecovery
phase="$(migration_phase "$NAMESPACE" "$MIG")"
if [[ "$phase" != "NeedsRecovery" ]]; then
  lab_log "FAIL: migration landed $phase after the source died mid post-copy; want NeedsRecovery"
  exit 1
fi
latched="$(migration_field "$NAMESPACE" "$MIG" '((o.get("status") or {}).get("adaptive") or {}).get("postCopyStarted") or False')"
[[ "$latched" == "True" ]] || { lab_log "FAIL: NeedsRecovery without status.adaptive.postCopyStarted"; exit 1; }
lab_log "PASS: $MIG in NeedsRecovery with post-copy latched; source not resumed, destination not aborted"

if [[ "$RECOVER" == "1" ]]; then
  "$KAIRONCTL" recover "$MIG" --namespace "$NAMESPACE" --action ForceAbort --diagnosis Unknown \
    --reason "lab drill: source QEMU killed after post-copy started; guest discarded" >&2
  wait_until 300 "$MIG Failed" migration_in "$NAMESPACE" "$MIG" Failed
  lab_log "recovered: $MIG Failed; recreate $MACHINE before the next run"
fi
