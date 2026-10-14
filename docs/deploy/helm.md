---
sidebar_position: 1
title: Helm (Kubernetes)
---

# Deploy with Helm

The chart and images are published to GitHub Container Registry on every release.
`kaironctl` embeds the same chart, so you do not need a `helm` binary either.

```bash
kubectl label node worker-1 kairon.zyvor.dev/capable=true

helm upgrade --install kairon oci://ghcr.io/zyvorai/charts/kairon \
  --version 0.10.0 -n kairon-system --create-namespace \
  -f https://raw.githubusercontent.com/zyvorai/kairon/v0.10.0/charts/kairon/values-production.yaml
```

Without Helm: `kaironctl install --profile production`.

## Check it

```bash
kaironctl doctor
kaironctl selftest --profile quick   # boots a tiny CirrOS VM, see ../guides/selftest.md
```

## Notes

- Drop the `-f` overlay for the evaluation profile (webhook and namespace isolation off).
- Helm applies `crds/` only on first install. Apply the CRDs from the same tag before upgrading;
  see [CRD versioning](../guides/crd-versioning.md#upgrading-an-existing-cluster).
- The dashboard serves HTTPS by default. `ui.tls.secretName` brings your own certificate;
  `ui.insecureHttp: true` is the explicit opt-out for use behind a TLS-terminating proxy.
- The VNC console relay is TLS by default (`console.tls.enabled`).

Everything else (prerequisites, production profile, upgrade and rollback) is in
[Getting started](../getting-started.md) and the [CLI reference](../CLI.md).
