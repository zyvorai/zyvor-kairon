#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# KubeVirt side of the Kairon vs KubeVirt benchmark. Run on the node under
# test with KubeVirt installed (scripts/bench/kubevirt-install.sh).
#
#   BENCH_IMAGE=/var/lib/fluxvm/images/ubuntu.qcow2 scripts/bench/bench-kubevirt.sh [bench.py flags]
#
# With BENCH_IMAGE set, the same file is imported as a containerDisk first
# so both sides boot identical bytes; or pass BENCH_CONTAINERDISK=REF for an
# image already in the node's containerd.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
REF="${BENCH_CONTAINERDISK:-}"
if [[ -z "$REF" ]]; then
  REF="localhost/bench/disk:latest"
  scripts/bench/containerdisk-import.sh "${BENCH_IMAGE:?set BENCH_IMAGE or BENCH_CONTAINERDISK}" "$REF"
fi
exec python3 scripts/bench/bench.py --platform kubevirt --containerdisk "$REF" "$@"
