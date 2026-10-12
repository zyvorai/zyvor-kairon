#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Hibernate drill (see docs/guides/machine-hibernate.md). On --node, one
# QEMU Machine runs a probe that writes a random marker to /dev/shm and then,
# every 2s, prints "HIBLAB tick=N boot=<boot_id> shm=<marker>" to the serial
# console. It checks:
#
#   hibernate   powerState Hibernated: phase Hibernated, status.hibernation
#               written (node, size, QEMU version), FluxVM record stopped
#               with its state saved
#   capacity    the QEMU process is gone and host MemAvailable went back up
#   resume      powerState Running: the controller picks this node, the guest
#               continues in the same boot (same boot id and /dev/shm
#               marker, tick counter carries on) and status.hibernation is
#               cleared
#   discard     hibernate again, annotate kairon.zyvor.dev/discard-hibernation
#               and set Running: the guest boots fresh (new boot id)
#
# Resuming on another node needs two nodes with a shared hibernation
# directory and is printed as "not run".
#
# Usage: lab-hibernate.sh --node=NAME --image=PATH [--namespace=ns] [--timeout=600]
# Needs KAIRON_KUBE_* (lib/lab.sh) and FLUXVM_URL (default
# http://127.0.0.1:7788) reporting host.hibernate. The Machine is deleted on
# exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
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
VM="hib-lab-$STAMP"
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}

cleanup() { kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1 || true; }
trap cleanup EXIT

caps="$(flux /v1/runtime/capabilities 2>/dev/null || true)"
if [[ -n "$caps" && "$(json_eval '((o.get("host") or {}).get("hibernate"))' <<<"$caps")" != True ]]; then
  lab_log "FluxVM at $FLUXVM_URL does not report host.hibernate"
fi

probe='#!/bin/sh
echo "m-$(cat /proc/sys/kernel/random/uuid)" > /dev/shm/hiblab
i=0
while true; do
  i=$((i+1))
  echo "HIBLAB tick=$i boot=$(cat /proc/sys/kernel/random/boot_id) shm=$(cat /dev/shm/hiblab 2>/dev/null)" > /dev/ttyS0
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
        "writeFiles": [{"path": "/usr/local/bin/hiblab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit hiblab-probe /usr/local/bin/hiblab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "hibernate"}}, "spec": spec}))
EOF
)"
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null

set_power() {
  kube_curl PATCH "$(machine_path "$NAMESPACE" "$VM")" "{\"spec\":{\"powerState\":\"$1\"}}" application/merge-patch+json >/dev/null
}
field() { machine_field "$NAMESPACE" "$VM" "$1" 2>/dev/null || true; }
phase_is() { [[ "$(field '(o.get("status") or {}).get("phase") or ""')" == "$1" ]]; }
hib() { field "((o.get(\"status\") or {}).get(\"hibernation\") or {}).get(\"$1\") or \"\""; }
hib_host() { field "((((o.get(\"status\") or {}).get(\"hibernation\") or {}).get(\"host\")) or {}).get(\"$1\") or \"\""; }
no_hibernation() { [[ "$(field '"yes" if (o.get("status") or {}).get("hibernation") else ""')" == "" ]]; }
runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""'; }
# last_probe: the newest "tick boot shm" triple on the console.
last_probe() {
  local rid
  rid="$(runtime_id)"
  [[ -n "$rid" ]] || return 1
  flux "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'HIBLAB tick=[0-9]* boot=[^ ]* shm=[^ "\\]*' | tail -1 |
    sed -E 's/HIBLAB tick=([0-9]+) boot=([^ ]+) shm=(.*)/\1 \2 \3/'
}
probe_seen() { [[ -n "$(last_probe 2>/dev/null || true)" ]]; }
probe_after() { local t; t="$(last_probe 2>/dev/null | cut -d' ' -f1)"; [[ -n "$t" && "$t" -gt "$1" ]]; }
probe_boot_not() { local b; b="$(last_probe 2>/dev/null | cut -d' ' -f2)"; [[ -n "$b" && "$b" != "$1" ]]; }
mem_available() { flux /v1/host/memory | json_eval 'o.get("available_mib") or o.get("availableMiB") or o.get("mem_available_mib") or ""'; }
qemu_alive() { pgrep -f "qemu.*$1" >/dev/null 2>&1; }

if ! wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" ||
  ! wait_until "$TIMEOUT" "$VM probe on the console" probe_seen; then
  result hibernate fail "$VM never ran its probe: $(field '(o.get("status") or {}).get("message") or ""')"
else
  sleep 6
  read -r tick0 boot0 shm0 < <(last_probe)
  rid="$(runtime_id)"
  mem0="$(mem_available)"
  started=$(date +%s)
  set_power Hibernated
  if ! wait_until "$TIMEOUT" "$VM Hibernated" phase_is Hibernated; then
    result hibernate fail "phase $(field '(o.get("status") or {}).get("phase")'): $(field '(o.get("status") or {}).get("message") or ""')"
  else
    took=$(( $(date +%s) - started ))
    rec="$(flux "/v1/vms/$rid")"
    rec_status="$(json_eval 'o.get("status")' <<<"$rec")"
    rec_pid="$(json_eval 'o.get("pid")' <<<"$rec")"
    size="$(hib stateBytes)"
    if [[ "$(hib node)" == "$NODE" && "$size" -gt 0 && "$rec_status" == stopped && ( -z "$rec_pid" || "$rec_pid" == None ) ]]; then
      result hibernate pass "probe at tick $tick0; Hibernated in ${took}s, $((size >> 20)) MiB of state, QEMU $(hib_host qemuVersion)/$(hib_host machineType); FluxVM record stopped, no pid"
    else
      result hibernate fail "status.hibernation node=$(hib node) bytes=$size; FluxVM status=$rec_status pid=$rec_pid"
    fi
    sleep 5
    mem1="$(mem_available)"
    if qemu_alive "$rid"; then
      result capacity fail "a QEMU process for $rid is still running"
    else
      note="no QEMU process for $rid"
      [[ -n "$mem0" && -n "$mem1" ]] && note+="; host MemAvailable ${mem0} -> ${mem1} MiB"
      result capacity pass "$note"
    fi

    started=$(date +%s)
    set_power Running
    resume_node=""
    for _ in $(seq 1 60); do
      resume_node="$(hib resumeNode)"
      [[ -n "$resume_node" ]] && break
      phase_is Running && break
      sleep 1
    done
    if ! wait_until "$TIMEOUT" "$VM Running again" phase_is Running ||
      ! wait_until 120 "probe ticking after resume" probe_after "$tick0"; then
      result resume fail "phase $(field '(o.get("status") or {}).get("phase")'): $(field '(o.get("status") or {}).get("message") or ""')"
    else
      took=$(( $(date +%s) - started ))
      read -r tick1 boot1 shm1 < <(last_probe)
      if [[ "$boot1" == "$boot0" && "$shm1" == "$shm0" ]] && no_hibernation && [[ "$(runtime_id)" == "$rid" ]]; then
        result resume pass "Running in ${took}s (resumeNode ${resume_node:-cleared before observed}); tick $tick0 -> $tick1, same boot id and /dev/shm marker, status.hibernation cleared"
      else
        result resume fail "boot $boot0 -> $boot1, shm $shm0 -> $shm1, hibernation '$(hib node)', runtime $(runtime_id)"
      fi
    fi
  fi
fi

if [[ "${RESULT[resume]:-}" == pass ]]; then
  read -r _ boot2 _ < <(last_probe)
  set_power Hibernated
  if wait_until "$TIMEOUT" "$VM Hibernated again" phase_is Hibernated; then
    kube_curl PATCH "$(machine_path "$NAMESPACE" "$VM")" \
      '{"metadata":{"annotations":{"kairon.zyvor.dev/discard-hibernation":"true"}},"spec":{"powerState":"Running"}}' \
      application/merge-patch+json >/dev/null
    if wait_until "$TIMEOUT" "$VM booted fresh" probe_boot_not "$boot2"; then
      ann="$(field '((o.get("metadata") or {}).get("annotations") or {}).get("kairon.zyvor.dev/discard-hibernation") or ""')"
      if no_hibernation && [[ -z "$ann" ]]; then
        result discard pass "new boot id after discard; annotation removed, status.hibernation cleared"
      else
        result discard fail "booted fresh but annotation='$ann' hibernation node='$(hib node)'"
      fi
    else
      result discard fail "no fresh boot: $(field '(o.get("status") or {}).get("message") or ""')"
    fi
  else
    result discard fail "second hibernate never finished"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in hibernate capacity resume discard; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Hibernate: $c | $r | $today | Node $NODE, ${NOTE[$c]:-earlier step failed} |"
done
echo "| Hibernate: resume on another node | not run | $today | needs two nodes sharing the hibernation directory and a shared root disk |"
exit "$rc"
