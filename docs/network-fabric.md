---
sidebar_position: 7
title: Network Fabric
---

# Network Fabric (Kairon ↔ FluxVM ↔ Fabric)

Kairon declares Kubernetes desired state for VM-edge networking. **FluxVM** owns
TAP/netns, TC/eBPF attach, maps, CNP compile, and Service Fabric VIPs.
**Fabric** owns UX/SDN labels and proxies FluxVM dataplane APIs. Kairon does
**not** own BPF programs or PacketWolf. Multus NAD remains non-primary; the
supported path onto the cluster Cilium network is **Cilium ExternalWorkload**
(`spec.network.ciliumAttach`), not Multus as the primary attach.

```mermaid
flowchart TB
  subgraph consumers [Consumers]
    Fabric[zyvor_fabric]
    Kubectl[kubectl_GitOps]
  end
  subgraph kairon [Kairon_API]
    Machine[Machine]
    NetPolicy[MachineNetworkPolicy]
    Groups[NetworkSecurityGroup]
  end
  subgraph node [Node]
    Agent[kairon_node]
    FluxVM[FluxVM_REST]
    TC[TC_eBPF_maps]
  end
  subgraph cilium [Cluster_Cilium]
    CEW[CiliumExternalWorkload]
    CNP[CiliumNetworkPolicy]
  end
  Fabric --> Machine
  Fabric --> NetPolicy
  Kubectl --> Machine
  Machine --> Agent
  NetPolicy --> Agent
  Agent -->|"POST /v1/vms create network"| FluxVM
  Agent -->|"POST .../network/policy"| FluxVM
  Agent -->|"migration quiesce/export/restore"| FluxVM
  FluxVM --> TC
  Machine -.->|ciliumAttach| CEW
  NetPolicy -.->|cilium.sync| CNP
```

## Field → FluxVM → Fabric mapping

| Kairon | FluxVM | Fabric proxy |
|---|---|---|
| `Machine.spec.network.{mode,netns,bridge,parent,mac,tapName,macvtapMode,forwards,staticNetwork,podUID,dataplaneMode}` | `POST /v1/vms` `network` (+ `dataplane_mode`) + `cloud_init.static_network` + `pod_uid` | VM create / edit |
| `Machine.spec.network.dataplaneRequired` | fail-closed on `GET …/network/status` when attach unhealthy | Dataplane health |
| `Machine.spec.network.ciliumAttach` | controller reconciles `CiliumExternalWorkload`; `podUID` from CEW UID | Cilium identity/IPAM |
| `Machine.spec.serviceFabric.services[]` | merge backend into `POST /v1/network/services` | Service Fabric membership |
| `Machine.status.network.{guestIP,tapName,dataplane.*,cilium.*}` | `guest_ip` / `GET …/network/status` / CEW status | Dataplane tab |
| `MachineNetworkPolicy` | `POST /v1/vms/{id}/network/policy` (+ optional `POST /v1/network/cnp`) | label→policy |
| `MachineNetworkPolicy.spec.cilium.sync` | controller upserts namespaced `CiliumNetworkPolicy` | Hubble/CNP visibility |
| `NetworkSecurityGroup` | `POST /v1/network/groups` | security groups |
| `Machine.spec.network.{antiSpoof,learnIP,qos}` + policy `allowSNI`/`allowDNS`/`maxIngress*` | `POST /v1/vms/{id}/network/edge` (schema 12 VM edge) | — |
| `Machine.status.network.edge.*` | edge identity, `GET …/network/learned-ip`, conntrack restore result | — |
| `kaironctl network capture` / `captures` | `POST`/`GET …/network/capture`, `GET …/network/capture/{token}` (tcpdump pcap) | — |
| live migrate network | `…/network/migration/{quiesce,export,restore,resume}` + `GET`/`POST …/network/conntrack` | cross-node CT/policy continuity |
| observability (pass-through) | `…/network/{stats,flows,drop-reasons}` | `/api/dataplane/hubble/flows` |

## Cilium enablement

All cluster-side Cilium features are **off by default** (bare-metal / non-Cilium clusters unchanged):

| Helm value | Effect |
|---|---|
| `network.ciliumDataplane` | Documentation-only flag; set FluxVM `/etc/fluxvm.toml` `sandbox.dataplane.mode = "cilium"` on each node |
| `network.ciliumAttach.enabled` | Controller flag `--cilium-attach`; RBAC for `ciliumexternalworkloads` |
| `network.ciliumPolicySync.enabled` | Controller flag `--cilium-policy-sync`; RBAC for `ciliumnetworkpolicies` |

Per-Machine: `spec.network.dataplaneMode: cilium` (with `dataplaneRequired: true` to fail closed), and optionally `ciliumAttach: true` (requires `mode: tap` + `netns: true`). Per-policy: `spec.cilium.sync: true`.

CLI: `kaironctl network status MACHINE` (emoji dataplane / CEW / CNP lines); `--flows` / `--drop-reasons` use the existing uiapi pass-through when `KAIRON_UI_URL` is set.

## Agent behavior

1. **Create** — map rich `spec.network` into FluxVM create payload (Fabric create parity), including `dataplane_mode` when set. With `ciliumAttach`, waits for controller-projected `podUID` before create.
2. **Status** — project guest IP + dataplane attach fields Fabric already expects; preserve `status.network.cilium` written by the controller.
3. **Policy** — when Machine is Running on this node and selected by `machineName` or labels, upsert FluxVM policy; on CR delete, reset every currently-selected Machine to `default_allow: true`. This reset also fails closed: the finalizer only clears once every reset `POST` actually succeeds (idempotently tolerating a selected Machine's VM already being gone), so a real reset failure leaves the `MachineNetworkPolicy` (and its finalizer) in place for a retry on the next tick, rather than the object silently vanishing while a selected VM keeps running under its now-stale restriction.
4. **Groups** — upsert node-local FluxVM security groups from `NetworkSecurityGroup`. Deletion fails closed: the finalizer only clears once `DELETE /v1/network/groups/{name}` actually succeeds (idempotently tolerating "already gone"), so a real delete failure -- FluxVM unreachable, a transient error -- leaves the object (and the finalizer) in place for a retry on the next tick, rather than the Kubernetes object silently vanishing while its FluxVM-side security group state leaks behind, untracked.
5. **Migration** — source quiesce+export before transfer; target restore after prepare; resume after commit (mTLS peer carries opaque snapshot, not CRD status).
6. **Service Fabric** — after guest IP is known, register Machine as backend of named VIPs; `status.appliedServiceFabricMemberships` is Kairon's own ground-truth record of exactly which `(service, port, guestIP)` triples it has actually registered so far, since FluxVM's Service Fabric has no "list what I applied" query of its own. Every reconcile tick while Running also prunes any *previously*-applied entry that this tick's fresh registration didn't reapply — closing the two ways a stale backend could otherwise be left registered on an otherwise still-live Machine: a `spec.serviceFabric.services[]` entry edited out of spec, or (the more insidious one, since it needs no spec edit at all) `guestIP` itself moving to a new address — a DHCP re-lease, or a guest reboot landing on a different lease — which used to just add a second backend for the new address and leave the stale one at the old address registered forever, silently routing VIP traffic nowhere, or — once that old address gets DHCP-reused — at a completely unrelated Machine. On top of that, Machine deletion, `spec.powerState: Stopped`, and `spec.powerState: Halted` deregister every entry in `status.appliedServiceFabricMemberships` before completing (finalizer removal for delete; the status patch to `Stopped`/`Halted` otherwise), covering the case where the guest goes away entirely rather than just moving. Both paths fail closed — a real deregistration error (FluxVM unreachable, a transient error) blocks that completion/tick and gets retried next time, rather than leaving a dead or misattributed backend registered against a VIP indefinitely. A VIP that's already gone is tolerated as nothing left to deregister from.

## Examples

See [`examples/network-fabric-machine.yaml`](../examples/network-fabric-machine.yaml).

## Docs

- Tutorial: [tutorials/network-fabric.md](tutorials/network-fabric.md)
- User guide — Machine networking: [guides/machine-network.md](guides/machine-network.md)
- User guide — policies & groups: [guides/network-policy.md](guides/network-policy.md)
- VM edge (anti-spoof, learn-IP, QoS, SNI/DNS, drops, conntrack move): [ebpf-edge.md](ebpf-edge.md)

## Non-goals

- Replacing Fabric host nftables SDN (`/api/network-policies`)
- Owning BPF programs inside Kairon
- Multus NAD as the primary attach path (Cilium ExternalWorkload is the supported cluster-network attach)
- Full Hubble UI embed (keep uiapi / `kaironctl network status --flows` pass-through only)
- PacketWolf control plane in Kairon
