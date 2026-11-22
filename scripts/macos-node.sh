#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# Run kairon-node on a Mac as a launchd LaunchAgent (docs/runbook-macos-node.md).
#
#   scripts/macos-node.sh install   [--bin PATH] [--env-file FILE] [--no-load]
#   scripts/macos-node.sh uninstall
#   scripts/macos-node.sh status
#   scripts/macos-node.sh render    [--bin PATH] [--env-file FILE]     # print the plist, change nothing
#
# A LaunchAgent, not a LaunchDaemon: FluxVM's vz backend restores saved VM state only in an unlocked login session, so the
# node runs in the logged-in user's session (FluxVM's deploy/launchd-notes.md).
#
# Secrets stay out of the plist. The agent runs `sh -c '. ENV_FILE; exec kairon-node ...'`, and ENV_FILE
# (default ~/.config/kairon/kairon-node.env) must be mode 0600 and define at least:
#
#   NODE_NAME=mac-studio-1
#   KAIRON_KUBE_URL=https://API:6443
#   KAIRON_KUBE_TOKEN=...            # ServiceAccount token for kairon-node
#   KAIRON_KUBE_CA=/path/to/ca.pem
#
# Optional: FLUXVM_URL, FLUXVM_TOKEN, KAIRON_NODE_CONSOLE_TOKEN (enables the console relay Veyron and kairon-ui use on
# :8090), KAIRON_MLX_URL, KAIRON_MEMORY_RECLAIM=false and KAIRON_PRESSURE_REPORT=false (both read Linux PSI; their macOS
# behaviour is not verified). Anything else kairon-node reads from its environment works the same way.
set -euo pipefail

LABEL="dev.zyvor.kairon-node"
HOME_DIR="${HOME:?HOME is not set}"
PLIST="$HOME_DIR/Library/LaunchAgents/$LABEL.plist"
LOG="$HOME_DIR/Library/Logs/kairon-node.log"
ENV_FILE="$HOME_DIR/.config/kairon/kairon-node.env"
BIN=""
LOAD=1
REQUIRED=(NODE_NAME KAIRON_KUBE_URL KAIRON_KUBE_TOKEN KAIRON_KUBE_CA)

die() { echo "macos-node.sh: $*" >&2; exit 1; }

xml_escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'; }
# A word for sh -c, in single quotes.
shq() { printf "'%s'" "${1//\'/\'\\\'\'}"; }

usage() { sed -n '5,10p' "$0" | sed 's/^# \{0,1\}//'; }

parse_flags() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --bin) BIN="${2:?--bin needs a path}"; shift 2 ;;
            --env-file) ENV_FILE="${2:?--env-file needs a path}"; shift 2 ;;
            --no-load) LOAD=0; shift ;;
            *) die "unknown option: $1" ;;
        esac
    done
}

file_mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }

check_env_file() {
    [ -f "$ENV_FILE" ] || die "$ENV_FILE does not exist (see the header of this script for what it must define)"
    local mode
    mode="$(file_mode "$ENV_FILE")"
    case "$mode" in
        600|400) ;;
        *) die "$ENV_FILE has mode $mode: it holds a token, so chmod 600 it" ;;
    esac
    local k
    for k in "${REQUIRED[@]}"; do
        grep -Eq "^(export[[:space:]]+)?$k=." "$ENV_FILE" || die "$ENV_FILE does not set $k"
    done
}

resolve_bin() {
    if [ -z "$BIN" ]; then
        BIN="$(command -v kairon-node || true)"
    fi
    [ -n "$BIN" ] || die "kairon-node not found: pass --bin PATH (make build writes bin/kairon-node)"
    [ -x "$BIN" ] || die "$BIN is not executable"
    case "$BIN" in /*) ;; *) BIN="$PWD/$BIN" ;; esac
}

render() {
    local script
    # set -a exports everything the file defines; exec keeps launchd supervising kairon-node itself.
    script="set -a; . $(shq "$ENV_FILE"); set +a; exec $(shq "$BIN") --interval=3s --health-addr=127.0.0.1:32302"
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>$(printf '%s' "$script" | xml_escape)</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>$(printf '%s' "$LOG" | xml_escape)</string>
  <key>StandardErrorPath</key><string>$(printf '%s' "$LOG" | xml_escape)</string>
</dict>
</plist>
EOF
}

domain() { echo "gui/$(id -u)"; }

cmd_install() {
    parse_flags "$@"
    check_env_file
    resolve_bin
    mkdir -p "$(dirname "$PLIST")" "$(dirname "$LOG")"
    render >"$PLIST"
    if command -v plutil >/dev/null 2>&1; then plutil -lint "$PLIST" >/dev/null || die "wrote an invalid plist: $PLIST"; fi
    echo "wrote $PLIST"
    [ "$LOAD" = 1 ] || return 0
    launchctl bootout "$(domain)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "$(domain)" "$PLIST"
    echo "loaded $LABEL; log: $LOG"
}

cmd_uninstall() {
    launchctl bootout "$(domain)/$LABEL" 2>/dev/null || true
    if [ -f "$PLIST" ]; then rm -f "$PLIST"; echo "removed $PLIST"; else echo "not installed"; fi
}

cmd_status() {
    echo "plist:  $PLIST ($([ -f "$PLIST" ] && echo present || echo absent))"
    local out
    out="$(mktemp)"
    if launchctl print "$(domain)/$LABEL" >"$out" 2>/dev/null; then
        grep -E '^[[:space:]]*(state|pid|last exit code) =' "$out" | sed 's/^[[:space:]]*/loaded: /' || true
    else
        echo "loaded: no"
    fi
    rm -f "$out"
    echo "log:    $LOG"
}

[ "$(uname -s)" = Darwin ] || [ "${1:-}" = render ] || die "this installs a launchd agent: run it on macOS (render works anywhere)"
case "${1:-}" in
    install) shift; cmd_install "$@" ;;
    uninstall) cmd_uninstall ;;
    status) cmd_status ;;
    render) shift; parse_flags "$@"; check_env_file; resolve_bin; render ;;
    -h|--help|help|"") usage ;;
    *) die "unknown command: $1 (install, uninstall, status, render)" ;;
esac
