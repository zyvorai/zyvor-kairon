---
sidebar_position: 71
title: Enterprise fleet reference
---

# Enterprise fleet reference (experimental)

Per-kind reference for the `fleet.kairon.zyvor.dev/v1alpha1` custom resources. For the feature status matrix, installation prerequisites and caveats read [Enterprise fleet automation](enterprise-fleet.md) first. Everything here is **experimental and opt-in**: nothing below runs unless `fleet.enabled` (Helm) / `--fleet-enabled` (controller) is set, and the feature set still needs cluster and hardware validation.

Sources of truth: the CRD schema in `charts/kairon/crds/fleet.yaml` (byte-identical to `deploy/fleet-crds.yaml`), the Go validators in `internal/fleet/spec.go`, and the reconcilers in `internal/fleet/`. Where the CRD schema and the controller validator disagree, both are listed; a resource must satisfy both to be accepted and to leave the `Invalid` phase.

## Conventions

- All 13 kinds are **namespaced** (none are cluster-scoped), serve only `v1alpha1`, and have a `status` subresource. `kubectl get` shows `Phase` and `Age` columns.
- `status` is declared in the CRD as a free-form object (`x-kubernetes-preserve-unknown-fields`). The status keys listed below come from the Go type `FleetStatus` in `internal/model/fleet.go` and the reconcilers; they are not enforced by the CRD.
- A single reconcile loop (`Engine.Reconcile`) walks the kinds in the order: NodeFenceRequest, MachineHAProfile, MachineBalancePolicy, MachineAutoscaler, MachineBackupGroup, MachineRecoveryPlan, MachineImportPlan, MachineVirtualNetwork, MachineNetworkClaim, MachineTemplateClaim, MachineUsageLedger. Objects are processed sorted by `namespace/name`. A spec that fails validation gets `phase: Invalid`; a reconcile error gets `phase: Blocked`, with the reason in `status.message`.
- `MachineTemplateVersion` and `MachineActionApproval` are not reconciled by that loop (see their sections).
- The validating webhook (`/validate-fleet`, `failurePolicy: Fail`) re-runs the same validator on CREATE and UPDATE and rejects spec changes on kinds marked immutable below. The CRD also carries `self == oldSelf` transition rules on the `spec` of NodeFenceRequest, MachineActionApproval, MachineImportPlan, MachineTemplateVersion, MachineTemplateClaim, MachineVirtualNetwork and MachineNetworkClaim.
- Controller-created children (Machines, backups, migrations, fence requests) carry the labels `fleet.kairon.zyvor.dev/owner` and `fleet.kairon.zyvor.dev/owner-uid`; the reconcilers refuse to adopt same-named objects they do not own.
- Names (and most name-like fields) must be DNS labels: `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, at most 63 characters. Specs are decoded with unknown fields disallowed by the controller.
- The examples below are the JSON files in `examples/fleet/` converted to YAML. They contain placeholder URLs, digests, Machine names and bridges; replace them before use.

## Kinds at a glance

| Kind | Resource (plural) | Reconciled by | Mutable spec | Admin-only via REST |
| --- | --- | --- | --- | --- |
| MachineHAProfile | `machinehaprofiles` | `highAvailability` | yes | yes (list and create) |
| NodeFenceRequest | `nodefencerequests` | `fenceNode` | no | yes (list and create) |
| MachineActionApproval | `machineactionapprovals` | none (MCP approval path) | no | creation blocked on REST |
| MachineBalancePolicy | `machinebalancepolicies` | `balance` | yes | create |
| MachineAutoscaler | `machineautoscalers` | `autoscale` | yes | create |
| MachineRecoveryPlan | `machinerecoveryplans` | `recovery` | only `start` | create |
| MachineBackupGroup | `machinebackupgroups` | `backupGroup` | yes | create |
| MachineImportPlan | `machineimportplans` | `importPlan` | no | create |
| MachineTemplateVersion | `machinetemplateversions` | none (read by claims) | no | create |
| MachineTemplateClaim | `machinetemplateclaims` | `templateClaim` | no | no (tenant) |
| MachineVirtualNetwork | `machinevirtualnetworks` | IPAM acceptance only | no | create |
| MachineNetworkClaim | `machinenetworkclaims` | `networkClaim` | no | no (tenant) |
| MachineUsageLedger | `machineusageledgers` | `meter` | yes | create |

"Mutable spec" is taken from the admission webhook: it denies spec changes on MachineTemplateVersion, MachineActionApproval, MachineVirtualNetwork, NodeFenceRequest, MachineRecoveryPlan (except `spec.start`), MachineImportPlan, MachineTemplateClaim and MachineNetworkClaim.

---

## MachineHAProfile

**Purpose.** Declares which Machines get verified high-availability restart and how to power-fence their hosts through Redfish BMCs. It is a trusted, administrator-owned object.

**Scope.** Namespaced, and the controller only acts on profiles in the **fleet control namespace** (default `kairon-system`; the Helm chart sets it to the release namespace). A profile elsewhere reports `Blocked`.

**Spec.**

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `selector` | map string to string | yes | Label selector for Machines. Controller rejects an empty selector. |
| `nodes` | map of node name to target | yes | At least one entry. Each target: `endpoint` (string, required), `systemID` (DNS label, required), `secretName` (DNS label). |
| `maxRestarts` | integer, min 1 | yes | Per-Machine restart attempts, counted in the Machine annotation `fleet.kairon.zyvor.dev/restart-count`. |
| `failureGraceSeconds` | integer, min 30 | yes | How long the Machine's NodeUnreachable condition must have been true. |

Schema versus controller: the CRD marks `endpoint`, `systemID` and `secretName` required for a node target; the controller validator additionally requires DNS-label forms for `systemID` and `secretName`. The endpoint must be a bare `https://host[:port]` origin (no userinfo, path, query or fragment) and must exactly match an entry of the administrator allowlist (`KAIRON_FLEET_REDFISH_ORIGINS` / `fleet.redfishOrigins`). The map key (node name) must be a DNS label.

**Status.** `phase` (`Ready`, `Invalid`, `Blocked`), `message`, `lastActionTime` (last time a Machine was released), `observedGeneration`.

**Controller behaviour.** A Machine is only considered if it carries the annotation `fleet.kairon.zyvor.dev/ha-profile: <profile name>` **and** matches the selector, has a node assigned, desires `Running`, and has no active migration. After its NodeUnreachable condition has lasted `failureGraceSeconds`, and while restarts remain, the controller creates an owned `NodeFenceRequest` (name derived from a hash, bound to the node UID). Once that request is `Succeeded` it re-reads the BMC, requires `Off` again, spends the shared disruption budget (`MachineDisruptionBudget`), increments the restart counter and releases the Machine through the existing fencing path (`VerifiedRedfishOff`).

**Safety gates.** Fleet enabled plus webhook TLS; control namespace only; per-Machine annotation opt-in; Redfish origin allowlist; BMC credentials read from a Secret in the control namespace; TLS verification on, redirects rejected; disruption budget check. Fenced hosts stay cordoned (the guide lists this as remaining work). Behaviour against real BMCs is not verified by the automated tests, which use test servers.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineHAProfile
metadata:
  name: ha-profile
  namespace: kairon-system
spec:
  selector:
    ha: enabled
  nodes:
    worker-1:
      endpoint: https://bmc.example.com
      systemID: system
      secretName: worker-1-bmc
  maxRestarts: 3
  failureGraceSeconds: 60
```

## NodeFenceRequest

**Purpose.** A one-shot request to power-fence a specific node through the profile's Redfish target. Normally created by the HA reconciler; an administrator may also create one.

**Scope.** Namespaced; only honoured in the fleet control namespace. Spec is immutable.

**Spec** (all required): `node` (DNS label), `nodeUID` (string; must equal the Kairon Node's UID), `profileName` (DNS label of a MachineHAProfile in the control namespace).

**Status.** `phase` (`WaitingForPowerOff`, `Succeeded`, `Blocked`, `Invalid`), `message`, `lastActionTime` (set when Off is confirmed).

**Controller behaviour.** Fails if the node disappeared or its UID changed, and refuses to fence a node whose `Ready` condition is `True`. Otherwise it cordons the node, reads the Redfish PowerState, and if not `Off` posts `ComputerSystem.Reset` with `ForceOff` and reports `WaitingForPowerOff`. `Succeeded` is set only after an observed `Off`. `Succeeded` is terminal.

**Safety gates.** As for MachineHAProfile, plus node UID binding and the Ready-node refusal. REST list and create require an administrator identity.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: NodeFenceRequest
metadata:
  name: fence-worker-1
  namespace: kairon-system
spec:
  node: worker-1
  nodeUID: 0b5d2f4e-1111-2222-3333-444455556666   # must match the Node's metadata.uid
  profileName: ha-profile
```

This example is not in `examples/fleet/`; it satisfies the CRD schema and the Go validator, but fencing only proceeds when the node is not Ready and the UID matches.

## MachineActionApproval

**Purpose.** A short-lived, immutable approval of one exact MCP action by a Kubernetes identity different from the requesting agent. Used by the MCP server in `KAIRON_MCP_APPROVAL_MODE=resource`.

**Scope.** Namespaced (the target's namespace). Spec is immutable. Not reconciled by the fleet engine and absent from the fleet controller's ClusterRole; it is consumed by the MCP approval path (`internal/kaironctl/mcpapproval_resource.go`) running under the agent's identity.

**Spec** (all required): `action` (string), `argumentsHash` (64 lowercase hex chars; SHA-256 of `action`, newline, canonical JSON arguments, newline, `principal`), `principal` (the agent's identity), `targetResource` (enum `machines`, `machinebackups`), `targetName` (DNS label), `targetUID`, `targetGeneration` (integer, min 0), `approver` (string), `expiresAt` (date-time).

**Status.** `phase` (empty until consumed, then `Consumed`), `message`, `lastActionTime`.

**Admission rules** (`validateFleet`): on CREATE, `approver` must equal the authenticated Kubernetes username and differ from `principal`; `expiresAt` must be in the future and at most 10 minutes ahead; status must be empty. Status may change once, from empty to `Consumed`, only by the bound `principal` and before expiry. `kaironctl fleet approve-action` fills `approver` from SelfSubjectReview and sets expiry to 5 minutes.

**Safety gates.** Needs the webhook (the chart's webhook rule also covers `machineactionapprovals/status`), RBAC (`kairon-action-approver` and `kairon-action-consumer`, never auto-bound), and a separate human credential. The REST API refuses to create approvals. As documented in the guide, consumption happens before the action, so a failed action needs a new approval, and an administrator credential can bypass all of this.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineActionApproval
metadata:
  name: approval-0123456789abcdef0123456789abcdef   # normally derived by `kaironctl fleet approve-action`
  namespace: default
spec:
  action: delete_machine
  argumentsHash: "0000000000000000000000000000000000000000000000000000000000000000"
  principal: system:serviceaccount:default:agent
  targetResource: machines
  targetName: dev-machine
  targetUID: 8f2d1c3a-1111-2222-3333-444455556666
  targetGeneration: 3
  approver: alice@example.com
  expiresAt: "2026-10-09T12:05:00Z"
```

The example validates against the CRD schema and the Go validator, but real approvals must be created through the CLI so the name, hash, approver and expiry are consistent; `action` is the MCP tool name (`internal/kaironctl/mcpapproval_resource.go` sets it from the tool being approved), for example `delete_machine`, `fork_machine` or `machine_backup`.

## MachineBalancePolicy

**Purpose.** Recommends or performs migrations that reduce reserved CPU/memory pressure across nodes.

**Scope.** Namespaced; acts on Machines in its own namespace.

**Spec.**

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `selector` | map string to string | yes | Controller rejects empty. |
| `minImprovementPercent` | number, 1 to 100 | yes | Minimum drop in the worst-of-two node pressure for a move to qualify. |
| `cooldownSeconds` | integer, min 30 | yes | Minimum time between actions. |
| `strategy` | enum `cold`, `auto` | yes | Passed to the created `MachineMigration`. |
| `dryRun` | boolean | no | When true only a recommendation is written. (CRD declares no default.) |

**Status.** `phase` (`Recommendation`, `Stable`, `Blocked`), `message` (names the Machine and source/target node), `lastActionTime`.

**Controller behaviour.** Skips Machines that are not Running, are not desired Running, have an active migration, have device claims, or use `cpuSet`/CPU pinning. It asks the scheduler for the best other node, computes reserved-resource pressure before and after, and requires the gain to be at least `minImprovementPercent`. Unless `dryRun`, it spends the disruption budget and creates one owned `MachineMigration` per policy per cooldown window. Pressure is based on reserved resources only; live PSI or I/O pressure and a migration cost model are not implemented.

**Safety gates.** Fleet enabled; `dryRun: true` in the example; disruption budget; cooldown.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineBalancePolicy
metadata:
  name: balancing
  namespace: default
spec:
  selector:
    app: worker
  minImprovementPercent: 10
  cooldownSeconds: 300
  strategy: cold
  dryRun: true
```

## MachineAutoscaler

**Purpose.** Adjusts `spec.replicas` of one MachineSet or MachinePool from Prometheus CPU samples.

**Scope.** Namespaced; target must be in the same namespace.

**Spec** (all required).

| Field | Type | Notes |
| --- | --- | --- |
| `targetKind` | enum `MachineSet`, `MachinePool` | |
| `targetName` | DNS label | |
| `minReplicas` | integer, min 1 | |
| `maxReplicas` | integer, 1 to 10000 | CRD rule: `maxReplicas >= minReplicas`. |
| `targetCPUPercent` | number, 0 to 100 | CRD minimum is 0, but the controller rejects values `<= 0`. |
| `stabilizationSeconds` | integer, min 30 | Recommendation must persist this long, and the same interval must pass since the last scale. |
| `maxStep` | integer, min 1 | Maximum replica change per action. |

**Status.** `phase` (`Stable`, `Stabilizing`, `Scaling`, `Blocked`), `message`, `desiredReplicas`, `recommendationSince`, `lastSampleTime`, `lastActionTime`.

**Controller behaviour.** Requires exactly one autoscaler per target (a second one reports `Blocked`). Reads `kairon_machine_resource_usage{resource="cpu_percent"}` and `kairon_machine_usage_sample_timestamp_seconds` from `KAIRON_FLEET_PROMETHEUS_URL`; samples older than 30 s (or from the future) are dropped, and any missing sample, non-Running target or replica/Machine count mismatch defers scaling. Desired replicas are `ceil(current * avgCPU / targetCPUPercent)`, clamped to min/max and `maxStep`. Scale-down additionally needs disruption-budget allowance and, for pools, no Bound claims. The scale is applied as a resourceVersion-guarded patch. The guide notes the CPU metric is the existing lifetime-average sample, not a rolling window.

**Safety gates.** Fleet enabled; `fleet.prometheusURL` configured; one scaler per target; stabilization; step bound; budget guard.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineAutoscaler
metadata:
  name: autoscaler
  namespace: default
spec:
  targetKind: MachineSet
  targetName: workers
  minReplicas: 2
  maxReplicas: 10
  targetCPUPercent: 60
  stabilizationSeconds: 120
  maxStep: 1
```

## MachineRecoveryPlan

**Purpose.** Restores a set of Machines from existing backups in dependency order, either in place or as isolated test restores.

**Scope.** Namespaced; Machines and `MachineBackup`s are looked up in the plan's namespace. Spec is immutable except `start`.

**Spec.**

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `steps` | array | yes | Each step: `machineName` (DNS label, required), `backupName` (DNS label, required), `dependsOn` (list of `machineName`s). Names must be unique; unknown dependencies and cycles are rejected. |
| `mode` | enum `RestoreInPlace`, `TestRestore` | yes | |
| `maxDataAgeSeconds` | integer, min 1 | yes | Backup completion must be no older than this. |
| `start` | boolean | yes | `false` keeps the plan `Paused`. The only field that may change after creation. |
| `testBridge` | DNS label | no | Required in practice for `TestRestore`; must be in the administrator allowlist `fleet.isolatedTestBridges`. |

**Status.** `phase` (`Paused`, `Pending`, `PreparingTest`, `Preparing`, `Halting`, `Restoring`, `Starting`, `Running`, `Succeeded`, `Blocked`), `message`, `completed` (finished machine names), `recoveryStartedAt`, `recoverySeconds`, `dataAgeSeconds` (oldest backup used).

**Controller behaviour.** With `start: false` it only reports `Paused`. With `start: true` it checks that each backup is `Succeeded`, belongs to the step's Machine and is within `maxDataAgeSeconds`; for image-disk backups it halts the target (spending the disruption budget), submits an owned `MachineBackupRestore`, and starts the Machine, waiting for `Running`. `TestRestore` creates an independent, owned Machine on the backup's node, attached to the isolated bridge, with a 3600 s TTL and without cloud-init, device claims, volumes or service fabric config. Success means all targets report Running; it is not an application health check. `TestRestore` currently supports only image-owned disk backups; Atlas PVC restores pause for operator attachment.

**Safety gates.** Created paused (`start: false`); review targets first; immutable steps; data-age check; isolated-bridge allowlist for tests; disruption budget.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineRecoveryPlan
metadata:
  name: recovery
  namespace: default
spec:
  steps:
    - machineName: database
      backupName: database-backup
    - machineName: api
      backupName: api-backup
      dependsOn:
        - database
  mode: RestoreInPlace
  maxDataAgeSeconds: 86400
  start: false
```

## MachineBackupGroup

**Purpose.** Takes (optionally recurring) backups of several Machines together and prunes old owned backups.

**Scope.** Namespaced; Machines in the same namespace.

**Spec.**

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `machines` | list of DNS labels | yes | Non-empty, unique. |
| `quiesce` | enum `auto`, `required`, `never` | yes | Passed to each `MachineBackup`. |
| `atlasBucketID` | string | no | If set, backups target that Atlas bucket. |
| `intervalSeconds` | integer, min 0 | no | `0` or unset: one batch only. |
| `keepLast` | integer, min 0 | no | `0` or unset: no pruning. |

**Status.** `phase` (`Pending`, `Running`, `Succeeded`, `Blocked`), `message`, `lastActionTime` (batch identity and start time).

**Controller behaviour.** The first pass only records the batch time (`Pending`); then it creates one owned `MachineBackup` per Machine (deterministic names, so a restart does not duplicate them), requiring each Machine to be `Running`. A failed backup blocks the group. After all succeed it deletes surplus owned succeeded backups beyond `keepLast` per Machine, and starts a new batch when `intervalSeconds` has elapsed. Quiesce is per Machine; this is not a distributed transaction.

**Safety gates.** Fleet enabled; retention only touches backups it owns.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineBackupGroup
metadata:
  name: backup-group
  namespace: default
spec:
  machines:
    - database
    - api
  quiesce: required
  intervalSeconds: 86400
  keepLast: 7
```

## MachineImportPlan

**Purpose.** Imports a batch of digest-pinned OVA images as Machines with bounded concurrency.

**Scope.** Namespaced. Spec is immutable.

**Spec.**

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `entries` | array | yes | Unique names. |
| `entries[].name` | DNS label | yes | Becomes the Machine name. |
| `entries[].image` | object | yes | `digest` (`sha256:` plus 64 hex, required) and `source` (`httpURL`, `oci`, `format`, `repair`; required). |
| `entries[].resources` | object | yes | `cpu` and `memory` (strings, required); `maxCpu`, `maxMemory` optional. |
| `entries[].backend` | enum `qemu`, `cloud-hypervisor`, `firecracker`, `fluxvm` | no | Controller defaults to `qemu`. |
| `maxConcurrent` | integer, 1 to 100 | yes | |
| `powerState` | enum `Running`, `Stopped` | yes | |

Schema versus controller: the CRD permits `oci` sources and any `format`, but the controller requires `source.httpURL` and `source.format: ova` for every entry and also runs `model.ValidateImageSource`. Entries using only `oci` will be marked `Invalid`.

**Status.** `phase` (`Running`, `Succeeded`, `Blocked`), `message` (`n/m imports reached requested power state`).

**Controller behaviour.** Creates owned Machines up to `maxConcurrent` in flight; blocks if a same-named Machine is not owned by the plan or an owned Machine reaches `Failed`. `Succeeded` when all reach the requested power state. `Stopped` entries are staged only; no vCenter inventory, cutover or rollback exists.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineImportPlan
metadata:
  name: import-campaign
  namespace: default
spec:
  entries:
    - name: imported-web
      image:
        source:
          httpURL: https://images.example.com/web.ova
          format: ova
          repair: true
        digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
      resources:
        cpu: "2"
        memory: 4Gi
      backend: qemu
  maxConcurrent: 1
  powerState: Running
```

## MachineTemplateVersion

**Purpose.** An immutable, administrator-approved Machine template that tenants can instantiate through a MachineTemplateClaim.

**Scope.** Namespaced; claims look it up in their own namespace. Spec is immutable.

**Spec** (all required): `version` (string), `template` (free-form object, `x-kubernetes-preserve-unknown-fields`; decoded into a Machine `metadata`-labels plus `spec`), `maxTTLSeconds` (integer, min 1). The controller requires `template.spec.resources.cpu` and `.memory`, and a digest-pinned image (`template.spec.image.source` and `.digest`), then runs the standard image-source validation. Unknown template fields are not checked by the CRD.

**Status.** None written. No reconciler acts on this kind; it is data read by `MachineTemplateClaim` (it is also absent from the controller's `/status` RBAC rule).

**Safety gates.** Immutable; digest pinning; creation requires an administrator on the REST API.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineTemplateVersion
metadata:
  name: vm-template
  namespace: default
spec:
  version: 1.0.0
  maxTTLSeconds: 86400
  template:
    labels:
      team: engineering
    spec:
      image:
        source:
          httpURL: https://images.example.com/ubuntu.qcow2
        digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
      resources:
        cpu: "2"
        memory: 2Gi
      runtime:
        backend: qemu
      network:
        mode: user
```

## MachineTemplateClaim

**Purpose.** Tenant self-service: create a TTL-limited Machine from an approved template, optionally on a claimed virtual network.

**Scope.** Namespaced. Spec is immutable. Tenants may create it (REST and the `kairon-fleet-self-service` ClusterRole allow create).

**Spec.** `templateName` (DNS label, required), `machineName` (DNS label, required), `ttlSeconds` (integer, min 1, required; must not exceed the template's `maxTTLSeconds`), `networkClaim` (DNS label, optional; name of a MachineNetworkClaim for the same Machine that is `Bound`).

**Status.** `phase` (`Pending` on creation, then mirrors the Machine's phase; `Blocked`/`Invalid` on error), `message`.

**Controller behaviour.** Creates one owned Machine from the template with `ttlSeconds` set. With a `networkClaim` it sets a tap interface on the network's bridge with a derived MAC, eBPF dataplane required, merges the network's node selector (conflicts are errors), and injects a systemd-networkd file and cloud-init command for the reserved address. The guest image therefore needs systemd-networkd and cloud-init. Fails if the Machine name is taken by something it does not own.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineTemplateClaim
metadata:
  name: vm-claim
  namespace: default
spec:
  templateName: vm-template
  machineName: dev-machine
  ttlSeconds: 3600
  networkClaim: network-claim
```

## MachineVirtualNetwork

**Purpose.** Defines a durable IPv4/IPv6 address pool on an administrator-provisioned bridge for a set of nodes.

**Scope.** Namespaced. Spec is immutable.

**Spec.** `cidr` (canonical prefix, required), `gateway` (usable address inside the CIDR and not the network address, required), `bridge` (at most 15 chars, and the controller requires a DNS-label form; required), `nodeSelector` (non-empty map, required), `dnsServers` (list of IPs, optional). IPv4 prefixes longer than /30 and IPv6 prefixes longer than /120 are rejected.

**Status.** `phase: Ready` with a message that this means IPAM was accepted, not that the bridge exists. `allocations`: map of claim UID to IP, written by the network-claim reconciler.

**Controller behaviour.** Does not create bridges, VRFs, DHCP or DNS. Two networks may not share a bridge once a claim is processed (the claim is blocked with "isolation cannot be established"). Allocation scans a bounded 65536-address window.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineVirtualNetwork
metadata:
  name: virtual-network
  namespace: default
spec:
  cidr: 10.77.0.0/24
  gateway: 10.77.0.1
  bridge: br-tenant-a
  nodeSelector:
    kairon.zyvor.dev/network-zone: tenant-a
  dnsServers:
    - 10.77.0.1
```

## MachineNetworkClaim

**Purpose.** Reserves an address from a virtual network for one Machine.

**Scope.** Namespaced. Spec is immutable. Tenants may create it.

**Spec.** `networkName` (DNS label, required), `machineName` (DNS label, required).

**Status.** `phase: Bound`, `message`, `allocations` (`address`, `network`, `machine`).

**Controller behaviour.** Allocates the first free address, persisted in the VirtualNetwork's `status.allocations` under the claim UID (resourceVersion CAS), so a recreated claim with the same name cannot reuse its predecessor's address. Addresses are never released automatically; after deleting the claim and every Machine carrying the allocation annotation, run `kaironctl fleet release-address NETWORK CLAIM-UID`.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineNetworkClaim
metadata:
  name: network-claim
  namespace: default
spec:
  networkName: virtual-network
  machineName: dev-machine
```

## MachineUsageLedger

**Purpose.** Accumulates showback totals of provisioned resources for selected Machines. Not a billing-grade record.

**Scope.** Namespaced; counts Machines in its own namespace.

**Spec.** `selector` (map, required, non-empty), `maxGapSeconds` (integer, 30 to 3600, required). The spec is mutable.

**Status.** `phase` (`Recording`, `Gap`), `message`, `watermark`, `totals` with keys `vcpu_hours`, `memory_gib_hours`, `machine_hours`, `unobserved_seconds`.

**Controller behaviour.** The first pass sets the watermark. On each pass it adds provisioned vCPU, memory (GiB) and Machine hours for matching Machines that are `Running` or `Paused` and assigned to a node. If more than `maxGapSeconds` elapsed since the watermark, it adds the whole interval to `unobserved_seconds` and bills nothing for it. Resource changes between observations are not reconstructed, and there is no storage or network metering. CSV export: `GET /api/v1/usage.csv?namespace=NS`.

```yaml
apiVersion: fleet.kairon.zyvor.dev/v1alpha1
kind: MachineUsageLedger
metadata:
  name: usage
  namespace: default
spec:
  selector:
    team: engineering
  maxGapSeconds: 60
```

---

## REST API

Served by the UI API server (`internal/uiapi`). `{resource}` is one of the plural names in the table above; unknown names return 404. Namespace handling uses the same scoped-namespace authorization as the rest of the UI API.

| Route | Behaviour |
| --- | --- |
| `GET /api/v1/fleet/{resource}?namespace=NS` | List objects (JSON array). `machinehaprofiles` and `nodefencerequests` require an administrator identity. |
| `POST /api/v1/fleet/{resource}?namespace=NS` | Create from a JSON body. The server overwrites `apiVersion`, `kind`, `metadata.namespace` and clears `status`; rejects client-supplied `uid`, `resourceVersion` or `deletionTimestamp`; validates with the same Go validator; returns 201. Requires an administrator identity **except** for `machinetemplateclaims` and `machinenetworkclaims`. `machineactionapprovals` always returns 403. |
| `DELETE /api/v1/fleet/{resource}/{namespace}/{name}` | Delete; administrator identity required; returns 204. |
| `GET /api/v1/usage.csv?namespace=NS` | CSV with columns `namespace, ledger, vcpu_hours, memory_gib_hours, machine_hours, unobserved_seconds` taken from MachineUsageLedger `status.totals`. |

The namespace comes from the `namespace` query parameter and defaults to `default`; `requireNamespace` applies the scoped-namespace check before the handler runs. I did not trace the exact behaviour of the viewer role beyond what the guide states (viewer write denial). The UI server's ServiceAccount is granted `get, list, create, delete` on the twelve non-approval kinds by the `kairon-fleet-ui` ClusterRole, but only when `ui.enabled` is true.

### Approval model

- Creating an approval needs the approver's own Kubernetes credential. The shared dashboard credential cannot do it (403).
- Admission (webhook) enforces approver identity, expiry within 10 minutes, no self-approval, and single `Consumed` transition by the bound principal.
- RBAC for approval is two ClusterRoles, `kairon-action-approver` (create, get on `machineactionapprovals`) and `kairon-action-consumer` (get, plus patch on `machineactionapprovals/status`), neither bound automatically.
- `kairon-fleet-self-service` grants tenants get/list on templates and networks and create on template and network claims.

## kaironctl

All subcommands read the kubeconfig/in-cluster environment and use the global `-n/--namespace` flag (default `default`). Unlike the REST API, the CLI relies on Kubernetes RBAC and the admission webhook, not on the UI administrator check.

| Command | Behaviour |
| --- | --- |
| `kaironctl fleet resources` | Print the 13 plural resource names, sorted. |
| `kaironctl fleet get RESOURCE -n NS` | List objects in one namespace as a single line of JSON. `RESOURCE` may be the plural or the Kind (case-insensitive). |
| `kaironctl fleet create FILE.json -n NS` | Read a JSON manifest (JSON only, not YAML), force `metadata.namespace` to `-n` and `apiVersion` to the fleet group, run the Go validator, then create. Refuses `MachineActionApproval` (use `approve-action`). |
| `kaironctl fleet delete RESOURCE NAME -n NS` | Delete one object. |
| `kaironctl fleet approve-action REQUEST.json -n NS` | Reads an `ApprovalSpec` JSON (the request printed by the MCP server), sets `approver` from the caller's Kubernetes identity and `expiresAt` to now plus 5 minutes, and creates the approval with a deterministic name. |
| `kaironctl fleet release-address NETWORK CLAIM-UID -n NS` | Remove a claim UID from the network's `status.allocations`. Refuses while a Machine still carries the allocation annotation or while the claim still exists. |

```bash
kaironctl fleet resources
kaironctl fleet create examples/fleet/autoscaler.json -n default
kaironctl fleet get machineautoscalers -n default
kaironctl fleet delete machineautoscalers autoscaler -n default
kaironctl fleet approve-action REQUEST.json -n default
kaironctl fleet release-address virtual-network CLAIM-UID -n default
```

## Controller flags, environment and Helm values

| Flag | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `--fleet-enabled` | `KAIRON_FLEET_ENABLED` | `false` | Turns on the fleet engine. The controller exits if enabled without webhook TLS configuration. |
| `--fleet-control-namespace` | `KAIRON_FLEET_CONTROL_NAMESPACE` | `kairon-system` | Namespace that must hold MachineHAProfile and NodeFenceRequest objects and the BMC Secrets. |
| none | `KAIRON_FLEET_REDFISH_ORIGINS` | empty | Comma-separated exact HTTPS origins allowed as Redfish `endpoint`s. |
| none | `KAIRON_FLEET_TEST_BRIDGES` | empty | Comma-separated allowlist of isolated bridges for `TestRestore`. |
| none | `KAIRON_FLEET_PROMETHEUS_URL` | empty | Prometheus base URL for autoscaler CPU queries. |
| none | `KAIRON_FLEET_PROMETHEUS_TOKEN` | empty | Optional bearer token for those queries. The Helm chart does not currently set it. |

Helm values (`charts/kairon/values.yaml`, all under `fleet:`):

| Value | Default | Effect |
| --- | --- | --- |
| `fleet.enabled` | `false` | Renders fleet RBAC and sets the controller env. Chart rendering fails unless `webhook.enabled`, `webhook.tlsSecretName`, `webhook.caBundle` are set and `webhook.failurePolicy` is `Fail`. Also creates the fleet `ValidatingWebhookConfiguration` (`/validate-fleet`) when the webhook TLS values are present. |
| `fleet.prometheusURL` | `""` | `KAIRON_FLEET_PROMETHEUS_URL`. |
| `fleet.redfishOrigins` | `[]` | Joined into `KAIRON_FLEET_REDFISH_ORIGINS`. |
| `fleet.redfishSecretNames` | `[]` | Names of BMC Secrets the controller may `get` in the release namespace (a Role with `resourceNames`). The Secret keys are `username`, `password`, optional `ca.crt`. |
| `fleet.isolatedTestBridges` | `[]` | Joined into `KAIRON_FLEET_TEST_BRIDGES`. |

The Helm release namespace is passed as `KAIRON_FLEET_CONTROL_NAMESPACE`. Run a single elected controller leader.

## What is not verified here

- Behaviour against physical BMCs, real migrations and restored application integrity is not covered by the repository's automated tests (per the guide); this reference does not claim it.
- The exact action names the MCP server asks approval for, and the viewer/editor role matrix of the REST API, were not traced for this document.
- CRD rules were read from the YAML; they were not exercised against a live API server.
