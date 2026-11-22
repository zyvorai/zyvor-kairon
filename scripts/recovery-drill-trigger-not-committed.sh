#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Trigger A (deterministic) for docs/runbook-recovery-drill.md: drives a
# real live migration to NeedsRecovery by firewalling off the
# destination's migration control-plane port (internal/migration.Client's
# Prepare/Commit/Abort/Diagnose all hit this one port, see
# internal/migration/client.go) -- but only AFTER the migration has
# already reached "Running", not before. Blocking it earlier would break
# Prepare() itself and the migration would never leave Starting; blocking
# it once Running is confirmed is safe, because transfer-status polling
# (internal/agent's SourceMigrator.Status) and the actual QEMU RAM stream
# both go through the adapter's own local socket / data-plane port, never
# through this control-plane port. Once blocked, Commit() -- called the
# instant the transfer reports "completed" -- fails deterministically
# every time, landing NeedsRecovery with the destination genuinely never
# having committed (drills ConfirmDestinationNotCommitted and ForceAbort).
#
# Usage:
#   recovery-drill-trigger-not-committed.sh DEST_USER@DEST_HOST MACHINE_NAME TARGET_NODE_NAME [--namespace=ns] [--port=9443] [--timeout=900]
#
# Requires the destination host to accept the SSH user running
# iptables/nft with sudo (no password prompt, or run interactively).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAIRONCTL="${KAIRONCTL:-$REPO_ROOT/bin/kaironctl}"
[[ -x "$KAIRONCTL" ]] || KAIRONCTL="kaironctl"

if [[ $# -lt 3 ]]; then
  echo "usage: $0 DEST_USER@DEST_HOST MACHINE_NAME TARGET_NODE_NAME [--namespace=ns] [--port=9443] [--timeout=900]" >&2
  exit 1
fi

DEST_TARGET="$1"; MACHINE="$2"; TARGET_NODE="$3"; shift 3
NAMESPACE="default"
PORT=9443
TIMEOUT=900
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --port=*) PORT="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done

drop_rule() { ssh "$DEST_TARGET" "sudo iptables -C INPUT -p tcp --dport $PORT -j DROP 2>/dev/null || sudo iptables -A INPUT -p tcp --dport $PORT -j DROP"; }
remove_rule() { ssh "$DEST_TARGET" "sudo iptables -D INPUT -p tcp --dport $PORT -j DROP 2>/dev/null || true"; }
trap remove_rule EXIT

phase_of() {
  "$KAIRONCTL" get migrations --namespace="$NAMESPACE" | awk -F'\t' -v n="$1" '$1==n{print $NF}'
}

MIG_NAME="drill-notcommitted-$MACHINE-$(date -u +%Y%m%d-%H%M%S)"
echo "[*] creating live migration $MIG_NAME ($MACHINE -> $TARGET_NODE)"
"$KAIRONCTL" migrate "$MACHINE" --namespace="$NAMESPACE" --name="$MIG_NAME" --strategy=live --target-node="$TARGET_NODE"

echo "[*] waiting for phase=Running before arming the block (Prepare must succeed first)"
deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  phase="$(phase_of "$MIG_NAME")"
  case "$phase" in
    Running) break ;;
    Failed|Blocked) echo "[!] migration landed $phase before reaching Running -- nothing to drill, investigate normally" >&2; trap - EXIT; exit 1 ;;
  esac
  (( $(date +%s) >= deadline )) && { echo "[!] timed out waiting for Running (last phase: $phase)" >&2; trap - EXIT; exit 1; }
  sleep 2
done

echo "[*] phase=Running -- arming DROP on $DEST_TARGET:tcp/$PORT (migration control-plane port only; transfer polling and the QEMU data stream are unaffected)"
drop_rule

echo "[*] waiting for the transfer to complete and Commit() to fail (timeout ${TIMEOUT}s)"
deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  phase="$(phase_of "$MIG_NAME")"
  case "$phase" in
    NeedsRecovery)
      echo "[+] landed NeedsRecovery as expected"
      break
      ;;
    Failed|Succeeded|Blocked)
      echo "[!] landed $phase instead of NeedsRecovery -- the port block did not have the intended effect; check the iptables rule and source/target node names" >&2
      trap - EXIT; exit 1
      ;;
  esac
  (( $(date +%s) >= deadline )) && { echo "[!] timed out waiting for NeedsRecovery (last phase: $phase)" >&2; trap - EXIT; exit 1; }
  sleep 3
done

echo
echo "=== drill point reached: $MIG_NAME is NeedsRecovery ==="
echo "    kubectl get machinemigration -n $NAMESPACE $MIG_NAME -o yaml   # inspect status.recovery"
echo
echo "This block was created by a network partition, not a real destination"
echo "failure, so ConfirmDestinationNotCommitted is the correct diagnosis:"
echo "the destination never received Commit() at all."
echo
echo "The DROP rule on $DEST_TARGET is removed automatically when this script exits"
echo "(needed before a retried commit can succeed). Press Enter once you're ready"
echo "to remove it and run the recovery command, or Ctrl-C to leave the drill"
echo "frozen in NeedsRecovery for manual inspection (rule stays until you clean it"
echo "up yourself: ssh $DEST_TARGET sudo iptables -D INPUT -p tcp --dport $PORT -j DROP)."
read -r _

echo "[*] run the recovery yourself (not done automatically -- this is the operator action being drilled):"
echo "    kaironctl recover $MIG_NAME --namespace=$NAMESPACE --action=ConfirmDestinationNotCommitted --diagnosis=DestinationNotCommitted --reason=\"recovery drill: control-plane port firewalled off during Running to force a deterministic commit failure\""
echo "    (or --action=ForceAbort --diagnosis=Unknown to drill that path instead)"
