#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
#
# Runs the per-feature lab drills (scripts/lab-*.sh) on the lab and records
# each drill's rows in docs/COMPATIBILITY.md, so the stability check
# (scripts/stability-check.py) has dated evidence. The nightly job
# (.github/workflows/lab-features.yml) calls it; it also works by hand.
#
# Usage: lab-run.sh [--write-compat] [--list] [FEATURE...]
#   no FEATURE runs every feature whose inputs are set.
#
# Inputs (env, all from the lab runner's vars/secrets):
#   KAIRON_KUBE_URL / KAIRON_KUBE_TOKEN / KAIRON_KUBE_CA   (lib/lab.sh)
#   KAIRON_LAB_NODE       node the single-node drills run on
#   KAIRON_LAB_IMAGE      boot image path on that node
#   KAIRON_LAB_NODE_B     second node (two-node rows), optional
#   KAIRON_LAB_PRESSURE_CMD / KAIRON_LAB_RELIEF_CMD   elastic memory
#   KAIRON_LAB_CBT_MACHINE, KAIRON_LAB_MINIO_SSH       incremental backups
# A drill missing an input is reported "skipped", never "pass".
#
# Exit: 1 if any row is `fail` or a drill exits non-zero; 0 otherwise.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$ROOT/scripts/lib/lab.sh"

WRITE=0; LIST=0; WANT=()
for a in "$@"; do
  case "$a" in
    --write-compat) WRITE=1 ;;
    --list) LIST=1 ;;
    -*) echo "unknown flag $a" >&2; exit 2 ;;
    *) WANT+=("$a") ;;
  esac
done

NODE="${KAIRON_LAB_NODE:-}"; IMAGE="${KAIRON_LAB_IMAGE:-}"

# feature -> "script|required env vars|extra args"
declare -A SPEC=(
  [maintenance]="lab-maintenance.sh|NODE IMAGE|"
  [drs-pressure]="lab-drs-pressure.sh|NODE IMAGE|"
  [hibernate]="lab-hibernate.sh|NODE IMAGE|"
  [incident-bundle]="lab-incident-bundle.sh|NODE IMAGE|"
  [nvmeof]="lab-nvmeof.sh|NODE IMAGE|"
  [topology]="lab-topology.sh|NODE IMAGE|"
  [accel]="lab-accel.sh|NODE IMAGE|"
  [vgpu]="lab-vgpu.sh|NODE IMAGE|"
  [virtual-network]="lab-virtual-network.sh|NODE IMAGE|"
  [elastic-memory]="lab-elastic-memory.sh|NODE IMAGE KAIRON_LAB_PRESSURE_CMD KAIRON_LAB_RELIEF_CMD|"
  [cbt-backup]="lab-cbt-backup.sh|KAIRON_LAB_CBT_MACHINE KAIRON_LAB_MINIO_SSH|"
  [dr]="lab-dr.sh|IMAGE|"
  [mac-fabric]="lab-mac-fabric.sh|NODE|"
)
ORDER=(maintenance drs-pressure hibernate incident-bundle nvmeof topology accel vgpu virtual-network elastic-memory cbt-backup dr mac-fabric)

if (( LIST )); then printf '%s\n' "${ORDER[@]}"; exit 0; fi
((${#WANT[@]})) || WANT=("${ORDER[@]}")

missing() { # names of unset inputs for a spec
  local v out=()
  for v in $1; do
    case "$v" in
      NODE) [[ -n "$NODE" ]] || out+=(KAIRON_LAB_NODE) ;;
      IMAGE) [[ -n "$IMAGE" ]] || out+=(KAIRON_LAB_IMAGE) ;;
      *) [[ -n "${!v:-}" ]] || out+=("$v") ;;
    esac
  done
  echo "${out[*]:-}"
}

LOGDIR="${LAB_LOG_DIR:-$ROOT/lab-logs}"; mkdir -p "$LOGDIR"
ROWS="$LOGDIR/rows.tsv"; : > "$ROWS"
SUMMARY="$LOGDIR/summary.md"; printf '| Drill | Outcome | Rows |\n|---|---|---|\n' > "$SUMMARY"
FAILED=0

for f in "${WANT[@]}"; do
  spec="${SPEC[$f]:-}"
  [[ -n "$spec" ]] || { echo "unknown feature $f" >&2; exit 2; }
  IFS='|' read -r script need _ <<< "$spec"
  miss="$(missing "$need")"
  if [[ -n "$miss" ]]; then
    lab_log "$f: skipped (unset: $miss)"
    echo "| $f | skipped | needs $miss |" >> "$SUMMARY"
    continue
  fi
  case "$f" in
    cbt-backup) args=(--machine="$KAIRON_LAB_CBT_MACHINE" --disposable --minio-ssh="$KAIRON_LAB_MINIO_SSH") ;;
    elastic-memory) args=(--node="$NODE" --image="$IMAGE" --pressure-cmd="$KAIRON_LAB_PRESSURE_CMD" --relief-cmd="$KAIRON_LAB_RELIEF_CMD") ;;
    dr) args=(--image="$IMAGE" --node="$NODE") ;;
    mac-fabric) args=(--node="$NODE") ;;
    *) args=(--node="$NODE" --image="$IMAGE") ;;
  esac
  lab_log "$f: running $script"
  logf="$LOGDIR/$f.log"
  rc=0
  bash "$ROOT/scripts/$script" "${args[@]}" > "$logf" 2> "$LOGDIR/$f.err" || rc=$?
  # Rows look like: | Case | result | date | notes |
  n=0; fails=0
  while IFS= read -r line; do
    [[ "$line" == "|"* ]] || continue
    IFS='|' read -r _ c r _d notes _ <<< "$line"
    c="$(echo "$c" | xargs)"; r="$(echo "$r" | xargs)"; notes="$(echo "$notes" | xargs)"
    [[ -n "$c" && "$c" != "Case" && "$c" != --* ]] || continue
    printf '%s\t%s\t%s\n' "$c" "$r" "$notes" >> "$ROWS"
    n=$((n + 1)); [[ "$r" == fail* ]] && fails=$((fails + 1))
  done < "$logf"
  if (( rc != 0 || fails > 0 )); then
    FAILED=1; outcome="FAILED (exit $rc, $fails failing rows)"
  elif (( n == 0 )); then
    FAILED=1; outcome="FAILED (no rows printed)"
  else
    outcome="ok"
  fi
  echo "| $f | $outcome | $n |" >> "$SUMMARY"
done

cat "$SUMMARY"
[[ -n "${GITHUB_STEP_SUMMARY:-}" ]] && cat "$SUMMARY" >> "$GITHUB_STEP_SUMMARY"
if (( WRITE )) && [[ -s "$ROWS" ]]; then
  python3 "$ROOT/scripts/lib/write_compat.py" "$ROOT/docs/COMPATIBILITY.md" "$(date -u +%F)" < "$ROWS" || FAILED=1
fi
exit "$FAILED"
