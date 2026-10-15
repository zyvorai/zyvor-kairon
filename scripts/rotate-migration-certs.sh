#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Rotates the leaf certificates of the live-migration mTLS peers and data-plane
# adapters on a set of hosts, keeping the same CA so nothing about trust changes.
# See docs/runbook-migration-cert-rotation.md.
#
# Usage:
#   rotate-migration-certs.sh --ca-dir DIR [--ssh-opt OPT]... [--dry-run] [--force] HOST_SPEC...
#
#   HOST_SPEC = USER@IP=NAME,ETC_DIR,NODE_UNIT[,ADAPTER_UNIT]
#     NAME         the host's name used in the data-plane cert (gen-migration-mtls-certs.sh)
#     ETC_DIR      /etc/kairon (or /etc/kairon-lab): certs live in ETC_DIR/migration/
#     NODE_UNIT    kairon-node (or kairon-node-lab)
#     ADAPTER_UNIT kairon-migration-adapter-fluxvm (optional; restarted before the node)
#
#   --ca-dir DIR  directory containing ca.pem and ca-key.pem (keep it offline)
#   --ssh-opt OPT repeatable, passed to ssh/scp (e.g. --ssh-opt -i --ssh-opt ~/.ssh/key)
#   --dry-run     generate and verify the new leaves, print the plan, change nothing
#   --force       skip the "no migration in flight" check
#
# Every host's old migration directory is copied to migration.bak-<timestamp>
# before anything is replaced. Services restart one host at a time, adapter
# first, then the node agent; a restart does not stop running VMs.
set -euo pipefail

CA_DIR="" DRY=0 FORCE=0
SSH_OPTS=()
HOSTS=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ca-dir) CA_DIR="${2:?--ca-dir needs a value}"; shift 2 ;;
    --ssh-opt) SSH_OPTS+=("${2:?--ssh-opt needs a value}"); shift 2 ;;
    --dry-run) DRY=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) sed -n '5,28p' "$0"; exit 0 ;;
    --*) echo "unknown flag $1" >&2; exit 2 ;;
    *) HOSTS+=("$1"); shift ;;
  esac
done
[[ -n "$CA_DIR" && ${#HOSTS[@]} -gt 0 ]] || { echo "usage: $0 --ca-dir DIR [flags] HOST_SPEC..." >&2; exit 2; }
[[ -f "$CA_DIR/ca.pem" && -f "$CA_DIR/ca-key.pem" ]] || { echo "$CA_DIR must contain ca.pem and ca-key.pem" >&2; exit 1; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- parse HOST_SPECs ------------------------------------------------------
declare -a TARGET NAME ETC NODE ADAPTER PAIRS
for i in "${!HOSTS[@]}"; do
  spec="${HOSTS[$i]}"
  left="${spec%%=*}"; right="${spec#*=}"
  [[ "$left" == *@* && "$right" != "$spec" ]] || { echo "bad HOST_SPEC '$spec'" >&2; exit 2; }
  IFS=, read -r nm et nd ad <<<"$right"
  NAME[i]="$nm"; ETC[i]="$et"; NODE[i]="$nd"; ADAPTER[i]="$ad"
  TARGET[i]="$left"
  [[ -n "${NAME[i]}" && -n "${ETC[i]}" && -n "${NODE[i]}" ]] || { echo "bad HOST_SPEC '$spec'" >&2; exit 2; }
  PAIRS+=("${NAME[i]}=${left#*@}")
done

# --- refuse while a migration is in flight ---------------------------------
if [[ $FORCE -eq 0 ]] && command -v "${KAIRONCTL:-kaironctl}" >/dev/null 2>&1; then
  if inflight="$("${KAIRONCTL:-kaironctl}" get migrations 2>/dev/null | awk 'NR>1 && $NF ~ /^(Starting|Running|Adopting|Cutover|Stopping|Restarting|NeedsRecovery)$/ {print $1" "$NF}')" && [[ -n "$inflight" ]]; then
    echo "refusing: migrations in flight (use --force to override):" >&2
    echo "$inflight" >&2
    exit 1
  fi
fi

# --- issue new leaves from the existing CA ---------------------------------
echo "[*] issuing new leaf certificates from $CA_DIR"
EXISTING_CA_DIR="$CA_DIR" "$HERE/gen-migration-mtls-certs.sh" "$WORK/certs" "${PAIRS[@]}" >/dev/null
for f in "$WORK/certs/control-plane/cert.pem" "$WORK/certs/data-plane/"*-cert.pem; do
  openssl verify -CAfile "$CA_DIR/ca.pem" "$f" >/dev/null || { echo "new certificate does not verify: $f" >&2; exit 1; }
done
echo "    new control-plane cert expires: $(openssl x509 -in "$WORK/certs/control-plane/cert.pem" -noout -enddate | cut -d= -f2)"

if [[ $DRY -eq 1 ]]; then
  echo "[dry-run] would rotate:"
  for i in "${!HOSTS[@]}"; do
    echo "  ${TARGET[$i]} (${NAME[$i]}): ${ETC[$i]}/migration; restart ${ADAPTER[$i]:+${ADAPTER[$i]} then }${NODE[$i]}"
  done
  exit 0
fi

# --- install, one host at a time -------------------------------------------
for i in "${!HOSTS[@]}"; do
  t="${TARGET[$i]}" n="${NAME[$i]}" etc="${ETC[$i]}"
  echo "[*] $t ($n)"
  scp -q "${SSH_OPTS[@]}" \
    "$WORK/certs/control-plane/ca.pem" "$WORK/certs/control-plane/cert.pem" "$WORK/certs/control-plane/key.pem" \
    "$WORK/certs/data-plane/$n-ca.pem" "$WORK/certs/data-plane/$n-cert.pem" "$WORK/certs/data-plane/$n-key.pem" \
    "$t:/tmp/"
  ssh "${SSH_OPTS[@]}" "$t" bash -s -- "$etc" "$n" "${NODE[$i]}" "${ADAPTER[$i]:-}" <<'REMOTE'
set -euo pipefail
etc="$1"; n="$2"; node="$3"; adapter="$4"
m="$etc/migration"
b="$m.bak-$(date +%Y%m%d%H%M%S)"
sudo cp -a "$m" "$b"
inst() { sudo install -m 0640 -o root -g kairon "$1" "$m/$2"; }
inst /tmp/ca.pem ca.pem
inst /tmp/cert.pem cert.pem
inst /tmp/key.pem key.pem
inst "/tmp/$n-ca.pem" adapter-ca.pem
inst "/tmp/$n-cert.pem" adapter-cert.pem
inst "/tmp/$n-key.pem" adapter-key.pem
rm -f /tmp/ca.pem /tmp/cert.pem /tmp/key.pem "/tmp/$n-ca.pem" "/tmp/$n-cert.pem" "/tmp/$n-key.pem"
[[ -n "$adapter" ]] && sudo systemctl restart "$adapter" && sleep 2
sudo systemctl restart "$node"
sleep 5
for u in $adapter $node; do
  [[ "$(systemctl is-active "$u")" == active ]] || { echo "$u is not active; previous certs are in $b" >&2; exit 1; }
done
echo "    restarted ${adapter:+$adapter, }$node; previous certs kept in $b"
REMOTE
done
echo "[+] rotated ${#HOSTS[@]} host(s). Verify with a migration or 'kaironctl selftest'."
