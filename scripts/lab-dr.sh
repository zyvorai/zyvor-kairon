#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Cross-cluster DR drill (experimental; see docs/guides/cross-cluster-dr.md).
# Two "sites" are two namespaces, --site-a and --site-b, each with its own
# MachineProtectionPlan "shop" whose peer is the other namespace, reached
# through the Kubernetes API with a kairon-dr-peer ServiceAccount token
# (deploy/dr-peer-rbac.yaml). Both sites share one S3 bucket, MinIO started
# with podman/docker on this host, or a versitygw binary with
# --s3-gateway=PATH (POSIX backend in /var/tmp). On a real deployment the peer is another
# cluster; here it is the same API server, so "site loss" is simulated by
# unlabelling the peer credentials Secret.
#
# Site A runs db and web (web depends on db). A systemd unit in each guest
# fsyncs "gen time" to disk every 2s and prints it on the serial console,
# with the values it found on disk at boot, so a restored guest reports the
# instant its disk returns to. Cases:
#
#   replicate  A's dr-shop policy backs both Machines up; B imports the
#              restore points and reports RPO with RPOMet=True
#   dr-test    `kaironctl dr test` on B: copies of db and web Running on the
#              isolated --test-bridge, restored data within the restore
#              point; A keeps running; `dr test-end` removes the copies
#   consolidate  a policy synthetic full of web's newest point on A: the
#              next replication is an incremental on it, not a full, and
#              retention does not keep rewriting synthetic fulls
#   failover   both sites cut off from each other, `kaironctl dr failover`
#              on B: db then web Running there from the newest restore
#              points; on reconnecting, A sees the higher epoch, halts its
#              Machines and stops replicating
#   failback   `kaironctl dr failover --planned` on A: B halts, takes final
#              backups, A restores them: the guest's last write on B is on A
#              (no data lost), and B is halted
#
# and prints COMPATIBILITY.md rows with the measured RPO, RTO and data loss.
# Needs KAIRON_KUBE_* (lib/lab.sh), FLUXVM_URL for the serial console,
# kairon-controller running with --dr-secret-namespace=kairon-system and
# --dr-test-bridges including --test-bridge, sudo for the bridge and
# S3 gateway, and a node labelled kairon.zyvor.dev/cbt=true.
#
# Usage:
#   lab-dr.sh --image=PATH [--node=NAME] [--site-a=dr-a] [--site-b=dr-b]
#       [--interval=5m] [--test-bridge=drtest0] [--timeout=1200]
#       [--s3-gateway=PATH-TO-versitygw] [--keep]
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

IMAGE=""; NODE=""; SITE_A=dr-a; SITE_B=dr-b; INTERVAL=5m; BRIDGE=drtest0; TIMEOUT=1200; KEEP=0
SECRET_NS=kairon-system; PLAN=shop; S3_CREDS=dr-lab-s3; S3_BUCKET=kairon-dr-lab; PEER_SECRET=dr-lab-peer
S3_GATEWAY=""; S3_PID=""; S3_DIR=""
for arg in "$@"; do
  case "$arg" in
    --s3-gateway=*) S3_GATEWAY="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --site-a=*) SITE_A="${arg#*=}" ;;
    --site-b=*) SITE_B="${arg#*=}" ;;
    --interval=*) INTERVAL="${arg#*=}" ;;
    --test-bridge=*) BRIDGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --keep) KEEP=1 ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$IMAGE" ]] || { echo "usage: $0 --image=PATH [--node=NAME]" >&2; exit 1; }
: "${FLUXVM_URL:?set FLUXVM_URL (serial console)}"
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }
MERGE=application/merge-patch+json

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}
rt() { if command -v podman >/dev/null; then sudo -n podman "$@"; else sudo -n docker "$@"; fi; }
json() { python3 -c "import json,sys; print(json.dumps($1))" "${@:2}"; }

plan_path() { echo "$LAB_API/namespaces/$1/machineprotectionplans/$PLAN"; }
plan_field() { kube_curl GET "$(plan_path "$1")" 2>/dev/null | json_eval "$2" 2>/dev/null || true; }
st() { plan_field "$1" "(o.get('status') or {})$2"; }
field() { machine_field "$1" "$2" "$3" 2>/dev/null || true; }
phase_is() { [[ "$(field "$1" "$2" '(o.get("status") or {}).get("phase") or ""')" == "$3" ]]; }
desired() { field "$1" "$2" '(o.get("spec") or {}).get("powerState") or "Running"'; }
exists() { kube_exists "$(machine_path "$1" "$2")"; }

# probe NS MACHINE -- "boot restored restoredT gen t" from the newest
# console line, empty when none yet.
probe() {
  local rid
  rid="$(field "$1" "$2" '(o.get("status") or {}).get("runtimeID") or ""')"
  [[ -n "$rid" ]] || return 0
  flux "/v1/vms/$rid/logs?lines=300" 2>/dev/null | tr -d '\r' |
    grep -o 'MLAB boot=[0-9a-f]* restored=[0-9]* restoredT=[0-9]* gen=[0-9]* t=[0-9]*' | tail -1 |
    sed -E 's/MLAB boot=([0-9a-f]*) restored=([0-9]*) restoredT=([0-9]*) gen=([0-9]*) t=([0-9]*)/\1 \2 \3 \4 \5/' || true
}
probing() { [[ -n "$(probe "$1" "$2")" ]]; }
restored_boot() { local p; p="$(probe "$1" "$2")"; [[ -n "$p" && "$(cut -d' ' -f2 <<<"$p")" -gt 0 ]]; }

PROBE='#!/bin/sh
f=/var/lib/drlab/state
mkdir -p /var/lib/drlab
boot=$(cut -c1-8 /proc/sys/kernel/random/boot_id)
set -- $(cat "$f" 2>/dev/null || echo "0 0")
r=$1; rt=$2; g=$1
while true; do
  g=$((g+1)); t=$(date +%s)
  echo "$g $t" > "$f.tmp" && sync "$f.tmp" && mv "$f.tmp" "$f" && sync /var/lib/drlab
  echo "MLAB boot=$boot restored=$r restoredT=$rt gen=$g t=$t" > /dev/ttyS0
  sleep 2
done'
UNIT='[Unit]
Description=Kairon DR lab probe
[Service]
ExecStart=/usr/local/bin/drlab-probe
Restart=always
[Install]
WantedBy=multi-user.target'

machine_body() {
  python3 - "$1" "$IMAGE" "$NODE" "$PROBE" "$UNIT" <<'EOF'
import json, sys
name, image, node, probe, unit = sys.argv[1:6]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "cloudInit": {
        "writeFiles": [
            {"path": "/usr/local/bin/drlab-probe", "content": probe, "permissions": "0755"},
            {"path": "/etc/systemd/system/drlab.service", "content": unit, "permissions": "0644"},
        ],
        "runCmd": ["systemctl daemon-reload", "systemctl enable --now drlab.service"],
    },
}
if node:
    spec["placement"] = {"nodeSelector": {"kubernetes.io/hostname": node}}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "dr", "app": name}}, "spec": spec}))
EOF
}

plan_body() {
  python3 - "$PLAN" "$1" "$2" "$KAIRON_KUBE_URL" "$PEER_SECRET" "$INTERVAL" "$S3_CREDS" "$S3_BUCKET" "$BRIDGE" <<'EOF'
import json, sys
plan, role, peer_ns, server, secret, interval, creds, bucket, bridge = sys.argv[1:10]
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "MachineProtectionPlan", "metadata": {"name": plan},
    "spec": {"role": role, "machines": [{"name": "db"}, {"name": "web", "dependsOn": ["db"]}],
             "replication": {"interval": interval, "fullEvery": "24h", "quiesce": "never",
                             "target": {"s3": {"endpoint": "http://127.0.0.1:9000", "bucket": bucket, "prefix": "dr-lab",
                                               "credentialsSecretRef": creds, "pathStyle": True}}},
             "objectives": {"rpo": "15m", "rto": "10m"},
             "peer": {"server": server, "credentialsSecretRef": secret, "namespace": peer_ns},
             "test": {"bridge": bridge, "ttl": "30m"}}}))
EOF
}

peer_label() {
  kube_curl PATCH "api/v1/namespaces/$SECRET_NS/secrets/$PEER_SECRET" "{\"metadata\":{\"labels\":{\"kairon.zyvor.dev/dr-peer-credentials\":$1}}}" "$MERGE" >/dev/null
}

cleanup() {
  [[ "$KEEP" == 1 ]] && { lab_log "--keep: leaving $SITE_A, $SITE_B, the S3 gateway and $BRIDGE"; return 0; }
  local ns r n
  for ns in "$SITE_A" "$SITE_B"; do kube_curl DELETE "$(plan_path "$ns")" >/dev/null 2>&1 || true; done
  sleep 6
  # kairon-node deletes the backups' S3 objects first, so S3 and its
  # credentials Secret go last.
  for ns in "$SITE_A" "$SITE_B"; do
    for r in machinebackuppolicies machinebackuprestores machinebackups machines; do
      for n in $(kube_curl GET "$LAB_API/namespaces/$ns/$r" 2>/dev/null | json_eval '" ".join(i["metadata"]["name"] for i in o["items"])' 2>/dev/null); do
        kube_curl DELETE "$LAB_API/namespaces/$ns/$r/$n" >/dev/null 2>&1 || true
      done
    done
  done
  backups_gone() {
    local ns
    for ns in "$SITE_A" "$SITE_B"; do
      [[ "$(kube_curl GET "$LAB_API/namespaces/$ns/machinebackups" 2>/dev/null | json_eval 'len(o["items"])' 2>/dev/null)" == 0 ]] || return 1
    done
  }
  wait_until 300 "backups deleted from S3" backups_gone || true
  for ns in "$SITE_A" "$SITE_B"; do kube_curl DELETE "api/v1/namespaces/$ns" >/dev/null 2>&1 || true; done
  kube_curl DELETE "api/v1/namespaces/$SECRET_NS/secrets/$PEER_SECRET" >/dev/null 2>&1 || true
  rt rm -f kairon-dr-minio >/dev/null 2>&1 || true
  [[ -n "$S3_PID" ]] && { kill "$S3_PID" 2>/dev/null || true; rm -rf "$S3_DIR" "$S3_DIR.log"; }
  sudo -n ip link del "$BRIDGE" 2>/dev/null || true
}
trap cleanup EXIT

# --- setup ------------------------------------------------------------------
lab_log "setup: namespaces $SITE_A/$SITE_B, S3, bridge $BRIDGE, peer token"
for ns in "$SITE_A" "$SITE_B"; do
  kube_curl POST api/v1/namespaces "$(json '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":sys.argv[1]}}' "$ns")" >/dev/null 2>&1 || true
done
ip link show "$BRIDGE" >/dev/null 2>&1 || { sudo -n ip link add "$BRIDGE" type bridge && sudo -n ip link set "$BRIDGE" up; }

S3_USER="kairon$RANDOM"; S3_PASS="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
s3() { curl -fsS --aws-sigv4 "aws:amz:us-east-1:s3" --user "$S3_USER:$S3_PASS" "$@"; }
if [[ -n "$S3_GATEWAY" ]]; then
  S3_DIR="$(mktemp -d /var/tmp/kairon-dr-s3.XXXXXX)"
  "$S3_GATEWAY" --access "$S3_USER" --secret "$S3_PASS" --port 127.0.0.1:9000 posix "$S3_DIR" >"$S3_DIR.log" 2>&1 &
  S3_PID=$!
else
  rt rm -f kairon-dr-minio >/dev/null 2>&1 || true
  rt run -d --name kairon-dr-minio -p 127.0.0.1:9000:9000 -e MINIO_ROOT_USER="$S3_USER" -e MINIO_ROOT_PASSWORD="$S3_PASS" \
    quay.io/minio/minio server /data >/dev/null
fi
s3_up() { s3 -o /dev/null http://127.0.0.1:9000/ 2>/dev/null; }
wait_until 60 "S3 ready" s3_up
s3 -X PUT "http://127.0.0.1:9000/$S3_BUCKET" >/dev/null
for ns in "$SITE_A" "$SITE_B"; do
  kube_curl DELETE "api/v1/namespaces/$ns/secrets/$S3_CREDS" >/dev/null 2>&1 || true
  kube_curl POST "api/v1/namespaces/$ns/secrets" "$(json '{"apiVersion":"v1","kind":"Secret","metadata":{"name":sys.argv[1],"labels":{"kairon.zyvor.dev/backup-credentials":"true"}},"stringData":{"accessKeyId":sys.argv[2],"secretAccessKey":sys.argv[3]}}' "$S3_CREDS" "$S3_USER" "$S3_PASS")" >/dev/null
done

# The peer RBAC from deploy/dr-peer-rbac.yaml, bound in both site namespaces.
kube_curl POST "api/v1/namespaces/$SECRET_NS/serviceaccounts" '{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"kairon-dr-peer"}}' >/dev/null 2>&1 || true
kube_curl POST apis/rbac.authorization.k8s.io/v1/clusterroles "$(python3 - <<'EOF'
import json
print(json.dumps({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": {"name": "kairon-dr-peer"}, "rules": [
    {"apiGroups": ["kairon.zyvor.dev"], "resources": ["machineprotectionplans"], "verbs": ["get", "patch"]},
    {"apiGroups": ["kairon.zyvor.dev"], "resources": ["machines"], "verbs": ["get"]},
    {"apiGroups": ["kairon.zyvor.dev"], "resources": ["machinebackups"], "verbs": ["list"]}]}))
EOF
)" >/dev/null 2>&1 || true
for ns in "$SITE_A" "$SITE_B"; do
  kube_curl POST "apis/rbac.authorization.k8s.io/v1/namespaces/$ns/rolebindings" "$(json '{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"kairon-dr-peer"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"kairon-dr-peer"},"subjects":[{"kind":"ServiceAccount","name":"kairon-dr-peer","namespace":sys.argv[1]}]}' "$SECRET_NS")" >/dev/null 2>&1 || true
done
TOKEN="$(kube_curl POST "api/v1/namespaces/$SECRET_NS/serviceaccounts/kairon-dr-peer/token" '{"apiVersion":"authentication.k8s.io/v1","kind":"TokenRequest","spec":{"expirationSeconds":14400}}' | json_eval 'o["status"]["token"]')"
CA="$(kube_curl GET "api/v1/namespaces/$SECRET_NS/configmaps/kube-root-ca.crt" | json_eval 'o["data"]["ca.crt"]')"
kube_curl DELETE "api/v1/namespaces/$SECRET_NS/secrets/$PEER_SECRET" >/dev/null 2>&1 || true
kube_curl POST "api/v1/namespaces/$SECRET_NS/secrets" "$(json '{"apiVersion":"v1","kind":"Secret","metadata":{"name":sys.argv[1],"labels":{"kairon.zyvor.dev/dr-peer-credentials":"true"}},"stringData":{"token":sys.argv[2],"ca.crt":sys.argv[3]}}' "$PEER_SECRET" "$TOKEN" "$CA")" >/dev/null

for m in db web; do kube_curl POST "$LAB_API/namespaces/$SITE_A/machines" "$(machine_body "$m")" >/dev/null; done
for m in db web; do
  wait_until 600 "$SITE_A/$m Running" phase_is "$SITE_A" "$m" Running
  wait_until 300 "$SITE_A/$m probe" probing "$SITE_A" "$m"
done
kube_curl POST "$LAB_API/namespaces/$SITE_A/machineprotectionplans" "$(plan_body Primary "$SITE_B")" >/dev/null
kube_curl POST "$LAB_API/namespaces/$SITE_B/machineprotectionplans" "$(plan_body Secondary "$SITE_A")" >/dev/null

# --- replicate --------------------------------------------------------------
# points NS -- how many protected Machines have a restore point there.
points() { st "$1" '.get("machines") and len([x for x in o["status"]["machines"] if x.get("restorePoint")])'; }
both_points() { [[ "$(points "$SITE_B")" == 2 ]]; }
T0=$(date +%s)
if wait_until "$TIMEOUT" "$SITE_B imports restore points of db and web" both_points; then
  sleep 6
  rpo="$(st "$SITE_B" '.get("rpoSeconds")')"; met="$(plan_field "$SITE_B" 'cond("RPOMet")')"
  imported="$(kube_curl GET "$LAB_API/namespaces/$SITE_B/machinebackups" | json_eval 'len([b for b in o["items"] if (b["metadata"].get("annotations") or {}).get("kairon.zyvor.dev/replicated-from")])')"
  if [[ "$(st "$SITE_A" '.get("active")')" == True && "$met" == True ]]; then
    result replicate pass "$SITE_A Active epoch 1, policy dr-$PLAN every $INTERVAL; $SITE_B imported $imported restore points $(( $(date +%s) - T0 ))s after the plan was created; RPO ${rpo%.*}s, RPOMet=True"
  else
    result replicate fail "A active=$(st "$SITE_A" '.get("active")') RPOMet=$met rpo=$rpo"
  fi
else
  result replicate fail "no restore points on $SITE_B: $(st "$SITE_B" '.get("machines")')"
fi

run_phase() { st "$1" ".get('$2') and o['status']['$2'].get('phase') or ''"; }
run_done() { local p; p="$(run_phase "$1" "$2")"; [[ "$p" == Succeeded || "$p" == Failed || "$p" == Ended ]]; }
run_field() { st "$1" ".get('$2') and o['status']['$2'].get('$3')"; }
# point_time NS BACKUP -- unix seconds of the restore point.
point_time() {
  kube_curl GET "$LAB_API/namespaces/$1/machinebackups/$2" | json_eval '__import__("datetime").datetime.fromisoformat(o["status"]["pointInTime"].replace("Z","+00:00")).timestamp()' | cut -d. -f1
}
join_notes() {
  local s
  s="$(printf '%s; ' "${notes[@]}")"
  echo "${s%; }"
}
# check_restored NS MACHINE SOURCE_NS BACKUP -- the restored disk's last
# write is within the restore point (-20s..+5s); prints the gap.
check_restored() {
  local p rt pit
  p="$(probe "$1" "$2")"; rt="$(cut -d' ' -f3 <<<"$p")"; pit="$(point_time "$3" "$4")"
  echo "$2's last write on the restored disk is $((rt - pit))s from the restore point's time"
  (( rt <= pit + 5 && rt >= pit - 20 ))
}

if [[ "${RESULT[replicate]}" == pass ]]; then
  # --- dr-test --------------------------------------------------------------
  "$KAIRONCTL" dr test "$PLAN" -n "$SITE_B" >&2
  if wait_until "$TIMEOUT" "DR test" run_done "$SITE_B" test && [[ "$(run_phase "$SITE_B" test)" == Succeeded ]]; then
    ok=1; notes=()
    for m in db web; do
      wait_until 300 "$m-drtest restored probe" restored_boot "$SITE_B" "$m-drtest" || ok=0
      br="$(field "$SITE_B" "$m-drtest" '(o.get("spec") or {}).get("network",{}).get("bridge") or ""')"
      [[ "$br" == "$BRIDGE" ]] || { ok=0; notes+=("$m-drtest bridge '$br'"); }
      pt="$(st "$SITE_B" ".get('test',{}).get('points',{}).get('$m')")"
      n="$(check_restored "$SITE_B" "$m-drtest" "$SITE_B" "$pt")" || ok=0; notes+=("$n")
    done
    phase_is "$SITE_A" db Running && phase_is "$SITE_A" web Running || { ok=0; notes+=("site A not Running"); }
    rto="$(run_field "$SITE_B" test rtoSeconds)"
    "$KAIRONCTL" dr test-end "$PLAN" -n "$SITE_B" >&2
    gone() { ! exists "$SITE_B" db-drtest && ! exists "$SITE_B" web-drtest; }
    wait_until 300 "test copies removed" gone || { ok=0; notes+=("copies not removed"); }
    if [[ "$ok" == 1 ]]; then
      result dr-test pass "copies of db, web Running on isolated bridge $BRIDGE in ${rto%.*}s; $(join_notes); $SITE_A kept running; test-end removed the copies"
    else
      result dr-test fail "$(join_notes)"
    fi
  else
    result dr-test fail "test run $(run_phase "$SITE_B" test): $(run_field "$SITE_B" test message)"
  fi

  # --- consolidate ----------------------------------------------------------
  # Retention only consolidates bases older than its window, which takes
  # hours, so the drill requests the policy's synthetic full of web's newest
  # point itself. Retention must then drop the old chain, the next
  # replication must be an incremental on the synthetic full, and no
  # further synthetic full may follow.
  web_backups() { kube_curl GET "$LAB_API/namespaces/$SITE_A/machinebackups?labelSelector=kairon.zyvor.dev/backup-policy%3Ddr-$PLAN,kairon.zyvor.dev/backup-machine%3Dweb"; }
  synth="dr-$PLAN-web-synth-lab-$(date +%s)"
  body="$(web_backups | python3 -c '
import json, sys
o, name = json.load(sys.stdin), sys.argv[1]
ok = [b for b in o["items"] if (b.get("status") or {}).get("phase") == "Succeeded" and b["spec"].get("mode") != "synthetic-full"]
src = max(ok, key=lambda b: b["status"]["pointInTime"])
spec = dict(src["spec"], mode="synthetic-full", parent=src["metadata"]["name"])
print(json.dumps({"apiVersion": src["apiVersion"], "kind": "MachineBackup",
                  "metadata": {"name": name, "labels": src["metadata"]["labels"]}, "spec": spec}))
' "$synth")"
  kube_curl POST "$LAB_API/namespaces/$SITE_A/machinebackups" "$body" >/dev/null
  on_synth() {
    web_backups | json_eval "next((b['metadata']['name'] for b in o['items'] if b['spec'].get('parent') == '$synth' and (b.get('status') or {}).get('phase') == 'Succeeded'), '')"
  }
  has_on_synth() { [[ -n "$(on_synth 2>/dev/null)" ]]; }
  if wait_until 900 "an incremental on $synth on $SITE_A" has_on_synth; then
    sleep 30
    left="$(web_backups | json_eval "sorted(b['metadata']['name'] for b in o['items'] if b['metadata']['name'] != '$synth' and (b.get('status') or {}).get('pointInTime', '9') <= next(x['status']['pointInTime'] for x in o['items'] if x['metadata']['name'] == '$synth'))")"
    synths="$(web_backups | json_eval "len([b for b in o['items'] if b['spec'].get('mode') == 'synthetic-full'])")"
    if [[ "$synths" == 1 ]]; then
      result consolidate pass "the next replication after $synth was an incremental on it, $(on_synth); no further synthetic full; older backups left: $left"
    else
      result consolidate fail "older backups left: $left; synthetic fulls: $synths"
    fi
  else
    result consolidate fail "no incremental on $synth: $(web_backups | json_eval '[(b["metadata"]["name"], b["spec"].get("parent"), (b.get("status") or {}).get("phase")) for b in o["items"]]')"
  fi

  # --- failover (unplanned) -------------------------------------------------
  lab_log "cutting the sites off from each other"
  peer_label null
  unreachable() { [[ "$(st "$SITE_B" '.get("peer",{}).get("reachable")')" == False && "$(st "$SITE_A" '.get("peer",{}).get("reachable")')" == False ]]; }
  wait_until 60 "both sites see the peer unreachable" unreachable || true
  "$KAIRONCTL" dr failover "$PLAN" -n "$SITE_B" >&2
  if wait_until "$TIMEOUT" "failover" run_done "$SITE_B" failover && [[ "$(run_phase "$SITE_B" failover)" == Succeeded ]]; then
    ok=1; notes=()
    order="$(run_field "$SITE_B" failover restored)"
    [[ "$order" == "['db', 'web']" ]] || { ok=0; notes+=("order $order"); }
    for m in db web; do
      wait_until 300 "$SITE_B/$m restored probe" restored_boot "$SITE_B" "$m" || ok=0
      pt="$(st "$SITE_B" ".get('failover',{}).get('points',{}).get('$m')")"
      n="$(check_restored "$SITE_B" "$m" "$SITE_B" "$pt")" || ok=0; notes+=("$n")
    done
    split="$(phase_is "$SITE_A" db Running && echo "still Running while cut off" || echo "not Running")"
    rto="$(run_field "$SITE_B" failover rtoSeconds)"; loss="$(run_field "$SITE_B" failover dataLossSeconds)"
    peer_label '"true"'
    fenced() { [[ "$(st "$SITE_A" '.get("active")')" == False && "$(desired "$SITE_A" db)" == Halted && "$(desired "$SITE_A" web)" == Halted ]] && ! kube_exists "$LAB_API/namespaces/$SITE_A/machinebackuppolicies/dr-$PLAN"; }
    T1=$(date +%s)
    if wait_until 120 "$SITE_A fenced" fenced; then
      notes+=("A $split, fenced $(( $(date +%s) - T1 ))s after reconnecting: Machines Halted, policy deleted, epoch $(st "$SITE_A" '.get("epoch")')")
    else
      ok=0; notes+=("A not fenced: active=$(st "$SITE_A" '.get("active")') db=$(desired "$SITE_A" db)")
    fi
    if [[ "$ok" == 1 ]]; then
      result failover pass "db then web Running on $SITE_B, RTO ${rto%.*}s (objective 10m), data loss ${loss%.*}s; $(join_notes)"
    else
      result failover fail "$(join_notes)"
    fi
  else
    peer_label '"true"'
    result failover fail "failover $(run_phase "$SITE_B" failover): $(run_field "$SITE_B" failover message)"
  fi
fi

# --- failback (planned) -----------------------------------------------------
if [[ "${RESULT[failover]:-}" == pass ]]; then
  a_points() { [[ "$(points "$SITE_A")" == 2 ]]; }
  if wait_until "$TIMEOUT" "$SITE_A imports $SITE_B's restore points" a_points; then
    "$KAIRONCTL" dr failover "$PLAN" -n "$SITE_A" --planned >&2
    # The guest's newest write on B before the handoff halted it.
    last_b=0
    track() {
      local g
      g="$(probe "$SITE_B" db | cut -d' ' -f4)"
      [[ -n "$g" ]] && (( g > last_b )) && last_b=$g
      [[ "$(desired "$SITE_B" db)" == Halted ]] && phase_is "$SITE_B" db Halted
    }
    wait_until "$TIMEOUT" "$SITE_B halted by the handoff" track || true
    track || true
    if wait_until "$TIMEOUT" "failback" run_done "$SITE_A" failover && [[ "$(run_phase "$SITE_A" failover)" == Succeeded ]]; then
      wait_until 300 "$SITE_A/db restored probe" restored_boot "$SITE_A" db || true
      got="$(probe "$SITE_A" db | cut -d' ' -f2)"
      rto="$(run_field "$SITE_A" failover rtoSeconds)"; loss="$(run_field "$SITE_A" failover dataLossSeconds)"
      b_halted=$([[ "$(st "$SITE_B" '.get("active")')" == False && "$(desired "$SITE_B" web)" == Halted ]] && echo yes || echo no)
      if [[ -n "$got" ]] && (( got >= last_b )) && [[ "$b_halted" == yes ]]; then
        result failback pass "planned: $SITE_B halted and took final backups, $SITE_A restored them in ${rto%.*}s; db's last write on B (gen $last_b) is on A (gen $got), data loss ${loss%.*}s; $SITE_B standby, epoch $(st "$SITE_A" '.get("epoch")')"
      else
        result failback fail "db on A restored gen '$got' < last on B $last_b, or B not halted ($b_halted)"
      fi
    else
      result failback fail "failback $(run_phase "$SITE_A" failover): $(run_field "$SITE_A" failover message)"
    fi
  else
    result failback fail "$SITE_A has no restore points from $SITE_B: $(st "$SITE_A" '.get("machines")')"
  fi
fi

echo
echo "| Case | Result | Notes |"
echo "|---|---|---|"
for c in replicate dr-test consolidate failover failback; do
  echo "| $c | ${RESULT[$c]:-not run} | ${NOTE[$c]:-an earlier case failed} |"
done
for c in replicate dr-test consolidate failover failback; do [[ "${RESULT[$c]:-}" == pass ]] || exit 1; done
