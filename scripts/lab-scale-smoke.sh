#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Scale smoke: ramp up small CirrOS Machines in batches, wait for each batch to
# run, stop at the first limit (Machines stuck Pending, a batch that does not
# come up in time, or a host below its free-memory floor), optionally migrate a
# few, then delete everything and confirm it is gone. The result is the number
# of Machines the lab held, not a benchmark.
#
#   KAIRON_HW_LAB=1                       safety gate (a real lab, not a laptop)
#   KAIRON_KUBE_URL / TOKEN / CA          cluster API (same as the other lab scripts)
#   KAIRON_SCALE_MAX                      upper bound to ramp to (default 20)
#   KAIRON_SCALE_BATCH                    Machines per batch (default 4)
#   KAIRON_SCALE_HOSTS                    comma list of user@host to watch free memory on
#   KAIRON_SCALE_MIN_FREE_MIB             stop when a watched host has less available (default 3072)
#   KAIRON_SCALE_MIGRATE                  how many to move to another node (default 0)
#   KAIRON_SCALE_TOLERATE_TAINTS          comma list of taint keys to tolerate, e.g.
#                                         kairon.zyvor.dev/vm-only (tainted nodes are
#                                         otherwise skipped)
#   KAIRON_HW_NAMESPACE, KAIRON_SCALE_TIMEOUT (seconds, default 900)
#
# Every Machine carries label kairon.zyvor.dev/scale-smoke=<run>, a 1 hour TTL
# and 1 vCPU / 256 MiB, so a killed run cannot leak them for long.
set -euo pipefail

if [[ "${KAIRON_HW_LAB:-}" != "1" ]]; then
  echo "refusing: set KAIRON_HW_LAB=1 to acknowledge a real lab" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

MAX="${KAIRON_SCALE_MAX:-20}"
BATCH="${KAIRON_SCALE_BATCH:-4}"
HOSTS="${KAIRON_SCALE_HOSTS:-}"
MIN_FREE="${KAIRON_SCALE_MIN_FREE_MIB:-3072}"
MIGRATE="${KAIRON_SCALE_MIGRATE:-0}"
NS="${KAIRON_HW_NAMESPACE:-default}"
TIMEOUT="${KAIRON_SCALE_TIMEOUT:-900}"
RUN="s$(date +%m%d%H%M%S)"
LABEL="kairon.zyvor.dev/scale-smoke"
IMG_URL="https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img"
IMG_DIGEST="sha256:7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b"

machine_json() { # NAME
  python3 - "$1" "$NS" "$RUN" "$LABEL" "$IMG_URL" "$IMG_DIGEST" "${KAIRON_SCALE_TOLERATE_TAINTS:-}" <<'PY'
import json, sys
name, ns, run, label, url, digest, taints = sys.argv[1:8]
tol = [{"key": k, "operator": "Exists", "effect": "NoSchedule"} for k in taints.split(",") if k]
spec = {
    "image": {"source": {"httpURL": url, "format": "qcow2"}, "digest": digest},
    "resources": {"cpu": "1", "memory": "256Mi"},
    "runtime": {"backend": "qemu"},
    "network": {"mode": "user"},
    "placement": {"architecture": "amd64"},
    "powerState": "Running",
    "ttlSeconds": 3600,
}
if tol:
    spec["placement"]["tolerations"] = tol
print(json.dumps({
    "apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
    "metadata": {"name": name, "namespace": ns, "labels": {label: run}},
    "spec": spec,
}))
PY
}

list_run() { kube_curl GET "$LAB_API/namespaces/$NS/machines?labelSelector=$LABEL%3D$RUN"; }

summary() { # prints "running total nodeCounts"
  list_run | python3 -c '
import json, sys, collections
items = json.load(sys.stdin).get("items", [])
run = sum(1 for m in items if (m.get("status") or {}).get("phase") == "Running")
nodes = collections.Counter((m.get("status") or {}).get("nodeName", "?") for m in items)
print(run, len(items), ",".join(f"{k}={v}" for k, v in sorted(nodes.items())))'
}

cleanup() {
  lab_log "deleting run $RUN"
  kube_curl DELETE "$LAB_API/namespaces/$NS/machines?labelSelector=$LABEL%3D$RUN" >/dev/null 2>&1 || true
}
trap cleanup EXIT

min_free_mib() { # smallest MemAvailable (MiB) across the watched hosts; empty when none
  local h v min=""
  [[ -n "$HOSTS" ]] || return 0
  IFS=, read -ra hs <<<"$HOSTS"
  for h in "${hs[@]}"; do
    v="$(lab_ssh "$h" "awk '/MemAvailable/{print int(\$2/1024)}' /proc/meminfo" 2>/dev/null || true)"
    [[ -n "$v" ]] && { [[ -z "$min" || "$v" -lt "$min" ]] && min="$v"; }
  done
  echo "$min"
}

lab_log "ramping to at most $MAX Machines in batches of $BATCH (run $RUN, memory floor ${MIN_FREE} MiB)"
start=$(date +%s)
COUNT=0
STOP_REASON=""
deadline=$(( start + TIMEOUT ))
while (( COUNT < MAX )); do
  want=$(( COUNT + BATCH )); (( want > MAX )) && want=$MAX
  for i in $(seq "$((COUNT + 1))" "$want"); do
    kube_curl POST "$LAB_API/namespaces/$NS/machines" "$(machine_json "scale-$RUN-$(printf '%03d' "$i")")" >/dev/null
  done
  batch_deadline=$(( $(date +%s) + 300 ))
  while :; do
    read -r running total nodes < <(summary)
    lab_log "running $running/$want ($nodes)"
    [[ "$running" == "$want" ]] && break
    if (( $(date +%s) >= batch_deadline )); then
      STOP_REASON="batch to $want did not reach Running in 300s (running $running)"
      break 2
    fi
    sleep 8
  done
  COUNT=$want
  free="$(min_free_mib)"
  if [[ -n "$free" ]]; then
    lab_log "lowest free memory across watched hosts: ${free} MiB"
    if (( free < MIN_FREE )); then
      STOP_REASON="a host is below the ${MIN_FREE} MiB free-memory floor (${free} MiB)"
      break
    fi
  fi
done
read -r running total nodes < <(summary)
all_running=$(( $(date +%s) - start ))
lab_log "held $running Machines Running ($nodes) after ${all_running}s${STOP_REASON:+; stopped: $STOP_REASON}"
COUNT=$running

if (( MIGRATE > 0 )); then
  lab_log "migrating $MIGRATE Machines"
  mig_start=$(date +%s)
  names=()
  for i in $(seq 1 "$MIGRATE"); do
    n="scale-$RUN-$(printf '%03d' "$i")"
    names+=("$n")
    "$KAIRONCTL" -n "$NS" migrate "$n" --strategy cold >/dev/null
  done
  for n in "${names[@]}"; do
    until machine_healthy "$NS" "$n"; do
      (( $(date +%s) >= deadline )) && { lab_log "FAIL: $n not healthy after migration"; exit 1; }
      sleep 5
    done
  done
  lab_log "migrated $MIGRATE in $(( $(date +%s) - mig_start ))s"
fi

cleanup
trap - EXIT
for _ in $(seq 1 60); do
  read -r _ total _ < <(summary)
  [[ "$total" == "0" ]] && { lab_log "DONE: $COUNT Machines held Running, then deleted${STOP_REASON:+ (stopped at: $STOP_REASON)}"; echo "scale-smoke,held=$COUNT,max=$MAX,elapsed=${all_running}s,stopped=${STOP_REASON:-reached max},deleted=ok"; exit 0; }
  sleep 5
done
lab_log "FAIL: Machines still present after delete"
exit 1
