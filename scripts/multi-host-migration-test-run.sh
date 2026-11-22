#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Drives one real live migration between the two hosts deployed by
# multi-host-migration-test-deploy.sh, then polls until it reaches a
# terminal phase. See docs/runbook-multi-host-migration-test.md.
#
# Usage:
#   KAIRON_KUBE_URL=... KAIRON_KUBE_TOKEN=... \
#   multi-host-migration-test-run.sh MACHINE_NAME TARGET_NODE_NAME [--namespace=ns] [--migration-network=NAME=IP] [--timeout=600]
#
# kaironctl (built by `make build`, or already on PATH) reads
# KAIRON_KUBE_URL/KAIRON_KUBE_TOKEN/KAIRON_KUBE_CA the same way kairon-node
# does -- see internal/kube.Client.FromEnvironment.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KAIRONCTL="${KAIRONCTL:-$REPO_ROOT/bin/kaironctl}"
[[ -x "$KAIRONCTL" ]] || KAIRONCTL="kaironctl"

if [[ $# -lt 2 ]]; then
  echo "usage: $0 MACHINE_NAME TARGET_NODE_NAME [--namespace=ns] [--migration-network=NAME=IP] [--timeout=600]" >&2
  exit 1
fi

MACHINE="$1"; TARGET_NODE="$2"; shift 2
NAMESPACE="default"
MIGRATION_NETWORK=""
TIMEOUT=600
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --migration-network=*) MIGRATION_NETWORK="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done

MIG_NAME="mhtest-$MACHINE-$(date -u +%Y%m%d-%H%M%S)"
migrate_args=("$MACHINE" --namespace="$NAMESPACE" --name="$MIG_NAME" --strategy="${KAIRON_MIGRATE_STRATEGY:-live}" --target-node="$TARGET_NODE")
[[ -n "$MIGRATION_NETWORK" ]] && migrate_args+=(--migration-network="$MIGRATION_NETWORK")

echo "[*] before: $MACHINE's current phase/node"
"$KAIRONCTL" get machines --namespace="$NAMESPACE" | (head -1; grep -w "^$MACHINE" || true)

echo "[*] creating live migration $MIG_NAME"
"$KAIRONCTL" migrate "${migrate_args[@]}"

echo "[*] polling machinemigration/$MIG_NAME (timeout ${TIMEOUT}s) -- Ctrl-C to stop polling without aborting the migration"
deadline=$(( $(date +%s) + TIMEOUT ))
last_phase=""
while :; do
  row="$("$KAIRONCTL" get migrations --namespace="$NAMESPACE" | awk -F'\t' -v n="$MIG_NAME" '$1==n')"
  phase="$(awk -F'\t' '{print $NF}' <<<"$row")"
  if [[ "$phase" != "$last_phase" ]]; then
    echo "    $(date -u +%H:%M:%S)  phase=$phase"
    last_phase="$phase"
  fi
  case "$phase" in
    Succeeded)
      echo "[+] migration succeeded"
      break
      ;;
    Failed|Blocked|NeedsRecovery)
      echo "[!] migration landed in $phase -- see docs/runbook-migration-failures.md" >&2
      exit 1
      ;;
  esac
  if (( $(date +%s) >= deadline )); then
    echo "[!] timed out after ${TIMEOUT}s waiting for a terminal phase (last observed: $phase)" >&2
    exit 1
  fi
  sleep 3
done

echo "[*] after: $MACHINE's current phase/node"
"$KAIRONCTL" get machines --namespace="$NAMESPACE" | (head -1; grep -w "^$MACHINE" || true)

echo
echo "[+] now run the manual verification checklist in"
echo "    docs/runbook-multi-host-migration-test.md (guest IP/MAC continuity,"
echo "    RAM-transferred cross-check against FluxVM's own status API, events)."
