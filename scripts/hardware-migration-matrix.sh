#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Drives the hardware migration matrix against a prepared multi-host lab.
# Requires the environment documented in
# docs/runbook-multi-host-migration-test.md plus:
#
#   KAIRON_HW_LAB=1            # safety gate -- refuses to run otherwise
#   KAIRON_KUBE_URL / TOKEN    # cluster API (KAIRON_KUBE_CA = CA file path)
#   KAIRON_HW_MACHINE          # existing Machine to migrate back and forth
#   KAIRON_HW_TARGET_NODE      # the other node
#   KAIRON_HW_SOURCE_NODE      # optional; defaults to the Machine's node at start
#   KAIRON_HW_EBPF_MACHINE     # optional; Machine with dataplaneMode=ebpf (live-ebpf)
#   KAIRON_HW_IMAGE            # optional; image path for the create/stop/start/delete
#                              # smoke (without it, smoke checks KAIRON_HW_MACHINE health)
#   KAIRON_HW_SOURCE_SSH / KAIRON_HW_TARGET_SSH
#                              # user@host for the drills that inject failures
#   KAIRON_HW_CONTROLLER_SSH   # optional; restart a systemd controller instead of
#                              # deleting controller Pods
#   KAIRON_HW_REQUIRE_GUEST_IP # 1 (default): "healthy" also needs status.guestIP
#   KAIRON_UI_URL / KAIRON_UI_TOKEN
#                              # admin kairon-ui session for the guest-agent case,
#                              # which runs a command in KAIRON_HW_MACHINE (needs
#                              # spec.guestAgent.enabled) through ui -> node -> QGA,
#                              # and for the live case, which starts a memory-dirtying
#                              # load in the guest the same way ("not run" without it)
#   KAIRON_HW_LOAD_MIB         # guest RAM the live-case load rewrites (default 256)
#   KAIRON_HW_ADAPTIVE_LOAD_MIB / KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS
#                              # adaptive cases: guest load (default 1024) and a
#                              # starting bandwidth low enough that the load
#                              # outruns it (default 200), so a remedy must fire
#   KAIRON_HW_POSTCOPY_MACHINE # disposable Machine for postcopy-source-failure,
#                              # which kills its source QEMU after the post-copy
#                              # switch; the lab must recreate it before each run
#
# Flags: --case=NAME (repeatable), --namespace=, --timeout=, --write-compat
# (rewrite the matrix rows in docs/COMPATIBILITY.md). Exits 1 if any case
# fails; cases that can't run here are reported "not run" and don't fail.
set -euo pipefail

if [[ "${KAIRON_HW_LAB:-}" != "1" ]]; then
  echo "refusing: set KAIRON_HW_LAB=1 to acknowledge a real multi-host lab" >&2
  exit 2
fi
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"
REPO_ROOT="$LAB_REPO_ROOT"

CASES=()
NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
TIMEOUT="${KAIRON_HW_TIMEOUT:-600}"
WRITE_COMPAT=0
for arg in "$@"; do
  case "$arg" in
    --case=*) CASES+=("${arg#*=}") ;;
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --write-compat) WRITE_COMPAT=1 ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
if [[ ${#CASES[@]} -eq 0 ]]; then
  CASES=(smoke cold live guest-agent needs-recovery source-failure controller-failover
    live-adaptive-converge live-postcopy-switch live-zstd postcopy-source-failure)
  [[ -n "${KAIRON_HW_EBPF_MACHINE:-}" ]] && CASES+=(live-ebpf)
fi

MACHINE="${KAIRON_HW_MACHINE:?set KAIRON_HW_MACHINE}"
TARGET="${KAIRON_HW_TARGET_NODE:?set KAIRON_HW_TARGET_NODE}"
SOURCE="${KAIRON_HW_SOURCE_NODE:-$(machine_node "$NAMESPACE" "$MACHINE")}"
[[ -n "$SOURCE" && "$SOURCE" != "$TARGET" ]] || { echo "Machine $MACHINE is on '${SOURCE:-<none>}'; KAIRON_HW_TARGET_NODE must be the other node" >&2; exit 1; }
DATE="$(date -u +%Y-%m-%d)"
RESULTS=()
FAILED=0

# Titles must match the first column of the matrix table in docs/COMPATIBILITY.md.
T_SMOKE="Create / restart / delete smoke"
T_COLD="Cold migration"
T_LIVE="Live pre-copy under load"
T_EBPF="Live + eBPF dataplane"
T_SRC="Source failure during transfer"
T_RECOVERY="Ambiguous commit → NeedsRecovery"
T_CTRL="Controller failover mid-migration"
T_AGENT="Guest agent exec after migration"
T_ADAPT="Adaptive: auto-converge under load"
T_PCSWITCH="Adaptive: post-copy switch under load"
T_ZSTD="Adaptive: zstd multifd compression"
T_PCFAIL="Source failure after post-copy → NeedsRecovery"

record() {
  local title="$1" result="$2" notes="${3:-}"
  RESULTS+=("${title}"$'\t'"${result}"$'\t'"${notes}")
  [[ "$result" == "fail" ]] && FAILED=$((FAILED + 1))
  echo "==> ${title}: ${result} ${notes}"
}

# Where the next migration of $1 should go: the node it isn't on.
next_target() { other_node "$(machine_node "$NAMESPACE" "$1")" "$SOURCE" "$TARGET"; }

# migrate_and_verify MACHINE STRATEGY [MIGRATE_FLAGS...] -- migration
# Succeeded and the Machine is healthy on the new node.
migrate_and_verify() {
  local machine="$1" strategy="$2" dest mig
  shift 2
  dest="$(next_target "$machine")"
  mig="$(start_migration "$NAMESPACE" "$machine" "$dest" "$strategy" "hw-${case:-$strategy}" "$@")"
  wait_until "$TIMEOUT" "$mig terminal" migration_in "$NAMESPACE" "$mig" Succeeded Failed Blocked Cancelled NeedsRecovery || return 1
  local phase
  phase="$(migration_phase "$NAMESPACE" "$mig")"
  [[ "$phase" == "Succeeded" ]] || { lab_log "$mig ended $phase"; return 1; }
  wait_until 300 "$machine healthy on $dest" machine_healthy "$NAMESPACE" "$machine" "$dest" || return 1
  LAST_MIGRATION="$mig"
  NOTE="$mig → $dest"
  if [[ "$strategy" == "live" || "$strategy" == "adaptive" ]]; then
    NOTE+="$(migration_field "$NAMESPACE" "$mig" '"; downtime %sms, total %sms" % ((o.get("status") or {}).get("downtimeMs", "?"), (o.get("status") or {}).get("totalTimeMs", "?"))' 2>/dev/null || true)"
  fi
}

# live_under_load MACHINE -- live migration while the guest dirties
# KAIRON_HW_LOAD_MIB of RAM. Without kairon-ui credentials there is no way
# to start the load, so the row is "not run" rather than an idle-guest pass.
live_under_load() {
  local machine="$1"
  if [[ -z "${KAIRON_UI_URL:-}" || -z "${KAIRON_UI_TOKEN:-}" ]]; then
    LOAD_SKIPPED=1
    NOTE="set KAIRON_UI_URL and KAIRON_UI_TOKEN to generate guest load"
    return 1
  fi
  local mib="${KAIRON_HW_LOAD_MIB:-256}"
  if ! start_guest_load "$NAMESPACE" "$machine" "$mib"; then
    NOTE="could not start guest load in ${machine}"
    return 1
  fi
  sleep 10
  local rc=0
  migrate_and_verify "$machine" live || rc=1
  stop_guest_load "$NAMESPACE" "$machine"
  NOTE+="; guest dirtying ${mib}MiB"
  return "$rc"
}

# adaptive_check EXPR DESCRIPTION -- EXPR (json_eval over $LAST_MIGRATION)
# must be "True"; otherwise NOTE gets the description and the engine's own
# stage, message and actions.
adaptive_check() {
  local got
  got="$(migration_field "$NAMESPACE" "$LAST_MIGRATION" "$1" 2>/dev/null || true)"
  [[ "$got" == "True" ]] && return 0
  NOTE+="; expected $2, got $(migration_field "$NAMESPACE" "$LAST_MIGRATION" '"stage %s: %s; actions %s" % (((o.get("status") or {}).get("adaptive") or {}).get("stage"), ((o.get("status") or {}).get("adaptive") or {}).get("message"), [a.get("action") for a in ((o.get("status") or {}).get("adaptive") or {}).get("actions") or []])' 2>/dev/null || echo "no status.adaptive")"
  return 1
}

ADAPTIVE_ACTIONS='[a.get("action") for a in (((o.get("status") or {}).get("adaptive") or {}).get("actions") or [])]'
ADAPTIVE_POSTCOPY='bool(((o.get("status") or {}).get("adaptive") or {}).get("postCopyStarted"))'

# adaptive_under_load MACHINE CHECK DESCRIPTION MIGRATE_FLAGS... -- an
# adaptive migration while the guest dirties KAIRON_HW_ADAPTIVE_LOAD_MIB of
# RAM through a link capped at KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS, then CHECK.
adaptive_under_load() {
  local machine="$1" check="$2" desc="$3"
  shift 3
  if [[ -z "${KAIRON_UI_URL:-}" || -z "${KAIRON_UI_TOKEN:-}" ]]; then
    LOAD_SKIPPED=1
    NOTE="set KAIRON_UI_URL and KAIRON_UI_TOKEN to generate guest load"
    return 1
  fi
  local mib="${KAIRON_HW_ADAPTIVE_LOAD_MIB:-1024}" bw="${KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS:-200}"
  if ! start_guest_load "$NAMESPACE" "$machine" "$mib"; then
    NOTE="could not start guest load in ${machine}"
    return 1
  fi
  sleep 10
  local rc=0
  migrate_and_verify "$machine" adaptive --bandwidth-mbps="$bw" "$@" || rc=1
  stop_guest_load "$NAMESPACE" "$machine"
  NOTE+="; guest dirtying ${mib}MiB at ${bw}Mbps"
  (( rc == 0 )) || return 1
  adaptive_check "$check" "$desc"
}

# run_adaptive TITLE MACHINE CHECK DESCRIPTION MIGRATE_FLAGS...
run_adaptive() {
  local title="$1"
  shift
  LOAD_SKIPPED=0
  if adaptive_under_load "$@"; then
    record "$title" pass "$NOTE"
  elif [[ "$LOAD_SKIPPED" == "1" ]]; then
    record "$title" "not run" "$NOTE"
  else
    record "$title" fail "$NOTE"
  fi
}

smoke() {
  if [[ -z "${KAIRON_HW_IMAGE:-}" ]]; then
    if machine_healthy "$NAMESPACE" "$MACHINE"; then
      NOTE="KAIRON_HW_IMAGE unset: checked ${MACHINE} Running/Ready only"
      return 0
    fi
    NOTE="${MACHINE} not healthy"
    return 1
  fi
  local name path
  name="hwsmoke-$(date -u +%H%M%S)"
  path="$(machine_path "$NAMESPACE" "$name")"
  "$KAIRONCTL" create "$name" --namespace "$NAMESPACE" --image="$KAIRON_HW_IMAGE" --cpu=1 --memory=1Gi >&2
  local ok=0
  {
    wait_until "$TIMEOUT" "$name healthy" machine_healthy "$NAMESPACE" "$name" &&
      "$KAIRONCTL" stop "$name" --namespace "$NAMESPACE" >&2 &&
      wait_until "$TIMEOUT" "$name Stopped" machine_is "$NAMESPACE" "$name" '(o.get("status") or {}).get("phase") == "Stopped"' &&
      "$KAIRONCTL" start "$name" --namespace "$NAMESPACE" >&2 &&
      wait_until "$TIMEOUT" "$name healthy again" machine_healthy "$NAMESPACE" "$name"
  } || ok=1
  "$KAIRONCTL" delete machine "$name" --namespace "$NAMESPACE" >&2 || ok=1
  wait_until "$TIMEOUT" "$name deleted" machine_gone "$path" || ok=1
  NOTE="$name from ${KAIRON_HW_IMAGE}: create → healthy → stop → start → delete"
  return "$ok"
}

# guest_agent_exec -- runs `echo <nonce>` in $MACHINE through kairon-ui's
# exec relay and checks the exit code and output.
guest_agent_exec() {
  if [[ "$(machine_field "$NAMESPACE" "$MACHINE" '((o.get("spec") or {}).get("guestAgent") or {}).get("enabled") or False')" != "True" ]]; then
    NOTE="${MACHINE} has no spec.guestAgent.enabled"
    return 1
  fi
  local nonce out
  nonce="kairon-hw-$(date -u +%s)-$RANDOM"
  out="$(guest_exec "$NAMESPACE" "$MACHINE" "echo ${nonce}")" || { NOTE="exec request failed"; return 1; }
  if ! python3 -c 'import json,sys; r=json.loads(sys.argv[1]); sys.exit(0 if r.get("exitCode")==0 and sys.argv[2] in r.get("stdout","") else 1)' "$out" "$nonce"; then
    NOTE="unexpected exec result: ${out:0:200}"
    return 1
  fi
  NOTE="${MACHINE} on $(machine_node "$NAMESPACE" "$MACHINE"): echo via QGA exit 0"
}

machine_gone() {
  local rc=0
  kube_exists "$1" || rc=$?
  [[ $rc -eq 1 ]]
}

require_dataplane_mode() {
  [[ "$(machine_field "$NAMESPACE" "$1" '((o.get("spec") or {}).get("network") or {}).get("dataplaneMode") or ""')" == "$2" ]]
}

# drill TITLE SCRIPT ARGS... -- runs an injection script if present.
drill() {
  local title="$1" script="$2"; shift 2
  if [[ ! -x "$REPO_ROOT/scripts/$script" ]]; then
    record "$title" "not run" "scripts/$script missing"
    return
  fi
  local dest
  dest="$(next_target "$MACHINE")"
  if "$REPO_ROOT/scripts/$script" --namespace="$NAMESPACE" --machine="$MACHINE" --target="$dest" "$@"; then
    record "$title" "pass" "$script → $dest"
  else
    record "$title" "fail" "$script → $dest"
  fi
}

for case in "${CASES[@]}"; do
  NOTE=""
  case "$case" in
    smoke)
      if smoke; then record "$T_SMOKE" pass "$NOTE"; else record "$T_SMOKE" fail "$NOTE"; fi
      ;;
    cold)
      if migrate_and_verify "$MACHINE" cold; then record "$T_COLD" pass "$NOTE"; else record "$T_COLD" fail "$NOTE"; fi
      ;;
    live)
      LOAD_SKIPPED=0
      if live_under_load "$MACHINE"; then
        record "$T_LIVE" pass "$NOTE"
      elif [[ "$LOAD_SKIPPED" == "1" ]]; then
        record "$T_LIVE" "not run" "$NOTE"
      else
        record "$T_LIVE" fail "$NOTE"
      fi
      ;;
    guest-agent)
      if [[ -z "${KAIRON_UI_URL:-}" || -z "${KAIRON_UI_TOKEN:-}" ]]; then
        record "$T_AGENT" "not run" "set KAIRON_UI_URL and KAIRON_UI_TOKEN"
      elif guest_agent_exec; then
        record "$T_AGENT" pass "$NOTE"
      else
        record "$T_AGENT" fail "$NOTE"
      fi
      ;;
    live-ebpf)
      EBPF_MACHINE="${KAIRON_HW_EBPF_MACHINE:-$MACHINE}"
      if ! require_dataplane_mode "$EBPF_MACHINE" ebpf; then
        record "$T_EBPF" fail "Machine ${EBPF_MACHINE} missing dataplaneMode=ebpf"
      elif migrate_and_verify "$EBPF_MACHINE" live; then
        record "$T_EBPF" pass "$NOTE"
      else
        record "$T_EBPF" fail "Machine ${EBPF_MACHINE} $NOTE"
      fi
      ;;
    needs-recovery)
      if [[ -n "${KAIRON_HW_TARGET_SSH:-}${KAIRON_HW_SOURCE_SSH:-}" ]]; then
        dest="$(next_target "$MACHINE")"
        ssh_for_dest="$KAIRON_HW_TARGET_SSH"
        [[ "$dest" == "$SOURCE" ]] && ssh_for_dest="${KAIRON_HW_SOURCE_SSH:-}"
        drill "$T_RECOVERY" recovery-drill.sh --dest-ssh="$ssh_for_dest"
      else
        record "$T_RECOVERY" "not run" "set KAIRON_HW_SOURCE_SSH/KAIRON_HW_TARGET_SSH"
      fi
      ;;
    source-failure)
      if [[ -n "${KAIRON_HW_SOURCE_SSH:-}" && -n "${KAIRON_HW_TARGET_SSH:-}" ]]; then
        src_ssh="$KAIRON_HW_SOURCE_SSH"
        [[ "$(machine_node "$NAMESPACE" "$MACHINE")" == "$TARGET" ]] && src_ssh="$KAIRON_HW_TARGET_SSH"
        drill "$T_SRC" lab-inject-source-failure.sh --source-ssh="$src_ssh"
      else
        record "$T_SRC" "not run" "set KAIRON_HW_SOURCE_SSH and KAIRON_HW_TARGET_SSH"
      fi
      ;;
    controller-failover)
      drill "$T_CTRL" lab-inject-controller-failover.sh
      ;;
    live-adaptive-converge)
      run_adaptive "$T_ADAPT" "$MACHINE" \
        "any(a in (\"auto-converge\", \"throttle\") for a in ${ADAPTIVE_ACTIONS}) and not ${ADAPTIVE_POSTCOPY} and bool((o.get(\"status\") or {}).get(\"progress\"))" \
        "an auto-converge/throttle action, no post-copy, and status.progress" \
        --auto-converge --progress-timeout=20s --max-downtime-ms=300 --completion-timeout-per-gib=300s
      ;;
    live-postcopy-switch)
      run_adaptive "$T_PCSWITCH" "$MACHINE" \
        "\"postcopy\" in ${ADAPTIVE_ACTIONS} and ${ADAPTIVE_POSTCOPY}" \
        "a postcopy action and status.adaptive.postCopyStarted" \
        --post-copy --post-copy-after=20s --max-downtime-ms=300 --completion-timeout-per-gib=300s
      ;;
    live-zstd)
      # Idle guest memory compresses well; the load scripts' urandom
      # fallback doesn't, so this case runs without load.
      if migrate_and_verify "$MACHINE" adaptive --compression=zstd --compression-level=3 &&
        adaptive_check '((o.get("status") or {}).get("progress") or {}).get("compressionRatioPercent", 0) > 100' "status.progress.compressionRatioPercent > 100"; then
        record "$T_ZSTD" pass "$NOTE; ratio $(migration_field "$NAMESPACE" "$LAST_MIGRATION" '((o.get("status") or {}).get("progress") or {}).get("compressionRatioPercent")')%"
      else
        record "$T_ZSTD" fail "$NOTE"
      fi
      ;;
    postcopy-source-failure)
      PC_MACHINE="${KAIRON_HW_POSTCOPY_MACHINE:-}"
      if [[ -z "$PC_MACHINE" || -z "${KAIRON_HW_SOURCE_SSH:-}" || -z "${KAIRON_HW_TARGET_SSH:-}" ]]; then
        record "$T_PCFAIL" "not run" "set KAIRON_HW_POSTCOPY_MACHINE (disposable), KAIRON_HW_SOURCE_SSH and KAIRON_HW_TARGET_SSH"
      elif [[ ! -x "$REPO_ROOT/scripts/lab-inject-postcopy-source-failure.sh" ]]; then
        record "$T_PCFAIL" "not run" "scripts/lab-inject-postcopy-source-failure.sh missing"
      else
        pc_dest="$(next_target "$PC_MACHINE")"
        pc_ssh="$KAIRON_HW_SOURCE_SSH"
        [[ "$(machine_node "$NAMESPACE" "$PC_MACHINE")" == "$TARGET" ]] && pc_ssh="$KAIRON_HW_TARGET_SSH"
        if "$REPO_ROOT/scripts/lab-inject-postcopy-source-failure.sh" --namespace="$NAMESPACE" --machine="$PC_MACHINE" \
          --target="$pc_dest" --source-ssh="$pc_ssh" --disposable --recover \
          --bandwidth-mbps="${KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS:-200}" --load-mib="${KAIRON_HW_ADAPTIVE_LOAD_MIB:-1024}"; then
          record "$T_PCFAIL" pass "${PC_MACHINE} → ${pc_dest}: source QEMU killed after the switch; NeedsRecovery, then ForceAbort"
        else
          record "$T_PCFAIL" fail "${PC_MACHINE} → ${pc_dest}; see the drill log"
        fi
      fi
      ;;
    *)
      echo "unknown case $case" >&2
      exit 1
      ;;
  esac
done

echo
echo "## Matrix rows"
for r in "${RESULTS[@]}"; do
  IFS=$'\t' read -r title result notes <<<"$r"
  echo "| ${title} | ${result} | ${DATE} | ${notes} |"
done

if [[ "$WRITE_COMPAT" == "1" ]]; then
  printf '%s\n' "${RESULTS[@]}" | python3 "$REPO_ROOT/scripts/lib/write_compat.py" "$REPO_ROOT/docs/COMPATIBILITY.md" "$DATE"
  echo "updated docs/COMPATIBILITY.md"
fi

if (( FAILED > 0 )); then
  echo "FAILED: ${FAILED} case(s)" >&2
  exit 1
fi
