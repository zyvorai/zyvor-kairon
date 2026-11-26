#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
#
# Build multi-platform kubectl-kairon archives for Krew (kubectl-kairon +
# LICENSE in kairon_<os>_<arch>.tar.gz) and emit a filled manifest under
# dist/krew/kairon.yaml (deploy/krew/kairon.yaml stays the placeholder
# template committed to git). After a GitHub Release, copy that filled
# manifest to plugins/kairon.yaml with scripts/krew-index-sync.sh so the
# custom index on the default branch stays current.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(tr -d '[:space:]' < "$ROOT/VERSION")"
VERSION_TAG="v${VERSION#v}"
OUT="$ROOT/dist/krew"
TEMPLATE="$ROOT/deploy/krew/kairon.yaml"
MANIFEST="$OUT/kairon.yaml"
SHAFILE="$OUT/shas.env"

mkdir -p "$OUT"
rm -f "$OUT"/kairon_*.tar.gz "$OUT"/kairon_*.sha256 "$MANIFEST" "$SHAFILE"
: >"$SHAFILE"

LDFLAGS="-s -w -X main.version=${VERSION}"

build_one() {
  local goos="$1" goarch="$2"
  local name="kairon_${goos}_${goarch}"
  local dir="$OUT/$name"
  rm -rf "$dir"
  mkdir -p "$dir"
  echo "building ${name}..."
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "$LDFLAGS" \
    -o "$dir/kubectl-kairon" "$ROOT/cmd/kubectl-kairon"
  if [ -f "$ROOT/LICENSE" ]; then
    cp "$ROOT/LICENSE" "$dir/LICENSE"
  fi
  tar -C "$dir" -czf "$OUT/${name}.tar.gz" .
  local sha
  sha="$(shasum -a 256 "$OUT/${name}.tar.gz" | awk '{print $1}')"
  echo "$sha  ${name}.tar.gz" | tee "$OUT/${name}.sha256"
  echo "SHA_${goos}_${goarch}=$sha" >>"$SHAFILE"
}

build_one darwin arm64
build_one darwin amd64
build_one linux amd64
build_one linux arm64

# shellcheck disable=SC1090
. "$SHAFILE"

python3 - <<PY
from pathlib import Path
import re
text = Path("$TEMPLATE").read_text()
text = re.sub(r'(?m)^  version: "v[^"]*"', '  version: "$VERSION_TAG"', text)
text = re.sub(r'/releases/download/v[\d.]+/', '/releases/download/$VERSION_TAG/', text)
repl = {
    "PLACEHOLDER_SHA256_DARWIN_ARM64": "$SHA_darwin_arm64",
    "PLACEHOLDER_SHA256_DARWIN_AMD64": "$SHA_darwin_amd64",
    "PLACEHOLDER_SHA256_LINUX_AMD64": "$SHA_linux_amd64",
    "PLACEHOLDER_SHA256_LINUX_ARM64": "$SHA_linux_arm64",
}
for k, v in repl.items():
    text = text.replace(k, v)
Path("$MANIFEST").write_text(text)
print("wrote $MANIFEST")
PY

ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
echo
echo "Krew archives in $OUT"
echo "Local install test:"
echo "  kubectl krew install --manifest=$MANIFEST --archive=$OUT/kairon_${OS}_${ARCH}.tar.gz"
