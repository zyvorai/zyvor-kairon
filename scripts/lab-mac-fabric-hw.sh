#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Mac Fabric on real Macs: the four rows scripts/lab-mac-fabric.sh can only
# print as "not run" (docs/COMPATIBILITY.md#mac-fabric). Run it from any
# workstation with kubectl for the cluster and SSH to every Mac; each Mac
# already runs FluxVM (vz) and kairon-node (scripts/macos-node.sh). Setup:
# docs/runbook-multi-mac-fabric.md.
#
#   discovery  each Mac's Node is Ready with backend.vz, chip, chip
#              generation and macOS labels that match what the Mac itself
#              reports over SSH (sysctl, sw_vers); with --fabric, the
#              thunderbolt / thunderbolt-fabric labels and the Thunderbolt
#              Bridge address in the Node annotations
#   boot       one vz Machine pinned to each Mac reaches Running with a guest
#              IP and answers SSH (through the Mac as a jump host)
#   network    a MacNetwork (over Thunderbolt with --fabric) lists every Mac
#              as a member; each guest pings every other guest's MacNetwork
#              address, and with --fabric the Thunderbolt Bridge byte
#              counters on the Macs move by at least the ping payload
#   nested     on a Mac labelled vz.rosetta and vz.nested-virtualization, a
#              Machine with spec.apple.rosetta and nestedVirtualization has
#              /dev/kvm and Rosetta (binfmt entry or the rosetta virtiofs
#              share) in the guest
#
# Usage:
#   lab-mac-fabric-hw.sh --mac=NODE=USER@HOST --mac=NODE=USER@HOST [...]
#                        --image=PATH [--fabric=NAME] [--guest-user=kairon]
#                        [--timeout=600] [--keep] [--write-compat]
#
#   --mac      a Mac's Kubernetes node name and an SSH target for it
#              (BatchMode; it is also the jump host to its guests). Two or more.
#   --image    raw arm64 Linux disk with cloud-init, at the same path on every
#              Mac (under kairon-node's image root)
#   --fabric   the --thunderbolt-fabric / KAIRON_THUNDERBOLT_FABRIC name the
#              Macs were started with; without it the network row runs over
#              the LAN and the Thunderbolt row is recorded "not run"
#   --write-compat  rewrite the four rows in docs/COMPATIBILITY.md
#
# Needs kubectl, ssh, ssh-keygen, python3. Creates namespace macfab-<stamp>
# and deletes it on exit (unless --keep).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

MACS=(); SSHS=(); IMAGE=""; FABRIC=""; GUSER=kairon; TIMEOUT=600; KEEP=0; WRITE=0
for arg in "$@"; do
  case "$arg" in
    --mac=*) v="${arg#*=}"; [[ "$v" == *=* ]] || { echo "--mac=NODE=USER@HOST" >&2; exit 2; }
             MACS+=("${v%%=*}"); SSHS+=("${v#*=}") ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --fabric=*) FABRIC="${arg#*=}" ;;
    --guest-user=*) GUSER="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --keep) KEEP=1 ;;
    --write-compat) WRITE=1 ;;
    -h|--help) sed -n '5,46p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag $arg" >&2; exit 2 ;;
  esac
done
(( ${#MACS[@]} >= 2 )) || { echo "need at least two --mac=NODE=USER@HOST" >&2; exit 2; }
[[ -n "$IMAGE" ]] || { echo "need --image=PATH (same path on every Mac)" >&2; exit 2; }
for c in kubectl ssh ssh-keygen python3; do command -v "$c" >/dev/null || { echo "$c not found" >&2; exit 2; }; done

T_DISC="Mac Fabric: capability and Thunderbolt discovery on real Macs"
T_BOOT="Mac Fabric: vz Machines booting on several Macs"
T_NET="Mac Fabric: MacNetwork traffic between Macs over Thunderbolt"
T_NESTED="Mac Fabric: Rosetta and nested virtualization inside a guest"

STAMP="$(date -u +%m%d%H%M%S)"; NS="macfab-$STAMP"; NET=fab
WORK="$(mktemp -d)"; KEY="$WORK/key"
RESULTS=()
log() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }
join() { local IFS=$'\x1f' s; s="$*"; echo "${s//$'\x1f'/; }"; }
record() { RESULTS+=("$1"$'\t'"$2"$'\t'"$3"); log "$1: $2 -- $3"; }
cleanup() {
  if (( KEEP )); then log "kept namespace $NS and $WORK"; return; fi
  kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

mssh() { ssh -o BatchMode=yes -o ConnectTimeout=15 "$@"; }
# gssh MAC_SSH GUEST_IP CMD -- SSH into a guest through its Mac.
gssh() {
  ssh -o BatchMode=yes -o ConnectTimeout=20 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR -i "$KEY" -J "$1" "$GUSER@$2" "$3"
}
nodejson() { kubectl get node "$1" -o json; }
jq_py() { python3 -c "import json,sys; o=json.load(sys.stdin); print($1)"; }
label() { nodejson "$1" | jq_py "(o['metadata'].get('labels') or {}).get('$2','')"; }
annot() { nodejson "$1" | jq_py "(o['metadata'].get('annotations') or {}).get('$2','')"; }
mfield() { kubectl -n "$NS" get machine "$1" -o json 2>/dev/null | jq_py "$2" 2>/dev/null || true; }

# --- discovery -----------------------------------------------------------------
notes=(); ok=1
for i in "${!MACS[@]}"; do
  n="${MACS[$i]}"; s="${SSHS[$i]}"
  if ! kubectl get node "$n" >/dev/null 2>&1; then ok=0; notes+=("$n: no Node"); continue; fi
  ready="$(nodejson "$n" | jq_py "[c['status'] for c in o['status'].get('conditions',[]) if c['type']=='Ready'][0] if o['status'].get('conditions') else ''")"
  vz="$(label "$n" kairon.zyvor.dev/backend.vz)"; chip="$(label "$n" kairon.zyvor.dev/apple-chip)"
  gen="$(label "$n" kairon.zyvor.dev/apple-chip-generation)"; mos="$(label "$n" kairon.zyvor.dev/macos-version)"
  brand="$(mssh "$s" 'sysctl -n machdep.cpu.brand_string' 2>/dev/null || true)"
  real_mos="$(mssh "$s" 'sw_vers -productVersion' 2>/dev/null | cut -d. -f1 || true)"
  want_gen="$(sed -nE 's/^Apple M([0-9]+).*/\1/p' <<<"$brand")"
  line="$n: Ready=$ready vz=$vz chip=$chip gen=$gen macOS=$mos (Mac says '$brand', macOS $real_mos)"
  if [[ "$ready" != True || "$vz" != true || -z "$chip" || "$gen" != "$want_gen" || "$mos" != "$real_mos" ]]; then ok=0; line+=" MISMATCH"; fi
  if [[ -n "$FABRIC" ]]; then
    tb="$(label "$n" kairon.zyvor.dev/thunderbolt)"; tbf="$(label "$n" kairon.zyvor.dev/thunderbolt-fabric)"
    ifc="$(annot "$n" kairon.zyvor.dev/thunderbolt-interface)"; addrs="$(annot "$n" kairon.zyvor.dev/thunderbolt-addresses)"
    real_tb="$(mssh "$s" "ipconfig getifaddr ${ifc:-bridge0}" 2>/dev/null || true)"
    line+="; thunderbolt=$tb fabric=$tbf ${ifc:-?}=$addrs (Mac says $real_tb)"
    if [[ "$tb" != true || "$tbf" != "$FABRIC" || -z "$real_tb" || "$addrs" != *"$real_tb"* ]]; then ok=0; line+=" MISMATCH"; fi
  fi
  notes+=("$line")
done
d_res=fail; (( ok )) && d_res=pass
record "$T_DISC" "$d_res" "$(join "${notes[@]}")"
[[ "$d_res" == pass ]] || log "discovery failed; continuing so the other rows say why"

# --- boot + network --------------------------------------------------------------
kubectl create namespace "$NS" >/dev/null
ssh-keygen -q -t ed25519 -N "" -f "$KEY"; PUB="$(cat "$KEY.pub")"
fabric_spec=""; [[ -n "$FABRIC" ]] && fabric_spec="fabric: $FABRIC"
kubectl apply -f - >/dev/null <<EOM
apiVersion: kairon.zyvor.dev/v1
kind: MacNetwork
metadata: {name: $NET, namespace: $NS}
spec:
  $fabric_spec
  mtu: 1400
EOM

# machine NAME NODE [EXTRA_APPLE_JSON]
machine() {
  local apple='{"networks":["'"$NET"'"]}'
  [[ -n "${3:-}" ]] && apple="$3"
  kubectl apply -f - >/dev/null <<EOM
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: $1, namespace: $NS}
spec:
  image: {path: "$IMAGE"}
  resources: {cpu: "2", memory: 2Gi}
  runtime: {backend: vz}
  powerState: Running
  cloudInit: {hostname: $1, user: $GUSER, sshAuthorizedKeys: ["$PUB"]}
  apple: $apple
  placement:
    nodeSelector: {kubernetes.io/hostname: "$2"}
EOM
}
wait_running() { # NAME -> guest IP
  local deadline=$(( $(date +%s) + TIMEOUT )) ip
  while (( $(date +%s) < deadline )); do
    ip="$(mfield "$1" "(o.get('status') or {}).get('guestIP') or '' if (o.get('status') or {}).get('phase')=='Running' else ''")"
    [[ -n "$ip" ]] && { echo "$ip"; return 0; }
    sleep 5
  done
  return 1
}

GUESTS=(); IPS=()
for i in "${!MACS[@]}"; do GUESTS+=("mfab-$i"); machine "mfab-$i" "${MACS[$i]}"; done
notes=(); ok=1
for i in "${!MACS[@]}"; do
  g="${GUESTS[$i]}"
  if ip="$(wait_running "$g")"; then
    IPS+=("$ip")
    arch="$(gssh "${SSHS[$i]}" "$ip" 'uname -m' 2>/dev/null || true)"
    node="$(mfield "$g" "(o.get('status') or {}).get('nodeName') or ''")"
    if [[ "$arch" == aarch64 && "$node" == "${MACS[$i]}" ]]; then notes+=("$g on $node: $ip, SSH uname -m=$arch")
    else ok=0; notes+=("$g on '$node': $ip, SSH gave '$arch'"); fi
  else
    IPS+=(""); ok=0
    notes+=("$g on ${MACS[$i]}: not Running with a guest IP in ${TIMEOUT}s (phase $(mfield "$g" "(o.get('status') or {}).get('phase')"), $(mfield "$g" "[c.get('message') for c in (o.get('status') or {}).get('conditions',[]) if c.get('status')!='True'][:1]"))")
  fi
done
b_res=fail; (( ok )) && b_res=pass
record "$T_BOOT" "$b_res" "${#MACS[@]} Macs: $(join "${notes[@]}")"

# MacNetwork: members, then guest-to-guest pings over the overlay.
netjson="$(kubectl -n "$NS" get macnetwork "$NET" -o json)"
subnet="$(jq_py "(o.get('status') or {}).get('subnetIndex','')" <<<"$netjson")"
port="$(jq_py "(o.get('status') or {}).get('port','')" <<<"$netjson")"
members="$(jq_py "' '.join(sorted(m['node']+'@'+m.get('address','') for m in (o.get('status') or {}).get('members') or []))" <<<"$netjson")"
notes=("MacNetwork $NET 10.89.${subnet}.0/24 udp/$port, members: $members"); ok=1
for n in "${MACS[@]}"; do [[ " $members" == *" $n@"* ]] || { ok=0; notes+=("$n not a member"); }; done
ovl=()
for i in "${!MACS[@]}"; do
  a=""
  [[ -n "${IPS[$i]}" ]] && a="$(gssh "${SSHS[$i]}" "${IPS[$i]}" "ip -4 -o addr | awk '{print \$4}' | cut -d/ -f1 | grep '^10\\.89\\.${subnet}\\.' | head -1" 2>/dev/null || true)"
  ovl+=("$a"); [[ -n "$a" ]] || { ok=0; notes+=("${GUESTS[$i]} has no 10.89.${subnet}.x address"); }
done
# Thunderbolt Bridge byte counters on each Mac, before and after the pings.
tb_bytes() { # SSH IFACE -> Ibytes+Obytes
  mssh "$1" "netstat -ibn -I $2" 2>/dev/null | awk 'NR==1{for(i=1;i<=NF;i++){if($i=="Ibytes")ib=i;if($i=="Obytes")ob=i}} NR==2{print $ib+$ob}'
}
before=()
if [[ -n "$FABRIC" ]]; then
  for i in "${!MACS[@]}"; do
    ifc="$(annot "${MACS[$i]}" kairon.zyvor.dev/thunderbolt-interface)"; before[i]="$(tb_bytes "${SSHS[$i]}" "${ifc:-bridge0}")"
  done
fi
COUNT=200; SIZE=1200
if (( ok )); then
  for i in "${!MACS[@]}"; do
    for j in "${!MACS[@]}"; do
      (( i == j )) && continue
      out="$(gssh "${SSHS[$i]}" "${IPS[$i]}" "ping -q -c $COUNT -i 0.05 -s $SIZE -W 2 ${ovl[$j]}" 2>&1 || true)"
      loss="$(sed -nE 's/.* ([0-9.]+)% packet loss.*/\1/p' <<<"$out")"
      rtt="$(sed -nE 's#.*= [0-9.]+/([0-9.]+)/.*#\1#p' <<<"$out")"
      notes+=("${GUESTS[$i]} -> ${GUESTS[$j]} ${ovl[$j]}: loss ${loss:-?}%, avg ${rtt:-?} ms")
      [[ "${loss:-100}" == 0 || "${loss:-100}" == 0.0 ]] || ok=0
    done
  done
fi
if [[ -n "$FABRIC" ]]; then
  min=$(( COUNT * SIZE ))
  for i in "${!MACS[@]}"; do
    ifc="$(annot "${MACS[$i]}" kairon.zyvor.dev/thunderbolt-interface)"; after="$(tb_bytes "${SSHS[$i]}" "${ifc:-bridge0}")"
    delta=$(( ${after:-0} - ${before[$i]:-0} ))
    notes+=("${MACS[$i]} ${ifc:-bridge0} moved ${delta} bytes")
    (( delta >= min )) || { ok=0; notes+=("${MACS[$i]}: under ${min} bytes, so the overlay did not use Thunderbolt"); }
  done
  n_res=fail; (( ok )) && n_res=pass
  record "$T_NET" "$n_res" "$(join "${notes[@]}")"
else
  r="ok"; (( ok )) || r="FAILED"
  record "$T_NET" "not run" "no --fabric, so the overlay ran over the LAN ($r): $(join "${notes[@]}")"
fi

# --- Rosetta + nested virtualization ---------------------------------------------------
pick=""; pick_ssh=""
for i in "${!MACS[@]}"; do
  if [[ "$(label "${MACS[$i]}" kairon.zyvor.dev/vz.rosetta)" == true && "$(label "${MACS[$i]}" kairon.zyvor.dev/vz.nested-virtualization)" == true ]]; then
    pick="${MACS[$i]}"; pick_ssh="${SSHS[$i]}"; break
  fi
done
if [[ -z "$pick" ]]; then
  record "$T_NESTED" "not run" "no Mac labelled vz.rosetta=true and vz.nested-virtualization=true (needs M3 or later, macOS 15 or later, Rosetta installed)"
else
  machine mfab-nested "$pick" '{"rosetta":true,"nestedVirtualization":true}'
  if ip="$(wait_running mfab-nested)"; then
    kvm="$(gssh "$pick_ssh" "$ip" 'test -c /dev/kvm && echo yes || echo no' 2>/dev/null || echo ssh-failed)"
    ros="$(gssh "$pick_ssh" "$ip" 'if ls /proc/sys/fs/binfmt_misc 2>/dev/null | grep -qi rosetta; then echo binfmt; elif grep -qi rosetta /proc/mounts; then echo share-mounted; elif sudo -n mkdir -p /mnt/rosetta && sudo -n mount -t virtiofs rosetta /mnt/rosetta 2>/dev/null && test -x /mnt/rosetta/rosetta; then echo share; else echo no; fi' 2>/dev/null || echo ssh-failed)"
    note="mfab-nested on $pick ($(label "$pick" kairon.zyvor.dev/apple-chip), macOS $(label "$pick" kairon.zyvor.dev/macos-version)): /dev/kvm $kvm, Rosetta $ros"
    if [[ "$kvm" == yes && "$ros" != no && "$ros" != ssh-failed ]]; then record "$T_NESTED" pass "$note"; else record "$T_NESTED" fail "$note"; fi
  else
    record "$T_NESTED" fail "mfab-nested on $pick: not Running with a guest IP in ${TIMEOUT}s"
  fi
fi

DATE="$(date -u +%Y-%m-%d)"
echo
echo "| Case | Last result | Date | Notes |"
echo "|---|---|---|---|"
FAILED=0
for r in "${RESULTS[@]}"; do
  IFS=$'\t' read -r t res n <<<"$r"
  echo "| $t | $res | $DATE | ${n//|/\\|} |"
  [[ "$res" == fail ]] && FAILED=$((FAILED + 1))
done
if (( WRITE )); then
  printf '%s\n' "${RESULTS[@]}" | python3 "$ROOT/scripts/lib/write_compat.py" "$ROOT/docs/COMPATIBILITY.md" "$DATE"
  echo "updated docs/COMPATIBILITY.md"
fi
(( FAILED == 0 ))
