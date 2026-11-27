#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
# Prints the exact gh commands (no values) needed for a green multi-host
# migration nightly. See docs/runbook-migration-matrix.md.

set -euo pipefail

echo "=== Green nightly secrets / variables checklist ==="
echo
echo "Required for any run:"
echo "  gh variable set KAIRON_HW_MACHINE      --body \"<source Machine name>\""
echo "  gh variable set KAIRON_HW_TARGET_NODE  --body \"<target node name>\""
echo
echo "Core profile:"
echo "  gh variable set KAIRON_UI_URL          --body \"<kairon-ui base URL>\""
echo "  gh secret set   KAIRON_UI_TOKEN        --body \"<bearer token>\""
echo "  gh variable set KAIRON_HW_EBPF_MACHINE --body \"<Machine for live-eBPF>\""
echo
echo "Full profile (in addition to core):"
echo "  gh variable set KAIRON_HW_IMAGE        --body \"<disk image path>\""
echo "  gh variable set KAIRON_HW_SOURCE_SSH   --body \"<ssh target source>\""
echo "  gh variable set KAIRON_HW_TARGET_SSH   --body \"<ssh target target>\""
echo "  gh variable set KAIRON_HW_POSTCOPY_MACHINE --body \"<Machine for post-copy>\""
echo
echo "Optional:"
echo "  gh variable set KAIRON_HW_NAMESPACE    --body \"kairon-lab\""
echo
echo "After setting, re-run the nightly or:"
echo "  gh workflow run hardware-migration.yml"
