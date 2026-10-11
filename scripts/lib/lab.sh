# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
#
# Shared helpers for the multi-host lab scripts (hardware-migration-matrix.sh,
# recovery-drill.sh, lab-inject-*.sh). Source it; don't execute it.
#
# Reads Machines/MachineMigrations straight from the Kubernetes API with
# KAIRON_KUBE_URL / KAIRON_KUBE_TOKEN / KAIRON_KUBE_CA (a CA file path) /
# KAIRON_KUBE_INSECURE=true -- the same variables kaironctl uses -- because
# kaironctl's table output isn't meant to be parsed field by field.

LAB_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KAIRONCTL="${KAIRONCTL:-$LAB_REPO_ROOT/bin/kaironctl}"
[[ -x "$KAIRONCTL" ]] || KAIRONCTL="kaironctl"
LAB_API="apis/kairon.zyvor.dev/v1"

lab_log() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }

# kube_curl METHOD PATH [JSON_BODY [CONTENT_TYPE]] -- prints the response body.
kube_curl() {
  local method="$1" path="$2" body="${3:-}" ctype="${4:-application/json}"
  local args=(-fsS -X "$method" -H "Authorization: Bearer ${KAIRON_KUBE_TOKEN:?set KAIRON_KUBE_TOKEN}")
  if [[ "${KAIRON_KUBE_INSECURE:-}" == "true" ]]; then
    args+=(-k)
  elif [[ -n "${KAIRON_KUBE_CA:-}" ]]; then
    args+=(--cacert "$KAIRON_KUBE_CA")
  fi
  [[ -n "$body" ]] && args+=(-H "Content-Type: $ctype" --data "$body")
  curl "${args[@]}" "${KAIRON_KUBE_URL:?set KAIRON_KUBE_URL}/${path#/}"
}

# kube_exists PATH -- 0 if the object exists, 1 on 404, 2 on any other error.
kube_exists() {
  local code
  code="$(kube_curl_code GET "$1")"
  case "$code" in
    200) return 0 ;;
    404) return 1 ;;
    *) return 2 ;;
  esac
}

kube_curl_code() {
  local method="$1" path="$2"
  local args=(-sS -o /dev/null -w '%{http_code}' -X "$method" -H "Authorization: Bearer ${KAIRON_KUBE_TOKEN}")
  if [[ "${KAIRON_KUBE_INSECURE:-}" == "true" ]]; then
    args+=(-k)
  elif [[ -n "${KAIRON_KUBE_CA:-}" ]]; then
    args+=(--cacert "$KAIRON_KUBE_CA")
  fi
  curl "${args[@]}" "${KAIRON_KUBE_URL}/${path#/}" || true
}

machine_path() { echo "$LAB_API/namespaces/$1/machines/$2"; }
migration_path() { echo "$LAB_API/namespaces/$1/machinemigrations/$2"; }

# json_eval EXPR -- evaluates a Python expression with `o` bound to the JSON
# document on stdin and prints the result ("" for None).
json_eval() {
  python3 -c '
import json, sys
o = json.load(sys.stdin)
def cond(t):
    for c in (o.get("status") or {}).get("conditions") or []:
        if c.get("type") == t:
            return c.get("status")
    return ""
v = eval(sys.argv[1])
print("" if v is None else v)
' "$1"
}

# machine_field NS NAME EXPR
machine_field() { kube_curl GET "$(machine_path "$1" "$2")" | json_eval "$3"; }

machine_node() { machine_field "$1" "$2" '(o.get("status") or {}).get("nodeName") or (o.get("spec") or {}).get("nodeName") or ""'; }
machine_phase() { machine_field "$1" "$2" '(o.get("status") or {}).get("phase") or ""'; }

migration_phase() {
  kube_curl GET "$(migration_path "$1" "$2")" 2>/dev/null | json_eval '(o.get("status") or {}).get("phase") or ""' 2>/dev/null || true
}

# wait_until TIMEOUT DESCRIPTION CMD... -- polls CMD every 3s until it succeeds.
wait_until() {
  local timeout="$1" desc="$2"; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@"; do
    if (( $(date +%s) >= deadline )); then
      lab_log "timed out after ${timeout}s waiting for: $desc"
      return 1
    fi
    sleep 3
  done
}

# machine_is NS NAME EXPR -- true when EXPR evaluates to "True".
machine_is() { [[ "$(machine_field "$1" "$2" "$3" 2>/dev/null)" == "True" ]]; }

# machine_healthy NS NAME [NODE] -- Running, Ready=True, a guest IP when
# KAIRON_HW_REQUIRE_GUEST_IP=1 (the default), and on NODE when given.
machine_healthy() {
  local ns="$1" name="$2" node="${3:-}"
  local expr='(o.get("status") or {}).get("phase") == "Running" and cond("Ready") == "True"'
  if [[ "${KAIRON_HW_REQUIRE_GUEST_IP:-1}" == "1" ]]; then
    expr+=' and bool((o.get("status") or {}).get("guestIP"))'
  fi
  if [[ -n "$node" ]]; then
    expr+=" and ((o.get(\"status\") or {}).get(\"nodeName\") or \"\") == \"$node\""
  fi
  machine_is "$ns" "$name" "$expr"
}

migration_in() {
  local phase
  phase="$(migration_phase "$1" "$2")"
  shift 2
  local want
  for want in "$@"; do
    [[ "$phase" == "$want" ]] && return 0
  done
  return 1
}

# start_migration NS MACHINE TARGET STRATEGY PREFIX [MIGRATE_FLAGS...] --
# prints the migration name. Extra flags go to `kaironctl migrate`.
start_migration() {
  local ns="$1" machine="$2" target="$3" strategy="$4" prefix="$5"
  shift 5
  local name
  name="$prefix-$machine-$(date -u +%Y%m%d-%H%M%S)"
  name="${name:0:63}"
  "$KAIRONCTL" migrate "$machine" --namespace "$ns" --name="$name" --strategy="$strategy" --target-node="$target" "$@" >&2
  echo "$name"
}

# other_node CURRENT A B -- whichever of A/B isn't CURRENT.
other_node() {
  if [[ "$1" == "$2" ]]; then echo "$3"; else echo "$2"; fi
}

lab_ssh() {
  local target="$1"; shift
  ssh -o BatchMode=yes -o ConnectTimeout=15 "$target" "$@"
}

# guest_exec NS MACHINE SCRIPT [TIMEOUT] -- runs `/bin/sh -c SCRIPT` in the
# guest through kairon-ui -> kairon-node -> qemu-guest-agent and prints the
# JSON result. Needs KAIRON_UI_URL / KAIRON_UI_TOKEN and spec.guestAgent.enabled.
guest_exec() {
  local ns="$1" machine="$2" script="$3" timeout="${4:-30}" body url="${KAIRON_UI_URL:?set KAIRON_UI_URL}"
  body="$(python3 -c 'import json,sys; print(json.dumps({"path":"/bin/sh","args":["-c",sys.argv[1]],"timeoutSeconds":int(sys.argv[2])}))' "$script" "$timeout")"
  curl -fsS -X POST -H "Authorization: Bearer ${KAIRON_UI_TOKEN:?set KAIRON_UI_TOKEN}" -H "Content-Type: application/json" \
    --data "$body" "${url%/}/api/v1/machines/${ns}/${machine}/exec"
}

# Dirties guest RAM continuously so a live migration has real work to
# converge: stress-ng when the image has it, otherwise a tmpfs rewrite loop.
# Detached, so the exec returns at once; stop_guest_load (or the duration
# cap) ends it.
# shellcheck disable=SC2016 # expanded by the guest's shell, not this one
GUEST_LOAD_SCRIPT='
dur="${KAIRON_LOAD_SECONDS:-900}"; mib="${KAIRON_LOAD_MIB:-256}"
if command -v stress-ng >/dev/null 2>&1; then
  nohup stress-ng --vm 1 --vm-bytes "${mib}M" --vm-keep --timeout "${dur}s" >/dev/null 2>&1 &
else
  dir=/dev/shm; [ -d "$dir" ] || dir=/tmp
  nohup sh -c "end=\$((\$(date +%s)+$dur)); while [ \$(date +%s) -lt \$end ]; do dd if=/dev/urandom of=$dir/kairon-load bs=1M count=$mib conv=notrunc 2>/dev/null; done; rm -f $dir/kairon-load" >/dev/null 2>&1 &
fi
echo started'

# start_guest_load NS MACHINE [MIB] [SECONDS]
start_guest_load() {
  local out
  out="$(guest_exec "$1" "$2" "KAIRON_LOAD_MIB=${3:-256} KAIRON_LOAD_SECONDS=${4:-900}; export KAIRON_LOAD_MIB KAIRON_LOAD_SECONDS; ${GUEST_LOAD_SCRIPT}")" || return 1
  python3 -c 'import json,sys; r=json.loads(sys.argv[1]); sys.exit(0 if r.get("exitCode")==0 and "started" in r.get("stdout","") else 1)' "$out"
}

stop_guest_load() {
  guest_exec "$1" "$2" 'pkill -f "stress-ng --vm" 2>/dev/null; pkill -f kairon-load 2>/dev/null; rm -f /dev/shm/kairon-load /tmp/kairon-load; true' >/dev/null 2>&1 || true
}

# migration_field NS NAME EXPR
migration_field() { kube_curl GET "$(migration_path "$1" "$2")" | json_eval "$3"; }
