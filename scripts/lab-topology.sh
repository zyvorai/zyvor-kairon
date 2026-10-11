#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Topology-aware placement drill (experimental feature; see
# docs/guides/machine-topology.md). On --node, it checks:
#
#   nodetopology   kairon-node published a NodeTopology with numaBinding
#   single-numa    a single-numa-node Machine runs with one guest NUMA node
#                  whose memory policy is bind and whose vCPU threads run
#                  only on the allocated CPUs (status.appliedTopology)
#   unsatisfiable  a single-numa-node Machine no NUMA node can hold stays
#                  Pending with reason TopologyUnsatisfiable: more vCPUs
#                  than any NUMA node has (two or more NUMA nodes), or a GPU
#                  the node does not offer (one NUMA node, where the plain
#                  CPU capacity check would refuse first)
#   multi-numa     a restricted Machine larger than one NUMA node spans
#                  two, both bound (needs a host with two NUMA nodes)
#   gpu-nic        a GPU and a NIC under one PCIe root, samePcieRoot
#                  (needs both allowlisted in KAIRON_VFIO_ALLOWLIST)
#   sriov          an SR-IOV VF on the Machine's NUMA node (needs an
#                  allowlisted VF)
#
# and prints COMPATIBILITY.md rows. Cases the node's hardware cannot run
# are reported "not run" with the reason.
#
# Usage:
#   lab-topology.sh --node=NAME --image=PATH [--namespace=ns] [--timeout=600]
#
# Needs KAIRON_KUBE_* (lib/lab.sh). The node needs the
# kairon.zyvor.dev/pinnable-cpus label (or discovery) and a FluxVM that
# reports host.topology. The Machines it creates are deleted on exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600
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

STAMP="$(date -u +%m%d%H%M%S)"
CREATED=()
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

cleanup() {
  local m
  for m in "${CREATED[@]}"; do kube_curl DELETE "$(machine_path "$NAMESPACE" "$m")" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

topology() { kube_curl GET "$LAB_API/nodetopologies/$NODE"; }

# create_machine NAME CPU MEMORY POLICY [DEVICES_JSON]
create_machine() {
  local body
  body="$(python3 - "$1" "$2" "$3" "$4" "$NODE" "$IMAGE" "${5:-[]}" <<'EOF'
import json, sys
name, cpu, mem, policy, node, image, devices = sys.argv[1:8]
spec = {
    "image": {"path": image},
    "resources": {"cpu": cpu, "memory": mem, "cpuPinning": True},
    "topology": {"policy": policy},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "powerState": "Running",
}
if json.loads(devices):
    spec["devices"] = json.loads(devices)
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "topology"}}, "spec": spec}))
EOF
)"
  kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null
  CREATED+=("$1")
}

applied() { machine_field "$NAMESPACE" "$1" 'json.dumps((o.get("status") or {}).get("appliedTopology") or {})'; }
has_applied() { [[ "$(machine_field "$NAMESPACE" "$1" 'len(((o.get("status") or {}).get("appliedTopology") or {}).get("numaNodes") or [])' 2>/dev/null)" -gt 0 ]]; }
scheduled_reason() {
  machine_field "$NAMESPACE" "$1" 'next(((c.get("reason") or "") for c in ((o.get("status") or {}).get("conditions") or []) if c.get("type") == "Scheduled"), "")'
}
is_unsatisfiable() { [[ "$(scheduled_reason "$1" 2>/dev/null)" == TopologyUnsatisfiable ]]; }

# check_bound MACHINE WANT_NODES -- appliedTopology has WANT_NODES guest
# nodes, each policy bind, each vCPU thread confined to its allocated CPUs.
check_bound() {
  local m="$1" want="$2" alloc got
  alloc="$(machine_field "$NAMESPACE" "$m" 'json.dumps((o.get("spec") or {}).get("topologyAllocation") or {})')"
  got="$(applied "$m")"
  python3 - "$alloc" "$got" "$want" <<'EOF'
import json, sys
alloc, got, want = json.loads(sys.argv[1]), json.loads(sys.argv[2]), int(sys.argv[3])
a, g = alloc.get("numaNodes") or [], got.get("numaNodes") or []
if len(a) != want or len(g) != want:
    sys.exit(f"want {want} NUMA nodes, allocated {len(a)}, applied {len(g)}")
for i, (x, y) in enumerate(zip(a, g)):
    if y.get("policy") != "bind":
        sys.exit(f"guest node {i} memory policy {y.get('policy')!r}")
    if y.get("hostNode") != x.get("id") or y.get("hostCpus") != x.get("cpus"):
        sys.exit(f"guest node {i}: applied host node {y.get('hostNode')} cpus {y.get('hostCpus')}, allocated {x.get('id')} cpus {x.get('cpus')}")
if got.get("message"):
    sys.exit(got["message"])
print("; ".join(f"guest{i} -> numa{y['hostNode']} cpus {y['hostCpus']} {y['memoryMiB']}MiB bind" for i, y in enumerate(g)))
EOF
}

machine_gone() { local rc=0; kube_exists "$(machine_path "$NAMESPACE" "$1")" || rc=$?; [[ $rc == 1 ]]; }
delete_machine() {
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$1")" >/dev/null 2>&1 || true
  wait_until 300 "$1 deleted" machine_gone "$1" || true
}

# run_bound CASE NAME CPU MEMORY POLICY WANT_NODES [DEVICES_JSON]
run_bound() {
  local case="$1" name="$2" out
  create_machine "$name" "$3" "$4" "$5" "${7:-[]}"
  if ! wait_until "$TIMEOUT" "$name Running" machine_healthy "$NAMESPACE" "$name"; then
    result "$case" fail "$name never became healthy: $(machine_field "$NAMESPACE" "$name" '(o.get("status") or {}).get("message") or ""' 2>/dev/null)"
  else
    wait_until 120 "$name appliedTopology" has_applied "$name" || true
    if out="$(check_bound "$name" "$6" 2>&1)"; then
      result "$case" pass "$out"
    else
      result "$case" fail "$out"
    fi
  fi
  delete_machine "$name"
}

NT="$(topology 2>/dev/null || true)"
if [[ -z "$NT" ]]; then
  result nodetopology fail "no NodeTopology $NODE (kairon-node not publishing, or FluxVM without host.topology)"
else
  binding="$(json_eval '(o.get("status") or {}).get("numaBinding") or False' <<<"$NT")"
  numa="$(json_eval 'len((o.get("status") or {}).get("numaNodes") or [])' <<<"$NT")"
  devs="$(json_eval 'len((o.get("status") or {}).get("devices") or [])' <<<"$NT")"
  if [[ "$binding" == True && "$numa" -ge 1 ]]; then
    result nodetopology pass "$numa NUMA node(s), $devs PCI device(s), numaBinding true"
  else
    result nodetopology fail "numaBinding $binding, $numa NUMA node(s)"
  fi
fi

if [[ "${RESULT[nodetopology]}" == pass ]]; then
  # Largest number of pinnable CPUs on one NUMA node.
  pinnable="$(kube_curl GET "api/v1/nodes/$NODE" | json_eval '(o["metadata"].get("labels") or {}).get("kairon.zyvor.dev/pinnable-cpus") or ""')"
  read -r max_numa_cpus numa_nodes <<<"$(python3 - "$NT" "$pinnable" <<'EOF'
import json, sys
def cpus(s):
    out = set()
    for part in s.replace("_", ",").split(","):
        if not part:
            continue
        lo, _, hi = part.partition("-")
        out.update(range(int(lo), int(hi or lo) + 1))
    return out
nt, pin = json.loads(sys.argv[1]), cpus(sys.argv[2])
per = [len(cpus(n["cpus"]) & pin) for n in nt["status"].get("numaNodes") or []]
print(max(per or [0]), len(per))
EOF
)"
  lab_log "node $NODE: $numa_nodes NUMA node(s), at most $max_numa_cpus pinnable CPUs on one"

  if (( max_numa_cpus >= 2 )); then
    run_bound single-numa "topo-lab-single-$STAMP" 2 1Gi single-numa-node 1
  else
    result single-numa fail "fewer than 2 pinnable CPUs on any NUMA node (label kairon.zyvor.dev/pinnable-cpus: '${pinnable}')"
  fi

  big="topo-lab-unsat-$STAMP"
  if (( numa_nodes >= 2 )); then
    what="$((max_numa_cpus + 1)) vCPUs"
    create_machine "$big" "$((max_numa_cpus + 1))" 1Gi single-numa-node
  else
    what="2 vCPUs and an NVIDIA GPU"
    create_machine "$big" 2 1Gi single-numa-node '[{"name":"gpu","class":"gpu","vendor":"nvidia"}]'
  fi
  if wait_until 120 "$big TopologyUnsatisfiable" is_unsatisfiable "$big"; then
    msg="$(machine_field "$NAMESPACE" "$big" '(o.get("status") or {}).get("message") or ""')"
    phase="$(machine_phase "$NAMESPACE" "$big")"
    if [[ "$phase" == Pending && -z "$(machine_node "$NAMESPACE" "$big")" ]]; then
      result unsatisfiable pass "$what: Pending, $msg"
    else
      result unsatisfiable fail "reason TopologyUnsatisfiable but phase $phase, node $(machine_node "$NAMESPACE" "$big")"
    fi
  else
    result unsatisfiable fail "Scheduled reason $(scheduled_reason "$big" 2>/dev/null || echo '?'): $(machine_field "$NAMESPACE" "$big" '(o.get("status") or {}).get("message") or ""' 2>/dev/null)"
  fi
  delete_machine "$big"

  if (( numa_nodes >= 2 )); then
    run_bound multi-numa "topo-lab-multi-$STAMP" "$((max_numa_cpus + 2))" 2Gi restricted 2
  else
    result multi-numa "not run" "host has $numa_nodes NUMA node"
  fi

  pair="$(python3 - "$NT" <<'EOF'
import json, sys
devs = json.loads(sys.argv[1])["status"].get("devices") or []
ok = lambda d: d.get("allocatable") and not d.get("allocated")
gpus = {d["pcieRoot"] for d in devs if d["class"] == "gpu" and ok(d) and not d.get("physfn")}
nics = {d["pcieRoot"] for d in devs if d["class"] == "nic" and ok(d)}
print("yes" if gpus & nics else "")
EOF
)"
  if [[ -n "$pair" ]]; then
    run_bound gpu-nic "topo-lab-gpunic-$STAMP" 2 2Gi single-numa-node 1 \
      '[{"name":"gpu","class":"gpu","alignment":["samePcieRoot"]},{"name":"nic","class":"nic","alignment":["samePcieRoot"]}]'
  else
    result gpu-nic "not run" "no allocatable GPU and NIC under one PCIe root"
  fi

  vf="$(json_eval 'next((d["bdf"] for d in (o.get("status") or {}).get("devices") or [] if d.get("physfn") and d.get("allocatable") and not d.get("allocated")), "")' <<<"$NT")"
  if [[ -n "$vf" ]]; then
    run_bound sriov "topo-lab-sriov-$STAMP" 2 2Gi single-numa-node 1 '[{"name":"vf","class":"nic","virtualFunction":true,"alignment":["sameNuma"]}]'
  else
    result sriov "not run" "no allocatable SR-IOV VF"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in nodetopology single-numa unsatisfiable multi-numa gpu-nic sriov; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Topology: $c | $r | $today | Node $NODE, ${NOTE[$c]:-earlier step failed} |"
done
exit "$rc"
