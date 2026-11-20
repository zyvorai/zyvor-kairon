#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Kairon side of the Kairon vs KubeVirt benchmark. Run on the node under
# test with kairon-controller, kairon-node and FluxVM running.
#
#   BENCH_IMAGE=/var/lib/fluxvm/images/ubuntu.qcow2 scripts/bench/bench-kairon.sh [bench.py flags]
#
# See docs/benchmarks/kairon-vs-kubevirt.md for the methodology.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
exec python3 scripts/bench/bench.py --platform kairon --image "${BENCH_IMAGE:?set BENCH_IMAGE to a qcow2/raw path on the node}" "$@"
