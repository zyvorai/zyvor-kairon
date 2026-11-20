#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Render the README cards (docs/social/readme/*.html) to JPEGs in docs/assets/.
# Needs Google Chrome and macOS `sips` (both already on a Mac); nothing is installed.
#   ./docs/social/readme/build.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="$HERE/../../assets"
mkdir -p "$OUT"
CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[[ -x "$CHROME" ]] || { echo "Google Chrome not found (set CHROME=...)" >&2; exit 1; }
TMP="$(mktemp -d "${TMPDIR:-/tmp}/kairon-readme.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
render() { # <html> <jpg> <height>
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
    --window-size="1600,$3" --screenshot="$TMP/$2.png" "file://$HERE/$1" >/dev/null 2>&1
  sips -s format jpeg -s formatOptions 90 "$TMP/$2.png" --out "$OUT/$2" >/dev/null
  echo "wrote docs/assets/$2 ($(du -k "$OUT/$2" | cut -f1) KB)"
}
render hero.html readme-hero.jpg 800
render benchmark.html readme-benchmark.jpg 800
render vs-kubevirt.html kairon-vs-kubevirt.jpg 820
render capabilities.html readme-capabilities.jpg 640
render architecture.html readme-architecture.jpg 700
