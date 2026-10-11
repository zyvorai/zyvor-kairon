#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Incremental S3 backup drill on one node (experimental feature; see
# docs/guides/machine-backup-incremental.md). Against a disposable,
# image-booted, guest-agent-enabled Machine running on a node labelled
# kairon.zyvor.dev/cbt=true, it checks:
#
#   changed-bytes  full backup, dirty --dirty-mib in the guest, incremental:
#                  status.changedBytes is at least --dirty-mib and at most
#                  --dirty-mib + --slack-mib
#   restore        corrupt the guest file, halt, restore the incremental,
#                  start: the file's sha256 matches the pre-corruption one
#   synthesize     synthetic full of the incremental, delete the full,
#                  corrupt, restore the incremental again: sha256 matches
#   forced-full    fresh full + incremental, SIGKILL the Machine's QEMU, bring
#                  it back: the next incremental fails with
#                  status.disk.fullRequired and a full succeeds
#
# and prints COMPATIBILITY.md rows. The guest's disk is overwritten twice by
# restores, hence --disposable.
#
# S3: pass --s3-endpoint/--s3-bucket/--s3-credentials (a Secret holding
# accessKeyId/secretAccessKey), or --minio-ssh=user@host to start a MinIO
# container there (docker or podman), create the bucket and the labelled
# Secret; --s3-endpoint then defaults to http://HOST:9000.
#
# Usage:
#   lab-cbt-backup.sh --machine=NAME --disposable [--namespace=ns]
#       [--node-ssh=user@host] [--minio-ssh=user@host]
#       [--s3-endpoint=URL] [--s3-bucket=NAME] [--s3-credentials=SECRET]
#       [--file-mib=256] [--dirty-mib=64] [--slack-mib=64] [--timeout=1800]
#       [--kill-cmd=CMD] [--cleanup]
#
# Needs KAIRON_KUBE_* (lib/lab.sh), KAIRON_UI_URL/KAIRON_UI_TOKEN for guest
# exec, and for forced-full passwordless SSH + sudo pkill on the node
# (--node-ssh, default KAIRON_HW_SOURCE_SSH; the case is "not run" without it).
# --kill-cmd runs there; {runtime} is the Machine's FluxVM runtime ID.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR source=lib/lab.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/lab.sh"

NAMESPACE="${KAIRON_HW_NAMESPACE:-default}"
MACHINE=""; DISPOSABLE=0; CLEANUP=0
NODE_SSH="${KAIRON_HW_SOURCE_SSH:-}"; MINIO_SSH=""
S3_ENDPOINT=""; S3_BUCKET="kairon-cbt-lab"; S3_CREDS="kairon-cbt-lab-s3"
FILE_MIB=256; DIRTY_MIB=64; SLACK_MIB=64; TIMEOUT=1800
KILL_CMD="sudo pkill -9 -f '[q]emu-system.*{runtime}'"
for arg in "$@"; do
  case "$arg" in
    --namespace=*) NAMESPACE="${arg#*=}" ;;
    --machine=*) MACHINE="${arg#*=}" ;;
    --node-ssh=*) NODE_SSH="${arg#*=}" ;;
    --minio-ssh=*) MINIO_SSH="${arg#*=}" ;;
    --s3-endpoint=*) S3_ENDPOINT="${arg#*=}" ;;
    --s3-bucket=*) S3_BUCKET="${arg#*=}" ;;
    --s3-credentials=*) S3_CREDS="${arg#*=}" ;;
    --file-mib=*) FILE_MIB="${arg#*=}" ;;
    --dirty-mib=*) DIRTY_MIB="${arg#*=}" ;;
    --slack-mib=*) SLACK_MIB="${arg#*=}" ;;
    --timeout=*) TIMEOUT="${arg#*=}" ;;
    --kill-cmd=*) KILL_CMD="${arg#*=}" ;;
    --disposable) DISPOSABLE=1 ;;
    --cleanup) CLEANUP=1 ;;
    *) echo "unknown flag $arg" >&2; exit 1 ;;
  esac
done
[[ -n "$MACHINE" ]] || { echo "usage: $0 --machine=NAME --disposable [--minio-ssh=user@host | --s3-endpoint=URL ...]" >&2; exit 1; }
[[ "$DISPOSABLE" == "1" ]] || { echo "refusing: restores overwrite ${MACHINE}'s disks; pass --disposable" >&2; exit 2; }
(( DIRTY_MIB < FILE_MIB )) || { echo "--dirty-mib must be below --file-mib" >&2; exit 1; }
: "${KAIRON_UI_URL:?set KAIRON_UI_URL (guest exec)}" "${KAIRON_UI_TOKEN:?set KAIRON_UI_TOKEN}"

GUEST_FILE=/var/tmp/kairon-cbt.bin
STAMP="$(date -u +%m%d%H%M%S)"
FULL="cbt-lab-full-$STAMP"; INCR="cbt-lab-incr-$STAMP"; SYNTH="cbt-lab-synth-$STAMP"
F2="cbt-lab-full2-$STAMP"; I2="cbt-lab-incr2-$STAMP"; I3="cbt-lab-incr3-$STAMP"; F3="cbt-lab-full3-$STAMP"
declare -A RESULT NOTE

backup_path() { echo "$LAB_API/namespaces/$NAMESPACE/machinebackups/$1"; }
restore_path() { echo "$LAB_API/namespaces/$NAMESPACE/machinebackuprestores/$1"; }
backup_field() { kube_curl GET "$(backup_path "$1")" | json_eval "$2"; }
backup_phase() { backup_field "$1" '(o.get("status") or {}).get("phase") or ""' 2>/dev/null || true; }
backup_done() { [[ "$(backup_phase "$1")" == Succeeded || "$(backup_phase "$1")" == Failed ]]; }
backup_gone() { local rc=0; kube_exists "$(backup_path "$1")" || rc=$?; [[ $rc == 1 ]]; }
restore_done() {
  local p
  p="$(kube_curl GET "$(restore_path "$1")" 2>/dev/null | json_eval '(o.get("status") or {}).get("phase") or ""' 2>/dev/null || true)"
  [[ "$p" == Succeeded || "$p" == Failed ]]
}
restore_phase() { kube_curl GET "$(restore_path "$1")" | json_eval '(o.get("status") or {}).get("phase") or ""'; }

# guest SCRIPT -- runs SCRIPT in the guest, prints stdout, fails on non-zero exit.
guest() {
  local out
  out="$(guest_exec "$NAMESPACE" "$MACHINE" "$1" "${2:-120}")" || return 1
  python3 -c 'import json,sys; r=json.loads(sys.argv[1]); sys.stdout.write(r.get("stdout","")); sys.exit(r.get("exitCode") or 0)' "$out"
}
guest_ready() { guest true 10 >/dev/null 2>&1; }
guest_sum() { guest "sha256sum $GUEST_FILE | cut -d' ' -f1"; }
machine_stopped() { [[ "$(machine_phase "$NAMESPACE" "$MACHINE" 2>/dev/null)" == Stopped ]]; }
wait_guest() {
  wait_until 600 "$MACHINE healthy" machine_healthy "$NAMESPACE" "$MACHINE" &&
    wait_until 300 "$MACHINE guest agent" guest_ready
}

# backup NAME [kaironctl backup create flags...] -- creates, waits, prints phase.
backup() {
  local name="$1"; shift
  "$KAIRONCTL" backup create "$MACHINE" --namespace "$NAMESPACE" --name "$name" "$@" >&2
  wait_until "$TIMEOUT" "backup $name" backup_done "$name" || true
  lab_log "backup $name: $(backup_phase "$name") $(backup_field "$name" '(o.get("status") or {}).get("message") or ""' 2>/dev/null || true)"
  backup_phase "$name"
}
s3_flags() { echo --s3-endpoint "$S3_ENDPOINT" --s3-bucket "$S3_BUCKET" --s3-credentials "$S3_CREDS" --s3-prefix "lab-$STAMP"; }

# restore_into BACKUP -- halts the Machine, restores BACKUP, starts it again.
restore_into() {
  local backup="$1" name="cbt-lab-restore-$STAMP-$RANDOM"
  "$KAIRONCTL" halt "$MACHINE" --namespace "$NAMESPACE" >&2
  wait_until 600 "$MACHINE Stopped" machine_stopped || return 1
  "$KAIRONCTL" backup restore "$backup" --namespace "$NAMESPACE" --name "$name" >&2
  wait_until "$TIMEOUT" "restore $name" restore_done "$name" || return 1
  local phase
  phase="$(restore_phase "$name")"
  kube_curl DELETE "$(restore_path "$name")" >/dev/null 2>&1 || true
  "$KAIRONCTL" start "$MACHINE" --namespace "$NAMESPACE" >&2
  [[ "$phase" == Succeeded ]] || { lab_log "restore $name: $phase"; return 1; }
  wait_guest
}

corrupt() { guest "dd if=/dev/zero of=$GUEST_FILE bs=1M count=$FILE_MIB conv=notrunc,fsync 2>/dev/null && sync" 300 >/dev/null; }

result() { RESULT[$1]="$2"; NOTE[$1]="$3"; lab_log "$1: $2 -- $3"; }

start_minio() {
  local host="${MINIO_SSH#*@}" user pass
  user="kairon$RANDOM"; pass="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
  S3_ENDPOINT="${S3_ENDPOINT:-http://$host:9000}"
  lab_log "starting MinIO on $MINIO_SSH ($S3_ENDPOINT, bucket $S3_BUCKET with Object Lock)"
  # shellcheck disable=SC2016 # expanded on the MinIO host
  lab_ssh "$MINIO_SSH" "U='$user' P='$pass' B='$S3_BUCKET' sh -s" <<'EOF'
set -e
rt=$(command -v docker || command -v podman) || { echo "no docker or podman" >&2; exit 1; }
sudo "$rt" rm -f kairon-cbt-minio >/dev/null 2>&1 || true
sudo "$rt" run -d --name kairon-cbt-minio -p 9000:9000 -e MINIO_ROOT_USER="$U" -e MINIO_ROOT_PASSWORD="$P" \
  quay.io/minio/minio server /data >/dev/null
for _ in $(seq 30); do curl -fsS http://127.0.0.1:9000/minio/health/ready >/dev/null 2>&1 && break; sleep 1; done
sudo "$rt" run --rm --network host --entrypoint sh quay.io/minio/mc -c \
  "mc alias set lab http://127.0.0.1:9000 $U $P >/dev/null && mc mb --ignore-existing --with-lock lab/$B"
EOF
  local body
  body="$(python3 -c 'import json,sys; print(json.dumps({"apiVersion":"v1","kind":"Secret","metadata":{"name":sys.argv[1],"labels":{"kairon.zyvor.dev/backup-credentials":"true"}},"stringData":{"accessKeyId":sys.argv[2],"secretAccessKey":sys.argv[3]}}))' "$S3_CREDS" "$user" "$pass")"
  kube_curl DELETE "api/v1/namespaces/$NAMESPACE/secrets/$S3_CREDS" >/dev/null 2>&1 || true
  kube_curl POST "api/v1/namespaces/$NAMESPACE/secrets" "$body" >/dev/null
}

cleanup() {
  [[ "$CLEANUP" == "1" ]] || return 0
  local b
  for b in "$F3" "$I3" "$I2" "$F2" "$SYNTH" "$INCR" "$FULL"; do kube_curl DELETE "$(backup_path "$b")" >/dev/null 2>&1 || true; done
  [[ -n "$MINIO_SSH" ]] && lab_ssh "$MINIO_SSH" 'sudo "$(command -v docker || command -v podman)" rm -f kairon-cbt-minio' >/dev/null 2>&1 || true
}
trap cleanup EXIT

[[ -n "$MINIO_SSH" ]] && start_minio
[[ -n "$S3_ENDPOINT" ]] || { echo "set --s3-endpoint or --minio-ssh" >&2; exit 1; }
NODE="$(machine_node "$NAMESPACE" "$MACHINE")"
[[ -n "$NODE" ]] || { echo "Machine $MACHINE is not on a node" >&2; exit 1; }
cbt="$(kube_curl GET "api/v1/nodes/$NODE" | json_eval '(o["metadata"].get("labels") or {}).get("kairon.zyvor.dev/cbt") or ""')"
[[ "$cbt" == "true" ]] || { echo "node $NODE lacks kairon.zyvor.dev/cbt=true (FluxVM without S3 incremental backups?)" >&2; exit 1; }
wait_guest
# shellcheck disable=SC2207
S3=($(s3_flags))

lab_log "writing ${FILE_MIB}MiB pattern to $GUEST_FILE"
guest "dd if=/dev/urandom of=$GUEST_FILE bs=1M count=$FILE_MIB conv=fsync 2>/dev/null && sync" 600 >/dev/null
FULL_OK=0; INCR_OK=0
if [[ "$(backup "$FULL" "${S3[@]}")" == Succeeded ]]; then FULL_OK=1; else result changed-bytes fail "full backup $FULL failed"; fi

if [[ "$FULL_OK" == 1 ]]; then
  guest "dd if=/dev/urandom of=$GUEST_FILE bs=1M count=$DIRTY_MIB conv=notrunc,fsync 2>/dev/null && sync" 300 >/dev/null
  WANT="$(guest_sum)"
  if [[ "$(backup "$INCR" --parent "$FULL")" == Succeeded ]]; then
    INCR_OK=1
    changed="$(backup_field "$INCR" '(o.get("status") or {}).get("changedBytes") or 0')"
    stored="$(backup_field "$FULL" '(o.get("status") or {}).get("storedBytes") or 0')"
    lo=$(( DIRTY_MIB << 20 )); hi=$(( (DIRTY_MIB + SLACK_MIB) << 20 ))
    if (( changed >= lo && changed <= hi )); then
      result changed-bytes pass "dirtied ${DIRTY_MIB}MiB, incremental changedBytes $((changed >> 20))MiB; full stored $((stored >> 20))MiB"
    else
      result changed-bytes fail "dirtied ${DIRTY_MIB}MiB but changedBytes $((changed >> 20))MiB (want ${DIRTY_MIB}..$((DIRTY_MIB + SLACK_MIB))MiB)"
    fi
  else
    result changed-bytes fail "incremental $INCR failed"
  fi
fi

if [[ "$INCR_OK" == 1 ]]; then
  corrupt
  if restore_into "$INCR" && [[ "$(guest_sum)" == "$WANT" ]]; then
    result restore pass "corrupted $GUEST_FILE, restored $INCR (full + incremental), sha256 matched"
  else
    result restore fail "restore of $INCR did not bring back sha256 $WANT"
  fi

  if [[ "$(backup "$SYNTH" --mode synthetic-full --parent "$INCR")" == Succeeded ]]; then
    "$KAIRONCTL" backup delete "$FULL" --namespace "$NAMESPACE" >&2
    if wait_until 600 "$FULL deleted" backup_gone "$FULL"; then
      corrupt
      if restore_into "$INCR" && [[ "$(guest_sum)" == "$WANT" ]]; then
        result synthesize pass "synthetic full $SYNTH, pruned $FULL (S3 objects deleted), restored $INCR through it: sha256 matched"
      else
        result synthesize fail "restore of $INCR after pruning $FULL did not match"
      fi
    else
      result synthesize fail "$FULL was not deleted (Object Lock or a stuck finalizer?)"
    fi
  else
    result synthesize fail "synthetic full $SYNTH failed"
  fi
fi

if [[ -z "$NODE_SSH" ]]; then
  result forced-full "not run" "no --node-ssh / KAIRON_HW_SOURCE_SSH"
else
  if [[ "$(backup "$F2" "${S3[@]}")" == Succeeded && "$(backup "$I2" --parent "$F2")" == Succeeded ]]; then
    runtime="$(machine_field "$NAMESPACE" "$MACHINE" '(o.get("status") or {}).get("runtimeID") or ""')"
    lab_log "killing runtime $runtime on $NODE_SSH"
    lab_ssh "$NODE_SSH" "${KILL_CMD//\{runtime\}/$runtime}" || lab_log "kill command reported failure"
    sleep 10
    if ! wait_until 180 "$MACHINE back" machine_healthy "$NAMESPACE" "$MACHINE"; then
      "$KAIRONCTL" halt "$MACHINE" --namespace "$NAMESPACE" >&2
      wait_until 600 "$MACHINE Stopped" machine_stopped || true
      "$KAIRONCTL" start "$MACHINE" --namespace "$NAMESPACE" >&2
    fi
    wait_guest
    phase="$(backup "$I3" --parent "$I2")"
    need="$(backup_field "$I3" '((o.get("status") or {}).get("disk") or {}).get("fullRequired") or False')"
    if [[ "$phase" == Failed && "$need" == True ]]; then
      if [[ "$(backup "$F3" "${S3[@]}")" == Succeeded ]]; then
        result forced-full pass "after SIGKILL the incremental failed with fullRequired; the next full succeeded"
      else
        result forced-full fail "fullRequired reported, but the follow-up full $F3 failed"
      fi
    else
      result forced-full fail "incremental after SIGKILL: phase $phase, fullRequired $need (want Failed, True)"
    fi
  else
    result forced-full fail "setup backups $F2/$I2 failed"
  fi
fi

today="$(date -u +%Y-%m-%d)"
rc=0
echo
for c in changed-bytes restore synthesize forced-full; do
  r="${RESULT[$c]:-not run}"
  [[ "$r" == fail ]] && rc=1
  echo "| CBT: $c | $r | $today | Node $NODE, ${NOTE[$c]:-earlier step failed} |"
done
exit "$rc"
