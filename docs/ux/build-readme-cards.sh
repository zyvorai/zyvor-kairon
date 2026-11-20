#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
# Render docs/ux/readme-*.html to 3200px-wide JPEGs used by the README. Needs Google Chrome and macOS `sips`.
#   ./docs/ux/build-readme-cards.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[[ -x "$CHROME" ]] || { echo "Google Chrome not found (set CHROME=...)" >&2; exit 1; }
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cards.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
for name in readme-capabilities readme-how-it-works readme-vs; do
  h=$("$CHROME" --headless=new --disable-gpu --window-size=1600,2000 --dump-dom "file://$HERE/$name.html" 2>/dev/null | grep -o 'data-h="[0-9]*"' | tr -dc '0-9')
  [[ -n "$h" ]] || { echo "could not measure $name" >&2; exit 1; }
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=2 \
    --window-size=1600,"$h" --screenshot="$TMP/$name.png" "file://$HERE/$name.html" >/dev/null 2>&1
  sips -s format jpeg -s formatOptions 88 "$TMP/$name.png" --out "$HERE/$name.jpg" >/dev/null
  echo "wrote docs/ux/$name.jpg"
done
