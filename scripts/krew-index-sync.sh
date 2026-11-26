#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
#
# Copy a filled Krew manifest to plugins/kairon.yaml (this repo's custom
# index). Checksums must match the GitHub Release archives the manifest
# uris point at — prefer the release asset over a locally built
# dist/krew/kairon.yaml unless those archives were uploaded as that tag's
# assets.
#
# Usage:
#   scripts/krew-index-sync.sh              # copy dist/krew/kairon.yaml
#   scripts/krew-index-sync.sh PATH.yaml    # copy that file
#   scripts/krew-index-sync.sh v0.10.6      # download that release asset
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/plugins/kairon.yaml"
SRC="${1:-}"

mkdir -p "$ROOT/plugins"

if [ -z "$SRC" ]; then
  SRC="$ROOT/dist/krew/kairon.yaml"
  if [ ! -f "$SRC" ]; then
    echo "no $SRC; run make krew-package or pass a release tag (e.g. v0.10.6)" >&2
    exit 1
  fi
  cp "$SRC" "$DEST"
elif [ -f "$SRC" ]; then
  cp "$SRC" "$DEST"
else
  VERSION_TAG="$SRC"
  case "$VERSION_TAG" in
    v*) ;;
    *) VERSION_TAG="v${VERSION_TAG}" ;;
  esac
  url="https://github.com/zyvorai/zyvor-kairon/releases/download/${VERSION_TAG}/kairon.yaml"
  curl -fsSL "$url" -o "$DEST"
fi

python3 - <<PY
from pathlib import Path
import sys
text = Path("$DEST").read_text()
if "kind: Plugin" not in text or "apiVersion: krew.googlecontainertools.github.com/v1alpha2" not in text:
    print("refusing to write $DEST: not a Krew Plugin manifest", file=sys.stderr)
    sys.exit(1)
print("wrote $DEST")
PY
