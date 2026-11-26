#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
# Preflight for the multi-host migration matrix.
# Prints which required vars/secrets are still missing for a green nightly.

set -euo pipefail

missing=0
need() {
  local name="$1" desc="$2"
  if [[ -z "${!name:-}" ]]; then
    echo "MISSING  $name  — $desc"
    missing=1
  else
    echo "ok       $name"
  fi
}

echo "=== Migration matrix preflight ==="
need KAIRON_HW_MACHINE          "source Machine name for cold/live tests"
need KAIRON_HW_TARGET_NODE      "target node name"
need KAIRON_UI_URL              "kairon-ui base URL"
need KAIRON_UI_TOKEN            "kairon-ui bearer token (secret)"
need KAIRON_HW_EBPF_MACHINE     "Machine for live-eBPF row"
need KAIRON_HW_IMAGE            "disk image path on both hosts (full profile)"
need KAIRON_HW_SOURCE_SSH       "ssh target for source host (full)"
need KAIRON_HW_TARGET_SSH       "ssh target for target host (full)"
need KAIRON_HW_NAMESPACE        "namespace (default kairon-lab)"

if [[ $missing -eq 0 ]]; then
  echo "All required vars present."
else
  echo
  echo "Set the MISSING variables (and secrets) before expecting a green nightly."
  exit 1
fi
