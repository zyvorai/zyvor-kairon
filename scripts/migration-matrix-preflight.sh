#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
# Preflight for the multi-host migration matrix: which vars/secrets are still
# missing for a green nightly. Shortcut for
#   scripts/migration-matrix.sh preflight --dry-run --profile=full
# (inputs only, no network). Extra flags pass through, e.g. --profile=core,
# --env-file=FILE. Drop --dry-run by calling migration-matrix.sh preflight
# directly to also check the cluster, kairon-ui and SSH targets.
# The input table, messages and plan live in scripts/migration-matrix.sh.
set -euo pipefail
exec "$(dirname "${BASH_SOURCE[0]}")/migration-matrix.sh" preflight --dry-run --profile=full "$@"
