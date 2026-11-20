---
sidebar_position: 70
title: Developer ecosystem
---

# Developer ecosystem

Kairon's developer kit provides dependency-free Python and TypeScript clients,
reviewed deployment recipes, disposable VM CI, warm-pool/fleet composition,
Terraform manifests, orphan cleanup and integration qualification tooling.

Start with the [ecosystem guide](https://github.com/zyvorai/zyvor-kairon/blob/main/ecosystem/README.md),
[Python SDK](https://github.com/zyvorai/zyvor-kairon/blob/main/sdk/python/README.md) or
[TypeScript SDK](https://github.com/zyvorai/zyvor-kairon/blob/main/sdk/typescript/README.md).

Lifecycle operations use the caller's Kubernetes RBAC. Guest operations reuse
the existing UI admin, namespace and Machine-access checks. SDKs do not change
controller/runtime behavior or enable experimental fleet features. Catalog
recipes require an operator-supplied pinned image and are not certified images.

Qualification reports cover a single Machine's lifecycle; use the existing
[hardware compatibility matrix](../COMPATIBILITY.md) for migration evidence.
