#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# NVMe-oF drill (see docs/guides/machine-storage-nvmeof.md), run on one node
# that is both storage node and Machine host. A dummy interface carries two
# tcp portals and, through soft-RoCE (rdma_rxe), an rdma portal. It checks:
#
#   tcp         nvme-tcp filesystem volume over both portals: CreateVolume,
#               stage (connect + mkfs), publish, writes, online expand
#   ana         one portal's ANA group set inaccessible: the path shows
#               unusable, writes and fsync carry on, back to optimized
#   failover    the volume unexported from one portal: that path drops,
#               writes carry on over the other, the path reconnects
#   rdma        raw Block volume over nvme-rdma (soft-RoCE): every path is
#               rdma, data written through the published device file reads
#               back from the device
#   block-boot  a Machine boots in place from an nvme-tcp Block PV:
#               kairon-node downloads --image (spec.image.source, served
#               from 127.0.0.1) and writes it onto the empty device, then
#               FluxVM boots /dev/nvme... with storage=shared
#   io          its data disks: virtio-blk with aio=io_uring, cache=none, an
#               iothread and 2 queues, and vhost-user-blk served by
#               qemu-storage-daemon; the guest sees both at their sizes
#   paths       the Machine's StoragePaths condition: True, False while a
#               portal is unexported, True again; the guest keeps running
#
# The first four drive the CSI Controller and Node servers in-process (the
# lab-tagged tests in internal/csinode) against this kernel; the rest go
# through the real binaries. Real RDMA NICs are printed as "not run".
#
# Usage: lab-nvmeof.sh --node=NAME --image=PATH [--namespace=ns] [--timeout=600]
# Needs passwordless sudo, KAIRON_KUBE_* (lib/lab.sh), FLUXVM_URL (default
# http://127.0.0.1:7788), k3s kubectl, and:
#   KAIRON_LAB_TEST     go test -c -tags lab -o ... ./internal/csinode
#   KAIRON_CSI_NODE_BIN a kairon-csi-node build from the same tree
# The Machine tolerates node.kubernetes.io/disk-pressure: its disks are
# NVMe-oF volumes. kairon-node runs as the systemd unit "kairon-node"; for the drill it gets a
# drop-in pointing it at a lab kairon-csi-node socket and image cache, and a udev rule giving
# its group the NVMe namespace devices. Everything is undone on exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
FLUXVM_WORKSPACES="${FLUXVM_WORKSPACES:-/var/lib/fluxvm/instances}"
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
LAB_TEST="${KAIRON_LAB_TEST:?set KAIRON_LAB_TEST}"
CSI_NODE_BIN="${KAIRON_CSI_NODE_BIN:?set KAIRON_CSI_NODE_BIN}"

STAMP="$(date -u +%m%d%H%M%S)"
VM="nvme-lab-$STAMP"
WORK="$(mktemp -d /tmp/nvme-lab.XXXXXX)"
NETDEV=knvmelab0
RXE=knvmelabrxe
IP1=10.211.0.1
IP2=10.211.0.2
PORTALS="tcp:$IP1:4420,tcp:$IP2:4420,rdma:$IP1:4421"
NVMET=/sys/kernel/config/nvmet
CSI_DIR=/run/kairon-lab-csi
DROPIN=/etc/systemd/system/kairon-node.service.d/lab-nvmeof.conf
IMAGE_CACHE=/var/lib/kairon/lab-nvmeof-images
UDEV_RULE=/run/udev/rules.d/99-kairon-lab-nvme.rules
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}
kctl() { sudo -n k3s kubectl "$@"; }
# labtest [VAR=value...] -test.run PATTERN
labtest() {
  local envs=()
  while [[ $# -gt 0 && "$1" != -* && "$1" == *=* ]]; do envs+=("$1"); shift; done
  sudo -n env KAIRON_LAB_NVME_PORTALS="$PORTALS" KAIRON_LAB_DIR="$WORK/csi" "${envs[@]}" "$LAB_TEST" -test.v -test.count=1 "$@"
}

csi_pid=""
http_pid=""
volume_ids=""
cleanup() {
  set +e
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1
  for _ in $(seq 1 60); do
    kube_exists "$(machine_path "$NAMESPACE" "$VM")" || break
    sleep 2
  done
  kctl delete pvc -n "$NAMESPACE" nvlab-boot nvlab-vblk nvlab-vub --ignore-not-found --wait=false >/dev/null 2>&1
  kctl delete pv "nvlab-boot-$STAMP" "nvlab-vblk-$STAMP" "nvlab-vub-$STAMP" --ignore-not-found --wait=false >/dev/null 2>&1
  if [[ -f "$DROPIN" ]]; then
    sudo -n rm -f "$DROPIN"
    sudo -n systemctl daemon-reload
    sudo -n systemctl restart kairon-node
  fi
  [[ -n "$csi_pid" ]] && sudo -n kill "$csi_pid" 2>/dev/null
  [[ -n "$http_pid" ]] && kill "$http_pid" 2>/dev/null
  [[ -n "$volume_ids" ]] && labtest KAIRON_LAB_RELEASE="$volume_ids" -test.run '^TestLabNVMeRelease$' >"$WORK/release.log" 2>&1
  sudo -n rm -f "$UDEV_RULE" && sudo -n udevadm control --reload
  for p in "$NVMET"/ports/74*; do
    [[ -d "$p" ]] || continue
    sudo -n find "$p/subsystems" -mindepth 1 -maxdepth 1 -type l -delete
    sudo -n rmdir "$p"
  done
  sudo -n rdma link delete "$RXE" 2>/dev/null
  sudo -n ip link del "$NETDEV" 2>/dev/null
  sudo -n rm -rf "$WORK" "$CSI_DIR" "$IMAGE_CACHE"
  sudo -n find /var/lib/kairon/csi -depth -type d -empty -delete 2>/dev/null
}
trap cleanup EXIT

lab_log "modules, $NETDEV ($IP1, $IP2) and soft-RoCE $RXE"
sudo -n modprobe -a nvmet nvmet_tcp nvme_tcp rdma_rxe nvmet_rdma nvme_rdma
sudo -n ip link add "$NETDEV" type dummy
sudo -n ip link set "$NETDEV" up
sudo -n ip addr add "$IP1/32" dev "$NETDEV"
sudo -n ip addr add "$IP2/32" dev "$NETDEV"
sudo -n rdma link add "$RXE" type rxe netdev "$NETDEV"
multipath="$(cat /sys/module/nvme_core/parameters/multipath)"
kernel="$(uname -r)"

# --- in-process CSI checks -------------------------------------------------
logline() { { grep -o "$1.*" "$2" || true; } | head -1 | sed 's/[|]/\//g'; }
lasterr() { { grep -E 'nvmeof_lab_test.go:[0-9]+:' "$1" || true; } | tail -1 | sed 's/^ *//; s/[|]/\//g'; }
if labtest -test.run '^TestLabNVMeTCPMultipath$' >"$WORK/tcp.log" 2>&1; then tcp_rc=0; else tcp_rc=$?; fi
connected="$(logline 'connected: ' "$WORK/tcp.log")"
if grep -q "filesystem after expand" "$WORK/tcp.log" && [[ $tcp_rc -eq 0 ]]; then
  result tcp pass "2 tcp portals, ext4, $(logline 'filesystem after expand' "$WORK/tcp.log"); ${connected}"
else
  result tcp fail "$(lasterr "$WORK/tcp.log")"
fi
if grep -q "path lost" "$WORK/tcp.log"; then
  result ana pass "$(logline 'ANA inaccessible on' "$WORK/tcp.log" | cut -c1-220); 1 MiB write+fsync went through"
elif grep -q "ANA inaccessible on" "$WORK/tcp.log"; then
  result ana fail "$(lasterr "$WORK/tcp.log")"
fi
if grep -q "path back" "$WORK/tcp.log"; then
  result failover pass "$(logline '1 MiB write+fsync with one path down' "$WORK/tcp.log"); $(logline 'path back' "$WORK/tcp.log" | cut -c1-200)"
elif grep -q "path lost" "$WORK/tcp.log"; then
  result failover fail "$(lasterr "$WORK/tcp.log")"
fi

if labtest -test.run '^TestLabNVMeRDMABlock$' >"$WORK/rdma.log" 2>&1; then
  result rdma pass "soft-RoCE on a dummy netdev; 4 KiB pattern at 64 MiB read back, 128 MiB device; $(logline 'connected: ' "$WORK/rdma.log" | cut -c1-200)"
else
  result rdma fail "$(lasterr "$WORK/rdma.log")"
fi

# --- Machine on NVMe-oF Block PVs ------------------------------------------
lab_log "provisioning boot/data volumes"
if ! labtest KAIRON_LAB_PROVISION="pvc-nvlab-boot-$STAMP:4096,pvc-nvlab-vblk-$STAMP:1024,pvc-nvlab-vub-$STAMP:1024" \
  KAIRON_LAB_PROVISION_OUT="$WORK/volumes" -test.run '^TestLabNVMeProvision$' >"$WORK/provision.log" 2>&1; then
  result block-boot fail "provisioning: $(tail -3 "$WORK/provision.log" | tr '\n' ' ')"
else
  volume_ids="$(awk '{print $2}' "$WORK/volumes" | paste -sd, -)"
  boot_id="$(awk 'NR==1{print $2}' "$WORK/volumes")"
  boot_nqn="$(cut -d'|' -f3 <<<"$boot_id")"

  python3 - "$WORK/volumes" "$NAMESPACE" "$STAMP" >"$WORK/pv.yaml" <<'EOF'
import json, sys
path, ns, stamp = sys.argv[1:4]
items = []
for line in open(path):
    name, vid, size = line.split()
    short = name.split("-")[2]  # pvc-nvlab-<short>-stamp
    pv, pvc = f"nvlab-{short}-{stamp}", f"nvlab-{short}"
    items.append({"apiVersion": "v1", "kind": "PersistentVolume", "metadata": {"name": pv},
                  "spec": {"capacity": {"storage": size}, "accessModes": ["ReadWriteOnce"], "volumeMode": "Block",
                           "persistentVolumeReclaimPolicy": "Retain", "storageClassName": "",
                           "claimRef": {"namespace": ns, "name": pvc},
                           "csi": {"driver": "csi.kairon.zyvor.dev", "volumeHandle": vid}}})
    items.append({"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": {"name": pvc, "namespace": ns},
                  "spec": {"accessModes": ["ReadWriteOnce"], "volumeMode": "Block", "storageClassName": "",
                           "volumeName": pv, "resources": {"requests": {"storage": size}}}})
print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
EOF
  kctl apply -f "$WORK/pv.yaml" >/dev/null

  lab_log "kairon-csi-node on $CSI_DIR/csi.sock, kairon-node drop-in, udev rule"
  # kairon-node (user kairon, ProtectSystem=strict here) creates and reads
  # its CSI directories, which kairon-csi-node (root) fills.
  sudo -n mkdir -p "$CSI_DIR" /var/lib/kairon/csi/staging /var/lib/kairon/csi/publish
  sudo -n chgrp kairon /var/lib/kairon /var/lib/kairon/csi /var/lib/kairon/csi/staging /var/lib/kairon/csi/publish
  sudo -n chmod 0750 /var/lib/kairon
  sudo -n chmod 0770 /var/lib/kairon/csi /var/lib/kairon/csi/staging /var/lib/kairon/csi/publish
  sudo -n bash -c "nohup '$CSI_NODE_BIN' -endpoint unix://$CSI_DIR/csi.sock -node-id '$NODE' >'$WORK/csi-node.log' 2>&1 & echo \$! >'$WORK/csi-node.pid'"
  csi_pid="$(cat "$WORK/csi-node.pid")"
  wait_until 30 "kairon-csi-node socket" test -S "$CSI_DIR/csi.sock"
  sudo -n chgrp kairon "$CSI_DIR" "$CSI_DIR/csi.sock"
  sudo -n chmod 0750 "$CSI_DIR"
  sudo -n chmod 0660 "$CSI_DIR/csi.sock"
  echo 'SUBSYSTEM=="block", KERNEL=="nvme*n*", GROUP="kairon", MODE="0660"' | sudo -n tee "$UDEV_RULE" >/dev/null
  sudo -n udevadm control --reload
  sudo -n mkdir -p "$(dirname "$DROPIN")"
  sudo -n install -d -o kairon -g kairon -m 0750 "$IMAGE_CACHE"
  printf '[Service]\nEnvironment=KAIRON_CSI_SOCKET=%s/csi.sock\nEnvironment=KAIRON_IMAGE_CACHE_DIR=%s\nReadWritePaths=/var/lib/kairon/csi %s\n' \
    "$CSI_DIR" "$IMAGE_CACHE" "$IMAGE_CACHE" | sudo -n tee "$DROPIN" >/dev/null
  sudo -n systemctl daemon-reload
  sudo -n systemctl restart kairon-node

  probe='#!/bin/sh
i=0
while true; do
  i=$((i+1))
  echo "NVLAB tick=$i root=$(findmnt -no SOURCE /) disks=$(lsblk -bdno NAME,SIZE | grep ^vd | tr " " ":" | tr "\n" ",")" > /dev/ttyS0
  sleep 2
done'
  image_digest="sha256:$(sha256sum "$IMAGE" | cut -d' ' -f1)"
  image_format="$(qemu-img info --output=json "$IMAGE" | json_eval 'o["format"]')"
  python3 -m http.server --bind 127.0.0.1 --directory "$(dirname "$IMAGE")" 18731 >"$WORK/http.log" 2>&1 &
  http_pid=$!
  image_url="http://127.0.0.1:18731/$(basename "$IMAGE")"
  wait_until 30 "image server" curl -fs -o /dev/null -I "$image_url"
  body="$(python3 - "$VM" "$NODE" "$image_url" "$image_digest" "$image_format" "$probe" <<'EOF'
import json, sys
name, node, url, digest, fmt, probe = sys.argv[1:7]
spec = {
    "image": {"digest": digest, "source": {"httpURL": url, "format": fmt}},
    "resources": {"cpu": "2", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    # Its disks are NVMe-oF volumes, not node-local storage.
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node},
                  "tolerations": [{"key": "node.kubernetes.io/disk-pressure", "operator": "Exists", "effect": "NoSchedule"}]},
    "volumes": [{"name": "boot", "claimName": "nvlab-boot"}],
    "disks": [
        {"name": "vblk", "claimName": "nvlab-vblk",
         "io": {"bus": "virtio-blk", "aio": "io_uring", "cache": "none", "iothread": True, "queues": 2}},
        {"name": "vub", "claimName": "nvlab-vub", "io": {"bus": "vhost-user-blk", "queues": 2}},
    ],
    "cloudInit": {
        "writeFiles": [{"path": "/usr/local/bin/nvlab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit nvlab-probe /usr/local/bin/nvlab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "nvmeof"}}, "spec": spec}))
EOF
)"
  kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null

  field() { machine_field "$NAMESPACE" "$VM" "$1" 2>/dev/null || true; }
  runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""'; }
  last_probe() {
    local rid
    rid="$(runtime_id)"
    [[ -n "$rid" ]] || return 1
    flux "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'NVLAB tick=[0-9]* root=[^ ]* disks=[^ "\\]*' | tail -1
  }
  probe_seen() { [[ -n "$(last_probe 2>/dev/null || true)" ]]; }
  tick() { { last_probe 2>/dev/null || true; } | sed -E 's/NVLAB tick=([0-9]+).*/\1/'; }
  probe_after() { local t; t="$(tick)"; [[ -n "$t" && "$t" -gt "$1" ]]; }
  cond() { field "next((c.get(\"$1\") or \"\") for c in ((o.get(\"status\") or {}).get(\"conditions\") or []) if c.get(\"type\") == \"StoragePaths\")"; }
  cond_is() { [[ "$(cond status)" == "$1" ]]; }
  # No guest agent reports an IP; the console probe shows the guest runs.
  running() { KAIRON_HW_REQUIRE_GUEST_IP=0 machine_healthy "$NAMESPACE" "$VM"; }

  if ! wait_until "$TIMEOUT" "$VM Running" running ||
    ! wait_until "$TIMEOUT" "$VM probe on the console" probe_seen; then
    result block-boot fail "$VM never ran its probe: $(field '(o.get("status") or {}).get("message") or ""')"
  else
    rid="$(runtime_id)"
    p="$(last_probe || true)"
    image_path="$(flux "/v1/vms/$rid" | json_eval '((o.get("spec") or {}).get("image") or {}).get("path") or o.get("image") or ""' || true)"
    qemu_args="$(ps -eo args | grep "[q]emu-system.*$rid" | head -1 || true)"
    qsd_count="$(ps -eo args | grep -c "[q]emu-storage-daemon.*$rid/disks/vub" || true)"
    handle="$(field '(o.get("status") or {}).get("volumeHandle") or ""')"
    if [[ "$handle" == "$boot_id" && "$qemu_args" == *"/dev/nvme"* && "$p" == *"root=/dev/vda"* ]]; then
      result block-boot pass "4 GiB nvme-tcp Block PV, image written onto it; QEMU root on $(grep -o '/dev/nvme[0-9]*n[0-9]*' <<<"$qemu_args" | head -1), guest root ${p#*root=}"
    else
      result block-boot fail "handle ok=$([[ "$handle" == "$boot_id" ]] && echo y || echo n), FluxVM image '$image_path', probe '$p'"
    fi
    disks="${p#*disks=}"
    # The data disks are hot-added, so QEMU's own view comes over QMP; QMP
    # does not report aio, which is checked in FluxVM's per-disk record.
    miss="$(sudo -n python3 - "$FLUXVM_WORKSPACES/$rid" <<'EOF' || echo " qmp-error"
import json, socket, sys
ws = sys.argv[1]
s = socket.socket(socket.AF_UNIX)
s.connect(ws + "/qmp.sock")
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
qtree = qmp("human-monitor-command", **{"command-line": "info qtree"}) or ""
def queues(dev):
    rest = qtree.split(dev, 1)[1] if dev in qtree else ""
    return next((l.split("=")[1].split()[0] for l in rest.splitlines() if "num-queues" in l), "")
nodes = {n["node-name"]: n for n in qmp("query-named-block-nodes", flat=True) or []}
side = json.load(open(ws + "/disks/vblk.io.json"))["io"]
miss = []
if not qmp("qom-get", path="/machine/peripheral/disk-vblk/virtio-backend", property="iothread"):
    miss.append("vblk-iothread")
if queues('virtio-blk-pci, id "disk-vblk"') != "2":
    miss.append("vblk-num-queues=2")
if not (nodes.get("data-vblk") or {}).get("cache", {}).get("direct"):
    miss.append("vblk-cache.direct")
if side.get("aio") != "io_uring":
    miss.append("vblk-aio=io_uring")
if queues('vhost-user-blk-pci, id "disk-vub"') != "2":
    miss.append("vub-vhost-user-blk-pci/num-queues=2")
print("".join(" " + m for m in miss))
EOF
)"
    [[ "$qsd_count" == 1 ]] || miss+=" one-qemu-storage-daemon(found $qsd_count)"
    [[ "$disks" == *"1073741824"*"1073741824"* ]] || miss+=" guest-disk-sizes"
    if [[ -z "$miss" ]]; then
      result io pass "QEMU over QMP: virtio-blk with an iothread, num-queues=2, cache.direct (cache=none), aio=io_uring in FluxVM's disk record; vhost-user-blk-pci num-queues=2 on one qemu-storage-daemon; guest disks $disks"
    else
      result io fail "missing:$miss; guest disks $disks"
    fi

    if wait_until 60 "StoragePaths True" cond_is True; then
      before="$(cond message)"
      t0="$(tick || true)"
      sudo -n rm "$NVMET/ports/7400/subsystems/$boot_nqn"
      if wait_until 60 "StoragePaths False" cond_is False; then
        degraded="$(cond reason)"
        sudo -n ln -s "$NVMET/subsystems/$boot_nqn" "$NVMET/ports/7400/subsystems/$boot_nqn"
        if wait_until 120 "StoragePaths True again" cond_is True && wait_until 60 "guest still ticking" probe_after "$t0"; then
          result paths pass "True ($(cut -c1-90 <<<"$before")...), $degraded with portal $IP1:4420 unexported, True after re-export; guest kept ticking"
        else
          result paths fail "never recovered: $(cond message | cut -c1-200)"
        fi
      else
        sudo -n ln -sf "$NVMET/subsystems/$boot_nqn" "$NVMET/ports/7400/subsystems/$boot_nqn"
        result paths fail "condition stayed $(cond status): $(cond message | cut -c1-200)"
      fi
    else
      result paths fail "no StoragePaths True: '$(cond status)' $(cond message | cut -c1-200)"
    fi
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in tcp ana failover rdma block-boot io paths; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| NVMe-oF: $c | $r | $today | Node $NODE, kernel $kernel, nvme_core.multipath=$multipath; ${NOTE[$c]:-earlier step failed} |"
done
echo "| NVMe-oF: rdma on a real RDMA NIC | not run | $today | soft-RoCE only in this lab |"
exit $rc
