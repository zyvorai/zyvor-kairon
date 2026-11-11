---
sidebar_position: 5
title: Air-gapped
---

# Deploy without internet access

Kairon needs three things from the outside world: container images, a test image for the self-test,
and (with `--remote-build`) a Go toolchain. In an air-gapped site, host the first two yourself and
build elsewhere.

## Container images

Mirror the images you enable, **keeping the tag**: release images are tagged with a `v` prefix (`v0.10.3`)
and the chart's default tag is `v` plus its `appVersion`.

| Image | Needed when |
|---|---|
| `ghcr.io/zyvorai/kairon-controller`, `kairon-node` | always |
| `ghcr.io/zyvorai/kairon-ui` | `ui.enabled` |
| `ghcr.io/zyvorai/kairon-csi-node`, `kairon-csi-controller` | `csiNode.enabled`, `csiController.enabled` |

```bash
helm upgrade --install kairon charts/kairon -n kairon-system \
  --set controller.image.repository=registry.internal/kairon-controller \
  --set node.image.repository=registry.internal/kairon-node \
  --set ui.image.repository=registry.internal/kairon-ui
```

The chart itself can be mirrored as an OCI artifact (`oci://ghcr.io/zyvorai/charts/kairon`, version `0.10.3`), or use the copy
embedded in `kaironctl` (`kaironctl install`, no download). Releases also attach `kairon-0.10.3.tgz` and
`kairon-manifests-v0.10.3.tgz` (raw manifests with the images pinned to the tag) for offline use. Verify downloads
against the release's `SHA256SUMS`. Image signature verification (cosign) needs network access to the signing
infrastructure unless you mirror that too; this has not been tested offline.

## Test image for the self-test

```bash
# pinned by digest, from your own server
kaironctl selftest --image-url https://mirror.internal/cirros.qcow2 --image-digest sha256:<digest>

# or a file already on the node
kaironctl selftest --image /var/lib/fluxvm/images/cirros.qcow2
```

## Image cache

`deploy-remote.sh --image-cache-dir=/var/lib/kairon-node/image-cache` sets the directory kairon-node
downloads `spec.image.source` images into. With Helm, set `node.imageCacheDir` to a host directory owned by UID 65532 (`install -d -o 65532 -g 65532 DIR`): `kairon-node` runs non-root and Kubernetes `fsGroup` does not apply to hostPath volumes.

## Building

`--remote-build` downloads a Go toolchain on the host. Without internet, omit it: the default mode
cross-compiles on a connected machine and ships static binaries.
