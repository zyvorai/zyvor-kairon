# User guide: running more than one kairon-controller replica

`controller.replicaCount` (Helm chart, default `1`) can be raised for HA --
today, `kairon-controller` is the one component this project's own
production-readiness review flags as a real single point of failure:
`kairon-node` is a DaemonSet (one per node, no single-writer concern),
`kairon-ui` already has a documented HA story
([`docs/guides/kairon-ui-ha.md`](kairon-ui-ha.md)), but `kairon-controller`
used to hardcode `replicas: 1` with nothing stopping a second instance from
racing the first if you raised it anyway.

## Why this needed real work

`kairon-controller`'s reconcile loop (`internal/controller.Controller.Run`)
is the only writer of `spec.nodeName` scheduling decisions, migration/
snapshot/quota reconciliation, and every status patch this project makes.
Two unsynchronized instances polling and writing on the same interval would
race: both could try to schedule the same pending Machine onto different
nodes, both could act on the same `MachineMigration` transition at once.
None of that is safe to just "run twice."

The admission webhook and the health/metrics server are a different story
-- deliberately **not** gated by any of this. Both are stateless per-request
reads against current cluster state (the webhook decides allow/deny from
what it reads at request time; health/metrics report this process's own
state), safe to serve from every replica concurrently, the same way a real
Kubernetes `Service` in front of any stateless backend load-balances across
pods. Only the reconcile loop needs single-writer coordination.

## How it actually works

`internal/leaderelection` (see its package doc for the full rationale)
implements Lease-based (`coordination.k8s.io/v1`) leader election on top of
Kairon's own hand-rolled `internal/kube` REST client, rather than pulling in
`client-go`'s much larger `tools/leaderelection` package -- consistent with
this project's Go-stdlib-only posture. Every replica competes for one
`Lease` named `kairon-controller` (`controller.leaderElection` in
`values.yaml`) in its own namespace; only the replica currently holding it
runs the reconcile loop. Losing the lease (a missed renewal, another
replica winning a race after a stall) cancels that replica's reconcile loop
promptly and it goes back to competing to reacquire.

This is on (`controller.leaderElection.enabled: true`) by default in the
Helm chart, even at `replicaCount: 1` -- it's a no-op single-writer race
there (the one replica always wins), and this way raising `replicaCount`
later never means also remembering to flip on a setting. The chart grants
the small, namespaced RBAC this needs (`get/list/watch/create/update` on
`leases`, scoped to one object in one namespace, not cluster-wide) and sets
`KAIRON_CONTROLLER_NAMESPACE` via the standard downward-API `fieldRef`
pattern this project already uses elsewhere (see `kairon-ui`'s
`KAIRON_UI_NAMESPACE`).

`cmd/kairon-controller`'s own `-leader-elect` flag defaults to **`false`**,
not `true` -- the opposite default from the Helm chart. This matters for
backward compatibility: a bare-metal/systemd deployment (like this
project's own real-infrastructure testing host) upgrading its
`kairon-controller` binary in place, without also granting the new `leases`
RBAC or setting a namespace, must keep working exactly as before with zero
configuration changes. `-leader-elect=true` without a resolvable namespace
(`-leader-elect-namespace` or `KAIRON_CONTROLLER_NAMESPACE`) is a startup
refusal ("secure startup refused"), not a silent fallback -- the same
fail-closed posture this project already uses for the webhook's TLS
cert/key pairing.

## Enabling it outside Helm (bare-metal/systemd)

`deploy/rbac.yaml` includes the namespaced `Role`/`RoleBinding` this needs
(applied unconditionally, harmless if unused). To actually turn it on for a
systemd-managed `kairon-controller`, add to its `ExecStart` (or a systemd
drop-in, e.g. `systemctl edit kairon-controller`):

```
--leader-elect=true --leader-elect-namespace=kairon-system
```

Then run more than one instance (a second host, or a second systemd unit
pointed at the same cluster) if you actually want HA out of this --
raising `replicaCount` is meaningless outside Kubernetes; this is the
manual equivalent.

## Real limits today (first cut)

- **Lease renewal/takeover is coarse, not instant.** Defaults (see
  `internal/leaderelection`'s `Default*` constants): a 15s lease duration,
  renewed every 5s. Losing a leader (a crash, a network partition) costs
  up to ~15s before another replica takes over and reconciliation resumes
  -- a real, expected gap, not a bug, the same shape as every other
  Lease-based leader election.
- **Every failure mode is fail-closed to "not leader."** A `Lease`
  read/write error, a decode failure, or genuine doubt about who currently
  holds it all resolve to stepping down (or never starting) rather than
  reconciling anyway -- consistent with this project's `NeedsRecovery`
  philosophy of naming an uncertain outcome instead of guessing. This means
  a cluster-wide apiserver outage stops reconciliation everywhere, same as
  today's single-instance behavior, not a new failure mode.
- **The admission webhook and health/metrics endpoints are unaffected on
  purpose** -- every replica answers webhook requests and reports healthy
  regardless of leadership. Don't read "N replicas Ready" as "N replicas
  reconciling"; only one ever is.
- **No data yet on the real per-replica reconcile-loop breaking point**
  for very large fleets (see ROADMAP.md) -- `replicaCount > 1` today is
  about availability, not horizontal reconcile throughput; only one
  replica ever does the work at a time.

## What you don't need to do anything for

- `kairon-node` (a DaemonSet) and `kaironctl` (a one-shot CLI): neither has
  a single-writer concern this applies to.
- The admission webhook: already safe behind a `Service` load-balancing
  across every replica, with or without leader election.
