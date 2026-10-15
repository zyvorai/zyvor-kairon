#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
bridge=${1:?usage: sudo scripts/rehearsal-bridge.sh krh-demo}
[[ "$bridge" =~ ^krh-[a-z0-9-]{1,11}$ ]] || { echo 'invalid bridge name' >&2; exit 1; }
[[ $EUID == 0 ]] || { echo 'run as root on the rehearsal VM host' >&2; exit 1; }
command -v ip >/dev/null
command -v nft >/dev/null
# Never reuse or modify an existing bridge/table: each is reserved for one run.
[[ ! -e /sys/class/net/$bridge ]] || { echo 'bridge already exists' >&2; exit 1; }
table="krh_${bridge#krh-}"
nft add table inet "$table"
nft "add chain inet $table input { type filter hook input priority -300; policy accept; }"
nft "add chain inet $table output { type filter hook output priority -300; policy accept; }"
nft add rule inet "$table" input iifname "$bridge" drop
nft add rule inet "$table" output oifname "$bridge" drop
ip link add name "$bridge" type bridge
if [[ -e /proc/sys/net/ipv6/conf/$bridge/disable_ipv6 ]]; then
  echo 1 > "/proc/sys/net/ipv6/conf/$bridge/disable_ipv6"
fi
ip link set dev "$bridge" up
printf 'Prepared %s with no uplink, host IP or host input/output access. Do not attach physical ports or reuse it for concurrent runs.\n' "$bridge"
printf 'After all copies are deleted: ip link delete %s; nft delete table inet %s\n' "$bridge" "$table"
