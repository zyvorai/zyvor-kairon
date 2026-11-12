---
sidebar_position: 1
title: Helm (Kubernetes)
---

# Deploy with Helm

The chart and images are published to GitHub Container Registry on every release.
`kaironctl` embeds the same chart, so you do not need a `helm` binary either.

```bash
kubectl label node worker-1 kairon.zyvor.dev/capable=true

# evaluation profile (the chart defaults)
helm upgrade --install kairon oci://ghcr.io/zyvorai/charts/kairon \
  --version 0.10.4 -n kairon-system --create-namespace
```

For production add the overlay **and** the webhook values, after creating the Secrets the profile needs
(see [Evaluation vs production](../getting-started.md#evaluation-vs-production)); the overlay alone stops with a message:

```bash
helm upgrade --install kairon oci://ghcr.io/zyvorai/charts/kairon \
  --version 0.10.4 -n kairon-system --create-namespace \
  -f https://raw.githubusercontent.com/zyvorai/zyvor-kairon/v0.10.4/charts/kairon/values-production.yaml \
  --set webhook.tlsSecretName=kairon-webhook-tls --set webhook.caBundle="$(cat ./webhook-certs/ca-bundle.b64)"
```

Without Helm: `kaironctl install --profile production --set webhook.tlsSecretName=... --set webhook.caBundle=...`.

## Check it

```bash
kaironctl doctor
kaironctl selftest --profile quick   # boots a tiny CirrOS VM, see ../guides/selftest.md
```

## Notes

- Without the overlay you get the evaluation profile (webhook and namespace isolation off).
- The production profile also needs the `kairon-migration-tls` Secret (`kairon-node` pods stay in `ContainerCreating`
  without it) and, per release, `webhook.caBundle` and the webhook certificate must be kept valid (an expired one blocks
  every Machine write under `failurePolicy: Fail`).
- Helm applies `crds/` only on first install. Apply the CRDs from the same tag before upgrading
  (`kubectl apply --server-side -f https://raw.githubusercontent.com/zyvorai/zyvor-kairon/v0.10.4/deploy/crd.yaml`, or let
  `kaironctl upgrade` do it) and keep your values (`--reuse-values`); see
  [CRD versioning](../guides/crd-versioning.md#upgrading-an-existing-cluster) and
  [Upgrade, rollback and node drains](../getting-started.md#upgrade-rollback-and-node-drains).
- Dashboard login: the dashboard is off by default (`ui.enabled`); when enabled with the evaluation defaults, a fresh install seeds `admin` / `Admin@321` (documented lab default,
  `ui.auth.wellKnownDefault`); the production profile generates a random password instead. See
  [the dashboard guide](../guides/kairon-ui-dashboard.md#signing-in).
- The dashboard serves HTTPS by default. `ui.tls.secretName` brings your own certificate;
  `ui.insecureHttp: true` is the explicit opt-out for use behind a TLS-terminating proxy.
- The VNC console relay is TLS by default (`console.tls.enabled`).

Everything else (prerequisites, production profile, upgrade and rollback) is in
[Getting started](../getting-started.md) and the [CLI reference](../CLI.md).
