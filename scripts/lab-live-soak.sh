#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Live-migration soak: move one Machine back and forth between two nodes N
# times and stop at the first migration that does not end Succeeded, saving the
# evidence that is lost once Kairon or FluxVM cleans up (the migration object,
# FluxVM's VM records with their `error`, the journals of both hosts). One
# clean pass of a migration says little; rare failures show up as a rate.
#
#   KAIRON_HW_LAB=1
#   KAIRON_KUBE_URL / TOKEN / CA        cluster API (as the other lab scripts)
#   KAIRON_HW_MACHINE                   the Machine to move
#   KAIRON_HW_NODE_A / KAIRON_HW_NODE_B the two nodes
#   KAIRON_HW_SSH_A  / KAIRON_HW_SSH_B  user@host of each (for evidence; optional)
#   KAIRON_SOAK_HOPS                    migrations to attempt (default 20)
#   KAIRON_SOAK_EVIDENCE_DIR            default ./soak-evidence
#   KAIRON_SOAK_CONTINUE=1              after a migration that did not Succeed, save
#                                       the evidence and keep going if the Machine is
#                                       healthy again (Kairon recovered it), so the
#                                       result is a failure rate; default stops at the
#                                       first failure
#   KAIRON_HW_NAMESPACE, KAIRON_HW_TIMEOUT (seconds per migration, default 300)
#
# Exit 0 when every hop Succeeded, 1 when any did not.
set -euo pipefail

if [[ "${KAIRON_HW_LAB:-}" != "1" ]]; then
  echo "refusing: set KAIRON_HW_LAB=1 to acknowledge a real lab" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NS="${KAIRON_HW_NAMESPACE:-default}"
MACHINE="${KAIRON_HW_MACHINE:?set KAIRON_HW_MACHINE}"
NODE_A="${KAIRON_HW_NODE_A:?set KAIRON_HW_NODE_A}"
NODE_B="${KAIRON_HW_NODE_B:?set KAIRON_HW_NODE_B}"
HOPS="${KAIRON_SOAK_HOPS:-20}"
TIMEOUT="${KAIRON_HW_TIMEOUT:-300}"
OUT="${KAIRON_SOAK_EVIDENCE_DIR:-./soak-evidence}"
RUNTIME_NAME="kairon-$NS-$MACHINE"

host_ssh() { # NODE -> user@host or empty
  case "$1" in
    "$NODE_A") echo "${KAIRON_HW_SSH_A:-}" ;;
    "$NODE_B") echo "${KAIRON_HW_SSH_B:-}" ;;
  esac
}

capture() { # MIGRATION PHASE
  mkdir -p "$OUT"
  local f="$OUT/$1-$2.txt" node target
  {
    echo "### $1 ended $2 at $(date -u +%FT%TZ)"
    kube_curl GET "$(migration_path "$NS" "$1")" | json_eval '"phase=%s\nmessage=%s\nrecovery=%s\nsource=%s target=%s downtimeMs=%s totalMs=%s encrypted=%s" % (
      (o.get("status") or {}).get("phase"), (o.get("status") or {}).get("message"),
      json.dumps((o.get("status") or {}).get("recovery")), (o.get("status") or {}).get("sourceNode"),
      (o.get("status") or {}).get("targetNode"), (o.get("status") or {}).get("downtimeMs"),
      (o.get("status") or {}).get("totalTimeMs"), (o.get("status") or {}).get("dataPlaneEncrypted"))' || true
    for node in "$NODE_A" "$NODE_B"; do
      target="$(host_ssh "$node")"
      [[ -n "$target" ]] || continue
      echo; echo "===== $node ($target): FluxVM records named $RUNTIME_NAME"
      lab_ssh "$target" "curl -s -m 15 localhost:7788/v1/vms?name=$RUNTIME_NAME" 2>&1 | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception as e:
    print("unreadable:", e); sys.exit(0)
items = d if isinstance(d, list) else d.get("items", d.get("vms", []))
for v in items:
    print(json.dumps({k: v.get(k) for k in ("id", "status", "pid", "created_at", "error", "disk")}))' || true
      echo "----- fluxvm journal, last 5 min (reaper, migration and receiver lines)"
      lab_ssh "$target" "sudo journalctl -u fluxvm --since '-5min' --no-pager -o cat 2>/dev/null | sed 's/\\x1b\\[[0-9;]*m//g' | grep -E 'stuck in Creating|migration\\.|receiver|vm\\.(delete|stop)' | tail -n 30" 2>&1 || true
    done
  } > "$f" 2>&1
  lab_log "evidence saved to $f"
}

ok=0
failed=0
for ((i = 1; i <= HOPS; i++)); do
  wait_until "$TIMEOUT" "$MACHINE Running with a guest IP" machine_healthy "$NS" "$MACHINE" || {
    lab_log "STOP before hop $i: $MACHINE is not healthy"; capture "pre-hop-$i" unhealthy
    echo "soak,hops_ok=$ok,hops_failed=$failed,stopped_at=$i,reason=machine-unhealthy"; exit 1; }
  cur="$(machine_node "$NS" "$MACHINE")"
  target="$(other_node "$cur" "$NODE_A" "$NODE_B")"
  name="$(start_migration "$NS" "$MACHINE" "$target" live soak)"
  lab_log "hop $i/$HOPS: $cur -> $target ($name)"
  phase=""
  deadline=$(( $(date +%s) + TIMEOUT ))
  until case "$phase" in Succeeded | Failed | Blocked | Cancelled | NeedsRecovery) true ;; *) false ;; esac; do
    (( $(date +%s) >= deadline )) && { phase="Timeout"; break; }
    sleep 4
    phase="$(migration_phase "$NS" "$name")"
  done
  if [[ "$phase" == "Succeeded" ]]; then
    ok=$(( ok + 1 ))
    continue
  fi
  failed=$(( failed + 1 ))
  lab_log "hop $i ended $phase ($ok clean so far)"
  capture "$name" "$phase"
  if [[ "${KAIRON_SOAK_CONTINUE:-0}" != "1" || "$phase" == "NeedsRecovery" || "$phase" == "Timeout" ]]; then
    # NeedsRecovery and Timeout leave the Machine in an unknown state: an operator decides.
    echo "soak,hops_ok=$ok,hops_failed=$failed,stopped_at=$i,phase=$phase,migration=$name"
    exit 1
  fi
done
lab_log "DONE: $ok/$HOPS live migrations Succeeded, $failed did not"
echo "soak,hops_ok=$ok,hops_failed=$failed,hops=$HOPS"
(( failed == 0 ))
