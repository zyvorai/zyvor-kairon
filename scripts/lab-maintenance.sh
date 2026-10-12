#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Node maintenance mode and DRS drill on one node (see
# docs/guides/node-maintenance-drs.md). One QEMU Machine runs a probe that
# prints "MLAB tick=N boot=<boot_id> shm=<marker>" to the serial console
# every 2s. It checks:
#
#   drs         kairon-controller with -drs-mode=recommend exports
#               kairon_drs_* metrics for the node, and `kaironctl drs plan`
#               reports why it plans no moves on a single node
#   wait        `kaironctl node maintenance enter`: the controller cordons
#               the node, reports Entering with "no eligible target", and
#               leaves the Machine running (fallback wait, no migration)
#   stop        --fallback stop: the Machine is stopped and annotated
#               maintenance-parked, its QEMU process is gone, the node
#               reports InMaintenance with a MaintenanceReady Event and the
#               kairon_node_maintenance metric
#   exit        `kaironctl node maintenance exit`: the node is uncordoned,
#               the controller's annotations are gone, the Machine is
#               Running again with a fresh boot
#   hibernate   --fallback hibernate: the Machine hibernates; exit resumes
#               the same boot (boot id and /dev/shm marker unchanged)
#   cordon      a node an operator cordoned stays cordoned after
#               maintenance exits
#
# Migrating Machines to another node, and DRS moves, need two nodes and are
# printed as "not run".
#
# Usage: lab-maintenance.sh --node=NAME --image=PATH [--namespace=ns] [--timeout=600]
# Needs KAIRON_KUBE_* (lib/lab.sh), FLUXVM_URL (default
# http://127.0.0.1:7788), sudo for the kairon-controller drop-in, and
# KAIRON_LAB_KAIRONCTL, the kaironctl command line to run (default:
# kaironctl with KUBECONFIG=/etc/rancher/k3s/k3s.yaml under sudo). The
# Machine, the drop-in and every maintenance annotation are removed on exit,
# and the node's schedulability is restored.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
METRICS_URL="${KAIRON_CONTROLLER_METRICS_URL:-http://127.0.0.1:32301/metrics}"
read -r -a KCTL <<<"${KAIRON_LAB_KAIRONCTL:-sudo -n env KUBECONFIG=/etc/rancher/k3s/k3s.yaml kaironctl}"
export KAIRON_HW_REQUIRE_GUEST_IP="${KAIRON_HW_REQUIRE_GUEST_IP:-0}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" ]] || { echo "usage: $0 --node=NAME --image=PATH" >&2; exit 1; }

STAMP="$(date -u +%m%d%H%M%S)"
VM="mlab-$STAMP"
DROPIN=/etc/systemd/system/kairon-controller.service.d/90-lab-drs.conf
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}
kctl() { "${KCTL[@]}" "$@"; }

NODE_PATH="api/v1/nodes/$NODE"
node_field() { kube_curl GET "$NODE_PATH" | json_eval "$1"; }
node_ann() { node_field "((o.get(\"metadata\") or {}).get(\"annotations\") or {}).get(\"$1\") or \"\""; }
node_unschedulable() { node_field '"yes" if (o.get("spec") or {}).get("unschedulable") else "no"'; }
WAS_UNSCHEDULABLE="$(node_unschedulable)"

clear_maintenance() {
  kube_curl PATCH "$NODE_PATH" '{"metadata":{"annotations":{"kairon.zyvor.dev/maintenance":null,"kairon.zyvor.dev/maintenance-fallback":null,"kairon.zyvor.dev/maintenance-strategy":null}}}' application/merge-patch+json >/dev/null
}
cleanup() {
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1 || true
  clear_maintenance 2>/dev/null || true
  sleep 12
  local unsched=false
  [[ "$WAS_UNSCHEDULABLE" == yes ]] && unsched=true
  kube_curl PATCH "$NODE_PATH" "{\"spec\":{\"unschedulable\":$unsched},\"metadata\":{\"annotations\":{\"kairon.zyvor.dev/maintenance-state\":null,\"kairon.zyvor.dev/maintenance-message\":null,\"kairon.zyvor.dev/maintenance-cordoned\":null}}}" application/merge-patch+json >/dev/null 2>&1 || true
  if sudo -n test -f "$DROPIN"; then
    sudo -n rm -f "$DROPIN"
    sudo -n systemctl daemon-reload
    sudo -n systemctl restart kairon-controller || true
  fi
}
trap cleanup EXIT

probe='#!/bin/sh
echo "m-$(cat /proc/sys/kernel/random/uuid)" > /dev/shm/mlab
i=0
while true; do
  i=$((i+1))
  echo "MLAB tick=$i boot=$(cat /proc/sys/kernel/random/boot_id) shm=$(cat /dev/shm/mlab 2>/dev/null)" > /dev/ttyS0
  sleep 2
done'
body="$(python3 - "$VM" "$NODE" "$IMAGE" "$probe" <<'EOF'
import json, sys
name, node, image, probe = sys.argv[1:5]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "cloudInit": {
        "writeFiles": [{"path": "/usr/local/bin/mlab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit mlab-probe /usr/local/bin/mlab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "maintenance"}}, "spec": spec}))
EOF
)"

# DRS in recommend mode for the whole run, so its metrics can be read.
sudo -n mkdir -p "$(dirname "$DROPIN")"
exec_start="$(systemctl show -p ExecStart --value kairon-controller | sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p' | sed 's/ *$//')"
[[ -n "$exec_start" ]] || exec_start="/usr/bin/kairon-controller --interval=5s --health-addr=:32301"
printf '[Service]\nExecStart=\nExecStart=%s --drs-mode=recommend --drs-interval=10s\n' "$exec_start" | sudo -n tee "$DROPIN" >/dev/null
sudo -n systemctl daemon-reload
sudo -n systemctl restart kairon-controller

kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null

set_power() {
  kube_curl PATCH "$(machine_path "$NAMESPACE" "$VM")" "{\"spec\":{\"powerState\":\"$1\"}}" application/merge-patch+json >/dev/null
}
field() { machine_field "$NAMESPACE" "$VM" "$1" 2>/dev/null || true; }
phase_is() { [[ "$(field '(o.get("status") or {}).get("phase") or ""')" == "$1" ]]; }
ann() { field "((o.get(\"metadata\") or {}).get(\"annotations\") or {}).get(\"$1\") or \"\""; }
spec_power() { field '(o.get("spec") or {}).get("powerState") or "Running"'; }
runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""'; }
last_probe() {
  local rid
  rid="$(runtime_id)"
  [[ -n "$rid" ]] || return 1
  flux "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'MLAB tick=[0-9]* boot=[^ ]* shm=[^ "\\]*' | tail -1 |
    sed -E 's/MLAB tick=([0-9]+) boot=([^ ]+) shm=(.*)/\1 \2 \3/'
}
probe_seen() { [[ -n "$(last_probe 2>/dev/null || true)" ]]; }
probe_after() { local t; t="$(last_probe 2>/dev/null | cut -d' ' -f1)"; [[ -n "$t" && "$t" -gt "$1" ]]; }
probe_boot_not() { local b; b="$(last_probe 2>/dev/null | cut -d' ' -f2)"; [[ -n "$b" && "$b" != "$1" ]]; }
qemu_alive() { pgrep -f "qemu.*$1" >/dev/null 2>&1; }
state_is() { [[ "$(node_ann kairon.zyvor.dev/maintenance-state)" == "$1" ]]; }
message_has() { [[ "$(node_ann kairon.zyvor.dev/maintenance-message)" == *"$1"* ]]; }
metric() { curl -fsS "$METRICS_URL" 2>/dev/null | grep -E "^$1" || true; }
metric_has() { [[ -n "$(metric "$1")" ]]; }
maintenance_migrations() {
  kube_curl GET "$LAB_API/namespaces/$NAMESPACE/machinemigrations" |
    json_eval "len([i for i in o.get('items') or [] if i['metadata']['name'].startswith('maintenance-$NODE-') and (i.get('spec') or {}).get('machineName') == '$VM'])"
}
node_event() {
  kube_curl GET "api/v1/namespaces/default/events?fieldSelector=involvedObject.kind=Node,involvedObject.name=$NODE,reason=$1" |
    json_eval 'len(o.get("items") or [])'
}
exited() {
  [[ "$(node_ann kairon.zyvor.dev/maintenance-state)" == "" && "$(node_ann kairon.zyvor.dev/maintenance-cordoned)" == "" ]]
}

if ! wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" ||
  ! wait_until "$TIMEOUT" "$VM probe on the console" probe_seen; then
  result drs fail "$VM never ran its probe: $(field '(o.get("status") or {}).get("message") or ""')"
else
  # drs
  if wait_until 60 "kairon_drs_node_utilization_percent for $NODE" metric_has "kairon_drs_node_utilization_percent\\{node=\"$NODE\",resource=\"max\"\\}"; then
    util="$(metric "kairon_drs_node_utilization_percent\\{node=\"$NODE\",resource=\"max\"\\}" | awk '{print $2}')"
    imb="$(metric kairon_drs_imbalance_percent | awk '{print $2}')"
    plan="$(kctl drs plan 2>&1 || true)"
    if [[ "$plan" == *"rebalancing needs at least 2"* && "$plan" == *"$NODE"* && -n "$imb" ]]; then
      result drs pass "recommend mode: $NODE at ${util}% reserved, imbalance ${imb}%; kaironctl drs plan: no moves, 1 eligible node(s)"
    else
      result drs fail "metric util=$util imbalance=$imb; plan: $(tr '\n' ' ' <<<"$plan")"
    fi
  else
    result drs fail "no kairon_drs_node_utilization_percent for $NODE at $METRICS_URL"
  fi

  # wait
  started=$(date +%s)
  kctl node maintenance enter "$NODE" >/dev/null
  if wait_until 90 "$NODE Entering with no eligible target" message_has "with no eligible target node"; then
    sleep 10
    status_out="$(kctl node maintenance status "$NODE" 2>&1 || true)"
    if [[ "$(node_unschedulable)" == yes && "$(node_ann kairon.zyvor.dev/maintenance-cordoned)" == true ]] && state_is Entering &&
      phase_is Running && [[ "$(maintenance_migrations)" == 0 && "$status_out" == *Entering* && "$(node_event MaintenanceEntering)" -ge 1 ]]; then
      result wait pass "cordoned and Entering in $(( $(date +%s) - started ))s: '$(node_ann kairon.zyvor.dev/maintenance-message)'; $VM still Running, no MachineMigration, MaintenanceEntering Event"
    else
      result wait fail "unschedulable=$(node_unschedulable) cordoned=$(node_ann kairon.zyvor.dev/maintenance-cordoned) state=$(node_ann kairon.zyvor.dev/maintenance-state) phase=$(field '(o.get("status") or {}).get("phase")') migrations=$(maintenance_migrations)"
    fi
  else
    result wait fail "state=$(node_ann kairon.zyvor.dev/maintenance-state) message=$(node_ann kairon.zyvor.dev/maintenance-message)"
  fi

  # stop
  read -r tick0 boot0 _ < <(last_probe)
  rid="$(runtime_id)"
  started=$(date +%s)
  kctl node maintenance enter "$NODE" --fallback stop >/dev/null
  if wait_until "$TIMEOUT" "$VM Stopped" phase_is Stopped && wait_until 90 "$NODE InMaintenance" state_is InMaintenance; then
    took=$(( $(date +%s) - started ))
    parked="$(ann kairon.zyvor.dev/maintenance-parked)"
    sleep 12
    gauge="$(metric "kairon_node_maintenance\\{node=\"$NODE\",state=\"InMaintenance\"\\}" | awk '{print $2}')"
    if [[ "$(spec_power)" == Stopped && "$parked" == *'"from":"Running"'* && "$parked" == *'"to":"Stopped"'* ]] && ! qemu_alive "$rid" &&
      [[ "$(node_event MaintenanceReady)" -ge 1 && "$gauge" == 1 ]]; then
      result stop pass "Stopped and InMaintenance in ${took}s; parked=$parked; no QEMU for $rid; MaintenanceReady Event; kairon_node_maintenance{state=InMaintenance} 1"
    else
      result stop fail "power=$(spec_power) parked=$parked qemu=$(qemu_alive "$rid" && echo alive || echo gone) events=$(node_event MaintenanceReady) gauge=$gauge"
    fi
  else
    result stop fail "phase=$(field '(o.get("status") or {}).get("phase")') state=$(node_ann kairon.zyvor.dev/maintenance-state) message=$(node_ann kairon.zyvor.dev/maintenance-message)"
  fi

  # exit
  if [[ "${RESULT[stop]}" == pass ]]; then
    started=$(date +%s)
    kctl node maintenance exit "$NODE" >/dev/null
    if wait_until 90 "$NODE out of maintenance" exited && wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" &&
      wait_until 180 "$VM probe in a new boot" probe_boot_not "$boot0"; then
      if [[ "$(node_unschedulable)" == no && "$(spec_power)" == Running && -z "$(ann kairon.zyvor.dev/maintenance-parked)" && "$(node_event MaintenanceExited)" -ge 1 ]]; then
        result exit pass "uncordoned and $VM Running again in $(( $(date +%s) - started ))s with a fresh boot (was tick $tick0 of boot ${boot0:0:8}); parked annotation removed, MaintenanceExited Event"
      else
        result exit fail "unschedulable=$(node_unschedulable) power=$(spec_power) parked=$(ann kairon.zyvor.dev/maintenance-parked)"
      fi
    else
      result exit fail "state=$(node_ann kairon.zyvor.dev/maintenance-state) phase=$(field '(o.get("status") or {}).get("phase")'): $(field '(o.get("status") or {}).get("message") or ""')"
    fi
  fi

  # hibernate
  if [[ "${RESULT[exit]:-}" == pass ]] && wait_until 60 "probe ticking" probe_seen; then
    sleep 6
    read -r tick1 boot1 shm1 < <(last_probe)
    started=$(date +%s)
    kctl node maintenance enter "$NODE" --fallback hibernate >/dev/null
    if wait_until "$TIMEOUT" "$VM Hibernated" phase_is Hibernated && wait_until 90 "$NODE InMaintenance" state_is InMaintenance; then
      took=$(( $(date +%s) - started ))
      kctl node maintenance exit "$NODE" >/dev/null
      if wait_until "$TIMEOUT" "$VM Running" phase_is Running && wait_until 120 "probe after resume" probe_after "$tick1"; then
        read -r tick2 boot2 shm2 < <(last_probe)
        if [[ "$boot2" == "$boot1" && "$shm2" == "$shm1" ]] && wait_until 60 "$NODE out of maintenance" exited; then
          result hibernate pass "Hibernated and InMaintenance in ${took}s; exit resumed it: tick $tick1 -> $tick2, same boot id and /dev/shm marker"
        else
          result hibernate fail "boot ${boot1} -> ${boot2}, shm ${shm1} -> ${shm2}"
        fi
      else
        result hibernate fail "no resume: phase=$(field '(o.get("status") or {}).get("phase")'): $(field '(o.get("status") or {}).get("message") or ""')"
      fi
    else
      result hibernate fail "phase=$(field '(o.get("status") or {}).get("phase")') state=$(node_ann kairon.zyvor.dev/maintenance-state): $(field '(o.get("status") or {}).get("message") or ""')"
    fi
  fi

  # cordon
  set_power Stopped
  if wait_until "$TIMEOUT" "$VM Stopped" phase_is Stopped; then
    kube_curl PATCH "$NODE_PATH" '{"spec":{"unschedulable":true}}' application/merge-patch+json >/dev/null
    kctl node maintenance enter "$NODE" >/dev/null
    if wait_until 90 "$NODE InMaintenance" state_is InMaintenance; then
      own="$(node_ann kairon.zyvor.dev/maintenance-cordoned)"
      kctl node maintenance exit "$NODE" >/dev/null
      if wait_until 90 "$NODE out of maintenance" exited && [[ "$(node_unschedulable)" == yes && -z "$own" ]]; then
        result cordon pass "operator-cordoned node reached InMaintenance without a maintenance-cordoned mark and stayed cordoned after exit"
      else
        result cordon fail "own-cordon mark='$own' unschedulable after exit=$(node_unschedulable)"
      fi
    else
      result cordon fail "state=$(node_ann kairon.zyvor.dev/maintenance-state)"
    fi
    kube_curl PATCH "$NODE_PATH" '{"spec":{"unschedulable":false}}' application/merge-patch+json >/dev/null
  else
    result cordon fail "$VM never stopped"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in drs wait stop exit hibernate cordon; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Node maintenance: $c | $r | $today | Node $NODE, ${NOTE[$c]:-earlier step failed} |"
done
echo "| Node maintenance: migrate Machines to another node | not run | $today | needs two nodes |"
echo "| DRS: recommend or automatic moves | not run | $today | needs two nodes |"
exit "$rc"
