#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# vGPU (mediated device) drill on one node with the kernel's mbochs sample
# driver standing in for a vGPU host driver (see docs/guides/gpu-vgpu.md).
# mbochs is a virtual mdev parent with three display profiles (4, 16 and
# 64 MiB) sharing one memory pool, and its guest device is a display
# controller (class 0x0380, 1234:1111) whose BAR0 is the profile's memory.
# A probe in the guest prints "MLAB tick=N vgpu=<count> bar0=<bytes>" every
# 2s. It checks:
#
#   inventory   FluxVM's /v1/host/mdev lists mbochs with framebuffer sizes,
#               and kairon-node publishes it in NodeTopology mediatedDevices
#   health      FluxVM's /v1/host/gpus/health judges the host's GPUs and
#               NodeTopology carries the result on the device
#   boot        a Machine asking for profile mbochs-small is scheduled with
#               a UUID on mbochs, kairon-node creates the instance, QEMU
#               holds it and the guest sees one 4 MiB vGPU
#   accounting  a second Machine asking for 4 mbochs-large (256 MiB) is
#               refused with TopologyUnsatisfiable (3 fit next to the
#               first), and the controller exports VRAM and free-instance
#               metrics
#   stop        stopping the Machine removes its instance and frees the pool
#   start       starting it again re-creates the same UUID and the guest
#               sees the vGPU again
#   delete      deleting the Machine removes the instance
#
# NVIDIA vGPU and MIG-backed profiles need an NVIDIA GPU with the vGPU host
# driver and are printed as "not run".
#
# Usage: lab-vgpu.sh --node=NAME --image=PATH [--mbochs-ko=PATH] [--namespace=ns] [--timeout=600]
# --mbochs-ko loads the module (after vfio_iommu_type1 and mdev) when
# /sys/class/mdev_bus/mbochs is missing and unloads it on exit. Needs
# KAIRON_KUBE_* (lib/lab.sh), FLUXVM_URL (default http://127.0.0.1:7788),
# and sudo for modules.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600; MBOCHS_KO=""
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
METRICS_URL="${KAIRON_CONTROLLER_METRICS_URL:-http://127.0.0.1:32301/metrics}"
export KAIRON_HW_REQUIRE_GUEST_IP="${KAIRON_HW_REQUIRE_GUEST_IP:-0}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --mbochs-ko=*) MBOCHS_KO="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" ]] || { echo "usage: $0 --node=NAME --image=PATH" >&2; exit 1; }

STAMP="$(date -u +%m%d%H%M%S)"
VM="vlab-$STAMP"
BIG="vlab-big-$STAMP"
LOADED=""
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}

cleanup() {
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$BIG")" >/dev/null 2>&1 || true
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1 || true
  if [[ -n "$LOADED" ]]; then
    for _ in $(seq 1 30); do
      sudo -n rmmod mbochs 2>/dev/null && break
      sleep 2
    done
  fi
}
trap cleanup EXIT

if [[ ! -e /sys/class/mdev_bus/mbochs ]]; then
  [[ -n "$MBOCHS_KO" ]] || { echo "mbochs is not loaded; pass --mbochs-ko=PATH" >&2; exit 1; }
  sudo -n modprobe vfio_iommu_type1
  sudo -n modprobe mdev
  sudo -n insmod "$MBOCHS_KO"
  LOADED=1
fi

probe='#!/bin/sh
i=0
while true; do
  i=$((i+1)); n=0; bar=0
  for d in /sys/bus/pci/devices/*; do
    [ "$(cat "$d/vendor")" = 0x1234 ] && [ "$(cat "$d/device")" = 0x1111 ] || continue
    case "$(cat "$d/class")" in 0x0380*) ;; *) continue ;; esac
    n=$((n+1))
    set -- $(head -1 "$d/resource")
    bar=$(( $2 - $1 + 1 ))
  done
  echo "MLAB tick=$i vgpu=$n bar0=$bar" > /dev/ttyS0
  sleep 2
done'
machine_body() {
  python3 - "$1" "$NODE" "$IMAGE" "$probe" "$2" "$3" <<'EOF'
import json, sys
name, node, image, probe, profile, count = sys.argv[1:7]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "devices": [{"name": "vga", "class": "gpu", "profile": profile, "count": int(count)}],
    "cloudInit": {
        "writeFiles": [{"path": "/usr/local/bin/vlab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit vlab-probe /usr/local/bin/vlab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "vgpu"}}, "spec": spec}))
EOF
}

field() { machine_field "$NAMESPACE" "$2" "$1" 2>/dev/null || true; }
phase_is() { [[ "$(field '(o.get("status") or {}).get("phase") or ""' "$VM")" == "$1" ]]; }
set_power() {
  kube_curl PATCH "$(machine_path "$NAMESPACE" "$VM")" "{\"spec\":{\"powerState\":\"$1\"}}" application/merge-patch+json >/dev/null
}
runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""' "$VM"; }
alloc_uuid() { field '",".join(d.get("uuid","") for d in ((o.get("spec") or {}).get("topologyAllocation") or {}).get("devices") or [])' "$VM"; }
alloc_parent() { field '",".join(d.get("parent","") for d in ((o.get("spec") or {}).get("topologyAllocation") or {}).get("devices") or [])' "$VM"; }
last_probe() {
  local rid
  rid="$(runtime_id)"
  [[ -n "$rid" ]] || return 1
  flux "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'MLAB tick=[0-9]* vgpu=[0-9]* bar0=[0-9]*' | tail -1 |
    sed -E 's/MLAB tick=([0-9]+) vgpu=([0-9]+) bar0=([0-9]+)/\1 \2 \3/'
}
probe_seen() { [[ -n "$(last_probe 2>/dev/null || true)" ]]; }
# A new runtime's console restarts the tick count.
probe_fresh() { local t; t="$(last_probe 2>/dev/null | cut -d' ' -f1)"; [[ -n "$t" && "$t" -lt "$1" ]]; }
mdev_json() { flux /v1/host/mdev; }
instance() { mdev_json | json_eval "next((f'{i[\"allocated\"]} {i[\"held\"]} {i[\"type_id\"]}' for i in o['instances'] if i['uuid']=='$1'), '')"; }
instance_gone() { [[ -z "$(instance "$1")" ]]; }
instance_held() { [[ "$(instance "$1")" == "True True mbochs-small" ]]; }
available() { mdev_json | json_eval "next(t['available_instances'] for p in o['parents'] if p['parent']=='mbochs' for t in p['types'] if t['id']=='$1')"; }
available_is() { [[ "$(available "$1")" == "$2" ]]; }
nt() { kube_curl GET "apis/kairon.zyvor.dev/v1/nodetopologies/$NODE" | json_eval "$1"; }
nt_mbochs() { nt "next((','.join(f'{t[\"id\"]}={t.get(\"framebufferMiB\",0)}' for t in p.get('types') or []) for p in (o.get('status') or {}).get('mediatedDevices') or [] if p['parent']=='mbochs'), '')"; }
nt_has_mbochs() { [[ "$(nt_mbochs)" == *mbochs-small=4* ]]; }
nt_instances() { nt "','.join(u for p in (o.get('status') or {}).get('mediatedDevices') or [] for u in p.get('instances') or [])"; }
nt_lists() { [[ "$(nt_instances)" == *"$1"* ]]; }
metric() { curl -fsS "$METRICS_URL" 2>/dev/null | grep -E "^$1" | awk '{print $2}' | head -1; }
metric_is() { [[ "$(metric "$1")" == "$2" ]]; }

# inventory
inv="$(mdev_json)"
types="$(json_eval "','.join(f'{t[\"id\"]}={t.get(\"framebuffer_mib\")}/{t[\"available_instances\"]}' for p in o['parents'] if p['parent']=='mbochs' for t in p['types'])" <<<"$inv")"
caps="$(flux /v1/runtime/capabilities | json_eval '"{} {}".format(o["host"].get("mdev"), o["host"].get("gpuHealth"))')"
if [[ "$types" == *mbochs-small=4/64* && "$types" == *mbochs-large=64/4* && "$caps" == "True True" ]] &&
  wait_until 150 "NodeTopology $NODE lists mbochs" nt_has_mbochs; then
  result inventory pass "FluxVM mdev+gpuHealth capabilities; /v1/host/mdev mbochs types (MiB/free) $types; NodeTopology mediatedDevices mbochs $(nt_mbochs)"
else
  result inventory fail "capabilities mdev/gpuHealth=$caps; types=$types; NodeTopology mbochs='$(nt_mbochs)'"
fi

# health
health="$(flux /v1/host/gpus/health | json_eval "' '.join(f'{i[\"bdf\"]}={i[\"status\"]}' for i in o['items'])")"
nt_health="$(nt "' '.join(f'{d[\"bdf\"]}={d.get(\"health\",\"\")}' for d in (o.get('status') or {}).get('devices') or [] if d['class']=='gpu')")"
if [[ -n "$health" && -n "$nt_health" && "$nt_health" != *"=" && "$nt_health" != *"= "* ]]; then
  result health pass "FluxVM: $health; NodeTopology gpu devices: $nt_health"
else
  result health fail "FluxVM: '$health'; NodeTopology: '$nt_health'"
fi

# boot
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$VM" mbochs-small 1)" >/dev/null
started=$(date +%s)
if wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" && wait_until "$TIMEOUT" "$VM probe" probe_seen; then
  took=$(( $(date +%s) - started ))
  uuid="$(alloc_uuid)"
  rid="$(runtime_id)"
  vfio="$(flux "/v1/vms/$rid" | json_eval '",".join(o["request"].get("vfio_devices") or [])')"
  cmdline="$(pgrep -af "qemu.*$rid" | grep -o "sysfsdev=[^ ]*" | head -1 || true)"
  read -r _ n bar < <(last_probe)
  if [[ "$(alloc_parent)" == mbochs && -n "$uuid" && "$vfio" == "$uuid" && "$cmdline" == *"$uuid"* && "$n" == 1 && "$bar" == 4194304 ]] && instance_held "$uuid"; then
    result boot pass "Running in ${took}s; allocation mbochs/$uuid; FluxVM vfio_devices=$vfio; QEMU $cmdline; instance allocated+held; guest sees $n display function 1234:1111 class 0x0380 with a $((bar / 1048576)) MiB BAR0"
  else
    result boot fail "parent=$(alloc_parent) uuid=$uuid vfio=$vfio qemu='$cmdline' guest vgpu=$n bar0=$bar instance='$(instance "$uuid")'"
  fi
else
  result boot fail "$(field '(o.get("status") or {}).get("phase") or ""' "$VM"): $(field '(o.get("status") or {}).get("message") or ""' "$VM")"
fi

# accounting
if [[ "${RESULT[boot]}" == pass ]]; then
  kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$BIG" mbochs-large 4)" >/dev/null
  sched() { field '" ".join(c.get("reason","")+": "+c.get("message","") for c in (o.get("status") or {}).get("conditions") or [] if c.get("type")=="Scheduled")' "$BIG"; }
  refused() { [[ "$(sched)" == *"TopologyUnsatisfiable"*"profile mbochs-large, 3 free"* ]]; }
  if wait_until 120 "$BIG refused" refused && wait_until 120 "NodeTopology lists $uuid" nt_lists "$uuid" &&
    wait_until 60 "vGPU metrics" metric_is "kairon_vgpu_available_instances\\{node=\"$NODE\",parent=\"mbochs\",profile=\"mbochs-small\"\\}" 63 &&
    metric_is "kairon_gpu_vram_mib\\{node=\"$NODE\",state=\"allocated\"\\}" 4; then
    node_name="$(field '(o.get("spec") or {}).get("nodeName") or ""' "$BIG")"
    if [[ -z "$node_name" ]]; then
      result accounting pass "4x mbochs-large stays unscheduled: '$(sched)'; kairon_vgpu_available_instances{mbochs-small} 63, kairon_gpu_vram_mib{allocated} 4, total $(metric "kairon_gpu_vram_mib\\{node=\"$NODE\",state=\"total\"\\}")"
    else
      result accounting fail "$BIG was placed on $node_name"
    fi
  else
    result accounting fail "scheduled='$(sched)' instances='$(nt_instances)' small-free=$(metric "kairon_vgpu_available_instances\\{node=\"$NODE\",parent=\"mbochs\",profile=\"mbochs-small\"\\}") vram-allocated=$(metric "kairon_gpu_vram_mib\\{node=\"$NODE\",state=\"allocated\"\\}")"
  fi
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$BIG")" >/dev/null 2>&1 || true

  # stop
  set_power Stopped
  if wait_until "$TIMEOUT" "$VM Stopped" phase_is Stopped && wait_until 60 "instance removed" instance_gone "$uuid" &&
    wait_until 30 "pool free" available_is mbochs-small 64; then
    result stop pass "Stopped; instance $uuid removed, mbochs-small free back to $(available mbochs-small), mbochs-large $(available mbochs-large)"
  else
    result stop fail "phase=$(field '(o.get("status") or {}).get("phase")' "$VM") instance='$(instance "$uuid")' small-free=$(available mbochs-small)"
  fi

  # start
  if [[ "${RESULT[stop]}" == pass ]]; then
    set_power Running
    if wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" && wait_until "$TIMEOUT" "probe in new boot" probe_fresh 10 &&
      instance_held "$uuid"; then
      read -r _ n bar < <(last_probe)
      if [[ "$(alloc_uuid)" == "$uuid" && "$n" == 1 && "$bar" == 4194304 ]]; then
        result start pass "Running again with the same UUID $uuid re-created and held; guest sees $n vGPU with a $((bar / 1048576)) MiB BAR0"
      else
        result start fail "uuid $(alloc_uuid) (was $uuid) guest vgpu=$n bar0=$bar"
      fi
    else
      result start fail "phase=$(field '(o.get("status") or {}).get("phase")' "$VM"): $(field '(o.get("status") or {}).get("message") or ""' "$VM") instance='$(instance "$uuid")'"
    fi
  fi

  # delete
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null
  gone() { ! kube_curl GET "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1; }
  if wait_until "$TIMEOUT" "$VM deleted" gone && wait_until 60 "instance removed" instance_gone "$uuid" &&
    wait_until 30 "pool free" available_is mbochs-small 64; then
    result delete pass "Machine deleted; instance $uuid removed, mbochs-small free $(available mbochs-small)"
  else
    result delete fail "instance='$(instance "$uuid")' small-free=$(available mbochs-small)"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in inventory health boot accounting stop start delete; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| vGPU: $c | $r | $today | Node $NODE, mbochs sample mdev, ${NOTE[$c]:-earlier step failed} |"
done
echo "| vGPU: NVIDIA vGPU profiles on a real GPU | not run | $today | needs an NVIDIA GPU with the vGPU host driver |"
echo "| vGPU: MIG-backed profiles, nvidia-smi health and VRAM | not run | $today | needs an NVIDIA MIG-capable GPU |"
echo "| GPU health: Unhealthy GPU skipped by the scheduler | not run | $today | needs a GPU with AER or ECC errors; unit-tested |"
exit "$rc"
