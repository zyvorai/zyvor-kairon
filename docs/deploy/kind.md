---
sidebar_position: 3
title: Laptop with kind
---

# Evaluate on a laptop with kind

A kind cluster has no KVM and no FluxVM, so **no VM boots there**. It is useful for learning the
objects and the CLI, and for checking that the chart applies and the control plane reports healthy.
This is what the repository's `kind-smoke` workflow does.

```bash
kind create cluster --name kairon
kubectl apply --server-side -f deploy/crd.yaml
helm install kairon charts/kairon -n kairon-system --create-namespace --skip-crds

kubectl label node --all kairon.zyvor.dev/capable=true
kaironctl selftest --no-vm --profile quick      # control-plane checks only; creates no Machine
```

To boot real VMs, use [a KVM host](bare-metal.md) or [Helm on a cluster with FluxVM nodes](helm.md).
