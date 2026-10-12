# User guide: cross-cluster disaster recovery (experimental)

A `MachineProtectionPlan` protects a group of Machines across two Kairon
clusters. One site runs the Machines and replicates them to S3; the other
keeps their specs and restore points, and can take over: in dependency
order, after a disaster (unplanned) or with nothing lost (planned), or
as a DR test into an isolated network while production keeps running.

Replication is the incremental S3 backup
([machine-backup-incremental.md](machine-backup-incremental.md)), so the
same requirements apply: image-booted Machines on nodes labelled
`kairon.zyvor.dev/cbt=true`, and an S3 bucket both sites can read.

## How it fits together

The same plan exists on both clusters, with `role: Primary` on the site
that runs the Machines first and `role: Secondary` on the other. Each
kairon-controller reads the other site's plan through that cluster's
Kubernetes API (`spec.peer`).

- **Active site.** Labels the protected Machines
  `kairon.zyvor.dev/protection-plan=<plan>` and owns a `MachineBackupPolicy`
  named `dr-<plan>` that backs them up every `replication.interval`, with
  a new chain every `fullEvery` and GFS `retention`.
- **Standby site.** Every reconcile it copies the active site's Machine
  specs into `status.machines[].spec` (placement, CPU pinning and device
  claims cleared) and imports each of its `dr-<plan>` backups that
  succeeded as a local `MachineBackup` with the same name, annotated
  `kairon.zyvor.dev/replicated-from`. An imported backup is a restore point
  only: Kairon never runs it, and deleting it leaves the S3 objects alone.
  When the active site's retention drops a backup, the standby drops its
  copy too.
- **Objectives.** The standby reports `status.rpoSeconds` (age of the
  oldest of the Machines' newest restore points) and the `RPOMet`
  condition against `objectives.rpo`. Failovers and tests report
  `rtoSeconds` and `dataLossSeconds`, and note when they missed
  `objectives.rto`.
- **Fencing.** `status.epoch` goes up with every failover. A site that
  sees its peer at a higher epoch halts its Machines (their disks stay for a
  failback), deletes its `dr-<plan>` policy and becomes the standby. A
  site that cannot reach its peer keeps running: Kairon does not stop
  Machines on a network partition, so after an unplanned failover the old
  site halts once it can reach the new one again. Fence the old site by
  other means (power, network) if both must never run at once.

## Failover

A failover runs on the standby. For each wave of `spec.machines` (a
Machine starts once everything in its `dependsOn` is Running) it:

1. creates the Machine from the mirrored spec, or uses the one that is
   already there (a failback finds the Machines it halted);
2. once it has a FluxVM runtime, halts it and restores the chosen restore
   point with a `MachineBackupRestore` named `dr-<plan>-<hash>-<machine>`;
3. starts it, and waits for it to be Running.

When every Machine is Running the site becomes active and starts
replicating. Its first backups are fulls: a new site starts new chains.

- **Unplanned** (`kaironctl dr failover PLAN`): restores each Machine's
  newest restore point. The epoch goes up at once, so the old site halts as
  soon as it sees the new one.
- **Planned** (`--planned`): the standby asks the active site to hand over
  (annotation `kairon.zyvor.dev/protection-handoff` on its plan). The
  active site halts its Machines, waits for any in-flight replication
  backup, takes a final incremental backup of each, and reports
  `status.handoff.phase: Ready`. The standby imports the final backups,
  raises the epoch and restores them. Nothing written before the halt is
  lost. Failing back is a planned failover run on the original site.

A failed failover stops where it is (`status.failover.phase: Failed`,
with the reason); the Machines it created stay for inspection. Fix the
cause and run the failover again with a new ID.

## DR tests

`kaironctl dr test PLAN` on the standby restores copies of the Machines,
named `<machine>-drtest` and labelled `kairon.zyvor.dev/protection-test`,
into `spec.test.bridge`: a host bridge with no uplink, so the copies can
talk to each other but not to production. The bridge must be in
kairon-controller's `--dr-test-bridges` (chart `controller.dr.testBridges`);
create it on every node that may run the copies
(`ip link add drtest0 type bridge && ip link set drtest0 up`). The active
site keeps running and replicating. `kaironctl dr test-end PLAN`, or
`test.ttl` (default 1h), deletes the copies and their restores.

## Setup

1. On both clusters, install kairon-controller with DR on (chart
   `controller.dr.enabled=true`, or `--dr-secret-namespace` on a
   kairon-controller you run yourself). This grants it create and delete
   on `machinebackuppolicies` and `machinebackuprestores`, and `get` on
   Secrets in its own namespace.
2. On each cluster, apply [`deploy/dr-peer-rbac.yaml`](../../deploy/dr-peer-rbac.yaml),
   bind `kairon-dr-peer` in the plan's namespace and create a token for the
   ServiceAccount. The peer can read the plan, patch it (the handoff
   annotation), read the protected Machines and list MachineBackups.
3. On the other cluster, store that token (`token`) and the API server's CA
   (`ca.crt`) in a Secret in kairon-controller's namespace, labelled
   `kairon.zyvor.dev/dr-peer-credentials=true`. Kairon reads no other
   Secret for this.
4. In the plan's namespace on both clusters, create the S3 credentials
   Secret (labelled `kairon.zyvor.dev/backup-credentials=true`) and, if the
   backups are encrypted, the same key Secret.
5. Apply the plan on both sites, see
   [`examples/machineprotectionplan.yaml`](../../examples/machineprotectionplan.yaml).

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineProtectionPlan
metadata: {name: shop, namespace: prod}
spec:
  role: Primary                # Secondary on the other site
  machines:
    - name: db
    - name: web
      dependsOn: [db]
  replication:
    interval: 15m              # at least 5m
    fullEvery: 7d
    target:
      s3: {endpoint: https://s3.dr.example.com, bucket: kairon-dr, prefix: shop, credentialsSecretRef: s3-backup}
  objectives: {rpo: 30m, rto: 15m}
  peer:
    server: https://api.site-b.example.com:6443
    credentialsSecretRef: dr-peer-site-b
    # namespace, plan: default to this plan's
  test: {bridge: drtest0, ttl: 2h}
```

`replication.target` is where the standby restores from. If the sites use
different buckets, replicate the bucket (same keys) and point each site's
plan at the copy it can reach.

## Operating it

```console
$ kaironctl dr get -n prod
NAME  ROLE       PHASE    ACTIVE  EPOCH  RPO    PEER       MACHINES
shop  Secondary  Standby  false   1      6m12s  reachable  2
$ kaironctl dr status shop -n prod      # restore points, runs, conditions
$ kaironctl dr test shop -n prod
$ kaironctl dr test-end shop -n prod
$ kaironctl dr failover shop -n prod --planned
```

Each command writes `spec.action` with a new ID; the controller takes up
an ID once (`status.lastAction`). A failover on the active site, or a test
while one is running, is ignored with the reason in `status.message`.

Metrics: `kairon_dr_rpo_seconds`, `kairon_dr_active`,
`kairon_dr_peer_reachable`, `kairon_dr_last_rto_seconds{kind}` and
`kairon_dr_last_data_loss_seconds{kind}` (kind `failover` or `test`).

## Limits

- Replication covers image-booted Machines' disks through FluxVM's changed
  block tracking. Machines on PVCs (`spec.volumes`) are not protected.
- Machines are restored crash-consistent unless the guest agent freezes
  filesystems (`replication.quiesce`).
- The standby needs the peer's API to mirror specs and import restore
  points. While the peer is unreachable it keeps what it has and can still
  fail over from it.
- An active site that is cut off but still running keeps replicating and
  applying retention, which may delete backups the standby imported.
  Retention keeps every backup the newest chain needs and consolidates
  only chains older than its window, so this takes either an outage
  longer than the window or a new chain (`fullEvery`) starting during it.
  A failover from a deleted point fails with the missing S3 key.
- Networking is whatever the mirrored spec says; making the Machines
  reachable at the new site (DNS, load balancers, IP addresses) is outside
  the plan.

Lab results are in the "Cross-cluster DR" section of
[`COMPATIBILITY.md`](../COMPATIBILITY.md#cross-cluster-dr).
