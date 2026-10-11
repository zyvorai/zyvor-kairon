# Enterprise fleet automation (experimental)

This opt-in extension uses `fleet.kairon.zyvor.dev/v1alpha1` resources and existing Kairon Machine, migration, backup, restore, pool and set primitives. It does not introduce a second hypervisor. Review the status below before deploying; this is a draft feature set needing cluster and hardware validation.

For per-kind field tables, status fields, safety gates, the REST/CLI surface and controller flags, see the [Enterprise fleet reference](enterprise-fleet-reference.md).

| Requested feature | Implemented behavior | Remaining work |
| --- | --- | --- |
| Verified HA | Node-UID-bound Redfish ForceOff, confirmed Off before ownership release, restart limits, disruption budgets | Hardware fault injection, vendor coverage; automatic recovery leaves hosts cordoned |
| Tenant authorization | Scoped namespace discovery and overview; unnamed shared sessions denied in scoped mode; viewer write denial; evacuation batch authorization | External audit and broader end-to-end role matrix |
| Trusted action approvals | Separate Kubernetes identity approves immutable, argument/UID/generation/expiry-bound intents; CAS single consumption | Exact action execution still has a consume-to-mutation race; legacy mode remains available |
| Recovery plans | Same-cluster dependency DAG, backup age checks, durable progress, restore timing, isolated image-disk test restores | Cross-cluster replication, failover/failback, automatic Atlas PVC cutover, application health verification |
| Balancing | Reserved CPU/memory pressure, scheduler constraints, cooldowns, dry-run recommendations, migration intents | Live PSI/I/O pressure, migration cost model and hardware certification |
| Autoscaling | MachineSet/Pool replica bounds, fresh Prometheus CPU, stabilization, max-step and budget checks | Custom metrics; CPU uses existing FluxVM lifetime-average sample, not a rolling window |
| Backup groups | Per-Machine quiesce, durable batches, scheduling, owned backup retention | Distributed transaction consistency. Incremental S3 backups with Object Lock are per Machine through `MachineBackupPolicy` ([machine-backup-incremental.md](machine-backup-incremental.md)), not yet through backup groups |
| Virtual networks | Durable IPv4/IPv6 IPAM on administrator-provisioned bridges; network/template claim binding | Overlay/VRF, DHCP/DNS services, routing and network provisioning |
| Import campaigns | Digest-pinned OVA batches, concurrency bounds and owned Machine progress | vCenter inventory, staged cutover and migration rollback; Stopped entries are only staged |
| Self-service templates | Immutable versions, digest-pinned sources, TTL-limited claims, optional network claims | Approval workflow UI and catalog publishing lifecycle |
| Cluster API provider | Not implemented | Separate provider CRDs, cluster bootstrap integration and conformance tests |
| Usage accounting | CAS watermarks, vCPU/memory/Machine hour totals, gap reporting and CSV | Storage/network metering and financial billing accuracy |

## Installation

Helm installs the new CRDs for fresh installs. For an existing release, apply `deploy/fleet-crds.yaml` explicitly because Helm does not upgrade CRDs. Provision a valid webhook certificate, Secret and CA bundle following the existing webhook guide. Enable `fleet.enabled`, `webhook.enabled` and `webhook.failurePolicy=Fail`; the chart refuses fleet activation without these prerequisites. The controller also refuses direct fleet activation without webhook TLS configuration.

Configure `fleet.prometheusURL`, `fleet.redfishOrigins`, `fleet.redfishSecretNames` and `fleet.isolatedTestBridges` as needed. HTTPS Redfish origins are an administrator allowlist. Each BMC Secret has `username`, `password` and optional `ca.crt` keys. Certificate verification stays enabled and redirects are rejected. Pre-provision isolated test bridges without production uplinks. Fleet profiles and fence requests must live in the configured control namespace (the Helm release namespace).

Keep a single elected controller leader. Backups, migrations, runtime readiness, quiesce, isolation and fencing require the corresponding existing Kairon/FluxVM services. A Ready fleet networking object means its IPAM spec was accepted, not that a bridge was provisioned.

## CLI and examples

Examples in `examples/fleet/` contain placeholder image URLs, digests, Machine names, bridges and credentials; replace them with actual deployment values.

```bash
kaironctl fleet resources
kaironctl fleet create examples/fleet/autoscaler.json -n default
kaironctl fleet get machineautoscalers -n default
kaironctl fleet create examples/fleet/recovery.json -n default
# Recovery is initially paused; set spec.start=true only after reviewing targets.
```

Recovery plan steps/mode/data-age constraints are immutable; `start` can pause/resume the plan. Completed steps remain completed. Recovery completion indicates all target Machines report Running, not that the application passed a health check. Atlas restore deliberately blocks pending operator attachment and application validation.

The Fleet dashboard lists tenant resources, phases, messages and usage totals and accepts JSON intents according to the authenticated role. Non-admin creation is restricted to template and network claims. Trusted approval creation is unavailable through the shared dashboard credential.

## Trusted MCP approvals

Use `KAIRON_MCP_APPROVAL_MODE=resource` with the MCP server's approval requirement enabled. The default legacy annotation mode remains for compatibility. Bind `kairon-action-consumer` to the agent and `kairon-action-approver` to a separate human credential using namespace RoleBindings. Neither role is automatically bound. Grant normal Machine action permissions separately and authorize SelfSubjectReview identity discovery where required. The agent prints an exact request; the separate approver runs:

```bash
kaironctl fleet approve-action REQUEST.json -n default
```

Admission binds the approver to Kubernetes userInfo and rejects self-approval, excessive expiry and replay/reset. Status can transition once to Consumed only by the bound principal. Consumption occurs before the action; a failed action requires a newly reviewed intent, and the approval does not make the subsequent operation transactional. Administrative Kubernetes credentials can bypass RBAC and must remain outside the agent trust boundary.

## IPAM and accounting

Network allocations persist in VirtualNetwork status under claim UID and use resourceVersion CAS. Addresses are not freed automatically on deletion. Use `kaironctl fleet release-address` after deleting the claim and all Machines referencing the allocation. Generated guest configuration expects systemd-networkd and cloud-init; a different guest OS needs an appropriate image/configuration.

Ledgers estimate reserved resources between controller observations for Running/Paused assigned Machines. Outages beyond maxGapSeconds are recorded as unobserved seconds rather than billed. Resource changes between observations are not reconstructed. This is showback data, not a billing-grade invoice. CSV is exposed at `/api/v1/usage.csv?namespace=...` with the same namespace authorization.

## Validation required before production

Automated tests exercise spec/DAG validation, address allocation, CAS/replay, scale arithmetic, metric freshness, usage gaps, approval binding and Redfish TLS/power-state transitions against test servers. They do not prove physical power fencing, cross-host migrations, restored application integrity or tenant bridge isolation. Consult the package test report for exact commands, failures and checks not run.
