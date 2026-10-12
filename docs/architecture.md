---
sidebar_position: 6
title: Architecture
---

# Architecture

Kairon separates Kubernetes orchestration from VM execution. Kubernetes is the source of truth; FluxVM owns normal VMM lifecycle. Live-transfer mechanics are behind a backend-neutral migration adapter rather than assumed FluxVM/QMP endpoints.

## Components

- `kairon-controller`: Machine placement, `MachineMigration` state and `MachineSnapshot` -> CSI `VolumeSnapshot` orchestration. Exposes Prometheus metrics and a migration concurrency quota (see Operational visibility below).
- `kairon-node`: one per VM node; reconciles assigned Machines into FluxVM, resolves DRA/VFIO, exposes the mTLS migration peer API, and optionally (`console.enabled`) a shared-token-gated relay (`internal/consoleproxy`) kairon-ui dials for everything that needs a network path only this node has -- VNC/text console, guest exec (both channels), guest file access, logs, VM-state snapshot/restore, streaming a backup's root qcow2 (`GET /backup-root/{name}`, used to capture a VM as a golden image), sandboxes/templates/HTTP-proxy, the image catalog, warm pools, and the runtime/network diagnostics -- see "Day-2 operations on a running Machine" below for the full list.
- migration peer: TLS 1.3, mandatory client certificate, target prepare/commit/abort and a local atomic session journal.
- migration adapter: optional HTTP-over-Unix-socket component that implements VMM-specific target/source migration operations.
- `kaironctl`: thin Kubernetes API client; it never bypasses the controllers.
- `kairon-ui` (optional): a web dashboard (`internal/uiapi` Go backend + `web/` React SPA) that is itself just another Kubernetes API client, same standing as `kaironctl` -- no privileged side channel, no second source of truth.

## Cold migration

```text
Pending -> Stopping -> Restarting -> Succeeded
             |             |
      verify source    assign target
         stopped       and restart
```

This state is represented in Kubernetes and is restart-safe. Kairon does not copy host-local disk content.

## Live migration

```text
Pending
  -> Starting
     source peer authenticates target over mTLS
     target adapter prepares destination
     target journals Prepared session
  -> Running
     source adapter transfers VM state to opaque endpoint
  -> Cutover
     transfer completed AND target commit succeeded
  -> Adopting
     Machine.nodeName=target
     kairon.zyvor.dev/adopt-only=true
  -> Succeeded
     target discovers incoming runtime and reports Running
```

Failure rules:

- Target unsupported: `Blocked`; source is untouched.
- Source start/transfer failure: target is aborted; no cutover.
- Source transfer succeeds but target commit fails: `NeedsRecovery`; no automatic cutover or restart.
- Target runtime missing after commit: adopt-only guard blocks duplicate creation.

## Cancelling an in-flight live migration

An operator can set `spec.cancel: true` (`kaironctl cancel-migration NAME` or the dashboard's "Cancel migration" button) while a live migration is still `Starting` or `Running` -- strictly before the destination has committed. `internal/agent`'s `reconcileMigration` honors it by calling the exact same source/destination `Abort` primitives an unrequested transfer failure already uses, then lands the migration in a `Cancelled` terminal phase with the source runtime left untouched and its network dataplane un-quiesced, same as every other pre-commit exit path.

`spec.cancel` is a one-way latch (Kairon never clears it back to `false`) and a documented no-op once phase has reached `Cutover` or later: by then the destination has already committed, and aborting would recreate exactly the split-brain risk `NeedsRecovery` exists to avoid. It is likewise a no-op for cold-strategy migrations, which have no in-flight transfer to abort -- use the Machine's own `spec.powerState`/`spec.nodeName` instead. `kaironctl cancel-migration` and the dashboard both refuse locally (before ever patching the object) unless phase is `Starting`/`Running` and `status.effectiveStrategy` is `live`, so an operator gets an immediate answer instead of a `spec.cancel` the agent will silently ignore.

No raw destination URI is accepted from users. A peer endpoint is derived from the selected target node `InternalIP`; TLS validates the configured migration server identity.

## Session durability

Destination sessions are persisted as mode `0600` JSON files using write -> fsync -> atomic rename. The default Helm `emptyDir` survives process/container restart within the Pod. For Pod replacement/node-level durability, configure `migration.stateHostPath` to a pre-created directory writable by the non-root Kairon UID.

## CSI snapshots

`Machine.spec.volumes[]` stores PVC references. `MachineSnapshot` creates one standard `snapshot.storage.k8s.io/v1` `VolumeSnapshot` per PVC and mirrors readiness into Kairon status.

## DRA / VFIO

`Machine.spec.deviceClaims[]` references same-namespace `resource.k8s.io/v1` `ResourceClaim` objects. The node agent requires an allocation, resolves a PCI BDF, normalizes it and checks a node-local allowlist. Namespace users cannot bypass the host PCI authorization boundary by editing annotations.

This path is deliberately device-type-agnostic -- `resolveVFIODevices` only
ever cares that a claim resolves to an allowlisted BDF, never what kind of
PCI device it is, and `spec.deviceClaims` is a list, resolved and passed
to FluxVM's own `vfio_devices` as a set. That means an SR-IOV NIC Virtual
Function, once created and bound to `vfio-pci` on the host (the same
operator/host-setup responsibility GPU passthrough already has -- Kairon
manages neither GPU nor VF driver binding itself), passes through via this
exact same mechanism with no new allocation code -- a Machine can combine
a GPU claim and a NIC-VF claim in one `spec.deviceClaims` list today. See
[guides/machine-sriov.md](guides/machine-sriov.md). Multus (the standard
Kubernetes SR-IOV integration path) doesn't apply here at all -- its whole
mechanism wires extra interfaces into a Pod's network namespace, and a
Machine has no Pod for it to attach to.

## Real CPU pinning

`internal/scheduler` gained a real capacity model, opt-in via
`spec.resources.cpuPinning`: a node's operator-asserted
`kairon.zyvor.dev/pinnable-cpus` label (the same "operator asserts a fact
Kairon can't otherwise know" shape DRA/VFIO's own allowlist and the
storage/network domain labels already use) becomes a real hard filter at
scheduling time, and `AllocateCPUSet` deterministically picks exact core
numbers from a candidate node's free set (its label minus every other
already-assigned Machine's own `spec.resources.allocatedCpuSet`) in the
same scheduling pass `Choose` uses -- node assignment and core allocation
are patched to the Machine atomically, in one call, so they can never land
separately. Deliberately does not attempt to read kubelet's own internal
CPU Manager state file (a known, fragile, unsupported community pattern)
-- the operator-asserted label already carries that responsibility. See
[guides/machine-cpu-pinning.md](guides/machine-cpu-pinning.md).

## Third-party CSI client

`kairon-node` gained a second CSI-client path
(`internal/agent/csi_thirdparty.go`) alongside its own iSCSI driver's
existing one: `node.thirdPartyCSIDrivers` maps a driver name to a socket
path, an explicit operator allowlist rather than reimplementing kubelet's
own `plugins_registry/` registration listener -- the same fail-closed
posture DRA/VFIO's own allowlist already has, applied to CSI.
`resolveCSIVolume` branches to the third-party path when a PV's
`spec.csi.driver` is listed; requests are built directly from the PV's own
fields, with staging/publish paths keyed by the Machine's own
`RuntimeName()` in place of kubelet's Pod-UID convention (there is no Pod
here for one to come from). Drivers whose `CSIDriver` says
`attachRequired` get a `VolumeAttachment` the driver's external-attacher
fulfils (its `attachmentMetadata` becomes the `PublishContext`); node-stage
and node-publish secret refs resolve only from one allowlisted namespace
(`--third-party-csi-secret-namespace`). `status.volumeDriver`
records which driver staged a Machine's volume so teardown routes
correctly later even after the PV/PVC is gone. See
[guides/machine-storage-thirdparty-csi.md](guides/machine-storage-thirdparty-csi.md).

## Day-2 operations on a running Machine

`spec.cloudInit` (SSH keys, hostname, packages, first-boot commands) and
`spec.network.forwards` (inbound SSH/etc. via SLIRP hostfwd) are both read
only once, inside the FluxVM create call in `internal/agent.reconcileMachine`
-- editing either field, or `spec.resources`, on an already-running Machine
is a silent no-op: no error, no status signal, no drift correction. Apply
the change at creation, or delete and recreate. See
[guides/machine-network.md](guides/machine-network.md) for `cloudInit`/
`forwards` usage.

A graphical VNC console is available (`console.enabled`, off by default):
`kairon-ui` relays a browser WebSocket through `kairon-node` to the VM's
local, otherwise-unreachable QEMU VNC socket (`<workspace>/vnc.sock`,
QEMU-backend only) -- see SECURITY.md's "VNC console" section for the
trust model before enabling it. Guest-exec (`POST /v1/vms/{id}/qga/exec`,
FluxVM's real qemu-guest-agent) is now wrapped the same way -- one
synchronous request/response relay, not a long-lived connection --
requiring `spec.guestAgent.enabled` and an admin account; see SECURITY.md's
"Guest exec" section. FluxVM's own vsock-based interactive text console
(`GET /v1/vms/{id}/console`, `virtctl console`'s equivalent) is also
wrapped now, as `spec.guestAgent.console`: `kairon-ui` (xterm.js) relays a
browser WebSocket through `kairon-node`'s own client-dialed WebSocket
connection to FluxVM's console endpoint -- unlike VNC (a Unix socket
kairon-node dials directly), FluxVM itself is the upstream WebSocket
server here. This genuinely needed FluxVM's own proprietary in-guest agent
binary (`fluxvm-guest-agent`, a separate systemd service from
qemu-guest-agent) baked into the guest image -- a real, new dependency
this project hadn't asked of users before -- see
[guides/machine-text-console.md](guides/machine-text-console.md) and
SECURITY.md's "Text console" section. Live cgroup resize
(`POST /v1/vms/{id}/resources`) is now wrapped too, as `spec.resources.limits`
(`internal/agent/resourcelimits.go`) -- a real, kernel-enforced host-side cap
distinct from the QMP hotplug Kairon already wraps (`internal/agent/hotplug.go`,
which grows what the *guest* sees); see
[guides/machine-resource-limits.md](guides/machine-resource-limits.md).

A full hypervisor-level VM-state checkpoint/restore
(`POST /v1/vms/{id}/snapshot`/`start-from-snapshot`/`stop`) is now wrapped
too, admin-only and API-only (`internal/uiapi/vmsnapshot.go` ->
`internal/consoleproxy` -> `internal/fluxvm/hibernate.go`) -- RAM, CPU, and
device state via QEMU's real `savevm` or a Cloud Hypervisor snapshot,
restored back into the *same* Machine, unrelated to `MachineSnapshot`'s
disk-content-only CSI snapshot. Restoring always stops the Machine first,
since FluxVM's own `start-from-snapshot` silently ignores the requested tag
on an already-running VM; `internal/agent`'s reconcile loop self-heals a
Machine left FluxVM-stopped mid-restore back to its last-known-good disk
state, not a second automatic restore attempt. See
[guides/machine-vm-state-snapshot.md](guides/machine-vm-state-snapshot.md)
and SECURITY.md's "VM-state snapshot/restore" section.

A `spec.powerState: Halted` value now sits alongside `Running`/`Stopped`/
`Paused` -- FluxVM's own `stop`/`start` (`internal/fluxvm/hibernate.go`),
powering the VMM process off while FluxVM keeps its own record/disk
intact, so resume reuses FluxVM's own last-applied config instead of a
full Kairon-side recreate the way `Stopped` -> `Running` does. Resume
reuses the exact reconcile branch that already handled `RestoreSnapshot`'s
own crash-recovery self-heal, since both leave FluxVM reporting the
runtime Stopped-with-record-kept; `status.phase` reports `Halted`,
deliberately distinct from `Stopped`, and both scheduler capacity
accounting and `MachineQuota` treat it the same way they already treat
`Stopped`. See [guides/machine-halt.md](guides/machine-halt.md).

Incident bundles are split between the two sides
(`internal/kaironctl/incident.go`): kaironctl writes the cluster half
(Machine, events, migrations, Node, NodeTopology, the ranked causes) itself,
and starts FluxVM's asynchronous `POST /v1/vms/{id}/diagnostics` job through
`internal/uiapi/incident.go` -> `internal/consoleproxy/incident.go` ->
`internal/fluxvm/incident.go`, polls it, downloads the node tar.gz, checks
its sha256 and nests it under `node/`. FluxVM collects the QMP, cgroup,
console and dataplane state and an optional memory dump on the node; the
dump stays there and is fetched on demand. See
[guides/machine-incident-bundle.md](guides/machine-incident-bundle.md).

A second, backend-agnostic guest-exec channel is now wrapped too:
`POST /v1/vms/{id}/agent` (`internal/fluxvm.Client.AgentExec`) rides the
same bespoke vsock guest agent guest file access already uses
(`spec.guestAgent.console`), a genuinely different mechanism from the
QEMU-only `qemu-guest-agent` exec above -- the only guest-exec path that
works on Cloud Hypervisor, Firecracker, or a FluxVm-backend sandbox. See
[guides/machine-guest-agent-files.md](guides/machine-guest-agent-files.md).

FluxVM's own agent-sandbox track -- a lightweight, fast-boot in-tree
hypervisor (`BackendKind::FluxVm`) for short-lived ephemeral workloads --
is wrapped via `spec.sandbox`: `kairon-node` calls FluxVM's real
`POST /v1/sandboxes` instead of `POST /v1/vms` when set
(`internal/fluxvm.Client.CreateSandboxForMachine`), optionally booting
from a pre-built template (`spec.sandbox.templateName`, a node-scoped
admin API wrapping FluxVM's own `GET`/`POST /v1/templates`) instead of
`spec.image`. An HTTP proxy relay
(`ANY /api/v1/machines/{ns}/{name}/sandbox-http/{port}/{path}`) reaches a
service running inside the sandbox directly. See
[guides/machine-sandboxes.md](guides/machine-sandboxes.md) and
SECURITY.md's "Sandboxes" section.

FluxVM's own node-local image catalog (`spec.image.catalogName`) and
warm-VM pools (node-scoped admin API under `/api/v1/nodes/{node}/pools`)
are wrapped the same way as sandbox templates -- both are per-node FluxVM
state, not a new Kairon CRD with its own reconcile loop; warm pools in
particular were deliberately kept API-only rather than a competing
controller, since FluxVM already ships its own `MicroVMPool` CRD/node-local
operator reconciling the identical `/v1/pools` state. See
[guides/machine-image-catalog.md](guides/machine-image-catalog.md) and
[guides/machine-sandboxes.md](guides/machine-sandboxes.md)'s "Warm pools"
section.

Four runtime diagnostics round out the FluxVM route audit this session
worked through: a node's real capability manifest
(`GET /api/v1/nodes/{node}/capabilities`), a Machine's cgroup-derived PSI
pressure stats and effective host CPU set, and a cgroup-v2-freezer-backed
freeze/thaw distinct from `spec.powerState: Paused` (a host-kernel
operation that works even when a VM's own control channel is
unresponsive). Alongside these, four network-observability endpoints
(effective policy, stats, flows, drop-reasons) wrap FluxVM's own
eBPF-dataplane diagnostics -- `network-drop-reasons` is the most direct
answer to "why is my `MachineNetworkPolicy` blocking traffic I expected to
allow." See [guides/machine-diagnostics.md](guides/machine-diagnostics.md)
and [guides/network-policy.md](guides/network-policy.md)'s
"Troubleshooting" section.

Cilium is a separate, opt-in layer on top of that edge. `kairon-controller`
reconciles `CiliumExternalWorkload` and `CiliumNetworkPolicy` with raw REST
when `network.ciliumAttach` / `network.ciliumPolicySync` are enabled — it
does not load BPF. See [network-fabric.md](network-fabric.md).

## Native macOS node

A Mac has no kubelet, so `kairon-node` registers the Mac as a Kubernetes `Node` itself (`internal/macnode`; `--register-node` is on by default on macOS) and refreshes it every 10 seconds. The Node carries `kubernetes.io/arch=arm64`, `kubernetes.io/os=darwin`, `kairon.zyvor.dev/capable=true` and `kairon.zyvor.dev/backend.vz=true` (plus `kairon.zyvor.dev/mlx=true` and an `inference-url` annotation when `--mlx-url` is set), reports allocatable `cpu` and `memory` (unified memory minus the larger of 3 GiB and a quarter, left to macOS), and carries the taint `kairon.zyvor.dev/vm-only:NoSchedule` so ordinary pods never land on a node with no kubelet. The controller schedules against `allocatable` like any other node, so memory admission is the unified-memory figure; Machines tolerate the taint. `kairon-node` sends the Machine to FluxVM with `backend: vz`, and `internal/fluxvm/apple.go` rejects, with a specific message, any feature the Apple backend cannot honour. Linux-only features (eBPF edge, migration, CPU pinning, VFIO) stay Linux-only. See [macos.md](macos.md) and [macos-cluster.md](macos-cluster.md).

## MachineImage catalog and install media

`MachineImage` is a cluster-scoped, named image: `kind: disk` or `kind: iso`, with an `httpURL` or `oci` source and a required sha256 `digest`. `kairon-controller` resolves `spec.image.imageRef` and each `spec.cdroms[].imageRef` once, before the Machine is scheduled: it copies `source` and `digest` into the Machine and fills `spec.image.diskSize` and an empty `spec.resources` from the image's `defaults`. Until every reference resolves the Machine stays `Pending` with a reason, so editing or deleting a `MachineImage` never changes a Machine that already resolved it. On the node, bytes land in a digest-keyed cache and each Machine gets its own overlay. `spec.image.blank` boots an empty root disk (inside `spec.volumes[0]` when one is set), and `spec.cdroms` attaches up to four ISOs read-only; after creation entries can only be removed, and `kairon-node` ejects any drive that still holds media but is no longer listed, because FluxVM will not live-migrate a VM with media in a CD-ROM. See [guides/machine-images.md](guides/machine-images.md) and [guides/machine-install-media.md](guides/machine-install-media.md).

## MachinePool and MachineClaim

A `MachinePool` keeps `spec.replicas` Machines, built from `spec.template`, Running and unclaimed (`kairon.zyvor.dev/pool-state=warm`). Each controller tick binds pending `MachineClaim`s, oldest first, to the oldest Running warm member with a label patch that carries the Machine's `resourceVersion` (a member changed since the listing is skipped, so none is bound twice), then brings the pool back to `spec.replicas` warm members. The bind relabels an already-running Machine rather than creating one, so the claimed Machine carries the pool template by construction, and warm members built from an older template (`machinepool-template-hash`) are replaced at once. A claim's `spec.egress` becomes a default-deny `MachineNetworkPolicy` named `claim-egress-<claim>`. The dashboard's separate FluxVM warm-pool claim with `createMachine: true` builds a `Machine` from the pool's raw template (resources and hotplug headroom, kernel, cloud-init, NUMA/cpuset/hugepages, secure boot/TPM, guest agents, TTL) and pins `spec.nodeName` to the claimed VM's node so the scheduler cannot boot a duplicate; template fields with no safe equivalent (VFIO devices, raw network map, shared folders, data disks, storage override) come back as `machineWarnings`. See [guides/machine-pools.md](guides/machine-pools.md) and [guides/machine-sandboxes.md](guides/machine-sandboxes.md).

## Tenant fence

`spec.tenant` is a label (`kairon.zyvor.dev/tenant`, projected by the controller; the admission webhook rejects a rename or clear), not a boundary on its own. A Machine that sets the annotation `kairon.zyvor.dev/tenant-fence: "true"` opts in. For each opted-in namespace/tenant pair the controller owns a `NetworkSecurityGroup` named `tenant-fence-<tenant>` (labelled `kairon.zyvor.dev/tenant-fence=managed`) whose `spec.policy.denyCidrs` are the other tenants' `status.guestIP`/`guestIPs` as `/32` or `/128`, and deletes it when no non-deleting Machine of that tenant still opts in. `kairon-node` upserts the group to FluxVM like any `NetworkSecurityGroup` and merges its deny list into the `MachineNetworkPolicy` it posts; a user policy is not replaced, and a Machine with no policy gets `defaultAllow: true` plus the denies. This denies only addresses Kubernetes has observed in the same namespace; it is not a VRF. See [guides/tenant-fence.md](guides/tenant-fence.md).

## Preemption by Halt

`spec.priority` orders pending Machines; it halts a running one only when both sides opt in (`internal/preempt`, called from `internal/controller/preempt.go`). The pending Machine carries `kairon.zyvor.dev/preempt: "true"`; the victim carries `kairon.zyvor.dev/preemption-policy: Halt`. A victim must be in the same namespace, strictly lower priority, scheduled, desired Running, and without a non-terminal `MachineMigration` or a `MachineDisruptionBudget` with no allowance. The controller picks one victim per preemptor per tick (lowest priority, then name), patches `spec.powerState: Halted`, and records `kairon.zyvor.dev/preempted-by` (the preemptor's `namespace/name`) and `kairon.zyvor.dev/preempted-power` (the power state to restore, `Running`). Pause is not used because a Paused Machine still counts in node load; Halted frees the capacity and drops out of `MachineQuota`. When the preemptor is deleted, Halted, Stopped, or no longer pending and unscheduled, the controller restores `powerState: Running` and clears both annotations. See [guides/preemption.md](guides/preemption.md).

## Stale evacuation

The `NodeUnreachable`/`Fenced` model is unchanged: Kairon never reschedules off a node on its own. `kairon-controller --stale-evacuation` (Helm `controller.staleEvacuation.enabled`, off by default) automates only the attested step. `kaironctl node fence NODE --reason ...` (or a power-fencing tool) sets `kairon.zyvor.dev/node-fenced` on the Node; a Machine annotated `kairon.zyvor.dev/evacuate=true` on that node is then fenced through the shared `internal/fencing` package (the same transition as `kaironctl fence`) only if the node is not Ready or its liveness Lease is stale, the Machine is `NodeUnreachable=True` with no unfinished migration, and every matching `MachineDisruptionBudget` allows a disruption, at most `--stale-evacuation-max-per-tick` (default 10) per tick. Nothing is migrated; the normal scheduler places the Machine on its next tick. See [guides/machine-fencing.md](guides/machine-fencing.md).

## MCP server, approval and audit log

`kaironctl mcp serve` is an MCP server on stdin/stdout and, like `kaironctl`, only a Kubernetes API client (plus kairon-ui for network reads). Write tools are offered only with `--allow-write`. Each write call is appended to a hash-chained audit log (`--audit-log`, default `~/.kairon/audit.jsonl`) before and after it runs, and is refused if the log cannot be written; `kaironctl agent audit-verify` checks the chain. With writes enabled, `delete_machine`, `fork_machine` and `machine_backup` restore/delete also need per-call human approval (`--require-approval`, on by default): the first call is refused with an id, and `kaironctl approve KIND/NAMESPACE/NAME ID` writes a time-limited, single-use `kairon.zyvor.dev/mcp-approval` annotation on the target that the identical retry consumes with an atomic patch. See [guides/hermes-mcp.md](guides/hermes-mcp.md) and [guides/agent-plane.md](guides/agent-plane.md).

## VM edge via the FluxVM TC program

Kairon declares a per-Machine network edge (stable identity, anti-spoof, learn-IP, QoS, DNS/SNI allow lists, attributed drops, conntrack move) and projects it to FluxVM; FluxVM enforces it in its TC/TCX program (`fluxvm_tc.bpf.o`) in the VM's datapath. Kairon does not own or load BPF. `dataplaneMode` is passed to FluxVM per Machine and an empty value uses the host's `[sandbox.dataplane]` setting; the conntrack snapshot carried by a live migration is validated on export and restore (32,768 entries and 16 MiB at most). See [ebpf-edge.md](ebpf-edge.md) and [ebpf-performance-patch.md](ebpf-performance-patch.md).

## Fleet automation and scoped UI authorization

Fleet automation is experimental and opt-in (`fleet.enabled`, `--fleet-enabled`). It adds namespaced resources in the `fleet.kairon.zyvor.dev/v1alpha1` group (HA profiles and fence requests, recovery plans, autoscalers, balancing, backup groups, virtual networks, import campaigns, templates and claims, usage ledgers, action approvals) that drive the existing Machine, migration, backup/restore, MachineSet and MachinePool paths, not a second hypervisor. It requires the admission webhook with `failurePolicy: Fail`; the chart refuses to render it otherwise, and the controller refuses to start it without webhook TLS configuration (it cannot see the webhook's `failurePolicy` itself). See [guides/enterprise-fleet.md](guides/enterprise-fleet.md) for the feature-status table.

`ui.auth.namespaceScoping.enabled` (off by default) adds a namespace axis to `kairon-ui`'s own authorization. A non-admin operator may act only on namespaces in `ui.auth.users[].namespaces` or reachable through `ui.oidc.namespaceGroups`, re-derived on every request. In scoped mode an unnamed session (the shared token) gets 403, the overview is filtered, a `viewer` cannot mutate, and an evacuation is authorized against every namespace in the batch before any migration is created.

## Developer ecosystem kit

`ecosystem/` (reviewed deployment recipes, disposable-VM CI, Terraform manifests, orphan cleanup, qualification tooling) and `sdk/python` and `sdk/typescript` (dependency-free clients) live outside the control plane. Lifecycle calls use the caller's Kubernetes RBAC and guest operations reuse the existing kairon-ui admin, namespace and Machine-access checks; they change no controller or node behavior. See [guides/developer-ecosystem.md](guides/developer-ecosystem.md).

## Operational visibility

`kairon-controller` computes Prometheus metrics (`internal/metrics`) from the same migration list it already fetches every reconcile tick -- one call site, not scattered instrumentation -- and serves them on its existing health port. `charts/kairon/alerts.yaml` ships example alert rules for a stuck `NeedsRecovery`, a high migration failure rate, a migration stuck in flight, and an unencrypted data-plane (plus reconcile-error, API-server-error, webhook-deny, quota, disruption-budget and MachineSet-rollout rules); the migration alerts link to [`runbook-migration-failures.md`](runbook-migration-failures.md).

A migration concurrency quota (`migration.maxConcurrentPerNode`/`maxConcurrentCluster`, both `0` = unlimited) is enforced in the same admission path as target-eligibility checks, rejecting into the existing `Blocked` phase -- not a new state, not a new mechanism.

Every reconcile-loop failure is logged (`internal/controller`, `internal/agent`), including the *secondary* case of the follow-up status-patch itself failing after a primary reconcile error -- both are surfaced, not just the first. All three workloads set CPU/memory `resources:` requests and limits by default, and their ClusterRoles grant exactly the verbs their code paths use (no unused `update` alongside `patch`, confirmed against `internal/kube.Client`'s actual HTTP methods).

## Delete during migration / network quiesce resume

Deleting a Machine while a `MachineMigration` still targets it is refused, not raced: `kairon-node` won't delete the source runtime until that migration reaches a terminal phase, so a delete issued mid-transfer waits rather than pulling the runtime out from under a live RAM/state stream.

A live migration also always resumes the source's own network dataplane when the migration stops short of a destination commit — blocked, an unsupported mode, a transfer that never started, or one that fails or is cancelled mid-flight. The source Machine stays the real, running VM on every one of those paths (only `NeedsRecovery`'s genuinely ambiguous case is left untouched, on purpose), so the network quiesce `kairon-node` takes out on it moments before the transfer begins is always undone again once the attempt is over — never left stranded through every future reconcile tick just because the migration didn't reach a commit.
