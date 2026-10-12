---
sidebar_position: 5
title: Air-gapped
---

# Deploy without internet access

Kairon needs three things from the outside world: container images, a test image for the self-test,
and (with `--remote-build`) a Go toolchain. In an air-gapped site, host the first two yourself and
build elsewhere.

## Container images

Mirror `ghcr.io/zyvorai/kairon-{controller,node,ui}` to your registry and point the chart at them:

```bash
helm upgrade --install kairon charts/kairon -n kairon-system \
  --set controller.image.repository=registry.internal/kairon-controller \
  --set node.image.repository=registry.internal/kairon-node
```

## Test image for the self-test

```bash
# pinned by digest, from your own server
kaironctl selftest --image-url https://mirror.internal/cirros.qcow2 --image-digest sha256:<digest>

# or a file already on the node
kaironctl selftest --image /var/lib/fluxvm/images/cirros.qcow2
```

## Image cache

`deploy-remote.sh --image-cache-dir=/var/lib/kairon-node/image-cache` sets the directory kairon-node
downloads `spec.image.source` images into.

## Building

`--remote-build` downloads a Go toolchain on the host. Without internet, omit it: the default mode
cross-compiles on a connected machine and ships static binaries.
