# Runbook: a second cluster for cold DR

This page stands up a second Kairon cluster that can take over a group of
Machines by restoring them from S3. The feature is the experimental
`MachineProtectionPlan`; [the guide](guides/cross-cluster-dr.md) explains
how it works. The steps below only use what the repo ships today.

## Limits

- **No live cross-cluster migration.** `MachineMigration` moves a running
  Machine between nodes of one cluster only. Across clusters, a Machine is
  restored from its newest restore point (planned failover: from a final
  backup taken after it halts), so it restarts and loses whatever it wrote
  after that point.
- **Never run on two real clusters.** The `DR:` rows in
  [`COMPATIBILITY.md`](COMPATIBILITY.md#cross-cluster-dr) passed with two
  namespaces on one k3s node ([`scripts/lab-dr.sh`](../scripts/lab-dr.sh)).
  These rows are still `not run`: two real clusters, separate buckets, a
  real network partition, external-dns writing a real zone, and a floating
  IP moving between clusters. No script drives two clusters yet.
- **Only image-booted Machines.** A Machine is protected only if it boots
  from `spec.image` on a node labelled `kairon.zyvor.dev/cbt=true`
  (incremental S3 backup). Machines on PVCs (`spec.volumes`) are not
  covered. For those, use [`kaironctl export`](runbook-vm-export.md) or
  your storage's own replication.
- **Peer tokens expire.** Each site reads the other through a
  `kairon-dr-peer` ServiceAccount token. Kairon does not renew it. Rotate
  the Secret before it expires; the controller reads it again on every
  reconcile.
- **No fencing on a partition.** A site that cannot reach its peer keeps
  running its Machines. Fence the old site yourself (power, network) if
  both must never run at once.
- **Replication covers only the protected Machines and their backups.**
  It does not copy other Kairon objects, chart Secrets, TLS Secrets or the
  bucket itself. Use [`scripts/backup-crds.sh`](runbook-backup-restore.md)
  for the Kubernetes objects, and your object store for bucket replication.

## Steps

Site A is the cluster that runs the Machines today, and site B is the new
one. The example plan is `shop` in namespace `prod`; change the names to
suit. Run each command against the cluster named in the step.

1. **Install Kairon on B** at the same version as A, with DR and
   incremental backups on, and do the same on A with `helm upgrade`:

   ```bash
   # On B, also pass the values you use on A (-f values.yaml).
   helm upgrade --install kairon oci://ghcr.io/zyvorai/charts/kairon -n kairon-system --create-namespace \
     --reuse-values --set controller.dr.enabled=true --set node.cbtBackup.enabled=true \
     --set 'controller.dr.testBridges={drtest0}'          # only for DR tests
   kubectl get nodes -l kairon.zyvor.dev/cbt=true          # both sites need some
   ```

2. **Match the namespace and networks on B.** Create `prod`. With
   `network.preserveIP`, also create a `VirtualNetwork` with the same name
   and a CIDR that contains A's addresses. For DR tests, create the bridge
   on every node of B that may run the copies:
   `ip link add drtest0 type bridge && ip link set drtest0 up`.
3. **Give each site read access to the other.** On *each* cluster:

   ```bash
   kubectl apply -f deploy/dr-peer-rbac.yaml
   kubectl create rolebinding kairon-dr-peer -n prod --clusterrole=kairon-dr-peer \
     --serviceaccount=kairon-system:kairon-dr-peer
   kubectl create token kairon-dr-peer -n kairon-system --duration=720h > peer.token
   ```

   On the *other* cluster, store that token and this cluster's API CA in
   `kairon-system`:

   ```bash
   kubectl create secret generic dr-peer-site-a -n kairon-system \
     --from-file=token=peer.token --from-file=ca.crt=site-a-ca.crt
   kubectl label secret dr-peer-site-a -n kairon-system kairon.zyvor.dev/dr-peer-credentials=true
   ```

   The API server may cap `--duration`. Note when the token expires so you
   can rotate it.
4. **Create the S3 credentials on both sites** in `prod`, with the same
   name and the same bucket (or a replicated copy with the same keys):

   ```bash
   kubectl create secret generic s3-backup -n prod \
     --from-literal=accessKeyId=... --from-literal=secretAccessKey=...
   kubectl label secret s3-backup -n prod kairon.zyvor.dev/backup-credentials=true
   ```

   If backups are encrypted, create the same key Secret on both sites.
5. **Apply the plan on both sites.** Start from
   [`examples/machineprotectionplan.yaml`](../examples/machineprotectionplan.yaml).
   On A, use `role: Primary` with `peer.server` set to B's API and
   `credentialsSecretRef: dr-peer-site-b`. On B, use `role: Secondary`
   with `peer.server` set to A's API and `credentialsSecretRef: dr-peer-site-a`.
6. **Check replication on B.** Run `kaironctl dr get -n prod`. B should show
   `PEER reachable`, an `RPO` below `objectives.rpo`, and every Machine
   listed. `kaironctl dr status shop -n prod` shows the imported restore
   points and the `RPOMet` condition.
7. **Prove a restore without touching production.** On B, run
   `kaironctl dr test shop -n prod`. Wait for the copies (`<machine>-drtest`)
   to reach Running, check their data, then run
   `kaironctl dr test-end shop -n prod`. Optionally set `test.every` so B
   repeats the test on its own and records `status.lastTest`.
8. **Take over when needed.** Run the command on B, the site that takes over:
   - `kaironctl dr failover shop -n prod --planned` while A is reachable
     (no data loss);
   - `kaironctl dr failover shop -n prod` after a disaster (newest restore
     point).

   To fail back, run the planned failover on A.
9. **Record it.** Add the results of steps 6 to 8 by hand to the
   `DR: two real clusters` rows of `COMPATIBILITY.md`, with RPO, RTO and
   data loss from `kaironctl dr status`. `lab-dr.sh` cannot write rows for
   two clusters.

## If it doesn't work

- **`PEER unreachable`:** check the `dr-peer-*` Secret (label, `token`,
  `ca.crt`), whether the token has expired, and whether the controller can
  reach `peer.server`.
- **No restore points on B:** check that A's `dr-shop` MachineBackupPolicy
  has succeeded backups (`kaironctl backup chain MACHINE -n prod`). Check
  that the Machines are image-booted on `cbt=true` nodes.
- **`NetworkReady=False`:** the VirtualNetwork on B is missing, its CIDR
  does not contain A's address, or another Machine holds that address.
