#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Back up every Kairon CRD object (cluster-wide, all namespaces) to plain
# YAML -- the Kubernetes-level state a cluster/etcd loss would otherwise
# take with it. Read docs/runbook-backup-restore.md before relying on this:
# it does NOT back up actual VM disk content (your storage backend's/CSI
# driver's own snapshot/backup tooling owns that) or FluxVM's own runtime
# state on each host -- only the Machine/MachineMigration/MachineSnapshot/
# MachineSnapshotRestore/MachineNetworkPolicy/NetworkSecurityGroup/
# MachineDisruptionBudget/MachineQuota objects themselves.
set -euo pipefail

OUT="${1:-kairon-backup-$(date +%Y%m%d%H%M%S)}"
NS="${KAIRON_NAMESPACE:-kairon-system}"
# Off by default: these Secrets hold live credentials (session-signing
# keys, bcrypt hashes, the legacy shared token, an OIDC client secret) --
# see docs/runbook-backup-restore.md's warning before turning this on.
INCLUDE_SECRETS="${KAIRON_BACKUP_SECRETS:-false}"

mkdir -p "$OUT/crds"

{
  echo "kairon CRD backup"
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  kubectl version 2>/dev/null || true
} >"$OUT/manifest.txt"

kubectl get crd -o name | grep kairon.zyvor.dev | while read -r crd; do
  # crd is "customresourcedefinition.apiextensions.k8s.io/machines.kairon.zyvor.dev"
  # (a TYPE/NAME identifier for the CRD object itself); strip the TYPE/
  # prefix to get "machines.kairon.zyvor.dev" -- a resource.group
  # specifier -- and use THAT for kubectl get below, not $crd, which
  # would fetch the CRD's own schema object instead of any Machine
  # instances.
  kind="$(basename "$crd")"
  kubectl get "$kind" -A -o yaml >"$OUT/crds/${kind}.yaml"
  count="$(kubectl get "$kind" -A --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  echo "$kind: $count object(s)" >>"$OUT/manifest.txt"
done

if [ "$INCLUDE_SECRETS" = "true" ]; then
  mkdir -p "$OUT/secrets"
  echo "WARNING: $OUT/secrets contains live credentials/signing keys in plaintext -- encrypt this backup at rest and restrict who can read it." >&2
  for name in kairon-ui-users kairon-ui-session kairon-ui-token kairon-ui-oidc kairon-console-token; do
    kubectl -n "$NS" get secret "$name" -o yaml >"$OUT/secrets/${name}.yaml" 2>/dev/null || true
  done
  echo "note: operator-supplied TLS secrets (webhook.tlsSecretName, migration.tlsSecretName, console.tls.secretName) are not chart-managed and are not included here -- back those up via whatever issued them (cert-manager, your own PKI)." >>"$OUT/manifest.txt"
fi

tar -czf "${OUT}.tar.gz" "$OUT"
rm -rf "$OUT"
echo "wrote ${OUT}.tar.gz"
