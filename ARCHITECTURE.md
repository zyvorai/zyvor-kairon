# Architecture

This is the ten-minute tour: what each piece owns, how a `Machine` actually turns into a running VM, and where the trust boundaries are. For exact state-machine transitions, session-durability internals, and code-identifier-level detail, see [`docs/architecture.md`](docs/architecture.md) — that doc is the reference; this one is the map.

## How it fits together

```text
                  kubectl / GitOps / kaironctl / kairon-ui
                                      │
                                      ▼
                 ┌────────────────────────────────────────┐
                 │             Kubernetes API             │
                 │ Machine · MachineMigration · Snapshot  │
                 │ MachineQuota · MachineDisruptionBudget │
                 └────────────────────────────────────────┘
                                      │
                  ┌───────────────────┴────────────────────┐
                  ▼                                        ▼
┌───────────────────────────────────┐      ┌───────────────────────────────┐
│         kairon-controller         │      │          kairon-node          │
│     placement · migration FSM     │      │ FluxVM lifecycle · DRA → VFIO │
│ CSI snapshots · admission webhook │      │        mTLS peer :9443        │
└───────────────────────────────────┘      └───────────────────────────────┘
                                                           │
                                                           ▼
                                     FluxVM local API · migration adapter (Unix socket) · KVM/VMM
```

Kubernetes is the only source of truth — there is no second database anywhere in this system, not even behind the dashboard. FluxVM owns actual VMM execution; Kairon owns placement, relocation policy, and Kubernetes lifecycle semantics. The one connection this diagram can't easily draw: when `webhook.enabled`, the API server calls back out to `kairon-controller`'s admission webhook *synchronously, during* every `Machine`/`MachineMigration` write — the arrow from the API box down to `kairon-controller` in a real deployment runs both ways, not just top-to-bottom.

## What each component owns

- **`kairon-controller`** — the cluster-wide reconciler. Owns placement (least-loaded, deterministic tie-break, required affinity/anti-affinity), the `MachineMigration` state machine (cold and live), `MachineSnapshot` → CSI `VolumeSnapshot` orchestration, `MachineQuota`/`MachineDisruptionBudget` status accounting, Prometheus metrics, and — opt-in — the `MachineQuota`/`MachineDisruptionBudget` admission webhook. Does not talk to FluxVM directly, and does not execute anything on a node. `controller.replicaCount > 1` is safe: a `coordination.k8s.io/v1` Lease (`internal/leaderelection`, on by default) ensures only the elected leader's reconcile loop runs at a time, while the admission webhook and health/metrics endpoints — stateless per-request reads — keep serving from every replica regardless. See [`docs/guides/kairon-controller-ha.md`](docs/guides/kairon-controller-ha.md).
- **`kairon-node`** — one per capable node. Owns turning an assigned `Machine` into a real FluxVM instance, resolving DRA `ResourceClaim`s to VFIO PCI BDFs against a node-local allowlist, serving the mTLS live-migration peer API, and — opt-in — the VNC console relay and its own TLS. Does not decide placement; it only executes what `kairon-controller` already assigned to it. When a Machine's boot disk is a CSI-backed `PersistentVolume`, it also acts as its own CSI client against `kairon-csi-node`'s local Unix socket, directly — see `kairon-csi-node` below.
- **`kairon-csi-node`** (optional) — one per capable node, deployed alongside `kairon-node`. Kairon's own first-cut CSI node plugin (`csi.kairon.zyvor.dev`, iSCSI and NVMe-oF; this binary has no Controller service — dynamic provisioning is the separate, opt-in `kairon-csi-controller`, `csiController.enabled`): logs in to an iSCSI target or connects an NVMe-oF subsystem, formats it if needed, and mounts it, all driven directly by `kairon-node` rather than through kubelet's Pod volume machinery (a `Machine` has no backing Pod for kubelet to trigger that against). The upstream `csi-node-driver-registrar` sidecar it ships with still registers it with kubelet, so a real Kubernetes Pod can also use it normally. Runs privileged, and is the one Kairon-built image not based on the minimal distroless base every other component uses — see SECURITY.md.
- **`kaironctl`** — a thin Kubernetes API client, nothing more. Every subcommand is a `GET`/`POST`/`PATCH` against the same CRDs anyone with `kubectl` could issue; it never bypasses `kairon-controller` or talks to a node directly.
- **`kairon-ui`** (optional) — a Go backend (`internal/uiapi`) plus a small React SPA, with exactly the same standing as `kaironctl`: another Kubernetes API client, no privileged side channel, no second source of truth. Its one genuinely separate piece of state is its own operator account list (`ui.auth.users`, a Kubernetes Secret) and session/lockout/console-ticket bookkeeping; `ui.replicaCount > 1` propagates that state across replicas via a shared Kubernetes `ConfigMap` rather than a new stateful dependency like Redis — eventually-consistent, not instant, see [`docs/guides/kairon-ui-ha.md`](docs/guides/kairon-ui-ha.md). Optional OIDC/SSO (`ui.oidc.enabled`) authenticates against an external identity provider instead of (or alongside) `ui.auth.users`, issuing the exact same kind of session token either way — see [`docs/guides/kairon-ui-oidc.md`](docs/guides/kairon-ui-oidc.md).
- **The migration adapter** — an optional, VMM-specific component (`cmd/kairon-migration-adapter-fluxvm` is the real implementation) that `kairon-node` talks to over a local Unix socket to actually move VM memory/state during a live migration. Cold migration needs none of this; a live migration request blocks before the source is even touched if no adapter is configured.
- **FluxVM** — not part of Kairon at all. It's the actual hypervisor-facing daemon (QEMU, Cloud Hypervisor, Firecracker, and Apple Virtualization.framework `vz` on a Mac, where `kairon-node` also registers the Mac as a Node; see [`docs/macos.md`](docs/macos.md)) that every `kairon-node` calls over its local REST API. Kairon never re-implements VMM control; it only orchestrates around it.

## From `kubectl apply` to a running VM

1. A `Machine` object lands in the Kubernetes API (via `kubectl`, `kaironctl create`, `kairon-ui`, or GitOps — all identical from here on).
2. If `webhook.enabled`, the API server calls `kairon-controller`'s admission webhook first: a `Machine` create that would immediately exceed a `MachineQuota` is rejected outright, before it's ever written — and so is an update that hotplug-resizes an already-scheduled Machine's `spec.resources` past quota, the one check here with no reconcile-loop equivalent backstopping it (`kairon-node`'s hotplug has no cluster-wide quota visibility of its own).
3. `kairon-controller`'s reconcile loop (on a fixed interval, not a watch — see the Go-stdlib-only note below) lists unscheduled Machines, filters nodes by required affinity/anti-affinity/architecture/selector, then scores the survivors (load balance, `preferredAffinity`/`preferredAntiAffinity`, `topologySpreadConstraints`, a best-effort DRA topology hint) and patches `spec.nodeName` to the winner — see [`docs/guides/machine-placement.md`](docs/guides/machine-placement.md).
4. The `kairon-node` running on that node notices the assignment on its own next tick and reconciles it: resolves the boot disk (`spec.volumes[0]`'s PVC, or `spec.image.path`), resolves any DRA device claims to VFIO BDFs, and calls FluxVM's local REST API to actually create and start the VM.
5. `kairon-node` patches `status.phase` back onto the `Machine` as it progresses — `kaironctl get machines` / the dashboard's Machines table are both just reading that same status field, not a separate poll of FluxVM.

Relocating an existing Machine (`kaironctl migrate`) and disrupting several at once (`kaironctl evacuate`, budget-checked against any matching `MachineDisruptionBudget`) both follow the same "write intent to the API, let the right component notice and act" shape — see [`docs/architecture.md`](docs/architecture.md)'s Cold migration/Live migration sections for the exact state diagrams.

## Beyond the core loop

Everything below is built from the same pieces — the API as the only store, `kairon-controller` deciding, `kairon-node` executing, FluxVM owning the VMM — and nothing adds a new stateful dependency.

- **A Mac as a Node.** A Mac has no kubelet, so `kairon-node` registers it as a Kubernetes `Node` itself (`internal/macnode`, on by default on macOS via `--register-node`) and refreshes it every 10 seconds: `kairon.zyvor.dev/capable` and `kairon.zyvor.dev/backend.vz` labels, allocatable `cpu` and `memory` (unified memory minus the larger of 3 GiB and a quarter, left to macOS), and the taint `kairon.zyvor.dev/vm-only:NoSchedule` so ordinary pods never land there. The controller schedules a Machine to it like any other node, and `kairon-node` drives it through FluxVM's Apple Virtualization.framework (`vz`) backend, which rejects with a specific message anything it cannot honour (port forwards, hotplug, VFIO, cdroms and so on). See [`docs/macos.md`](docs/macos.md).
- **Images and install media.** `MachineImage` is a cluster-scoped catalog entry, a boot disk (`kind: disk`) or an ISO (`kind: iso`). `kairon-controller` resolves `spec.image.imageRef` and `spec.cdroms[].imageRef` once, before scheduling, and pins the source and sha256 digest into the Machine, so later edits to the catalog change only Machines created afterwards. `spec.image.blank` boots an empty root disk; `spec.cdroms` attaches up to four ISOs read-only, and removing an entry makes `kairon-node` eject the medium (a VM with media in a CD-ROM cannot live-migrate). See [`docs/guides/machine-images.md`](docs/guides/machine-images.md) and [`docs/guides/machine-install-media.md`](docs/guides/machine-install-media.md).
- **Warm pools and claims.** A `MachinePool` keeps `spec.replicas` Machines built from its `spec.template` booted and unclaimed; a `MachineClaim` binds the oldest Running warm member with a `resourceVersion`-guarded label patch in one reconcile tick, then the pool boots a replacement. Because the claimed Machine is a member built from the pool template, the claim carries that whole template with it; the dashboard's FluxVM-side warm-pool claim (`createMachine: true`) maps the template onto a new `Machine` field by field and reports what it cannot carry. See [`docs/guides/machine-pools.md`](docs/guides/machine-pools.md).
- **Tenant fence.** With `kairon.zyvor.dev/tenant-fence: "true"` on a Machine, `kairon-controller` owns a `NetworkSecurityGroup` named `tenant-fence-<tenant>` whose `denyCidrs` are the other tenants' observed guest addresses in the namespace, and `kairon-node` merges that list into the `MachineNetworkPolicy` it already posts to FluxVM. It is east-west deny of observed addresses, not a VRF. See [`docs/guides/tenant-fence.md`](docs/guides/tenant-fence.md).
- **Preemption by Halt.** When placement fails, a pending Machine with `kairon.zyvor.dev/preempt: "true"` may halt one strictly lower-priority, same-namespace Machine that opted in with `kairon.zyvor.dev/preemption-policy: Halt`. Nothing is evicted or migrated; the controller records `preempted-by` and `preempted-power` on the victim and restores it when the preemptor is gone, halted, stopped or no longer pending. See [`docs/guides/preemption.md`](docs/guides/preemption.md).
- **Stale evacuation.** Off by default (`controller.staleEvacuation.enabled`). The same operator-attested fencing as `kaironctl fence`, automated: only a Machine annotated `kairon.zyvor.dev/evacuate=true` on a NotReady node annotated `kairon.zyvor.dev/node-fenced` (set by `kaironctl node fence`) is fenced and handed back to the scheduler, subject to disruption budgets and a per-tick cap. See [`docs/guides/machine-fencing.md`](docs/guides/machine-fencing.md).
- **MCP server.** `kaironctl mcp serve` is another API client over stdin/stdout. Writes need `--allow-write`, every write is recorded in a hash-chained audit log before and after it runs (and refused if the log cannot be written), and `delete_machine`, `fork_machine` and destructive `machine_backup` calls also need a human to run `kaironctl approve`, which writes a one-shot `kairon.zyvor.dev/mcp-approval` annotation. See [`docs/guides/hermes-mcp.md`](docs/guides/hermes-mcp.md) and [`docs/guides/agent-plane.md`](docs/guides/agent-plane.md).
- **VM edge.** Anti-spoof, learn-IP, QoS, DNS/SNI allow lists, attributed drops and conntrack that follows a live migration are enforced by FluxVM's TC/TCX program (`fluxvm_tc.bpf.o`). Kairon declares the edge through `Machine.spec.network` and `MachineNetworkPolicy`; it does not load BPF. See [`docs/ebpf-edge.md`](docs/ebpf-edge.md).
- **Fleet automation (experimental, opt-in).** Declarative resources in the `fleet.kairon.zyvor.dev/v1alpha1` group (HA, recovery plans, autoscaling, balancing, backup groups, virtual networks, import campaigns, templates, usage) over the existing Machine, migration, backup and pool primitives. Off by default; enabling it requires the admission webhook with `failurePolicy: Fail`. See [`docs/guides/enterprise-fleet.md`](docs/guides/enterprise-fleet.md).
- **Scoped UI authorization.** With `ui.auth.namespaceScoping.enabled` (off by default), `kairon-ui` restricts non-admin operators to namespaces listed in `ui.auth.users[].namespaces` or reachable through `ui.oidc.namespaceGroups`. In scoped mode unnamed sessions (the shared token) are denied, the overview is filtered, viewers cannot mutate, and an evacuation is authorized against every namespace in the batch before any migration starts. See [`docs/STATUS.md`](docs/STATUS.md) and [`docs/guides/enterprise-fleet.md`](docs/guides/enterprise-fleet.md).
- **Developer ecosystem kit.** `ecosystem/` (recipes, CI helpers, Terraform manifests, orphan cleanup, qualification tooling) and `sdk/` (dependency-free Python and TypeScript clients) sit entirely outside the control plane: they use the caller's Kubernetes RBAC and change no controller or runtime behavior. See [`docs/guides/developer-ecosystem.md`](docs/guides/developer-ecosystem.md).

## Trust boundaries and deployment topology

Two deployment shapes exist, and they change what "the network" means for the diagram above:

- **In-cluster (Helm)** — `kairon-controller`, `kairon-node` (as a DaemonSet), and optionally `kairon-ui` all run as Pods in one Kubernetes cluster, talking to the in-cluster API server over the normal service-account token path. This is the default, and what `charts/kairon/` assumes.
- **Bare-metal (`scripts/deploy-remote.sh`)** — the same binaries run as systemd services on a host outside Kubernetes entirely, pointed at a cluster via `KAIRON_KUBE_URL`. Useful for a node FluxVM already runs on that isn't itself a good place to run a full kubelet, or for trying Kairon out without standing up a cluster at all.

Three optional TLS hops exist, each independently opt-in, each following the same "you supply the certificate, this project doesn't mint one for you" convention (`migration.tlsSecretName`, `console.tls.secretName`, `webhook.tlsSecretName`/`webhook.caBundle`):

| Hop | Default | What it protects |
|---|---|---|
| `kairon-node` ↔ `kairon-node` (live migration peer, `:9443`) | mTLS **required** whenever `migration.enabled` | The actual VM memory/state transfer between two nodes |
| `kairon-ui` → `kairon-node` (VNC console relay) | plaintext HTTP, opt-in TLS (`console.tls.enabled`) | A shared bearer token plus, optionally, one-way TLS on the relay hop |
| Kubernetes API server → `kairon-controller` (admission webhook) | webhook itself is off by default; `failurePolicy: Fail` once on | Whether `Machine`/`MachineMigration` writes can bypass quota/budget checks |

All three hop's leaf certificates hot-reload (`internal/tlsreload`, a stdlib-only polling watcher — no fsnotify): whatever renews the cert file on disk (cert-manager or anything else) takes effect within 30s, no restart. Only the leaf cert/key reload this way; a CA bundle used to build `ClientCAs`/`RootCAs` still loads once at startup.

## Why it's built this way

Three constraints shape almost every design choice above — see the top-level [README](README.md#why-kairon) for the full argument, summarized here for context:

- **Go standard library only for the control plane, with named exceptions.** `go.mod` has no `client-go` on the controller/node path, no controller-runtime, no generated deepcopy. `kairon-controller` is a hand-rolled interval poll loop over a raw REST client (`internal/kube`), not a watch-based `Manager`/`Reconciler` — which is also why the admission webhook hand-rolls the small `AdmissionReview` JSON shape (`internal/admission`) instead of importing `k8s.io/api` for it. The tradeoff is explicit: no client-side caching or watch-based low-latency reconciliation, in exchange for a codebase small enough to actually read start to finish. Named exceptions (CLI Cobra + Helm SDK, UI OIDC, CSI) are listed in [`docs/DEPENDENCIES.md`](docs/DEPENDENCIES.md). Cilium objects are raw REST, not the Cilium Go SDK. `kairon-ui`'s opt-in OIDC/SSO (`ui.oidc.enabled`) uses `golang.org/x/oauth2` and `github.com/coreos/go-oidc/v3` — real JWT/JWK verification against an external identity provider isn't something to hand-roll. Kairon's own opt-in CSI node plugin, `kairon-csi-node` (`csiNode.enabled`), speaks the CSI gRPC/protobuf contract (`google.golang.org/grpc`, `github.com/container-storage-interface/spec`); `kairon-node` also links `grpc` to dial it as a client (`internal/agent/csi.go`), exercised only when a Machine actually uses a CSI-backed volume. `kairon-controller` stays stdlib-only.
- **Kubernetes is the only source of truth.** Every component above — including the dashboard — is a client of the same API, never a second store. There is no Kairon-side database to get out of sync with reality.
- **An ambiguous outcome gets a name, not a guess.** `NeedsRecovery` exists because a live migration whose commit result is genuinely uncertain has exactly two wrong automatic answers (assume success, assume failure) and one right one: stop, and ask an operator to attest what actually happened. Node fencing follows the same shape: `kairon-controller` names a Machine's node `NodeUnreachable` on its own, but only an operator-run `kaironctl fence`, attesting the node is truly gone (not just unreachable), makes it eligible for rescheduling — automatic rescheduling here risks the same split-brain double-run `NeedsRecovery` prevents for migrations.

## Going deeper

- [`docs/architecture.md`](docs/architecture.md) — exact FSM diagrams for cold/live migration, session durability, CSI/DRA mechanics, operational visibility
- [`docs/migration-adapter.md`](docs/migration-adapter.md) — the migration adapter's HTTP contract and trust boundary
- [`docs/network-fabric.md`](docs/network-fabric.md) — `MachineNetworkPolicy`/`NetworkSecurityGroup` and the FluxVM eBPF edge
- [`docs/tutorials/machine-lifecycle.md`](docs/tutorials/machine-lifecycle.md) — a hands-on walkthrough of the create → relocate → snapshot → restore flow described above
- [`docs/guides/machine-fencing.md`](docs/guides/machine-fencing.md) — `NodeUnreachable` detection, `kaironctl fence`'s safety model, storage/network migration preflight labels
- [`docs/guides/kairon-ui-ha.md`](docs/guides/kairon-ui-ha.md) — running `ui.replicaCount > 1`, what's shared and how
- [`docs/guides/kairon-controller-ha.md`](docs/guides/kairon-controller-ha.md) — running `controller.replicaCount > 1`, Lease-based leader election
- [`docs/guides/observability.md`](docs/guides/observability.md) — what each component's `/metrics` exposes, and the alert rules
- [`docs/runbook-backup-restore.md`](docs/runbook-backup-restore.md) — backing up/restoring Kairon's CRD state, and what it doesn't cover
- [`docs/guides/kairon-ui-oidc.md`](docs/guides/kairon-ui-oidc.md) — OIDC/SSO setup and the Go-stdlib-only exception it is
- [`docs/guides/machine-storage.md`](docs/guides/machine-storage.md) · [`docs/guides/machine-storage-csi.md`](docs/guides/machine-storage-csi.md) — PVC-backed boot disks, and Kairon's own first-cut iSCSI CSI driver
- [`docs/macos.md`](docs/macos.md) — a Mac as a Node, the `vz` backend, what is verified and what is not
- [`docs/guides/machine-images.md`](docs/guides/machine-images.md) · [`docs/guides/machine-install-media.md`](docs/guides/machine-install-media.md) — the `MachineImage` catalog, ISO install media and eject
- [`docs/guides/machine-pools.md`](docs/guides/machine-pools.md) · [`docs/guides/tenant-fence.md`](docs/guides/tenant-fence.md) · [`docs/guides/preemption.md`](docs/guides/preemption.md) — warm pools and claims, the tenant fence, preemption by Halt
- [`docs/guides/hermes-mcp.md`](docs/guides/hermes-mcp.md) — the MCP server, human approval and the audit log
- [`docs/guides/enterprise-fleet.md`](docs/guides/enterprise-fleet.md) · [`docs/guides/developer-ecosystem.md`](docs/guides/developer-ecosystem.md) — experimental fleet automation; SDKs and the integration kit
- [SECURITY.md](SECURITY.md) — the full threat model behind every TLS hop and trust boundary mentioned here
