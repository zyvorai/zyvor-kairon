#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Trigger B (BEST-EFFORT, RACY -- not deterministic, unlike Trigger A in
# recovery-drill-trigger-not-committed.sh) for docs/runbook-recovery-drill.md.
#
# Forces the genuinely-ambiguous "destination secretly committed" case
# needed to drill ConfirmDestinationCommitted. Real mechanics (verified
# against source): internal/migration.NetworkAwareDestination.Commit
# (internal/migration/network.go) does two sequential, INDEPENDENT steps
# for a network-fabric-enabled migration:
#   1. d.Inner.Commit(...)        -- the adapter's destCommit handler,
#                                     which calls FluxVM's real
#                                     "/v1/migration/receivers/{id}/activate"
#                                     and logs "fluxvm: receiver activated"
#                                     (cmd/kairon-migration-adapter-fluxvm/
#                                     main.go's destCommit) -- THE GUEST IS
#                                     NOW RUNNING ON THE DESTINATION.
#   2. d.Flux.NetworkMigrationResume(...) -- a separate, local FluxVM API
#                                     call on the SAME host.
# If step 2 fails after step 1 already succeeded, migration.Server.commit's
# response is an error (session store never advances past "Prepared"), but
# the destination guest is genuinely live -- exactly what
# destinationSessionPhase=="Prepared" is NOT proof against (see
# docs/runbook-migration-failures.md's caveat).
#
# Both steps run back-to-back on the destination host with NO network hop
# between them -- there is no reliable window to inject a failure between
# them from outside that host. This script's approach (tail the adapter's
# journal for "receiver activated", then immediately try to make the next
# local FluxVM call fail) is a SUB-SECOND, UNMEASURED race. It may take
# several attempts, or may never land. That is the honest, documented
# limitation -- do not tune this into looking more reliable than it is.
#
# Usage:
#   recovery-drill-trigger-committed.sh DEST_USER@DEST_HOST MACHINE_NAME TARGET_NODE_NAME [--namespace=ns] [--attempts=5] [--timeout=900]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAIRONCTL="${KAIRONCTL:-$REPO_ROOT/bin/kaironctl}"
[[ -x "$KAIRONCTL" ]] || KAIRONCTL="kaironctl"

if [[ $# -lt 3 ]]; then
  echo "usage: $0 DEST_USER@DEST_HOST MACHINE_NAME TARGET_NODE_NAME [--namespace=ns] [--attempts=5] [--timeout=900]" >&2
  exit 1
fi

DEST_TARGET="$1"; MACHINE="$2"; TARGET_NODE="$3"; shift 3
NAMESPACE="default"
ATTEMPTS=5
TIMEOUT=900
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --attempts=*) ATTEMPTS="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done

phase_of() {
  "$KAIRONCTL" get migrations --namespace="$NAMESPACE" | awk -F'\t' -v n="$1" '$1==n{print $NF}'
}

echo "[!] this trigger is best-effort and may need several attempts (--attempts=$ATTEMPTS)."
echo "    Each attempt runs one full live migration -- this is slow. Consider running"
echo "    recovery-drill-trigger-not-committed.sh first if you haven't drilled that path yet."
echo

for attempt in $(seq 1 "$ATTEMPTS"); do
  MIG_NAME="drill-committed-$MACHINE-$(date -u +%Y%m%d-%H%M%S)-$attempt"
  echo "=== attempt $attempt/$ATTEMPTS: $MIG_NAME ==="

  # Arm the race on the destination BEFORE creating the migration: a
  # background watcher that tails the adapter's journal and, the instant
  # it sees "receiver activated" for OUR session, tries to break the next
  # local FluxVM call. The most realistic, least-invasive way to do that
  # without patching the adapter is to briefly SIGSTOP the local FluxVM
  # process so its next request stalls past kairon-node's own migration
  # RPC timeout (internal/migration.NewClient defaults to 15s) --
  # then SIGCONT it back. If FluxVM has no single top-level process to
  # pause (e.g. it forks per-request), this attempt will simply miss the
  # window, which is why --attempts exists.
  watcher_pid=""
  ssh "$DEST_TARGET" bash -s -- "$MIG_NAME" <<'REMOTE_EOF' &
set -euo pipefail
session_substr="$1"
timeout 60 journalctl -u kairon-migration-adapter-fluxvm -f -n 0 |
  while read -r line; do
    if [[ "$line" == *"receiver activated"* ]]; then
      fluxvm_pid="$(pgrep -f 'fluxvm' | head -1 || true)"
      [[ -n "$fluxvm_pid" ]] && sudo kill -STOP "$fluxvm_pid" && sleep 0.05 && sudo kill -CONT "$fluxvm_pid"
      break
    fi
  done
REMOTE_EOF
  watcher_pid=$!

  echo "[*] creating live migration $MIG_NAME ($MACHINE -> $TARGET_NODE)"
  "$KAIRONCTL" migrate "$MACHINE" --namespace="$NAMESPACE" --name="$MIG_NAME" --strategy=live --target-node="$TARGET_NODE"

  deadline=$(( $(date +%s) + TIMEOUT ))
  landed=""
  while :; do
    phase="$(phase_of "$MIG_NAME")"
    case "$phase" in
      NeedsRecovery) landed="NeedsRecovery"; break ;;
      Succeeded|Failed|Blocked) landed="$phase"; break ;;
    esac
    (( $(date +%s) >= deadline )) && { landed="timeout"; break; }
    sleep 2
  done
  wait "$watcher_pid" 2>/dev/null || true

  if [[ "$landed" == "NeedsRecovery" ]]; then
    echo "[+] landed NeedsRecovery on attempt $attempt -- checking whether the destination actually committed"
    echo "    kubectl get machinemigration -n $NAMESPACE $MIG_NAME -o yaml   # check status.recovery.destinationRuntimeFound/Status"
    echo "    If destinationRuntimeFound=true and the runtime looks healthy: this IS the drilled case."
    echo "        kaironctl recover $MIG_NAME --namespace=$NAMESPACE --action=ConfirmDestinationCommitted --diagnosis=DestinationCommitted --reason=\"recovery drill: destination confirmed running via live FluxVM lookup\""
    echo "    If destinationRuntimeFound=false: the race landed NeedsRecovery for a different reason (e.g. Trigger A's"
    echo "    kind of failure) -- this was NOT the committed case; treat with ConfirmDestinationNotCommitted instead."
    exit 0
  fi
  echo "[*] attempt $attempt landed '$landed', not the ambiguous case -- retrying" >&2
done

echo "[!] all $ATTEMPTS attempts missed the race window. This is the expected, documented outcome of a best-effort" >&2
echo "    sub-second race -- see this script's header comment. Increase --attempts, or accept that this specific" >&2
echo "    drill (ConfirmDestinationCommitted) may not be reliably reproducible on your hardware." >&2
exit 1
