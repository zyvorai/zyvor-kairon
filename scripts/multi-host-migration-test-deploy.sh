#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Deploys kairon-node (+ kairon-controller on HOST_A) via the existing,
# UNMODIFIED scripts/deploy-remote.sh, then closes the one real gap it
# has today: it has no support for the real, FluxVM-backed
# kairon-migration-adapter-fluxvm binary (only --with-migration-adapter-stub,
# a test double). This script cross-compiles that binary itself, installs
# systemd/kairon-migration-adapter-fluxvm.service, and stages the
# per-host data-plane certs from scripts/gen-migration-mtls-certs.sh's
# output layout.
#
# See docs/runbook-multi-host-migration-test.md for the full walkthrough.
#
# Usage:
#   multi-host-migration-test-deploy.sh CERTS_DIR HOST_A_USER@HOST_A_IP=HOST_A_NAME HOST_B_USER@HOST_B_IP=HOST_B_NAME [flags]
#
# CERTS_DIR is scripts/gen-migration-mtls-certs.sh's OUT_DIR (must already
# contain control-plane/{ca,cert,key}.pem and, per host,
# data-plane/<HOST_NAME>-{ca,cert,key}.pem).
#
# Flags (all forwarded to deploy-remote.sh for BOTH hosts):
#   --kube-url=URL --kube-token=TOKEN --kube-ca=PATH --kube-insecure
#   --fluxvm-url=URL (default http://127.0.0.1:7788, same on both hosts)
#   --arch=amd64|arm64 (skips the remote 'uname -m' probe on BOTH hosts;
#     omit if the two hosts have different architectures -- the probe
#     runs per host in that case)
#
# HOST_A additionally gets --with-controller (runs kairon-controller).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ $# -lt 3 ]]; then
  echo "usage: $0 CERTS_DIR USER@IP=HOST_A_NAME USER@IP=HOST_B_NAME [deploy-remote.sh flags...]" >&2
  exit 1
fi

CERTS_DIR="$1"; shift
HOST_A_TARGET="${1%%=*}"; HOST_A_NAME="${1#*=}"; shift
HOST_B_TARGET="${1%%=*}"; HOST_B_NAME="${1#*=}"; shift
EXTRA_FLAGS=("$@")

CP_CA="$CERTS_DIR/control-plane/ca.pem"
CP_CERT="$CERTS_DIR/control-plane/cert.pem"
CP_KEY="$CERTS_DIR/control-plane/key.pem"
for f in "$CP_CA" "$CP_CERT" "$CP_KEY"; do
  [[ -f "$f" ]] || { echo "missing $f -- run scripts/gen-migration-mtls-certs.sh first" >&2; exit 1; }
done

deploy_node() {
  local target="$1" host_name="$2" extra_deploy_flag="$3"
  local dp_ca="$CERTS_DIR/data-plane/$host_name-ca.pem"
  local dp_cert="$CERTS_DIR/data-plane/$host_name-cert.pem"
  local dp_key="$CERTS_DIR/data-plane/$host_name-key.pem"
  for f in "$dp_ca" "$dp_cert" "$dp_key"; do
    [[ -f "$f" ]] || { echo "missing $f for host '$host_name' -- did you pass the right HOST_NAME to gen-migration-mtls-certs.sh?" >&2; exit 1; }
  done

  echo "=== [$host_name] deploying kairon-node via deploy-remote.sh (unmodified) ==="
  "$REPO_ROOT/scripts/deploy-remote.sh" "$target" \
    --migration-ca="$CP_CA" --migration-cert="$CP_CERT" --migration-key="$CP_KEY" \
    --migration-adapter-socket=/run/kairon/migration-adapter.sock \
    --node-name="$host_name" \
    $extra_deploy_flag \
    "${EXTRA_FLAGS[@]}"

  echo "=== [$host_name] building kairon-migration-adapter-fluxvm ==="
  local arch=""
  for a in "${EXTRA_FLAGS[@]}"; do
    [[ "$a" == --arch=* ]] && arch="${a#*=}"
  done
  if [[ -z "$arch" ]]; then
    arch="$(ssh "$target" 'case "$(uname -m)" in x86_64) echo amd64;; aarch64|arm64) echo arm64;; *) echo unknown;; esac')"
  fi
  [[ "$arch" == "unknown" || -z "$arch" ]] && { echo "could not resolve arch for $target; pass --arch=amd64|arm64" >&2; exit 1; }

  local build_dir; build_dir="$(mktemp -d)"
  trap 'rm -rf "$build_dir"' RETURN
  ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
      -o "$build_dir/kairon-migration-adapter-fluxvm" ./cmd/kairon-migration-adapter-fluxvm )

  local advertise_host="${host_name}"
  # Prefer the IP half of USER@IP as the advertise address QEMU peers
  # actually dial, since that's what the data-plane cert's SAN covers
  # (see gen-migration-mtls-certs.sh) -- fall back to host_name if the
  # target wasn't given as user@ip.
  [[ "$target" == *@* ]] && advertise_host="${target#*@}"

  echo "=== [$host_name] staging binary, unit, and data-plane certs ==="
  ssh "$target" 'sudo install -d -m 0750 -o root -g kairon /etc/kairon/migration 2>/dev/null || sudo install -d -m 0750 /etc/kairon/migration'
  scp "$build_dir/kairon-migration-adapter-fluxvm" "$REPO_ROOT/systemd/kairon-migration-adapter-fluxvm.service" \
      "$dp_ca" "$dp_cert" "$dp_key" "$target:/tmp/"
  ssh "$target" bash -s -- "$advertise_host" <<'REMOTE_EOF'
set -euo pipefail
advertise_host="$1"
sudo install -m 0755 -o root -g root /tmp/kairon-migration-adapter-fluxvm /usr/bin/kairon-migration-adapter-fluxvm
for suffix in ca cert key; do
  f="$(ls /tmp/*-$suffix.pem 2>/dev/null | head -1)"
  [[ -n "$f" ]] && sudo install -m 0640 -o root -g kairon "$f" "/etc/kairon/migration/adapter-$suffix.pem"
done
sudo install -m 0644 -o root -g root /tmp/kairon-migration-adapter-fluxvm.service /etc/systemd/system/kairon-migration-adapter-fluxvm.service
sudo sed -i "s#__ADVERTISE_HOST__#$advertise_host#" /etc/systemd/system/kairon-migration-adapter-fluxvm.service
sudo systemctl daemon-reload
sudo systemctl enable --now kairon-migration-adapter-fluxvm.service
rm -f /tmp/kairon-migration-adapter-fluxvm /tmp/kairon-migration-adapter-fluxvm.service /tmp/*-ca.pem /tmp/*-cert.pem /tmp/*-key.pem
echo "kairon-migration-adapter-fluxvm active, advertising $advertise_host"
REMOTE_EOF
}

deploy_node "$HOST_A_TARGET" "$HOST_A_NAME" "--with-controller"
deploy_node "$HOST_B_TARGET" "$HOST_B_NAME" ""

echo
echo "[+] both hosts deployed. Verify with:"
echo "    ssh $HOST_A_TARGET systemctl status kairon-node kairon-controller kairon-migration-adapter-fluxvm"
echo "    ssh $HOST_B_TARGET systemctl status kairon-node kairon-migration-adapter-fluxvm"
echo "    then run scripts/multi-host-migration-test-run.sh"
