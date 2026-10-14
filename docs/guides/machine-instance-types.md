# User guide: MachineInstanceType

A reusable, named CPU/memory shape a Machine references instead of
inlining `spec.resources` itself -- Kairon's equivalent of an EC2 instance
type or KubeVirt's `VirtualMachineInstancetype`.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineInstanceType
metadata:
  name: standard-2x4
  namespace: prod
spec:
  resources:
    cpu: "2"
    memory: 4Gi
```

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web-1
  namespace: prod
spec:
  instanceTypeName: standard-2x4
  image: {path: /var/lib/fluxvm/images/ubuntu-24.04.qcow2}
  runtime: {backend: qemu}
  powerState: Running
```

`spec.resources` is omitted entirely -- `kairon-controller` resolves
`instanceTypeName` and fills it in for you, from a `MachineInstanceType` in
the *same namespace*.

Or, via `kaironctl`:

```console
$ kaironctl create instancetype standard-2x4 --cpu 2 --memory 4Gi
instancetype/standard-2x4 created
```

`--max-cpu`/`--max-memory` (hotplug headroom), `--hugepages`, `--numa-node`,
`--cpu-set`, and `--cpu-pinning` are all also available, matching every
field `ResourceSpec` accepts -- this covers the whole spec (it's just a
`ResourceSpec`, nothing else to set), so unlike `create machineset` there's
no YAML-only escape hatch needed here.

## How resolution works

Resolution happens **once**, the first reconcile tick after a Machine sets
`spec.instanceTypeName` while `spec.resources` is still completely empty:

1. `kairon-controller` looks up the named `MachineInstanceType` in the
   Machine's own namespace.
2. If found, `spec.resources` is patched onto the Machine with that
   instance type's `spec.resources` verbatim -- from then on, the Machine
   behaves exactly like one that always had `spec.resources` set directly
   (hotplug, quota, the admission webhook, `kubectl get machines`'
   CPU/Memory columns -- nothing downstream needs to know an instance type
   was ever involved).
3. If not found (a typo, or the `MachineInstanceType` hasn't been created
   yet), `spec.resources` stays empty and resolution is retried every
   tick -- once a matching `MachineInstanceType` shows up, it resolves
   normally.

**Explicit `spec.resources` always wins.** Setting both
`spec.instanceTypeName` and `spec.resources` on the same Machine leaves
`spec.resources` exactly as you wrote it -- resolution only ever fills in
resources that are still empty, never overwrites an explicit value.

**This is creation-time-only**, the same as `spec.image`/`spec.network.forwards`
(see [`machine-storage.md`](machine-storage.md)): editing
`spec.instanceTypeName` on an already-resolved Machine has no further
effect, and editing a `MachineInstanceType` itself never retroactively
resizes any Machine that already resolved against it. To change an
existing Machine's resources, edit `spec.resources` directly (a hotplug
resize -- see [`machine-hotplug.md`](machine-hotplug.md)) the same way
you always could.

## Cluster-wide instance types

A `MachineClusterInstanceType` has the same spec but no namespace, so one
catalog serves every namespace. Point a Machine at it with
`spec.instanceTypeKind`:

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineClusterInstanceType
metadata:
  name: c2-4
spec:
  resources: {cpu: "2", memory: 4Gi}
---
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: web-1, namespace: prod}
spec:
  instanceTypeName: c2-4
  instanceTypeKind: MachineClusterInstanceType
  image: {path: /var/lib/fluxvm/images/ubuntu-24.04.qcow2}
  powerState: Running
```

`kaironctl create web-1 --image PATH --instance-type c2-4 --cluster-instance-type`
does the same. `kaironctl get clusterinstancetypes` lists them.

## Preferences

A `MachinePreference` (namespaced) or `MachineClusterPreference`
(cluster-scoped) holds the guest-OS defaults KubeVirt keeps in
`VirtualMachinePreference`. A Machine names one with
`spec.preferenceName` (and `spec.preferenceKind: MachineClusterPreference`
for the cluster-scoped kind):

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineClusterPreference
metadata:
  name: windows-11
spec:
  guestOS: windows
  backend: qemu
  architecture: amd64
  secureBoot: true
  tpm: true
  guestAgent: true
```

| Preference field | Fills on the Machine |
|---|---|
| `backend` | `spec.runtime.backend` |
| `architecture` | `spec.placement.architecture` |
| `secureBoot`, `tpm` | `spec.security.secureBoot`, `spec.security.tpm` |
| `guestAgent` | `spec.guestAgent.enabled` |
| `guestOS` | nothing; a label for `kaironctl` and the dashboard |

`kairon-controller` applies the preference once, before scheduling, and
only to fields the Machine leaves unset: an explicit value always wins.
It records what it applied in the
`kairon.zyvor.dev/preference-applied` annotation; editing the preference
later does not change Machines that already have it. Booleans can only
turn a feature on, because `false` on the Machine is the same as unset.
`kaironctl create NAME --preference windows-11 --cluster-preference`
leaves `--backend` unset unless you pass it.

## Admission checks

With `webhook.enabled`, creating a Machine is refused when it names an
instance type or preference that does not exist, or an unknown
`instanceTypeKind`/`preferenceKind`. A Machine that sets
`spec.resources` itself is not checked against its instance type, since
the instance type would never be used. Without the webhook, a missing
reference leaves the Machine unresolved and the controller retries every
tick, as before.

## Real limits today

- **Preferences cover five fields.** There is no NIC model, disk bus or
  machine-type preference, because the Machine spec has no such fields
  for the root disk or primary NIC.
- **No `kairon-ui` picker yet.** The dashboard's Machine-create form
  doesn't offer a dropdown of instance types or preferences -- set them
  via `kaironctl`/`kubectl`/the API. The "Instance types" dashboard page
  lists namespaced and cluster-wide instance types and preferences
  (`GET /api/v1/instancetypes`, `/api/v1/cluster-instancetypes`,
  `/api/v1/preferences`, `/api/v1/cluster-preferences`).
