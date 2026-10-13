#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# MachineAcceleratorProfile drill on one node, with the kernel's mbochs
# sample mdev driver standing in for a vGPU host driver (as lab-vgpu.sh).
# mbochs has one 256 MiB pool; mbochs-large instances take 64 MiB each and
# show up in the guest as display functions 1234:1111 with a 64 MiB BAR0.
# A probe in the guest prints "MLAB tick=N vgpu=<count> bar0=<bytes>".
# It checks:
#
#   profile-status  a pair profile (2 mbochs-large, p2pRequired, sameNuma)
#                   is Ready with capacity 2 on the node; a live-migration
#                   profile is not Ready; a 1Gi-per-device profile is Ready
#                   but not Available
#   crd-validation  the CRD refuses memory.minimumPerDevice on class
#                   accelerator
#   expand-boot     a Machine with spec.accelerators gets the profile's
#                   devices and the record annotation, is placed with two
#                   instances on mbochs, and the guest sees two 64 MiB vGPUs
#   capacity        the pair profile then reports capacity 1 and one Machine,
#                   in status and in kairon_accelerator_profile_* metrics
#   unavailable     Machines naming a missing profile or the live-migration
#                   profile stay Pending with AcceleratorProfileUnavailable
#   unsatisfiable   a Machine with the 1Gi profile stays Pending with
#                   TopologyUnsatisfiable
#   snapshot        editing the pair profile does not change the running
#                   Machine's devices or record
#   delete          deleting the Machine frees its instances; the profile
#                   reports capacity 2 again
#
# On a cluster that also runs KubeVirt, turn on its DisableMDEVConfiguration
# feature gate for the run: virt-handler otherwise removes mdev instances it
# did not create, under a running guest.
#
# Usage: lab-accel.sh --node=NAME --image=PATH [--mbochs-ko=PATH] [--namespace=ns] [--timeout=600]
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
VM="alab-$STAMP"
MISSING="alab-missing-$STAMP"
LIVE="alab-live-$STAMP"
HUGE="alab-huge-$STAMP"
PAIR="alab-pair-$STAMP"
LIVEP="alab-live-$STAMP"
HUGEP="alab-huge-$STAMP"
PROFILES="$LAB_API/machineacceleratorprofiles"
LOADED=""
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}

cleanup() {
  for m in "$VM" "$MISSING" "$LIVE" "$HUGE"; do
    kube_curl DELETE "$(machine_path "$NAMESPACE" "$m")" >/dev/null 2>&1 || true
  done
  for p in "$PAIR" "$LIVEP" "$HUGEP"; do
    kube_curl DELETE "$PROFILES/$p" >/dev/null 2>&1 || true
  done
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

profile_body() {
  python3 - "$1" "$2" <<'EOF'
import json, sys
name, spec = sys.argv[1], json.loads(sys.argv[2])
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "MachineAcceleratorProfile",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "accel"}}, "spec": spec}))
EOF
}

machine_body() {
  python3 - "$1" "$NODE" "$IMAGE" "$probe" "$2" <<'EOF'
import json, sys
name, node, image, probe, profile = sys.argv[1:6]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "accelerators": [{"name": "vga", "profile": profile}],
    "cloudInit": {
        "writeFiles": [{"path": "/usr/local/bin/alab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit alab-probe /usr/local/bin/alab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "accel"}}, "spec": spec}))
EOF
}

field() { machine_field "$NAMESPACE" "$2" "$1" 2>/dev/null || true; }
pfield() { kube_curl GET "$PROFILES/$2" 2>/dev/null | json_eval "$1" 2>/dev/null || true; }
pcond() { pfield "next((c['status']+'/'+c.get('reason','') for c in (o.get('status') or {}).get('conditions') or [] if c['type']=='$2'), '')" "$1"; }
pcap() { pfield "'{} {}'.format((o.get('status') or {}).get('capacity'), ','.join(f\"{n['node']}={n['fits']}\" for n in (o.get('status') or {}).get('nodes') or []))" "$1"; }
pmachines() { pfield "(o.get('status') or {}).get('machines')" "$1"; }
pcap_is() { [[ "$(pcap "$1")" == "$2" ]]; }
pcond_is() { [[ "$(pcond "$1" "$2")" == "$3" ]]; }
runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""' "$VM"; }
alloc() { field '",".join(d.get("parent","")+"/"+d.get("profile","")+"/"+d.get("uuid","") for d in ((o.get("spec") or {}).get("topologyAllocation") or {}).get("devices") or [])' "$VM"; }
devices() { field 'json.dumps((o.get("spec") or {}).get("devices") or [], sort_keys=True)' "$VM"; }
record() { field '((o.get("metadata") or {}).get("annotations") or {}).get("kairon.zyvor.dev/accelerators") or ""' "$VM"; }
sched() { field '" ".join(c.get("reason","")+": "+c.get("message","") for c in (o.get("status") or {}).get("conditions") or [] if c.get("type")=="Scheduled")' "$1"; }
sched_has() { [[ "$(sched "$1")" == *"$2"* ]]; }
last_probe() {
  local rid
  rid="$(runtime_id)"
  [[ -n "$rid" ]] || return 1
  flux "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'MLAB tick=[0-9]* vgpu=[0-9]* bar0=[0-9]*' | tail -1 |
    sed -E 's/MLAB tick=([0-9]+) vgpu=([0-9]+) bar0=([0-9]+)/\1 \2 \3/'
}
probe_seen() { [[ -n "$(last_probe 2>/dev/null || true)" ]]; }
instances() { flux /v1/host/mdev | json_eval "' '.join(sorted(i['uuid'] for i in o['instances'] if i['parent']=='mbochs'))"; }
metric() { curl -fsS "$METRICS_URL" 2>/dev/null | grep -E "^$1" | awk '{print $2}' | head -1; }
metric_is() { [[ "$(metric "$1")" == "$2" ]]; }

# profile-status
kube_curl POST "$PROFILES" "$(profile_body "$PAIR" '{"profile":"mbochs-large","count":2,"topology":{"p2pRequired":true,"sameNuma":true}}')" >/dev/null
kube_curl POST "$PROFILES" "$(profile_body "$LIVEP" '{"profile":"mbochs-small","migration":{"required":true}}')" >/dev/null
kube_curl POST "$PROFILES" "$(profile_body "$HUGEP" '{"profile":"mbochs-large","memory":{"minimumPerDevice":"1Gi"}}')" >/dev/null
if wait_until 120 "$PAIR capacity" pcap_is "$PAIR" "2 $NODE=2" && wait_until 60 "$LIVEP not Ready" pcond_is "$LIVEP" Ready False/LiveMigrationUnsupported &&
  wait_until 60 "$HUGEP not Available" pcond_is "$HUGEP" Available False/NoCapacity; then
  result profile-status pass "$PAIR Ready=$(pcond "$PAIR" Ready) Available=$(pcond "$PAIR" Available) capacity $(pcap "$PAIR"); $LIVEP Ready=$(pcond "$LIVEP" Ready); $HUGEP Ready=$(pcond "$HUGEP" Ready) Available=$(pcond "$HUGEP" Available): $(pfield "next((c.get('message','') for c in o['status']['conditions'] if c['type']=='Available'), '')" "$HUGEP")"
else
  result profile-status fail "$PAIR $(pcond "$PAIR" Ready) $(pcap "$PAIR"); $LIVEP $(pcond "$LIVEP" Ready); $HUGEP $(pcond "$HUGEP" Available)"
fi

# crd-validation
if out="$(kube_curl POST "$PROFILES" "$(profile_body "alab-bad-$STAMP" '{"class":"accelerator","memory":{"minimumPerDevice":"1Gi"}}')" 2>&1)"; then
  kube_curl DELETE "$PROFILES/alab-bad-$STAMP" >/dev/null 2>&1 || true
  result crd-validation fail "the API server accepted memory.minimumPerDevice on class accelerator"
elif [[ "$out" == *422* ]]; then
  result crd-validation pass "class accelerator with memory.minimumPerDevice refused with 422"
else
  result crd-validation fail "$out"
fi

# expand-boot
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$VM" "$PAIR")" >/dev/null
started=$(date +%s)
if wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" && wait_until "$TIMEOUT" "$VM probe" probe_seen; then
  took=$(( $(date +%s) - started ))
  read -r _ n bar < <(last_probe)
  want_devs='[{"alignment": ["sameNuma", "samePcieRoot"], "class": "gpu", "count": 2, "name": "vga", "profile": "mbochs-large"}]'
  a="$(alloc)"
  if [[ "$(record)" == "vga=$PAIR@1" && "$(devices)" == "$want_devs" && "$a" == mbochs/mbochs-large/*,mbochs/mbochs-large/* && "$n" == 2 && "$bar" == 67108864 ]]; then
    result expand-boot pass "Running in ${took}s; record $(record); spec.devices $(devices); allocation $a; guest sees $n display functions with a $((bar / 1048576)) MiB BAR0"
  else
    result expand-boot fail "record=$(record) devices=$(devices) alloc=$a guest vgpu=$n bar0=$bar"
  fi
else
  result expand-boot fail "$(field '(o.get("status") or {}).get("phase") or ""' "$VM"): $(field '(o.get("status") or {}).get("message") or ""' "$VM")"
fi

# capacity
if [[ "${RESULT[expand-boot]}" == pass ]]; then
  if wait_until 120 "$PAIR capacity 1" pcap_is "$PAIR" "1 $NODE=1" &&
    wait_until 60 "capacity metric" metric_is "kairon_accelerator_profile_capacity\\{node=\"$NODE\",profile=\"$PAIR\"\\}" 1 &&
    metric_is "kairon_accelerator_profile_machines\\{profile=\"$PAIR\"\\}" 1 && [[ "$(pmachines "$PAIR")" == 1 ]]; then
    result capacity pass "status capacity $(pcap "$PAIR"), machines $(pmachines "$PAIR"); kairon_accelerator_profile_capacity{$PAIR,$NODE} 1, kairon_accelerator_profile_machines 1"
  else
    result capacity fail "capacity $(pcap "$PAIR") machines $(pmachines "$PAIR") metric $(metric "kairon_accelerator_profile_capacity\\{node=\"$NODE\",profile=\"$PAIR\"\\}")"
  fi
fi

# unavailable
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$MISSING" "alab-nope-$STAMP")" >/dev/null
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$LIVE" "$LIVEP")" >/dev/null
if wait_until 120 "$MISSING waiting" sched_has "$MISSING" "AcceleratorProfileUnavailable: waiting for MachineAcceleratorProfile" &&
  wait_until 120 "$LIVE waiting" sched_has "$LIVE" "AcceleratorProfileUnavailable: MachineAcceleratorProfile \"$LIVEP\" cannot be used"; then
  if [[ -z "$(field '(o.get("spec") or {}).get("nodeName") or ""' "$MISSING")$(field '(o.get("spec") or {}).get("nodeName") or ""' "$LIVE")" ]]; then
    result unavailable pass "missing profile: '$(sched "$MISSING")'; live-migration profile: '$(sched "$LIVE")'"
  else
    result unavailable fail "a waiting Machine was placed"
  fi
else
  result unavailable fail "missing: '$(sched "$MISSING")'; live: '$(sched "$LIVE")'"
fi
kube_curl DELETE "$(machine_path "$NAMESPACE" "$MISSING")" >/dev/null 2>&1 || true
kube_curl DELETE "$(machine_path "$NAMESPACE" "$LIVE")" >/dev/null 2>&1 || true

# unsatisfiable
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$HUGE" "$HUGEP")" >/dev/null
if wait_until 120 "$HUGE refused" sched_has "$HUGE" "TopologyUnsatisfiable" && [[ -z "$(field '(o.get("spec") or {}).get("nodeName") or ""' "$HUGE")" ]]; then
  result unsatisfiable pass "record $(field '((o.get("metadata") or {}).get("annotations") or {}).get("kairon.zyvor.dev/accelerators") or ""' "$HUGE"); '$(sched "$HUGE")'"
else
  result unsatisfiable fail "'$(sched "$HUGE")'"
fi
kube_curl DELETE "$(machine_path "$NAMESPACE" "$HUGE")" >/dev/null 2>&1 || true

# snapshot
if [[ "${RESULT[expand-boot]}" == pass ]]; then
  before="$(devices)"
  kube_curl PATCH "$PROFILES/$PAIR" '{"spec":{"count":1}}' application/merge-patch+json >/dev/null
  gen2() { [[ "$(pfield "(o.get('status') or {}).get('observedGeneration')" "$PAIR")" == 2 ]]; }
  if wait_until 120 "$PAIR generation 2 observed" gen2 && sleep 20 && [[ "$(devices)" == "$before" && "$(record)" == "vga=$PAIR@1" ]] &&
    [[ "$(alloc)" == mbochs/mbochs-large/*,mbochs/mbochs-large/* ]]; then
    result snapshot pass "profile count 2 -> 1 (generation 2, now capacity $(pcap "$PAIR")); the Machine keeps spec.devices count 2, record $(record) and both instances"
  else
    result snapshot fail "devices=$(devices) (was $before) record=$(record) alloc=$(alloc)"
  fi
fi

# delete
if [[ "${RESULT[expand-boot]}" == pass ]]; then
  uuids="$(alloc | tr ',' '\n' | cut -d/ -f3 | sort | tr '\n' ' ')"
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null
  gone() { ! kube_curl GET "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1; }
  freed() { local i; i="$(instances)"; for u in $uuids; do [[ "$i" == *"$u"* ]] && return 1; done; return 0; }
  if wait_until "$TIMEOUT" "$VM deleted" gone && wait_until 60 "instances removed" freed &&
    wait_until 120 "$PAIR capacity back" pcap_is "$PAIR" "4 $NODE=4"; then
    result delete pass "Machine deleted, instances $uuids removed; $PAIR (count now 1) capacity $(pcap "$PAIR"), machines $(pmachines "$PAIR")"
  else
    result delete fail "instances='$(instances)' capacity $(pcap "$PAIR") machines $(pmachines "$PAIR")"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in profile-status crd-validation expand-boot capacity unavailable unsatisfiable snapshot delete; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Accelerator profile: $c | $r | $today | Node $NODE, mbochs sample mdev, ${NOTE[$c]:-earlier step failed} |"
done
echo "| Accelerator profile: whole GPUs with p2pRequired under one PCIe root | not run | $today | needs two allowlisted GPUs; unit-tested |"
echo "| Accelerator profile: RDMA NIC next to the GPUs | not run | $today | needs a GPU and a NIC under one PCIe root; unit-tested |"
echo "| Accelerator profile: sameNuma on a multi-NUMA host | not run | $today | lab host has one NUMA node |"
echo "| Accelerator profile: NVIDIA vGPU or MIG profile | not run | $today | needs an NVIDIA GPU |"
exit "$rc"
