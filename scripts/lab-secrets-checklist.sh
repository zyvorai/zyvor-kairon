#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
#
# Prints the exact `gh secret set` / `gh variable set` commands still needed
# for a green hardware-migration nightly, as a markdown checklist. Values are
# never printed or read: each command has a VALUE (or $VALUE, ca.crt)
# placeholder to fill in. The inputs come from the table in
# scripts/migration-matrix.sh (`env --gh-commands`), so nothing is
# duplicated here.
#
#   lab-secrets-checklist.sh [--profile=full|core] [--repo=OWNER/NAME] [--all] [--offline]
#
#   --profile  full (default, the nightly) or core (cold, live, live-ebpf)
#   --all      list every input of the profile, already set or not
#   --offline  do not ask GitHub what is set; list every input of the profile
#
# Without --offline it lists the repository's secret and variable *names*,
# read-only, with `gh secret list` / `gh variable list` (needs a signed-in gh
# with read access to them) and skips the ones already set. Optional inputs
# (working defaults) are never listed. See docs/runbook-migration-matrix.md.
set -euo pipefail

MATRIX="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/migration-matrix.sh"
PROFILE=full; REPO=""; ALL=0; OFFLINE=0
for arg in "$@"; do
  case "$arg" in
    --profile=*) PROFILE="${arg#*=}" ;;
    --repo=*) REPO="${arg#*=}" ;;
    --all) ALL=1 ;;
    --offline) OFFLINE=1 ;;
    -h|--help) sed -n '5,21p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown flag $arg (see --help)" >&2; exit 2 ;;
  esac
done
case "$PROFILE" in
  full) needs=" required core full " ;;
  core) needs=" required core " ;;
  *) echo "--profile must be full or core" >&2; exit 2 ;;
esac

have=""; source_note="not checked (--offline): every input of the profile is listed"
if (( ! OFFLINE && ! ALL )); then
  if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    repo_args=()
    [[ -n "$REPO" ]] && repo_args=(--repo "$REPO")
    if s="$(gh secret list "${repo_args[@]}" --json name -q '.[].name' 2>/dev/null)" &&
      v="$(gh variable list "${repo_args[@]}" --json name -q '.[].name' 2>/dev/null)"; then
      have="$(printf '%s\n%s\n' "$s" "$v")"
      source_note="repository secret/variable names via gh (read-only, repository scope; environment-scoped values not checked)"
    else
      source_note="gh secret/variable list failed (needs read access): every input of the profile is listed"
    fi
  else
    source_note="gh missing or not signed in: every input of the profile is listed"
  fi
fi
(( ALL )) && source_note="--all: every input of the profile is listed"

echo "# Green nightly ($PROFILE profile): what is still unset"
echo "# Checked: $source_note"
echo "# Replace VALUE / \$VALUE / ca.crt with the real value; secrets are piped, never on the command line."
echo
count=0
while IFS='|' read -r name kind need rows cmd; do
  [[ "$needs" == *" $need "* ]] || continue
  if [[ -n "$have" ]] && grep -qx "$name" <<<"$have"; then continue; fi
  gates="every case"
  [[ "$rows" != all ]] && gates="${rows// /, }"
  echo "- [ ] \`$name\` ($kind, $need): unblocks $gates"
  echo "      $cmd"
  count=$((count + 1))
done < <("$MATRIX" env --gh-commands)
echo
if (( count == 0 )); then
  echo "Nothing left to set for the $PROFILE profile. Next: make migration-preflight PROFILE=$PROFILE"
else
  echo "$count item(s). After setting them: make migration-dry-run PROFILE=$PROFILE, then make migration-preflight PROFILE=$PROFILE"
fi
