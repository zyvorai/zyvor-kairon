---
sidebar_position: 4
title: Apple silicon Mac
---

# Use a Mac as a node

`kairon-node` registers an Apple silicon Mac as a Kubernetes Node and runs `backend: vz` Machines
through FluxVM.

```bash
# prerequisites: a Kubernetes API, FluxVM with the vz backend, Go, Rust, Xcode command line tools
KUBECONFIG=/path/to/kubeconfig scripts/macos-e2e.sh /path/to/arm64-debian.raw
```

Verified on an Apple M4 against a real k3s cluster: the Mac registers as a Ready Node, a `vz` Machine
is scheduled to it, boots, reports `status.guestIP`, accepts SSH and is removed cleanly.
**Not verified:** macOS guests, live migration, CSI, the VNC console relay, kairon-ui, multi-Mac
clusters and Intel Macs.

The full walk-through, defaults and honest limits are in [Kairon on macOS](../macos.md) and
[Mac clusters](../macos-cluster.md).
