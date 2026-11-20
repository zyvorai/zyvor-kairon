#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Controller failover mid-migration: starts a live migration, waits for
# Running, then kills kairon-controller -- either by restarting its systemd
# unit over SSH (--controller-ssh) or by deleting its Pods through the
# Kubernetes API (default, namespace --controller-namespace). The migration
# must still finish Succeeded with the Machine healthy on the target: all
# migration state lives in the MachineMigration object, not controller memory.
#
# Usage:
#   lab-inject-controller-failover.sh --machine=NAME --target=NODE [--namespace=ns]
#       [--controller-ssh=user@host] [--controller-unit=kairon-controller]
#       [--controller-namespace=kairon-system] [--timeout=900]
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
MACHINE=""; TARGET=""; TIMEOUT=900
CTRL_SSH="${KAIRON_HW_CONTROLLER_SSH:-}"
CTRL_UNIT="${KAIRON_HW_CONTROLLER_UNIT:-kairon-controller}"
CTRL_NS="${KAIRON_HW_CONTROLLER_NAMESPACE:-kairon-system}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --machine=*) MACHINE="${arg#*=}" ;;
    --target=*) TARGET="${arg#*=}" ;;
    --controller-ssh=*) CTRL_SSH="${arg#*=}" ;;
    --controller-unit=*) CTRL_UNIT="${arg#*=}" ;;
    --controller-namespace=*) CTRL_NS="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$MACHINE" && -n "$TARGET" ]] || { echo "usage: $0 --machine=NAME --target=NODE" >&2; exit 1; }

SOURCE="$(machine_node "$NAMESPACE" "$MACHINE")"
[[ -n "$SOURCE" && "$SOURCE" != "$TARGET" ]] || { echo "Machine $MACHINE is on '${SOURCE:-<none>}'; pick a different --target" >&2; exit 1; }

kill_controller() {
  if [[ -n "$CTRL_SSH" ]]; then
    lab_log "restarting $CTRL_UNIT on $CTRL_SSH"
    lab_ssh "$CTRL_SSH" "sudo systemctl restart $CTRL_UNIT"
  else
    lab_log "deleting kairon-controller Pods in $CTRL_NS"
    kube_curl DELETE "api/v1/namespaces/$CTRL_NS/pods?labelSelector=app.kubernetes.io%2Fname%3Dkairon-controller" >/dev/null
  fi
}

MIG="$(start_migration "$NAMESPACE" "$MACHINE" "$TARGET" live ctrlfail)"
lab_log "migration $MIG: $MACHINE $SOURCE -> $TARGET"
wait_until "$TIMEOUT" "$MIG Running" migration_in "$NAMESPACE" "$MIG" Running Failed Blocked Succeeded
[[ "$(migration_phase "$NAMESPACE" "$MIG")" == "Running" ]] || { lab_log "migration never reached Running"; exit 1; }

kill_controller
wait_until "$TIMEOUT" "$MIG terminal" migration_in "$NAMESPACE" "$MIG" Succeeded Failed Cancelled NeedsRecovery
phase="$(migration_phase "$NAMESPACE" "$MIG")"
[[ "$phase" == "Succeeded" ]] || { lab_log "FAIL: migration landed $phase after controller failover"; exit 1; }
wait_until 300 "$MACHINE healthy on $TARGET" machine_healthy "$NAMESPACE" "$MACHINE" "$TARGET"
lab_log "PASS: migration Succeeded across a controller restart"
