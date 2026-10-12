#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Mac Fabric drill without Macs (see docs/guides/mac-fabric.md). Three Mac
# Nodes are created through the Kubernetes API with the labels, annotations,
# vm-only taint and Ready heartbeat kairon-node publishes on a Mac:
#
#   mac-a  M4 Max, macOS 26, Rosetta, nested virtualization, Thunderbolt
#          fabric "lab" (169.254.77.1)
#   mac-b  M3, macOS 15, Rosetta, nested virtualization, fabric "lab"
#          (169.254.77.2)
#   mac-c  M1, macOS 14, no Rosetta, no Thunderbolt
#
# Nothing boots: no agent runs for these Nodes, so the cases check what
# kairon-controller and kairon-node's overlay writer decide:
#
#   capability  vz Machines with spec.apple minimums land on a Mac that
#               meets them, and stay Pending with the reason otherwise
#   isolation   the API rejects spec.apple on the qemu backend; a vz Machine
#               pinned to the Linux node and a qemu Machine pinned to a Mac
#               stay Pending
#   network     MacNetworks get a subnet, port and per-Mac host slices: a
#               fabric network over the Thunderbolt addresses of the fabric's
#               Macs, a plain one over every Mac's InternalIP
#   membership  a Machine on a MacNetwork lands on a member; pinned to a
#               non-member, or naming a missing network, it stays Pending
#   overlay     macnode.OverlaySync (what kairon-node runs on each Mac)
#               writes each Mac's overlay.json and key files: matching keys,
#               the other members as peers, mode 0600, entries it does not
#               own kept
#   rotate      raising spec.keyEpoch puts the new key first and keeps the
#               previous one
#   leave       a Mac leaving the fabric drops out of the network, the
#               others keep their slices, its overlay entry and key go
#   cli         `kaironctl mac nodes` and `kaironctl mac network`
#
# Booting on real Macs, Rosetta and KVM in the guest, the overlay carrying
# traffic between Macs, and discovery on real hardware are printed as
# "not run".
#
# Usage: lab-mac-fabric.sh --node=LINUX_NODE [--timeout=120] [--keep]
# Needs KAIRON_KUBE_* (lib/lab.sh) with rights to create Nodes, Go to build
# the overlay helper, and KAIRONCTL. Everything is removed on exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

LINUX_NODE=""; TIMEOUT=120; KEEP=0
for arg in "$@"; do
  case "$arg" in
    --node=*) LINUX_NODE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --keep) KEEP=1 ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$LINUX_NODE" ]] || { echo "usage: $0 --node=LINUX_NODE" >&2; exit 1; }

STAMP="$(date -u +%m%d%H%M%S)"
NS="maclab-$STAMP"
MAC_A="maclab-$STAMP-a"; MAC_B="maclab-$STAMP-b"; MAC_C="maclab-$STAMP-c"
MACS=("$MAC_A" "$MAC_B" "$MAC_C")
WORK="$(mktemp -d)"
HELPER_DIR="$LAB_REPO_ROOT/internal/macnode/labsync"
SECRET_FILE="$WORK/fabric-secret"
HB_PID=""
RESULTS=()

join_notes() { local IFS=$'\x1f' s; s="${notes[*]}"; echo "${s//$'\x1f'/; }"; }

record() {
  RESULTS+=("| $1 | $2 | $3 |")
  lab_log "$1: $2 -- $3"
}

cleanup() {
  [[ -n "$HB_PID" ]] && kill "$HB_PID" 2>/dev/null || true
  rm -rf "$HELPER_DIR"
  if [[ "$KEEP" == 1 ]]; then
    lab_log "--keep: left namespace $NS, Nodes ${MACS[*]} and $WORK"
    return
  fi
  for r in machines macnetworks; do
    for n in $(kube_curl GET "$LAB_API/namespaces/$NS/$r" 2>/dev/null | json_eval '" ".join(i["metadata"]["name"] for i in o["items"])' 2>/dev/null); do
      kube_curl DELETE "$LAB_API/namespaces/$NS/$r/$n" >/dev/null 2>&1 || true
    done
  done
  kube_curl DELETE "api/v1/namespaces/$NS" >/dev/null 2>&1 || true
  for n in "${MACS[@]}"; do kube_curl DELETE "api/v1/nodes/$n" >/dev/null 2>&1 || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- simulated Macs ---------------------------------------------------------
# mac_node NAME CHIP_LABEL GEN MACOS ROSETTA NESTED MODEL CORES TB_ADDR FABRIC
mac_node() {
  python3 - "$@" "$STAMP" <<'PY'
import json, sys
name, chip, gen, macos, rosetta, nested, model, cores, tb, fabric, stamp = sys.argv[1:]
labels = {
    "kubernetes.io/hostname": name, "kubernetes.io/arch": "arm64", "kubernetes.io/os": "darwin",
    "kairon.zyvor.dev/capable": "true", "kairon.zyvor.dev/backend.vz": "true",
    "kairon.zyvor.dev/apple-chip": chip, "kairon.zyvor.dev/apple-chip-generation": gen,
    "kairon.zyvor.dev/macos-version": macos, "kairon.zyvor.dev/mac-lab": stamp,
}
if rosetta == "true":
    labels["kairon.zyvor.dev/vz.rosetta"] = "true"
if nested == "true":
    labels["kairon.zyvor.dev/vz.nested-virtualization"] = "true"
ann = {"kairon.zyvor.dev/apple-model": model, "kairon.zyvor.dev/apple-cores": cores}
if tb:
    labels["kairon.zyvor.dev/thunderbolt"] = "true"
    labels["kairon.zyvor.dev/thunderbolt-fabric"] = fabric
    ann["kairon.zyvor.dev/thunderbolt-interface"] = "bridge0"
    ann["kairon.zyvor.dev/thunderbolt-addresses"] = tb
print(json.dumps({"apiVersion": "v1", "kind": "Node",
    "metadata": {"name": name, "labels": labels, "annotations": ann},
    "spec": {"taints": [{"key": "kairon.zyvor.dev/vm-only", "value": "true", "effect": "NoSchedule"}]}}))
PY
}

# mac_status IP -- the Ready heartbeat kairon-node sends (macnode.BuildStatus).
mac_status() {
  local t; t="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '{"status":{"conditions":[{"type":"Ready","status":"True","reason":"KaironNodeReady","message":"lab-mac-fabric.sh","lastHeartbeatTime":"%s","lastTransitionTime":"%s"}],"addresses":[{"type":"InternalIP","address":"%s"}],"capacity":{"cpu":"8","memory":"16Gi","pods":"0"},"allocatable":{"cpu":"8","memory":"16Gi","pods":"0"}}}' "$t" "$t" "$1"
}

declare -A MAC_IP=(["$MAC_A"]=10.250.77.1 ["$MAC_B"]=10.250.77.2 ["$MAC_C"]=10.250.77.3)
heartbeat() {
  while :; do
    for n in "${MACS[@]}"; do
      kube_curl PATCH "api/v1/nodes/$n/status" "$(mac_status "${MAC_IP[$n]}")" application/merge-patch+json >/dev/null 2>&1 || true
    done
    sleep 10
  done
}

# machine_body NAME SPEC_PATCH_JSON -- a vz Machine on this run's Macs, with the patch applied.
machine_body() {
  python3 - "$NS" "$1" "$2" "$STAMP" <<'PY'
import json, sys
ns, name, patch, stamp = sys.argv[1], sys.argv[2], json.loads(sys.argv[3]), sys.argv[4]
spec = {"image": {"path": "/var/lib/fluxvm/images/none.raw"}, "resources": {"cpu": "1", "memory": "512Mi"},
        "runtime": {"backend": "vz"}, "powerState": "Running", "placement": {"nodeSelector": {}}}
placement = patch.pop("placement", {})
spec.update(patch)
spec["placement"].update(placement)
if "kubernetes.io/hostname" not in spec["placement"]["nodeSelector"]:
    spec["placement"]["nodeSelector"]["kairon.zyvor.dev/mac-lab"] = stamp
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "namespace": ns}, "spec": spec}))
PY
}
# create_machine NAME SPEC_PATCH_JSON -- POSTs it; prints the HTTP code.
create_machine() {
  local f="$WORK/machine.json" tls=(-k)
  [[ "${KAIRON_KUBE_INSECURE:-}" == "true" ]] || tls=(--cacert "$KAIRON_KUBE_CA")
  machine_body "$1" "$2" >"$f"
  curl -sS -o "$WORK/create.out" -w '%{http_code}' -X POST -H "Authorization: Bearer $KAIRON_KUBE_TOKEN" \
    -H "Content-Type: application/json" --data @"$f" "${tls[@]}" \
    "$KAIRON_KUBE_URL/$LAB_API/namespaces/$NS/machines"
}

placed_on() { machine_node "$NS" "$1"; }
pending_msg() { machine_field "$NS" "$1" '(o.get("status") or {}).get("message") or ""'; }
is_placed() { [[ -n "$(placed_on "$1" 2>/dev/null)" ]]; }
is_pending_with() { [[ "$(machine_phase "$NS" "$1" 2>/dev/null)" == Pending && "$(pending_msg "$1" 2>/dev/null)" == *"$2"* ]]; }

net_path() { echo "$LAB_API/namespaces/$NS/macnetworks/$1"; }
net_field() { kube_curl GET "$(net_path "$1")" | json_eval "$2"; }
net_ready() { [[ "$(net_field "$1" '(o.get("status") or {}).get("phase") or ""' 2>/dev/null)" == Ready ]]; }

# --- setup ------------------------------------------------------------------
lab_log "setup: namespace $NS, Mac Nodes ${MACS[*]}"
kube_curl POST api/v1/namespaces "{\"apiVersion\":\"v1\",\"kind\":\"Namespace\",\"metadata\":{\"name\":\"$NS\"}}" >/dev/null
kube_curl POST api/v1/nodes "$(mac_node "$MAC_A" m4-max 4 26 true true Mac16,9 12P+4E 169.254.77.1 lab)" >/dev/null
kube_curl POST api/v1/nodes "$(mac_node "$MAC_B" m3 3 15 true true Mac15,12 4P+4E 169.254.77.2 lab)" >/dev/null
kube_curl POST api/v1/nodes "$(mac_node "$MAC_C" m1 1 14 false false Macmini9,1 4P+4E "" "")" >/dev/null
heartbeat &
HB_PID=$!
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$SECRET_FILE"

mkdir -p "$HELPER_DIR"
cat >"$HELPER_DIR/main.go" <<'GO'
// Command labsync runs one Mac's MacNetwork overlay sync, for scripts/lab-mac-fabric.sh.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/macnode"
)

func main() {
	kc, err := kube.FromEnvironment()
	if err == nil {
		var secret []byte
		if secret, err = os.ReadFile(os.Args[2]); err == nil {
			err = macnode.OverlaySync{API: kc, Node: os.Args[1], Path: os.Args[3], Secret: []byte(strings.TrimSpace(string(secret)))}.Sync(context.Background())
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
GO
(cd "$LAB_REPO_ROOT" && go build -o "$WORK/labsync" ./internal/macnode/labsync)
rm -rf "$HELPER_DIR"
for n in "${MACS[@]}"; do
  mkdir -p "$WORK/$n"
  # An entry owned by someone else (a FluxVM fleet central) must survive.
  printf '{"networks":{"fleet-net":{"subnet":12,"hosts":[2,9],"listen":"0.0.0.0:47012","peers":[],"key_file":"%s"}}}\n' \
    "$WORK/$n/overlay-keys/fleet-net.key" >"$WORK/$n/overlay.json"
  mkdir -p "$WORK/$n/overlay-keys"; echo "0 fleet" >"$WORK/$n/overlay-keys/fleet-net.key"
done
sync_mac() { "$WORK/labsync" "$1" "$SECRET_FILE" "$WORK/$1/overlay.json"; }

mac_ready() {
  local n
  for n in "${MACS[@]}"; do
    [[ "$(kube_curl GET "api/v1/nodes/$n" | json_eval '[c["status"] for c in o["status"].get("conditions", []) if c["type"] == "Ready"]')" == "['True']" ]] || return 1
  done
}
wait_until 60 "Mac Nodes Ready" mac_ready

# --- capability -------------------------------------------------------------
create_machine cap-m4 '{"apple":{"minChipGeneration":4}}' >/dev/null
create_machine cap-nested '{"apple":{"minChipGeneration":3,"minMacOSVersion":15,"nestedVirtualization":true}}' >/dev/null
create_machine cap-m5 '{"apple":{"minChipGeneration":5}}' >/dev/null
create_machine cap-rosetta-c "{\"apple\":{\"rosetta\":true},\"placement\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$MAC_C\"}}}" >/dev/null
ok=1; notes=()
wait_until "$TIMEOUT" "cap-m4 placed" is_placed cap-m4 || true
wait_until "$TIMEOUT" "cap-nested placed" is_placed cap-nested || true
n4="$(placed_on cap-m4)"; nn="$(placed_on cap-nested)"
[[ "$n4" == "$MAC_A" ]] || ok=0; notes+=("minChipGeneration 4 -> ${n4:-none}")
[[ "$nn" == "$MAC_A" || "$nn" == "$MAC_B" ]] || ok=0; notes+=("M3+, macOS 15+, nested -> ${nn:-none}")
if wait_until "$TIMEOUT" "cap-m5 Pending" is_pending_with cap-m5 "minChipGeneration 5"; then
  notes+=("minChipGeneration 5 Pending, no Mac meets it")
else ok=0; notes+=("cap-m5: $(machine_phase "$NS" cap-m5) on $(placed_on cap-m5)"); fi
if wait_until "$TIMEOUT" "cap-rosetta-c Pending" is_pending_with cap-rosetta-c "no Rosetta"; then
  notes+=("rosetta pinned to the M1 Pending")
else ok=0; notes+=("cap-rosetta-c: $(machine_phase "$NS" cap-rosetta-c) $(pending_msg cap-rosetta-c)"); fi
record capability "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- isolation --------------------------------------------------------------
ok=1; notes=()
code="$(create_machine iso-qemu-apple '{"runtime":{"backend":"qemu"},"apple":{"rosetta":true}}')"
if [[ "$code" == 422 ]] && grep -q "spec.apple needs the vz backend" "$WORK/create.out"; then
  notes+=("API rejected spec.apple on qemu (422)")
else ok=0; notes+=("spec.apple on qemu: HTTP $code"); fi
create_machine iso-vz-linux "{\"placement\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$LINUX_NODE\"}}}" >/dev/null
create_machine iso-qemu-mac "{\"runtime\":{\"backend\":\"qemu\"},\"placement\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$MAC_A\"},\"tolerations\":[{\"key\":\"kairon.zyvor.dev/vm-only\",\"operator\":\"Exists\",\"effect\":\"NoSchedule\"}]}}" >/dev/null
create_machine iso-qemu-taint "{\"runtime\":{\"backend\":\"qemu\"},\"placement\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$MAC_A\"}}}" >/dev/null
for c in "iso-vz-linux|the vz backend needs a Mac" "iso-qemu-mac|a Mac runs only the vz backend" "iso-qemu-taint|kairon.zyvor.dev/vm-only"; do
  m="${c%%|*}"; want="${c#*|}"
  if wait_until "$TIMEOUT" "$m Pending" is_pending_with "$m" "$want"; then notes+=("$m Pending: $want")
  else ok=0; notes+=("$m: $(machine_phase "$NS" "$m") on $(placed_on "$m") $(pending_msg "$m")"); fi
done
record isolation "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- network ----------------------------------------------------------------
kube_curl POST "$LAB_API/namespaces/$NS/macnetworks" "{\"apiVersion\":\"kairon.zyvor.dev/v1\",\"kind\":\"MacNetwork\",\"metadata\":{\"name\":\"fab\",\"namespace\":\"$NS\"},\"spec\":{\"fabric\":\"lab\",\"mtu\":9000}}" >/dev/null
kube_curl POST "$LAB_API/namespaces/$NS/macnetworks" "{\"apiVersion\":\"kairon.zyvor.dev/v1\",\"kind\":\"MacNetwork\",\"metadata\":{\"name\":\"lan\",\"namespace\":\"$NS\"},\"spec\":{\"hostsPerNode\":20}}" >/dev/null
ok=1; notes=()
if wait_until "$TIMEOUT" "fab Ready" net_ready fab && wait_until "$TIMEOUT" "lan Ready" net_ready lan; then
  FAB="$(kube_curl GET "$(net_path fab)")"; LAN="$(kube_curl GET "$(net_path lan)")"
  check="$(python3 - "$FAB" "$LAN" "$MAC_A" "$MAC_B" "$MAC_C" <<'PY'
import json, sys
fab, lan = json.loads(sys.argv[1])["status"], json.loads(sys.argv[2])["status"]
a, b, c = sys.argv[3:]
errs = []
fm = {m["node"]: m for m in fab.get("members", [])}
lm = {m["node"]: m for m in lan.get("members", [])}
if sorted(fm) != sorted([a, b]): errs.append("fab members %s" % sorted(fm))
if sorted(lm) != sorted([a, b, c]): errs.append("lan members %s" % sorted(lm))
if fm.get(a, {}).get("address") != "169.254.77.1" or fm.get(a, {}).get("underlay") != "thunderbolt": errs.append("fab a %s" % fm.get(a))
if lm.get(c, {}).get("address") != "10.250.77.3" or lm.get(c, {}).get("underlay") != "internal": errs.append("lan c %s" % lm.get(c))
if fab["subnetIndex"] == lan["subnetIndex"]: errs.append("same subnet")
for st in (fab, lan):
    if not (192 <= st["subnetIndex"] <= 254) or st["subnet"] != "10.89.%d.0/24" % st["subnetIndex"] or st["port"] != 48000 + st["subnetIndex"]:
        errs.append("subnet/port %s %s" % (st["subnet"], st["port"]))
if fab["mtu"] != 9000 or lan["mtu"] != 1400: errs.append("mtu %s %s" % (fab["mtu"], lan["mtu"]))
if [m["hosts"] for m in sorted(lan["members"], key=lambda m: m["node"])] != [[2, 21], [22, 41], [42, 61]]: errs.append("lan slices %s" % [m["hosts"] for m in lan["members"]])
print("; ".join(errs) if errs else "fab %s port %d over Thunderbolt: %s; lan %s port %d over InternalIP: %s" % (
    fab["subnet"], fab["port"], ", ".join("%s .%d-.%d" % (m["node"][-1], *m["hosts"]) for m in fab["members"]),
    lan["subnet"], lan["port"], ", ".join("%s .%d-.%d" % (m["node"][-1], *m["hosts"]) for m in lan["members"])))
print("OK" if not errs else "BAD")
PY
)"
  [[ "$(tail -1 <<<"$check")" == OK ]] || ok=0
  notes+=("$(head -1 <<<"$check")")
else
  ok=0; notes+=("fab $(net_field fab 'o.get("status")' 2>/dev/null); lan $(net_field lan 'o.get("status")' 2>/dev/null)")
fi
record network "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- membership -------------------------------------------------------------
create_machine net-fab '{"apple":{"networks":["fab"]}}' >/dev/null
create_machine net-fab-c "{\"apple\":{\"networks\":[\"fab\"]},\"placement\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$MAC_C\"}}}" >/dev/null
create_machine net-missing '{"apple":{"networks":["nope"]}}' >/dev/null
ok=1; notes=()
wait_until "$TIMEOUT" "net-fab placed" is_placed net-fab || true
nf="$(placed_on net-fab)"
[[ "$nf" == "$MAC_A" || "$nf" == "$MAC_B" ]] || ok=0; notes+=("on fab -> ${nf:-none}")
for c in "net-fab-c|not a member of MacNetwork fab" "net-missing|MacNetwork nope not found"; do
  m="${c%%|*}"; want="${c#*|}"
  if wait_until "$TIMEOUT" "$m Pending" is_pending_with "$m" "$want"; then notes+=("$m Pending: $want")
  else ok=0; notes+=("$m: $(machine_phase "$NS" "$m") on $(placed_on "$m") $(pending_msg "$m")"); fi
done
record membership "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- overlay ----------------------------------------------------------------
# overlay_check PHASE -- compares every Mac's overlay.json and keys with the MacNetworks' status.
overlay_check() {
  python3 - "$1" "$WORK" "$(kube_curl GET "$LAB_API/namespaces/$NS/macnetworks")" "${MACS[@]}" <<'PY'
import json, os, stat, sys
phase, work, nets = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])["items"]
macs = sys.argv[4:]
errs, keys = [], {}
for mac in macs:
    path = os.path.join(work, mac, "overlay.json")
    have = json.load(open(path))["networks"]
    if "fleet-net" not in have or not os.path.exists(os.path.join(work, mac, "overlay-keys", "fleet-net.key")):
        errs.append(mac + " lost fleet-net")
    if stat.S_IMODE(os.stat(path).st_mode) != 0o600: errs.append(mac + " overlay.json mode")
    want = set()
    for n in nets:
        st = n["status"]
        me = [m for m in st.get("members", []) if m["node"] == mac]
        name = st["name"]
        if not me:
            if name in have: errs.append("%s still has %s" % (mac, n["metadata"]["name"]))
            if os.path.exists(os.path.join(work, mac, "overlay-keys", name + ".key")): errs.append("%s still has %s key" % (mac, n["metadata"]["name"]))
            continue
        want.add(name)
        e = have.get(name)
        if not e: errs.append("%s missing %s" % (mac, n["metadata"]["name"])); continue
        peers = sorted("%s:%d" % (m["address"], st["port"]) for m in st["members"] if m["node"] != mac)
        if e["subnet"] != st["subnetIndex"] or e["hosts"] != me[0]["hosts"] or e["peers"] != peers or e["listen"] != "0.0.0.0:%d" % st["port"] or e["mtu"] != st["mtu"]:
            errs.append("%s %s entry %s" % (mac, n["metadata"]["name"], e))
        kf = e["key_file"]
        if stat.S_IMODE(os.stat(kf).st_mode) != 0o600: errs.append(kf + " mode")
        lines = open(kf).read().split("\n")[:-1]
        epochs = [int(l.split()[0]) for l in lines]
        want_epochs = [st.get("keyEpoch", 0)] + ([st["previousKeyEpoch"]] if st.get("previousKeyEpoch") is not None and st["previousKeyEpoch"] != st["keyEpoch"] else [])
        if epochs != want_epochs: errs.append("%s %s key epochs %s, want %s" % (mac, n["metadata"]["name"], epochs, want_epochs))
        keys.setdefault(name, set()).add(tuple(lines))
    for name in have:
        if name.startswith("k") and name not in want and name != "fleet-net":
            errs.append("%s has stale %s" % (mac, name))
for name, v in keys.items():
    if len(v) != 1: errs.append(name + " keys differ between Macs")
json.dump({n: sorted(v)[0] for n, v in keys.items()}, open(os.path.join(work, "keys-" + phase + ".json"), "w"))
print("; ".join(errs) if errs else "OK")
PY
}
summary_of() {
  python3 - "$WORK/$1/overlay.json" <<'PY'
import json, sys
nets = json.load(open(sys.argv[1]))["networks"]
print(", ".join("%s hosts .%d-.%d peers %s" % (n, *e["hosts"], ",".join(e["peers"]) or "none") for n, e in sorted(nets.items()) if n != "fleet-net"))
PY
}
ok=1; notes=()
for n in "${MACS[@]}"; do sync_mac "$n" || ok=0; done
res="$(overlay_check before)"
[[ "$res" == OK ]] || ok=0
notes+=("$([[ "$res" == OK ]] && echo "3 Macs agree on keys and peers, files 0600, fleet-net kept" || echo "$res"); mac-a: $(summary_of "$MAC_A"); mac-c: $(summary_of "$MAC_C")")
record overlay "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- rotate -----------------------------------------------------------------
ok=1; notes=()
kube_curl PATCH "$(net_path fab)" '{"spec":{"keyEpoch":1}}' application/merge-patch+json >/dev/null
rotated() { [[ "$(net_field fab '[(o.get("status") or {}).get("keyEpoch"), (o.get("status") or {}).get("previousKeyEpoch")]')" == "[1, 0]" ]]; }
if wait_until "$TIMEOUT" "fab key epoch 1" rotated; then
  for n in "${MACS[@]}"; do sync_mac "$n" || ok=0; done
  res="$(overlay_check after)"
  [[ "$res" == OK ]] || ok=0
  same="$(python3 - "$WORK" "$(net_field fab 'o["status"]["name"]')" <<'PY'
import json, sys
b, a = (json.load(open("%s/keys-%s.json" % (sys.argv[1], p))) for p in ("before", "after"))
n = sys.argv[2]
print("yes" if a[n][1] == b[n][0] and a[n][0] != b[n][0] and a[n][0].startswith("1 ") else "no: %s / %s" % (b[n], a[n]))
PY
)"
  [[ "$same" == yes ]] || ok=0
  notes+=("status keyEpoch 1, previousKeyEpoch 0; $([[ "$res" == OK ]] && echo "both members' key files: epoch 1 first, then epoch 0 unchanged" || echo "$res")$([[ "$same" == yes ]] || echo "; $same")")
else ok=0; notes+=("fab status $(net_field fab 'o.get("status")')"); fi
record rotate "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- leave ------------------------------------------------------------------
ok=1; notes=()
before_a="$(net_field fab '[m["hosts"] for m in o["status"]["members"] if m["node"] == "'"$MAC_A"'"]')"
before_sub="$(net_field fab 'o["status"]["subnet"]')"
kube_curl PATCH "api/v1/nodes/$MAC_B" '{"metadata":{"labels":{"kairon.zyvor.dev/thunderbolt-fabric":null}}}' application/merge-patch+json >/dev/null
only_a() { [[ "$(net_field fab '[m["node"] for m in o["status"]["members"]]')" == "['$MAC_A']" ]]; }
if wait_until "$TIMEOUT" "mac-b out of fab" only_a; then
  after_a="$(net_field fab '[m["hosts"] for m in o["status"]["members"] if m["node"] == "'"$MAC_A"'"]')"
  [[ "$after_a" == "$before_a" && "$(net_field fab 'o["status"]["subnet"]')" == "$before_sub" ]] || { ok=0; notes+=("mac-a slice $before_a -> $after_a"); }
  for n in "${MACS[@]}"; do sync_mac "$n" || ok=0; done
  res="$(overlay_check leave)"
  [[ "$res" == OK ]] || ok=0
  notes+=("fab members [mac-a], mac-a kept $after_a and $before_sub; $([[ "$res" == OK ]] && echo "mac-b's fab entry and key removed, fleet-net kept; mac-a: $(summary_of "$MAC_A")" || echo "$res")")
else ok=0; notes+=("fab members $(net_field fab '[m["node"] for m in o["status"]["members"]]')"); fi
record leave "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

# --- cli --------------------------------------------------------------------
ok=1; notes=()
nodes_out="$("$KAIRONCTL" mac nodes 2>&1 || true)"
net_out="$("$KAIRONCTL" mac network fab -n "$NS" 2>&1 || true)"
grep -E "^${MAC_A}[[:space:]]+True[[:space:]]+m4-max[[:space:]]+12P\+4E" <<<"$nodes_out" >/dev/null || { ok=0; notes+=("mac nodes: $(grep "$MAC_A" <<<"$nodes_out")"); }
grep -E "^${MAC_C}[[:space:]]+True[[:space:]]+m1.*[[:space:]]no[[:space:]]+no[[:space:]]+-[[:space:]]+-" <<<"$nodes_out" >/dev/null || { ok=0; notes+=("mac nodes: $(grep "$MAC_C" <<<"$nodes_out")"); }
if ! grep -q "UDP port" <<<"$net_out" || ! grep -q "$MAC_A" <<<"$net_out"; then ok=0; notes+=("mac network: $net_out"); fi
[[ $ok == 1 ]] && notes+=("mac nodes listed chip, cores, macOS, Rosetta, nested, Thunderbolt and fabric for the 3 Macs; mac network fab showed subnet, port and members")
record cli "$([[ $ok == 1 ]] && echo pass || echo fail)" "$(join_notes)"

echo
echo "| Case | Result | Notes |"
echo "|---|---|---|"
printf '%s\n' "${RESULTS[@]}"
for r in "multi-Mac boot of vz Machines" "Rosetta and nested virtualization inside a guest" "overlay traffic between Macs over Thunderbolt" "capability and Thunderbolt discovery on real Macs"; do
  echo "| $r | not run | needs Macs |"
done
