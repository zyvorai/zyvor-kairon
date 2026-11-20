# User guide: MachineNetworkPolicy and NetworkSecurityGroup

Declarative VM-edge policy for Machines. The node agent applies objects only
for Machines scheduled on its node. Enforcement runs in FluxVM (TC/eBPF), not
in Kairon.

## Concepts

| CRD | FluxVM route | Purpose |
|---|---|---|
| `NetworkSecurityGroup` | `POST /v1/network/groups` | Named reusable group (+ labels/priority) |
| `MachineNetworkPolicy` | `POST /v1/vms/{id}/network/policy` | Per-VM (or label-selected) edge policy |
| `spec.cnp` on policy (optional) | `POST /v1/network/cnp` | CNP-shaped document compiled by FluxVM |

This does **not** replace Fabric’s host nftables SDN (`/api/network-policies`).
It also does **not** own BPF. On a Cilium cluster, an opt-in path can copy
the same intent onto a real `CiliumNetworkPolicy` (below).

## Sync onto Cilium (`spec.cilium.sync`)

Off unless both of these are set:

- Helm `network.ciliumPolicySync.enabled=true` (controller flag `--cilium-policy-sync`)
- `spec.cilium.sync: true` on this policy

```yaml
spec:
  selector:
    app: web
  policy:
    defaultAllow: false
    allowCidrs: ["10.0.0.0/8"]
    allowPorts: ["tcp/443"]
  cilium:
    sync: true
    policyName: web-edge   # optional; defaults to metadata.name
```

The controller upserts a namespaced `CiliumNetworkPolicy` targeting the
policy selector (or `kairon.zyvor.dev/machine-name` when `machineName` is
set). If `spec.cnp` is set, that document is preferred as the CNP body.
FluxVM edge enforcement is unchanged. Status lands in
`status.ciliumNetworkPolicyRef` and `status.ciliumSyncMessage`. Turning
sync off, or deleting the policy, deletes the CNP.

`kaironctl network status MACHINE` shows whether a matching policy synced.
See [network-fabric.md](../network-fabric.md).

## NetworkSecurityGroup

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: NetworkSecurityGroup
metadata:
  name: frontend
  namespace: default
spec:
  groupName: frontend          # defaults to metadata.name
  labels:
    - tier=frontend            # key=value strings FluxVM matches
  priority: 100                # lower wins on rate/deny ties
  description: HTTPS egress
  policy:
    defaultAllow: false
    allowCidrs: ["10.0.0.0/8"]
    allowPorts: ["tcp/443", "udp/53"]
    allowIcmp: true
```

Status:

- `phase: Applied` — upserted on this node’s FluxVM
- `identity` — dataplane group identity
- `appliedOn` — node name that last wrote the group

On delete, the agent removes the FluxVM group (finalizer
`kairon.zyvor.dev/network-group`).

## MachineNetworkPolicy

Select Machines by name **or** labels (same namespace):

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineNetworkPolicy
metadata:
  name: web-edge
  namespace: default
spec:
  # machineName: web          # optional; wins over selector
  selector:
    app: web
  policy:
    defaultAllow: false
    allowCidrs:
      - 10.0.0.0/8
      - 2001:db8:20::/48
    denyCidrs: []
    allowPorts: ["tcp/443", "udp/53"]
    groups: ["frontend"]
    labels: ["tier=frontend"]
    allowFqdns: []
    maxEgressMbps: 250
    maxEgressPps: 100000
    auditMode: false
    allowIcmp: true
    sampleRate: 0
```

### Policy fields (FluxVM `VmNetworkPolicy`)

| Field | Effect |
|---|---|
| `defaultAllow` | Action when allowlists are empty |
| `allowCidrs` / `denyCidrs` | Destination CIDRs (IPv6 needs eBPF mode) |
| `allowPorts` | `tcp/443`, `udp/53`, … — AND with CIDRs if both set |
| `maxEgressMbps` / `maxEgressPps` | Fixed-window egress ceilings (eBPF) |
| `groups` / `labels` | Security-group membership |
| `allowFqdns` | FQDNs resolved at apply time |
| `auditMode` | Log-and-allow instead of drop |
| `allowIcmp` | Permit ICMP/ICMPv6 through L4 checks |
| `sampleRate` | Allow-event sampling (0 = off) |
| `allowSNI` | TLS SNI allow list (`example.com` or `*.example.com`); VM edge only |
| `allowDNS` | DNS query-name allow list, same syntax; falls back to `allowFqdns` when empty; VM edge only |
| `maxIngressMbps` / `maxIngressPps` | Network-to-guest limits; VM edge only |

### SNI, DNS and ingress limits (VM edge)

`allowSNI`, `allowDNS` and `maxIngress*` are not part of the FluxVM VM
policy. `kairon-node` copies them into the per-Machine VM edge, which
FluxVM enforces in the same TC program:

```yaml
spec:
  selector:
    app: web
  policy:
    defaultAllow: true
    allowSNI: ["example.com", "*.example.com"]
    allowDNS: ["example.com", "*.example.com"]
    maxIngressMbps: 100
```

- They apply to every selected `mode: tap` Machine. Setting any of them
  turns the VM edge on for that Machine even when its `spec.network` has
  no edge field (`antiSpoof`, `learnIP`, `qos`, `dataplaneMode: ebpf`),
  including `dataplaneMode: cilium` Machines. A policy-only edge is
  best effort: if FluxVM rejects it, the node logs a warning.
- `*.example.com` does not match `example.com`; list both.
- DNS is checked on queries to port 53 and SNI on TLS ClientHellos to
  TCP 443. DNS over HTTPS, QUIC and other ports are not inspected; pair
  the lists with `allowPorts` / `allowCidrs` to close those paths.
- Denials appear as `dns_deny` / `sni_deny` in `kaironctl network
  drops`. With `auditMode: true` they are recorded but allowed.
- When a Machine selects more than one policy, the first one listed is
  merged into the edge.

Details: [ebpf-edge.md](../ebpf-edge.md).

### Optional CNP body

```yaml
spec:
  cnp:
    metadata: { name: web-cnp }
    spec:
      # FluxVM CiliumNetworkPolicy-shaped document
```

Posted to `/v1/network/cnp` before the VM policy upsert.

### Status

| Field | Meaning |
|---|---|
| `phase` | `Applied` / `Error` |
| `observedMachines` | Running local Machines that received the policy |
| `effectiveSynced` | At least one Machine was updated *and* a read-back of `GET /v1/vms/{id}/network/policy` against every applied Machine matches what was sent -- a real confirmation, not just "the write call returned success". A FluxVM that silently normalizes or partially rejects part of the request now shows up as `false` here instead of a false `true`. |
| `lastAppliedTime` | Last successful apply |
| `appliedMachines` | Ground truth of which Machine names this policy actually pushed `spec.policy` onto as of the most recent successful reconcile -- not just which Machines currently match `spec.selector`/`machineName`. Diffed every tick against what currently matches, so a Machine edited out of scope gets caught even while it keeps running (see below); not meant to be read directly, but present since `status` is one object and this is what drives that pruning. |

On delete, matched Running Machines on this node are reset to
`defaultAllow: true` (finalizer `kairon.zyvor.dev/network-policy`).

The same reset also fires *without* a delete: editing `spec.selector` (or
`spec.machineName`), or relabeling a Machine, so a previously-matched
Machine no longer matches, resets that Machine's FluxVM-side policy to
`defaultAllow: true` on the very next reconcile tick -- it is not left
running under its old, now-orphaned restriction until someone eventually
deletes the whole `MachineNetworkPolicy` (or stops/halts that one Machine)
to trigger a reset. `status.appliedMachines` above is what makes this
possible: it is the only record of which Machines this policy actually
touched, since `spec.selector` alone can no longer answer "does this
policy still claim this Machine" for one that just fell out of it. If a
*different*, still-current `MachineNetworkPolicy` also claims the same
Machine, the reset is skipped -- that other policy's own apply (already
run, or due later the same tick) is left to decide the Machine's actual
policy, so overlapping policies during a migration between them can never
race a reset against a real apply.

## Admission webhook (`webhook.enabled`)

Neither CRD's `spec.policy` was ever validated at write time until now --
`allowCidrs`/`denyCidrs`/`allowPorts` are free-form strings, and the only
thing that ever checked their syntax was FluxVM's own eBPF dataplane
(`validate_policy` in `crates/fluxvm-network/src/ebpf.rs`), called only
once the agent above actually tries to apply the policy to a real
Machine or upsert a group. A typo -- `10.0.0.0` with no `/prefix`,
`http/443` instead of `tcp/443`, `maxEgressMbps: 0` -- sailed straight
through `kubectl apply` and only ever surfaced as `status.phase: Error`,
retried forever on every subsequent reconcile tick (see "Reconcile
rules" below) since the same malformed spec is reapplied unchanged each
time -- a mistake with no path to self-heal.

An opt-in validating admission webhook on `kairon-controller`
(`webhook.enabled`, off by default -- see the Helm chart's `webhook`
values and `SECURITY.md`) now rejects a malformed `MachineNetworkPolicy`
or `NetworkSecurityGroup` outright, on both `CREATE` and `UPDATE`
(`kubectl edit` on either CRD hits this the same as a fresh apply), with
a specific message pointing at the bad field instead of a generic
"webhook denied." The check
(`model.ValidateVmNetworkPolicy`, `internal/model/network.go`)
deliberately mirrors FluxVM's own `validate_policy` grammar exactly --
same CIDR `/prefix` requirement, same `tcp`/`udp`/`sctp`/`icmp`/`icmp6`
protocol set, same `maxEgressMbps`/`maxEgressPps` "greater than zero
when set" rule -- reimplemented in Go rather than shared, since Kairon
has no dependency on FluxVM's Rust crates.

With `webhook.enabled` false (the default), nothing changes: a malformed
policy still isn't caught until an agent tries to apply it, same as
always.

## Reconcile rules

1. Machine must be on this node (`spec.nodeName`).
2. Machine `status.phase` must be `Running` with a `runtimeID`.
3. Selector / `machineName` must match.
4. If `Machine.spec.network.dataplaneRequired` and attach is unhealthy, Machine
   reconcile errors (policy may still attempt apply independently).

## Tenant fence (`kairon.zyvor.dev/tenant-fence`)

A Machine with `spec.tenant` and `kairon.zyvor.dev/tenant-fence: "true"`
gets a controller-owned `NetworkSecurityGroup/tenant-fence-<tenant>` that
denies other tenants' observed guest addresses in the same namespace.
kairon-node merges that deny list into the policy it posts, without
replacing user policies. See [`tenant-fence.md`](tenant-fence.md).

## Opt-in default-deny for unmatched Machines (`node.networkDefaultDeny`)

A Machine matched by zero `MachineNetworkPolicy`/`NetworkSecurityGroup`
objects is never touched by the reconcile rules above at all — it silently
keeps FluxVM's own native default, `defaultAllow: true`. Since these CRDs'
selectors are pure L3/L4 CIDR/port matching with zero namespace-awareness,
the practical effect on a cluster with no policies written yet is that any
Machine in any namespace can reach any other Machine in any other
namespace by default, cluster-wide, until someone writes a policy for it.

`node.networkDefaultDeny` (Helm value; `kairon-node -network-default-deny`
flag; `Agent.NetworkDefaultDeny` in code) closes that specific gap, opt-in,
off by default:

```yaml
node:
  networkDefaultDeny: true
```

Once enabled, every reconcile tick, after every current
`MachineNetworkPolicy` has already applied its own `spec.policy` to the
Machines it selects, `kairon-node` pushes `defaultAllow: false` to every
*other* Machine on that node — one currently matched by nothing at all.
A Machine that later starts matching a real policy is simply left alone by
this pass from then on; that policy's own apply is what actually governs
it, so there's never a double-push or flicker between the synthetic deny
and a real policy.

**This is a coarse, global toggle, not namespace-aware isolation.** It
does not make policy matching itself namespace-aware — a
`MachineNetworkPolicy` selector still can't reference namespace at all,
same as before. Turning this on requires a policy for same-namespace
traffic too, not just cross-namespace traffic: once enabled, *any* Machine
with no matching policy gets cut off from everything, including Machines
in its own namespace it may have been relying on reaching by default.

**This is explicitly disruptive if flipped on blind.** Enabling
`node.networkDefaultDeny` on an existing cluster with no
`MachineNetworkPolicy` objects written yet cuts all VM-to-VM connectivity
on every node it's enabled on, immediately, on the very next reconcile
tick — the same posture `webhook.enabled` already has for admission, and
why this defaults to `false`. Write and verify the policies you need
*first*, then enable this toggle, not the other way around.

Like every other FluxVM-side push in this file, a `SetVMNetworkPolicy`
failure here fails closed: it's logged and left for the next reconcile
tick to retry (the "matched by nothing" condition that triggered it is
still true then) — it never marks anything as having succeeded, and one
Machine's push failure doesn't block the same pass from continuing on to
other Machines. There's no new status field for this either:
`status.network.dataplane.policyFingerprint`/`.policySynced`, already
patched by the existing status projection, reflect whatever's actually
applied to a Machine, default-deny included.

## Inspecting policy objects

`kaironctl get networkpolicies` / `kaironctl get securitygroups` list every
`MachineNetworkPolicy`/`NetworkSecurityGroup` in a namespace (aliases:
`networkpolicy`/`machinenetworkpolicies`, `securitygroup`/
`networksecuritygroups`); `kaironctl describe networkpolicy NAME` /
`describe securitygroup NAME` and `kaironctl delete ...` round it out —
these two CRDs had no `kaironctl` support at all until a prior session
added get/describe/delete; create/edit came later still (this section).

`kaironctl create networkpolicy NAME (--machine-name X | --selector k=v)
[--allow-cidr CIDR] [--deny-cidr CIDR] [--allow-port proto/port]
[--allow-fqdn FQDN] [--policy-group NAME] [--policy-label k=v]
[--entity NAME] [--default-allow] [--audit-mode] [--allow-icmp]
[--max-egress-mbps N] [--max-egress-pps N] [--sample-rate N]` and
`kaironctl create securitygroup NAME [--group-name X] [--group-label k=v]
[--priority N] [--description TEXT] [same policy flags as above]` round out
the create side (`spec.cnp`'s free-form CiliumNetworkPolicy-shaped document
stays kubectl/YAML-only — no sensible flag shape for an arbitrary nested
JSON document). `create networkpolicy` refuses to create a policy that
targets no Machine at all: at least one of `--machine-name`/`--selector` is
required, the same "don't create an object that provably does nothing"
check `kaironctl create quota` already applies to its own dimensions.

`kaironctl edit networkpolicy NAME [--machine-name X] [--selector k=v]
[--allow-cidr CIDR] [--deny-cidr CIDR] [--allow-port proto/port]
[--default-allow BOOL] [--audit-mode BOOL] [--max-egress-mbps N]
[--max-egress-pps N]` and `kaironctl edit securitygroup NAME
[--group-label k=v] [--priority N] [--description TEXT] [same policy flags
as above]` patch only the fields an explicit flag was passed for, same
merge-patch convention as `edit quota`/`edit budget`. This is deliberately
narrower than `create`: `--allow-fqdn`/`--policy-group`/`--policy-label`/
`--entity`/`--allow-icmp`/`--sample-rate` stay create-time-only through
`edit` for a first cut, the same way `edit machine` only ever exposed
`--priority` out of every `Machine.spec` field. The dashboard's **Network
policies** and **Security groups** pages give the same read-only view and
stay list-only for now — `kaironctl`/`kubectl` remain the only way to
create or edit either CRD.

## Troubleshooting: why is traffic being allowed/blocked?

Five read-only, any-authenticated-operator API endpoints (also on the
dashboard **Network** panel and `kaironctl network
flows|drop-reasons|stats|effective`) answer "what is actually happening,"
as opposed to "what was configured":

- **`GET /api/v1/machines/{ns}/{name}/network-effective`** -- the
  Machine's fully-resolved effective policy, *after*
  `NetworkSecurityGroup`/label merging -- what's actually enforced right
  now, not just what the last `MachineNetworkPolicy` apply sent.
- **`GET .../network-drop-reasons?limit=N`** -- why the eBPF dataplane
  most recently dropped packets for this Machine. The single most direct
  answer to "why is my policy blocking traffic I expected to allow" --
  start here before re-reading your own policy YAML.
- **`GET .../network-flows?limit=N`** -- the Machine's most recent
  eBPF-observed network flows (allowed and denied).
- **`GET .../network-stats`** -- real, eBPF-dataplane-derived byte/packet
  counters for the Machine.
- **`GET .../network-drops?limit=N`** (`kaironctl network drops`) --
  VM-edge drops named with Kairon's reasons (`spoof_ip`, `dns_deny`,
  `sni_deny`, `rate_limit`, `policy_deny`, ...), each with the policy
  name, flow and packet count. See [ebpf-edge.md](../ebpf-edge.md#drops).

These need the console/diagnostics relay (`KAIRON_NODE_CONSOLE_TOKEN` on
both `kairon-ui` and `kairon-node`). Flows / drops / stats also need
FluxVM's eBPF dataplane on the Machine (`mode: tap`, `netns: true`,
`dataplaneMode: ebpf`); a `network.mode: user` Machine can still return
`network-effective`, but the other three typically 502 from the node
relay.

All five are raw JSON passthroughs of FluxVM's own response (no fixed
Kairon-side schema) -- FluxVM's own handlers return dynamic, evolving
shapes here rather than a versioned struct.

**Deliberately not wrapped**, discovered during the same FluxVM route
audit that added the four endpoints above: FluxVM's own Cilium-style
network dataplane exposes a much larger surface
(`/v1/network/cnp`, `/v1/network/identities`, `/v1/network/observe`,
`/v1/network/health`, `/v1/network/ipcache*`, `/v1/network/hubble/*`,
`/v1/network/services/*` health/stats/telemetry/conntrack-export-import-
delta-ack, per-service L7 Envoy contracts). Most of this is either
cluster-mesh-style internal node-to-node coordination machinery (conntrack
delta/ack, telemetry export, ipcache federation) FluxVM's own Fabric
integration uses internally, not something an operator calls directly, or
a substantial standalone observability product in its own right (Hubble's
full flow-observability UI) that would need its own dedicated design pass
rather than a quick wrap alongside four smaller diagnostics. Not
implementing these for now; the endpoints above already cover the
concrete "why isn't my policy working" question this section exists to
answer.

## RBAC

`kairon-node` and `kairon-controller` ClusterRoles include
`machinenetworkpolicies` and `networksecuritygroups` (get/list/watch/patch).

## Tutorial

Step-by-step: [tutorials/network-fabric.md](../tutorials/network-fabric.md).
