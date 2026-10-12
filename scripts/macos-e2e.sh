#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# End-to-end check of Kairon on a Mac: this Mac registers as a Node, a Machine with backend "vz" is scheduled to it,
# kairon-node starts it through FluxVM's Apple Virtualization.framework backend, and it is reachable over SSH.
#
#   KUBECONFIG=/path/to/kubeconfig scripts/macos-e2e.sh /path/to/arm64-debian.raw
#
# Needs: Apple silicon, Go, Rust, the Xcode command line tools, kubectl, and a Kubernetes API (any cluster; Velora's k3s works).
#        ../fluxvm on the branch with the vz backend (override with FLUXVM_DIR).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FLUXVM_DIR="${FLUXVM_DIR:-$ROOT/../fluxvm}"
IMG="${1:?usage: macos-e2e.sh /path/to/raw-arm64-linux-disk.raw}"
[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || { echo "Apple silicon macOS only" >&2; exit 2; }
T="$(mktemp -d "${TMPDIR:-/tmp}/kairon-e2e.XXXXXX")"
PIDS=()
cleanup() {
  kubectl delete machine mac-e2e -n default --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for p in "${PIDS[@]:-}"; do [[ -n "$p" ]] && kill "$p" 2>/dev/null || true; done
  pkill -f "fluxvm-vz-runner run --config $T" 2>/dev/null || true
  kubectl delete node "$NODE" --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$T"
}
trap cleanup EXIT
ok() { echo "ok   $*"; }; bad() { echo "FAIL $*"; for f in "$T"/*.log; do echo "--- $f"; tail -8 "$f"; done; exit 1; }
NODE="$(scutil --get LocalHostName 2>/dev/null | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-\n' '-' )"; NODE="${NODE:-mac}"
PORT="${FLUXVM_E2E_PORT:-7798}"

kubectl version -o json >/dev/null 2>&1 || bad "no Kubernetes API reachable through KUBECONFIG=${KUBECONFIG:-unset}"
ok "Kubernetes API reachable ($(kubectl version -o json | python3 -c 'import sys,json;print(json.load(sys.stdin)["serverVersion"]["gitVersion"])'))"

(cd "$ROOT" && go build -o "$T/kairon-controller" ./cmd/kairon-controller && go build -o "$T/kairon-node" ./cmd/kairon-node)
(cd "$FLUXVM_DIR" && cargo build -p fluxctl -j 4 >/dev/null 2>&1) || bad "could not build FluxVM"
ok "built kairon-controller, kairon-node and FluxVM on darwin"

kubectl apply --server-side -f "$ROOT/deploy/crd.yaml" >/dev/null && kubectl apply -f "$ROOT/deploy/rbac.yaml" -f "$ROOT/deploy/macos/rbac.yaml" >/dev/null
ok "CRDs and RBAC applied"
API="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > "$T/ca.pem"
TOKEN_C="$(kubectl -n kairon-system create token kairon-controller --duration=2h)"
TOKEN_N="$(kubectl -n kairon-system create token kairon-node --duration=2h)"

cat > "$T/fluxvm.toml" <<EOT
listen = "127.0.0.1:$PORT"
state_dir = "$T/fluxvm-state"
run_dir = "/tmp/fluxvm-run-e2e"
EOT
"$FLUXVM_DIR/target/debug/fluxctl" --config "$T/fluxvm.toml" serve > "$T/fluxvm.log" 2>&1 & PIDS+=($!)
for _ in $(seq 1 30); do curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null && ok "FluxVM daemon is up on macOS" || bad "FluxVM did not start"

export KAIRON_KUBE_URL="$API" KAIRON_KUBE_CA="$T/ca.pem"
KAIRON_KUBE_TOKEN="$TOKEN_C" "$T/kairon-controller" --interval=3s --health-addr=127.0.0.1:32391 > "$T/controller.log" 2>&1 & PIDS+=($!)
NODE_NAME="$NODE" KAIRON_KUBE_TOKEN="$TOKEN_N" "$T/kairon-node" --interval=3s --health-addr=127.0.0.1:32392 \
  --fluxvm-url="http://127.0.0.1:$PORT" --image-root="$(dirname "$IMG")" --console-addr= > "$T/node.log" 2>&1 & PIDS+=($!)

for _ in $(seq 1 30); do kubectl get node "$NODE" >/dev/null 2>&1 && break; sleep 1; done
kubectl get node "$NODE" >/dev/null 2>&1 || bad "this Mac did not register as a Node"
for _ in $(seq 1 20); do [[ "$(kubectl get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] && break; sleep 1; done
[[ "$(kubectl get node "$NODE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] && ok "Node $NODE is Ready ($(kubectl get node "$NODE" -o jsonpath='{.status.allocatable.cpu} CPU, {.status.allocatable.memory}'))" || bad "node not Ready"
[[ "$(kubectl get node "$NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/arch}')" == arm64 ]] && ok "labels: arch=arm64, backend.vz, capable" || bad "labels"

ssh-keygen -q -t ed25519 -N "" -f "$T/key"; PUB="$(cat "$T/key.pub")"
kubectl apply -f - >/dev/null <<EOM
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: mac-e2e, namespace: default}
spec:
  image: {path: "$IMG"}
  resources: {cpu: "2", memory: 2Gi}
  runtime: {backend: vz}
  powerState: Running
  cloudInit: {hostname: mac-e2e, user: velora, sshAuthorizedKeys: ["$PUB"]}
  placement:
    architecture: arm64
    nodeSelector: {kairon.zyvor.dev/backend.vz: "true"}
    tolerations: [{key: kairon.zyvor.dev/vm-only, operator: Exists, effect: NoSchedule}]
EOM
phase() { kubectl get machine mac-e2e -o jsonpath="{.status.$1}" 2>/dev/null || true; }
for _ in $(seq 1 60); do [[ "$(phase nodeName)" == "$NODE" ]] && break; sleep 2; done
[[ "$(phase nodeName)" == "$NODE" ]] && ok "the controller scheduled the Machine to this Mac" || bad "not scheduled (phase=$(phase phase) message=$(phase message))"
for _ in $(seq 1 90); do [[ "$(phase phase)" == Running ]] && break; sleep 3; done
[[ "$(phase phase)" == Running ]] && ok "Machine phase Running (FluxVM runtime $(phase runtimeID))" || bad "phase $(phase phase): $(phase message)"
IP=""; for _ in $(seq 1 60); do IP="$(phase guestIP)"; [[ -n "$IP" ]] && break; sleep 3; done
[[ -n "$IP" ]] && ok "Machine status reports the guest address $IP" || bad "no guestIP in status"
for _ in $(seq 1 30); do ssh -i "$T/key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -o ConnectTimeout=6 "velora@$IP" hostname >/dev/null 2>&1 && break; sleep 3; done
[[ "$(ssh -i "$T/key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes "velora@$IP" hostname)" == mac-e2e ]] && ok "SSH into the guest works (cloud-init applied)" || bad "ssh"

kubectl delete machine mac-e2e -n default --wait=true --timeout=120s >/dev/null
sleep 5
VMS_JSON="$T/vms.json"
curl -fs -o "$VMS_JSON" "http://127.0.0.1:$PORT/v1/vms" || true
[[ "$(python3 -c 'import sys,json;print(len(json.load(open(sys.argv[1]))["items"]))' "$VMS_JSON" 2>/dev/null)" == 0 ]] && ok "deleting the Machine removed the VM from FluxVM" || bad "VM still present"
echo "PASS"
