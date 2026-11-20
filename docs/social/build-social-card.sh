#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
# Render kairon social cards with Google Chrome + macOS `sips` (nothing to install).
#   ./docs/social/build-social-card.sh
# Outputs:
#   kairon-social-card.jpg  — 1600x900 for LinkedIn / X
#   kairon-share-card.png   — 1200x630 for Open Graph
# README cards: ./docs/social/readme/build.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[[ -x "$CHROME" ]] || { echo "Google Chrome not found (set CHROME=...)" >&2; exit 1; }

SOCIAL_OUT="${1:-$HERE/kairon-social-card.jpg}"
SHARE_OUT="${2:-$HERE/kairon-share-card.png}"

PNG="$(mktemp "${TMPDIR:-/tmp}/kairon-social.XXXXXX.png")"
PNG2="$(mktemp "${TMPDIR:-/tmp}/kairon-share.XXXXXX.png")"
trap 'rm -f "$PNG" "$PNG2"' EXIT

"$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
  --window-size=1600,900 --screenshot="$PNG" "file://$HERE/kairon-social-card.html" >/dev/null 2>&1
sips -s format jpeg -s formatOptions 92 "$PNG" --out "$SOCIAL_OUT" >/dev/null
echo "wrote $SOCIAL_OUT ($(sips -g pixelWidth -g pixelHeight "$SOCIAL_OUT" | awk '/pixel/{printf "%s ", $2}')px, $(du -k "$SOCIAL_OUT" | cut -f1) KB)"

"$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
  --window-size=1200,630 --screenshot="$PNG2" "file://$HERE/kairon-share-card.html" >/dev/null 2>&1
sips -s format png "$PNG2" --out "$SHARE_OUT" >/dev/null
echo "wrote $SHARE_OUT ($(sips -g pixelWidth -g pixelHeight "$SHARE_OUT" | awk '/pixel/{printf "%s ", $2}')px, $(du -k "$SHARE_OUT" | cut -f1) KB)"
