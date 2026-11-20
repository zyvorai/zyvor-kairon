#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Installs a pinned KubeVirt release for the benchmark and waits for it.
#
#   KUBEVIRT_VERSION=v1.9.0 scripts/bench/kubevirt-install.sh
set -euo pipefail
VERSION="${KUBEVIRT_VERSION:-v1.9.0}"
KUBECTL="${KUBECTL:-kubectl}"
BASE="https://github.com/kubevirt/kubevirt/releases/download/$VERSION"

if $KUBECTL get kubevirt -n kubevirt kubevirt >/dev/null 2>&1; then
  echo "KubeVirt already installed: $($KUBECTL get kubevirt -n kubevirt kubevirt -o jsonpath='{.status.observedKubeVirtVersion}')" >&2
  exit 0
fi
$KUBECTL apply -f "$BASE/kubevirt-operator.yaml"
$KUBECTL apply -f "$BASE/kubevirt-cr.yaml"
$KUBECTL -n kubevirt wait kubevirt kubevirt --for=condition=Available --timeout=15m
echo "KubeVirt $VERSION ready" >&2
