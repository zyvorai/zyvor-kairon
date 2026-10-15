# Advanced operations (experimental)

Five workflows help an operator test changes against application behavior.
These are CLI workflows with durable local JSON evidence. Build from this
source tree; they are not claimed as released or hardware-qualified features.

| Workflow | Command | Result |
| --- | --- | --- |
| Upgrade rehearsal | `rehearsal run` | Independent CSI copies, baseline/change/application probes, cleanup |
| Configuration transaction | `changeplan preview/apply/recover` | Reviewed CAS patches, health gates, durable commit or reverse compensation |
| Application recovery | `recovery verify` | Restore recorded MachineSnapshots, measure time through successful application probes and snapshot age |
| Network placement | `placement collect/recommend` | Measured egress rates + node RTTs + live placement constraints; recommendations only |
| Incident investigation | `incident rehearse` | Bind a redacted bundle to recovery copies, reproduce a declared failure, diagnose, execute a declared fix and verify |

Read [Rehearsal](rehearsal.md) first for the isolation setup, supported QEMU/QGA
Filesystem PVC workload shapes, CSI permissions and cleanup contract. Recovery
and incident flows share that engine and its restrictions. Both require the
updated node agent and the dedicated bridge guard. CLI access also needs get/list on CSI snapshot and
storage classes/content for offline recovery, get/patch on transaction targets,
and list on Nodes, Machines and migrations for placement. The additional snapshot
`status.sourceUID` field requires applying the updated CRDs **before** deploying
the updated controller and capturing new checkpoints.

```bash
go build -o ./bin/kaironctl ./cmd/kaironctl
export PATH="$PWD/bin:$PATH"
export KAIRON_UI_URL=https://your-kairon-ui
# Configure your normal UI authentication and Kubernetes context.
```

Keep plans and reports private. Plans can contain sensitive command arguments;
reports contain source identities, diagnostic summaries and operational state.
Reports are execution evidence, not signed attestations. Match the original
cluster context on every recovery or cleanup command.

## Health-gated change plans

The allowlist is deliberately bounded: Machine priority, power state and
hotplug disk attach/detach via `spec.disks`; native single-Machine `MachineNetworkPolicy.spec.policy`.
Each resource appears in one stage. Every stage requires forward **and** rollback
health probes. Guest exec is performed on the declared Machines, so use this
workflow only for changes you intend to apply to those live resources.

1. Edit `examples/operations/changeplan.json` for your disposable application.
   The example assumes an existing native policy named `demo-db-edge` with
   `spec.machineName: demo-db` and a working HTTP service on the database guest.
   It probes both database loopback and web-to-database connectivity. Replace the
   example address/CIDR with your actual disposable lab network.
2. Run the read-only preview. It records object UIDs, resourceVersions, original
   values and a digest of the prepared plan. Review that exact file.
3. Apply it with a new journal path. Any status update changing a resourceVersion
   between preview and apply can cause a conservative refusal; preview again.

```bash
kaironctl changeplan preview --file examples/operations/changeplan.json --output prepared.json
kaironctl changeplan apply --file prepared.json --output change-journal.json
# Following interruption, API outage or NeedsRecovery:
kaironctl changeplan recover --file change-journal.json
```

The engine persists intent before mutation, reserves the object with an annotation
in the same UID/resourceVersion-tested JSON Patch, waits for relevant runtime
status and runs probes. Policy status must show `effectiveSynced` and an
application timestamp after intent; synchronize controller/node/operator clocks.
CNP/Cilium and selector-wide policies are refused because their side effects or
multi-node completion cannot be established by this status contract.

A failed gate records a rollback decision and compensates in reverse order.
Compensation keeps the reservation until rollback status and rollback probes
succeed. A durable commit decision is recovered by releasing reservations;
forward operations are never replayed after restart. Lost responses are resolved
by inspecting identity, reservation and changed fields. Replacement UIDs, foreign
reservations or external edits to those fields produce `NeedsRecovery` and halt
compensation. Other Kubernetes writers are not blocked by the advisory annotation.

This is **compensation, not distributed ACID**. Other observers can see intermediate
stages. It does not roll back guest writes, packets, messages, detached disk contents
or external side effects. A disk plan, shown in `disk-changeplan.json`, requires a
ready MachineSnapshot with the same source UID, QEMU and Bound added claims.
Changing a disk under an existing name is refused because attached-name status
cannot establish replacement completion. The checkpoint is retained as operator recovery material; it is not automatically
restored. No source PVC is deleted. Choose application-aware probes, including
actual connectivity for policy changes and filesystem behavior for disk changes.
An image-backed VM without persistent boot volumes cannot be switched to
`Stopped` by this workflow because that could discard its ephemeral disk.

## Verify application recovery

Create ready MachineSnapshots of all required members using your normal backup
consistency procedure. Install the demo fixture as described in the Rehearsal
guide. Edit `examples/operations/recovery.json` to identify the healthy points,
namespace, isolated host and storage classes. With the CRDs/controller updated,
new snapshots record source spec and UID at capture time.

```bash
kaironctl recovery verify --file examples/operations/recovery.json --report recovery-report.json
```

Recovery restores from each point's recorded source spec, so the original Machine
and PVC may be absent. When the PVC is missing, specify `restoreStorageClass` for
each volume. The engine requires bound CSI content metadata, Filesystem source
mode and a matching snapshot/storage-class driver. Existing source PVC metadata
is used when available; the override is for a missing PVC. The report identifies
the snapshots used and preserves externally supplied points during cleanup.

Recovery permits setup and verification, and forbids baseline/upgrade changes.
`applicationRTOSeconds` measures workflow start through **all successful
application probes**, including provisioning and boot, excluding cleanup. This
is a drill duration, not time since an actual production outage.
`recoveryPointAgeSeconds` is age at workflow start of the oldest CSI
`creationTime` across all selected volumes. `maxRPOSeconds` gates that measured
backup age; missing/future timestamps fail a configured RPO gate. It does not
prove a database's last committed transaction time or coordinated consistency.
Use transaction-aware application probes when that assurance is needed.
`maxRTOSeconds` and `maxRPOSeconds` are optional positive objectives; zero disables
that gate. Gate failure still cleans owned copies and returns a failed report.

## Collect traffic and recommend placement

Provide independently **measured**, fresh node-pair RTTs and a measured same-node
RTT in `rtts-template.json`. The example's old timestamps and numbers are placeholders,
so it intentionally cannot authorize a recommendation unchanged. Measure every
node pair needed by the current graph and candidate placements. RTTs are treated
as symmetric; directional asymmetry is outside this model.

```bash
kaironctl placement collect --rtts measured-rtts.json --window 10s --output traffic.json
kaironctl placement recommend --telemetry traffic.json --max-moves 2 > recommendations.json
```

Collection reads the existing authenticated `network-flows` UI route twice and
uses per-VM elapsed time to derive egress byte rates from cumulative counters.
It checks VM UID, runtime, addresses and node after collection. Duplicate guest
IPs, unknown counter schemas or no measured inter-VM traffic cause refusal.
Denied, external, reset and newly visible pre-existing flows are skipped and
counted. Destination-side observations are excluded to avoid double counting.
This models observed flow records; flow eviction/truncation can underrepresent
traffic. It is not complete packet accounting or traffic replay.

Recommendation minimizes estimated `sum(bytes/second * RTT milliseconds)`.
This is a traffic-weighted network cost, not an application latency prediction.
It reuses capacity, capability, selectors/affinity and topology-spread filters,
requires fresh low host PSI, confirmed matching storage/network domains and
excludes devices, pinned workloads, active migrations and recently moved VMs.
It evaluates sequential moves against updated capacity and placement. Missing
candidate RTTs are rejected rather than guessed. `--migration-penalty` is an
operator-supplied minimum saved cost in the same units, not measured migration
duration. No migration or node-name change is applied: review the recommendation
and use Kairon's existing migration preflight before moving anything.

## Reproduce an incident and test a fix

Capture the incident bundle with
`kaironctl diagnose machine demo-db --deep -o redacted-incident.tar.gz`, keeping
redaction enabled (see [incident bundles](machine-incident-bundle.md)). Separately capture ready CSI
MachineSnapshots while the disposable demo database has the broken schema.
The bundle supplies identity/diagnostic context; **it does not contain VM disks**.
For the supplied scenario, run `app.py breaking` on the disposable database before
capturing the incident point. The web point only needs its fixture installation.

```bash
kaironctl incident rehearse --bundle redacted-incident.tar.gz \
  --file examples/operations/incident.json --report incident-report.json
# Optional configured-model summary of copy diagnostics:
kaironctl incident rehearse --ai --bundle redacted-incident.tar.gz \
  --file examples/operations/incident.json --report incident-ai-report.json
```

The parser bounds compressed/decompressed bytes and file count, rejects unsafe
paths, links and duplicate manifests, and never extracts or executes bundle
contents. It requires a redacted manifest, matching namespace, source Machine UID
and a recovery point recording that same UID. Older points without a recorded UID
must be recaptured for incident use. A bundle hash binds evidence but does not
establish authenticity or deterministic historical replay.

The baseline must declare an expected nonzero exit. The fixture's
`--expect-schema-failure` returns 1 only for the exact HTTP 503/schema signature;
healthy responses, network failures and other errors return 2. Therefore an
unreachable service cannot masquerade as reproducing this schema incident.
The engine diagnoses the isolated copies only after reproduction, records findings,
then executes the **operator-declared** changes and requires successful final probes.
The example repairs the table name and verifies the HTTP API. Runtime health alone
does not mark application health. `--ai` uses the configured model only for an
additional proposal/summary; model output never becomes executable fix steps.
This is controlled failure reproduction and fix testing, not arbitrary automatic
root-cause discovery or autonomous production repair.

## Scenarios and qualification

Run `scripts/test-advanced-operations.sh` for automated scenarios. It uses a TCP
Kubernetes API simulator, independent real SQLite files and real Python HTTP
probes; no VM host is required. Read the [validation record](../validation/advanced-operations-2026-10-15.md)
for exact scope and results. `scripts/lab-advanced-operations.sh` runs your edited
plans in a configured disposable cluster. It applies the supplied live change
plan: use only lab targets. Native KVM boot, CSI restore, nftables enforcement,
real policy convergence and migration benefit still need hardware qualification.
