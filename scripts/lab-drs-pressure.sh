#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# DRS on measured pressure, on one real node (see
# docs/guides/node-maintenance-drs.md). Two Machines boot on --node: one
# burns both vCPUs, one idles. A second, synthetic Node (no kubelet, no
# kairon-node) is created with a heartbeat, a lease and a calm pressure
# annotation the script keeps fresh, so the planner has somewhere to point.
# DRS runs in recommend mode only: nothing is migrated to the synthetic node.
# It checks:
#
#   publish               kairon-node writes the host's PSI (some avg60) and
#                         each Machine's own PSI to the Node annotation, and
#                         the host values track /proc/pressure
#   machine-stall         with both Machines' QEMU threads pinned to
#                         --contend-cpus and two busy loops on those CPUs,
#                         the burning Machine's cgroup stalls on CPU more
#                         than the idle one
#   plan                  kaironctl drs plan --signal pressure moves the most
#                         stalled Machine from the real node to the calm one
#   controller-recommend  kairon-controller -drs-mode=recommend
#                         -drs-signal=pressure records a DRSRecommendation
#                         Event on that Machine explaining the pressure
#   stale                 with the synthetic node's sample older than
#                         --pressure-max-age, the plan leaves it out
#
# The threshold is 25% unless the real node is calmer than that, in which
# case it is set just under the node's measured worst PSI (printed in the
# rows).
#
# Usage: lab-drs-pressure.sh --node=NAME --image=PATH [--contend-cpus=10,11] [--namespace=ns] [--timeout=600]
# Needs passwordless sudo, KAIRON_KUBE_* (lib/lab.sh), k3s kubectl, a
# kaironctl and a host-installed kairon-controller (systemd unit
# kairon-controller) from this change.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600; CONTEND=10,11
KAIRONCTL="${KAIRONCTL:-kaironctl}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --contend-cpus=*) CONTEND="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" ]] || { echo "usage: $0 --node=NAME --image=PATH" >&2; exit 1; }

STAMP="$(date -u +%m%d%H%M%S)"
BUSY="plab-busy-$STAMP"
IDLE="plab-idle-$STAMP"
CALM="plab-calm-$STAMP"
WORK="$(mktemp -d)"
DROPIN=/run/systemd/system/kairon-controller.service.d/plab-drs.conf
START="$(date -u +%FT%TZ)"
hb_pid=""
echo 0 > "$WORK/offset"
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }
kctl() { sudo -n k3s kubectl "$@"; }

restore_controller() {
  [[ -f "$DROPIN" ]] || return 0
  sudo -n rm -f "$DROPIN"
  sudo -n systemctl daemon-reload
  sudo -n systemctl restart kairon-controller
}

cleanup() {
  set +e
  [[ -n "$hb_pid" ]] && kill "$hb_pid" 2>/dev/null
  pkill -f "plab-host-burn-$STAMP" 2>/dev/null
  restore_controller
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$BUSY")" >/dev/null 2>&1
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$IDLE")" >/dev/null 2>&1
  kctl delete node "$CALM" --ignore-not-found --wait=false >/dev/null 2>&1
  kctl delete lease -n kube-node-lease "$CALM" --ignore-not-found >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

node_pressure() { kctl get node "$1" -o jsonpath='{.metadata.annotations.kairon\.zyvor\.dev/pressure}' 2>/dev/null; }
plan_json() { "$KAIRONCTL" drs plan --signal pressure --pressure-threshold-percent "$1" -o json; }

machine_body() {
  python3 - "$1" "$IMAGE" "$2" <<'EOF'
import json, sys
name, image, burn = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
spec = {
    "image": {"path": image},
    "resources": {"cpu": "2", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    # The synthetic node has no kubelet and no Cilium agent, and the loaded
    # lab node's own heartbeat lapses at times: tolerate the taints that
    # follow (the first two every Pod tolerates by default). Nothing ever
    # runs on the synthetic node; DRS only recommends.
    "placement": {"tolerations": [
        {"key": "node.kubernetes.io/not-ready", "operator": "Exists"},
        {"key": "node.kubernetes.io/unreachable", "operator": "Exists"},
        {"key": "node.cilium.io/agent-not-ready", "operator": "Exists"},
    ]},
}
if burn:
    spec["cloudInit"] = {"runCmd": [
        "systemd-run --unit plab-burn sh -c 'for i in 1 2; do (while :; do :; done) & done; wait'",
        "echo PLAB burn started > /dev/ttyS0",
    ]}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "drs-pressure"}}, "spec": spec}))
EOF
}

# --- Machines on the real node ----------------------------------------------
# Created before the synthetic node exists, so the scheduler can only pick
# the real one.
for spec in "$BUSY 1" "$IDLE 0"; do
  read -r name burn <<<"$spec"
  create() { kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$(machine_body "$name" "$burn")" >/dev/null 2>&1 || kube_exists "$(machine_path "$NAMESPACE" "$name")"; }
  wait_until 120 "$name created" create
done
for name in "$BUSY" "$IDLE"; do
  running() { [[ "$(machine_phase "$NAMESPACE" "$name" 2>/dev/null)" == Running ]]; }
  wait_until "$TIMEOUT" "$name Running" running
  [[ "$(machine_node "$NAMESPACE" "$name")" == "$NODE" ]] || { lab_log "$name landed on $(machine_node "$NAMESPACE" "$name"), not $NODE"; exit 1; }
done

# --- publish, machine-stall -----------------------------------------------
# The burn starts when cloud-init finishes, about a minute after boot; avg60
# needs most of a minute after that.
busy_console="${FLUXVM_WORKSPACES:-/var/lib/fluxvm/instances}/$(machine_field "$NAMESPACE" "$BUSY" '(o.get("status") or {}).get("runtimeID") or ""')/console.log"
burning() { sudo -n grep -q "PLAB burn started" "$busy_console" 2>/dev/null; }
wait_until "$TIMEOUT" "$BUSY burning" burning
# A guest only stalls when the host cannot give it CPU. Pin both Machines'
# QEMU threads to two host CPUs and keep those CPUs busy for a while, so the
# burning guest competes and the idle one mostly sleeps.
for name in "$BUSY" "$IDLE"; do
  rid="$(machine_field "$NAMESPACE" "$name" '(o.get("status") or {}).get("runtimeID") or ""')"
  sudo -n taskset -a -p -c "$CONTEND" "$(pgrep -f "qemu.*$rid" | head -1)" >/dev/null
done
for _ in 1 2; do
  timeout 150 taskset -c "$CONTEND" sh -c 'while :; do :; done' "plab-host-burn-$STAMP" &
done
lab_log "guest burning; QEMU pinned to CPUs $CONTEND with two host busy loops; waiting for kairon-node to publish both Machines, then 75s for avg60 to fill"
has_both() { node_pressure "$NODE" | python3 -c 'import json,sys; m=json.load(sys.stdin).get("machines") or {}; sys.exit(0 if all(k in m for k in sys.argv[1:]) else 1)' "$NAMESPACE/$BUSY" "$NAMESPACE/$IDLE"; }
wait_until 240 "both Machines in the pressure annotation" has_both
sleep 75
# Compare against /proc/pressure read right after a fresh sample.
fresh() { node_pressure "$NODE" | python3 -c 'import json,sys,datetime; a=json.load(sys.stdin); t=datetime.datetime.strptime(a["sampledAt"],"%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc); sys.exit(0 if (datetime.datetime.now(datetime.timezone.utc)-t).total_seconds() <= 10 else 1)'; }
wait_until 90 "a fresh pressure sample on $NODE" fresh
ann="$(node_pressure "$NODE")"
proc="$(python3 - <<'EOF'
import json
out = {}
for r in ("cpu", "memory", "io"):
    for line in open("/proc/pressure/" + r):
        if line.startswith("some"):
            out[r] = float(dict(kv.split("=") for kv in line.split()[1:])["avg60"])
print(json.dumps(out))
EOF
)"
read -r pub_ok pub_note <<<"$(python3 - "$ann" "$proc" "$(date -u +%s)" <<'EOF'
import json, sys, datetime
a, p, now = json.loads(sys.argv[1]), json.loads(sys.argv[2]), int(sys.argv[3])
at = datetime.datetime.strptime(a["sampledAt"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()
age = now - at
h = a["host"]
diff = max(abs(h[r] - p[r]) for r in ("cpu", "memory", "io"))
ok = age <= 15 and diff <= 5
print("pass" if ok else "fail",
      f"sample {age:.0f}s old; annotation cpu={h['cpu']} memory={h['memory']} io={h['io']}; /proc/pressure avg60 cpu={p['cpu']} memory={p['memory']} io={p['io']} (largest gap {diff:.1f} points)")
EOF
)"
result publish "$pub_ok" "$pub_note"

read -r stall_ok hottest stall_note <<<"$(python3 - "$ann" "$NAMESPACE/$BUSY" "$NAMESPACE/$IDLE" "$CONTEND" <<'EOF'
import json, sys
m = json.loads(sys.argv[1])["machines"]
b, i = m[sys.argv[2]], m[sys.argv[3]]
worst = lambda p: max(p["cpu"], p["memory"], p["io"])
hottest = (sys.argv[2] if worst(b) >= worst(i) else sys.argv[3]).split("/", 1)[1]
print("pass" if b["cpu"] > i["cpu"] else "fail", hottest,
      f"QEMU pinned to CPUs {sys.argv[4]} with two host busy loops: busy cpu={b['cpu']} io={b['io']}; idle cpu={i['cpu']} io={i['io']} (some avg60, each Machine's own cgroup)")
EOF
)"
result machine-stall "$stall_ok" "$stall_note"

host_max="$(python3 -c 'import json,sys; h=json.loads(sys.argv[1])["host"]; print(max(h.values()))' "$ann")"
THRESHOLD="$(python3 -c 'import sys; m=float(sys.argv[1]); print(25 if m >= 26 else max(2, int(m) - 1))' "$host_max")"
lab_log "real node worst PSI $host_max%; threshold $THRESHOLD%"

# --- synthetic calm node ------------------------------------------------------
alloc="$(kctl get node "$NODE" -o json | python3 -c 'import json,sys; a=json.load(sys.stdin)["status"]["allocatable"]; print(json.dumps({k: a[k] for k in ("cpu", "memory", "pods") if k in a}))')"
calm_json="$(cat <<EOF
{"apiVersion": "v1", "kind": "Node",
 "metadata": {"name": "$CALM", "labels": {"kubernetes.io/hostname": "$CALM", "kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64",
                                          "kairon.zyvor.dev/capable": "true", "kairon.zyvor.dev/lab": "drs-pressure"}}}
EOF
)"
create_calm() { kctl create -f - >/dev/null 2>&1 <<<"$calm_json" || kctl get node "$CALM" >/dev/null 2>&1; }
wait_until 120 "$CALM created" create_calm
# Runs without errexit: a slow API must not end the heartbeat.
heartbeat() {
  local now at off
  set +e
  while true; do
    now="$(date -u +%FT%TZ)"
    off="$(cat "$WORK/offset" 2>/dev/null || echo 0)"
    at="$(date -u -d "-$off seconds" +%FT%TZ)"
    kctl patch node "$CALM" --subresource=status --type=merge -p "{\"status\":{\"conditions\":[{\"type\":\"Ready\",\"status\":\"True\",\"reason\":\"KubeletReady\",\"message\":\"lab-drs-pressure synthetic node\",\"lastHeartbeatTime\":\"$now\",\"lastTransitionTime\":\"$START\"}],\"capacity\":$alloc,\"allocatable\":$alloc}}" >/dev/null 2>&1
    kctl annotate node "$CALM" --overwrite "kairon.zyvor.dev/pressure={\"sampledAt\":\"$at\",\"host\":{\"cpu\":0,\"memory\":0,\"io\":0}}" >/dev/null 2>&1
    kctl apply -f - >/dev/null 2>&1 <<LEASE
{"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": {"name": "$CALM", "namespace": "kube-node-lease"},
 "spec": {"holderIdentity": "$CALM", "leaseDurationSeconds": 40, "renewTime": "$(date -u +%FT%T.000000Z)"}}
LEASE
    sleep 10
  done
}
heartbeat & hb_pid=$!
calm_ready() { [[ "$(kctl get node "$CALM" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" == True ]] && [[ -n "$(node_pressure "$CALM")" ]]; }
wait_until 120 "$CALM Ready with a pressure sample" calm_ready

# --- plan ---------------------------------------------------------------------
plan="$(plan_json "$THRESHOLD" 2>&1 || true)"
read -r plan_ok plan_note <<<"$(python3 - "$plan" "$NODE" "$CALM" "$hottest" <<'EOF'
import json, sys
raw, node, calm, hottest = sys.argv[1:5]
try:
    p = json.loads(raw)
except ValueError:
    print("fail", "kaironctl: " + raw.replace("\n", " ")[:200]); sys.exit()
mv = p.get("moves") or []
ok = len(mv) == 1 and mv[0]["from"] == node and mv[0]["to"] == calm and mv[0]["machine"] == hottest
desc = "; ".join(f"{m['namespace']}/{m['machine']} {m['from']} -> {m['to']}: {m['reason']}" for m in mv) or "no moves: " + p.get("reason", "")
print("pass" if ok else "fail", f"spread {p.get('imbalanceBefore')} points; " + desc)
EOF
)"
[[ "$plan_ok" == pass ]] || plan_note="$plan_note; $CALM Ready=$(kctl get node "$CALM" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null) taints=$(kctl get node "$CALM" -o jsonpath='{.spec.taints}' 2>/dev/null)"
result plan "$plan_ok" "$plan_note"

# --- controller-recommend -----------------------------------------------------
exec_start="$(systemctl show kairon-controller -p ExecStart --value | sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p')"
sudo -n mkdir -p "$(dirname "$DROPIN")"
printf '[Service]\nExecStart=\nExecStart=%s --drs-mode=recommend --drs-signal=pressure --drs-pressure-threshold-percent=%s --drs-interval=30s\n' \
  "$exec_start" "$THRESHOLD" | sudo -n tee "$DROPIN" >/dev/null
sudo -n systemctl daemon-reload
sudo -n systemctl restart kairon-controller
event_msg() {
  kctl get events -n "$NAMESPACE" --field-selector "reason=DRSRecommendation,involvedObject.name=$hottest" \
    -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null | tail -1
}
has_event() { [[ "$(event_msg)" == *"pressure on $NODE"* ]]; }
if wait_until 240 "DRSRecommendation on $hottest" has_event; then
  result controller-recommend pass "Event on $NAMESPACE/$hottest: $(event_msg)"
else
  result controller-recommend fail "no DRSRecommendation naming the pressure: '$(event_msg)'; $(sudo -n journalctl -u kairon-controller --since "-4min" --no-pager | grep 'drs: plan' | tail -1 | cut -c1-200)"
fi
restore_controller

# --- stale ------------------------------------------------------------------------
echo 600 > "$WORK/offset"
stale() { node_pressure "$CALM" | python3 -c 'import json,sys,datetime; a=json.load(sys.stdin); t=datetime.datetime.strptime(a["sampledAt"],"%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc); sys.exit(0 if (datetime.datetime.now(datetime.timezone.utc)-t).total_seconds() > 500 else 1)'; }
wait_until 60 "$CALM sample backdated" stale
plan="$(plan_json "$THRESHOLD" 2>&1 || true)"
read -r stale_ok stale_note <<<"$(python3 - "$plan" "$CALM" <<'EOF'
import json, sys
try:
    p = json.loads(sys.argv[1])
except ValueError:
    print("fail", "kaironctl: " + sys.argv[1].replace("\n", " ")[:200]); sys.exit()
r = p.get("reason", "")
print("pass" if not p.get("moves") and ("no fresh sample: " + sys.argv[2]) in r else "fail", "no moves: " + r)
EOF
)"
result stale "$stale_ok" "$stale_note"

rc=0
echo
today="$(date -u +%F)"
kernel="$(uname -r)"
for c in publish machine-stall plan controller-recommend stale; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| DRS pressure: $c | $r | $today | Node $NODE (kernel $kernel) and synthetic node $CALM, threshold $THRESHOLD%; ${NOTE[$c]:-earlier step failed} |"
done
echo "| DRS pressure: automatic move between two real nodes | not run | $today | one lab node; the synthetic node only receives recommendations |"
exit $rc
