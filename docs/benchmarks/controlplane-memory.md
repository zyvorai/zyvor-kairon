# Control-plane memory and connection efficiency

This patch reduces audit read memory and bounds idle HTTP and rate-limit state.
It does not change VM guest RAM, VM boot time, or FluxVM execution performance.

## Changes

* Audit open and verification scan one record at a time rather than retaining
  the entire history. Live retained event memory is independent of record count;
  the scanner still permits records up to its existing 4 MiB limit. Total
  allocations and scan time still grow with the number of records.
* Filtered replay verifies the complete chain and retains matching events only.
  Replay of all events remains proportional to history size. Append durability,
  hashes, ordering, and corruption checks are unchanged.
* The Kubernetes transport caches up to 16 idle connections per host and 100
  overall, expires idle connections after 90 seconds, bounds dial and TLS setup,
  and attempts HTTP/2. Watches keep their timeout-free client. The transport has
  no hard active connection cap, so long-lived watches cannot consume all slots.
* Rate limiting tracks at most 65,536 keys by default. NewWithCapacity allows a
  different bound. At capacity, unknown clients get throttled until pruning
  frees space; existing clients retain their token state. The UI already prunes
  every five minutes with a 30-minute idle age. Deployments with greater client
  cardinality should choose a suitable capacity and prune schedule. This bounds
  key count, not arbitrary key string length.

## Local allocation comparison

Identical source fixtures, 512-byte diffs, three runs at 200 ms per benchmark,
Linux amd64, AMD EPYC 9V74, Go 1.22.2. Values below are medians of total allocated
bytes per operation, not peak heap or process RSS. Before and after benchmark
processes ran concurrently, so timing differences are not claimed as speedups.

| Operation | Before B/op | After B/op | Reduction |
|---|---:|---:|---:|
| Verify 10,000 records | 41,406,594 | 32,753,464 | 20.9% |
| Open 10,000 records | 41,412,626 | 32,761,298 | 20.9% |
| Replay one of 10,000 claims | 41,410,962 | 32,760,405 | 20.9% |
| Parallel empty Machine list via local HTTP | 9,392 | 6,160 | 34.4% |

The reused-key limiter benchmark reports 0 B/op and 0 allocs/op.
Raw output: [before](controlplane-memory/before.txt) and
[after](controlplane-memory/after.txt). The empty-list HTTP benchmark measures
client overhead and local connection churn; it is not a realistic fleet load.

## Validation and reproduction

With the repository toolchain (Go 1.27.1) the full module passes `go vet ./...`,
`go test ./...`, `golangci-lint` and race tests of the three affected packages.
Deployment tests and VM runtime benchmarks were not part of this change. No
repository Go version or external dependency was changed.

With the repository toolchain installed:

```sh
go test -race ./internal/agentplane ./internal/kube ./internal/ratelimit
go vet ./internal/agentplane ./internal/kube ./internal/ratelimit
make bench-controlplane
```

Use the same benchmark files on the parent revision for comparisons. Omit the
new capacity test there because NewWithCapacity does not exist on the parent.
Run baseline and patched benchmarks sequentially on the same idle host for
latency comparisons. For production validation, measure pod RSS, Go live heap,
GC CPU, goroutine count, API calls per reconcile, and p95/p99 reconcile latency
at 100, 1,000, and 10,000 Machines, with migration and audit traffic included.
