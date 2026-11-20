#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Reverses multi-host-migration-test-deploy.sh: uninstalls kairon-node (+
# kairon-controller) via deploy-remote.sh --uninstall (unmodified), then
# removes the real kairon-migration-adapter-fluxvm binary/unit/certs this
# script's deploy counterpart installed by hand (deploy-remote.sh itself
# has no knowledge of them). See docs/runbook-multi-host-migration-test.md.
#
# Usage:
#   multi-host-migration-test-teardown.sh HOST_A_USER@HOST_A_IP HOST_B_USER@HOST_B_IP [--purge]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ $# -lt 2 ]]; then
  echo "usage: $0 USER@HOST_A USER@HOST_B [--purge]" >&2
  exit 1
fi

HOST_A="$1"; HOST_B="$2"; shift 2
PURGE_FLAG=()
[[ "${1:-}" == "--purge" ]] && PURGE_FLAG=(--purge)

for target in "$HOST_A" "$HOST_B"; do
  echo "=== [$target] removing kairon-migration-adapter-fluxvm ==="
  ssh "$target" <<'REMOTE_EOF'
sudo systemctl stop kairon-migration-adapter-fluxvm.service 2>/dev/null || true
sudo systemctl disable kairon-migration-adapter-fluxvm.service 2>/dev/null || true
sudo rm -f /etc/systemd/system/kairon-migration-adapter-fluxvm.service /usr/bin/kairon-migration-adapter-fluxvm
sudo rm -f /etc/kairon/migration/adapter-ca.pem /etc/kairon/migration/adapter-cert.pem /etc/kairon/migration/adapter-key.pem
sudo systemctl daemon-reload
echo "removed kairon-migration-adapter-fluxvm"
REMOTE_EOF

  echo "=== [$target] uninstalling kairon-node/kaironctl/kairon-controller via deploy-remote.sh ==="
  "$REPO_ROOT/scripts/deploy-remote.sh" "$target" --uninstall "${PURGE_FLAG[@]}"
done

echo "[+] teardown complete on both hosts."
