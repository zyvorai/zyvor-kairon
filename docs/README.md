---
sidebar_position: 1
title: Overview
---

# Kairon documentation

The root [README](https://github.com/zyvorai/zyvor-kairon/blob/main/README.md) is the product landing page. **This `docs/` tree is authoritative** for install, runbooks, guides, and status detail.

## Start here

| I want to… | Read this |
|---|---|
| See the product landing | [Root README](https://github.com/zyvorai/zyvor-kairon/blob/main/README.md) |
| Install and run a first Machine | [getting-started.md](getting-started.md) |
| Deploy to a host in one command, no local build | [getting-started.md#fastest-path](getting-started.md#fastest-path) |
| Check that it all works | [guides/selftest.md](guides/selftest.md) (`kaironctl selftest`) |
| Ten-minute architecture tour | [../ARCHITECTURE.md](https://github.com/zyvorai/zyvor-kairon/blob/main/ARCHITECTURE.md) |
| Deep architecture reference | [architecture.md](architecture.md) |
| Full feature inventory | [WHAT_SHIPS.md](WHAT_SHIPS.md) |
| CLI reference | [CLI.md](CLI.md) |
| Dependency policy | [DEPENDENCIES.md](DEPENDENCIES.md) |
| Release status & production gaps | [STATUS.md](STATUS.md) |
| Hardware / lab matrix | [COMPATIBILITY.md](COMPATIBILITY.md) |
| Roadmap | [../ROADMAP.md](https://github.com/zyvorai/zyvor-kairon/blob/main/ROADMAP.md) |
| Migration failures / NeedsRecovery | [runbook-migration-failures.md](runbook-migration-failures.md) |
| Security / threat model | [../SECURITY.md](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md) |
| Contributing | [../CONTRIBUTING.md](https://github.com/zyvorai/zyvor-kairon/blob/main/CONTRIBUTING.md) |

## Documentation map

| Doc | Covers |
|---|---|
| [`ARCHITECTURE.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/ARCHITECTURE.md) | The ten-minute tour: components, request flow, trust boundaries/deployment topology, design rationale |
| [`getting-started.md`](getting-started.md) | Install, first Machine, cold migrate, live migration, the dashboard, Network Fabric |
| [`tutorials/machine-lifecycle.md`](tutorials/machine-lifecycle.md) | Narrated walkthrough: one Machine through create → relocate → snapshot → restore |
| [`architecture.md`](architecture.md) | Deep reference: the cold/live migration state machines, session durability, CSI/DRA mechanics, operational visibility |
| [`migration-adapter.md`](migration-adapter.md) | The migration adapter HTTP contract, trust boundary, the real `kairon-migration-adapter-fluxvm` implementation |
| [`network-fabric.md`](network-fabric.md) | `MachineNetworkPolicy`/`NetworkSecurityGroup`, FluxVM eBPF edge, opt-in Cilium ExternalWorkload attach and CNP sync |
| [`macos.md`](macos.md) · [`macos-cluster.md`](macos-cluster.md) | Kairon on Apple silicon: Mac as a Node, `vz` Machines, verified vs not, Mac mini to Mac Studio cluster |
| [`ecosystem/README.md`](https://github.com/zyvorai/kairon/blob/main/ecosystem/README.md) | Developer ecosystem kit: SDKs, recipes, GitHub Actions, Terraform |
| [`ebpf-edge.md`](ebpf-edge.md) | VM edge enforced by FluxVM: anti-spoof, learn-IP, QoS, DNS/SNI allow lists, attributed drops and metrics, packet capture, conntrack move on live migration |
| [`tutorials/network-fabric.md`](tutorials/network-fabric.md) · [`guides/machine-network.md`](guides/machine-network.md) · [`guides/network-policy.md`](guides/network-policy.md) | Network Fabric walkthrough and field-level guides |
| [`guides/machine-quotas.md`](guides/machine-quotas.md) · [`guides/machine-disruption-budgets.md`](guides/machine-disruption-budgets.md) | `MachineQuota`/`MachineDisruptionBudget` reference, including the admission webhook |
| [`runbook-migration-failures.md`](runbook-migration-failures.md) | Diagnosing and resolving `NeedsRecovery`, alert-to-runbook cross-references |
| [`guides/machine-fencing.md`](guides/machine-fencing.md) | `NodeUnreachable`/`Fenced` conditions, `kaironctl fence`'s safety model, storage/network migration preflight labels |
| [`ai-agents.md`](ai-agents.md) · [`guides/hermes-mcp.md`](guides/hermes-mcp.md) | AI agent integration over MCP (Hermes Agent, Claude Code, Cursor): Kairon and FluxVM servers, scoped credentials, workflows; `kaironctl mcp serve` reference |
| [`guides/agent-plane.md`](guides/agent-plane.md) | Agent plane: sealed claims, strict egress compiler, edge Warning events and per-Machine traffic baselines, cosign image verification, SEV-SNP/TDX attestation, the audited MCP write path, and the proposal-only AI assistant (`ask`) and `diagnose` |
| [`guides/kairon-ui-dashboard.md`](guides/kairon-ui-dashboard.md) | The web console: sign-in and default-password banner, namespace selector and all-namespaces Machines view, table filter, bulk power actions, navigation and command palette, per-resource pages, machine inspector operations, node tools |
| [`guides/kairon-ui-api.md`](guides/kairon-ui-api.md) | Every `kairon-ui` route with its access model (roles, namespace scoping, console tickets, unauthenticated image blobs) |
| [`guides/kairon-ui-ha.md`](guides/kairon-ui-ha.md) | Running `ui.replicaCount > 1`: what's shared, how, and its real limits |
| [`guides/kairon-controller-ha.md`](guides/kairon-controller-ha.md) | Running `controller.replicaCount > 1`: Lease-based leader election, RBAC, bare-metal setup |
| [`guides/observability.md`](guides/observability.md) | What each component's `/metrics` exposes, the `kairon-health` alert group, and the renamed `PrometheusRule` |
| [`guides/machine-migration-tls.md`](guides/machine-migration-tls.md) | Control-plane vs. data-plane migration TLS, and real per-node data-plane identity via `migration.dataplaneTlsSecretName` |
| [`guides/kairon-ui-oidc.md`](guides/kairon-ui-oidc.md) | OIDC/SSO setup, the Authorization Code + PKCE flow, and why it breaks Go-stdlib-only |
| [`guides/kairon-ui-console-rbac.md`](guides/kairon-ui-console-rbac.md) | Real Kubernetes RBAC (`machines/console` + `SubjectAccessReview`) for console access, opt-in alongside the annotation allowlist |
| [`guides/machine-storage.md`](guides/machine-storage.md) · [`guides/machine-storage-csi.md`](guides/machine-storage-csi.md) | PVC-backed boot disks; Kairon's own first-cut iSCSI CSI driver and why it breaks Go-stdlib-only |
| [`guides/machine-storage-nvmeof.md`](guides/machine-storage-nvmeof.md) | NVMe-oF volumes (`nvme-tcp`, `nvme-rdma`, native multipath), raw Block boot in place, per-disk QEMU I/O and the `StoragePaths` condition (experimental) |
| [`guides/machine-storage-atlas.md`](guides/machine-storage-atlas.md) | `spec.volumes[].atlas`: controller provisions Machine disks through Atlas (PVC or raw RBD), gates scheduling on readiness, releases after runtime cleanup |
| [`guides/machine-storage-thirdparty-csi.md`](guides/machine-storage-thirdparty-csi.md) | `node.thirdPartyCSIDrivers`: `kairon-node` as a generic CSI client against an allowlisted third-party driver, including `attachRequired` drivers (VolumeAttachment) and namespace-allowlisted node secrets |
| [`guides/migrate-from-vmware.md`](guides/migrate-from-vmware.md) | Moving VMware VMs to Kairon: `kaironctl import ova`, offline virtio repair, checklist |
| [`guides/machine-image-import.md`](guides/machine-image-import.md) | `spec.image.source`: downloading a remote image URL into `kairon-node`'s own digest-keyed cache |
| [`guides/machine-images.md`](guides/machine-images.md) | `MachineImage`: a cluster-scoped, versioned catalog of boot disks and ISOs, pinned into a Machine by digest once |
| [`guides/machine-install-media.md`](guides/machine-install-media.md) | `spec.cdroms` + `spec.image.blank`: install Windows or Linux from an ISO onto an empty disk |
| [`guides/machine-sets.md`](guides/machine-sets.md) | `MachineSet`: replica reconciliation, `RollingUpdate`/`Recreate` rollout strategy |
| [`guides/machine-fork.md`](guides/machine-fork.md) | `kaironctl fork` / `fork_machine`: live copies of a Running Machine from one memory snapshot |
| [`guides/machine-pools.md`](guides/machine-pools.md) | `MachinePool` / `MachineClaim`: warm, pre-booted Machines claimed in one reconcile tick |
| [`guides/machine-instance-types.md`](guides/machine-instance-types.md) | `MachineInstanceType`: a reusable named CPU/memory shape resolved into `spec.resources` once |
| [`guides/machine-cpu-numa.md`](guides/machine-cpu-numa.md) | `spec.resources.numaNode`/`.cpuSet`/`.hugepages`: qemu-only NUMA/hugepage passthroughs, and what they don't guarantee (no real host-core pinning) |
| [`guides/machine-elastic-memory.md`](guides/machine-elastic-memory.md) | `spec.resources.elasticMemory`: virtio-balloon reclaim under host memory pressure, a node memory overcommit ratio, the `KaironMemoryPressure` condition and opt-in evacuation (experimental) |
| [`guides/virtual-networks.md`](guides/virtual-networks.md) | `VirtualNetwork`: isolated IPv4 networks routed in eBPF with a Geneve overlay, controller IPAM, egress SNAT, floating IPs and an announce-only BGP speaker (experimental) |
| [`guides/machine-topology.md`](guides/machine-topology.md) | `spec.topology` + `spec.devices` + `NodeTopology`: NUMA, GPU, NIC and NVMe locality with hard placement policies and bound guest NUMA memory (experimental) |
| [`guides/gpu-vgpu.md`](guides/gpu-vgpu.md) | `spec.devices[].profile`: vGPU (mdev) and MIG-backed profiles scheduled by free instances and GPU memory, created and removed with the Machine; GPU health in `NodeTopology` and VRAM metrics (experimental) |
| [`guides/cross-cluster-dr.md`](guides/cross-cluster-dr.md) | `MachineProtectionPlan`: replicate Machines to a second cluster, RPO/RTO objectives, ordered failover and failback with epoch fencing, DR tests into an isolated bridge, `kaironctl dr` (experimental) |
| [`guides/machine-cpu-pinning.md`](guides/machine-cpu-pinning.md) | `spec.resources.cpuPinning`: real exclusive host-core allocation, and the `pinnable-cpus` node label it depends on (operator-set or discovered by kairon-node) |
| [`guides/machine-windows-guests.md`](guides/machine-windows-guests.md) | What Windows guest support covers today (legacy-BIOS + cloudbase-init, and now UEFI Secure Boot/vTPM for Windows 11 given a node-configured OVMF vars template) |
| [`guides/migration-policies.md`](guides/migration-policies.md) | `MigrationPolicy`: selector-scoped migration bandwidth defaulting and concurrency caps |
| [`guides/machine-snapshot-quiesce.md`](guides/machine-snapshot-quiesce.md) | Real guest `fsfreeze`/`fsthaw` around `MachineSnapshot`, and the controller↔node coordination protocol behind it |
| [`guides/machine-backup.md`](guides/machine-backup.md) | Full disk backups with guest fsfreeze (`MachineBackup`) through FluxVM or Atlas S3, and in-place restore (`MachineBackupRestore`) |
| [`guides/machine-backup-incremental.md`](guides/machine-backup-incremental.md) | Incremental S3 backups with changed-block tracking, `MachineBackupPolicy` schedules, GFS retention, synthetic fulls, encryption and Object Lock (experimental) |
| [`guides/machine-snapshot-restore.md`](guides/machine-snapshot-restore.md) | Restoring a `MachineSnapshot` volume into a new `PersistentVolumeClaim` via the standard CSI `dataSource` flow |
| [`guides/machine-pause-resume.md`](guides/machine-pause-resume.md) · [`guides/machine-halt.md`](guides/machine-halt.md) | `spec.powerState: Paused`/`Halted`: suspending guest CPUs vs. powering off the VMM process while FluxVM keeps its record |
| [`guides/machine-incident-bundle.md`](guides/machine-incident-bundle.md) | `kaironctl diagnose machine --deep`: one tar.gz with the Machine's cluster state and FluxVM's QMP, cgroup, console, dataplane and pcap from its node, redacted by default, plus an opt-in guest memory dump (experimental) |
| [`guides/machine-hibernate.md`](guides/machine-hibernate.md) | `spec.powerState: Hibernated`: saving RAM and device state to disk, freeing the node, and resuming the same boot here or on a compatible node (experimental) |
| [`guides/machine-hotplug.md`](guides/machine-hotplug.md) | Growing `spec.resources.cpu`/`.memory` (kept across stop/start, `hotplug-persist`), and hot-attaching PVC disks (`spec.disks`) and extra NICs (`spec.network.extraInterfaces`) on a `Running` Machine |
| [`guides/node-maintenance-drs.md`](guides/node-maintenance-drs.md) | Node maintenance mode (cordon, migrate or park Machines, restore on exit) and DRS rebalancing by reserved CPU and memory: `kaironctl node maintenance`, `kaironctl drs plan`, `-drs-mode` (experimental) |
| [`guides/preemption.md`](guides/preemption.md) | Opt-in preemption: a pending higher-priority Machine halts an opted-in lower-priority one, which resumes when the preemptor is gone |
| [`guides/tenant-fence.md`](guides/tenant-fence.md) | `spec.tenant` + `tenant-fence`: east-west deny of other tenants' observed guest addresses in a namespace |
| [`guides/machine-resource-limits.md`](guides/machine-resource-limits.md) | `spec.resources.limits`: real, kernel-enforced cgroup v2 caps, and `status.resourceUsage` live usage |
| [`guides/machine-placement.md`](guides/machine-placement.md) | Scheduler internals: affinity/anti-affinity, weighted soft scoring, `topologySpreadConstraints`, DRA topology hints |
| [`guides/machine-sriov.md`](guides/machine-sriov.md) | SR-IOV NIC passthrough by reusing the existing GPU/VFIO DRA mechanism — why Multus doesn't apply to Kairon Machines at all |
| [`guides/machine-guest-agent.md`](guides/machine-guest-agent.md) | `spec.guestAgent`: real `qemu-guest-agent`-reported `status.guestIP`, including `user`/SLIRP networking |
| [`guides/machine-guest-exec.md`](guides/machine-guest-exec.md) | Guest exec (`qemu-guest-agent`), and the smaller API-only fsfreeze-status/firewall endpoints alongside it |
| [`guides/machine-guest-agent-files.md`](guides/machine-guest-agent-files.md) | Guest file access and the backend-agnostic vsock-agent guest exec, both over `spec.guestAgent.console` |
| [`guides/machine-text-console.md`](guides/machine-text-console.md) | Interactive in-browser text console over FluxVM's own proprietary vsock guest agent |
| [`guides/machine-logs.md`](guides/machine-logs.md) | Streaming a Machine's real captured serial console output -- Kairon's `kubectl logs`/`-f` equivalent |
| [`guides/machine-vm-state-snapshot.md`](guides/machine-vm-state-snapshot.md) | Full hypervisor-level VM-state checkpoint/restore (RAM, CPU, device state) -- distinct from `MachineSnapshot`'s disk-content-only CSI snapshot |
| [`guides/machine-sandboxes.md`](guides/machine-sandboxes.md) | `spec.sandbox`: FluxVM's lightweight agent-sandbox track, templates, the HTTP proxy relay, warm pools, and the egress check |
| [`guides/machine-image-catalog.md`](guides/machine-image-catalog.md) | `spec.image.catalogName`: FluxVM's node-local, checksummed image catalog and its admin API |
| [`guides/machine-diagnostics.md`](guides/machine-diagnostics.md) | Runtime capabilities, PSI pressure, effective CPU set, and cgroup-level freeze/thaw |
| [`guides/crd-versioning.md`](guides/crd-versioning.md) | What the next CRD version bump after `v1` still requires; the conversion webhook scaffold that exists today |
| [`runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md) · [`runbook-recovery-drill.md`](runbook-recovery-drill.md) | Real two-host live-migration testing; deliberately drilling a `NeedsRecovery` recovery |
| [`runbook-backup-restore.md`](runbook-backup-restore.md) | Backing up/restoring Kairon's CRD state (`scripts/backup-crds.sh`/`restore-crds.sh`), and what it doesn't cover (VM disk content, FluxVM host state) |
| [`runbook-velero-backup.md`](runbook-velero-backup.md) | Using generic Velero (no Kairon-specific plugin) instead — what works out of the box, and the one real gap (no disk-content snapshot without a real CSI storage backend) |
| [`runbook-vm-export.md`](runbook-vm-export.md) | Getting a Machine's disk content out of the cluster entirely, with standard Kubernetes primitives (no new Kairon-specific export tooling) |
| [`guides/enterprise-fleet.md`](guides/enterprise-fleet.md) · [`guides/enterprise-fleet-reference.md`](guides/enterprise-fleet-reference.md) | Experimental, opt-in fleet automation (`fleet.kairon.zyvor.dev`): feature-status table, installation, trusted MCP action approvals, IPAM and usage accounting; and the field-level reference for those resources |
| [`guides/developer-ecosystem.md`](guides/developer-ecosystem.md) | Developer kit: dependency-free Python and TypeScript SDKs, deployment recipes, disposable-VM CI, Terraform manifests, orphan cleanup and qualification tooling |
| [`guides/machine-snapshot-schedules.md`](guides/machine-snapshot-schedules.md) | `MachineSnapshotSchedule`: periodic or `dailyAt` snapshots by label selector, `jitterSeconds`, `keepLast`/`maxAgeSeconds` retention, manual trigger, missed-run deadline |
| [`guides/kaironctl-bulk-delete.md`](guides/kaironctl-bulk-delete.md) · [`guides/kaironctl-get-selector.md`](guides/kaironctl-get-selector.md) · [`guides/kaironctl-scale-selector.md`](guides/kaironctl-scale-selector.md) | `kaironctl delete`, `get` and `scale` with `--selector`: act on, preview or scale every object matching a label selector |
| [`ebpf-performance-patch.md`](ebpf-performance-patch.md) | Per-Machine partitioned eBPF drop-counter state and validated conntrack snapshots: what changed, and the measured bookkeeping cost |
| [`SUBSCRIPTION-MODEL.md`](SUBSCRIPTION-MODEL.md) | Annual support subscription plans (Community, Standard, Supported, Enterprise) and terms; the software stays open source |
| [`OPENSSF_BEST_PRACTICES.md`](OPENSSF_BEST_PRACTICES.md) | OpenSSF Best Practices badge: where the evidence for each criteria area lives |
| [`design-cluster-api-provider.md`](design-cluster-api-provider.md) | Scoped, not built: what a real Cluster API infrastructure provider would take, and the real `ownerReferences` gap that blocks it today |
| [`ROADMAP.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/ROADMAP.md) · [`RELEASE_NOTES.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/RELEASE_NOTES.md) | What shipped per version, what's next; per-release changelog |
| [`SECURITY.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md) | Threat model, vulnerability reporting |
| [`CONTRIBUTING.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/CONTRIBUTING.md) | PR checklist, coverage floor, frontend checks |

---

### Also in this tree

| Doc | Covers |
|---|---|
| [`guides/relocating-a-machine.md`](guides/relocating-a-machine.md) | Cold/live migrate, evacuate, enable mTLS, cancel/delete/quiesce behavior |
| [`guides/admission-webhook.md`](guides/admission-webhook.md) | Enabling the ValidatingWebhook for quotas/budgets (README “Guarding the fleet”) |
| [`WHAT_SHIPS.md`](WHAT_SHIPS.md) | Full feature inventory (was README “What ships today”) |
| [`CLI.md`](CLI.md) | Full `kaironctl` / `kubectl kairon` command reference (Cobra, embedded Helm, Krew) |
| [`DEPENDENCIES.md`](DEPENDENCIES.md) | Stdlib-only controller/node vs named exceptions (CLI Helm, OIDC, CSI) |
| [`STATUS.md`](STATUS.md) | v0.9.1 status + production gaps |
