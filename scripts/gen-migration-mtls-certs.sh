#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Generates the mTLS material for a real two-host live-migration test (see
# docs/runbook-multi-host-migration-test.md). Two DISTINCT cert families,
# because they're verified differently:
#
#   1. Control-plane cert (internal/migration/tls.go): ONE shared
#      cert+key used by kairon-node on every host. Verified via a fixed
#      DNS SAN ("kairon-node" by default, matching --migration-server-name /
#      values.yaml's migration.tlsServerName) -- not the host's real
#      identity -- because kairon-node peers are addressed by Kubernetes
#      node InternalIP, and the control plane deliberately checks "is this
#      a Kairon node" rather than "is this host X.Y.Z.W". ClientAuth +
#      ServerAuth, since each peer is simultaneously a TLS client and
#      server to the other (mutual peer connections).
#
#   2. Data-plane certs (cmd/kairon-migration-adapter-fluxvm, the QEMU
#      tls-creds-x509 object): one cert PER HOST, SAN = that host's real
#      advertised hostname/IP. Unlike the control plane, the source
#      adapter passes the receiver's real advertise-host as the expected
#      TLS hostname (see migrationTLSSpec's hostname param) -- QEMU
#      itself does real hostname verification against the peer it
#      actually dials. A shared/wildcard SAN here would silently defeat
#      that check; this script deliberately does not offer one.
#
# Usage:
#   gen-migration-mtls-certs.sh OUT_DIR HOST_A_NAME=HOST_A_IP HOST_B_NAME=HOST_B_IP [...]
#
# Example:
#   gen-migration-mtls-certs.sh ./certs vm-host-1=10.0.1.11 vm-host-2=10.0.1.12
#
# Produces, under OUT_DIR:
#   control-plane/ca.pem  control-plane/cert.pem  control-plane/key.pem
#     -- copy to every host's /etc/kairon/migration/{ca,cert,key}.pem
#   data-plane/<HOST_A_NAME>-ca.pem  data-plane/<HOST_A_NAME>-cert.pem  data-plane/<HOST_A_NAME>-key.pem
#     -- copy to that host's /etc/kairon/migration/adapter-{ca,cert,key}.pem
#   data-plane/kairon-migration-dataplane-tls.secret.yaml
#     -- a ready-to-`kubectl apply -f` Secret bundling every host's
#        data-plane cert (keys "<name>-ca.crt"/"<name>-tls.crt"/"<name>-tls.key"),
#        in the exact shape charts/kairon expects for
#        migration.dataplaneTlsSecretName -- each kairon-node pod picks out
#        only its own node's three keys at runtime (see
#        charts/kairon/templates/all.yaml's migration-adapter-tls init
#        container), so this one Secret is all every node needs, not one
#        Secret per node. Only useful when HOST_NAME above is the real
#        Kubernetes node name (kubectl get nodes), not an arbitrary test
#        hostname -- the init container matches on spec.nodeName exactly.
#   (same CA reused across both families' PEM bundles is intentional --
#   one root simplifies distribution; ONLY the leaf SANs differ.)
#
# Requires: openssl (any version with -addext support, 1.1.1+), base64.
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 OUT_DIR HOST_NAME=HOST_IP [HOST_NAME=HOST_IP ...]" >&2
  echo "example: $0 ./certs vm-host-1=10.0.1.11 vm-host-2=10.0.1.12" >&2
  exit 1
fi

OUT_DIR="$1"; shift
CONTROL_PLANE_SERVER_NAME="${KAIRON_MIGRATION_SERVER_NAME:-kairon-node}"
# Leaves are short-lived and rotated (scripts/rotate-migration-certs.sh); the CA is
# long-lived so a rotation never changes what the hosts trust. CERT_DAYS /
# CA_DAYS override. EXISTING_CA_DIR=<dir with ca.pem + ca-key.pem> reuses a CA
# and issues new leaf certificates only.
DAYS="${CERT_DAYS:-90}"
CA_DAYS="${CA_DAYS:-3650}"
EXISTING_CA_DIR="${EXISTING_CA_DIR:-}"

mkdir -p "$OUT_DIR/ca" "$OUT_DIR/control-plane" "$OUT_DIR/data-plane"

SECRET_NAME="${KAIRON_DATAPLANE_SECRET_NAME:-kairon-migration-dataplane-tls}"
SECRET_NAMESPACE="${KAIRON_DATAPLANE_SECRET_NAMESPACE:-kairon-system}"
# Accumulated into the Secret's data: map as the per-host loop below runs --
# base64 with -w0/-b0 isn't portable across GNU/BSD base64, so pipe through
# `tr -d '\n'` instead, which strips whatever line-wrapping either variant
# defaults to.
SECRET_DATA_FILE="$(mktemp)"
trap 'rm -f "$SECRET_DATA_FILE"' EXIT

if [[ -n "$EXISTING_CA_DIR" ]]; then
  [[ -f "$EXISTING_CA_DIR/ca.pem" && -f "$EXISTING_CA_DIR/ca-key.pem" ]] || {
    echo "EXISTING_CA_DIR=$EXISTING_CA_DIR must contain ca.pem and ca-key.pem" >&2; exit 1; }
  echo "[*] reusing the CA in $EXISTING_CA_DIR (issuing leaf certificates only, valid ${DAYS}d)"
  cp "$EXISTING_CA_DIR/ca.pem" "$OUT_DIR/ca/ca.pem"
  cp "$EXISTING_CA_DIR/ca-key.pem" "$OUT_DIR/ca/ca-key.pem"
  chmod 0600 "$OUT_DIR/ca/ca-key.pem"
else
  echo "[*] generating root CA (valid ${CA_DAYS}d; keep ca/ca-key.pem offline -- it is only needed to issue or rotate leaves)"
  openssl req -x509 -sha256 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes \
    -keyout "$OUT_DIR/ca/ca-key.pem" -out "$OUT_DIR/ca/ca.pem" \
    -days "$CA_DAYS" -subj "/CN=kairon-migration-ca" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -addext "subjectKeyIdentifier=hash" 2>/dev/null
  chmod 0600 "$OUT_DIR/ca/ca-key.pem"
fi

echo "[*] generating control-plane cert (SAN: DNS:$CONTROL_PLANE_SERVER_NAME, clientAuth+serverAuth)"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes \
  -keyout "$OUT_DIR/control-plane/key.pem" -out "$OUT_DIR/control-plane/csr.pem" \
  -subj "/CN=$CONTROL_PLANE_SERVER_NAME" 2>/dev/null
openssl x509 -req -sha256 -in "$OUT_DIR/control-plane/csr.pem" \
  -CA "$OUT_DIR/ca/ca.pem" -CAkey "$OUT_DIR/ca/ca-key.pem" -CAcreateserial \
  -out "$OUT_DIR/control-plane/cert.pem" -days "$DAYS" \
  -extfile <(printf 'subjectAltName=DNS:%s\nbasicConstraints=CA:FALSE\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth,serverAuth\n' "$CONTROL_PLANE_SERVER_NAME") \
  2>/dev/null
cp "$OUT_DIR/ca/ca.pem" "$OUT_DIR/control-plane/ca.pem"
rm -f "$OUT_DIR/control-plane/csr.pem"
echo "    -> $OUT_DIR/control-plane/{ca,cert,key}.pem"

for pair in "$@"; do
  name="${pair%%=*}"
  ip="${pair#*=}"
  if [[ -z "$name" || -z "$ip" || "$name" == "$pair" ]]; then
    echo "skipping malformed HOST_NAME=HOST_IP entry: $pair" >&2
    continue
  fi
  echo "[*] generating data-plane cert for $name (SAN: DNS:$name, IP:$ip)"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes \
    -keyout "$OUT_DIR/data-plane/$name-key.pem" -out "$OUT_DIR/data-plane/$name-csr.pem" \
    -subj "/CN=$name" 2>/dev/null
  openssl x509 -req -sha256 -in "$OUT_DIR/data-plane/$name-csr.pem" \
    -CA "$OUT_DIR/ca/ca.pem" -CAkey "$OUT_DIR/ca/ca-key.pem" -CAcreateserial \
    -out "$OUT_DIR/data-plane/$name-cert.pem" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=DNS:%s,IP:%s\nbasicConstraints=CA:FALSE\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth,serverAuth\n' "$name" "$ip") \
    2>/dev/null
  cp "$OUT_DIR/ca/ca.pem" "$OUT_DIR/data-plane/$name-ca.pem"
  rm -f "$OUT_DIR/data-plane/$name-csr.pem"
  echo "    -> $OUT_DIR/data-plane/$name-{ca,cert,key}.pem"

  {
    printf '  %s-ca.crt: %s\n' "$name" "$(base64 <"$OUT_DIR/data-plane/$name-ca.pem" | tr -d '\n')"
    printf '  %s-tls.crt: %s\n' "$name" "$(base64 <"$OUT_DIR/data-plane/$name-cert.pem" | tr -d '\n')"
    printf '  %s-tls.key: %s\n' "$name" "$(base64 <"$OUT_DIR/data-plane/$name-key.pem" | tr -d '\n')"
  } >>"$SECRET_DATA_FILE"
done

if [[ -s "$SECRET_DATA_FILE" ]]; then
  SECRET_MANIFEST="$OUT_DIR/data-plane/kairon-migration-dataplane-tls.secret.yaml"
  {
    echo "apiVersion: v1"
    echo "kind: Secret"
    echo "metadata:"
    echo "  name: $SECRET_NAME"
    echo "  namespace: $SECRET_NAMESPACE"
    echo "type: Opaque"
    echo "data:"
    cat "$SECRET_DATA_FILE"
  } >"$SECRET_MANIFEST"
  echo "[+] wrote $SECRET_MANIFEST -- kubectl apply -f it, then set"
  echo "    migration.dataplaneTlsSecretName=$SECRET_NAME in Helm (KAIRON_DATAPLANE_SECRET_NAME/"
  echo "    KAIRON_DATAPLANE_SECRET_NAMESPACE env vars override the name/namespace above)."
fi

echo "[+] done. Distribute control-plane/ to every host; distribute each host's own data-plane/<name>-* files to that host only."
echo "    A deliberate negative test (wrong SAN): re-run for a THIRD host name/IP the adapter never advertises, install"
echo "    those data-plane certs on one host instead of its own, and confirm the peer migration is refused for a"
echo "    hostname-verification failure -- see docs/runbook-multi-host-migration-test.md's verification checklist."
