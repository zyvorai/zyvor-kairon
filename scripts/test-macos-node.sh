#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

# Offline test of scripts/macos-node.sh: renders the LaunchAgent for a throwaway HOME and checks it. Loads nothing into
# launchd and needs no Kubernetes or FluxVM, so it runs on any host with bash (plutil is used when present).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/macos-node.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
export HOME="$T/home with space"
mkdir -p "$HOME/.config/kairon"
FAILS=0
ok() { echo "  ok    $1"; }
bad() { echo "  FAIL  $1"; FAILS=$((FAILS + 1)); }
expect() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
# refuses NAME ARGS... MESSAGE: the command fails and says MESSAGE (output captured first: grep -q would SIGPIPE it).
refuses() {
    local name="$1" msg="$2" out rc
    shift 2
    out="$("$@" 2>&1)"; rc=$?
    if [ "$rc" -ne 0 ] && [[ "$out" == *"$msg"* ]]; then ok "$name"; else bad "$name (rc=$rc: $out)"; fi
}

ENVF="$HOME/.config/kairon/kairon-node.env"
printf 'NODE_NAME=mac-1\nKAIRON_KUBE_URL=https://api:6443\nKAIRON_KUBE_TOKEN=s3cret-token-value\nKAIRON_KUBE_CA=/x/ca.pem\n' >"$ENVF"
chmod 600 "$ENVF"
printf '#!/bin/sh\nexit 0\n' >"$T/kairon-node"; chmod +x "$T/kairon-node"

OUT="$("$SCRIPT" render --bin "$T/kairon-node" 2>&1)"
expect "render succeeds and names the label" '[[ "$OUT" == *"<key>Label</key><string>dev.zyvor.kairon-node</string>"* ]]'
expect "the token never appears in the plist" '[[ "$OUT" != *s3cret-token-value* ]]'
expect "the plist sources the env file and execs the node" '[[ "$OUT" == *"exec "* && "$OUT" == *"kairon-node.env"* ]]'
expect "paths with spaces survive (single-quoted)" '[[ "$OUT" == *"home with space"* ]]'
expect "the health listener is loopback-only" '[[ "$OUT" == *"--health-addr=127.0.0.1:32302"* ]]'
expect "it is restarted and not throttled by the system" '[[ "$OUT" == *"<key>KeepAlive</key><true/>"* && "$OUT" == *"Interactive"* ]]'
if command -v plutil >/dev/null 2>&1; then
    echo "$OUT" >"$T/p.plist"
    expect "plutil accepts it" 'plutil -lint "$T/p.plist" >/dev/null'
fi

# The shell command inside the plist must itself be valid and must not run anything when the env file is sourced.
# shellcheck disable=SC2034 # used inside the eval'd expectations below
CMD="$(echo "$OUT" | sed -n '/<string>set -a/s/.*<string>\(.*\)<\/string>.*/\1/p' | sed 's/&quot;/"/g;s/&amp;/\&/g;s/&lt;/</g;s/&gt;/>/g')"
expect "the embedded sh command parses" 'sh -n -c "$CMD"'
expect "and runs the node with the env file's values" '[ "$(sh -c "$(echo "$CMD" | sed "s#exec .*#echo \$NODE_NAME#")")" = mac-1 ]'

chmod 644 "$ENVF"
refuses "a group/world-readable env file is refused" "chmod 600" "$SCRIPT" render --bin "$T/kairon-node"
chmod 600 "$ENVF"
grep -v KAIRON_KUBE_CA "$ENVF" >"$ENVF.new" && mv "$ENVF.new" "$ENVF" && chmod 600 "$ENVF"
refuses "a missing required key is named" "does not set KAIRON_KUBE_CA" "$SCRIPT" render --bin "$T/kairon-node"
echo "KAIRON_KUBE_CA=/x/ca.pem" >>"$ENVF"
refuses "a missing binary is refused" "not executable" "$SCRIPT" render --bin "$T/nope"
expect "an unknown command is refused" '! "$SCRIPT" frobnicate >/dev/null 2>&1'

echo
if [ "$FAILS" -eq 0 ]; then echo "all passed"; else echo "$FAILS failed"; exit 1; fi
