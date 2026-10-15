#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Dead-node drill (docs/runbook-node-fence.md, Option B): a throwaway Machine
# opted into stale evacuation runs on the victim node; the victim's node agent
# is stopped and the Machine's QEMU is killed (what a power loss does to that
# VM -- the host itself is not powered off); the node is fenced with an
# attestation; the controller must restart the Machine on another node. Then
# the victim is brought back and the attestation cleared.
#
#   KAIRON_HW_LAB=1
#   KAIRON_KUBE_URL / TOKEN / CA          cluster API (as the other lab scripts)
#   KAIRON_HW_VICTIM_NODE                 node to "lose"
#   KAIRON_HW_VICTIM_SSH                  user@host of the victim (sudo, passwordless)
#   KAIRON_HW_VICTIM_NODE_UNIT            its node agent unit (default kairon-node)
#   KAIRON_HW_CONTROLLER_SSH              user@host running kairon-controller
#   KAIRON_HW_CONTROLLER_ENV              its EnvironmentFile (default /etc/kairon/kairon-controller.env)
#   KAIRON_HW_CONTROLLER_UNIT             default kairon-controller
#   KAIRON_HW_NAMESPACE, KAIRON_SCALE_TOLERATE_TAINTS (taint keys the victim carries)
#   KAIRON_HW_TIMEOUT                     seconds per wait (default 420)
#
# The controller's stale evacuation is switched on for the drill and switched
# back off afterwards; if it was already on it is left on.
set -euo pipefail

if [[ "${KAIRON_HW_LAB:-}" != "1" ]]; then
  echo "refusing: set KAIRON_HW_LAB=1 to acknowledge a real lab" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

VICTIM="${KAIRON_HW_VICTIM_NODE:?set KAIRON_HW_VICTIM_NODE}"
VICTIM_SSH="${KAIRON_HW_VICTIM_SSH:?set KAIRON_HW_VICTIM_SSH}"
VICTIM_UNIT="${KAIRON_HW_VICTIM_NODE_UNIT:-kairon-node}"
CTRL_SSH="${KAIRON_HW_CONTROLLER_SSH:?set KAIRON_HW_CONTROLLER_SSH}"
CTRL_ENV="${KAIRON_HW_CONTROLLER_ENV:-/etc/kairon/kairon-controller.env}"
CTRL_UNIT="${KAIRON_HW_CONTROLLER_UNIT:-kairon-controller}"
NS="${KAIRON_HW_NAMESPACE:-default}"
TIMEOUT="${KAIRON_HW_TIMEOUT:-420}"
NAME="drill-nodefail-$(date +%m%d%H%M%S)"
IMG_URL="https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img"
IMG_DIGEST="sha256:7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b"

enabled_by_drill=0
victim_stopped=0
fenced=0

field() { # JSON-PATH-EXPR on the drill Machine, e.g. 'o["status"]["phase"]'
  kube_curl GET "$(machine_path "$NS" "$NAME")" 2>/dev/null | python3 -c "
import json, sys
try:
    o = json.load(sys.stdin)
    print($1)
except Exception:
    print('')"
}

restore() {
  set +e
  if [[ $victim_stopped -eq 1 ]]; then
    lab_log "restoring: starting $VICTIM_UNIT on $VICTIM_SSH"
    lab_ssh "$VICTIM_SSH" "sudo systemctl start $VICTIM_UNIT"
  fi
  if [[ $fenced -eq 1 ]]; then
    lab_log "restoring: clearing the node attestation"
    "$KAIRONCTL" node fence "$VICTIM" --clear
  fi
  kube_curl DELETE "$(machine_path "$NS" "$NAME")" >/dev/null 2>&1
  if [[ $enabled_by_drill -eq 1 ]]; then
    lab_log "restoring: stale evacuation off on the controller"
    lab_ssh "$CTRL_SSH" "sudo sed -i '/^KAIRON_STALE_EVACUATION=/d' $CTRL_ENV && sudo systemctl restart $CTRL_UNIT"
  fi
}
trap restore EXIT

# --- 1. stale evacuation on -------------------------------------------------
if lab_ssh "$CTRL_SSH" "sudo grep -qs '^KAIRON_STALE_EVACUATION=true' $CTRL_ENV"; then
  lab_log "stale evacuation already enabled on the controller"
else
  lab_log "enabling stale evacuation on the controller"
  lab_ssh "$CTRL_SSH" "echo KAIRON_STALE_EVACUATION=true | sudo tee -a $CTRL_ENV >/dev/null && sudo systemctl restart $CTRL_UNIT"
  enabled_by_drill=1
  sleep 8
fi

# --- 2. a Machine on the victim ---------------------------------------------
lab_log "creating $NAME pinned to $VICTIM"
python3 - "$NAME" "$NS" "$VICTIM" "$IMG_URL" "$IMG_DIGEST" "${KAIRON_SCALE_TOLERATE_TAINTS:-}" <<'PY' | kube_curl POST "$LAB_API/namespaces/$NS/machines" "$(cat)" >/dev/null
import json, sys
name, ns, victim, url, digest, taints = sys.argv[1:7]
placement = {"architecture": "amd64", "nodeSelector": {"kubernetes.io/hostname": victim}}
tol = [{"key": k, "operator": "Exists", "effect": "NoSchedule"} for k in taints.split(",") if k]
if tol:
    placement["tolerations"] = tol
print(json.dumps({
    "apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
    "metadata": {"name": name, "namespace": ns, "annotations": {"kairon.zyvor.dev/evacuate": "true"}},
    "spec": {
        "image": {"source": {"httpURL": url, "format": "qcow2"}, "digest": digest},
        "resources": {"cpu": "1", "memory": "256Mi"},
        "runtime": {"backend": "qemu"}, "network": {"mode": "user"},
        "placement": placement, "powerState": "Running", "ttlSeconds": 3600,
    },
}))
PY
wait_until "$TIMEOUT" "$NAME Running on $VICTIM" machine_healthy "$NS" "$NAME" "$VICTIM"
RUNTIME_ID="$(field 'o["status"].get("runtimeID","")')"
lab_log "Running on $VICTIM (runtime $RUNTIME_ID)"

# --- 3. lose the node -------------------------------------------------------
lab_log "stopping $VICTIM_UNIT and killing this Machine's QEMU on $VICTIM_SSH"
lab_ssh "$VICTIM_SSH" "sudo systemctl stop $VICTIM_UNIT" ; victim_stopped=1
# [q] keeps the pattern from matching the remote shell whose own command line
# contains it (a plain pkill -f would kill the session running it).
lab_ssh "$VICTIM_SSH" "sudo pkill -9 -f '[q]emu-system.*$NAME' || true"
alive="$(lab_ssh "$VICTIM_SSH" "pgrep -fc '[q]emu-system.*$NAME' || true" | tr -d '[:space:]')"
[[ "${alive:-0}" == "0" ]] || { lab_log "FAIL: the Machine's QEMU is still alive on the victim"; exit 1; }

# --- 4. wait until Kairon sees the node as gone -----------------------------
t0=$(date +%s)
until [[ "$(field '[c["status"] for c in o["status"].get("conditions",[]) if c["type"]=="NodeUnreachable"][0]')" == "True" ]]; do
  (( $(date +%s) - t0 > TIMEOUT )) && { lab_log "FAIL: NodeUnreachable was never set"; exit 1; }
  sleep 5
done
lab_log "NodeUnreachable=True after $(( $(date +%s) - t0 ))s"

# --- 5. attest and let the controller evacuate ------------------------------
t1=$(date +%s)
"$KAIRONCTL" node fence "$VICTIM" --reason "lab-node-failure-drill $NAME: agent stopped and VM killed" ; fenced=1
until [[ "$(field 'o["status"].get("nodeName","")')" != "$VICTIM" && "$(field 'o["status"].get("phase","")')" == "Running" ]]; do
  (( $(date +%s) - t1 > TIMEOUT )) && { lab_log "FAIL: $NAME was not restarted elsewhere within ${TIMEOUT}s"; exit 1; }
  sleep 5
done
NEW="$(field 'o["status"].get("nodeName","")')"
lab_log "PASS: $NAME restarted on $NEW $(( $(date +%s) - t1 ))s after the attestation"
echo "node-failure-drill,victim=$VICTIM,new=$NEW,unreachable_after=$(( t1 - t0 ))s,restarted_after=$(( $(date +%s) - t1 ))s"
