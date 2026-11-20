# Tenant fence

`spec.tenant` stays optional. Set `kairon.zyvor.dev/tenant-fence: "true"` on a Machine when that tenant must not reach guest addresses observed on other tenants in the same namespace.

What the controller does:

- Projects `kairon.zyvor.dev/tenant` from `spec.tenant`. Admission rejects a rename or a clear.
- Owns `NetworkSecurityGroup/tenant-fence-<tenant>` in that namespace. `spec.policy.denyCidrs` is the other tenants' `status.guestIP` and `status.guestIPs` values, as `/32` or `/128`.
- Deletes that group when no non-deleting Machine in the namespace still opts in for that tenant.

What kairon-node does:

- Upserts the group to FluxVM, as it already does for every `NetworkSecurityGroup`.
- Merges the group's deny list into the `MachineNetworkPolicy` it posts. A user policy is not replaced.
- If no policy selects the Machine, posts `defaultAllow: true` plus those denies. Node-wide default-deny still wins on `defaultAllow` when it is enabled.

This is not a VRF. An address Kubernetes has not observed, or an address outside this namespace, is not denied. An address that also appears on a same-tenant Machine is not denied.

A `MachineNetworkPolicy` whose selector sets `kairon.zyvor.dev/tenant` must carry the same label. That stops a tenant-scoped object from selecting another tenant.

```yaml
metadata:
  annotations:
    kairon.zyvor.dev/tenant-fence: "true"
spec:
  tenant: acme
```

See `examples/tenant-fence.yaml`.

## Requirements

- kairon-controller and kairon-node from this release. The controller builds the group; kairon-node merges it.
- Controller RBAC with `create` and `delete` on `networksecuritygroups`. On an upgrade, re-apply `deploy/rbac.yaml` (or upgrade the Helm chart) before the new controller starts, or group creation fails with 403.
- The admission webhook (`webhook.enabled`) for the tenant-name, rename, clear and selector checks. Without it the fence still works, but nothing stops a tenant from being renamed.

## Inspect

```bash
kubectl -n NS get machines -L kairon.zyvor.dev/tenant
kubectl -n NS get networksecuritygroups -l kairon.zyvor.dev/tenant-fence=managed \
  -o custom-columns=NAME:.metadata.name,DENY:.spec.policy.denyCidrs,PHASE:.status.phase
```

`PHASE: Applied` with `status.appliedOn` set means kairon-node pushed the group to FluxVM. An empty `denyCidrs` is normal until the other tenants' Machines report a guest IP.

Verified on a live k3s host on 2026-10-06: two fenced tenants each got a group that denied only the other tenant's addresses (`/32` and `/128`), and removing the annotation from the last opted-in Machine deleted that tenant's group.
