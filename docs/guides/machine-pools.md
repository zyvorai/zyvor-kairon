# MachinePool and MachineClaim

A `MachinePool` keeps `spec.replicas` Machines booted and unclaimed. A
`MachineClaim` takes one of them over. The bind is a label change on an
already-Running Machine, so it completes in one kairon-controller reconcile
tick instead of a cold boot. The pool then boots a replacement.

Use it for agent sandboxes, CI runners, or anything that needs a VM now and
can tolerate it having been booted a few minutes earlier.

## Example

```bash
kubectl apply -f examples/machinepool.yaml      # pool "agents" + claim "job-42"
kaironctl get machinepools
kaironctl claim agents --label team=ml --ttl 1h
kaironctl get machineclaims
```

`kaironctl claim` creates the claim, waits for it to bind (`--wait`, default
60s) and prints the Machine name and bind time:

```text
machineclaim/agents-claim-3fa91c bound to machine/agents-8c21d0e4 in 412ms
```

## How it works

Pool members carry these labels:

| Label | Value |
| --- | --- |
| `kairon.zyvor.dev/machinepool` | pool name |
| `kairon.zyvor.dev/machinepool-template-hash` | hash of `spec.template` |
| `kairon.zyvor.dev/pool-state` | `warm` or `claimed` |
| `kairon.zyvor.dev/machineclaim` | claim name, once claimed |

Each tick, kairon-controller:

1. Binds pending claims, oldest first, to the oldest Running `warm` member of
   their pool. The label patch carries the Machine's `resourceVersion`, so a
   member changed since the listing is skipped (409) instead of being bound
   twice. `spec.labels` from the claim are added to the Machine.
2. Brings each pool to `spec.replicas` warm members. Claimed members don't
   count, so a claim triggers a replacement in the same tick. Warm members
   built from an older template are deleted and recreated at once (nobody is
   using them). Surplus members are trimmed, booting ones first.

A claim with no Running warm member stays `Pending` with a message like
`no Running warm machine in pool "agents" (2 warming)` and binds as soon as one
is ready.

With `spec.warmPowerState: Paused` (experimental), a member runs for
`spec.warmupSeconds` (default 30) and is then paused, so the pool holds memory
but no CPU. A Paused member counts as warm, and binding resumes it in
milliseconds. See [Agent sandboxes](agent-sandboxes.md#paused-warm-pools).

## Claim lifecycle

| Field | Effect |
| --- | --- |
| `spec.poolName` | Pool in the claim's namespace. Immutable. |
| `spec.labels` | Added to the Machine at bind. |
| `spec.reclaimPolicy` | `Delete` (default): deleting the claim deletes the Machine. `Retain`: the pool and claim labels are removed and the Machine is kept as a plain Machine. |
| `spec.ttlSeconds` | Deletes the claim this long after it binds, which releases the Machine per `reclaimPolicy`. |
| `spec.egress` | Egress allowlist for the claimed Machine (see below). |
| `status.phase` | `Pending`, `Bound`, or `Lost` (the Machine was deleted out from under the claim). |
| `status.bindMillis` | Claim creation to bind, in milliseconds. |
| `status.egressPolicy` | Name of the MachineNetworkPolicy enforcing `spec.egress`. |
| `status.usage` | Active and suspended seconds, vCPU and GiB-memory seconds accrued while bound (experimental, see [Agent sandboxes](agent-sandboxes.md#claim-usage)). A `Released` Event keeps the final figures. |

## Per-claim egress

`spec.egress` confines the claimed Machine to the listed destinations for as
long as the claim is bound. Everything else is dropped at the VM's edge.

```yaml
spec:
  poolName: agents
  egress:
    allowFqdns: [pypi.org, files.pythonhosted.org]
    allowPorts: ["443"]
```

```bash
kaironctl claim agents --allow-fqdn pypi.org --allow-fqdn files.pythonhosted.org --allow-port 443
```

At bind, kairon-controller creates a default-deny `MachineNetworkPolicy`
named `claim-egress-<claim>`, targeting the Machine by name, with the fields
below. Editing `spec.egress` updates the policy and removing it deletes the
policy. Releasing the claim deletes the policy before the Machine is deleted
or handed back.

| Field | Meaning |
| --- | --- |
| `allowFqdns` | Hostnames the guest may resolve and reach. |
| `allowSNI` | TLS server names allowed, exact or `*.suffix`. |
| `allowCidrs` | Destination CIDRs allowed. |
| `allowPorts` | Destination ports or ranges (`443`, `8000-8100`). |
| `allowDNS` | DNS query names allowed; empty falls back to `allowFqdns`. |
| `allowIcmp` | Allow ICMP. |

Deleting a pool deletes its warm members only. Claimed Machines belong to
their claims and are left running.

## Scaling

`MachinePool` and `MachineSet` both have the `scale` subresource, so these
work:

```bash
kubectl scale machinepool agents --replicas 10
kubectl scale machineset web --replicas 5
kaironctl scale machinepool agents --replicas 10
```

A `HorizontalPodAutoscaler` (or KEDA) can target either kind through the
same subresource.

## AI agents

`kaironctl mcp serve --allow-write` exposes `claim_machine`, `release_claim`,
`create_sealed_claim` (validated tenant, TTL and egress allowlist),
`apply_claim_step` (expires a claim past its TTL) and `delete_machine`, each
recorded in the audit log. `list_machine_pools`, `list_claims` and
`describe_claim` are read-only. See [hermes-mcp.md](hermes-mcp.md) and
[agent-plane.md](agent-plane.md).

## Limits

- Readiness is `status.phase == Running`, not a guest-agent heartbeat.
- Members boot through the normal Machine path (scheduler, quota, webhook),
  so a pool counts against `MachineQuota` for every warm member.
- FluxVM's node-local warm pools (`/v1/pools/{name}/claim`) are a separate,
  lower-level mechanism; a MachinePool works with any backend.
