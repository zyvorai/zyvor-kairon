#!/usr/bin/env bash
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

# Generates a self-signed serving certificate for kairon-controller's
# validating admission webhook (webhook.enabled, see docs/guides/
# machine-quotas.md's "Admission webhook" section and SECURITY.md), the one
# other TLS-gated feature in this chart (alongside migration.tlsSecretName/
# console.tls.secretName) that ships with no chart-minted certificate --
# unlike those, though, there was previously no generator script for this
# one at all, so an operator had to hand-roll their own cert to ever turn
# webhook.enabled on. Mirrors gen-migration-mtls-certs.sh's own structure
# and self-signed-CA approach.
#
# This is evaluation/self-service tooling, same disclaimer as the migration
# script: a throwaway CA with a short default validity, meant for getting a
# real webhook running quickly to try it out or in a lab -- a production
# deployment should issue this certificate from cert-manager (or your own
# real CA) instead, with proper rotation, not a cert this script prints
# once and forgets about.
#
# Unlike migration.tlsSecretName's peers (kairon-node instances addressed
# by Kubernetes node InternalIP, verified against a fixed DNS SAN --
# "kairon-node" by default), the admission webhook is called by the
# kube-apiserver itself, over a real Kubernetes Service DNS name -- this
# chart does not create that Service or the ValidatingWebhookConfiguration
# that references it (see webhook.tlsSecretName's own doc comment in
# values.yaml: "this chart does not mint a serving certificate for you").
# You still need to create both yourself, with the Service's DNS name
# matching SERVICE_DNS_NAME below exactly (kube-apiserver does real
# hostname verification against whatever `spec.clientConfig.service` names
# in your ValidatingWebhookConfiguration).
#
# Usage:
#   gen-webhook-tls-certs.sh OUT_DIR [SERVICE_NAME] [NAMESPACE]
#
# Example (defaults match this chart's own release-name-free resource
# names, i.e. a Service you'd create as `kairon-controller-webhook` in
# kairon-system):
#   gen-webhook-tls-certs.sh ./webhook-certs
#   gen-webhook-tls-certs.sh ./webhook-certs kairon-controller-webhook kairon-system
#
# Produces, under OUT_DIR:
#   ca.pem  cert.pem  key.pem
#     -- the raw PEM files, if you'd rather build your own Secret manually
#   kairon-webhook-tls.secret.yaml
#     -- a ready-to-`kubectl apply -f` Secret (tls.crt/tls.key), in the
#        exact shape charts/kairon expects for webhook.tlsSecretName
#   ca-bundle.b64
#     -- the base64 value to paste into webhook.caBundle
#
# Requires: openssl (any version with -addext support, 1.1.1+), base64.
set -euo pipefail

OUT_DIR="${1:-./webhook-certs}"
SERVICE_NAME="${2:-kairon-controller-webhook}"
NAMESPACE="${3:-kairon-system}"
DAYS="${CERT_DAYS:-30}"
SECRET_NAME="${KAIRON_WEBHOOK_SECRET_NAME:-kairon-webhook-tls}"

# A webhook Service is reachable under three DNS forms inside the cluster
# (short name, name.namespace, and the fully-qualified name.namespace.svc)
# -- kube-apiserver's own webhook client may use any of the three depending
# on how spec.clientConfig.service is set, so all three go in the SAN
# rather than guessing which one your ValidatingWebhookConfiguration uses.
SAN_SHORT="$SERVICE_NAME"
SAN_NS="$SERVICE_NAME.$NAMESPACE"
SAN_FQDN="$SERVICE_NAME.$NAMESPACE.svc"

mkdir -p "$OUT_DIR"

echo "[*] generating root CA (valid ${DAYS}d -- this is a throwaway/self-service CA, not for production use)"
openssl req -x509 -sha256 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes \
  -keyout "$OUT_DIR/ca-key.pem" -out "$OUT_DIR/ca.pem" \
  -days "$DAYS" -subj "/CN=kairon-webhook-test-ca" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -addext "subjectKeyIdentifier=hash" 2>/dev/null

echo "[*] generating webhook serving cert (SAN: DNS:$SAN_SHORT, DNS:$SAN_NS, DNS:$SAN_FQDN, serverAuth only -- kube-apiserver is the client here, never the other way around)"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -pkeyopt ec_param_enc:named_curve -nodes \
  -keyout "$OUT_DIR/key.pem" -out "$OUT_DIR/csr.pem" \
  -subj "/CN=$SAN_FQDN" 2>/dev/null
openssl x509 -req -sha256 -in "$OUT_DIR/csr.pem" \
  -CA "$OUT_DIR/ca.pem" -CAkey "$OUT_DIR/ca-key.pem" -CAcreateserial \
  -out "$OUT_DIR/cert.pem" -days "$DAYS" \
  -extfile <(printf 'subjectAltName=DNS:%s,DNS:%s,DNS:%s\nbasicConstraints=CA:FALSE\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$SAN_SHORT" "$SAN_NS" "$SAN_FQDN") \
  2>/dev/null
rm -f "$OUT_DIR/csr.pem" "$OUT_DIR/ca.srl"
echo "    -> $OUT_DIR/{ca,cert,key}.pem"

# base64 -w0/-b0 isn't portable across GNU/BSD base64 -- pipe through
# `tr -d '\n'` instead, same portability fix gen-migration-mtls-certs.sh
# already uses for the identical reason.
CERT_B64="$(base64 <"$OUT_DIR/cert.pem" | tr -d '\n')"
KEY_B64="$(base64 <"$OUT_DIR/key.pem" | tr -d '\n')"
CA_B64="$(base64 <"$OUT_DIR/ca.pem" | tr -d '\n')"

SECRET_MANIFEST="$OUT_DIR/kairon-webhook-tls.secret.yaml"
cat >"$SECRET_MANIFEST" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: $SECRET_NAME
  namespace: $NAMESPACE
type: kubernetes.io/tls
data:
  tls.crt: $CERT_B64
  tls.key: $KEY_B64
EOF
echo "[+] wrote $SECRET_MANIFEST"

printf '%s' "$CA_B64" >"$OUT_DIR/ca-bundle.b64"
echo "[+] wrote $OUT_DIR/ca-bundle.b64"

echo "[+] done. Next steps:"
echo "    1. kubectl apply -f $SECRET_MANIFEST"
echo "    2. Set in Helm: webhook.enabled=true, webhook.tlsSecretName=$SECRET_NAME,"
echo "       webhook.caBundle=\$(cat $OUT_DIR/ca-bundle.b64)"
echo "    3. Create a Service named '$SERVICE_NAME' in namespace '$NAMESPACE' selecting"
echo "       kairon-controller's pods on the webhook port (webhook.port, 8443 by default),"
echo "       and a ValidatingWebhookConfiguration whose clientConfig.service names it --"
echo "       neither is chart-managed (see this script's own header comment for why)."
echo "    4. A production deployment should replace this self-signed CA with cert-manager"
echo "       (or your own real CA) and real rotation -- this script is evaluation/"
echo "       self-service tooling, same posture as gen-migration-mtls-certs.sh."
