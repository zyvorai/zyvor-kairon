#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Virtual network drill (experimental feature; see
# docs/guides/virtual-networks.md). On one --node it creates two
# VirtualNetworks with the same CIDR -- "a" with egress SNAT, "b" without --
# and three Machines: a1 and a2 on "a", b1 on "b" with a1's address. Each
# guest runs a probe loop from cloud-init that writes results to its serial
# console, which this script reads back through FluxVM's /v1/vms/{id}/logs
# (the lab image needs no guest agent). It checks:
#
#   ipam          both networks Ready with a VNI, each Machine allocated the
#                 address it asked for, status.network.virtualNetwork set
#   same-host     a1 and a2 ping each other on the node
#   gateway       a1 pings the distributed gateway
#   isolation     b1 (overlapping CIDR, a1's address) cannot reach a2's
#                 address
#   snat          a1 reaches --egress-target with ping and HTTP (TCP)
#                 through the node's uplink
#   closed        b1 (no egressSNAT) cannot reach --egress-target
#   cleanup       after the Machines go, kairon-node removes both networks
#                 from FluxVM
#
# and prints COMPATIBILITY.md rows, plus "not run" rows for what one node
# cannot show (cross-host Geneve, floating IPs, BGP).
#
# Usage:
#   lab-virtual-network.sh --node=NAME --image=PATH [--namespace=ns]
#     [--cidr=10.88.0.0/24] [--egress-target=1.1.1.1] [--timeout=600]
#
# Run on the node. Needs KAIRON_KUBE_* (lib/lab.sh), FluxVM with host.vnet
# at FLUXVM_URL (FLUXVM_TOKEN if it requires one) and kairon-node started
# with --vnet-uplink. The objects it creates are removed on exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600; CIDR="10.88.0.0/24"; EGRESS="1.1.1.1"
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --cidr=*) CIDR="${arg#*=}" ;;
    --egress-target=*) EGRESS="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" ]] || { echo "usage: $0 --node=NAME --image=PATH" >&2; exit 1; }

read -r GW IP1 IP2 < <(python3 - "$CIDR" <<'EOF'
import ipaddress, sys
n = ipaddress.ip_network(sys.argv[1])
print(n[1], n[10], n[11])
EOF
)

STAMP="$(date -u +%m%d%H%M%S)"
VA="vnet-lab-a-$STAMP"; VB="vnet-lab-b-$STAMP"
A1="vnet-lab-a1-$STAMP"; A2="vnet-lab-a2-$STAMP"; B1="vnet-lab-b1-$STAMP"
VNETS=(); MACHINES=()
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

vnet_path() { echo "$LAB_API/namespaces/$NAMESPACE/virtualnetworks/$1"; }
vnet_field() { kube_curl GET "$(vnet_path "$1")" | json_eval "$2"; }

cleanup() {
  local x
  for x in "${MACHINES[@]}"; do kube_curl DELETE "$(machine_path "$NAMESPACE" "$x")" >/dev/null 2>&1 || true; done
  for x in "${VNETS[@]}"; do kube_curl DELETE "$(vnet_path "$x")" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}

caps="$(flux /v1/runtime/capabilities 2>/dev/null || true)"
if [[ "$(json_eval '((o.get("host") or {}).get("vnet"))' <<<"$caps" 2>/dev/null)" != True ]]; then
  echo "FluxVM at $FLUXVM_URL does not report host.vnet" >&2
  exit 2
fi

create_vnet() {
  local body
  body="$(python3 -c 'import json,sys; n,c,s=sys.argv[1:4]; print(json.dumps({"apiVersion":"kairon.zyvor.dev/v1","kind":"VirtualNetwork","metadata":{"name":n,"labels":{"kairon.zyvor.dev/lab":"virtual-network"}},"spec":{"cidr":c,"egressSNAT":s=="1"}}))' "$1" "$CIDR" "$2")"
  kube_curl POST "$LAB_API/namespaces/$NAMESPACE/virtualnetworks" "$body" >/dev/null
  VNETS+=("$1")
}

# The probe loop each guest runs: every 5s, one line per target on the
# serial console, "VNETLAB <check> <target> ok|fail".
probe_script() {
  cat <<EOF
#!/bin/sh
end=\$((\$(date +%s) + $TIMEOUT))
while [ \$(date +%s) -lt \$end ]; do
  for t in $1; do
    if ping -c1 -W1 "\$t" >/dev/null 2>&1; then r=ok; else r=fail; fi
    echo "VNETLAB ping \$t \$r" >/dev/ttyS0
  done
  for t in $2; do
    if curl -s -o /dev/null -m 3 "http://\$t/"; then r=ok; else r=fail; fi
    echo "VNETLAB http \$t \$r" >/dev/ttyS0
  done
  sleep 5
done
EOF
}

# create_machine NAME VNET IP PING_TARGETS HTTP_TARGETS
create_machine() {
  local body
  body="$(python3 - "$1" "$2" "$3" "$NODE" "$IMAGE" "$(probe_script "$4" "$5")" <<'EOF'
import json, sys
name, vnet, ip, node, image, probe = sys.argv[1:7]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "network": {"virtualNetwork": {"name": vnet, "ip": ip}},
    "cloudInit": {
        "writeFiles": [{"path": "/usr/local/bin/vnetlab-probe", "content": probe, "permissions": "0755"}],
        "runCmd": ["systemd-run --unit vnetlab-probe /usr/local/bin/vnetlab-probe"],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "virtual-network"}}, "spec": spec}))
EOF
)"
  kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null
  MACHINES+=("$1")
}

console() {
  local rid
  rid="$(machine_field "$NAMESPACE" "$1" '(o.get("status") or {}).get("runtimeID") or ""' 2>/dev/null)"
  [[ -n "$rid" ]] || return 1
  flux "/v1/vms/$rid/logs?lines=5000" 2>/dev/null | tr -d '\r'
}
# probes NAME KIND TARGET -- "ok fail" counts seen so far.
probes() {
  local log
  log="$(console "$1" || true)"
  printf '%s %s\n' "$(grep -c "VNETLAB $2 $3 ok" <<<"$log" || true)" "$(grep -c "VNETLAB $2 $3 fail" <<<"$log" || true)"
}
probe_ok() { local ok _f; read -r ok _f < <(probes "$1" "$2" "$3"); [[ "$ok" -gt 0 ]]; }
probe_seen() { local ok f; read -r ok f < <(probes "$1" "$2" "$3"); [[ $((ok + f)) -ge "$4" ]]; }
vnet_ready() { [[ "$(vnet_field "$1" '(o.get("status") or {}).get("phase") or ""' 2>/dev/null)" == Ready ]]; }
vnets_applied() { [[ "$(flux /v1/network/vnets | json_eval "len(o.get(\"items\") or [])")" -ge "$1" ]]; }
vni_applied() { flux /v1/network/vnets | json_eval "any((i.get(\"config\") or {}).get(\"vni\") == $1 for i in o.get(\"items\") or [])"; }
vni_gone() { [[ "$(vni_applied "$1")" == False ]]; }

create_vnet "$VA" 1
create_vnet "$VB" 0
create_machine "$A1" "$VA" "$IP1" "$IP2 $GW $EGRESS" "$EGRESS"
create_machine "$A2" "$VA" "$IP2" "$IP1" ""
create_machine "$B1" "$VB" "$IP1" "$IP2 $EGRESS" ""

healthy=1
for m in "$A1" "$A2" "$B1"; do
  if ! wait_until "$TIMEOUT" "$m Running" machine_healthy "$NAMESPACE" "$m"; then
    healthy=0
    lab_log "$m not healthy: $(machine_field "$NAMESPACE" "$m" '(o.get("status") or {}).get("message") or ""' 2>/dev/null || true)"
  fi
done
VNI_A="$(vnet_field "$VA" '(o.get("status") or {}).get("vni") or 0' 2>/dev/null || echo 0)"
VNI_B="$(vnet_field "$VB" '(o.get("status") or {}).get("vni") or 0' 2>/dev/null || echo 0)"

alloc_of() { vnet_field "$1" "next((a.get(\"ip\") for a in (o.get(\"status\") or {}).get(\"allocations\") or [] if a.get(\"machine\") == \"$2\"), \"\")"; }
mstatus_ip() { machine_field "$NAMESPACE" "$1" '(((o.get("status") or {}).get("network") or {}).get("virtualNetwork") or {}).get("ip") or ""'; }
if vnet_ready "$VA" && vnet_ready "$VB" && [[ "$VNI_A" != 0 && "$VNI_B" != 0 && "$VNI_A" != "$VNI_B" ]] &&
  [[ "$(alloc_of "$VA" "$A1")" == "$IP1" && "$(alloc_of "$VA" "$A2")" == "$IP2" && "$(alloc_of "$VB" "$B1")" == "$IP1" ]] &&
  [[ "$(mstatus_ip "$A1")" == "$IP1" && "$(mstatus_ip "$B1")" == "$IP1" ]]; then
  result ipam pass "$VA vni $VNI_A and $VB vni $VNI_B, both $CIDR; a1 $IP1, a2 $IP2, b1 $IP1 (overlapping); status.network.virtualNetwork set"
else
  result ipam fail "phases $(vnet_field "$VA" '(o.get("status") or {}).get("phase")' 2>/dev/null)/$(vnet_field "$VB" '(o.get("status") or {}).get("phase")' 2>/dev/null), vnis $VNI_A/$VNI_B, message: $(vnet_field "$VA" '(o.get("status") or {}).get("message")' 2>/dev/null)"
fi

if [[ "$healthy" == 1 ]]; then
  wait_until 180 "networks applied on FluxVM" vnets_applied 2 || true
  if wait_until 300 "a1 -> a2" probe_ok "$A1" ping "$IP2" && wait_until 120 "a2 -> a1" probe_ok "$A2" ping "$IP1"; then
    result same-host pass "a1 $IP1 <-> a2 $IP2 ping on $NODE (vni $VNI_A)"
  else
    result same-host fail "a1->a2 $(probes "$A1" ping "$IP2"), a2->a1 $(probes "$A2" ping "$IP1") (ok fail)"
  fi
  if wait_until 120 "a1 -> gateway" probe_ok "$A1" ping "$GW"; then
    result gateway pass "a1 pings gateway $GW"
  else
    result gateway fail "a1->gw $(probes "$A1" ping "$GW") (ok fail)"
  fi
  if [[ "${RESULT[same-host]}" == pass ]] && wait_until 120 "b1 probed a2's address" probe_seen "$B1" ping "$IP2" 6; then
    read -r ok f < <(probes "$B1" ping "$IP2")
    if [[ "$ok" == 0 ]]; then
      result isolation pass "b1 ($IP1 on vni $VNI_B) failed all $f pings to $IP2 while a1 reached it on vni $VNI_A"
    else
      result isolation fail "b1 reached $IP2 $ok times across VNIs"
    fi
  else
    result isolation fail "same-host failed or b1 never probed (b1 $(probes "$B1" ping "$IP2"))"
  fi
  if wait_until 180 "a1 -> $EGRESS" probe_ok "$A1" ping "$EGRESS" && wait_until 60 "a1 http $EGRESS" probe_ok "$A1" http "$EGRESS"; then
    result snat pass "a1 reached $EGRESS by ping and HTTP through the node uplink"
  else
    result snat fail "a1 ping $(probes "$A1" ping "$EGRESS"), http $(probes "$A1" http "$EGRESS") (ok fail)"
  fi
  if [[ "${RESULT[snat]}" == pass ]] && wait_until 60 "b1 probed egress" probe_seen "$B1" ping "$EGRESS" 6; then
    read -r ok f < <(probes "$B1" ping "$EGRESS")
    if [[ "$ok" == 0 ]]; then
      result closed pass "b1 (no egressSNAT) failed all $f pings to $EGRESS while a1 reached it"
    else
      result closed fail "b1 reached $EGRESS $ok times without egressSNAT"
    fi
  else
    result closed fail "snat failed or b1 never probed (b1 $(probes "$B1" ping "$EGRESS"))"
  fi
fi

for m in "${MACHINES[@]}"; do kube_curl DELETE "$(machine_path "$NAMESPACE" "$m")" >/dev/null 2>&1 || true; done
if [[ "$VNI_A" != 0 && "$VNI_B" != 0 ]] && wait_until 180 "vni $VNI_A removed" vni_gone "$VNI_A" && wait_until 60 "vni $VNI_B removed" vni_gone "$VNI_B"; then
  result cleanup pass "after the Machines were deleted kairon-node removed vni $VNI_A and $VNI_B from FluxVM"
else
  result cleanup fail "FluxVM still has: $(flux /v1/network/vnets | json_eval '[(i.get("config") or {}).get("vni") for i in o.get("items") or []]' 2>/dev/null)"
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in ipam same-host gateway isolation snat closed cleanup; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Virtual network: $c | $r | $today | Node $NODE, ${NOTE[$c]:-earlier step failed} |"
done
echo "| Virtual network: cross-host | not run | — | Needs two nodes; Geneve between hosts has only run in FluxVM's netns test |"
echo "| Virtual network: floating-ip | not run | — | Needs a public address routed to the node |"
echo "| Virtual network: bgp | not run | — | Needs a BGP router; the speaker is unit-tested against a fake peer |"
exit "$rc"
