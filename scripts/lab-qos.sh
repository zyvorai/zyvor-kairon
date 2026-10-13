#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Per-disk I/O QoS drill on one node (see docs/guides/disk-qos.md). A Machine
# boots with a root IOPS limit and a hostPath-PVC data disk with a bandwidth
# limit. A probe in the guest times O_DIRECT writes each round and prints
# "QLAB n=N data_kibps=K root_iops=I" on the serial console. It checks:
#
#   boot           Machine status, FluxVM's disk list and QEMU (QMP
#                  query-block) carry both limits; the guest measures at or
#                  under them
#   live-change    raising both limits in spec reaches QEMU with the same
#                  QEMU process, and the guest measures the new rates
#   lift           removing the root limit clears it in QEMU and status; the
#                  guest's root IOPS go past the old limit
#   recreate       with the root limit back, powerState Stopped then
#                  Running: kairon-node creates a new runtime whose QEMU has
#                  the root limit from create and the re-attached data
#                  disk's limit
#   fluxvm-boot    a plain FluxVM VM (no Kairon): root_disk_io.throttle from
#                  create, a data disk hot-added with io.throttle, and a live
#                  PUT .../disks/root/throttle are all in QEMU, and again
#                  after stop/start
#   crd-validation the CRD refuses iopsTotal with iopsRead, and a throttle on
#                  a vhost-user-blk disk
#   ch-virtiofs    a FluxVM Cloud Hypervisor VM with a virtiofs share keeps
#                  running and has the fs device (Cloud Hypervisor has no
#                  DAX option to pass)
#
# Usage: lab-qos.sh --node=NAME --image=PATH [--ch-firmware=PATH] [--namespace=ns] [--timeout=600]
# Needs passwordless sudo, KAIRON_KUBE_* (lib/lab.sh), FLUXVM_URL (default
# http://127.0.0.1:7788) and k3s kubectl.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600
CH_FIRMWARE=/usr/local/share/cloud-hypervisor/CLOUDHV.fd
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
FLUXVM_WORKSPACES="${FLUXVM_WORKSPACES:-/var/lib/fluxvm/instances}"
export KAIRON_HW_REQUIRE_GUEST_IP="${KAIRON_HW_REQUIRE_GUEST_IP:-0}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --ch-firmware=*) CH_FIRMWARE="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" ]] || { echo "usage: $0 --node=NAME --image=PATH" >&2; exit 1; }

STAMP="$(date -u +%m%d%H%M%S)"
VM="qlab-$STAMP"
BAD="qlab-bad-$STAMP"
PV="qlab-data-$STAMP"
PVC="qlab-data"
DATA_DIR="/var/lib/fluxvm/lab-qos-$STAMP"
MIB=1048576
ROOT_IOPS=200
ROOT_IOPS2=400
DATA_BPS=$((10 * MIB))
DATA_BPS2=$((20 * MIB))
plain_id=""
ch_id=""
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local method="$1" path="$2" body="${3:-}" auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  if [[ -n "$body" ]]; then
    curl -fsS -X "$method" "${auth[@]}" -H 'Content-Type: application/json' --data "$body" "$FLUXVM_URL$path"
  else
    curl -fsS -X "$method" "${auth[@]}" "$FLUXVM_URL$path"
  fi
}
kctl() { sudo -n k3s kubectl "$@"; }

cleanup() {
  set +e
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$BAD")" >/dev/null 2>&1
  [[ -n "$plain_id" ]] && flux DELETE "/v1/vms/$plain_id" >/dev/null 2>&1
  [[ -n "$ch_id" ]] && flux DELETE "/v1/vms/$ch_id" >/dev/null 2>&1
  for _ in $(seq 1 60); do
    kube_exists "$(machine_path "$NAMESPACE" "$VM")" || break
    sleep 2
  done
  kctl delete pvc -n "$NAMESPACE" "$PVC" --ignore-not-found --wait=false >/dev/null 2>&1
  kctl delete pv "$PV" --ignore-not-found --wait=false >/dev/null 2>&1
  sudo -n rm -rf "$DATA_DIR"
}
trap cleanup EXIT

# qmp_throttles WORKSPACE -- "virtio0 iops=N bps=N; disk-x iops=N bps=N; ..."
qmp_throttles() {
  sudo -n python3 - "$1" <<'EOF'
import json, socket, sys
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1] + "/qmp.sock")
f = s.makefile("rw")
f.readline()
def qmp(cmd, **args):
    f.write(json.dumps({"execute": cmd, "arguments": args}) + "\n")
    f.flush()
    while True:
        r = json.loads(f.readline())
        if "return" in r or "error" in r:
            return r.get("return")
qmp("qmp_capabilities")
out = []
for b in qmp("query-block") or []:
    ins = b.get("inserted")
    if not ins:
        continue
    name = b.get("device") or b.get("qdev", "").split("/")[3 if b.get("qdev", "").startswith("/machine/peripheral/") else -1]
    out.append(f"{name} iops={ins.get('iops', 0)} bps={ins.get('bps', 0)}")
print("; ".join(out))
EOF
}
qemu_pid() { pgrep -f "qemu-system.*$1" | head -1 || true; }
# qmp_rw WORKSPACE QDEV_ID -- "iops_rd=N iops_wr=N" of that device.
qmp_rw() {
  sudo -n python3 - "$1" "$2" <<'EOF'
import json, socket, sys
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1] + "/qmp.sock")
f = s.makefile("rw")
f.readline()
def qmp(cmd, **args):
    f.write(json.dumps({"execute": cmd, "arguments": args}) + "\n")
    f.flush()
    while True:
        r = json.loads(f.readline())
        if "return" in r or "error" in r:
            return r.get("return")
qmp("qmp_capabilities")
for b in qmp("query-block") or []:
    if sys.argv[2] in b.get("qdev", "").split("/") and b.get("inserted"):
        i = b["inserted"]
        print(f"iops_rd={i.get('iops_rd', 0)} iops_wr={i.get('iops_wr', 0)}")
EOF
}

today="$(date -u +%Y-%m-%d)"

# --- crd-validation --------------------------------------------------------
bad_body() {
  python3 - "$BAD" "$IMAGE" "$1" <<'EOF'
import json, sys
name, image, case = sys.argv[1:4]
spec = {"image": {"path": image}, "resources": {"cpu": "1", "memory": "512Mi"}, "runtime": {"backend": "qemu"}}
if case == "total-and-read":
    spec["image"]["io"] = {"throttle": {"iopsTotal": 100, "iopsRead": 50}}
else:
    spec["disks"] = [{"name": "v", "claimName": "c", "io": {"bus": "vhost-user-blk", "throttle": {"iopsTotal": 100}}}]
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine", "metadata": {"name": name}, "spec": spec}))
EOF
}
codes=""
for c in total-and-read vhost-user-blk; do
  code="$(curl -sS -o "/tmp/qlab-bad-$c.json" -w '%{http_code}' -X POST -H "Authorization: Bearer $KAIRON_KUBE_TOKEN" \
    -H 'Content-Type: application/json' ${KAIRON_KUBE_INSECURE:+-k} --data "$(bad_body "$c")" \
    "$KAIRON_KUBE_URL/$LAB_API/namespaces/$NAMESPACE/machines" || true)"
  msg="$(json_eval '(o.get("message") or "")' <"/tmp/qlab-bad-$c.json" 2>/dev/null | grep -oE '(iopsTotal excludes[^"]*|vhost-user-blk bypasses[^"]*)' | head -1 || true)"
  codes+="$c=$code ($msg); "
  rm -f "/tmp/qlab-bad-$c.json"
  [[ "$code" == 422 && -n "$msg" ]] || bad=1
done
if [[ -z "${bad:-}" ]]; then
  result crd-validation pass "${codes%; }"
else
  result crd-validation fail "${codes%; }"
fi

sudo -n mkdir -p "$DATA_DIR"
sudo -n truncate -s 1G "$DATA_DIR/disk.img"
sudo -n truncate -s 256M "$DATA_DIR/plain.img"

# --- fluxvm-boot -----------------------------------------------------------
lab_log "plain FluxVM VM with root_disk_io.throttle"
plain="$(flux POST /v1/vms "{\"name\":\"qlab-plain-$STAMP\",\"backend\":\"qemu\",\"image\":\"$IMAGE\",\"vcpus\":1,\"memory_mib\":512,\"root_disk_io\":{\"throttle\":{\"iops_total\":300,\"bps_total\":$((50 * MIB))}}}" || true)"
plain_id="$(json_eval 'o.get("id") or ""' <<<"$plain" 2>/dev/null || true)"
if [[ -z "$plain_id" ]]; then
  result fluxvm-boot fail "create failed: $(cut -c1-200 <<<"$plain")"
else
  plain_ws="$FLUXVM_WORKSPACES/$plain_id"
  plain_running() { [[ "$(flux GET "/v1/vms/$plain_id" | json_eval 'o.get("status") or ""')" == running ]]; }
  wait_until 120 "plain VM running" plain_running || true
  first="$(qmp_throttles "$plain_ws" 2>&1 || true)"
  flux POST "/v1/vms/$plain_id/disks" "{\"name\":\"pdata\",\"path\":\"$DATA_DIR/plain.img\",\"io\":{\"throttle\":{\"iops_read\":150,\"iops_write\":120}}}" >/dev/null || true
  flux PUT "/v1/vms/$plain_id/disks/root/throttle" '{"throttle":{"iops_total":350}}' >/dev/null || true
  live="$(qmp_throttles "$plain_ws" 2>&1 || true)"
  pid1="$(qemu_pid "$plain_id")"
  flux POST "/v1/vms/$plain_id/stop" >/dev/null || true
  plain_stopped() { [[ "$(flux GET "/v1/vms/$plain_id" | json_eval 'o.get("status") or ""')" == stopped ]]; }
  wait_until 120 "plain VM stopped" plain_stopped || true
  flux POST "/v1/vms/$plain_id/start" >/dev/null || true
  wait_until 120 "plain VM running again" plain_running || true
  second="$(qmp_throttles "$plain_ws" 2>&1 || true)"
  pid2="$(qemu_pid "$plain_id")"
  pread="$(qmp_rw "$plain_ws" disk-pdata 2>&1 || true)"
  if [[ "$first" == *"virtio0 iops=300 bps=$((50 * MIB))"* && "$live" == *"virtio0 iops=350 bps=0"* &&
    "$second" == *"virtio0 iops=350 bps=0"* && "$pread" == "iops_rd=150 iops_wr=120" && -n "$pid2" && "$pid1" != "$pid2" ]]; then
    result fluxvm-boot pass "QMP after create: root iops=300 bps=50 MiB/s; live PUT root iops=350 and hot-added pdata iops_rd=150 iops_wr=120; after stop/start (QEMU pid $pid1 -> $pid2): root iops=350, pdata $pread"
  else
    result fluxvm-boot fail "first '$first', live '$live', second '$second', pdata '$pread', pids $pid1 -> $pid2"
  fi
  flux DELETE "/v1/vms/$plain_id" >/dev/null || true
  plain_id=""
fi

# --- ch-virtiofs ----------------------------------------------------------
if [[ -f "$CH_FIRMWARE" ]] && command -v cloud-hypervisor >/dev/null; then
  lab_log "Cloud Hypervisor VM with a virtiofs share"
  sudo -n mkdir -p "$DATA_DIR/share"
  ch="$(flux POST /v1/vms "{\"name\":\"qlab-ch-$STAMP\",\"backend\":\"cloud-hypervisor\",\"image\":\"$IMAGE\",\"vcpus\":1,\"memory_mib\":512,\"network\":{\"mode\":\"none\"},\"firmware\":\"$CH_FIRMWARE\",\"shared_folders\":[{\"host_path\":\"$DATA_DIR/share\",\"guest_path\":\"/mnt/share\"}]}" || true)"
  ch_id="$(json_eval 'o.get("id") or ""' <<<"$ch" 2>/dev/null || true)"
  ch_pid="$(json_eval 'o.get("pid") or ""' <<<"$ch" 2>/dev/null || true)"
  sleep 20
  ch_ws="$FLUXVM_WORKSPACES/$ch_id"
  fs="$(sudo -n ch-remote --api-socket "$ch_ws/ch-api.sock" info 2>/dev/null | json_eval '";".join(f.get("tag", "") + "@" + f.get("socket", "").rsplit("/", 1)[-1] for f in ((o.get("config") or {}).get("fs") or []))' 2>/dev/null || true)"
  chv="$(cloud-hypervisor --version | head -1)"
  if [[ -n "$ch_pid" ]] && ps -p "$ch_pid" >/dev/null && [[ "$fs" == "fs0@virtiofs-0.sock" ]]; then
    result ch-virtiofs pass "$chv; CH pid $ch_pid alive after 20s, fs device $fs"
  else
    result ch-virtiofs fail "$chv; pid '$ch_pid', fs '$fs'; $(sudo -n tail -2 "$ch_ws/console.log" 2>/dev/null | tr '\n' ' ' | cut -c1-200)"
  fi
  flux DELETE "/v1/vms/$ch_id" >/dev/null || true
  ch_id=""
fi

# --- Machine ---------------------------------------------------------------
lab_log "hostPath PV $PV ($DATA_DIR/disk.img, 1 GiB raw)"
pv_json="$(cat <<EOF
{"apiVersion": "v1", "kind": "List", "items": [
 {"apiVersion": "v1", "kind": "PersistentVolume", "metadata": {"name": "$PV"},
  "spec": {"capacity": {"storage": "1Gi"}, "accessModes": ["ReadWriteOnce"], "persistentVolumeReclaimPolicy": "Retain",
           "storageClassName": "", "claimRef": {"namespace": "$NAMESPACE", "name": "$PVC"}, "hostPath": {"path": "$DATA_DIR"}}},
 {"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": "$PVC", "namespace": "$NAMESPACE"},
  "spec": {"accessModes": ["ReadWriteOnce"], "storageClassName": "", "volumeName": "$PV", "resources": {"requests": {"storage": "1Gi"}}}}
]}
EOF
)"
applied_pv() { kctl apply -f - >/dev/null <<<"$pv_json"; }
wait_until 120 "PV and PVC created" applied_pv

probe='#!/bin/sh
dev=/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_qdata
f=/var/tmp/qlab.bin
dd if=/dev/zero of=$f bs=4k count=600 status=none
sync
n=0
while true; do
  n=$((n+1))
  d=none
  if [ -b $dev ]; then
    s=$(date +%s%N); dd if=/dev/zero of=$dev bs=1M count=24 oflag=direct status=none; e=$(date +%s%N)
    d=$((25165824 * 1000000000 / (e - s) / 1024))
  fi
  s=$(date +%s%N); dd if=/dev/zero of=$f bs=4k count=600 oflag=direct conv=notrunc status=none; e=$(date +%s%N)
  r=$((600 * 1000000000 / (e - s)))
  echo "QLAB n=$n data_kibps=$d root_iops=$r" > /dev/ttyS0
  sleep 1
done'
machine_body() {
  python3 - "$VM" "$NODE" "$IMAGE" "$probe" "$ROOT_IOPS" "$DATA_BPS" "$PVC" <<'EOF'
import json, sys
name, node, image, probe, iops, bps, pvc = sys.argv[1:8]
spec = {
    "image": {"path": image, "io": {"throttle": {"iopsTotal": int(iops)}}},
    "resources": {"cpu": "2", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "disks": [{"name": "qdata", "claimName": pvc, "io": {"throttle": {"bpsTotal": int(bps)}}}],
    "cloudInit": {
        "writeFiles": [{"path": "/usr/local/bin/qlab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit qlab-probe /usr/local/bin/qlab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "qos"}}, "spec": spec}))
EOF
}
create_machine() { kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body)" >/dev/null 2>&1 || kube_exists "$(machine_path "$NAMESPACE" "$VM")"; }
wait_until 120 "$VM created" create_machine
patch() {
  local _
  for _ in 1 2 3 4 5; do
    kube_curl PATCH "$(machine_path "$NAMESPACE" "$VM")" "$1" application/merge-patch+json >/dev/null && return 0
    sleep 3
  done
  return 1
}

field() { machine_field "$NAMESPACE" "$VM" "$1" 2>/dev/null || true; }
runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""'; }
applied() { field '";".join(t["disk"] + ":" + ",".join(k + "=" + str(v) for k, v in sorted(t.get("throttle", {}).items())) for t in (o.get("status") or {}).get("appliedDiskThrottles") or [])'; }
last_probe() {
  local rid
  rid="$(runtime_id)"
  [[ -n "$rid" ]] || return 1
  flux GET "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'QLAB n=[0-9]* data_kibps=[0-9a-z]* root_iops=[0-9]*' | tail -1
}
tick() { { last_probe 2>/dev/null || true; } | sed -E 's/QLAB n=([0-9]+).*/\1/'; }
probe_with_data() { [[ "$(last_probe 2>/dev/null || true)" == *"data_kibps="[0-9]* ]]; }
# rounds_after T -- true once two full rounds have started after tick T.
rounds_after() { local t; t="$(tick)"; [[ -n "$t" && "$t" -gt $(($1 + 2)) ]]; }
kibps() { sed -E 's/.*data_kibps=([0-9]+).*/\1/' <<<"$1"; }
iops() { sed -E 's/.*root_iops=([0-9]+).*/\1/' <<<"$1"; }
# within VALUE LIMIT -- VALUE between half and 1.25x LIMIT.
within() { (($1 * 100 <= $2 * 125 && $1 * 2 >= $2)); }
applied_is() { [[ "$(applied)" == "$1" ]]; }
running() { machine_healthy "$NAMESPACE" "$VM"; }

if ! wait_until "$TIMEOUT" "$VM Running" running ||
  ! wait_until "$TIMEOUT" "$VM probe with the data disk" probe_with_data; then
  result boot fail "$VM never ran its probe: $(field '(o.get("status") or {}).get("message") or ""')"
else
  rid="$(runtime_id)"
  ws="$FLUXVM_WORKSPACES/$rid"
  want1="qdata:bpsTotal=$DATA_BPS;root:iopsTotal=$ROOT_IOPS"
  wait_until 60 "status.appliedDiskThrottles" applied_is "$want1" || true
  wait_until 120 "two probe rounds" rounds_after "$(tick)" || true
  p="$(last_probe || true)"
  q="$(qmp_throttles "$ws" 2>&1 || true)"
  fl="$(flux GET "/v1/vms/$rid/disks" | json_eval '"; ".join(d["name"] + ":" + json.dumps(d.get("throttle"), sort_keys=True) for d in o.get("items") or [])' || true)"
  pid="$(qemu_pid "$rid")"
  if [[ "$(applied)" == "$want1" && "$q" == *"virtio0 iops=$ROOT_IOPS bps=0"* && "$q" == *"disk-qdata iops=0 bps=$DATA_BPS"* ]] &&
    within "$(kibps "$p")" $((DATA_BPS / 1024)) && within "$(iops "$p")" "$ROOT_IOPS"; then
    result boot pass "status $want1; QMP '$q'; FluxVM disks '$fl'; guest $(kibps "$p") KiB/s on qdata (limit $((DATA_BPS / 1024))), $(iops "$p") 4k IOPS on root (limit $ROOT_IOPS)"
  else
    result boot fail "status '$(applied)'; QMP '$q'; FluxVM '$fl'; probe '$p'"
  fi

  lab_log "raising both limits"
  t0="$(tick)"
  patch "{\"spec\":{\"image\":{\"io\":{\"throttle\":{\"iopsTotal\":$ROOT_IOPS2}}},\"disks\":[{\"name\":\"qdata\",\"claimName\":\"$PVC\",\"io\":{\"throttle\":{\"bpsTotal\":$DATA_BPS2}}}]}}"
  want2="qdata:bpsTotal=$DATA_BPS2;root:iopsTotal=$ROOT_IOPS2"
  wait_until 60 "status updated" applied_is "$want2" || true
  t0="$(tick)"
  wait_until 120 "two probe rounds" rounds_after "$t0" || true
  p="$(last_probe || true)"
  q="$(qmp_throttles "$ws" 2>&1 || true)"
  pid2="$(qemu_pid "$rid")"
  if [[ "$(applied)" == "$want2" && "$q" == *"virtio0 iops=$ROOT_IOPS2 bps=0"* && "$q" == *"disk-qdata iops=0 bps=$DATA_BPS2"* && "$pid" == "$pid2" ]] &&
    within "$(kibps "$p")" $((DATA_BPS2 / 1024)) && within "$(iops "$p")" "$ROOT_IOPS2" &&
    (($(kibps "$p") * 100 > DATA_BPS / 1024 * 125)) && (($(iops "$p") * 100 > ROOT_IOPS * 125)); then
    result live-change pass "same QEMU pid $pid; QMP '$q'; guest $(kibps "$p") KiB/s (limit $((DATA_BPS2 / 1024))), $(iops "$p") IOPS (limit $ROOT_IOPS2)"
  else
    result live-change fail "status '$(applied)'; QMP '$q'; pid $pid -> $pid2; probe '$p'"
  fi

  lab_log "lifting the root limit"
  patch '{"spec":{"image":{"io":null}}}'
  want3="qdata:bpsTotal=$DATA_BPS2"
  wait_until 60 "root dropped from status" applied_is "$want3" || true
  t0="$(tick)"
  wait_until 120 "two probe rounds" rounds_after "$t0" || true
  p="$(last_probe || true)"
  q="$(qmp_throttles "$ws" 2>&1 || true)"
  if [[ "$(applied)" == "$want3" && "$q" == *"virtio0 iops=0 bps=0"* ]] && (($(iops "$p") * 100 > ROOT_IOPS2 * 150)); then
    result lift pass "status $want3; QMP '$q'; guest root $(iops "$p") IOPS (was limited to $ROOT_IOPS2)"
  else
    result lift fail "status '$(applied)'; QMP '$q'; probe '$p'"
  fi

  lab_log "recreate with the root limit back"
  patch "{\"spec\":{\"image\":{\"io\":{\"throttle\":{\"iopsTotal\":$ROOT_IOPS}}}}}"
  wait_until 60 "root back in status" applied_is "qdata:bpsTotal=$DATA_BPS2;root:iopsTotal=$ROOT_IOPS" || true
  patch '{"spec":{"powerState":"Stopped"}}'
  stopped() { [[ "$(field '(o.get("status") or {}).get("phase") or ""')" == Stopped ]]; }
  wait_until 180 "$VM Stopped" stopped || true
  patch '{"spec":{"powerState":"Running"}}'
  new_runtime() { local r; r="$(runtime_id)"; [[ -n "$r" && "$r" != "$rid" ]]; }
  if wait_until 180 "new runtime" new_runtime && wait_until "$TIMEOUT" "$VM Running" running; then
    rid2="$(runtime_id)"
    qmp_has_both() {
      local q
      q="$(qmp_throttles "$FLUXVM_WORKSPACES/$rid2" 2>/dev/null || true)"
      [[ "$q" == *"virtio0 iops=$ROOT_IOPS bps=0"* && "$q" == *"disk-qdata iops=0 bps=$DATA_BPS2"* ]]
    }
    wait_until 120 "limits in the new QEMU" qmp_has_both || true
    q="$(qmp_throttles "$FLUXVM_WORKSPACES/$rid2" 2>&1 || true)"
    rootio="$(flux GET "/v1/vms/$rid2" | json_eval 'json.dumps(((o.get("request") or {}).get("root_disk_io") or {}).get("throttle"), sort_keys=True)' || true)"
    if [[ "$q" == *"virtio0 iops=$ROOT_IOPS bps=0"* && "$q" == *"disk-qdata iops=0 bps=$DATA_BPS2"* && "$rootio" == "{\"iops_total\": $ROOT_IOPS}" ]]; then
      result recreate pass "runtime $rid -> $rid2; FluxVM create request root_disk_io.throttle $rootio; QMP '$q'"
    else
      result recreate fail "runtime $rid -> $rid2; root_disk_io.throttle '$rootio'; QMP '$q'"
    fi
  else
    result recreate fail "no new runtime or not Running: $(field '(o.get("status") or {}).get("message") or ""')"
  fi
fi

rc=0
echo
kernel="$(uname -r)"
qemu="$(qemu-system-x86_64 --version | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
for c in boot live-change lift recreate fluxvm-boot crd-validation ch-virtiofs; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Disk QoS: $c | $r | $today | Node $NODE, kernel $kernel, QEMU $qemu; ${NOTE[$c]:-earlier step failed} |"
done
echo "| Disk QoS: limits across live migration | not run | $today | one lab node |"
echo "| virtiofs DAX | not supported | $today | QEMU $qemu vhost-user-fs-pci has no cache-size; Cloud Hypervisor v53 --fs has no dax |"
exit $rc
