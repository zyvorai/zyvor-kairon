#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

# One entry point for the multi-host migration matrix
# (scripts/hardware-migration-matrix.sh): which inputs it reads, whether the
# lab in front of you can produce a green run, and running it.
#
#   migration-matrix.sh env       [--markdown | --gh-commands]
#       every input the matrix and its drills read: GitHub kind (secret/var),
#       which profile needs it, default, and whether it is set here. Secret
#       values are never printed. --gh-commands prints NAME|kind|need|rows|
#       the `gh` command that sets it (no value), for
#       scripts/lab-secrets-checklist.sh.
#   migration-matrix.sh preflight [--profile=core|full] [--case=NAME]... [--strict]
#                                 [--dry-run] [--github-summary]
#       read-only checks against the cluster, kairon-ui and the SSH targets.
#       Exits 1 on a hard failure (the matrix cannot run). Lists every row
#       that would be recorded "not run" and why; --strict makes those fatal
#       too, so a pass means the profile can come out fully green.
#       --dry-run: inputs only, no network (no cluster, kairon-ui or SSH);
#       prints each missing input with its GitHub kind and the exact
#       `gh secret set` / `gh variable set` line, then the per-row plan.
#   migration-matrix.sh run       [--profile=core|full] [--case=NAME]... [--strict]
#                                 [--dry-run] [--skip-preflight] [--write-compat] [--timeout=S]
#       preflight, then hardware-migration-matrix.sh with the profile's cases.
#       --dry-run stops after the input check and prints the plan and the
#       exact command; nothing is created, migrated or written.
#   migration-matrix.sh audit-ci  [--offline] [--repo=OWNER/NAME]
#       every KAIRON_* secret/var referenced by .github/workflows must be in
#       the input table below, and every matrix input must be wired into
#       hardware-migration.yml. Without --offline and with a signed-in `gh`,
#       also lists (read-only: `gh secret list`, `gh variable list`) which
#       of them are not set on the repository.
#
# Global: --env-file=FILE sources FILE first (KEY=value lines; keep it 0600,
# it holds the kube token), e.g. ~/.config/kairon/lab.env.
# scripts/migration-matrix-preflight.sh is kept as a shortcut for
# `preflight --dry-run --profile=full`.
#
# Profiles:
#   core  cold, live (pre-copy under guest load), live-ebpf -- the three rows
#         ROADMAP's "Green multi-host migration matrix" item names
#   full  every case hardware-migration-matrix.sh knows, live-ebpf included
#
# See docs/runbook-migration-matrix.md.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MATRIX="$ROOT/scripts/hardware-migration-matrix.sh"

# Single source of truth for the matrix inputs. Fields:
# NAME | GitHub kind (secret, var) | need (required, core, full, optional) |
# default | meaning | rows it gates ("all", or case names).
# "required": the matrix refuses to start without it.
# "core"/"full": without it that profile records at least one row "not run".
INPUTS='KAIRON_HW_LAB|var|required||1 acknowledges a real multi-host lab (safety gate)|all
KAIRON_KUBE_URL|secret|required||Kubernetes API URL reachable from the runner|all
KAIRON_KUBE_TOKEN|secret|required||token of the kairon-lab-drills ServiceAccount|all
KAIRON_KUBE_CA|secret|required||cluster CA: PEM content in GitHub (the workflow writes it to a file), a file path locally|all
KAIRON_HW_MACHINE|var|required||existing qemu Machine the matrix migrates back and forth|all
KAIRON_HW_TARGET_NODE|var|required||the other node (must differ from the Machine'"'"'s node)|all
KAIRON_HW_NAMESPACE|var|optional|default|namespace of the lab Machines (lab: kairon-lab)|
KAIRON_UI_URL|var|core||kairon-ui base URL: guest load for live/adaptive, guest-agent exec|live live-adaptive-converge live-postcopy-switch guest-agent
KAIRON_UI_TOKEN|secret|core||admin kairon-ui bearer token for KAIRON_UI_URL|live live-adaptive-converge live-postcopy-switch guest-agent
KAIRON_HW_EBPF_MACHINE|var|core||Machine with spec.network.dataplaneMode=ebpf and dataplaneRequired=true (live-ebpf row)|live-ebpf
KAIRON_HW_IMAGE|var|full||image path on the nodes for the create/stop/start/delete smoke|smoke
KAIRON_HW_SOURCE_SSH|var|full||user@host of the source node (failure drills; passwordless SSH + sudo for iptables/systemctl/pkill)|source-failure needs-recovery postcopy-source-failure
KAIRON_HW_TARGET_SSH|var|full||user@host of the target node (same requirements)|source-failure needs-recovery postcopy-source-failure
KAIRON_HW_POSTCOPY_MACHINE|var|full||disposable Machine for the post-copy source-failure drill (recreated before each run)|postcopy-source-failure
KAIRON_HW_CONTROLLER_SSH|var|optional||user@host to restart a systemd kairon-controller instead of deleting its Pods|
KAIRON_HW_CONTROLLER_NAMESPACE|var|optional|kairon-system|namespace of the controller Pods|
KAIRON_HW_CONTROLLER_UNIT|var|optional|kairon-controller|systemd unit for KAIRON_HW_CONTROLLER_SSH|
KAIRON_HW_SOURCE_NODE|var|optional|Machine'"'"'s node at start|first node of the pair|
KAIRON_HW_NODE_UNIT|var|optional|kairon-node|kairon-node systemd unit (source-failure drill)|
KAIRON_HW_SOURCE_NODE_UNIT|var|optional|KAIRON_HW_NODE_UNIT|kairon-node unit on the source host|
KAIRON_HW_TARGET_NODE_UNIT|var|optional|KAIRON_HW_NODE_UNIT|kairon-node unit on the target host|
KAIRON_HW_SMOKE_FLAGS|var|optional||extra kaironctl create flags for the smoke Machine (e.g. --guest-agent --runcmd=true)|
KAIRON_HW_REQUIRE_GUEST_IP|var|optional|1|healthy also needs status.guestIP|
KAIRON_HW_TIMEOUT|var|optional|600|per-migration timeout, seconds|
KAIRON_HW_LOAD_MIB|var|optional|256|guest RAM the live case rewrites|
KAIRON_HW_ADAPTIVE_LOAD_MIB|var|optional|1024|guest RAM the adaptive cases rewrite|
KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS|var|optional|200|starting bandwidth cap for the adaptive cases|
KAIRON_MIGRATION_PORT|var|optional|9443|kairon-node migration port the recovery drill firewalls|'

CORE_CASES=(cold live live-ebpf)
FULL_CASES=(smoke cold live guest-agent needs-recovery source-failure controller-failover
  live-adaptive-converge live-postcopy-switch live-zstd postcopy-source-failure live-ebpf)

usage() { sed -n '5,46p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

CMD="${1:-}"
[[ -n "$CMD" ]] || { usage >&2; exit 2; }
shift
case "$CMD" in -h|--help|help) usage; exit 0 ;; esac

PROFILE=core; STRICT=0; SUMMARY=0; MARKDOWN=0; GHCMDS=0; OFFLINE=0; SKIP_PREFLIGHT=0; DRY_RUN=0
REPO=""; CASES=(); PASS=()
for arg in "$@"; do
  case "$arg" in
    --env-file=*)
      f="${arg#*=}"
      [[ -r "$f" ]] || { echo "cannot read $f" >&2; exit 2; }
      set -a; # shellcheck disable=SC1090
      source "$f"; set +a ;;
    --profile=*) PROFILE="${arg#*=}" ;;
    --case=*) CASES+=("${arg#*=}") ;;
    --strict) STRICT=1 ;;
    --github-summary) SUMMARY=1 ;;
    --markdown) MARKDOWN=1 ;;
    --gh-commands) GHCMDS=1 ;;
    --offline) OFFLINE=1 ;;
    --repo=*) REPO="${arg#*=}" ;;
    --skip-preflight) SKIP_PREFLIGHT=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --write-compat|--timeout=*|--namespace=*) PASS+=("$arg") ;;
    *) echo "unknown flag $arg (see $0 --help)" >&2; exit 2 ;;
  esac
done
case "$PROFILE" in
  core) PROFILE_CASES=("${CORE_CASES[@]}") ;;
  full) PROFILE_CASES=("${FULL_CASES[@]}") ;;
  *) echo "--profile must be core or full" >&2; exit 2 ;;
esac
((${#CASES[@]})) || CASES=("${PROFILE_CASES[@]}")

input_rows() { printf '%s\n' "$INPUTS"; }
input_names() { input_rows | cut -d'|' -f1; }
is_set() { [[ -n "${!1:-}" ]]; }

cmd_env() {
  if (( GHCMDS )); then
    while IFS='|' read -r name kind need _ _ rows; do
      echo "$name|$kind|$need|$rows|$(gh_cmd "$name" "$kind")"
    done < <(input_rows)
    return
  fi
  if (( MARKDOWN )); then
    echo "| Name | GitHub kind | Needed by | Default | Meaning |"
    echo "|---|---|---|---|---|"
    while IFS='|' read -r name kind need def meaning _; do
      echo "| \`$name\` | $kind | $need | ${def:+\`$def\`} | $meaning |"
    done < <(input_rows)
    return
  fi
  printf '%-34s %-6s %-8s %-10s %s\n' NAME KIND NEED HERE DEFAULT
  while IFS='|' read -r name kind need def _; do
    local here="unset"
    if is_set "$name"; then
      here="set"
    fi
    printf '%-34s %-6s %-8s %-10s %s\n' "$name" "$kind" "$need" "$here" "${def:--}"
  done < <(input_rows)
}

# --- audit-ci ---------------------------------------------------------------
cmd_audit_ci() {
  local rc=0 refs wired name kind
  refs="$(grep -rhoE '(secrets|vars)\.KAIRON_[A-Z0-9_]+' "$ROOT/.github/workflows" | sort -u)"
  echo "== KAIRON_* names referenced by .github/workflows"
  while read -r ref; do
    [[ -n "$ref" ]] || continue
    kind="${ref%%.*}"; name="${ref#*.}"
    case "$name" in KAIRON_LAB_*) continue ;; esac # lab-features.yml, documented in runbook-lab-setup.md
    local row
    row="$(input_rows | grep -E "^${name}\|" || true)"
    if [[ -z "$row" ]]; then
      echo "UNDOCUMENTED $ref (add it to INPUTS in scripts/migration-matrix.sh)"; rc=1
    elif [[ "$(cut -d'|' -f2 <<<"$row")" != "${kind%s}" ]]; then
      echo "KIND MISMATCH $ref (table says $(cut -d'|' -f2 <<<"$row"))"; rc=1
    else
      echo "ok   $ref"
    fi
  done <<<"$refs"
  echo "== matrix inputs wired into hardware-migration.yml"
  wired="$(grep -oE '(secrets|vars)\.KAIRON_[A-Z0-9_]+' "$ROOT/.github/workflows/hardware-migration.yml" | sort -u)"
  while IFS='|' read -r name kind _; do
    if grep -qx "${kind}s\.${name}" <<<"$wired"; then echo "ok   ${kind}s.${name}"; else echo "NOT WIRED ${kind}s.${name}"; rc=1; fi
  done < <(input_rows)

  (( OFFLINE )) && return "$rc"
  if ! command -v gh >/dev/null 2>&1 || ! gh auth status >/dev/null 2>&1; then
    echo "== repository values: skipped (gh missing or not signed in; rerun after \`gh auth login\`, or use --offline)"
    return "$rc"
  fi
  local repo_args=()
  [[ -n "$REPO" ]] && repo_args=(--repo "$REPO")
  local have_s have_v
  if ! have_s="$(gh secret list "${repo_args[@]}" --json name -q '.[].name' 2>/dev/null)"; then
    echo "== gh secret list failed (needs admin/maintain on the repo); repository secrets not checked"
    have_s="?"
  fi
  if ! have_v="$(gh variable list "${repo_args[@]}" --json name -q '.[].name' 2>/dev/null)"; then
    echo "== gh variable list failed; repository variables not checked"
    have_v="?"
  fi
  echo "== repository values (names only, read-only)"
  local missing=()
  while IFS='|' read -r name kind need _; do
    local have="$have_v"; [[ "$kind" == secret ]] && have="$have_s"
    [[ "$have" == "?" ]] && continue
    if grep -qx "$name" <<<"$have"; then
      echo "set     $kind $name"
    else
      echo "MISSING $kind $name ($need)"
      [[ "$need" != optional ]] && missing+=("$name")
    fi
  done < <(input_rows)
  if ((${#missing[@]})); then
    echo "for a green nightly (full profile) still set: ${missing[*]}"
  fi
  # Environment-scoped values are not listed by the commands above.
  echo "(environment-scoped secrets/vars, if any, are not covered: gh secret list --env NAME)"
  return "$rc"
}

# --- preflight ----------------------------------------------------------------
FAILS=0; WARNS=0; NOTRUN=(); LINES=()
say() { LINES+=("$1"); echo "$1"; }
ok() { say "ok    $*"; }
warn() { WARNS=$((WARNS + 1)); say "warn  $*"; }
bad() { FAILS=$((FAILS + 1)); say "FAIL  $*"; }
notrun() { NOTRUN+=("$1: $2"); say "skip  $1 would be recorded \"not run\": $2"; }
# Quiet variant for the dry run: one NOTRUN entry per row, reasons joined.
notrun_add() {
  local i
  for i in "${!NOTRUN[@]}"; do
    if [[ "${NOTRUN[$i]}" == "$1: "* ]]; then NOTRUN[i]="${NOTRUN[$i]}; $2"; return; fi
  done
  NOTRUN+=("$1: $2")
}
want_case() { local c; for c in "${CASES[@]}"; do [[ "$c" == "$1" ]] && return 0; done; return 1; }

input_row() { input_rows | grep -E "^$1\|" || true; }

# How to set one input: locally and as a GitHub secret/var, which never puts
# a secret value on a command line.
# The `gh` command that sets one input in the repository, without its value.
gh_cmd() {
  local name="$1" kind="$2"
  if [[ "$name" == KAIRON_KUBE_CA ]]; then echo "gh secret set KAIRON_KUBE_CA < ca.crt"
  elif [[ "$name" == KAIRON_HW_LAB ]]; then echo "gh variable set KAIRON_HW_LAB --body 1"
  elif [[ "$kind" == secret ]]; then echo "printf %s \"\$VALUE\" | gh secret set $name"
  else echo "gh variable set $name --body VALUE"; fi
}
set_hint() {
  local name="$1" kind="$2"
  local local_hint="export $name=..., or put it in --env-file"
  [[ "$name" == KAIRON_KUBE_CA ]] && local_hint="KAIRON_KUBE_CA=/path/to/ca.crt"
  [[ "$name" == KAIRON_HW_LAB ]] && local_hint="export KAIRON_HW_LAB=1"
  echo "$(gh_cmd "$name" "$kind")   (locally: $local_hint)"
}

# Print one unset input: what it is, which rows it gates, how to set it.
missing_input() {
  local name kind need def meaning rows
  IFS='|' read -r name kind need def meaning rows <<<"$(input_row "$1")"
  local gates="every case (the matrix refuses to start)"
  [[ "$rows" != all ]] && gates="${rows// /, }"
  say "MISSING $name  [GitHub $kind, $need]"
  say "        what:  $meaning"
  say "        gates: $gates"
  say "        set:   $(set_hint "$name" "$kind")"
}

# Rows of $1 (a space-separated list) that this run would execute.
wanted_rows() {
  local r out=()
  for r in $1; do want_case "$r" && out+=("$r"); done
  echo "${out[*]:-}"
}

# --- dry run: inputs only, no network -----------------------------------------
cmd_dry_run() {
  say "dry run (${PROFILE}: ${CASES[*]}): inputs only; no cluster, kairon-ui or SSH contact, nothing migrates"
  local name kind need def meaning rows hit n
  while IFS='|' read -r name kind need def meaning rows; do
    [[ -n "$rows" ]] || continue
    if [[ "$rows" == all ]]; then hit=all; else hit="$(wanted_rows "$rows")"; fi
    [[ -n "$hit" ]] || continue
    if [[ "$name" == KAIRON_HW_LAB && "${KAIRON_HW_LAB:-}" != 1 && -n "${KAIRON_HW_LAB:-}" ]]; then
      bad "KAIRON_HW_LAB is '${KAIRON_HW_LAB}', must be 1"; continue
    fi
    if [[ "$name" == KAIRON_KUBE_CA && "${KAIRON_KUBE_INSECURE:-}" == true ]]; then
      warn "KAIRON_KUBE_CA unset, KAIRON_KUBE_INSECURE=true (lab only)"; continue
    fi
    if is_set "$name"; then ok "$name ($kind)"; continue; fi
    missing_input "$name"
    if [[ "$hit" == all ]]; then
      FAILS=$((FAILS + 1))
    elif [[ "$name" == KAIRON_HW_IMAGE ]]; then
      warn "smoke runs, but only checks the Machine is healthy (no create/stop/start/delete)"
    else
      for n in $hit; do notrun_add "$n" "$name unset"; done
    fi
  done < <(input_rows)
  if [[ -n "${KAIRON_KUBE_CA:-}" && ! -f "$KAIRON_KUBE_CA" ]]; then
    bad "KAIRON_KUBE_CA is not a file (locally it is a path; in GitHub it is PEM content the workflow writes to a file)"
  fi
  say ""
  say "plan:"
  local c reason r
  for c in "${CASES[@]}"; do
    reason=""
    for r in "${NOTRUN[@]}"; do [[ "$r" == "$c: "* ]] && reason="${r#"$c: "}"; done
    if (( FAILS )); then say "  $c: blocked (required inputs missing)"
    elif [[ -n "$reason" ]]; then say "  $c: recorded \"not run\" ($reason)"
    else say "  $c: runs"; fi
  done
  say "  checks still needed against the lab: scripts/migration-matrix.sh preflight --profile=${PROFILE}"
  finish
}

ssh_ok() { ssh -o BatchMode=yes -o ConnectTimeout=10 "$1" 'sudo -n true' >/dev/null 2>&1; }

cmd_preflight() {
  local c
  for c in curl python3; do
    if command -v "$c" >/dev/null 2>&1; then ok "$c found"; else bad "$c not found"; fi
  done
  # shellcheck source-path=SCRIPTDIR source=lib/lab.sh
  source "$ROOT/scripts/lib/lab.sh"
  if [[ -x "$KAIRONCTL" ]] || command -v "$KAIRONCTL" >/dev/null 2>&1; then ok "kaironctl: $KAIRONCTL"; else bad "kaironctl not found (make build, or set KAIRONCTL)"; fi

  if [[ "${KAIRON_HW_LAB:-}" == "1" ]]; then ok "KAIRON_HW_LAB=1"; else bad "KAIRON_HW_LAB is not 1: the matrix refuses to run"; fi
  local missing=() n
  for n in KAIRON_KUBE_URL KAIRON_KUBE_TOKEN KAIRON_HW_MACHINE KAIRON_HW_TARGET_NODE; do is_set "$n" || missing+=("$n"); done
  if [[ -z "${KAIRON_KUBE_CA:-}" && "${KAIRON_KUBE_INSECURE:-}" != "true" ]]; then missing+=("KAIRON_KUBE_CA"); fi
  if ((${#missing[@]})); then
    for n in "${missing[@]}"; do missing_input "$n"; done
    bad "required inputs unset: ${missing[*]} (the matrix refuses to start)"
    finish; return
  fi
  if [[ -n "${KAIRON_KUBE_CA:-}" && ! -f "$KAIRON_KUBE_CA" ]]; then
    bad "KAIRON_KUBE_CA is not a file (locally it is a path; in GitHub it is PEM content the workflow writes to a file)"
    finish; return
  fi
  local ver
  if ver="$(kube_curl GET version 2>/dev/null | json_eval 'o.get("gitVersion")' 2>/dev/null)" && [[ -n "$ver" ]]; then
    ok "Kubernetes API $KAIRON_KUBE_URL ($ver)"
  else
    bad "Kubernetes API not reachable or token rejected: GET $KAIRON_KUBE_URL/version"
    finish; return
  fi

  local ns="${KAIRON_HW_NAMESPACE:-default}" m="$KAIRON_HW_MACHINE" t="$KAIRON_HW_TARGET_NODE" src rc=0
  kube_exists "$(machine_path "$ns" "$m")" || rc=$?
  if (( rc != 0 )); then
    bad "Machine $ns/$m not found (rc $rc; check KAIRON_HW_NAMESPACE and the token's RBAC)"
    finish; return
  fi
  src="${KAIRON_HW_SOURCE_NODE:-$(machine_node "$ns" "$m")}"
  if [[ -z "$src" ]]; then bad "Machine $ns/$m is not on a node"
  elif [[ "$src" == "$t" ]]; then bad "Machine $ns/$m is on $src, the same node as KAIRON_HW_TARGET_NODE"
  else ok "Machine $ns/$m on $src; target $t"; fi
  if machine_healthy "$ns" "$m"; then ok "Machine $ns/$m Running and Ready$([[ "${KAIRON_HW_REQUIRE_GUEST_IP:-1}" == 1 ]] && echo ' with a guest IP')"
  else bad "Machine $ns/$m is not healthy (Running, Ready, guest IP): every migration case would fail"; fi
  local backend
  backend="$(machine_field "$ns" "$m" '((o.get("spec") or {}).get("runtime") or {}).get("backend") or ""')"
  [[ "$backend" == "vz" ]] && bad "Machine $ns/$m uses the vz backend; live migration needs qemu"

  local node
  for node in "$src" "$t"; do
    [[ -n "$node" ]] || continue
    local ready
    ready="$(kube_curl GET "api/v1/nodes/$node" 2>/dev/null | json_eval 'cond("Ready")' 2>/dev/null || true)"
    if [[ "$ready" == "True" ]]; then ok "Node $node Ready"; else bad "Node $node not Ready or not found"; fi
  done

  # kairon-ui: guest load (live, adaptive), guest-agent exec.
  local ui=0
  if is_set KAIRON_UI_URL && is_set KAIRON_UI_TOKEN; then
    if curl -fsS -o /dev/null -H "Authorization: Bearer $KAIRON_UI_TOKEN" "${KAIRON_UI_URL%/}/api/v1/machines/$ns/$m" 2>/dev/null; then
      ok "kairon-ui $KAIRON_UI_URL accepts the token"; ui=1
    else
      bad "kairon-ui GET ${KAIRON_UI_URL%/}/api/v1/machines/$ns/$m failed (URL, token, or a certificate curl does not trust)"
    fi
    if [[ "$(machine_field "$ns" "$m" '((o.get("spec") or {}).get("guestAgent") or {}).get("enabled") or False')" != "True" ]]; then
      bad "Machine $ns/$m has no spec.guestAgent.enabled: the guest load cannot start, live and adaptive cases fail"; ui=0
    fi
  fi
  if (( ! ui )) && [[ -n "$(wanted_rows "live live-adaptive-converge live-postcopy-switch guest-agent")" ]]; then
    for n in KAIRON_UI_URL KAIRON_UI_TOKEN; do is_set "$n" || missing_input "$n"; done
  fi
  if (( ! ui )); then
    for c in live live-adaptive-converge live-postcopy-switch guest-agent; do
      want_case "$c" && notrun "$c" "set KAIRON_UI_URL (var) and KAIRON_UI_TOKEN (secret), Machine with spec.guestAgent.enabled"
    done
  fi

  if want_case live-ebpf; then
    local e="${KAIRON_HW_EBPF_MACHINE:-}"
    if [[ -z "$e" ]]; then
      missing_input KAIRON_HW_EBPF_MACHINE
      notrun live-ebpf "set KAIRON_HW_EBPF_MACHINE to a Machine with dataplaneMode=ebpf"
    else
      local mode req
      mode="$(machine_field "$ns" "$e" '((o.get("spec") or {}).get("network") or {}).get("dataplaneMode") or ""' 2>/dev/null || true)"
      req="$(machine_field "$ns" "$e" '((o.get("spec") or {}).get("network") or {}).get("dataplaneRequired") or False' 2>/dev/null || true)"
      if [[ "$mode" != ebpf ]]; then bad "KAIRON_HW_EBPF_MACHINE $ns/$e: dataplaneMode is '${mode}', want ebpf (or the Machine is missing)"
      else
        ok "eBPF Machine $ns/$e dataplaneMode=ebpf"
        [[ "$req" == True ]] || warn "eBPF Machine $ns/$e: set spec.network.dataplaneRequired=true so a broken attach fails the row"
        if machine_healthy "$ns" "$e"; then ok "eBPF Machine $ns/$e healthy"; else bad "eBPF Machine $ns/$e not healthy"; fi
      fi
    fi
  fi

  if want_case smoke && ! is_set KAIRON_HW_IMAGE; then
    warn "KAIRON_HW_IMAGE unset: smoke only checks $m is healthy (a pass, but not the create/stop/start/delete row)"
  fi
  local s
  for s in KAIRON_HW_SOURCE_SSH KAIRON_HW_TARGET_SSH KAIRON_HW_CONTROLLER_SSH; do
    is_set "$s" || continue
    command -v ssh >/dev/null 2>&1 || { bad "ssh not found ($s is set)"; continue; }
    if ssh_ok "${!s}"; then ok "$s ${!s}: SSH and sudo -n work"; else bad "$s ${!s}: BatchMode SSH or passwordless sudo failed"; fi
  done
  if { ! is_set KAIRON_HW_SOURCE_SSH || ! is_set KAIRON_HW_TARGET_SSH; } &&
    [[ -n "$(wanted_rows "source-failure needs-recovery postcopy-source-failure")" ]]; then
    for n in KAIRON_HW_SOURCE_SSH KAIRON_HW_TARGET_SSH; do is_set "$n" || missing_input "$n"; done
  fi
  if ! is_set KAIRON_HW_SOURCE_SSH || ! is_set KAIRON_HW_TARGET_SSH; then
    for c in source-failure needs-recovery postcopy-source-failure; do
      want_case "$c" && notrun "$c" "set KAIRON_HW_SOURCE_SSH and KAIRON_HW_TARGET_SSH"
    done
  fi
  if want_case postcopy-source-failure; then
    if ! is_set KAIRON_HW_POSTCOPY_MACHINE; then
      missing_input KAIRON_HW_POSTCOPY_MACHINE
      notrun postcopy-source-failure "set KAIRON_HW_POSTCOPY_MACHINE (a disposable Machine)"
    elif ! kube_exists "$(machine_path "$ns" "$KAIRON_HW_POSTCOPY_MACHINE")"; then
      bad "KAIRON_HW_POSTCOPY_MACHINE $ns/$KAIRON_HW_POSTCOPY_MACHINE not found (the lab must recreate it before each run)"
    fi
  fi
  finish
}

finish() {
  local verdict
  if (( FAILS )); then verdict="NOT READY: ${FAILS} failure(s)"
  elif ((${#NOTRUN[@]})); then verdict="READY, but ${#NOTRUN[@]} row(s) of profile '${PROFILE}' would be \"not run\" (not green)"
  else verdict="READY: every '${PROFILE}' case has its inputs"; fi
  echo "preflight ($PROFILE: ${CASES[*]}): $verdict"
  if (( SUMMARY )) && [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Migration matrix preflight (${PROFILE}): ${verdict}"
      echo
      echo '```'
      printf '%s\n' "${LINES[@]}"
      echo '```'
    } >> "$GITHUB_STEP_SUMMARY"
    local r
    for r in "${NOTRUN[@]}"; do echo "::warning title=Matrix row will be not run::$r"; done
  fi
  if (( FAILS )) || { (( STRICT )) && ((${#NOTRUN[@]})); }; then
    PREFLIGHT_RC=1
  else
    PREFLIGHT_RC=0
  fi
}

case "$CMD" in
  env) cmd_env ;;
  audit-ci) cmd_audit_ci ;;
  preflight)
    PREFLIGHT_RC=0
    if (( DRY_RUN )); then cmd_dry_run; else cmd_preflight; fi
    exit "$PREFLIGHT_RC" ;;
  run)
    if (( DRY_RUN )); then
      PREFLIGHT_RC=0; cmd_dry_run
      args=()
      for c in "${CASES[@]}"; do args+=("--case=$c"); done
      if (( FAILS )); then
        echo "would not start: set the required inputs above (MISSING ... required, or FAIL) first"
      else
        echo "would run: $MATRIX ${args[*]} ${PASS[*]:-}"
      fi
      exit "$PREFLIGHT_RC"
    fi
    if (( ! SKIP_PREFLIGHT )); then
      PREFLIGHT_RC=0; cmd_preflight
      (( PREFLIGHT_RC == 0 )) || { echo "preflight failed; not running the matrix (--skip-preflight to override)" >&2; exit 1; }
    fi
    args=()
    for c in "${CASES[@]}"; do args+=("--case=$c"); done
    exec "$MATRIX" "${args[@]}" "${PASS[@]}" ;;
  *) echo "unknown command $CMD (env, preflight, run, audit-ci)" >&2; exit 2 ;;
esac
