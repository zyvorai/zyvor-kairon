#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Incident bundle drill (see docs/guides/machine-incident-bundle.md). On
# --node, one QEMU Machine gets two planted secrets through cloud-init (a
# file with api_key=<s1> and a runcmd with password=<s2>) and a probe that
# prints "INCLAB tick=N boot=<boot_id>" to the serial console every 2s.
# It runs "kaironctl diagnose machine --deep --memory-dump --pcap 5s" and
# checks:
#
#   auth        kairon-ui refuses the diagnostics routes without a token
#   bundle      one tar.gz with cluster/ (Machine, events, migrations,
#               diagnosis, Node) and node/ (FluxVM manifest, VM record,
#               QMP state, cgroup, console log), the node job done
#   redact      neither planted secret appears in any bundle file
#   memdump     the manifest names the dump (size, sha256, pause);
#               "kaironctl diagnose memory-dump" downloads it, the sha256
#               matches and it is an ELF core holding the guest kernel
#   continuity  the guest keeps ticking in the same boot after the dump
#   pcap        node/network/capture.pcap holds packets (needs the eBPF
#               dataplane; "not run" with FluxVM's reason otherwise)
#   busy        a second bundle while one runs is refused with 409
#   session     an admin session from /api/v1/auth/login reads the job
#               (needs KAIRON_UI_ADMIN_PASSWORD; "not run" otherwise)
#   cleanup     deleting the Machine removes its diagnostics directory
#
# Usage: lab-incident-bundle.sh --node=NAME --image=PATH [--namespace=ns] [--timeout=600]
# Needs KAIRON_KUBE_* (lib/lab.sh), KAIRON_UI_URL and KAIRON_UI_TOKEN for
# kairon-ui with the node console relay enabled (KAIRON_UI_CA or
# KAIRON_UI_INSECURE=true for a self-signed certificate), kaironctl on PATH
# (or KAIRONCTL), and sudo for the diagnostics directory check
# (FLUXVM_DIAGNOSTICS_DIR, default /var/lib/fluxvm/diagnostics).
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
NODE=""; IMAGE=""; TIMEOUT=600
FLUXVM_URL="${FLUXVM_URL:-http://127.0.0.1:7788}"
KAIRONCTL="${KAIRONCTL:-kaironctl}"
DIAG_DIR="${FLUXVM_DIAGNOSTICS_DIR:-/var/lib/fluxvm/diagnostics}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$NODE" && -n "$IMAGE" ]] || { echo "usage: $0 --node=NAME --image=PATH" >&2; exit 1; }
[[ -n "${KAIRON_UI_URL:-}" && -n "${KAIRON_UI_TOKEN:-}" ]] || { echo "set KAIRON_UI_URL and KAIRON_UI_TOKEN" >&2; exit 1; }
export KAIRON_UI_URL KAIRON_UI_TOKEN
UI_TLS=()
[[ -n "${KAIRON_UI_CA:-}" ]] && UI_TLS=(--cacert "$KAIRON_UI_CA")
[[ "${KAIRON_UI_INSECURE:-}" == true ]] && UI_TLS=(-k)

STAMP="$(date -u +%m%d%H%M%S)"
VM="inc-lab-$STAMP"
WORK="$(mktemp -d)"
S1="s1$(openssl rand -hex 12)"
S2="s2$(openssl rand -hex 12)"
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

flux() {
  local auth=()
  [[ -n "${FLUXVM_TOKEN:-}" ]] && auth=(-H "Authorization: Bearer $FLUXVM_TOKEN")
  curl -fsS "${auth[@]}" "$FLUXVM_URL$1"
}

cleanup() {
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

probe='#!/bin/sh
i=0
while true; do
  i=$((i+1))
  echo "INCLAB tick=$i boot=$(cat /proc/sys/kernel/random/boot_id)" > /dev/ttyS0
  sleep 2
done'
body="$(python3 - "$VM" "$NODE" "$IMAGE" "$probe" "$S1" "$S2" <<'EOF'
import json, sys
name, node, image, probe, s1, s2 = sys.argv[1:7]
spec = {
    "image": {"path": image},
    "resources": {"cpu": "1", "memory": "1Gi"},
    "runtime": {"backend": "qemu"},
    "placement": {"nodeSelector": {"kubernetes.io/hostname": node}},
    "cloudInit": {
        "writeFiles": [
            {"path": "/usr/local/bin/inclab-probe", "content": probe, "permissions": "0755"},
            {"path": "/etc/inclab.env", "content": "api_key=%s\n" % s1, "permissions": "0600"},
        ],
        "runCmd": [
            "echo password=%s > /dev/null" % s2,
            "systemd-run --unit inclab-probe /usr/local/bin/inclab-probe",
        ],
    },
}
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "Machine",
                  "metadata": {"name": name, "labels": {"kairon.zyvor.dev/lab": "incident-bundle"}}, "spec": spec}))
EOF
)"
kube_curl POST "$LAB_API/namespaces/$NAMESPACE/machines" "$body" >/dev/null

field() { machine_field "$NAMESPACE" "$VM" "$1" 2>/dev/null || true; }
runtime_id() { field '(o.get("status") or {}).get("runtimeID") or ""'; }
last_probe() {
  local rid
  rid="$(runtime_id)"
  [[ -n "$rid" ]] || return 1
  flux "/v1/vms/$rid/logs?lines=400" 2>/dev/null | tr -d '\r' | grep -o 'INCLAB tick=[0-9]* boot=[0-9a-f-]*' | tail -1 |
    sed -E 's/INCLAB tick=([0-9]+) boot=(.*)/\1 \2/'
}
probe_seen() { [[ -n "$(last_probe 2>/dev/null || true)" ]]; }
probe_after() { local t; t="$(last_probe 2>/dev/null | cut -d' ' -f1)"; [[ -n "$t" && "$t" -gt "$1" ]]; }
diag_url() { echo "$KAIRON_UI_URL/api/v1/machines/$NAMESPACE/$VM/diagnostics$1"; }
mf() { json_eval "$1" <"$WORK/bundle/manifest.json"; }
diag_dir_gone() { ! sudo -n test -d "$DIAG_DIR/$1"; }
diag_dir_kept=no

if ! wait_until "$TIMEOUT" "$VM Running" machine_healthy "$NAMESPACE" "$VM" ||
  ! wait_until "$TIMEOUT" "$VM probe on the console" probe_seen; then
  result bundle fail "$VM never ran its probe: $(field '(o.get("status") or {}).get("message") or ""')"
else
  rid="$(runtime_id)"
  code="$(curl -s "${UI_TLS[@]}" -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{}' "$(diag_url "")")"
  if [[ "$code" == 401 ]]; then
    result auth pass "POST diagnostics without a token: HTTP 401"
  else
    result auth fail "POST diagnostics without a token: HTTP $code"
  fi

  sleep 4
  read -r tick0 boot0 < <(last_probe)
  started=$(date +%s)
  if ! "$KAIRONCTL" diagnose machine "$VM" -n "$NAMESPACE" --deep --memory-dump --pcap 5s \
    -o "$WORK/bundle.tar.gz" >"$WORK/kaironctl.out" 2>&1; then
    result bundle fail "kaironctl: $(tail -3 "$WORK/kaironctl.out" | tr '\n' ' ')"
  else
    took=$(( $(date +%s) - started ))
    mkdir -p "$WORK/bundle"
    tar -xzf "$WORK/bundle.tar.gz" -C "$WORK/bundle"
    missing=""
    for f in cluster/machine.json cluster/events.json cluster/migrations.json cluster/diagnosis.json cluster/node.json \
      node/manifest.json node/vm/record.json node/qmp/query-status.json node/vm/cgroup/memory.current node/logs/console.log \
      manifest.json README.txt; do
      [[ -s "$WORK/bundle/$f" ]] || missing+=" $f"
    done
    state="$(mf '(o.get("nodeBundle") or {}).get("state")')"
    token="$(mf '(o.get("nodeBundle") or {}).get("token")')"
    sections="$(json_eval '"%d/%d ok" % (sum(1 for s in o["sections"] if s["ok"]), len(o["sections"]))' <"$WORK/bundle/node/manifest.json" 2>/dev/null || echo "?")"
    failed="$(json_eval '",".join(s["name"] for s in o["sections"] if not s["ok"])' <"$WORK/bundle/node/manifest.json" 2>/dev/null || true)"
    files="$(find "$WORK/bundle" -type f | wc -l | tr -d ' ')"
    skipped="$(mf '"; ".join(o.get("skipped") or [])')"
    if [[ -z "$missing" && "$state" == "done" && -z "$skipped" ]]; then
      result bundle pass "kaironctl --deep --memory-dump --pcap 5s in ${took}s: $(stat -c %s "$WORK/bundle.tar.gz") bytes, $files files; node sections $sections (failed: ${failed:-none})"
    else
      result bundle fail "missing:${missing:- none}; node job '$state'; skipped: ${skipped:-none}"
    fi

    leaks="$(grep -rlF -e "$S1" -e "$S2" "$WORK/bundle" 2>/dev/null | sed "s|$WORK/bundle/||" | tr '\n' ' ' || true)"
    fps="$( (grep -rho 'redacted:sha256:' "$WORK/bundle" 2>/dev/null || true) | wc -l | tr -d ' ')"
    if [[ -z "$leaks" && "$(mf 'o.get("redacted")')" == True && "$fps" -gt 0 ]]; then
      result redact pass "neither planted secret (write_files api_key, runcmd password) in $files files; $fps sha256 fingerprints"
    else
      result redact fail "secrets found in: ${leaks:-none}; fingerprints $fps"
    fi

    dsize="$(mf '((o.get("nodeBundle") or {}).get("memory_dump") or {}).get("size_bytes") or 0')"
    dsha="$(mf '((o.get("nodeBundle") or {}).get("memory_dump") or {}).get("sha256") or ""')"
    paused="$(mf '((o.get("nodeBundle") or {}).get("memory_dump") or {}).get("paused_ms") or 0')"
    if [[ "$dsize" -gt 0 && -n "$dsha" ]] &&
      "$KAIRONCTL" diagnose memory-dump "$VM" "$token" -n "$NAMESPACE" -o "$WORK/mem.elf" >"$WORK/dump.out" 2>&1; then
      got="$(sha256sum "$WORK/mem.elf" | cut -d' ' -f1)"
      kind="$(file -b "$WORK/mem.elf")"
      if [[ "$got" == "$dsha" && "$(stat -c %s "$WORK/mem.elf")" == "$dsize" && "$kind" == ELF*core* ]] &&
        grep -aq 'Linux version' "$WORK/mem.elf"; then
        result memdump pass "$((dsize >> 20)) MiB ELF core, guest paused ${paused}ms; downloaded through kairon-ui, sha256 matches, holds the guest kernel banner"
      else
        result memdump fail "sha $got vs $dsha, size $(stat -c %s "$WORK/mem.elf") vs $dsize, file: $kind"
      fi
      rm -f "$WORK/mem.elf"
    else
      result memdump fail "dump size $dsize sha '$dsha': $(tail -2 "$WORK/dump.out" 2>/dev/null | tr '\n' ' ')"
    fi

    if wait_until 60 "probe ticking after the bundle" probe_after "$tick0"; then
      read -r tick1 boot1 < <(last_probe)
      if [[ "$boot1" == "$boot0" ]]; then
        result continuity pass "tick $tick0 -> $tick1 in the same boot after a ${paused}ms dump pause"
      else
        result continuity fail "boot id changed: $boot0 -> $boot1"
      fi
    else
      result continuity fail "probe stopped at tick $tick0"
    fi

    pcap="$WORK/bundle/node/network/capture.pcap"
    if [[ -s "$pcap" && "$(stat -c %s "$pcap")" -gt 24 ]]; then
      result pcap pass "$(stat -c %s "$pcap") bytes captured over 5s"
    else
      why="$(json_eval '"; ".join("%s: %s" % (s["name"], s.get("error")) for s in o["sections"] if s["name"].startswith("network/") and not s["ok"])' <"$WORK/bundle/node/manifest.json" 2>/dev/null || true)"
      result pcap "not run" "no capture: ${why:-no network section errors}"
    fi

    ui_post() {
      curl -sS "${UI_TLS[@]}" -o "$WORK/post.json" -w '%{http_code}' -X POST -H "Authorization: Bearer $KAIRON_UI_TOKEN" \
        -H 'Content-Type: application/json' -d "$1" "$(diag_url "")"
    }
    # The dump keeps the first job running for a second or more.
    first="$(ui_post '{"memory_dump":true,"redact":true}')"
    busy_token="$(json_eval 'o.get("token") or ""' <"$WORK/post.json")"
    second="$(ui_post '{"redact":true}')"
    busy_msg="$(json_eval 'o.get("error") or ""' <"$WORK/post.json")"
    job_done() {
      [[ "$(curl -fsS "${UI_TLS[@]}" -H "Authorization: Bearer $KAIRON_UI_TOKEN" "$(diag_url "/$busy_token")" | json_eval 'o.get("state")')" != running ]]
    }
    if [[ "$first" == 202 && "$second" == 409 ]] && wait_until 120 "second bundle finished" job_done; then
      result busy pass "second start while one runs: HTTP 409 ($busy_msg)"
    else
      result busy fail "first start HTTP $first, second HTTP $second: $busy_msg"
    fi

    if [[ -n "${KAIRON_UI_ADMIN_PASSWORD:-}" ]]; then
      session="$(curl -fsS "${UI_TLS[@]}" -H 'Content-Type: application/json' \
        -d "$(python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["KAIRON_UI_ADMIN_PASSWORD"]}))')" \
        "$KAIRON_UI_URL/api/v1/auth/login" | json_eval 'o.get("token") or ""' || true)"
      job_state="$(curl -fsS "${UI_TLS[@]}" -H "Authorization: Bearer $session" "$(diag_url "/$token")" 2>/dev/null | json_eval 'o.get("state")' || true)"
      if [[ -n "$session" && "$job_state" == "done" ]]; then
        result session pass "admin login session reads the job (state done)"
      else
        result session fail "login token '${session:0:6}...' job state '$job_state'"
      fi
    fi
  fi

  sudo -n test -d "$DIAG_DIR/$rid" && diag_dir_kept=yes
  kube_curl DELETE "$(machine_path "$NAMESPACE" "$VM")" >/dev/null 2>&1 || true
  if [[ "$diag_dir_kept" == no ]]; then
    result cleanup "not run" "no diagnostics directory for $rid before the delete"
  elif wait_until 180 "diagnostics dir for $rid removed" diag_dir_gone "$rid"; then
    result cleanup pass "deleting the Machine removed $DIAG_DIR/$rid"
  else
    result cleanup fail "$DIAG_DIR/$rid still there"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in auth bundle redact memdump continuity pcap busy session cleanup; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  n="${NOTE[$c]:-earlier step failed}"
  [[ "$c" == session && -z "${RESULT[$c]:-}" && -z "${KAIRON_UI_ADMIN_PASSWORD:-}" ]] && n="needs KAIRON_UI_ADMIN_PASSWORD"
  echo "| Incident bundle: $c | $r | $today | Node $NODE, $n |"
done
echo "| Incident bundle: non-admin account refused | not run | $today | the lab has no non-admin kairon-ui account; covered by internal/uiapi tests |"
exit "$rc"
