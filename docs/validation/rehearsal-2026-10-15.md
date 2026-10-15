# Kairon Rehearsal validation — 2026-10-15

The combined implementation is documented in [advanced operations validation](advanced-operations-2026-10-15.md); this is the earlier Rehearsal-only record.

Status: experimental implementation, automated scenarios passed; hardware lab
qualification remains open. Rebased onto `main` commit
`22cb559eae533dc22e88f13e99459db8c42a3212` before final affected-package checks.
Environment: Linux container, Go 1.27.2, Python 3, Helm 3.22.0,
golangci-lint 2.14.0. No KVM host/CSI deployment or native nftables lab was used.

## Final checks

| Check | Result |
| --- | --- |
| `go test -race ./internal/rehearsal` | PASS: 13 top-level tests, 41 parent/subtest pass events |
| Rehearsal statement coverage | 76.9%; native host bridge/sysfs paths remain unqualified |
| `go test -race ./internal/kaironctl ./internal/kube ./internal/model` | PASS |
| `go test -race ./internal/agent -run TestRehearsal` | PASS: refuses boot and requests immediate stop when isolation is invalid |
| `go test -race -count=10 ./internal/kaironctl -run TestLogsPrefixesAndFilters` | PASS after removing an unused, racy request recorder from the existing diagnostics fixture |
| `golangci-lint run ./...` | PASS, 0 issues on the rebased tree |
| `go vet ./...` | PASS |
| `go build ./cmd/...` | PASS |
| Repository validation / license headers / stdlib dependency boundary | PASS |
| Shell script syntax / Python compilation | PASS |

## Scenario results

The test API simulates Kubernetes objects, CSI snapshot/restore, VM readiness
and finalizers. Disk files are actually copied. Guest commands run as real
Python subprocesses against independent SQLite databases. A persistent HTTP
server reads the database copy; a separate web probe process calls it.

| Scenario | Baseline | Schema change | HTTP verification | Cleanup |
| --- | --- | --- | --- | --- |
| Add an optional column | PASS | Exit 0 | HTTP 200, PASS | All owned API resources deleted; reservation released |
| Rename the table the application queries | PASS | Exit 0 | HTTP 503, correctly FAILS the rehearsal | All owned API resources deleted; reservation released |

Additional tests cover multiple Machines and data volumes, WaitForFirstConsumer
ordering, QGA readiness retry, snapshot/boot failures, cancelled waits, lost
create responses, baseline/setup failures, cleanup retry, foreign ownership,
replaced UIDs, existing snapshot retention, reservation conflicts, unsupported
storage/device inputs, shared-claim refusal, plan typos and durable private
reports. Scenario assertions confirm source Machine data and source disk bytes
remain unchanged. Guest commands never target a source Machine.

To emit the supplied JSON scenario evidence again:

```bash
mkdir -p /tmp/kairon-rehearsal-evidence
KAIRON_REHEARSAL_TEST_EVIDENCE=/tmp/kairon-rehearsal-evidence \
  go test -race -count=1 -v ./internal/rehearsal -run TestTwoTierHTTPRehearsal
```

The breaking report intentionally has `phase: Failed`; detecting that upgrade
is the expected passing test outcome. The supplied reports describe simulated
infrastructure, not a deployed VM cluster.

## Broader suite limitations

An unfiltered `go test -race ./...` run was also attempted. Thirty existing
tests across agent, consoleproxy, migration and uiapi could not create Unix
sockets: `socket: operation not permitted`. The same failures were reproduced
on the unchanged starting commit `8fb44135575b1c97b25e490959ba205f7db404ce`.
An initial Helm CLI test also failed because Helm was missing; installing Helm
resolved it, and the final full CLI package passes with the race detector.
The final affected-package run exposed an intermittent race in an existing
CLI diagnostics fixture's unused request slice. Removing that recorder fixed
it; the offending test also passed ten consecutive race-detector iterations.

The entire repository test suite is therefore **not claimed green in this
container**. No socket-test failures were suppressed or changed. The hardware
scenario script was supplied, not executed. Native KVM boot, real CSI snapshots,
bridge/nftables enforcement, host failure, real TTL deletion and production
application consistency need the configured lab described in
[the guide](../guides/rehearsal.md).
