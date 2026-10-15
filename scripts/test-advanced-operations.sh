#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/.."
evidence=${1:-$(mktemp -d "${TMPDIR:-/tmp}/kairon-operations-tests.XXXXXX")}
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
export KAIRON_REHEARSAL_TEST_EVIDENCE="$evidence"
export KAIRON_OPERATIONS_EVIDENCE="$evidence"
go test -race -count=1 -v ./internal/rehearsal ./internal/changeplan ./internal/incidentwork ./internal/scheduler \
  -run 'TestTwoTierHTTPRehearsal|TestRecovery|TestIncident|TestChangePlan|TestLostForward|TestExternalMutation|TestRollback|TestDiskCheckpoint|TestPreconditions|TestRecover|TestNetworkPlacement|TestTrafficCollection|TestMeasuredFlow|TestBind'
printf 'Scenario evidence: %s\n' "$evidence"
