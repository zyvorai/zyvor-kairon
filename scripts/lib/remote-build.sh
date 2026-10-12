#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Runs ON the deploy host (shipped and invoked by scripts/deploy-remote.sh
# --remote-build). Unpacks a source tarball, builds the requested static Go
# binaries and, when asked, the dashboard's web assets, so the machine you
# deploy from needs neither a Go toolchain, a build cache nor node_modules.
#
# Usage: remote-build.sh SRC.tar.gz OUTDIR GOARCH VERSION TARGET...
#   TARGET is a cmd/ name (kairon-node, kaironctl, kairon-ui, ...) or "web".
#
# Toolchains are cached under $KAIRON_BUILD_CACHE (default
# ~/.cache/kairon-build): a Go release matching go.mod is downloaded once and
# its sha256 verified against go.dev; Node is downloaded only when the host's
# is too old for the web build. Nothing is installed system-wide.
set -euo pipefail

src_tgz="${1:?source tarball}"; out="${2:?output dir}"; arch="${3:?goarch}"; version="${4:?version}"
shift 4
targets=("$@")

log() { printf '[build] %s\n' "$*"; }
die() { printf '[build] ERROR: %s\n' "$*" >&2; exit 1; }

cache="${KAIRON_BUILD_CACHE:-$HOME/.cache/kairon-build}"
mkdir -p "$cache" "$out"
work="$(mktemp -d "${TMPDIR:-/tmp}/kairon-build.XXXXXX")"
trap 'rm -rf "$work"' EXIT

command -v curl >/dev/null || die "curl is required to fetch the Go toolchain"
tar -xzf "$src_tgz" -C "$work"

version_ge() { [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" == "$2" ]]; }

fetch_verified() { # url expected_sha256 dest
  local url="$1" sum="$2" dest="$3"
  curl -fsSL --retry 3 -o "$dest" "$url" || die "download failed: $url"
  printf '%s  %s\n' "$sum" "$dest" | sha256sum -c --status - || die "checksum mismatch for $url"
}

# ---- Go ---------------------------------------------------------------
need_go="$(sed -n 's/^go \([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$work/go.mod" | head -1)"
[[ -n "$need_go" ]] || die "could not read the go version from go.mod"

go_bin=""
if command -v go >/dev/null 2>&1; then
  have="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  if [[ -n "$have" ]] && version_ge "$have" "$need_go"; then go_bin="$(command -v go)"; log "using system Go $have"; fi
fi
if [[ -z "$go_bin" ]]; then
  goroot="$cache/go$need_go"
  if [[ ! -x "$goroot/go/bin/go" ]]; then
    file="go${need_go}.linux-${arch}.tar.gz"
    log "downloading Go $need_go ($arch) into $goroot"
    command -v python3 >/dev/null || die "python3 is required to look up the Go checksum"
    index="$(mktemp "$cache/godl-index.XXXXXX")"
    curl -fsSL --retry 3 -o "$index" "https://go.dev/dl/?mode=json&include=all" || die "could not fetch the Go release index"
    sum="$(python3 -c '
import json, sys
want = sys.argv[1]
with open(sys.argv[2]) as fh:
    releases = json.load(fh)
for rel in releases:
    for f in rel.get("files", []):
        if f.get("filename") == want:
            print(f["sha256"]); raise SystemExit
raise SystemExit("no checksum listed for " + want)
' "$file" "$index")" || { rm -f "$index"; die "could not find a published checksum for $file"; }
    rm -f "$index"
    tmp="$(mktemp -d "$cache/dl.XXXXXX")"
    fetch_verified "https://go.dev/dl/$file" "$sum" "$tmp/$file"
    mkdir -p "$goroot.part" && tar -xzf "$tmp/$file" -C "$goroot.part"
    rm -rf "$tmp" "$goroot"
    mv "$goroot.part" "$goroot"
  fi
  go_bin="$goroot/go/bin/go"
  log "using cached Go $need_go"
fi
export GOMODCACHE="$cache/gomod" GOCACHE="$cache/gobuild" GOTOOLCHAIN=local GOFLAGS=-mod=readonly
export CGO_ENABLED=0 GOOS=linux GOARCH="$arch"

build_web=0
for t in "${targets[@]}"; do
  if [[ "$t" == "web" ]]; then build_web=1; continue; fi
  log "go build $t (linux/$arch, version $version)"
  ldflags="-s -w -X main.version=$version"
  [[ "$t" == "kairon-migration-adapter-stub" ]] && ldflags=""
  if [[ -n "$ldflags" ]]; then
    ( cd "$work" && "$go_bin" build -trimpath -ldflags="$ldflags" -o "$out/$t" "./cmd/$t" )
  else
    ( cd "$work" && "$go_bin" build -trimpath -o "$out/$t" "./cmd/$t" )
  fi
done

# ---- web assets ---------------------------------------------------------
if [[ "$build_web" == "1" ]]; then
  node_ok=0
  if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
    nv="$(node --version | sed 's/^v//')"
    # Vite 8 needs Node 20.19+ or 22.12+.
    if version_ge "$nv" "22.12.0" || { [[ "${nv%%.*}" == "20" ]] && version_ge "$nv" "20.19.0"; }; then node_ok=1; log "using system Node $nv"; fi
  fi
  node_dir=""
  if [[ "$node_ok" == "0" ]]; then
    case "$arch" in amd64) narch=x64 ;; arm64) narch=arm64 ;; *) die "no Node build for $arch" ;; esac
    command -v xz >/dev/null || die "xz is required to unpack Node"
    base="https://nodejs.org/dist/latest-v22.x"
    sums="$(curl -fsSL "$base/SHASUMS256.txt")" || die "could not fetch the Node checksums"
    line="$(grep -E "node-v[0-9.]+-linux-${narch}\.tar\.xz\$" <<<"$sums" | head -1)"
    [[ -n "$line" ]] || die "no Node tarball listed for linux-$narch"
    sum="${line%% *}"; file="${line##* }"
    node_dir="$cache/${file%.tar.xz}"
    if [[ ! -x "$node_dir/bin/node" ]]; then
      log "downloading ${file%.tar.xz} into $node_dir"
      tmp="$(mktemp -d "$cache/dl.XXXXXX")"
      fetch_verified "$base/$file" "$sum" "$tmp/$file"
      tar -xJf "$tmp/$file" -C "$cache"
      rm -rf "$tmp"
    fi
    export PATH="$node_dir/bin:$PATH"
  fi
  log "building web assets (npm ci && npm run build)"
  ( cd "$work/web" && npm ci --no-audit --no-fund --loglevel=error && npm run build )
  [[ -d "$work/web/dist" ]] || die "npm run build did not produce web/dist"
  tar -C "$work/web/dist" -czf "$out/kairon-ui-web.tar.gz" .
fi

log "done: $(ls "$out" | tr '\n' ' ')"
