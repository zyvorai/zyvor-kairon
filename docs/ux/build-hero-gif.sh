#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
#
# Build docs/ux/anim/hero.gif from the dashboard screenshots in docs/ux.
# Needs ffmpeg.   ./docs/ux/build-hero-gif.sh
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/hero.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
i=0
for f in day-00-overview day-01-machines day-02-migrations day-09-machines-all-namespaces day-03-snapshots day-04-nodes day-05-network-policies; do
  cp "$HERE/$f.png" "$TMP/f$i.png"; i=$((i+1))
done
mkdir -p "$HERE/anim"
ffmpeg -y -loglevel error -framerate 0.5 -i "$TMP/f%d.png" \
  -vf "scale=1000:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=160[p];[b][p]paletteuse=dither=bayer:bayer_scale=3" \
  -loop 0 "$HERE/anim/hero.gif"
ls -l "$HERE/anim/hero.gif"
