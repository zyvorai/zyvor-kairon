#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Elastic memory drill (experimental feature; see
# docs/guides/machine-elastic-memory.md). On --node, it checks:
#
#   balloon-device  an elastic Machine boots with a virtio-balloon and
#                   kairon-node writes status.memory (and, when this script
#                   runs on the node, QEMU's command line has
#                   free-page-reporting=on)
#   inflate         forced pressure (--pressure-cmd) inflates the balloon
#                   toward min and status.memory says HostPressure
#   exhausted       with the balloon at min and pressure persisting, the
#                   node reports KaironMemoryPressure=True and a new Machine
#                   pinned to it stays unscheduled
#   relief          after --relief-cmd the balloon deflates back to 0 and
#                   KaironMemoryPressure goes False
#   overcommit      two elastic Machines whose full sizes exceed the node's
#                   allocatable memory: the second stays unscheduled until
#                   the node gets kairon.zyvor.dev/memory-overcommit-ratio
#
# and prints COMPATIBILITY.md rows.
#
# Usage:
#   lab-elastic-memory.sh --node=NAME --image=PATH \
#     --pressure-cmd='...' --relief-cmd='...' [--namespace=ns] [--timeout=600]
#
# --pressure-cmd must make kairon-node see pressure, e.g. set
# KAIRON_MEMORY_LOW_WATERMARK_MIB above the host's MemAvailable and restart
# it; --relief-cmd must undo that. Needs KAIRON_KUBE_* (lib/lab.sh) and a
# FluxVM that reports host.balloon. The Machines it creates and the ratio
# annotation are removed on exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600; PRESSURE_CMD=""; RELIEF_CMD=""
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --pressure-cmd=*) PRESSURE_CMD="${arg#*=}" ;;
    --relief-cmd=*) RELIEF_CMD="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" && -n "$PRESSURE_CMD" && -n "$RELIEF_CMD" ]] ||
  { echo "usage: $0 --node=NAME --image=PATH --pressure-cmd=CMD --relief-cmd=CMD" >&2; exit 1; }

STAMP="$(date -u +%m%d%H%M%S)"
CREATED=()
RELIEVED=1
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

RATIO_ANNOTATION="kairon.zyvor.dev/memory-overcommit-ratio"
set_ratio() {
  local v="null"
  [[ -n "$1" ]] && v="\"$1\""
  kube_curl PATCH "api/v1/nodes/$NODE" "{\"metadata\":{\"annotations\":{\"$RATIO_ANNOTATION\":$v}}}" application/merge-patch+json >/dev/null
}

cleanup() {
  local m
  for m in "${CREATED[@]}"; do kube_curl DELETE "$(machine_path "$NAMESPACE" "$m")" >/dev/null 2>&1 || true; done
  set_ratio "" 2>/dev/null || true
  if [[ "$RELIEVED" == 0 ]]; then bash -c "$RELIEF_CMD" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

# create_machine NAME MEMORY MIN POWER_STATE
create_machine() {
  local body
  body="$(python3 - "$1" "$2" "$3" "$4" "$NODE" "$IMAGE" <<'EOF'
import json, sys
name, mem, mn, power, node, image = sys.argv[1:7]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": mem},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "powerState": power,
}
if mn:
    spec["resources"]["elasticMemory"] = {"min": mn}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "elastic-memory"}}, "spec": spec}))
EOF
)"
  kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null
  CREATED+=("$1")
}

mem_field() { machine_field "$NAMESPACE" "$1" "((o.get(\"status\") or {}).get(\"memory\") or {}).get(\"$2\")"; }
has_memory_status() { [[ -n "$(mem_field "$1" balloonTargetMiB 2>/dev/null)" ]]; }
target_is() { [[ "$(mem_field "$1" balloonTargetMiB 2>/dev/null)" == "$2" ]]; }
target_above() { local t; t="$(mem_field "$1" balloonTargetMiB 2>/dev/null)"; [[ -n "$t" && "$t" -gt "$2" ]]; }
node_condition() {
  kube_curl GET "api/v1/nodes/$NODE" |
    json_eval 'next(((c.get("status") or "") for c in (o.get("status") or {}).get("conditions") or [] if c.get("type") == "KaironMemoryPressure"), "")'
}
pressure_is() { [[ "$(node_condition 2>/dev/null)" == "$1" ]]; }
scheduled() { [[ -n "$(machine_field "$NAMESPACE" "$1" '(o.get("spec") or {}).get("nodeName") or ""' 2>/dev/null)" ]]; }
schedule_message() { machine_field "$NAMESPACE" "$1" '(o.get("status") or {}).get("message") or ""' 2>/dev/null || true; }
summary() {
  machine_field "$NAMESPACE" "$1" '"target %sMiB actual %sMiB min %sMiB reason %s" % tuple(((o.get("status") or {}).get("memory") or {}).get(k) for k in ("balloonTargetMiB", "balloonActualMiB", "minMiB", "reason"))'
}

caps="$(curl -fsS "${FLUXVM_URL:-http://127.0.0.1:7788}/v1/runtime/capabilities" 2>/dev/null || true)"
if [[ -n "$caps" && "$(json_eval '((o.get("host") or {}).get("balloon"))' <<<"$caps")" != True ]]; then
  lab_log "FluxVM at ${FLUXVM_URL:-http://127.0.0.1:7788} does not report host.balloon"
fi

VM="emem-lab-$STAMP"
create_machine "$VM" 2Gi 1Gi Running
if ! wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM"; then
  result balloon-device fail "$VM never became healthy: $(schedule_message "$VM")"
elif ! wait_until 120 "$VM status.memory" has_memory_status "$VM"; then
  result balloon-device fail "no status.memory after 120s (kairon-node reclaim off, or FluxVM without host.balloon)"
else
  note="status.memory $(summary "$VM")"
  rid="$(machine_field "$NAMESPACE" "$VM" '(o.get("status") or {}).get("runtimeID") or ""')"
  qemu_dev="$(pgrep -af qemu 2>/dev/null | grep -F -- "$rid" | grep -o 'virtio-balloon-pci[^ ]*' | head -1 || true)"
  if [[ -n "$qemu_dev" ]]; then
    note+="; qemu $qemu_dev"
    [[ "$qemu_dev" == *free-page-reporting=on* ]] || { result balloon-device fail "$note: free-page-reporting not on"; qemu_dev=bad; }
  else
    note+="; QEMU command line not checked (not on the node)"
  fi
  [[ "$qemu_dev" == bad ]] || result balloon-device pass "$note"
fi

if [[ "${RESULT[balloon-device]}" == pass ]]; then
  RELIEVED=0
  bash -c "$PRESSURE_CMD"
  if wait_until 180 "$VM balloon inflating" target_above "$VM" 0; then
    reason="$(mem_field "$VM" reason)"
    if [[ "$reason" == HostPressure ]]; then
      result inflate pass "$(summary "$VM")"
    else
      result inflate fail "target moved but reason $reason"
    fi
  else
    result inflate fail "balloon target still $(mem_field "$VM" balloonTargetMiB 2>/dev/null) after 180s"
  fi

  if wait_until 180 "$VM at min" target_is "$VM" 1024 && wait_until 60 "KaironMemoryPressure True" pressure_is True; then
    blocked="emem-lab-blocked-$STAMP"
    create_machine "$blocked" 512Mi "" Running
    sleep 30
    why="$(schedule_message "$blocked")"
    if scheduled "$blocked"; then
      result exhausted fail "KaironMemoryPressure True but $blocked was scheduled"
    elif [[ "$why" != *KaironMemoryPressure* ]]; then
      result exhausted fail "$blocked unscheduled for another reason: $why"
    else
      result exhausted pass "$(summary "$VM"); KaironMemoryPressure True; new Machine unscheduled: $why"
    fi
    kube_curl DELETE "$(machine_path "$NAMESPACE" "$blocked")" >/dev/null 2>&1 || true
  else
    result exhausted fail "$(summary "$VM"), KaironMemoryPressure $(node_condition 2>/dev/null)"
  fi

  bash -c "$RELIEF_CMD"
  RELIEVED=1
  if wait_until 300 "$VM deflated" target_is "$VM" 0 && wait_until 60 "KaironMemoryPressure False" pressure_is False; then
    result relief pass "$(summary "$VM"); KaironMemoryPressure False"
  else
    result relief fail "$(summary "$VM"), KaironMemoryPressure $(node_condition 2>/dev/null)"
  fi
fi
kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1 || true

alloc="$(kube_curl GET "api/v1/nodes/$NODE" | json_eval '(o.get("status") or {}).get("allocatable", {}).get("memory") or ""')"
alloc_mib="$(python3 - "$alloc" <<'EOF'
import re, sys
m = re.fullmatch(r"(\d+)(Ki|Mi|Gi)?", sys.argv[1])
n, u = int(m.group(1)), m.group(2) or ""
print({"Ki": n // 1024, "Mi": n, "Gi": n * 1024, "": n // (1024 * 1024)}[u])
EOF
)"
each=$(( alloc_mib * 6 / 10 ))
a="emem-lab-oc-a-$STAMP"; b="emem-lab-oc-b-$STAMP"
# Running: the controller does not schedule Stopped Machines. Each is
# deleted as soon as the case is decided; guests touch little of it.
create_machine "$a" "${each}Mi" 512Mi Running
if ! wait_until 120 "$a scheduled" scheduled "$a"; then
  result overcommit fail "$a (${each}Mi) not scheduled: $(schedule_message "$a")"
else
  create_machine "$b" "${each}Mi" 512Mi Running
  sleep 30
  if scheduled "$b"; then
    result overcommit fail "$b scheduled without a ratio: 2x ${each}Mi on ${alloc_mib}Mi allocatable"
  else
    before="$(schedule_message "$b")"
    set_ratio 1.5
    if wait_until 120 "$b scheduled" scheduled "$b"; then
      result overcommit pass "2x ${each}Mi (min 512Mi) on ${alloc_mib}Mi allocatable: second unscheduled ($before), scheduled with ratio 1.5"
    else
      result overcommit fail "ratio 1.5 set but $b still unscheduled: $(schedule_message "$b")"
    fi
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in balloon-device inflate exhausted relief overcommit; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Elastic memory: $c | $r | $today | Node $NODE, ${NOTE[$c]:-earlier step failed} |"
done
exit "$rc"
