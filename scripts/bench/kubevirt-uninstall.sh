#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Removes the KubeVirt release kubevirt-install.sh applied, CR first so the
# operator tears down its own components. Destroys every KubeVirt VM on the
# cluster: refuses unless KUBEVIRT_UNINSTALL=yes.
set -euo pipefail
[[ "${KUBEVIRT_UNINSTALL:-}" == "yes" ]] || { echo "refusing: set KUBEVIRT_UNINSTALL=yes (deletes every KubeVirt VM)" >&2; exit 2; }
VERSION="${KUBEVIRT_VERSION:-v1.9.0}"
KUBECTL="${KUBECTL:-kubectl}"
BASE="https://github.com/kubevirt/kubevirt/releases/download/$VERSION"

$KUBECTL delete kubevirt -n kubevirt kubevirt --wait=true --timeout=10m --ignore-not-found
$KUBECTL delete -f "$BASE/kubevirt-operator.yaml" --ignore-not-found --wait=true
echo "KubeVirt removed" >&2
