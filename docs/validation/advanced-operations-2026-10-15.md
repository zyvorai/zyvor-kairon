# Advanced operations validation — 2026-10-15

Experimental implementation of all five workflows. Based on upstream main
`b364cd3` (including topology-spread admission fixes), with the earlier Rehearsal
change rebased on that revision. This record supersedes the earlier Rehearsal-only
validation for the combined package.

Environment: Linux container, Go 1.27.2, Python 3, Helm 3.22.0 and
golangci-lint 2.14.0. TCP HTTP works. Unix-domain socket creation is prohibited;
there is no KVM/CSI/nftables hardware lab here.

## Automated checks

- All eight affected packages pass with the race detector: `internal/rehearsal`,
  `changeplan`, `incidentwork`, `scheduler`, `kube`, `model`, `kaironctl`,
  `controller`. The final run records 1,044 test/subtest pass events and two
  existing skips. Parent/subtest events are not a count of distinct scenarios.
- The three affected node-agent isolation tests pass with the race detector.
- Both two-tier HTTP upgrade scenarios pass 20 consecutive race-detector runs
  after fixing the fixture's port-file publication/readiness race.
- `golangci-lint run ./...`: zero issues. `go vet ./...`,
  `go build ./cmd/...`, repository validation, license headers and stdlib
  dependency boundary checks pass. Shell scripts pass `bash -n`.
- Examples are decoded/validated in tests. An integration test passes an archive
  written by the existing deep-incident writer through the new bundle binder.

```bash
go test -race ./internal/rehearsal ./internal/changeplan ./internal/incidentwork \
  ./internal/scheduler ./internal/kube ./internal/model ./internal/kaironctl ./internal/controller
go test -race ./internal/agent -run TestRehearsal
go test -race -count=20 ./internal/rehearsal -run TestTwoTierHTTPRehearsal
./scripts/test-advanced-operations.sh /tmp/kairon-operations-evidence
```

## Scenarios

| Scenario | Expected and observed result |
| --- | --- |
| Compatible schema upgrade | Real copied SQLite database and persistent HTTP server; baseline/upgrade/API pass; owned copies cleaned |
| Breaking schema upgrade | Upgrade exits zero, API returns 503; workflow fails as expected and cleans copies |
| Change plan commit | Real JSON Patch CAS against simulated API; both stages/probes pass; durable commit; reservations released |
| Failed post-change health | Reverse policy then VM compensation; rollback probes pass; original values restored |
| Lost forward API response | Inspect committed object/reservation, compensate safely; no blind forward replay |
| Journal unavailable before commit decision | Restart from last durable journal rolls back |
| Journal unavailable after durable commit | Restart completes reservation release without undoing committed changes |
| External edit / replacement UID | Refuses overwrite, retains NeedsRecovery evidence |
| Disk attachment failure later in plan | Checkpoint required; attached-name convergence; detach/restore configuration; user PVC retained |
| Offline recovery | Original Machine and PVC absent; restore from recorded spec/CSI point; real HTTP probe succeeds; RTO/point age recorded |
| Recovery objective failures | Old/unknown/future timestamp, too-small RTO, wrong driver, raw Block and boot failure are detected; owned copies cleaned |
| Incident reproduction and repair | Specific schema-failure exit expected; diagnose copy, repair table name, API passes; production bytes unchanged |
| Incident fails to reproduce | Stops before diagnosis/fix; a connectivity failure returns a different exit code from the schema signature |
| Malformed incident archive | Rejects unredacted/wrong identity, traversal, links, duplicate manifests; no archive execution |
| Network placement | Synthetic measured graph recommends a lower estimated cost without changing input Machines |
| Placement hard constraints | Capacity, anti-affinity, maxSkew, cordon, pressure, domains, pinning, cooldown, busy migrations and missing RTTs retained |
| Flow collection | Canonical existing route; actual elapsed counter deltas; no lifetime-counter rate estimate; detects resets/recreated flows and ambiguous IPs |

Infrastructure APIs, CSI and VM transitions are simulated. Application commands
are real Python subprocesses using independent SQLite files and actual TCP HTTP
servers/probes. Configuration transaction probes use a deterministic executor;
the fake API applies actual JSON Patch test operations and status transitions.
Network cost examples use synthetic telemetry and establish planner behavior,
not measured cluster improvement. The incident diagnosis adapter is controlled
in the application scenario; existing diagnosis/model libraries have their own
unit coverage. No live model provider was required or tested.

Ten supplied JSON evidence files cover the representative workflows. Their
breaking-upgrade report intentionally says `Failed`; that is the correct passing
test outcome. Reports tagged Committed/RolledBack are simulated infrastructure
outcomes. Use the test script to emit evidence again.

## Broader suite and remaining qualification

An unfiltered repository race run found 32 tests blocked by
`listen unix ... socket: operation not permitted` across migration-adapter-stub,
agent, consoleproxy, migration and uiapi. The earlier base had 30 such blocked
tests; this package is based on newer main with additional socket tests.
The first broad run also exposed a collector fixture whose recreated `firstSeen`
timestamp contradicted the new counter-reset guard; its simulated flow now keeps
a stable start time. A later broad run exposed the HTTP port-file race described
above. Both fixture problems are fixed and their affected tests pass.

The package includes the raw broad-run log and exact 32 environment-blocked
test names. A separate run uses Go's command-line `-skip` with those names only;
no socket tests are removed, altered or disabled in source. See the package's
`TEST_RESULTS.md` for that final run's counts. The unfiltered repository suite
is **not claimed green** in this container.

Native KVM boot, actual CSI snapshot/restore/retention and driver compatibility,
QGA behavior on the chosen images, bridge/nftables enforcement, host failure,
policy/disk convergence and real migration benefit remain unqualified here.
The lab script is supplied, not executed. Hardware qualification requires the
configured disposable cluster in [the operations guide](../guides/advanced-operations.md).
Transactions are compensating configuration changes, not distributed ACID or
guest-write rollback. Recovery age is backup metadata, not transaction-level
RPO. Incidents require declared failure signatures and fixes. Placement remains
proposal-only.
