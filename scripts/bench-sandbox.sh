#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Agent-sandbox drill and benchmark (see docs/guides/agent-sandboxes.md).
# Everything runs on one node through kaironctl, and each case prints one
# docs/COMPATIBILITY.md row:
#
#   idle pause      a Machine with --idle-timeout 1m pauses itself once idle
#   wake (paused)   `guest exec` on the paused Machine wakes it (wakeOnConnect)
#   wake (hibernated)  the same after `hibernate`
#   pool claim      bindMillis of a claim against a Running and a Paused pool
#   fork            `fork --count 1` until the child is Running
#   lifetime        a --max-lifetime 1m Machine is deleted by the controller
#   freeze          freeze with --unfreeze-timeout 1m thaws by itself;
#                   freeze + unfreeze round trip
#   vnc screenshot  writes a PNG with a sane size
#   create clone    clone of the Halted Machine boots and its agent answers
#   guestfs         guestfish --ro inspects the Halted Machine's disk
#   cdrom           insert on the Stopped Machine shows up in the guest after
#                   start; eject empties the drive live
#
# Usage: bench-sandbox.sh --image=PATH [--node=NAME] [--namespace=default]
#          [--timeout=600] [--iso-url=URL --iso-sha256=HEX]
# Needs kaironctl with cluster access and KAIRON_UI_URL / KAIRON_UI_TOKEN,
# kubectl, and an image that runs qemu-guest-agent. --node pins every Machine
# (the image must exist there; clone and cdrom need kairon-node
# --image-cache-dir, guestfs needs libguestfs-tools on it). Without
# --iso-url the cdrom case is "not run". Machines and pools are deleted on
# exit.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NS="default"; NODE=""; IMAGE=""; TIMEOUT=600; ISO_URL=""; ISO_SHA=""
KUBECTL="${KUBECTL:-kubectl}"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NS="${arg#*=}" ;;
    --node=*) NODE="${arg#*=}" ;;
    --image=*) IMAGE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --iso-url=*) ISO_URL="${arg#*=}" ;;
    --iso-sha256=*) ISO_SHA="${arg#*=}" ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$IMAGE" ]] || { echo "usage: $0 --image=PATH [--node=NAME]" >&2; exit 1; }

STAMP="$(date -u +%H%M%S)"
VM="sbx-$STAMP"
LIFE="sbx-life-$STAMP"
FORK="sbx-fk-$STAMP"
CLONE="sbx-cl-$STAMP"
POOL_RUN="sbx-pr-$STAMP"
POOL_PAUSED="sbx-pp-$STAMP"
TMP="$(mktemp -d)"
CASES=(idle-pause wake-paused wake-hibernated pool-running pool-paused fork lifetime freeze-autothaw freeze-unfreeze vnc-screenshot create-clone guestfs-ro cdrom)
declare -A RESULT NOTE
result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

K() { "$KAIRONCTL" -n "$NS" "$@"; }

cleanup() {
  set +e
  [[ -n "${LIFE_POLL:-}" ]] && kill "$LIFE_POLL" 2>/dev/null
  "$KUBECTL" -n "$NS" delete machineclaims,machinepools -l "kairon.zyvor.dev/lab=$VM" --wait=false >/dev/null 2>&1
  for p in "$POOL_RUN" "$POOL_PAUSED"; do "$KUBECTL" -n "$NS" delete machinepool "$p" --wait=false >/dev/null 2>&1; done
  "$KUBECTL" -n "$NS" delete machines -l "kairon.zyvor.dev/lab=$VM" --wait=false >/dev/null 2>&1
  for m in "$VM" "$LIFE" "$FORK-1" "$CLONE"; do "$KUBECTL" -n "$NS" delete machine "$m" --wait=false >/dev/null 2>&1; done
  rm -rf "$TMP"
}
trap cleanup EXIT

# labelled OBJECT-JSON: adds the lab label and pins to --node.
labelled() {
  python3 -c '
import json, sys
o = json.load(sys.stdin)
node = sys.argv[1]
o.setdefault("metadata", {}).setdefault("labels", {})["kairon.zyvor.dev/lab"] = sys.argv[2]
spec = o["spec"]["template"].setdefault("spec", {}) if o["kind"] == "MachinePool" else o["spec"]
if node:
    spec.setdefault("placement", {}).setdefault("nodeSelector", {})["kubernetes.io/hostname"] = node
print(json.dumps(o))
' "$NODE" "$VM"
}

# new_machine NAME [create flags...]
new_machine() {
  local name="$1"; shift
  "$KAIRONCTL" create "$name" --namespace "$NS" --image "$IMAGE" --cpu 1 --memory 1Gi "$@" --dry-run -o json |
    labelled | "$KUBECTL" -n "$NS" apply -f - >/dev/null
}

# new_pool NAME [create machinepool flags...]
new_pool() {
  local name="$1"; shift
  "$KAIRONCTL" create machinepool "$name" --namespace "$NS" --image "$IMAGE" --cpu 1 --memory 1Gi --guest-agent \
    --replicas 1 "$@" --dry-run -o json | labelled | "$KUBECTL" -n "$NS" apply -f - >/dev/null
}

mfield() { "$KUBECTL" -n "$NS" get machine "$1" -o json 2>/dev/null | json_eval "$2" 2>/dev/null || true; }
phase() { mfield "$1" '(o.get("status") or {}).get("phase") or ""'; }
phase_is() { [[ "$(phase "$1")" == "$2" ]]; }
annotation() { mfield "$1" "((o.get(\"metadata\") or {}).get(\"annotations\") or {}).get(\"$2\") or \"\""; }
frozen() { [[ -n "$(annotation "$1" kairon.zyvor.dev/fs-frozen)" ]]; }
thawed() { ! frozen "$1"; }
gone() { ! "$KUBECTL" -n "$NS" get machine "$1" >/dev/null 2>&1; }
stopped_clean() { phase_is "$1" Stopped && [[ -z "$(mfield "$1" '(o.get("status") or {}).get("runtimeID") or ""')" ]]; }
message() { mfield "$1" '(o.get("status") or {}).get("message") or ""'; }
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }
secs_since() { python3 -c 'import sys,time; print(f"{(time.time()*1000-int(sys.argv[1]))/1000:.1f}")' "$1"; }
agent_up() { K wait "$1" --for agent --timeout "${TIMEOUT}s" >/dev/null; }

# warm_member POOL STATE: a member labelled warm in phase STATE.
warm_member() {
  "$KUBECTL" -n "$NS" get machines -l "kairon.zyvor.dev/machinepool=$1,kairon.zyvor.dev/pool-state=warm" -o json 2>/dev/null |
    json_eval "any(((m.get(\"status\") or {}).get(\"phase\") == \"$2\") for m in o.get(\"items\") or [])" 2>/dev/null |
    grep -qx True
}

# claim_ms POOL: claims POOL once a member is warm and prints bindMillis.
claim_ms() {
  local name="$1-c"
  python3 -c '
import json, sys
print(json.dumps({"apiVersion": "kairon.zyvor.dev/v1", "kind": "MachineClaim",
  "metadata": {"name": sys.argv[1], "labels": {"kairon.zyvor.dev/lab": sys.argv[3]}},
  "spec": {"poolName": sys.argv[2]}}))' "$name" "$1" "$VM" | "$KUBECTL" -n "$NS" apply -f - >/dev/null
  local i
  for ((i = 0; i < 60; i++)); do
    local got
    got="$("$KUBECTL" -n "$NS" get machineclaim "$name" -o json | json_eval '(o.get("status") or {}).get("bindMillis") if (o.get("status") or {}).get("phase") == "Bound" else ""')"
    [[ -n "$got" ]] && { echo "$got"; return 0; }
    sleep 1
  done
  return 1
}

# ---- start everything that boots in the background -------------------------
new_machine "$VM" --guest-agent --idle-timeout 1m --wake-on-connect
LIFE_START="$(now_ms)"
new_machine "$LIFE" --memory 512Mi --max-lifetime 1m
( until gone "$LIFE"; do sleep 3; done; secs_since "$LIFE_START" >"$TMP/life" ) &
LIFE_POLL=$!
new_pool "$POOL_RUN"
new_pool "$POOL_PAUSED" --warm-power-state Paused --warmup 30s

# ---- idle pause and wake on connect ---------------------------------------
if ! wait_until "$TIMEOUT" "$VM Running" phase_is "$VM" Running || ! agent_up "$VM"; then
  result idle-pause fail "$VM never got a guest agent: $(message "$VM")"
else
  t0="$(now_ms)"
  if wait_until 420 "$VM idle-paused" phase_is "$VM" Paused; then
    result idle-pause pass "agent up, then Paused by the controller after $(secs_since "$t0")s (idleTimeout 60s); lastActivityTime $(mfield "$VM" '((o.get("status") or {}).get("lifecycle") or {}).get("lastActivityTime") or "unset"')"
    t0="$(now_ms)"
    if out="$(K guest exec "$VM" -- cat /proc/uptime 2>&1)"; then
      result wake-paused pass "\`guest exec\` on the Paused Machine returned in $(secs_since "$t0")s (wake + exec); phase $(phase "$VM"), powerState $(mfield "$VM" '(o.get("spec") or {}).get("powerState") or ""')"
    else
      result wake-paused fail "exec failed after $(secs_since "$t0")s: ${out:0:200}"
    fi
  else
    result idle-pause fail "still $(phase "$VM") 7 minutes after the agent came up"
  fi

  K hibernate "$VM" >/dev/null 2>&1 || true
  if wait_until "$TIMEOUT" "$VM Hibernated" phase_is "$VM" Hibernated; then
    t0="$(now_ms)"
    if out="$(K guest exec "$VM" -- cat /proc/uptime 2>&1)"; then
      result wake-hibernated pass "\`guest exec\` on the Hibernated Machine returned in $(secs_since "$t0")s; same boot (uptime ${out%% *}s)"
    else
      result wake-hibernated fail "exec failed after $(secs_since "$t0")s: ${out:0:200}"
    fi
  else
    result wake-hibernated fail "never Hibernated: $(message "$VM")"
  fi
  # The rest needs the Machine to stay up.
  "$KUBECTL" -n "$NS" patch machine "$VM" --type=merge -p '{"spec":{"lifecycle":null,"powerState":"Running"}}' >/dev/null
  wait_until "$TIMEOUT" "$VM Running" phase_is "$VM" Running && agent_up "$VM" || true
fi

# ---- pools ----------------------------------------------------------------
for pool in "$POOL_RUN" "$POOL_PAUSED"; do
  if [[ "$pool" == "$POOL_RUN" ]]; then c=pool-running want=Running; else c=pool-paused want=Paused; fi
  if wait_until "$TIMEOUT" "$pool warm $want" warm_member "$pool" "$want"; then
    if ms="$(claim_ms "$pool")"; then
      result "$c" pass "claim bound in ${ms}ms against a warm $want member"
    else
      result "$c" fail "claim did not bind within 60s"
    fi
  else
    result "$c" fail "no warm $want member"
  fi
done

# ---- fork -----------------------------------------------------------------
if phase_is "$VM" Running; then
  t0="$(now_ms)"
  if out="$(K fork "$VM" --count 1 --prefix "$FORK" --wait 300s 2>&1)" && phase_is "$FORK-1" Running; then
    result fork pass "\`fork --count 1\` returned with the child Running in $(secs_since "$t0")s"
  else
    result fork fail "${out:0:200} / child $(phase "$FORK-1"): $(message "$FORK-1")"
  fi
  "$KUBECTL" -n "$NS" delete machine "$FORK-1" --wait=false >/dev/null 2>&1 || true
fi

# ---- freeze ---------------------------------------------------------------
if phase_is "$VM" Running; then
  t0="$(now_ms)"
  if K freeze "$VM" --unfreeze-timeout 1m >/dev/null 2>&1 && frozen "$VM"; then
    took="$(secs_since "$t0")"
    if wait_until 180 "$VM auto-thaw" thawed "$VM"; then
      result freeze-autothaw pass "frozen in ${took}s; kairon-node thawed it $(secs_since "$t0")s after the request (deadline 60s)"
    else
      result freeze-autothaw fail "still frozen 3 minutes after a 1m freeze"
    fi
  else
    result freeze-autothaw fail "freeze did not confirm: $(message "$VM")"
  fi
  t0="$(now_ms)"
  if K freeze "$VM" >/dev/null 2>&1 && frozen "$VM" && K unfreeze "$VM" >/dev/null 2>&1 && thawed "$VM" &&
    K guest exec "$VM" -- sh -c 'echo ok > /tmp/sbx-thaw && cat /tmp/sbx-thaw' >/dev/null 2>&1; then
    result freeze-unfreeze pass "freeze + unfreeze confirmed in $(secs_since "$t0")s; guest writes again"
  else
    result freeze-unfreeze fail "round trip failed; frozen='$(annotation "$VM" kairon.zyvor.dev/fs-frozen)'"
  fi
fi

# ---- vnc screenshot -------------------------------------------------------
if phase_is "$VM" Running; then
  if out="$(K vnc screenshot "$VM" -o "$TMP/shot.png" 2>&1)" &&
    dims="$(python3 -c '
import struct, sys
b = open(sys.argv[1], "rb").read(24)
assert b[:8] == b"\x89PNG\r\n\x1a\n"
w, h = struct.unpack(">II", b[16:24])
assert w > 0 and h > 0
print(f"{w}x{h}")' "$TMP/shot.png" 2>/dev/null)"; then
    result vnc-screenshot pass "PNG ${dims}, $(wc -c <"$TMP/shot.png" | tr -d ' ') bytes"
  else
    result vnc-screenshot fail "${out:0:200}"
  fi
fi

# ---- create clone from Halted ---------------------------------------------
K halt "$VM" >/dev/null 2>&1 || true
if wait_until "$TIMEOUT" "$VM Halted" phase_is "$VM" Halted; then
  t0="$(now_ms)"
  if out="$(K create clone "$VM" --name "$CLONE" --wait "${TIMEOUT}s" 2>&1)" && agent_up "$CLONE"; then
    up="$(K guest exec "$CLONE" -- cat /etc/hostname 2>/dev/null || true)"
    result create-clone pass "clone of the Halted Machine Running with its agent up in $(secs_since "$t0")s (disk flattened on the node); guest hostname ${up:-unknown}"
  else
    result create-clone fail "${out:0:200} / $(phase "$CLONE"): $(message "$CLONE")"
  fi
  "$KUBECTL" -n "$NS" delete machine "$CLONE" --wait=false >/dev/null 2>&1 || true

  # ---- guestfs read-only ---------------------------------------------------
  out="$(timeout 180 "$KAIRONCTL" -n "$NS" guestfs "$VM" < <(printf 'mountpoints\nexit\n'; sleep 150) 2>&1 | tr -d '\r' || true)"
  if grep -qE '^/dev/[a-z0-9]+: /$' <<<"$out"; then
    result guestfs-ro pass "guestfish --ro -i on the Halted Machine's disk: $(grep -cE '^/dev/[a-z0-9]+: ' <<<"$out") mountpoints"
  else
    result guestfs-ro fail "$(tail -c 200 <<<"$out")"
  fi
else
  result create-clone fail "never Halted: $(message "$VM")"
fi

# ---- cdrom insert / eject -------------------------------------------------
if [[ -z "$ISO_URL" || -z "$ISO_SHA" ]]; then
  result cdrom "not run" "needs --iso-url and --iso-sha256"
else
  K stop "$VM" >/dev/null 2>&1 || true
  sr0='for d in /sys/block/sr*; do echo "$(basename "$d") $(cat "$d/size")"; done'
  if ! wait_until "$TIMEOUT" "$VM Stopped" stopped_clean "$VM"; then
    result cdrom fail "never Stopped without a runtime: $(message "$VM")"
  elif ! out="$(K cdrom insert "$VM" bench --url "$ISO_URL" --sha256 "$ISO_SHA" 2>&1)"; then
    result cdrom fail "insert: ${out:0:200}"
  else
    K start "$VM" >/dev/null 2>&1 || true
    if wait_until "$TIMEOUT" "$VM Running" phase_is "$VM" Running && agent_up "$VM"; then
      before="$(K guest exec "$VM" -- sh -c "$sr0" 2>&1 | tr '\n' ' ')"
      K cdrom eject "$VM" bench >/dev/null 2>&1 || true
      sleep 20
      after="$(K guest exec "$VM" -- sh -c "$sr0" 2>&1 | tr '\n' ' ')"
      if grep -qE 'sr[0-9]+ [1-9]' <<<"$before" && ! grep -qE 'sr[0-9]+ [1-9]' <<<"$after"; then
        result cdrom pass "inserted while Stopped, guest saw media after start (${before% }); eject emptied it live (${after% })"
      else
        result cdrom fail "guest drives before eject: ${before:-none}; after: ${after:-none}"
      fi
    else
      result cdrom fail "did not start with the ISO: $(message "$VM")"
    fi
  fi
fi

# ---- lifetime -------------------------------------------------------------
if wait_until "$TIMEOUT" "$LIFE deleted" test -s "$TMP/life"; then
  result lifetime pass "--max-lifetime 1m Machine deleted by the controller $(cat "$TMP/life")s after creation"
else
  result lifetime fail "$LIFE still exists ($(phase "$LIFE")) after $(secs_since "$LIFE_START")s"
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in "${CASES[@]}"; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| Sandbox: $c | $r | $today | ${NODE:+Node $NODE, }${NOTE[$c]:-an earlier step failed} |"
done
exit "$rc"
