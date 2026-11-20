#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Restore a backup made by scripts/backup-crds.sh into the current
# kubectl context's cluster. Read docs/runbook-backup-restore.md first --
# in particular, whether this gets you working VMs back (not just
# Kubernetes objects) depends entirely on whether the hosts/FluxVM
# runtimes named in the backup are still alive; this script has no way to
# check that for you.
#
# Usage: restore-crds.sh BACKUP.tar.gz [--yes]
#   Without --yes, prints what would be applied (kubectl apply --dry-run=client)
#   and exits without changing anything -- this is deliberately not the
#   default action given how easily a restore into the wrong cluster/
#   context could go wrong.
set -euo pipefail

BACKUP="${1:-}"
CONFIRM="${2:-}"
if [ -z "$BACKUP" ] || [ ! -f "$BACKUP" ]; then
  echo "usage: $0 BACKUP.tar.gz [--yes]" >&2
  exit 1
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
tar -xzf "$BACKUP" -C "$WORKDIR"
BACKUP_DIR="$(find "$WORKDIR" -maxdepth 1 -mindepth 1 -type d | head -1)"
if [ -z "$BACKUP_DIR" ] || [ ! -d "$BACKUP_DIR/crds" ]; then
  echo "$BACKUP does not look like a scripts/backup-crds.sh archive (no crds/ directory found)" >&2
  exit 1
fi

echo "restoring into: $(kubectl config current-context)"
echo "backup manifest:"
cat "$BACKUP_DIR/manifest.txt"
echo

DRY_RUN_FLAGS=()
if [ "$CONFIRM" != "--yes" ]; then
  echo "=== dry run (pass --yes to actually apply) ==="
  DRY_RUN_FLAGS=(--dry-run=client)
fi

for f in "$BACKUP_DIR"/crds/*.yaml; do
  # backup-crds.sh writes a `kind: List` with `items: []` for a CRD kind
  # with no real objects (every kind, on a fresh/empty cluster) --
  # `kubectl apply` refuses an empty list outright ("no objects passed to
  # apply"), so skip those rather than letting that abort the whole
  # restore.
  if grep -q '^items: \[\]$' "$f"; then
    continue
  fi
  kubectl apply -f "$f" "${DRY_RUN_FLAGS[@]}"
done

if [ "$CONFIRM" != "--yes" ]; then
  exit 0
fi

if [ -d "$BACKUP_DIR/secrets" ]; then
  echo "note: $BACKUP_DIR/secrets was not restored automatically -- review and apply those manually (they contain live credentials, see docs/runbook-backup-restore.md)." >&2
fi

echo "restore applied. Machines with spec.nodeName pointing at a host whose FluxVM runtime is still alive will be re-adopted by kairon-node on its next reconcile tick (matched by name, not status.runtimeID -- see the runbook). Machines whose runtime is gone will have a fresh VM created from spec.image/spec.resources on the next tick, same as any new Machine."
