# eBPF control-plane and migration performance patch

## Kairon changes

Drop-counter state is partitioned by Machine. An update scans only that
Machine's prior snapshot, preserving reset and eviction semantics. Input maps
are copied into owned state. Deleted/unassigned Machine baselines are pruned
only after a successful authoritative local Machine list. The agent processes
at most 256 reported series per VM response, even if FluxVM ignores its limit.
This reduces telemetry bookkeeping; it does not change packet verdicts.

Conntrack snapshots are validated on export, serialization, decode, and restore:
32,768 entries, at most 16 MiB encoded data, TCP/UDP/SCTP, valid same-family IP
addresses without zones, nonzero identity, export timestamp, and at most 64
bytes of state metadata. Oversized snapshots are rejected rather than truncated.
Existing migration callers can still choose their existing warning/fallback
behavior when conntrack export fails; this patch does not change that policy.

## Measured counter bookkeeping

Go 1.27.1, Linux amd64, AMD EPYC 9V74. Each Machine has 64 drop series;
measure one Machine's unchanged snapshot, three sequential runs of 200 ms.
The legacy comparison mirrors the parent implementation including its mutex.

| Machines tracked | Legacy median ns/op | Partitioned median ns/op |
|---|---:|---:|
| 1 | 2,886 | 2,871 |
| 100 | 101,318 | 3,232 |
| 1,000 | 1,557,080 | 2,950 |

Both paths allocate 48 B/op and 1 allocation/op in this unchanged-snapshot
fixture. The speed difference at scale comes from avoiding a whole-node scan.
This is not a packet-throughput, VM-startup, or end-to-end migration benchmark.

## Validation

Actual repository source and dependencies, Go 1.27.1:

* All 10 internal/ebpfedge tests passed with the race detector.
* All 11 targeted agent edge tests passed with the race detector, including
  successful-list cleanup, failed-list preservation, and response-series bounds.
* Six supporting packages passed race tests: migration, fluxvm, kube, agentplane,
  ratelimit, and ebpfedge.
* Agent tests excluding 22 CSI tests blocked by Unix-domain-socket permissions
  passed (170 top-level tests).
* Vet passed for agent, ebpfedge, fluxvm, migration, kube, agentplane, ratelimit.
* kairon-node built successfully. Formatting, license headers, and diff checks
  passed. The full repository test suite and a live VM migration were not run.

The first broad agent run failed its 22 CSI tests because this environment
cannot create their Unix sockets. This is recorded in the supplied logs; it is
not a successful full-suite run. The prior control-plane patch's original
Go 1.22 validation notes describe an earlier run; Go 1.27.1 validation above is
now also available for those affected packages.

Reproduce:

```sh
go test -race ./internal/ebpfedge ./internal/agent ./internal/migration ./internal/fluxvm ./internal/kube ./internal/agentplane ./internal/ratelimit
go vet ./internal/agent ./internal/ebpfedge ./internal/fluxvm ./internal/migration ./internal/kube ./internal/agentplane ./internal/ratelimit
go build ./cmd/kairon-node
go test -run '^$' -bench BenchmarkDropCounters -benchmem -count=5 ./internal/ebpfedge
```

## Remaining work

Shared kernel policy maps, configurable VM map sizes, XDP DDoS filtering,
EDT pacing, and new encryption integration are separate changes. The kernel
programs and their schema are unchanged by this patch. Network throughput,
p99 latency, total kernel map memory, and migration downtime need host tests.
