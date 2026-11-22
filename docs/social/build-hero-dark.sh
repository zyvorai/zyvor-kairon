#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
# Render docs/social/kairon-hero-dark.html to kairon-hero-dark.jpg (2400x1260): README hero and GitHub social preview.
# Needs Google Chrome and macOS `sips`; nothing is installed.
#   ./docs/social/build-hero-dark.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[[ -x "$CHROME" ]] || { echo "Google Chrome not found (set CHROME=...)" >&2; exit 1; }
TMP="$(mktemp -d "${TMPDIR:-/tmp}/hero.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
"$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=2 \
  --window-size=1200,630 --screenshot="$TMP/card.png" "file://$HERE/kairon-hero-dark.html" >/dev/null 2>&1
sips -s format jpeg -s formatOptions 90 "$TMP/card.png" --out "$HERE/kairon-hero-dark.jpg" >/dev/null
echo "wrote docs/social/kairon-hero-dark.jpg"
