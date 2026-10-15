# Kairon Rehearsal (experimental)

Rehearse a database/application upgrade on independent VM disk copies. Run a
baseline check, change the copies, verify the application, and delete the copies.
Nothing promotes a copy or applies a change to the source Machines.

This first release is a CLI workflow, not a new controller/CRD or dashboard page.
It supports Linux/QEMU guests with QGA and CSI **Filesystem** PVCs in
`spec.volumes`, including data volumes. It rejects hotplug disks, Atlas volumes,
raw Block claims, shared source PVCs, devices, accelerators, sandbox templates, TPM, secure boot and
CD-ROMs, host-specific CPU/NUMA pinning, hugepages and custom host kernel paths. Restore and boot must work with your CSI driver before using this flow.

## What happens

1. Validate the entire plan and every source before creating resources.
2. Reserve the target Node with a resourceVersion-tested annotation update.
   Only one rehearsal may use a Node at a time, across all namespaces. No expired
   reservation is stolen: cleanup must finish before another run can start.
3. Take CSI snapshots of every source volume (`consistency: crash`), or use
   explicitly supplied ready snapshots (`consistency: existing`).
4. Create independent PVCs from those snapshots, then create the consumer VMs
   before waiting for binding, supporting `WaitForFirstConsumer` storage classes.
5. Boot all copies on the reserved, isolated host bridge and wait for QGA.
6. Run `setup`, `baseline`, `changes` and `verify` steps in their listed order.
   A failed baseline stops the upgrade. Upgrade commands are never retried.
7. Delete Machines and wait for their finalizers, then delete claims and wait for
   PVC protection, then delete snapshots created by this run. Release the Node
   reservation only when every tracked resource has gone.

A private JSON journal is atomically persisted before every resource creation.
Cleanup checks the run label and UID, and sends Kubernetes UID delete
preconditions. An interrupted or ambiguous API request remains in the journal.
User-supplied snapshots are never deleted. A failed cleanup is a failed run,
with its remaining inventory preserved for a retry.

## Deploy and prepare a host

Build/deploy **both the updated CLI and kairon-node from this change**. Using an
older node agent bypasses the new isolation guard; do not assert the v1 annotation
until the updated agent is running. The host needs `ip`, `nft`, Linux sysfs and
permission for the node agent to read nftables (normally root/CAP_NET_ADMIN).
The node agent's Kubernetes identity needs `get` on Nodes, which the existing
chart grants. The CLI operator needs get/patch on the selected Node,
get/create/delete on Machines, PVCs and CSI VolumeSnapshots, and normal admin
authorization for guest exec through kairon-ui. No new chart-wide permissions
are granted by this feature.

On the target Linux VM host, as an administrator:

```bash
sudo ./scripts/rehearsal-bridge.sh krh-demo
```

The script creates a **new** bridge with no address or physical uplink, disables
its IPv6 link-local address, and installs nftables host input/output drop rules.
It refuses to reuse an existing bridge/table. Reserve it exclusively for
rehearsal; never attach a physical NIC, veth or other workload to it.

After deploying the updated node agent:

```bash
kubectl annotate node rehearsal-worker \
  kairon.zyvor.dev/rehearsal-bridge=krh-demo \
  kairon.zyvor.dev/rehearsal-guard=v1
```

The node agent verifies the local bridge has no IP, all ports are TAPs, the
nftables drop chains match, the Machine has no alternate NIC/forwarding path,
and the Node reservation belongs to this run. It refuses to create/start an
invalid copy. On a later failed check it asks FluxVM to stop the copy immediately.
This is checked at reconcile time, not a kernel-enforced prohibition on a trusted
host administrator changing networking between ticks. A failed stop RPC is
reported; it is not proof that a VM stopped. The host administrator remains part
of the trust boundary. All rehearsal copies intentionally communicate with each
other on the bridge. There is no NAT, DHCP, host service access or Internet access.
Guest services may keep their original credentials, jobs and static addresses;
network isolation is essential. Their data still consumes real host/storage
capacity. Use a dedicated host with sufficient CPU, RAM and CSI restore capacity.

## Reproducible two-VM scenario

Prepare disposable source Machines named `demo-db` and `demo-web`, both QEMU with
QGA enabled, Python 3 installed and **CSI snapshot-capable Filesystem boot PVCs**.
Use a CSI VolumeSnapshotClass with `deletionPolicy: Delete` if snapshot storage
should be reclaimed. `Retain` classes can intentionally leave backend snapshot
content after the API object is deleted.

Install `examples/rehearsal/app.py` at `/opt/kairon-rehearsal/app.py` in both
source guests. Prepare the fixture database in both guests:

```bash
mkdir -p /var/lib/kairon-rehearsal
python3 /opt/kairon-rehearsal/app.py init
```

These commands prepare the demo sources; the rehearsal runner itself never
executes a command inside a source. The sources can keep running during the
rehearsal. Modify the examples' Node, namespace and snapshot class to match your
lab, and ensure the fixture service is not already occupying port 8080.

```bash
export KAIRON_UI_URL=https://your-kairon-ui
# Supply KAIRON_UI_TOKEN using your normal authenticated CLI configuration.
kaironctl rehearsal run --file examples/rehearsal/compatible.json --report compatible-report.json
kaironctl rehearsal run --file examples/rehearsal/breaking.json --report breaking-report.json
```

Both examples restore the database and web Machines. Setup assigns private
addresses `10.77.0.2` and `10.77.0.3` and starts the database copy's tiny HTTP API.
The web copy checks the database copy's `/health` endpoint before and after the
schema change. Everything is offline and uses Python's standard library.

- **Compatible:** adds an optional SQL column; baseline and verification pass;
  command exits 0 after cleanup.
- **Breaking:** renames the SQL table; baseline passes, the upgrade exits 0, but
  the web copy receives HTTP 503; verification fails and the command exits nonzero
  after cleanup. This is an expected successful detection of a bad upgrade.

The private JSON report includes source UIDs/resourceVersions, recovery point
names, generated resource names/UIDs, guest readiness, step exit codes, durations,
stdout/stderr hashes, warnings and cleanup outcomes. Guest output is not stored.
The plan **is** stored, so avoid putting secrets directly in command arguments.
A report is execution evidence, not a cryptographically signed attestation.

## Consistency and dependency contracts

`consistency: crash` creates independently timed disk snapshots. It does not
freeze production, stop application writes, or promise an atomic recovery point
across volumes or Machines. Database recovery and application-level probes must
establish whether that copy is usable.

For application-coordinated recovery points, supply a snapshot for **every**
volume using `consistency: existing`:

```json
{
  "name": "demo-db",
  "snapshots": {
    "root": "application-coordinated-db-root"
  }
}
```

The runner checks snapshot readiness and that each snapshot references the
expected source claim. It does not establish when/how an external snapshot was
quiesced, nor prove that it matches a subsequently edited source spec. Use the
source shape that produced those snapshots. If a workload requires additional
volumes/devices that the first release rejects, do not omit those dependencies.
Setup commands must configure mock dependency endpoints inside the isolated
network when the application needs them. There is no transparent traffic replay,
external service cloning, automatic DNS rewrite, generic distributed quiesce or
production promotion in this release.

## Interruptions, TTL and cleanup

SIGINT/SIGTERM and workflow failures trigger cleanup with a fresh two-minute
context. A SIGKILL, host crash, API outage or journal write failure can leave
resources. Each copy also has `lifecycle.maxLifetimeSeconds`, so the controller
can delete its Machine after the TTL; this is a fallback for the **VM**, not a
background garbage collector for snapshots, claims or reservations. Keep the
report and retry cleanup against the **same cluster**:

```bash
kaironctl --context YOUR_ORIGINAL_CLUSTER rehearsal cleanup --report compatible-report.json
```

Never manually clear a reservation while its copies/resources still exist.
Cleanup refuses changed UIDs or ownership labels and leaves storage in place
when VM deletion has not finished. No finalizer is forcibly removed. Check
backend CSI retention separately. The journal is private operational state;
only run cleanup on trusted reports you created, with the original cluster
context. Cleanup does not restore the original run's success/failure verdict.

Once cleanup completes, remove the bridge on its host:

```bash
sudo ip link delete krh-demo
sudo nft delete table inet krh_demo
```

## Automated evidence and remaining lab work

```bash
go test -race -v ./internal/rehearsal
# Optional: keep JSON evidence from the simulated two-tier scenarios.
mkdir -p /tmp/kairon-rehearsal-evidence
KAIRON_REHEARSAL_TEST_EVIDENCE=/tmp/kairon-rehearsal-evidence \
  go test -race -count=1 ./internal/rehearsal -run TestTwoTierHTTPRehearsal
go test -race ./internal/kaironctl -run TestRehearsal
./scripts/lab-rehearsal.sh examples/rehearsal/compatible.json examples/rehearsal/breaking.json
```

Automated tests use a simulated Kubernetes/CSI/VM readiness API and **real
Python subprocesses, SQLite migrations, disk file copies and HTTP probes**.
They exercise compatible and breaking upgrades, a two-process web/database
scenario, multiple members and data volumes, readiness, failure paths, reserved
nodes, unsafe inputs, interrupted runs, lost create responses, cleanup ordering,
changed ownership/UIDs and report persistence. Guard tests validate nftables JSON
and reject absent bridges. They do not boot a VM or exercise a real CSI driver.
The lab script needs a configured, disposable cluster; it has not been executed
as part of the container test run. Native bridge/nftables enforcement and VM
lifecycle still need hardware qualification before this feature is promoted.
