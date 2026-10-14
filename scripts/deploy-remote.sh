#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Adapted from ../fabric/scripts/deploy-remote.sh for kairon's architecture:
# kairon-node/kairon-controller are not self-contained daemons (they require
# a reachable Kubernetes API, see internal/kube/client.go) and kairon is Go
# with zero third-party deps. By default this script cross-compiles locally
# and ships static binaries; with --remote-build it copies a source tarball
# and compiles on the host instead (nothing is built on this machine).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_NAME="$(basename "$0")"
DEPLOY_LAST_FILE="$REPO_ROOT/.deploy-last"

if [[ -t 1 && "${NO_COLOR:-}" != "1" ]]; then
  C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'; C_RESET=$'\033[0m'
else
  C_RED=''; C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_RESET=''
fi
info() { printf '%s[*]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[+]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[!]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf '%s[x]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
die()  { err "$*"; exit 1; }
tip()  { printf '%s    tip:%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }

usage() {
  cat <<EOF
Usage:
  $SCRIPT_NAME USER@HOST [flags]
  $SCRIPT_NAME USER HOST [flags]
  $SCRIPT_NAME HOST USER [flags]        (auto-detected & swapped)
  $SCRIPT_NAME status [USER@HOST]
  $SCRIPT_NAME -h | --help

Deploys kairon-node (+ kaironctl) to a remote Linux host over SSH as a
systemd service. Unlike deploying a self-contained daemon, kairon-node
requires a reachable Kubernetes API (in-cluster, or via --kube-url) to
become ready -- see --kube-url below. A service that starts but reports
"not ready" because no cluster is configured yet is expected, not a bug.

Optionally also deploys kairon-controller (--with-controller) and/or the
kairon-ui web dashboard (--with-ui). Locally, --with-ui is the one component
that needs a Node.js/npm toolchain (to build web/dist); with --remote-build
that build happens on the host and this machine needs neither Node nor Go.

By default everything is cross-compiled on your machine with
  CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build
and shipped as a static binary; no Go toolchain and no source tree is then
required on the remote host. With --remote-build the source is copied to the
host and compiled there instead (for a machine with little disk space).

Target:
  USER@HOST, or USER HOST, or HOST USER (IPv4-vs-non-IPv4 auto swap).
  An optional trailing bare token after the target is treated as an SSH
  password (exported as SSHPASS; requires the 'sshpass' tool). At most one
  such token is accepted; anything more is an error, not silently ignored.
  Prefer the SSHPASS environment variable (SSHPASS=... $SCRIPT_NAME ...) so the
  password stays out of the process list and your shell history.
  With a working SSH key none of this is needed.

Flags:
  --with-controller       Also build/install/enable kairon-controller
                          (systemd unit kairon-controller.service).
  --with-ui               Also build/install/enable kairon-ui (systemd unit
                          kairon-ui.service), the web dashboard for
                          Machines/Migrations/Snapshots/recovery. Requires
                          'npm' locally (used once to build web/dist -- the
                          only place this script needs Node.js, and only
                          with --with-ui).
  --ui-token=TOKEN        Static bearer token for the dashboard. If omitted
                          (and --ui-allow-unauthenticated isn't set), one is
                          auto-generated locally and printed once at the end
                          of the run -- save it, it is not shown again.
  --ui-admin-password=PW  Password for the dashboard's built-in 'admin' account
                          (username/password login; default Admin@321). Seeded
                          into a fresh kairon-ui.env together with a generated
                          KAIRON_UI_SESSION_SECRET. CHANGE IT for anything
                          reachable beyond a lab network. No whitespace,
                          quotes, '$' or backslash (systemd EnvironmentFile).
  --apply-crds            Also 'kubectl apply --server-side' deploy/crd.yaml and
                          deploy/fleet-crds.yaml on the host (CRDs are never
                          installed by this script otherwise).
  --apply-rbac            Also apply deploy/rbac.yaml (generated from the Helm
                          chart: 'make rbac-bundle') so the node/controller/UI
                          service accounts carry the chart's current grants.
  --reseed-ui-auth        Also (re)apply the admin password and, if missing, a
                          session secret to an EXISTING kairon-ui.env, then
                          restart is picked up by the normal service start.
  --ui-allow-unauthenticated  Start kairon-ui without a token (local
                          development only -- every dashboard route is then
                          open to anyone who can reach the port).
  --ui-port=N             kairon-ui listen port (default 18082), same
                          auto-fallback-if-busy behavior as --node-port.
                          On a re-deploy without this flag, the port already
                          in the installed unit is kept, so the dashboard URL
                          does not change from one deploy to the next.
  --with-console          Also enable the graphical VNC console relay
                          (kairon-ui's "Console" button per Machine).
                          Requires --with-ui (the browser only ever talks
                          to kairon-node through kairon-ui's relay). A
                          shared token is auto-generated and wired into
                          both kairon-node.env and kairon-ui.env -- unlike
                          --ui-token, it's never typed by a human, so
                          there's no --console-token override. Read
                          SECURITY.md's "VNC console" section first:
                          FluxVM's own VNC socket has no auth of its own.
  --console-port=N        kairon-node's console relay port (default 8090),
                          same auto-fallback-if-busy behavior as --node-port.
  --ui-tls-cert=PATH      Use your own dashboard certificate (PEM, local path)
                          instead of the self-signed one this script otherwise
                          generates. Needs --ui-tls-key and --with-ui.
  --ui-tls-key=PATH       Private key for --ui-tls-cert.
  --ui-regenerate-cert    Generate a new self-signed dashboard certificate even
                          though one is already installed on the host (by
                          default it is reused, so browsers do not warn again
                          after every deploy).
  --ui-insecure-http      Serve the dashboard over plain HTTP. Logins and the
                          bearer token then cross the network in clear text;
                          only for a trusted loopback or a throwaway lab.
                          Without it the dashboard is HTTPS: a self-signed
                          certificate (SAN: this host's names and addresses)
                          is generated with openssl, installed to
                          /etc/kairon/ui-tls/ and wired into kairon-ui.env
                          (set even when the file exists). Browsers warn once
                          about a self-signed certificate.
  --console-tls           (the default with --with-console; kept for scripts)
                          One-way TLS on the kairon-ui -> kairon-node console
                          relay hop. With no --console-tls-cert/-key/-ca a
                          private CA + server certificate for the name
                          kairon-node is generated locally (openssl) and
                          installed; kairon-ui verifies that fixed name, so it
                          does not matter which address it dials.
  --console-insecure-http Serve the console relay as plain HTTP. The shared
                          token and the VM's console data are then clear text
                          on the network between kairon-ui and kairon-node;
                          only for hosts where both run on one machine.
  --console-tls-cert=PATH  Bring your own console relay server certificate
                          (local path). Must be given together with
                          --console-tls-key and --console-tls-ca.
  --console-tls-key=PATH   Bring your own console relay server private key.
  --console-tls-ca=PATH    CA certificate that signed --console-tls-cert;
                          installed on the kairon-ui side to verify it.
  --self-test[=PROFILE]   After the deploy, run 'kaironctl selftest' on the host
                          over an SSH terminal (coloured): it boots a tiny CirrOS
                          VM and checks power, network and more, then cleans up.
                          PROFILE is quick (default), standard or full. Needs a
                          reachable Kubernetes API (see --kube-url); the run's
                          result becomes this script's exit code.
  --remote-build          Compile on the host instead of on this machine: a
                          small source tarball (about 6 MB) is copied there and
                          built with a Go toolchain (and Node, for --with-ui)
                          cached under ~/.cache/kairon-build on the host, so
                          this machine needs no Go, no Node and no build cache.
                          The host needs curl, tar, and network access to
                          go.dev and the Go module proxy (and npm, for the
                          dashboard). Downloads are checksum-verified.
  --no-start              Install files but do not enable/start the service(s).
  --sync-only             Copy binaries + unit files to the remote host only;
                          skip user/dir/config/unit install and service start.
  --dry-run               Print the resolved plan; no SSH, no build, no writes.
  --arch=amd64|arm64      Skip the remote 'uname -m' probe.
  --uninstall             Stop/disable/remove the service(s) and binaries.
                          Keeps /etc/kairon and the 'kairon' user.
  --purge                 With --uninstall, also remove /etc/kairon and
                          the 'kairon' system user.
  --kube-url=URL          Seed KAIRON_KUBE_URL (only if env file absent).
  --kube-token=TOKEN      Seed KAIRON_KUBE_TOKEN (only if env file absent).
  --kube-ca=PATH          Seed KAIRON_KUBE_CA (a path on the REMOTE host).
  --kube-insecure         Seed KAIRON_KUBE_INSECURE=true.
  --fluxvm-url=URL        Seed FLUXVM_URL (default http://127.0.0.1:7788).
  --backend=NAME          Seed KAIRON_DEFAULT_BACKEND (default qemu).
  --image-root=PATH       Seed KAIRON_IMAGE_ROOT (default /var/lib/fluxvm/images).
  --node-name=NAME        Seed NODE_NAME (default: the remote's own hostname).
                          Must match the Node object name in your cluster.
  --interval=DURATION     Reconciliation interval baked into the unit (default 3s).
  --node-port=N           kairon-node health port (default 32302). On a
                          re-deploy without this flag, the port already in the
                          installed unit is kept. Otherwise, if the default is
                          busy on the remote host, a random free port is
                          chosen automatically and reported.
  --controller-port=N     kairon-controller health port (default 32301), same
                          auto-fallback-if-busy behavior as --node-port.
  --reserved-cpus=LIST    Set KAIRON_RESERVED_CPUS (kubelet's reserved CPUs,
                          e.g. 0-1) in kairon-node.env, even when the file
                          already exists. Other keys are left untouched.
  --image-cache-dir=PATH  Set KAIRON_IMAGE_CACHE_DIR, the directory kairon-node
                          downloads spec.image.source images into (needed by
                          --self-test and any Machine that downloads its image).
                          Created if absent and made writable for the service;
                          set even when kairon-node.env already exists. A path
                          under /var/lib/kairon-node/ needs nothing else.
  --cpu-pinning           Turn on pinnable-CPU discovery: install
                          kairon-cpustate.path/.service, which copy kubelet's
                          root-only cpu_manager_state to a file kairon-node
                          can read, and point KAIRON_CPU_MANAGER_STATE at it.
                          Needs reserved CPUs (--reserved-cpus, or already
                          in kairon-node.env).
  --no-cpu-pinning        Turn discovery off again: remove those units, the
                          copy, and both env keys.
  --migration-ca=PATH     Local CA PEM file for the live-migration mTLS peer
                          control plane. All three of --migration-ca/-cert/-key
                          must be given together (fail-closed, matches
                          kairon-node's own validation) -- omit all three to
                          leave live migration disabled (cold migration still
                          works). Copied to /etc/kairon/migration/ on the host.
  --migration-cert=PATH   Local node certificate PEM (needs ExtKeyUsage
                          serverAuth + clientAuth -- this node acts as both
                          migration server and client).
  --migration-key=PATH    Local node private key PEM matching --migration-cert.
  --migration-server-name=NAME  Expected TLS ServerName from migration peers
                          (default kairon-node).
  --migration-port=N      Migration mTLS peer port (default 9443). Every node
                          must use the same port; the deploy fails if it is
                          busy. A hostPort or NodePort rule can claim the
                          port without a listener showing up here.
  --migration-adapter-socket=PATH  Unix socket path for the local migration
                          adapter (default /run/kairon/migration-adapter.sock).
                          Only meaningful once migration mTLS is configured.
  --with-migration-adapter-stub  Build and install kairon-migration-adapter-stub,
                          a test double that simulates transfers without moving
                          real VM memory -- see docs/migration-adapter.md. Use
                          only for testing the migration control plane; a real
                          deployment needs a real hypervisor-level adapter.
  --version=STRING        Version stamped into the binary (default: git describe,
                          or 'dev' if this checkout isn't a git repository).
  --ssh-port=N            SSH port (default 22, env SSH_PORT).
  -h, --help              Show this help.

Not present here (and why): fabric's --quick mode -- kairon has no
separate toolchain step to skip. fabric's --bind / --open-firewall --
kairon-node's health port is a private liveness/readiness probe, not a
public API to expose.

Examples:
  $SCRIPT_NAME sus@80.79.5.173 --dry-run
  $SCRIPT_NAME 80.79.5.173 sus
  $SCRIPT_NAME sus@80.79.5.173 --kube-url=https://10.0.0.5:6443 --kube-insecure
  $SCRIPT_NAME sus@80.79.5.173 --with-controller --with-ui
  $SCRIPT_NAME sus@80.79.5.173 --with-ui --with-console
  $SCRIPT_NAME status sus@80.79.5.173
  $SCRIPT_NAME --uninstall sus@80.79.5.173
EOF
}

MODE="deploy"
WITH_CONTROLLER=0
WITH_UI=0
UI_TOKEN=""
UI_ALLOW_UNAUTHENTICATED=0
UI_ADMIN_PASSWORD="Admin@321"
RESEED_UI_AUTH=0
APPLY_CRDS=0
APPLY_RBAC=0
RESOLVED_UI_SESSION_SECRET=""
UI_PORT="18082"
UI_PORT_EXPLICIT=0
RESOLVED_UI_TOKEN=""
RESOLVED_UI_TOKEN_SOURCE="none"
WITH_CONSOLE=0
CONSOLE_PORT="8090"
CONSOLE_PORT_EXPLICIT=0
RESOLVED_CONSOLE_TOKEN=""
WITH_CONSOLE_TLS=0
CONSOLE_INSECURE_HTTP=0
CONSOLE_TLS_SERVER_NAME=""
UI_TLS=0
UI_INSECURE_HTTP=0
UI_TLS_REUSE=0
UI_TLS_REGEN=0
UI_TLS_CERT=""
UI_TLS_KEY=""
CONSOLE_TLS_CERT=""
CONSOLE_TLS_KEY=""
CONSOLE_TLS_CA=""
RESOLVED_CONSOLE_TLS_SOURCE="none"
NO_START=0
SYNC_ONLY=0
REMOTE_BUILD=0
SELF_TEST=0
SELF_TEST_PROFILE="quick"
DRY_RUN=0
ARCH_OVERRIDE=""
UNINSTALL=0
PURGE=0
KUBE_URL=""
KUBE_TOKEN=""
KUBE_CA=""
KUBE_INSECURE=0
FLUXVM_URL_OVERRIDE=""
BACKEND_OVERRIDE=""
IMAGE_ROOT_OVERRIDE=""
NODE_NAME_OVERRIDE=""
INTERVAL="3s"
NODE_PORT="32302"
NODE_PORT_EXPLICIT=0
CONTROLLER_PORT="32301"
CONTROLLER_PORT_EXPLICIT=0
MIGRATION_CA=""
MIGRATION_CERT=""
MIGRATION_KEY=""
MIGRATION_SERVER_NAME="kairon-node"
MIGRATION_PORT="9443"
MIGRATION_PORT_EXPLICIT=0
MIGRATION_ADAPTER_SOCKET="/run/kairon/migration-adapter.sock"
WITH_MIGRATION_ADAPTER_STUB=0
RESERVED_CPUS=""
IMAGE_CACHE_DIR=""
CPU_PINNING=""
VERSION_OVERRIDE="${KAIRON_VERSION:-}"
SSH_PORT="${SSH_PORT:-22}"
USER_ARG=""
HOST_ARG=""

if [[ $# -eq 0 ]]; then
  usage
  die "missing target"
fi
if [[ "$1" == "-h" || "$1" == "--help" ]]; then
  usage
  exit 0
fi
if [[ "$1" == "status" ]]; then
  MODE="status"
  shift
fi

# Single order-independent pass: flags may appear before, after, or around
# the target (e.g. both "--uninstall sus@host" and "sus@host --uninstall"
# must work), so bucket non-flag tokens as positionals and handle them once
# flag parsing is done, rather than requiring the target to come first.
POSITIONAL=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --with-controller) WITH_CONTROLLER=1 ;;
    --with-ui) WITH_UI=1 ;;
    --ui-token=*) UI_TOKEN="${1#*=}" ;;
    --ui-allow-unauthenticated) UI_ALLOW_UNAUTHENTICATED=1 ;;
    --ui-admin-password=*)
      UI_ADMIN_PASSWORD="${1#*=}"
      case "$UI_ADMIN_PASSWORD" in
        *[[:space:]]*|*\"*|*\'*|*\$*|*\\*|*\#*) die "--ui-admin-password must not contain whitespace, quotes, '\$', '#' or backslash" ;;
      esac
      [[ ${#UI_ADMIN_PASSWORD} -ge 8 ]] || die "--ui-admin-password must be at least 8 characters"
      ;;
    --reseed-ui-auth) RESEED_UI_AUTH=1 ;;
    --apply-crds) APPLY_CRDS=1 ;;
    --apply-rbac) APPLY_RBAC=1 ;;
    --ui-port=*) UI_PORT="${1#*=}"; UI_PORT_EXPLICIT=1 ;;
    --with-console) WITH_CONSOLE=1 ;;
    --console-port=*) CONSOLE_PORT="${1#*=}"; CONSOLE_PORT_EXPLICIT=1 ;;
    --console-tls) WITH_CONSOLE_TLS=1 ;;   # TLS is already the default with --with-console
    --console-insecure-http) CONSOLE_INSECURE_HTTP=1 ;;
    --ui-tls) UI_TLS=1 ;;   # HTTPS is already the default with --with-ui; kept for scripts
    --ui-insecure-http) UI_INSECURE_HTTP=1 ;;
    --ui-regenerate-cert) UI_TLS_REGEN=1 ;;
    --ui-tls-cert=*) UI_TLS_CERT="${1#*=}"; UI_TLS=1 ;;
    --ui-tls-key=*) UI_TLS_KEY="${1#*=}"; UI_TLS=1 ;;
    --console-tls-cert=*) CONSOLE_TLS_CERT="${1#*=}" ;;
    --console-tls-key=*) CONSOLE_TLS_KEY="${1#*=}" ;;
    --console-tls-ca=*) CONSOLE_TLS_CA="${1#*=}" ;;
    --no-start) NO_START=1 ;;
    --sync-only) SYNC_ONLY=1 ;;
    --remote-build) REMOTE_BUILD=1 ;;
    --self-test) SELF_TEST=1 ;;
    --self-test=*)
      SELF_TEST=1
      SELF_TEST_PROFILE="${1#*=}"
      case "$SELF_TEST_PROFILE" in quick|standard|full) ;; *) die "--self-test=PROFILE must be quick, standard or full (got: $SELF_TEST_PROFILE)" ;; esac
      ;;
    --dry-run) DRY_RUN=1 ;;
    --arch=*) ARCH_OVERRIDE="${1#*=}" ;;
    --uninstall) UNINSTALL=1 ;;
    --purge) PURGE=1 ;;
    --kube-url=*) KUBE_URL="${1#*=}" ;;
    --kube-token=*) KUBE_TOKEN="${1#*=}" ;;
    --kube-ca=*) KUBE_CA="${1#*=}" ;;
    --kube-insecure) KUBE_INSECURE=1 ;;
    --fluxvm-url=*) FLUXVM_URL_OVERRIDE="${1#*=}" ;;
    --backend=*) BACKEND_OVERRIDE="${1#*=}" ;;
    --image-root=*) IMAGE_ROOT_OVERRIDE="${1#*=}" ;;
    --node-name=*) NODE_NAME_OVERRIDE="${1#*=}" ;;
    --interval=*) INTERVAL="${1#*=}" ;;
    --node-port=*) NODE_PORT="${1#*=}"; NODE_PORT_EXPLICIT=1 ;;
    --controller-port=*) CONTROLLER_PORT="${1#*=}"; CONTROLLER_PORT_EXPLICIT=1 ;;
    --migration-ca=*) MIGRATION_CA="${1#*=}" ;;
    --migration-cert=*) MIGRATION_CERT="${1#*=}" ;;
    --migration-key=*) MIGRATION_KEY="${1#*=}" ;;
    --migration-server-name=*) MIGRATION_SERVER_NAME="${1#*=}" ;;
    --migration-port=*) MIGRATION_PORT="${1#*=}"; MIGRATION_PORT_EXPLICIT=1 ;;
    --migration-adapter-socket=*) MIGRATION_ADAPTER_SOCKET="${1#*=}" ;;
    --with-migration-adapter-stub) WITH_MIGRATION_ADAPTER_STUB=1 ;;
    --version=*) VERSION_OVERRIDE="${1#*=}" ;;
    --ssh-port=*) SSH_PORT="${1#*=}" ;;
    --reserved-cpus=*) RESERVED_CPUS="${1#*=}" ;;
    --image-cache-dir=*) IMAGE_CACHE_DIR="${1#*=}" ;;
    --cpu-pinning) CPU_PINNING=1 ;;
    --no-cpu-pinning) CPU_PINNING=0 ;;
    -h|--help) usage; exit 0 ;;
    --) shift; while [[ $# -gt 0 ]]; do POSITIONAL+=("$1"); shift; done ;;
    -*) die "unknown flag: $1 (see --help)" ;;
    *) POSITIONAL+=("$1") ;;
  esac
  shift
done

if [[ -n "$IMAGE_CACHE_DIR" && ! "$IMAGE_CACHE_DIR" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
  die "--image-cache-dir=$IMAGE_CACHE_DIR: want an absolute path of letters, digits, . _ - and /"
fi
if [[ -n "$RESERVED_CPUS" && ! "$RESERVED_CPUS" =~ ^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$ ]]; then
  die "--reserved-cpus=$RESERVED_CPUS: want a CPU list like 0-1 or 0,6"
fi
if [[ "$CPU_PINNING" == "0" && -n "$RESERVED_CPUS" ]]; then
  die "--no-cpu-pinning and --reserved-cpus contradict each other"
fi

MIGRATION_CONFIGURED=0
migration_paths_given=0
for p in "$MIGRATION_CA" "$MIGRATION_CERT" "$MIGRATION_KEY"; do
  [[ -n "$p" ]] && migration_paths_given=$((migration_paths_given + 1))
done
if [[ "$migration_paths_given" -gt 0 && "$migration_paths_given" -lt 3 ]]; then
  die "--migration-ca, --migration-cert and --migration-key must be given together (fail-closed, matches kairon-node's own validation)"
fi
if [[ "$migration_paths_given" -eq 3 ]]; then
  MIGRATION_CONFIGURED=1
  for p in "$MIGRATION_CA" "$MIGRATION_CERT" "$MIGRATION_KEY"; do
    [[ -f "$p" ]] || die "migration cert file not found: $p"
  done
fi
if [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" && "$MIGRATION_CONFIGURED" != "1" ]]; then
  die "--with-migration-adapter-stub requires --migration-ca/-cert/-key (the stub is only useful once migration mTLS is configured)"
fi
if [[ "$WITH_CONSOLE" == "1" && "$WITH_UI" != "1" ]]; then
  die "--with-console requires --with-ui (the browser only ever talks to kairon-node's console relay through kairon-ui)"
fi

console_tls_paths_given=0
for p in "$CONSOLE_TLS_CERT" "$CONSOLE_TLS_KEY" "$CONSOLE_TLS_CA"; do
  [[ -n "$p" ]] && console_tls_paths_given=$((console_tls_paths_given + 1))
done
if [[ "$console_tls_paths_given" -gt 0 && "$console_tls_paths_given" -lt 3 ]]; then
  die "--console-tls-cert, --console-tls-key and --console-tls-ca must be given together (fail-closed, matches --migration-ca/-cert/-key's own validation)"
fi
if [[ "$console_tls_paths_given" -eq 3 ]]; then
  [[ "$WITH_CONSOLE" == "1" ]] || die "--console-tls-cert/-key/-ca require --with-console"
  [[ "$CONSOLE_INSECURE_HTTP" != "1" ]] || die "--console-tls-cert/-key/-ca contradict --console-insecure-http"
  for p in "$CONSOLE_TLS_CERT" "$CONSOLE_TLS_KEY" "$CONSOLE_TLS_CA"; do
    [[ -f "$p" ]] || die "console TLS file not found: $p"
  done
fi
if [[ "$WITH_CONSOLE_TLS" == "1" && "$WITH_CONSOLE" != "1" ]]; then
  die "--console-tls requires --with-console"
fi
if [[ "$CONSOLE_INSECURE_HTTP" == "1" ]]; then
  [[ "$WITH_CONSOLE" == "1" ]] || die "--console-insecure-http requires --with-console"
  [[ "$WITH_CONSOLE_TLS" != "1" ]] || die "--console-tls contradicts --console-insecure-http"
fi
# The console relay carries the shared token and the VM's console, exec and
# file data, so its TLS is on by default; plain HTTP is asked for by name.
if [[ "$WITH_CONSOLE" == "1" && "$CONSOLE_INSECURE_HTTP" != "1" ]]; then
  WITH_CONSOLE_TLS=1
fi
if [[ "$UI_INSECURE_HTTP" == "1" ]]; then
  [[ "$WITH_UI" == "1" ]] || die "--ui-insecure-http requires --with-ui"
  [[ -z "$UI_TLS_CERT$UI_TLS_KEY" ]] || die "--ui-insecure-http contradicts --ui-tls-cert/--ui-tls-key; pick plain HTTP or a certificate"
fi
if [[ "$UI_TLS" == "1" || -n "$UI_TLS_CERT$UI_TLS_KEY" ]]; then
  [[ "$WITH_UI" == "1" ]] || die "--ui-tls / --ui-tls-cert / --ui-tls-key require --with-ui"
  [[ "$UI_INSECURE_HTTP" != "1" ]] || die "--ui-tls contradicts --ui-insecure-http"
  if [[ -n "$UI_TLS_CERT" || -n "$UI_TLS_KEY" ]]; then
    [[ -n "$UI_TLS_CERT" && -n "$UI_TLS_KEY" ]] || die "--ui-tls-cert and --ui-tls-key must be given together"
    [[ -f "$UI_TLS_CERT" ]] || die "dashboard TLS certificate not found: $UI_TLS_CERT"
    [[ -f "$UI_TLS_KEY" ]] || die "dashboard TLS key not found: $UI_TLS_KEY"
  fi
fi
# The dashboard is HTTPS by default: with --with-ui a certificate is always
# installed unless plain HTTP was asked for by name.
if [[ "$WITH_UI" == "1" && "$UI_INSECURE_HTTP" != "1" ]]; then
  UI_TLS=1
fi

# USER@HOST [PASSWORD] is at most 2 bare tokens; USER HOST [PASSWORD] and
# HOST USER [PASSWORD] at most 3. Extra tokens used to be dropped silently,
# which hides typos like a stray word after the target.
if [[ ${#POSITIONAL[@]} -ge 1 ]]; then
  _max_positional=3
  [[ "${POSITIONAL[0]}" == *@* ]] && _max_positional=2
  if [[ ${#POSITIONAL[@]} -gt $_max_positional ]]; then
    die "unexpected extra argument(s): ${POSITIONAL[*]:$_max_positional} (a target is USER@HOST or USER HOST, plus at most one SSH password; flags start with --)"
  fi
fi

if [[ ${#POSITIONAL[@]} -ge 1 ]]; then
  if [[ "${POSITIONAL[0]}" == *@* ]]; then
    USER_ARG="${POSITIONAL[0]%%@*}"
    HOST_ARG="${POSITIONAL[0]#*@}"
    if [[ ${#POSITIONAL[@]} -ge 2 ]]; then
      command -v sshpass >/dev/null 2>&1 || die "a password argument was given but 'sshpass' is not installed"
      export SSHPASS="${POSITIONAL[1]}"
    fi
  elif [[ ${#POSITIONAL[@]} -ge 2 ]]; then
    if [[ "${POSITIONAL[0]}" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && ! "${POSITIONAL[1]}" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
      HOST_ARG="${POSITIONAL[0]}"
      USER_ARG="${POSITIONAL[1]}"
      tip "detected HOST then USER order -- using ${USER_ARG}@${HOST_ARG} (equivalent: $SCRIPT_NAME ${USER_ARG} ${HOST_ARG})"
    else
      USER_ARG="${POSITIONAL[0]}"
      HOST_ARG="${POSITIONAL[1]}"
    fi
    if [[ ${#POSITIONAL[@]} -ge 3 ]]; then
      command -v sshpass >/dev/null 2>&1 || die "a password argument was given but 'sshpass' is not installed"
      export SSHPASS="${POSITIONAL[2]}"
    fi
  else
    die "a single target argument must be in USER@HOST form (got: ${POSITIONAL[0]})"
  fi
fi

if [[ -z "$HOST_ARG" || -z "$USER_ARG" ]]; then
  if [[ -n "${DEPLOY_HOST:-}" && -n "${DEPLOY_USER:-}" ]]; then
    HOST_ARG="$DEPLOY_HOST"
    USER_ARG="$DEPLOY_USER"
  elif [[ -f "$DEPLOY_LAST_FILE" ]]; then
    HOST_ARG="$(grep -E '^HOST=' "$DEPLOY_LAST_FILE" | cut -d= -f2-)"
    USER_ARG="$(grep -E '^DEPLOY_USER=' "$DEPLOY_LAST_FILE" | cut -d= -f2-)"
    [[ -n "$HOST_ARG" && -n "$USER_ARG" ]] && tip "no target given -- reusing last deploy target ${USER_ARG}@${HOST_ARG} from .deploy-last"
  fi
fi
[[ -n "$HOST_ARG" && -n "$USER_ARG" ]] || { usage; die "no target host/user given (and no .deploy-last found)"; }

REMOTE="${USER_ARG}@${HOST_ARG}"
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=30 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -p "$SSH_PORT")
SCP_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=30 -P "$SSH_PORT")
NEEDS_TTY=0
SUDO=""

ssh_cmd() {
  if [[ -n "${SSHPASS:-}" ]] && command -v sshpass >/dev/null 2>&1; then
    sshpass -e ssh "${SSH_OPTS[@]}" "$@"
  else
    ssh "${SSH_OPTS[@]}" "$@"
  fi
}
ssh_exec_privileged() {
  local extra=()
  [[ "$NEEDS_TTY" == "1" ]] && extra=(-tt)
  if [[ -n "${SSHPASS:-}" ]] && command -v sshpass >/dev/null 2>&1; then
    sshpass -e ssh ${extra[@]+"${extra[@]}"} "${SSH_OPTS[@]}" "$@"
  else
    ssh ${extra[@]+"${extra[@]}"} "${SSH_OPTS[@]}" "$@"
  fi
}
scp_cmd() {
  if [[ -n "${SSHPASS:-}" ]] && command -v sshpass >/dev/null 2>&1; then
    sshpass -e scp "${SCP_OPTS[@]}" "$@"
  else
    scp "${SCP_OPTS[@]}" "$@"
  fi
}

detect_sudo() {
  if [[ "$USER_ARG" == "root" ]]; then
    SUDO=""
    NEEDS_TTY=0
    return
  fi
  SUDO="sudo"
  if ssh_cmd "$REMOTE" 'sudo -n true' >/dev/null 2>&1; then
    NEEDS_TTY=0
    ok "passwordless sudo available for $USER_ARG"
  else
    NEEDS_TTY=1
    warn "passwordless sudo not detected for $USER_ARG -- allocating a TTY so sudo can prompt for a password"
  fi
}

detect_arch() {
  if [[ -n "$ARCH_OVERRIDE" ]]; then
    RESOLVED_GOARCH="$ARCH_OVERRIDE"
    return
  fi
  local uname_m
  uname_m="$(ssh_cmd "$REMOTE" uname -m)"
  case "$uname_m" in
    x86_64) RESOLVED_GOARCH="amd64" ;;
    aarch64|arm64) RESOLVED_GOARCH="arm64" ;;
    *) die "unrecognized remote architecture '$uname_m' -- pass --arch=amd64|arm64 explicitly" ;;
  esac
}

resolve_version() {
  if [[ -n "$VERSION_OVERRIDE" ]]; then
    RESOLVED_VERSION="$VERSION_OVERRIDE"
  elif git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    RESOLVED_VERSION="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
  else
    RESOLVED_VERSION="dev"
  fi
  if git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    RESOLVED_COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  else
    RESOLVED_COMMIT="unknown"
  fi
}

print_plan() {
  cat <<EOF

Dry run -- no SSH connections, no build, no changes.

Target:                 ${USER_ARG}@${HOST_ARG} (port $SSH_PORT)
Components:              kairon-node, kaironctl$([[ "$WITH_CONTROLLER" == "1" ]] && echo ", kairon-controller")$([[ "$WITH_UI" == "1" ]] && echo ", kairon-ui")
Arch:                    ${RESOLVED_GOARCH}
Version:                 ${RESOLVED_VERSION}
Node name:                ${RESOLVED_NODE_NAME}
FLUXVM_URL:               ${RESOLVED_FLUXVM_URL}
KAIRON_DEFAULT_BACKEND:   ${RESOLVED_BACKEND}
KAIRON_IMAGE_ROOT:        ${RESOLVED_IMAGE_ROOT}
KAIRON_KUBE_URL:          ${KUBE_URL:-<not set -- service will start but stay not-ready until configured>}
Node health port:         ${NODE_PORT}$([[ "$NODE_PORT_EXPLICIT" != "1" ]] && echo " (default; auto-replaced with a random free port if busy)")
$([[ "$WITH_CONTROLLER" == "1" ]] && echo "Controller health port:  ${CONTROLLER_PORT}$([[ "$CONTROLLER_PORT_EXPLICIT" != "1" ]] && echo " (default; auto-replaced with a random free port if busy)")")
$([[ "$WITH_UI" == "1" ]] && echo "UI port:                 ${UI_PORT}$([[ "$UI_PORT_EXPLICIT" != "1" ]] && echo " (default; auto-replaced with a random free port if busy)")")
$([[ "$WITH_UI" == "1" ]] && echo "Dashboard token:          $(if [[ -n "$UI_TOKEN" ]]; then echo "provided via --ui-token"; elif [[ "$UI_ALLOW_UNAUTHENTICATED" == "1" ]]; then echo "none (--ui-allow-unauthenticated -- open dashboard, local/dev only)"; else echo "will be auto-generated at deploy time (openssl rand -hex 24) and printed once at the end"; fi)")
$([[ "$WITH_CONSOLE" == "1" ]] && echo "Console relay port:       ${CONSOLE_PORT}$([[ "$CONSOLE_PORT_EXPLICIT" != "1" ]] && echo " (default; auto-replaced with a random free port if busy)")")
$([[ "$WITH_CONSOLE" == "1" ]] && echo "Console relay token:      will be auto-generated at deploy time (openssl rand -hex 32), shared between kairon-node and kairon-ui only -- never shown to an operator")
$([[ "$WITH_CONSOLE_TLS" == "1" ]] && echo "Console relay TLS:        $(if [[ -n "$CONSOLE_TLS_CERT" ]]; then echo "provided via --console-tls-cert/-key/-ca"; else echo "self-signed CA + cert will be auto-generated at deploy time (openssl)"; fi)")
$([[ "$CONSOLE_INSECURE_HTTP" == "1" ]] && echo "Console relay TLS:        none (--console-insecure-http: token and console data in clear text)")
Start after install:      $([[ "$NO_START" == "1" ]] && echo "no (--no-start)" || echo "yes")
Image cache dir:          ${IMAGE_CACHE_DIR:-<unchanged>}
$([[ "$WITH_UI" == "1" ]] && echo "Dashboard scheme:        $(if [[ "$UI_INSECURE_HTTP" == "1" ]]; then echo "http (--ui-insecure-http: clear-text logins)"; elif [[ -n "$UI_TLS_CERT" ]]; then echo "https, certificate provided via --ui-tls-cert/-key"; else echo "https, self-signed certificate generated at deploy time (openssl)"; fi)")
Build location:           $([[ "$REMOTE_BUILD" == "1" ]] && echo "on the host (--remote-build)" || echo "this machine (cross-compile)")
Self-test after deploy:   $([[ "$SELF_TEST" == "1" ]] && echo "yes (profile ${SELF_TEST_PROFILE})" || echo "no (add --self-test)")

Remote paths:
  /usr/bin/kairon-node, /usr/bin/kaironctl$([[ "$WITH_CONTROLLER" == "1" ]] && echo ", /usr/bin/kairon-controller")$([[ "$WITH_UI" == "1" ]] && echo ", /usr/bin/kairon-ui")
  /etc/kairon/kairon-node.env   (seeded only if absent)
  /etc/systemd/system/kairon-node.service$([[ "$WITH_CONTROLLER" == "1" ]] && echo ", kairon-controller.service")$([[ "$WITH_UI" == "1" ]] && echo ", kairon-ui.service")
$([[ "$WITH_UI" == "1" ]] && echo "  /etc/kairon/ui-web/ (web assets, replaced on every --with-ui deploy)")
$([[ "$WITH_UI" == "1" ]] && echo "Dashboard login:          admin / $(if [[ "$UI_ADMIN_PASSWORD" == "Admin@321" ]]; then echo "Admin@321 (default -- change it)"; else echo "(from --ui-admin-password)"; fi)  [token login stays enabled]")
$([[ "$WITH_UI" == "1" ]] && echo "  /etc/kairon/kairon-ui.env   (seeded only if absent$([[ "$RESEED_UI_AUTH" == "1" ]] && echo "; admin password + session secret re-applied"))")
EOF
}

run_status() {
  info "checking kairon-node on ${USER_ARG}@${HOST_ARG}..."
  ssh_cmd "$REMOTE" '
    port_of() { grep -oE -- "--health-addr=:[0-9]+" "$1" 2>/dev/null | cut -d: -f2; }
    if systemctl list-unit-files kairon-node.service >/dev/null 2>&1; then
      systemctl status kairon-node.service --no-pager -l || true
      p=$(port_of /etc/systemd/system/kairon-node.service)
      echo "---"
      curl -s -o /dev/null -w "healthz=%{http_code}\n" "http://127.0.0.1:${p:-32302}/healthz" 2>/dev/null
      curl -s -o /dev/null -w "readyz=%{http_code}\n" "http://127.0.0.1:${p:-32302}/readyz" 2>/dev/null
    else
      echo "kairon-node.service is not installed on this host"
    fi
    if systemctl list-unit-files kairon-controller.service >/dev/null 2>&1; then
      echo "---"
      systemctl status kairon-controller.service --no-pager -l || true
    fi
    if systemctl list-unit-files kairon-ui.service >/dev/null 2>&1; then
      echo "---"
      systemctl status kairon-ui.service --no-pager -l || true
      cp=$(grep -oE -- "--console-addr=:[0-9]+" /etc/systemd/system/kairon-node.service 2>/dev/null | cut -d: -f2)
      if [[ -n "$cp" ]]; then
        echo "---"
        curl -s -o /dev/null -w "console relay port $cp: %{http_code} (401 is expected without a bearer token -- it means the listener is up)\n" "http://127.0.0.1:${cp}/console/nonexistent" 2>/dev/null
      fi
    fi
  ' || true
}

run_uninstall() {
  if [[ "$DRY_RUN" == "1" ]]; then
    cat <<EOF

Dry run -- no SSH connections, no changes.

Would uninstall from ${USER_ARG}@${HOST_ARG}:
  stop+disable kairon-node.service, kairon-controller.service, kairon-ui.service
  remove /etc/systemd/system/{kairon-node,kairon-controller,kairon-ui}.service
  remove /usr/bin/{kairon-node,kairon-controller,kaironctl,kairon-ui}
  $([[ "$PURGE" == "1" ]] && echo "remove /etc/kairon (incl. ui-web) and the 'kairon' user (--purge)" || echo "keep /etc/kairon (incl. ui-web) and the 'kairon' user (pass --purge to remove them)")
EOF
    return 0
  fi
  info "connecting to ${USER_ARG}@${HOST_ARG}..."
  ssh_cmd "$REMOTE" true || die "could not SSH to ${REMOTE}"
  detect_sudo
  info "uninstalling kairon-node / kairon-controller / kairon-ui / kaironctl from ${USER_ARG}@${HOST_ARG}..."
  local remote_script
  remote_script="$(cat <<'UNINSTALL_EOF'
set -euo pipefail
PURGE="$1"
systemctl stop kairon-node.service 2>/dev/null || true
systemctl stop kairon-controller.service 2>/dev/null || true
systemctl stop kairon-ui.service 2>/dev/null || true
systemctl stop kairon-migration-adapter-stub.service 2>/dev/null || true
systemctl disable --now kairon-cpustate.path 2>/dev/null || true
systemctl disable kairon-node.service 2>/dev/null || true
systemctl disable kairon-controller.service 2>/dev/null || true
systemctl disable kairon-ui.service 2>/dev/null || true
systemctl disable kairon-migration-adapter-stub.service 2>/dev/null || true
rm -f /etc/systemd/system/kairon-node.service /etc/systemd/system/kairon-controller.service /etc/systemd/system/kairon-ui.service /etc/systemd/system/kairon-migration-adapter-stub.service /etc/systemd/system/kairon-cpustate.path /etc/systemd/system/kairon-cpustate.service
systemctl daemon-reload
rm -f /usr/bin/kairon-node /usr/bin/kairon-controller /usr/bin/kairon-ui /usr/bin/kaironctl /usr/bin/kairon-migration-adapter-stub
if [[ "$PURGE" == "1" ]]; then
  rm -rf /etc/kairon
  userdel kairon 2>/dev/null || true
  echo "purged /etc/kairon and removed the 'kairon' user"
else
  echo "kept /etc/kairon and the 'kairon' user (pass --purge to remove them)"
fi
echo "uninstall complete"
UNINSTALL_EOF
)"
  ssh_exec_privileged "$REMOTE" "${SUDO} bash -s -- $PURGE" <<< "$remote_script"
}

verify_remote() {
  local unit="$1" port="$2" scheme="${3:-http}"
  # -k: the dashboard's own certificate may be self-signed and is addressed
  # as 127.0.0.1 here; this probe checks liveness, not the certificate.
  ssh_cmd "$REMOTE" "
    systemctl is-active $unit 2>/dev/null || true
    curl -sk -o /dev/null -w '%{http_code}\n' ${scheme}://127.0.0.1:$port/healthz 2>/dev/null
    curl -sk -o /dev/null -w '%{http_code}\n' ${scheme}://127.0.0.1:$port/readyz 2>/dev/null
  " || true
}

# remote_build copies a small source tarball and scripts/lib/remote-build.sh to
# the host and compiles there, writing the binaries (and, for the dashboard,
# kairon-ui-web.tar.gz) into the staging directory so install.sh finds them
# exactly where a local build would have put them.
remote_build() {
  local local_stage="$1" remote_tmp="$2" targets="$3"
  local src="$local_stage/kairon-src.tar.gz" tar_extra=()
  if tar --no-xattrs -cf /dev/null -T /dev/null 2>/dev/null; then tar_extra+=(--no-xattrs); fi
  info "packing the source (cmd, internal, charts, web, go.mod)..."
  if git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    # Tracked and untracked-but-not-ignored files, so uncommitted edits ship
    # and node_modules/dist/bin never do.
    git -C "$REPO_ROOT" ls-files -z --cached --others --exclude-standard -- go.mod go.sum VERSION cmd internal charts web \
      | while IFS= read -r -d '' f; do [[ -e "$REPO_ROOT/$f" ]] && printf '%s\0' "$f"; done \
      | ( cd "$REPO_ROOT" && COPYFILE_DISABLE=1 tar ${tar_extra[@]+"${tar_extra[@]}"} --null -T - -czf "$src" )
  else
    ( cd "$REPO_ROOT" && COPYFILE_DISABLE=1 tar ${tar_extra[@]+"${tar_extra[@]}"} --exclude=node_modules --exclude=dist \
        -czf "$src" go.mod go.sum VERSION cmd internal charts web )
  fi
  [[ -s "$src" ]] || die "could not pack the source tree from $REPO_ROOT"
  info "source tarball: $(du -k "$src" | cut -f1) KiB; copying to ${REMOTE}"
  scp_cmd "$src" "$REPO_ROOT/scripts/lib/remote-build.sh" "${REMOTE}:${remote_tmp}/"
  info "compiling on ${REMOTE} (first run downloads a Go toolchain and modules into ~/.cache/kairon-build)..."
  # shellcheck disable=SC2086 # targets is a space-separated word list
  ssh_cmd "$REMOTE" "bash ${remote_tmp}/remote-build.sh ${remote_tmp}/kairon-src.tar.gz ${remote_tmp} ${RESOLVED_GOARCH} ${RESOLVED_VERSION} ${targets}" \
    || die "the build on ${REMOTE} failed (see the output above; the build cache is ~/.cache/kairon-build)"
  ok "built on ${REMOTE}"
}

save_deploy_last() {
  {
    echo "HOST=$HOST_ARG"
    echo "DEPLOY_USER=$USER_ARG"
    echo "MODE=node$([[ "$WITH_CONTROLLER" == "1" ]] && echo +controller)$([[ "$WITH_UI" == "1" ]] && echo +ui)"
    echo "VERSION=$RESOLVED_VERSION"
    echo "COMMIT=$RESOLVED_COMMIT"
    echo "UPDATED=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "$DEPLOY_LAST_FILE"
  chmod 600 "$DEPLOY_LAST_FILE"
}

run_deploy() {
  resolve_version
  RESOLVED_FLUXVM_URL="${FLUXVM_URL_OVERRIDE:-http://127.0.0.1:7788}"
  RESOLVED_BACKEND="${BACKEND_OVERRIDE:-qemu}"
  RESOLVED_IMAGE_ROOT="${IMAGE_ROOT_OVERRIDE:-/var/lib/fluxvm/images}"

  if [[ "$DRY_RUN" == "1" ]]; then
    RESOLVED_NODE_NAME="${NODE_NAME_OVERRIDE:-<remote hostname, detected at run time>}"
    RESOLVED_GOARCH="${ARCH_OVERRIDE:-<auto-detected at run time>}"
    print_plan
    return 0
  fi

  if [[ "$WITH_UI" == "1" ]]; then
    if [[ -n "$UI_TOKEN" ]]; then
      RESOLVED_UI_TOKEN="$UI_TOKEN"
      RESOLVED_UI_TOKEN_SOURCE="explicit"
    elif [[ "$UI_ALLOW_UNAUTHENTICATED" != "1" ]]; then
      command -v openssl >/dev/null 2>&1 || die "--with-ui needs a token: install 'openssl' (used to auto-generate one), or pass --ui-token=..., or pass --ui-allow-unauthenticated (local/dev only)"
      RESOLVED_UI_TOKEN="$(openssl rand -hex 24)"
      RESOLVED_UI_TOKEN_SOURCE="generated"
      warn "auto-generated a dashboard token (shown once at the end of this run) -- pass --ui-token=... to pin it across redeploys"
    fi
    if [[ "$UI_ALLOW_UNAUTHENTICATED" != "1" ]]; then
      command -v openssl >/dev/null 2>&1 || die "--with-ui needs 'openssl' to generate the dashboard session secret"
      RESOLVED_UI_SESSION_SECRET="$(openssl rand -hex 32)"
    fi
  fi
  if [[ "$WITH_CONSOLE" == "1" ]]; then
    # Unlike UI_TOKEN, this is never typed by a human -- it only ever
    # travels between kairon-node and kairon-ui on the same deploy, so
    # there's no --console-token override flag; it's just regenerated
    # (and re-applied, if the env files are fresh) on every deploy.
    command -v openssl >/dev/null 2>&1 || die "--with-console needs a token: install 'openssl' (used to auto-generate one)"
    RESOLVED_CONSOLE_TOKEN="$(openssl rand -hex 32)"
  fi

  info "connecting to ${USER_ARG}@${HOST_ARG}..."
  local remote_hostname
  remote_hostname="$(ssh_cmd "$REMOTE" hostname)" || die "could not SSH to ${REMOTE}"
  ok "connected (remote hostname: $remote_hostname)"
  # Kubernetes Node names are always lowercase DNS-1123 labels; 'hostname'
  # commonly returns mixed case, which would silently mismatch spec.nodeName
  # and cause kairon-node to skip every machine assigned to it with no error.
  RESOLVED_NODE_NAME="${NODE_NAME_OVERRIDE:-$(tr '[:upper:]' '[:lower:]' <<< "$remote_hostname")}"

  detect_sudo
  detect_arch
  info "target arch: linux/$RESOLVED_GOARCH"

  local build_dir=""
  if [[ "$REMOTE_BUILD" == "1" ]]; then
    info "--remote-build: the source will be copied to ${REMOTE} and compiled there (version $RESOLVED_VERSION)"
  else
    build_dir="$REPO_ROOT/dist/deploy-remote/$RESOLVED_GOARCH"
    mkdir -p "$build_dir"
    info "cross-compiling for linux/$RESOLVED_GOARCH (version $RESOLVED_VERSION)..."
    ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$RESOLVED_GOARCH" go build -trimpath -ldflags="-s -w -X main.version=$RESOLVED_VERSION" -o "$build_dir/kairon-node" ./cmd/kairon-node )
    ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$RESOLVED_GOARCH" go build -trimpath -ldflags="-s -w -X main.version=$RESOLVED_VERSION" -o "$build_dir/kaironctl" ./cmd/kaironctl )
    if [[ "$WITH_CONTROLLER" == "1" ]]; then
      ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$RESOLVED_GOARCH" go build -trimpath -ldflags="-s -w -X main.version=$RESOLVED_VERSION" -o "$build_dir/kairon-controller" ./cmd/kairon-controller )
    fi
    if [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" ]]; then
      ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$RESOLVED_GOARCH" go build -trimpath -o "$build_dir/kairon-migration-adapter-stub" ./cmd/kairon-migration-adapter-stub )
    fi
    if [[ "$WITH_UI" == "1" ]]; then
      ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$RESOLVED_GOARCH" go build -trimpath -ldflags="-s -w -X main.version=$RESOLVED_VERSION" -o "$build_dir/kairon-ui" ./cmd/kairon-ui )
    fi
    ok "build complete: $build_dir"

    if [[ "$WITH_UI" == "1" ]]; then
      command -v npm >/dev/null 2>&1 || die "--with-ui requires 'npm' on PATH to build web/dist locally (the only place this script needs Node.js, and only with --with-ui)"
      info "building kairon-ui web assets (npm ci && npm run build)..."
      ( cd "$REPO_ROOT/web" && npm ci && npm run build )
      [[ -d "$REPO_ROOT/web/dist" ]] || die "npm run build did not produce web/dist -- check the build output above"
      ok "web/dist built"
    fi

  fi

  local local_stage
  local_stage="$(mktemp -d)"
  trap 'rm -rf "$local_stage"' RETURN

  if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
    if [[ -n "$CONSOLE_TLS_CERT" ]]; then
      RESOLVED_CONSOLE_TLS_SOURCE="explicit"
      cp "$CONSOLE_TLS_CA" "$local_stage/console-tls-ca.pem"
      cp "$CONSOLE_TLS_CERT" "$local_stage/console-tls-cert.pem"
      cp "$CONSOLE_TLS_KEY" "$local_stage/console-tls-key.pem"
      ok "using provided console TLS materials (--console-tls-cert/-key/-ca)"
    else
      command -v openssl >/dev/null 2>&1 || die "--console-tls needs a certificate: install 'openssl' (used to auto-generate a self-signed one), or pass --console-tls-cert/-key/-ca"
      # Auto-generated, same convention as the console/UI tokens above: a
      # private CA + server cert scoped to this one deploy, not something
      # an operator is expected to already have lying around. kairon-ui
      # verifies this cert against whatever address it dials kairon-node's
      # console relay at (the Machine's Node's status.addresses internal
      # IP, per internal/uiapi/console.go's nodeInternalIP -- NOT
      # necessarily $HOST_ARG, which may be a different address, e.g. a
      # public IP used only for SSH) -- so the SAN list below is
      # deliberately broad (every address this host reports, plus the
      # hostname and $HOST_ARG) to maximize the chance it already covers
      # whatever Kubernetes reports as this Node's InternalIP. If it
      # doesn't, TLS verification will fail with a clear certificate error
      # naming the mismatched address -- regenerate with an explicit
      # --console-tls-cert/-key/-ca covering the right one.
      info "auto-generating a self-signed console TLS certificate..."
      local remote_ips san_list
      remote_ips="$(ssh_cmd "$REMOTE" "hostname -I" 2>/dev/null || true)"
      san_list="DNS:kairon-node,DNS:${remote_hostname},IP:127.0.0.1"
      local ip
      for ip in $remote_ips; do
        san_list="${san_list},IP:${ip}"
      done
      if [[ "$HOST_ARG" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && "$san_list" != *"IP:${HOST_ARG}"* ]]; then
        san_list="${san_list},IP:${HOST_ARG}"
      fi
      openssl req -x509 -newkey rsa:2048 -sha256 -days 3650 -nodes \
        -keyout "$local_stage/console-tls-ca-key.pem" -out "$local_stage/console-tls-ca.pem" \
        -subj "/CN=kairon-console-ca" >/dev/null 2>&1
      openssl req -newkey rsa:2048 -sha256 -nodes \
        -keyout "$local_stage/console-tls-key.pem" -out "$local_stage/console-tls.csr" \
        -subj "/CN=${remote_hostname}" >/dev/null 2>&1
      openssl x509 -req -sha256 -days 825 \
        -in "$local_stage/console-tls.csr" \
        -CA "$local_stage/console-tls-ca.pem" -CAkey "$local_stage/console-tls-ca-key.pem" -CAcreateserial \
        -out "$local_stage/console-tls-cert.pem" \
        -extfile <(printf 'subjectAltName=%s' "$san_list") >/dev/null 2>&1
      RESOLVED_CONSOLE_TLS_SOURCE="generated"
      CONSOLE_TLS_SERVER_NAME="kairon-node"
      ok "generated console TLS certificate (SAN: $san_list)"
    fi
  fi

  if [[ "$UI_TLS" == "1" ]]; then
    if [[ -n "$UI_TLS_CERT" ]]; then
      cp "$UI_TLS_CERT" "$local_stage/ui-tls-cert.pem"
      cp "$UI_TLS_KEY" "$local_stage/ui-tls-key.pem"
      ok "using the provided dashboard TLS certificate (--ui-tls-cert/-key)"
    elif [[ "$UI_TLS_REGEN" != "1" ]] && ssh_exec_privileged "$REMOTE" "${SUDO} test -s /etc/kairon/ui-tls/cert.pem -a -s /etc/kairon/ui-tls/key.pem" >/dev/null 2>&1; then
      UI_TLS_REUSE=1
      ok "reusing the dashboard certificate already installed on the host (--ui-regenerate-cert to replace it)"
    else
      command -v openssl >/dev/null 2>&1 || die "--ui-tls needs a certificate: install 'openssl' (used to generate a self-signed one), or pass --ui-tls-cert/-key"
      local ui_ips ui_sans ui_ip
      ui_ips="$(ssh_cmd "$REMOTE" "hostname -I" 2>/dev/null || true)"
      ui_sans="DNS:${remote_hostname},DNS:localhost,IP:127.0.0.1"
      for ui_ip in $ui_ips; do
        ui_sans="${ui_sans},IP:${ui_ip}"
      done
      if [[ "$HOST_ARG" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && "$ui_sans" != *"IP:${HOST_ARG}"* ]]; then
        ui_sans="${ui_sans},IP:${HOST_ARG}"
      elif [[ ! "$HOST_ARG" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ && "$ui_sans" != *"DNS:${HOST_ARG}"* ]]; then
        ui_sans="${ui_sans},DNS:${HOST_ARG}"
      fi
      info "generating a self-signed dashboard certificate..."
      # Some openssl builds (e.g. a Homebrew install with no openssl.cnf) fail
      # on the default config; an empty one is enough for -subj + -extfile.
      local ui_gen_log="$local_stage/ui-tls-openssl.log"
      ui_tls_generate() {
        openssl req -newkey rsa:2048 -sha256 -nodes \
          -keyout "$local_stage/ui-tls-key.pem" -out "$local_stage/ui-tls.csr" \
          -subj "/CN=${remote_hostname}" >/dev/null 2>"$ui_gen_log" || return 1
        openssl x509 -req -sha256 -days 825 \
          -in "$local_stage/ui-tls.csr" -signkey "$local_stage/ui-tls-key.pem" \
          -out "$local_stage/ui-tls-cert.pem" \
          -extfile <(printf 'subjectAltName=%s\nbasicConstraints=CA:FALSE' "$ui_sans") >/dev/null 2>>"$ui_gen_log" || return 1
        [[ -s "$local_stage/ui-tls-cert.pem" ]]
      }
      if ! ui_tls_generate && ! OPENSSL_CONF=/dev/null ui_tls_generate; then
        die "could not generate the dashboard certificate with openssl: $(tr '\n' ' ' < "$ui_gen_log" | cut -c1-300) -- install a working openssl or pass --ui-tls-cert/-key"
      fi
      ok "generated a self-signed dashboard certificate (SAN: $ui_sans)"
    fi
  fi

  {
    printf 'WITH_CONTROLLER=%q\n' "$WITH_CONTROLLER"
    printf 'NO_START=%q\n' "$NO_START"
    printf 'NODE_NAME=%q\n' "$RESOLVED_NODE_NAME"
    printf 'FLUXVM_URL=%q\n' "$RESOLVED_FLUXVM_URL"
    printf 'KAIRON_DEFAULT_BACKEND=%q\n' "$RESOLVED_BACKEND"
    printf 'KAIRON_IMAGE_ROOT=%q\n' "$RESOLVED_IMAGE_ROOT"
    printf 'KUBE_URL=%q\n' "$KUBE_URL"
    printf 'KUBE_TOKEN=%q\n' "$KUBE_TOKEN"
    printf 'KUBE_CA=%q\n' "$KUBE_CA"
    printf 'KUBE_INSECURE=%q\n' "$KUBE_INSECURE"
    printf 'INTERVAL=%q\n' "$INTERVAL"
    printf 'NODE_PORT=%q\n' "$NODE_PORT"
    printf 'NODE_PORT_EXPLICIT=%q\n' "$NODE_PORT_EXPLICIT"
    printf 'CONTROLLER_PORT=%q\n' "$CONTROLLER_PORT"
    printf 'CONTROLLER_PORT_EXPLICIT=%q\n' "$CONTROLLER_PORT_EXPLICIT"
    printf 'MIGRATION_CONFIGURED=%q\n' "$MIGRATION_CONFIGURED"
    printf 'MIGRATION_SERVER_NAME=%q\n' "$MIGRATION_SERVER_NAME"
    printf 'MIGRATION_PORT=%q\n' "$MIGRATION_PORT"
    printf 'MIGRATION_PORT_EXPLICIT=%q\n' "$MIGRATION_PORT_EXPLICIT"
    printf 'MIGRATION_ADAPTER_SOCKET=%q\n' "$MIGRATION_ADAPTER_SOCKET"
    printf 'WITH_MIGRATION_ADAPTER_STUB=%q\n' "$WITH_MIGRATION_ADAPTER_STUB"
    printf 'WITH_UI=%q\n' "$WITH_UI"
    printf 'UI_ALLOW_UNAUTHENTICATED=%q\n' "$UI_ALLOW_UNAUTHENTICATED"
    printf 'UI_TOKEN=%q\n' "$RESOLVED_UI_TOKEN"
    # UI_TOKEN above may be auto-generated; this records whether the operator
    # actually passed --ui-token, so "flag ignored" warnings are truthful.
    printf 'UI_TOKEN_EXPLICIT=%q\n' "$([[ -n "$UI_TOKEN" ]] && echo 1 || echo 0)"
    printf 'UI_ADMIN_PASSWORD=%q\n' "$UI_ADMIN_PASSWORD"
    printf 'UI_SESSION_SECRET=%q\n' "$RESOLVED_UI_SESSION_SECRET"
    printf 'RESEED_UI_AUTH=%q\n' "$RESEED_UI_AUTH"
    printf 'APPLY_CRDS=%q\n' "$APPLY_CRDS"
    printf 'APPLY_RBAC=%q\n' "$APPLY_RBAC"
    printf 'UI_PORT=%q\n' "$UI_PORT"
    printf 'UI_PORT_EXPLICIT=%q\n' "$UI_PORT_EXPLICIT"
    printf 'WITH_CONSOLE=%q\n' "$WITH_CONSOLE"
    printf 'CONSOLE_TOKEN=%q\n' "$RESOLVED_CONSOLE_TOKEN"
    printf 'CONSOLE_PORT=%q\n' "$CONSOLE_PORT"
    printf 'CONSOLE_PORT_EXPLICIT=%q\n' "$CONSOLE_PORT_EXPLICIT"
    printf 'WITH_CONSOLE_TLS=%q\n' "$WITH_CONSOLE_TLS"
    printf 'CONSOLE_INSECURE_HTTP=%q\n' "$CONSOLE_INSECURE_HTTP"
    printf 'CONSOLE_TLS_SERVER_NAME=%q\n' "$CONSOLE_TLS_SERVER_NAME"
    printf 'RESERVED_CPUS=%q\n' "$RESERVED_CPUS"
    printf 'IMAGE_CACHE_DIR=%q\n' "$IMAGE_CACHE_DIR"
    printf 'UI_TLS=%q\n' "$UI_TLS"
    printf 'UI_INSECURE_HTTP=%q\n' "$UI_INSECURE_HTTP"
    printf 'UI_TLS_REUSE=%q\n' "$UI_TLS_REUSE"
    printf 'CPU_PINNING=%q\n' "$CPU_PINNING"
  } > "$local_stage/params.env"

  cat > "$local_stage/install.sh" <<'INSTALL_EOF'
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck disable=SC1091
source ./params.env

info() { printf '[*] %s\n' "$*"; }
ok()   { printf '[+] %s\n' "$*"; }
warn() { printf '[!] %s\n' "$*" >&2; }

# Idempotent KEY=VALUE upsert into an env file (also un-comments a #KEY= line).
set_env_key() {
  local file="$1" key="$2" value="$3"
  if grep -qE "^#?${key}=" "$file"; then
    sed -i -E "s#^\#?${key}=.*#${key}=${value}#" "$file"
  else
    echo "${key}=${value}" >> "$file"
  fi
}

# A port held only by OWNER (the binary this deploy is about to restart) is
# not in use. ss reports the kernel comm name, truncated to 15 characters.
port_in_use() {
  local port="$1" owner="${2:-}" listeners
  listeners="$(ss -ltnp 2>/dev/null | awk -v p=":$port" 'substr($4, length($4) - length(p) + 1) == p')"
  [[ -n "$listeners" ]] || return 1
  [[ -n "$owner" ]] || return 0
  grep -qvF "((\"${owner:0:15}\"," <<<"$listeners"
}

# Port a flag (--listen / --health-addr) carries in an installed unit, or
# nothing when the unit is absent or has no such flag.
unit_port() {
  local unit="$1" flag="$2"
  [[ -f "$unit" ]] || return 0
  grep -oE -- "--${flag}=:[0-9]+" "$unit" | head -1 | cut -d: -f2 || true
}

port_owner() {
  case "$1" in
    node|console|migration) echo kairon-node ;;
    controller) echo kairon-controller ;;
    ui) echo kairon-ui ;;
  esac
}

# Resolves the port to actually use for a health/relay server: if the
# caller explicitly asked for a port, that port is used as-is (a busy
# explicit port is a hard failure, not silently overridden). Otherwise, if
# the default is busy (common: some unrelated service already on it), a
# random free port in the ephemeral range is picked automatically.
resolve_port() {
  local desired="$1" explicit="$2" name="$3" current="${4:-}"
  # Re-deploy: keep the port the installed unit already uses, so the URL
  # does not change every time the default happens to be taken. Written to
  # stderr because stdout is captured as the result.
  if [[ "$explicit" != "1" && -n "$current" && "$current" != "$desired" ]] && ! port_in_use "$current" "$(port_owner "$name")"; then
    echo "[*] keeping port $current from the installed $name unit (default $desired not used)" >&2
    echo "$current"
    return 0
  fi
  if ! port_in_use "$desired" "$(port_owner "$name")"; then
    echo "$desired"
    return 0
  fi
  if [[ "$explicit" == "1" ]]; then
    echo "ERROR: --$name-port=$desired is already in use on this host" >&2
    return 1
  fi
  warn "default port $desired for $name is already in use on this host -- picking a random free port"
  local tries=0 candidate
  while [[ $tries -lt 50 ]]; do
    candidate=$(( (RANDOM % 20000) + 20000 ))
    if ! port_in_use "$candidate"; then
      warn "$name port auto-selected: $candidate (pass --$name-port=$candidate to pin it on future deploys)"
      echo "$candidate"
      return 0
    fi
    tries=$((tries + 1))
  done
  echo "ERROR: could not find a free port for $name after 50 attempts" >&2
  return 1
}

if ! getent group kairon >/dev/null 2>&1; then
  groupadd --system kairon
fi
if ! id kairon >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin -g kairon kairon
  ok "created system user 'kairon'"
fi

install -d -m 0750 -o root -g kairon /etc/kairon

# Resolved early (before any config files reference it) so kairon-node.env
# and kairon-ui.env can both be seeded with the SAME actually-applied
# port, not just the caller's desired one -- an auto-fallback pick made
# separately by each file's seeding step could otherwise disagree.
RESOLVED_CONSOLE_PORT=""
if [[ "$WITH_CONSOLE" == "1" ]]; then
  RESOLVED_CONSOLE_PORT="$(resolve_port "$CONSOLE_PORT" "$CONSOLE_PORT_EXPLICIT" "console")" || exit 1
fi

install -m 0755 -o root -g root ./kairon-node /usr/bin/kairon-node
install -m 0755 -o root -g root ./kaironctl /usr/bin/kaironctl
ok "installed kairon-node, kaironctl to /usr/bin"

# CRDs and RBAC come from the repo's generated bundles (RBAC is rendered from
# the Helm chart, so the chart stays the single source of truth).
kube_apply() {
  local kc=""
  if command -v kubectl >/dev/null 2>&1; then kc="kubectl"
  elif command -v k3s >/dev/null 2>&1; then kc="k3s kubectl"
  fi
  if [[ -z "$kc" ]]; then
    warn "no kubectl/k3s on this host; apply $* from a machine with cluster access: kubectl apply --server-side --force-conflicts -f <file>"
    return 0
  fi
  local f
  for f in "$@"; do
    if $kc apply --server-side --force-conflicts -f "$f"; then
      ok "applied $(basename "$f")"
    else
      warn "kubectl apply of $(basename "$f") failed; apply it manually (see output above)"
    fi
  done
}
if [[ "$APPLY_CRDS" == "1" ]]; then kube_apply ./crd.yaml ./fleet-crds.yaml; fi
if [[ "$APPLY_RBAC" == "1" ]]; then kube_apply ./rbac.yaml; fi

if [[ "$WITH_CONTROLLER" == "1" ]]; then
  install -m 0755 -o root -g root ./kairon-controller /usr/bin/kairon-controller
  ok "installed kairon-controller to /usr/bin"
fi

if [[ "$WITH_UI" == "1" ]]; then
  install -m 0755 -o root -g root ./kairon-ui /usr/bin/kairon-ui
  ok "installed kairon-ui to /usr/bin"
fi

if [[ ! -f /etc/kairon/kairon-node.env ]]; then
  {
    echo "NODE_NAME=$NODE_NAME"
    echo "FLUXVM_URL=$FLUXVM_URL"
    echo "KAIRON_DEFAULT_BACKEND=$KAIRON_DEFAULT_BACKEND"
    echo "KAIRON_IMAGE_ROOT=$KAIRON_IMAGE_ROOT"
    if [[ -n "$KUBE_URL" ]]; then echo "KAIRON_KUBE_URL=$KUBE_URL"; else echo "#KAIRON_KUBE_URL="; fi
    if [[ -n "$KUBE_TOKEN" ]]; then echo "KAIRON_KUBE_TOKEN=$KUBE_TOKEN"; else echo "#KAIRON_KUBE_TOKEN="; fi
    if [[ -n "$KUBE_CA" ]]; then echo "KAIRON_KUBE_CA=$KUBE_CA"; else echo "#KAIRON_KUBE_CA="; fi
    if [[ "$KUBE_INSECURE" == "1" ]]; then echo "KAIRON_KUBE_INSECURE=true"; else echo "#KAIRON_KUBE_INSECURE=false"; fi
    echo "#FLUXVM_TOKEN="
    if [[ "$MIGRATION_CONFIGURED" == "1" ]]; then
      echo "KAIRON_MIGRATION_CA=/etc/kairon/migration/ca.pem"
      echo "KAIRON_MIGRATION_CERT=/etc/kairon/migration/cert.pem"
      echo "KAIRON_MIGRATION_KEY=/etc/kairon/migration/key.pem"
      echo "KAIRON_MIGRATION_SERVER_NAME=$MIGRATION_SERVER_NAME"
      echo "KAIRON_MIGRATION_ADAPTER_SOCKET=$MIGRATION_ADAPTER_SOCKET"
    else
      echo "#KAIRON_MIGRATION_CA="
      echo "#KAIRON_MIGRATION_CERT="
      echo "#KAIRON_MIGRATION_KEY="
    fi
    if [[ "$WITH_CONSOLE" == "1" ]]; then
      echo "KAIRON_NODE_CONSOLE_TOKEN=$CONSOLE_TOKEN"
    else
      echo "#KAIRON_NODE_CONSOLE_TOKEN="
    fi
    if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
      echo "KAIRON_NODE_CONSOLE_TLS_CERT=/etc/kairon/console-tls/cert.pem"
      echo "KAIRON_NODE_CONSOLE_TLS_KEY=/etc/kairon/console-tls/key.pem"
    else
      echo "#KAIRON_NODE_CONSOLE_TLS_CERT="
      echo "#KAIRON_NODE_CONSOLE_TLS_KEY="
    fi
  } > /etc/kairon/kairon-node.env
  chmod 0640 /etc/kairon/kairon-node.env
  chown root:kairon /etc/kairon/kairon-node.env
  ok "seeded /etc/kairon/kairon-node.env with defaults"
else
  warn "/etc/kairon/kairon-node.env already exists -- left untouched"
  if [[ -n "$KUBE_URL$KUBE_TOKEN$KUBE_CA" || "$KUBE_INSECURE" == "1" ]]; then
    warn "--kube-* flags were given but ignored because the env file already exists"
    warn "edit /etc/kairon/kairon-node.env by hand, then: systemctl restart kairon-node"
  fi
  if [[ "$MIGRATION_CONFIGURED" == "1" ]]; then
    warn "--migration-* flags were given but ignored because the env file already exists"
  fi
  if [[ "$WITH_CONSOLE" == "1" ]]; then
    warn "--with-console was given but KAIRON_NODE_CONSOLE_TOKEN was NOT applied because the env file already exists"
    warn "edit /etc/kairon/kairon-node.env by hand (KAIRON_NODE_CONSOLE_TOKEN must match kairon-ui.env's), then: systemctl restart kairon-node"
  fi
  if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
    set_env_key /etc/kairon/kairon-node.env KAIRON_NODE_CONSOLE_TLS_CERT /etc/kairon/console-tls/cert.pem
    set_env_key /etc/kairon/kairon-node.env KAIRON_NODE_CONSOLE_TLS_KEY /etc/kairon/console-tls/key.pem
    ok "console relay TLS wired into /etc/kairon/kairon-node.env"
  elif [[ "$CONSOLE_INSECURE_HTTP" == "1" ]]; then
    sed -i -E '/^KAIRON_NODE_CONSOLE_TLS_(CERT|KEY)=/d' /etc/kairon/kairon-node.env
    warn "console relay served as plain HTTP (--console-insecure-http)"
  fi
fi

# Sets KEY=VALUE in an env file, replacing a live or commented-out line.

# Image cache: kairon-node refuses spec.image.source (and the self-test's
# downloaded CirrOS) without one. The unit runs with ProtectSystem=strict, so
# a directory outside its own state directory needs a ReadWritePaths drop-in.
if [[ -n "$IMAGE_CACHE_DIR" ]]; then
  install -d -m 0755 -o kairon -g kairon "$IMAGE_CACHE_DIR"
  set_env_key /etc/kairon/kairon-node.env KAIRON_IMAGE_CACHE_DIR "$IMAGE_CACHE_DIR"
  case "$IMAGE_CACHE_DIR" in
    /var/lib/kairon-node|/var/lib/kairon-node/*) rm -f /etc/systemd/system/kairon-node.service.d/image-cache.conf ;;
    *)
      install -d -m 0755 /etc/systemd/system/kairon-node.service.d
      printf '[Service]\nReadWritePaths=%s\n' "$IMAGE_CACHE_DIR" > /etc/systemd/system/kairon-node.service.d/image-cache.conf
      ;;
  esac
  ok "set KAIRON_IMAGE_CACHE_DIR=$IMAGE_CACHE_DIR in /etc/kairon/kairon-node.env"
fi

CPU_STATE_COPY=/var/lib/kairon-node/cpu_manager_state
if [[ -n "$RESERVED_CPUS" ]]; then
  set_env_key /etc/kairon/kairon-node.env KAIRON_RESERVED_CPUS "$RESERVED_CPUS"
  ok "set KAIRON_RESERVED_CPUS=$RESERVED_CPUS in /etc/kairon/kairon-node.env"
fi
if [[ "$CPU_PINNING" == "1" ]]; then
  if ! grep -qE '^KAIRON_RESERVED_CPUS=.+' /etc/kairon/kairon-node.env; then
    echo "ERROR: --cpu-pinning needs reserved CPUs: pass --reserved-cpus=LIST (kubelet's reserved set)" >&2
    exit 1
  fi
  [[ -f /var/lib/kubelet/cpu_manager_state ]] || warn "/var/lib/kubelet/cpu_manager_state not found -- discovery will refuse until kubelet writes it"
  install -m 0644 -o root -g root ./kairon-cpustate.service /etc/systemd/system/kairon-cpustate.service
  install -m 0644 -o root -g root ./kairon-cpustate.path /etc/systemd/system/kairon-cpustate.path
  set_env_key /etc/kairon/kairon-node.env KAIRON_CPU_MANAGER_STATE "$CPU_STATE_COPY"
  ok "installed kairon-cpustate.path/.service; KAIRON_CPU_MANAGER_STATE=$CPU_STATE_COPY"
elif [[ "$CPU_PINNING" == "0" ]]; then
  systemctl disable --now kairon-cpustate.path 2>/dev/null || true
  rm -f /etc/systemd/system/kairon-cpustate.path /etc/systemd/system/kairon-cpustate.service "$CPU_STATE_COPY"
  sed -i -E '/^(KAIRON_RESERVED_CPUS|KAIRON_CPU_MANAGER_STATE)=/d' /etc/kairon/kairon-node.env
  ok "pinnable-CPU discovery turned off (units, state copy and env keys removed)"
fi

if [[ "$MIGRATION_CONFIGURED" == "1" ]]; then
  install -d -m 0750 -o root -g kairon /etc/kairon/migration
  install -m 0640 -o root -g kairon ./migration-ca.pem /etc/kairon/migration/ca.pem
  install -m 0640 -o root -g kairon ./migration-cert.pem /etc/kairon/migration/cert.pem
  install -m 0640 -o root -g kairon ./migration-key.pem /etc/kairon/migration/key.pem
  ok "installed migration mTLS materials to /etc/kairon/migration/"
fi

if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
  install -d -m 0750 -o root -g kairon /etc/kairon/console-tls
  install -m 0640 -o root -g kairon ./console-tls-ca.pem /etc/kairon/console-tls/ca.crt
  install -m 0640 -o root -g kairon ./console-tls-cert.pem /etc/kairon/console-tls/cert.pem
  install -m 0640 -o root -g kairon ./console-tls-key.pem /etc/kairon/console-tls/key.pem
  ok "installed console TLS materials to /etc/kairon/console-tls/"
fi

if [[ "$WITH_UI" == "1" ]]; then
  # Pure build output, not hand-edited config -- always replaced on redeploy
  # (unlike kairon-ui.env below) so a version bump never leaves stale chunks
  # behind alongside the new ones.
  rm -rf /etc/kairon/ui-web
  install -d -m 0750 -o root -g kairon /etc/kairon/ui-web
  tar -xzf ./kairon-ui-web.tar.gz -C /etc/kairon/ui-web
  find /etc/kairon/ui-web -type d -exec chmod 0750 {} \; -exec chown root:kairon {} \;
  find /etc/kairon/ui-web -type f -exec chmod 0640 {} \; -exec chown root:kairon {} \;
  ok "installed web dashboard assets to /etc/kairon/ui-web"

  if [[ ! -f /etc/kairon/kairon-ui.env ]]; then
    {
      if [[ -n "$UI_TOKEN" ]]; then echo "KAIRON_UI_TOKEN=$UI_TOKEN"; else echo "#KAIRON_UI_TOKEN="; fi
      if [[ "$UI_ALLOW_UNAUTHENTICATED" == "1" ]]; then echo "KAIRON_UI_ALLOW_UNAUTHENTICATED=true"; else echo "#KAIRON_UI_ALLOW_UNAUTHENTICATED=false"; fi
      if [[ -n "$UI_SESSION_SECRET" ]]; then
        echo "KAIRON_UI_DEFAULT_ADMIN_PASSWORD=$UI_ADMIN_PASSWORD"
        echo "KAIRON_UI_SESSION_SECRET=$UI_SESSION_SECRET"
      else
        echo "#KAIRON_UI_DEFAULT_ADMIN_PASSWORD="
        echo "#KAIRON_UI_SESSION_SECRET="
      fi
      if [[ -n "$KUBE_URL" ]]; then echo "KAIRON_KUBE_URL=$KUBE_URL"; else echo "#KAIRON_KUBE_URL="; fi
      if [[ -n "$KUBE_TOKEN" ]]; then echo "KAIRON_KUBE_TOKEN=$KUBE_TOKEN"; else echo "#KAIRON_KUBE_TOKEN="; fi
      if [[ -n "$KUBE_CA" ]]; then echo "KAIRON_KUBE_CA=$KUBE_CA"; else echo "#KAIRON_KUBE_CA="; fi
      if [[ "$KUBE_INSECURE" == "1" ]]; then echo "KAIRON_KUBE_INSECURE=true"; else echo "#KAIRON_KUBE_INSECURE=false"; fi
      if [[ "$WITH_CONSOLE" == "1" ]]; then
        echo "KAIRON_NODE_CONSOLE_TOKEN=$CONSOLE_TOKEN"
        echo "KAIRON_NODE_CONSOLE_PORT=$RESOLVED_CONSOLE_PORT"
      else
        echo "#KAIRON_NODE_CONSOLE_TOKEN="
        echo "#KAIRON_NODE_CONSOLE_PORT="
      fi
      if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
        echo "KAIRON_NODE_CONSOLE_CA=/etc/kairon/console-tls/ca.crt"
        if [[ -n "$CONSOLE_TLS_SERVER_NAME" ]]; then
          echo "KAIRON_NODE_CONSOLE_SERVER_NAME=$CONSOLE_TLS_SERVER_NAME"
        fi
      else
        echo "#KAIRON_NODE_CONSOLE_CA="
      fi
    } > /etc/kairon/kairon-ui.env
    chmod 0640 /etc/kairon/kairon-ui.env
    chown root:kairon /etc/kairon/kairon-ui.env
    ok "seeded /etc/kairon/kairon-ui.env"
  else
    if [[ "$RESEED_UI_AUTH" == "1" && -n "$UI_SESSION_SECRET" ]]; then
      # Only the two username/password keys are touched; token, kube and
      # console settings stay exactly as the operator left them.
      cp -p /etc/kairon/kairon-ui.env /etc/kairon/kairon-ui.env.bak
      grep -vE '^#?KAIRON_UI_DEFAULT_ADMIN_PASSWORD=' /etc/kairon/kairon-ui.env > /etc/kairon/kairon-ui.env.new || true
      echo "KAIRON_UI_DEFAULT_ADMIN_PASSWORD=$UI_ADMIN_PASSWORD" >> /etc/kairon/kairon-ui.env.new
      if ! grep -qE '^KAIRON_UI_SESSION_SECRET=.+' /etc/kairon/kairon-ui.env.new; then
        grep -vE '^#?KAIRON_UI_SESSION_SECRET=' /etc/kairon/kairon-ui.env.new > /etc/kairon/kairon-ui.env.new2 || true
        echo "KAIRON_UI_SESSION_SECRET=$UI_SESSION_SECRET" >> /etc/kairon/kairon-ui.env.new2
        mv /etc/kairon/kairon-ui.env.new2 /etc/kairon/kairon-ui.env.new
      fi
      chmod 0640 /etc/kairon/kairon-ui.env.new
      chown root:kairon /etc/kairon/kairon-ui.env.new
      mv /etc/kairon/kairon-ui.env.new /etc/kairon/kairon-ui.env
      ok "re-applied admin password + session secret in /etc/kairon/kairon-ui.env (backup: kairon-ui.env.bak)"
    else
    warn "/etc/kairon/kairon-ui.env already exists -- left untouched"
    if [[ -n "$UI_SESSION_SECRET" ]]; then
      warn "username/password login keys were NOT applied; re-run with --reseed-ui-auth to add KAIRON_UI_DEFAULT_ADMIN_PASSWORD + KAIRON_UI_SESSION_SECRET"
    fi
    if [[ "$UI_TOKEN_EXPLICIT" == "1" || -n "$KUBE_URL$KUBE_TOKEN$KUBE_CA" || "$UI_ALLOW_UNAUTHENTICATED" == "1" || "$KUBE_INSECURE" == "1" ]]; then
      warn "--ui-token/--ui-allow-unauthenticated/--kube-* flags were given but ignored because the env file already exists"
      warn "edit /etc/kairon/kairon-ui.env by hand, then: systemctl restart kairon-ui"
    fi
    if [[ "$WITH_CONSOLE" == "1" ]]; then
      warn "--with-console was given but KAIRON_NODE_CONSOLE_TOKEN/_PORT were NOT applied because the env file already exists"
      warn "edit /etc/kairon/kairon-ui.env by hand (KAIRON_NODE_CONSOLE_TOKEN must match kairon-node.env's), then: systemctl restart kairon-ui"
    fi
    if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
      set_env_key /etc/kairon/kairon-ui.env KAIRON_NODE_CONSOLE_CA /etc/kairon/console-tls/ca.crt
      if [[ -n "$CONSOLE_TLS_SERVER_NAME" ]]; then
        set_env_key /etc/kairon/kairon-ui.env KAIRON_NODE_CONSOLE_SERVER_NAME "$CONSOLE_TLS_SERVER_NAME"
      else
        sed -i -E '/^KAIRON_NODE_CONSOLE_SERVER_NAME=/d' /etc/kairon/kairon-ui.env
      fi
      ok "console relay TLS wired into /etc/kairon/kairon-ui.env"
    elif [[ "$CONSOLE_INSECURE_HTTP" == "1" ]]; then
      sed -i -E '/^KAIRON_NODE_CONSOLE_(CA|SERVER_NAME)=/d' /etc/kairon/kairon-ui.env
    fi
    fi
  fi
fi

if [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" ]]; then
  install -m 0755 -o root -g root ./kairon-migration-adapter-stub /usr/bin/kairon-migration-adapter-stub
  install -m 0644 -o root -g root ./kairon-migration-adapter-stub.service /etc/systemd/system/kairon-migration-adapter-stub.service
  sed -i "s#^ExecStart=.*#ExecStart=/usr/bin/kairon-migration-adapter-stub --socket=$MIGRATION_ADAPTER_SOCKET#" /etc/systemd/system/kairon-migration-adapter-stub.service
  ok "installed kairon-migration-adapter-stub (TEST DOUBLE -- simulates transfers, does not move real VM memory)"
fi

RESOLVED_NODE_PORT="$(resolve_port "$NODE_PORT" "$NODE_PORT_EXPLICIT" "node" "$(unit_port /etc/systemd/system/kairon-node.service health-addr)")" || exit 1
NODE_EXEC_ARGS="--interval=$INTERVAL --health-addr=:$RESOLVED_NODE_PORT --edge-baseline-file=/var/lib/kairon-node/edge-baseline.json"
if [[ "$MIGRATION_CONFIGURED" == "1" ]]; then
  # Every node dials peers on its own --migration-port, so a per-host
  # random fallback can't work: a busy port is an error.
  RESOLVED_MIGRATION_PORT="$(resolve_port "$MIGRATION_PORT" 1 "migration")" || exit 1
  NODE_EXEC_ARGS="$NODE_EXEC_ARGS --migration-addr=:$RESOLVED_MIGRATION_PORT --migration-port=$RESOLVED_MIGRATION_PORT"
fi
if [[ "$WITH_CONSOLE" == "1" ]]; then
  NODE_EXEC_ARGS="$NODE_EXEC_ARGS --console-addr=:$RESOLVED_CONSOLE_PORT"
fi
install -m 0644 -o root -g root ./kairon-node.service /etc/systemd/system/kairon-node.service
sed -i "s#^ExecStart=.*#ExecStart=/usr/bin/kairon-node $NODE_EXEC_ARGS#" /etc/systemd/system/kairon-node.service
if [[ "$WITH_CONTROLLER" == "1" ]]; then
  RESOLVED_CONTROLLER_PORT="$(resolve_port "$CONTROLLER_PORT" "$CONTROLLER_PORT_EXPLICIT" "controller" "$(unit_port /etc/systemd/system/kairon-controller.service health-addr)")" || exit 1
  install -m 0644 -o root -g root ./kairon-controller.service /etc/systemd/system/kairon-controller.service
  sed -i "s#^ExecStart=.*#ExecStart=/usr/bin/kairon-controller --interval=5s --health-addr=:$RESOLVED_CONTROLLER_PORT#" /etc/systemd/system/kairon-controller.service
  if [[ ! -f /etc/kairon/kairon-controller.env ]]; then
    : > /etc/kairon/kairon-controller.env
    chmod 0640 /etc/kairon/kairon-controller.env
    chown root:kairon /etc/kairon/kairon-controller.env
    [[ -n "$KUBE_URL" ]] && echo "KAIRON_KUBE_URL=$KUBE_URL" >> /etc/kairon/kairon-controller.env
    [[ -n "$KUBE_TOKEN" ]] && echo "KAIRON_KUBE_TOKEN=$KUBE_TOKEN" >> /etc/kairon/kairon-controller.env
    [[ -n "$KUBE_CA" ]] && echo "KAIRON_KUBE_CA=$KUBE_CA" >> /etc/kairon/kairon-controller.env
    [[ "$KUBE_INSECURE" == "1" ]] && echo "KAIRON_KUBE_INSECURE=true" >> /etc/kairon/kairon-controller.env
    ok "seeded /etc/kairon/kairon-controller.env"
  fi
fi
if [[ "$WITH_UI" == "1" ]]; then
  RESOLVED_UI_PORT="$(resolve_port "$UI_PORT" "$UI_PORT_EXPLICIT" "ui" "$(unit_port /etc/systemd/system/kairon-ui.service listen)")" || exit 1
  install -m 0644 -o root -g root ./kairon-ui.service /etc/systemd/system/kairon-ui.service
  sed -i "s#^ExecStart=.*#ExecStart=/usr/bin/kairon-ui --listen=:$RESOLVED_UI_PORT --web-dir=/etc/kairon/ui-web#" /etc/systemd/system/kairon-ui.service
  if [[ "$UI_INSECURE_HTTP" == "1" ]]; then
    set_env_key /etc/kairon/kairon-ui.env KAIRON_UI_INSECURE_HTTP true
    sed -i -E '/^(KAIRON_UI_TLS_CERT|KAIRON_UI_TLS_KEY)=/d' /etc/kairon/kairon-ui.env
    warn "dashboard served over plain HTTP (--ui-insecure-http): logins and tokens are clear text on the network"
  elif [[ "$UI_TLS" == "1" ]]; then
    if [[ "$UI_TLS_REUSE" != "1" ]]; then
      install -d -m 0750 -o root -g kairon /etc/kairon/ui-tls
      install -m 0644 -o root -g kairon ./ui-tls-cert.pem /etc/kairon/ui-tls/cert.pem
      install -m 0640 -o root -g kairon ./ui-tls-key.pem /etc/kairon/ui-tls/key.pem
    fi
    sed -i -E '/^KAIRON_UI_INSECURE_HTTP=/d' /etc/kairon/kairon-ui.env
    set_env_key /etc/kairon/kairon-ui.env KAIRON_UI_TLS_CERT /etc/kairon/ui-tls/cert.pem
    set_env_key /etc/kairon/kairon-ui.env KAIRON_UI_TLS_KEY /etc/kairon/ui-tls/key.pem
    if [[ "$UI_TLS_REUSE" == "1" ]]; then
      ok "dashboard TLS: existing certificate in /etc/kairon/ui-tls/ kept and wired into kairon-ui.env"
    else
      ok "dashboard TLS: certificate installed to /etc/kairon/ui-tls/ and wired into kairon-ui.env"
    fi
  fi
fi

systemctl daemon-reload

if [[ "$NO_START" != "1" ]]; then
  # `enable --now` is a no-op start if the unit is already active from a
  # prior deploy -- the process keeps running with its OLD ExecStart
  # (stale port, stale token/env), silently ignoring whatever this deploy
  # just changed. `restart` starts a stopped unit or restarts a running
  # one, so a redeploy always actually takes effect.
  if [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" ]]; then
    systemctl enable kairon-migration-adapter-stub.service
    systemctl restart kairon-migration-adapter-stub.service
    ok "enabled + started kairon-migration-adapter-stub.service"
  fi
  if [[ "$CPU_PINNING" == "1" ]]; then
    systemctl enable --now kairon-cpustate.path
    systemctl start kairon-cpustate.service || warn "kairon-cpustate.service failed -- check: journalctl -u kairon-cpustate"
    ok "enabled kairon-cpustate.path (state copy refreshed)"
  fi
  systemctl enable kairon-node.service
  systemctl restart kairon-node.service
  ok "enabled + started kairon-node.service"
  if [[ "$WITH_CONTROLLER" == "1" ]]; then
    systemctl enable kairon-controller.service
    systemctl restart kairon-controller.service
    ok "enabled + started kairon-controller.service"
  fi
  if [[ "$WITH_UI" == "1" ]]; then
    systemctl enable kairon-ui.service
    systemctl restart kairon-ui.service
    ok "enabled + started kairon-ui.service"
  fi
  sleep 2
else
  systemctl enable kairon-node.service
  [[ "$WITH_CONTROLLER" == "1" ]] && systemctl enable kairon-controller.service
  [[ "$WITH_UI" == "1" ]] && systemctl enable kairon-ui.service
  [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" ]] && systemctl enable kairon-migration-adapter-stub.service
  warn "--no-start given: service(s) installed and enabled but not started"
fi

systemctl status kairon-node.service --no-pager -l 2>&1 | tail -n 15 || true
INSTALL_EOF

  # With --remote-build the binaries and web assets are produced on the host
  # (into the staging directory), so only units and config travel from here.
  local scp_files=("$REPO_ROOT/systemd/kairon-node.service")
  if [[ "$REMOTE_BUILD" != "1" ]]; then
    scp_files+=("$build_dir/kairon-node" "$build_dir/kaironctl")
  fi
  if [[ "$WITH_CONTROLLER" == "1" ]]; then
    [[ "$REMOTE_BUILD" == "1" ]] || scp_files+=("$build_dir/kairon-controller")
    scp_files+=("$REPO_ROOT/systemd/kairon-controller.service")
  fi
  if [[ "$CPU_PINNING" == "1" ]]; then
    scp_files+=("$REPO_ROOT/systemd/kairon-cpustate.service" "$REPO_ROOT/systemd/kairon-cpustate.path")
  fi
  if [[ "$MIGRATION_CONFIGURED" == "1" ]]; then
    cp "$MIGRATION_CA" "$local_stage/migration-ca.pem"
    cp "$MIGRATION_CERT" "$local_stage/migration-cert.pem"
    cp "$MIGRATION_KEY" "$local_stage/migration-key.pem"
    scp_files+=("$local_stage/migration-ca.pem" "$local_stage/migration-cert.pem" "$local_stage/migration-key.pem")
  fi
  if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
    scp_files+=("$local_stage/console-tls-ca.pem" "$local_stage/console-tls-cert.pem" "$local_stage/console-tls-key.pem")
  fi
  if [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" ]]; then
    [[ "$REMOTE_BUILD" == "1" ]] || scp_files+=("$build_dir/kairon-migration-adapter-stub")
    scp_files+=("$REPO_ROOT/systemd/kairon-migration-adapter-stub.service")
  fi
  if [[ "$WITH_UI" == "1" ]]; then
    scp_files+=("$REPO_ROOT/systemd/kairon-ui.service")
    if [[ "$REMOTE_BUILD" != "1" ]]; then
      scp_files+=("$build_dir/kairon-ui")
      # bsdtar on macOS otherwise stores com.apple.* xattrs that GNU tar on the
      # remote host warns about once per file.
      tar_extra=()
      # Probe by trying it: bsdtar's --help does not list --no-xattrs even though it accepts it.
      if tar --no-xattrs -cf /dev/null -T /dev/null 2>/dev/null; then tar_extra+=(--no-xattrs); fi
      COPYFILE_DISABLE=1 tar "${tar_extra[@]}" -C "$REPO_ROOT/web/dist" -czf "$local_stage/kairon-ui-web.tar.gz" .
      scp_files+=("$local_stage/kairon-ui-web.tar.gz")
    fi
    if [[ "$UI_TLS" == "1" && "$UI_TLS_REUSE" != "1" ]]; then
      scp_files+=("$local_stage/ui-tls-cert.pem" "$local_stage/ui-tls-key.pem")
    fi
  fi
  if [[ "$APPLY_CRDS" == "1" ]]; then
    scp_files+=("$REPO_ROOT/deploy/crd.yaml" "$REPO_ROOT/deploy/fleet-crds.yaml")
  fi
  if [[ "$APPLY_RBAC" == "1" ]]; then
    scp_files+=("$REPO_ROOT/deploy/rbac.yaml")
  fi
  scp_files+=("$local_stage/params.env" "$local_stage/install.sh")

  local remote_tmp
  remote_tmp="$(ssh_cmd "$REMOTE" mktemp -d)"
  info "staging files in ${REMOTE}:${remote_tmp}"
  scp_cmd "${scp_files[@]}" "${REMOTE}:${remote_tmp}/"

  if [[ "$REMOTE_BUILD" == "1" ]]; then
    local build_targets="kairon-node kaironctl"
    [[ "$WITH_CONTROLLER" == "1" ]] && build_targets+=" kairon-controller"
    [[ "$WITH_MIGRATION_ADAPTER_STUB" == "1" ]] && build_targets+=" kairon-migration-adapter-stub"
    [[ "$WITH_UI" == "1" ]] && build_targets+=" kairon-ui web"
    remote_build "$local_stage" "$remote_tmp" "$build_targets"
  fi

  if [[ "$SYNC_ONLY" == "1" ]]; then
    ok "--sync-only: files copied to ${remote_tmp} on the remote host, nothing installed or started"
    tip "run manually: ssh ${REMOTE} 'cd ${remote_tmp} && sudo bash ./install.sh'"
    return 0
  fi

  info "running install on ${REMOTE}..."
  ssh_exec_privileged "$REMOTE" "cd ${remote_tmp} && ${SUDO} bash ./install.sh"
  ssh_cmd "$REMOTE" "rm -rf ${remote_tmp}" || true

  save_deploy_last

  if [[ "$NO_START" == "1" ]]; then
    ok "deploy complete (service not started, per --no-start)"
    return 0
  fi

  info "verifying kairon-node..."
  local node_port
  node_port="$(ssh_cmd "$REMOTE" "grep -oE -- '--health-addr=:[0-9]+' /etc/systemd/system/kairon-node.service | cut -d: -f2" || echo 32302)"
  local node_report node_active node_healthz node_readyz
  node_report="$(verify_remote kairon-node.service "${node_port:-32302}")"
  node_active="$(sed -n '1p' <<< "$node_report")"
  node_healthz="$(sed -n '2p' <<< "$node_report")"
  node_readyz="$(sed -n '3p' <<< "$node_report")"

  echo
  if [[ "$node_active" == "active" ]]; then
    ok "kairon-node.service is active"
  else
    err "kairon-node.service is '$node_active' (expected active)"
  fi
  [[ "$node_healthz" == "200" ]] && ok "healthz: $node_healthz" || warn "healthz: $node_healthz"
  if [[ "$node_readyz" == "200" ]]; then
    ok "readyz: $node_readyz (Kubernetes API reachable)"
  else
    warn "readyz: $node_readyz (not ready -- expected if KAIRON_KUBE_URL isn't configured yet)"
    tip "fix: ssh ${REMOTE}, edit /etc/kairon/kairon-node.env, then: sudo systemctl restart kairon-node"
  fi

  if [[ "$WITH_CONTROLLER" == "1" ]]; then
    local ctrl_port
    ctrl_port="$(ssh_cmd "$REMOTE" "grep -oE -- '--health-addr=:[0-9]+' /etc/systemd/system/kairon-controller.service | cut -d: -f2" || echo 32301)"
    local ctrl_report ctrl_active ctrl_healthz ctrl_readyz
    ctrl_report="$(verify_remote kairon-controller.service "${ctrl_port:-32301}")"
    ctrl_active="$(sed -n '1p' <<< "$ctrl_report")"
    ctrl_healthz="$(sed -n '2p' <<< "$ctrl_report")"
    ctrl_readyz="$(sed -n '3p' <<< "$ctrl_report")"
    [[ "$ctrl_active" == "active" ]] && ok "kairon-controller.service is active" || err "kairon-controller.service is '$ctrl_active'"
    [[ "$ctrl_healthz" == "200" ]] && ok "controller healthz: $ctrl_healthz" || warn "controller healthz: $ctrl_healthz"
    [[ "$ctrl_readyz" == "200" ]] && ok "controller readyz: $ctrl_readyz" || warn "controller readyz: $ctrl_readyz"
  fi

  if [[ "$WITH_UI" == "1" ]]; then
    local ui_port
    ui_port="$(ssh_cmd "$REMOTE" "grep -oE -- '--listen=:[0-9]+' /etc/systemd/system/kairon-ui.service | cut -d: -f2" || echo 18082)"
    # kairon-ui's /readyz is an unconditional 200 with no actual Kubernetes
    # reachability check (unlike node/controller), so only healthz is a
    # meaningful signal here -- verify_remote's readyz line is discarded.
    # https unless kairon-ui.env opts out of it by name.
    ui_scheme="$(ssh_exec_privileged "$REMOTE" "${SUDO} sh -c 'grep -qE \"^KAIRON_UI_INSECURE_HTTP=true\" /etc/kairon/kairon-ui.env && echo http || echo https'" 2>/dev/null | tr -d '\r' | tail -1)"
    [[ "$ui_scheme" == "http" ]] || ui_scheme="https"
    local ui_report ui_active ui_healthz
    ui_report="$(verify_remote kairon-ui.service "${ui_port:-18082}" "$ui_scheme")"
    ui_active="$(sed -n '1p' <<< "$ui_report")"
    ui_healthz="$(sed -n '2p' <<< "$ui_report")"
    [[ "$ui_active" == "active" ]] && ok "kairon-ui.service is active" || err "kairon-ui.service is '$ui_active' (expected active)"
    [[ "$ui_healthz" == "200" ]] && ok "ui healthz: $ui_healthz" || warn "ui healthz: $ui_healthz"

    echo
    ok "dashboard: ${ui_scheme}://${HOST_ARG}:${ui_port:-18082}"
    if [[ "$ui_scheme" == "http" ]]; then
      tip "the dashboard is plain HTTP: logins and tokens cross the network in clear text; add --ui-tls (or terminate TLS in front of it)"
    elif [[ "$UI_TLS" == "1" && -z "$UI_TLS_CERT" ]]; then
      tip "the certificate is self-signed, so browsers warn once; pass --ui-tls-cert/--ui-tls-key for a trusted one"
    fi
    if [[ -n "$RESOLVED_UI_SESSION_SECRET" ]]; then
      if ssh_exec_privileged "$REMOTE" "${SUDO} grep -q '^KAIRON_UI_DEFAULT_ADMIN_PASSWORD=' /etc/kairon/kairon-ui.env" >/dev/null 2>&1; then
        ok "dashboard login: username 'admin' (password from --ui-admin-password; default Admin@321 -- change it)"
      else
        tip "username/password login is not configured on this host yet; re-run with --reseed-ui-auth"
      fi
    fi
    # /etc/kairon/kairon-ui.env is root:kairon 0640 -- the SSH user is
    # neither, so this check needs sudo (ssh_exec_privileged), not a plain
    # ssh_cmd, or "permission denied" gets misread as "token not applied".
    case "$RESOLVED_UI_TOKEN_SOURCE" in
      generated)
        if ssh_exec_privileged "$REMOTE" "${SUDO} grep -q '^KAIRON_UI_TOKEN=$RESOLVED_UI_TOKEN\$' /etc/kairon/kairon-ui.env" >/dev/null 2>&1; then
          ok "dashboard token (auto-generated, shown once -- save it now): $RESOLVED_UI_TOKEN"
        else
          tip "an existing /etc/kairon/kairon-ui.env was left untouched -- the freshly generated token above was NOT applied; ssh in and 'sudo cat /etc/kairon/kairon-ui.env' to see the active one"
        fi
        ;;
      explicit)
        if ssh_exec_privileged "$REMOTE" "${SUDO} grep -q '^KAIRON_UI_TOKEN=$RESOLVED_UI_TOKEN\$' /etc/kairon/kairon-ui.env" >/dev/null 2>&1; then
          ok "dashboard token: the value passed via --ui-token"
        else
          tip "an existing /etc/kairon/kairon-ui.env was left untouched -- --ui-token was NOT applied; ssh in and 'sudo cat /etc/kairon/kairon-ui.env' to see the active one"
        fi
        ;;
      *)
        tip "dashboard token: unchanged (--ui-allow-unauthenticated, or an existing env file was left as-is) -- ssh in and 'sudo cat /etc/kairon/kairon-ui.env' to see the active config"
        ;;
    esac

    if [[ "$WITH_CONSOLE" == "1" ]]; then
      # /etc/kairon/kairon-node.env is root:kairon 0640, same as
      # kairon-ui.env above -- same sudo-required check, same reason.
      if ssh_exec_privileged "$REMOTE" "${SUDO} grep -q '^KAIRON_NODE_CONSOLE_TOKEN=$RESOLVED_CONSOLE_TOKEN\$' /etc/kairon/kairon-node.env" >/dev/null 2>&1 \
        && ssh_exec_privileged "$REMOTE" "${SUDO} grep -q '^KAIRON_NODE_CONSOLE_TOKEN=$RESOLVED_CONSOLE_TOKEN\$' /etc/kairon/kairon-ui.env" >/dev/null 2>&1; then
        applied_console_port="$(ssh_exec_privileged "$REMOTE" "${SUDO} sed -n 's/^KAIRON_NODE_CONSOLE_PORT=//p' /etc/kairon/kairon-ui.env" 2>/dev/null | tr -d '\r')"
        ok "VNC console: enabled (port ${applied_console_port:-$CONSOLE_PORT}, shared token applied to both kairon-node.env and kairon-ui.env)"
        tip "read SECURITY.md's \"VNC console\" section before relying on this on a shared/untrusted network -- FluxVM's own VNC socket has no auth of its own"
      else
        tip "--with-console was given but at least one of kairon-node.env/kairon-ui.env already existed -- the auto-generated token was NOT applied to both; ssh in and compare 'sudo cat /etc/kairon/kairon-node.env /etc/kairon/kairon-ui.env'"
      fi
    fi

    if [[ "$WITH_CONSOLE_TLS" == "1" ]]; then
      if ssh_exec_privileged "$REMOTE" "${SUDO} test -f /etc/kairon/console-tls/cert.pem" >/dev/null 2>&1; then
        ok "console relay TLS: enabled ($RESOLVED_CONSOLE_TLS_SOURCE certificate installed to /etc/kairon/console-tls/)"
        if [[ "$RESOLVED_CONSOLE_TLS_SOURCE" == "generated" ]]; then
          tip "kairon-ui verifies this certificate against the fixed name kairon-node (KAIRON_NODE_CONSOLE_SERVER_NAME), whatever address it dials; a certificate you supply with --console-tls-cert/-key/-ca is verified against the dialled address instead, so its SANs must cover it"
        fi
      else
        tip "--console-tls was given but /etc/kairon/console-tls/cert.pem is missing on the remote host -- ssh in and check 'sudo journalctl -u kairon-node -n 50'"
      fi
    fi
  fi

  echo
  ok "deploy to ${REMOTE} complete"
  tip "ssh ${REMOTE}"
  tip "ssh ${REMOTE} 'systemctl status kairon-node --no-pager -l'"
  tip "ssh ${REMOTE} 'journalctl -u kairon-node -n 30 --no-pager'"
  tip "ssh ${REMOTE} 'sudo cat /etc/kairon/kairon-node.env'"
  tip "use kaironctl on that host: export KAIRON_KUBE_URL=... ; kaironctl get"
  if [[ "$SELF_TEST" == "1" ]]; then
    if [[ "$NO_START" == "1" ]]; then
      warn "--self-test skipped: --no-start left the services stopped"
    else
      echo
      run_selftest_remote "$SELF_TEST_PROFILE" "${ui_port:-}" "${ui_scheme:-http}" || { err "self-test failed (see the log above)"; return 1; }
    fi
  else
    tip "check it all works: ssh -t ${REMOTE} 'sudo kaironctl selftest --profile quick'  (or re-run with --self-test)"
  fi
}

# run_selftest_remote runs 'kaironctl selftest' on the host through a
# pseudo-terminal so its colours survive. It loads the node's env file for
# the Kubernetes connection (a k3s kubeconfig is the fallback), and, when
# kairon-ui was deployed, points the self-test at it for serial-console and
# usage checks.
run_selftest_remote() {
  local profile="$1" ui_port="${2:-}" ui_scheme="${3:-http}" script
  # The self-test creates and deletes Machines, which the kairon-node
  # service account may not do, so prefer an admin kubeconfig (k3s) and fall
  # back to the connection kairon-node itself uses.
  script='kc=;'
  script+=' if [ -r /etc/rancher/k3s/k3s.yaml ]; then kc="--kubeconfig /etc/rancher/k3s/k3s.yaml";'
  script+=' elif [ -r /etc/kairon/kairon-node.env ]; then set -a; . /etc/kairon/kairon-node.env; set +a; fi;'
  if [[ -n "$ui_port" ]]; then
    script+=" export KAIRON_UI_URL=${ui_scheme}://127.0.0.1:${ui_port};"
    if [[ "$ui_scheme" == "https" ]]; then
      script+=" export KAIRON_UI_CA=/etc/kairon/ui-tls/cert.pem;"
    fi
    script+=' t=$(sed -n "s/^KAIRON_UI_TOKEN=//p" /etc/kairon/kairon-ui.env 2>/dev/null | head -1); [ -n "$t" ] && export KAIRON_UI_TOKEN="$t";'
  fi
  script+=" exec kaironctl \$kc selftest --profile ${profile}"
  info "running kaironctl selftest --profile ${profile} on ${REMOTE}"
  local rc=0
  if [[ -n "${SSHPASS:-}" ]] && command -v sshpass >/dev/null 2>&1; then
    sshpass -e ssh -tt "${SSH_OPTS[@]}" "$REMOTE" "${SUDO} sh -c '${script}'" || rc=$?
  else
    ssh -tt "${SSH_OPTS[@]}" "$REMOTE" "${SUDO} sh -c '${script}'" || rc=$?
  fi
  return "$rc"
}

case "$MODE" in
  status)
    run_status
    ;;
  deploy)
    if [[ "$UNINSTALL" == "1" ]]; then
      run_uninstall
    else
      run_deploy
    fi
    ;;
esac
