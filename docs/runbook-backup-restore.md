# Runbook: backing up and restoring Kairon's Kubernetes-level state

Kubernetes is Kairon's only source of truth for *declarative* state
(`Machine`, `MachineMigration`, `MachineSnapshot`, `MachineSnapshotRestore`,
`MachineNetworkPolicy`, `NetworkSecurityGroup`, `MachineDisruptionBudget`,
`MachineQuota`) -- see [`ARCHITECTURE.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/ARCHITECTURE.md). Until now
that had no backup story at all: an etcd/cluster loss had no documented
recovery path. This closes that gap for the Kubernetes-level state; read
"What this does and doesn't protect" below before assuming more than that.

## What this does and doesn't protect

Three genuinely different things can be lost, independently of each other:

1. **Kubernetes' own state (etcd)** -- every Kairon CRD object, and every
   Secret the Helm chart manages (`kairon-ui-users`, `kairon-ui-session`,
   etc.). `scripts/backup-crds.sh` backs this up. This is what this
   runbook is actually about.
2. **The VM's disk content** -- lives on the host filesystem (a
   `hostPath`/`local` `PersistentVolume`) or in a CSI storage backend
   (iSCSI via Kairon's own `kairon-csi-node`, or any other CSI driver).
   Kairon's own answer here is `MachineSnapshot`/`MachineSnapshotRestore`
   (see [`docs/guides/machine-snapshot-restore.md`](guides/machine-snapshot-restore.md))
   -- but that's an *in-cluster*, storage-class-local operation. Getting
   snapshot content *off* the cluster (to object storage, a second site,
   etc.) is your CSI driver's/storage backend's own export tooling, not
   something Kairon does -- the same boundary already documented for iSCSI
   itself in [`docs/guides/machine-storage-csi.md`](guides/machine-storage-csi.md)
   ("iSCSI's own operational requirements aren't Kairon's to solve").
   Kairon's own CSI driver can create in-cluster snapshots for
   dynamically-provisioned volumes (`csiController.snapshotter.enabled`), but
   it has no export capability of its own.
3. **FluxVM's own runtime state on each host** -- the actual running QEMU/
   Cloud Hypervisor/Firecracker process, its VNC socket, etc. This is
   FluxVM's concern, not Kairon's, and this runbook doesn't touch it.

So: this runbook gets you back the *scheduling and policy* state (which
Machines exist, their specs, quotas, disruption budgets, network policy)
after a Kubernetes-level loss. Whether that actually gets you working VMs
back depends entirely on whether (2) and (3) survived independently --
see "Restoring" below for exactly how that plays out.

## Backing up

```bash
./scripts/backup-crds.sh [output-name]
```

Writes `output-name.tar.gz` (default `kairon-backup-<timestamp>.tar.gz`)
containing one YAML file per Kairon CRD kind, cluster-wide (every
namespace), plus a `manifest.txt` with an object count per kind. Run it
from anywhere with a working `kubectl` context pointed at the cluster --
it needs the same read access `kaironctl`/`kairon-ui` already have (`get`/
`list` on the `kairon.zyvor.dev` CRDs), nothing new.

Chart-managed Secrets (`kairon-ui-users`, `kairon-ui-session`,
`kairon-ui-token`, `kairon-ui-oidc`, `kairon-console-token`) are **not**
included by default -- set `KAIRON_BACKUP_SECRETS=true` to also capture
them. They hold live credentials (bcrypt hashes, a session-signing HMAC
key, the legacy shared bearer token, an OIDC client secret): treat that
half of the archive like any other credential backup -- encrypt it at
rest, restrict who can read it. Operator-supplied TLS secrets
(`webhook.tlsSecretName`, `migration.tlsSecretName`,
`console.tls.secretName`) are never included; back those up via whatever
issued them (cert-manager, your own PKI) -- this chart doesn't mint them,
so it doesn't own backing them up either, same posture documented for
`webhook.caBundle` in SECURITY.md.

Nothing here is Kairon-specific about *how often* to run this or where to
store the archive -- treat it like any other Kubernetes object backup
(a cron job piping into your existing backup target is the obvious setup;
this script deliberately does only the "dump the objects" part, not
scheduling or off-cluster upload, so it composes with whatever you already
use for that).

## Restoring

```bash
./scripts/restore-crds.sh BACKUP.tar.gz          # dry run (default)
./scripts/restore-crds.sh BACKUP.tar.gz --yes    # actually applies
```

Without `--yes` it only prints `kubectl apply --dry-run=client` output --
nothing changes. Review the target context (`kubectl config
current-context`) before adding `--yes`; restoring into the wrong cluster
is exactly the kind of mistake dry-run exists to catch.

**What actually happens after a restore, verified against
`internal/agent/agent.go`'s real reconcile code, not assumed:**

- `kairon-node` looks up a Machine's existing FluxVM runtime by
  `status.runtimeID` **first**, but falls back to a deterministic name
  lookup, `RuntimeName()` = `"kairon-<namespace>-<name>"`
  (`internal/model/types.go`), if that's empty or stale. A restored
  Machine object won't have a live `status.runtimeID` (status is
  regenerated, not meaningfully restorable), but as long as its
  `metadata.namespace`/`metadata.name` match what they were before, and
  the host named in `spec.nodeName` still has that FluxVM runtime alive,
  **`kairon-node` re-adopts the existing VM by name on its next reconcile
  tick** -- no duplicate VM, no `kairon.zyvor.dev/adopt-only` annotation
  needed (that annotation is specifically for migration-cutover ambiguity,
  a different problem -- see
  [`docs/guides/machine-fencing.md`](guides/machine-fencing.md)). This is
  the scenario this runbook actually protects against well: **Kubernetes/
  etcd lost, hosts and their VMs still running.**
- If the named runtime is gone too (the host was also lost, or the VM was
  actually stopped), `kairon-node` does exactly what it does for any new
  Machine: creates a fresh VM from `spec.image`/`spec.resources`. Whether
  that's "restored" or "a brand new empty VM" depends entirely on whether
  the boot disk itself survived independently (a `hostPath`/`local` PV
  pointing at a still-intact image file, or a PVC-backed volume whose
  underlying storage lived through whatever took out Kubernetes) -- this
  script has no way to know or guarantee that; see "What this does and
  doesn't protect" above.
- A restored `MachineMigration` in a non-terminal phase (`Starting`,
  `Running`, `Cutover`, `Adopting`, `NeedsRecovery`, ...) does **not**
  resume an in-flight migration -- the actual session/journal state that
  drove it lived on the source/destination hosts (see
  [`docs/runbook-migration-failures.md`](runbook-migration-failures.md)),
  not in the `MachineMigration` object alone. Delete any restored
  migration still in a non-terminal phase rather than trusting it; let the
  Machine's own status re-derive from `kairon-node`'s reconcile instead.
- The `ValidatingWebhookConfiguration` itself (if `webhook.enabled`) is
  Helm-chart-managed, not a Kairon CRD object -- `helm upgrade --install`
  (or reapplying `charts/kairon`) restores it, this script doesn't.

Chart-managed Secrets, if included in the backup, are deliberately **not**
auto-applied by `restore-crds.sh` -- review and `kubectl apply` them
yourself. Restoring `kairon-ui-session` in particular changes the session-
signing key: every operator gets signed out.

## Real limits today (first cut)

- Not yet drilled against an actual full cluster-loss scenario on real
  hardware in this repo's own CI, the same honesty this project already
  applies to `docs/runbook-multi-host-migration-test.md`/
  `docs/runbook-recovery-drill.md` -- both need infrastructure this
  repository's own CI doesn't have. What's been verified: a real backup
  against a real cluster, and a real dry-run restore of that backup
  against the same cluster's current state (see the commit history for
  when).
- No scheduling, retention, or off-cluster upload -- deliberately out of
  scope, see "Backing up" above.
- No point-in-time consistency across the CRD kinds -- each is
  listed independently, one `kubectl get -A` at a time, not a single
  atomic snapshot. A Machine created between two of those calls could be
  in one CRD's backup and absent from a related one it referenced (rare,
  and no worse than any other non-transactional multi-object backup).
