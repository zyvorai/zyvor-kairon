---
sidebar_position: 8
title: Migration adapter
---

# Kairon migration adapter contract

The adapter isolates hypervisor-specific migration mechanics from Kairon's Kubernetes API. It is a local HTTP service on a root/admin-controlled Unix socket. Kairon never exposes this socket to workload Pods.

`cmd/kairon-migration-adapter-fluxvm` implements this contract against real FluxVM migration/QMP endpoints -- it is not a stub (see `cmd/kairon-migration-adapter-stub` for the test double used in unit/integration tests). It is not installed automatically by `scripts/deploy-remote.sh` or the Helm chart today; see [`docs/runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md) for how to deploy and wire it up.

## Destination API

`POST /v1/destination/prepare`

Input is `migration.PrepareRequest` containing the immutable session identity. Response is:

```json
{
  "transferSupported": true,
  "endpoint": "opaque-backend-endpoint",
  "backend": "qemu",
  "dataPlaneEncrypted": true
}
```

The endpoint is opaque to Kubernetes and is sent only from the target peer to the authenticated source peer, then to the local source adapter.

`dataPlaneEncrypted` reports whether *this* receiver's RAM/state stream was actually configured with TLS (independent of the always-on mTLS control-plane RPCs between kairon-node peers) -- it flows through to `MachineMigration.status.dataPlaneEncrypted` and the `kairon_migration_dataplane_encrypted` metric. An adapter that predates this field can simply omit it; it decodes to `false`, the conservative assumption.

The session carries three fields the destination may need to build its receiver:

- `sourceRecord` -- the source runtime's FluxVM VM record, verbatim. The FluxVM adapter launches its receiver from this record, so a destination without it answers `transferSupported: false`.
- `stream` -- the migration stream capabilities both ends must agree on before the transfer starts: `multifdChannels`, `compression` (`{"algorithm": "zstd"|"zlib"|"none", "level": N}`) and `postCopy`. QEMU fixes these at stream start; they cannot be changed mid-flight.
- `destinationRuntimeID` is set by the destination, not the source: when the receiver runs as a distinct runtime (FluxVM's receiver VM), prepare returns its ID as `destinationRuntimeID`. Kairon then restores the guest network snapshot on that runtime after commit instead of at prepare, because the receiver only owns the guest once it is adopted.

`POST /v1/destination/{sessionID}/commit`

Makes the transferred target runtime authoritative/runnable according to backend semantics. It must be idempotent. The FluxVM adapter adopts the receiver here, retrying while FluxVM reports the incoming stream is still in flight.

`POST /v1/destination/{sessionID}/abort`

Destroys or invalidates the prepared target. It must be idempotent before commit, and refuses (409) after a commit succeeded.

## Source API

`POST /v1/source/start`

Input is `migration.SourceRequest`: immutable session identity, FluxVM runtime ID, opaque target endpoint, and pre-copy/post-copy/bandwidth/downtime/multifd hints. Response is `migration.TransferStatus` and must contain an opaque `transferID` for asynchronous transfers.

`GET /v1/source/{sessionID}/{transferID}`

Returns transfer progress. Terminal phases understood by Kairon are `completed`/`transferred`, `failed`, `cancelled`/`canceled`, and `aborted`.

`POST /v1/source/{sessionID}/{transferID}/abort`

Cancels the source transfer where safe.

### Optional routes

An adapter that doesn't implement these answers 404; Kairon then runs the transfer with the start-time settings only and records that adaptive control is unsupported.

`GET /v1/capabilities` returns `migration.Capabilities`: `tune`, `postCopySwitch`, `autoConverge`, `compression` (algorithm list), `postCopyWithMultifd` (QEMU 10.0 or later) and `finish`.

`POST /v1/source/{sessionID}/{transferID}/tune` takes `migration.TuneRequest` (`bandwidthMbps`, `maxDowntimeMs`, `cpuThrottleInitialPct`, `cpuThrottleIncrementPct`, `maxCPUThrottlePct`; zero leaves a value unchanged) and applies it to the running transfer.

`POST /v1/source/{sessionID}/{transferID}/postcopy` switches an armed transfer (`postCopy: "armed"` at start) to post-copy. After this the destination owns the guest's live state: Kairon never resumes the source on a later failure and moves the migration to `NeedsRecovery` instead.

`POST /v1/source/{sessionID}/{transferID}/finish` releases the paused source runtime after the destination committed. Kairon calls it best-effort.

`TransferStatus` may carry `postCopyActive` and `telemetry` (dirty page rate, page size, duplicate pages, throughput, expected downtime, CPU throttle, post-copy requests); Kairon reports them on `MachineMigration.status.progress`.

### Mixed versions

The peer prepare endpoint rejects unknown fields, so a source node sending `sourceRecord`/`stream` to an older destination node fails prepare. Upgrade kairon-node on both ends of a migration together.

## Trust boundary

- Socket ownership/permissions are an administrator responsibility.
- Never accept shell commands or command strings in the adapter API.
- The adapter must validate session/runtime identity independently before destructive operations.
- Target `commit` is the point after which Kairon treats rollback as potentially ambiguous and uses `NeedsRecovery` on commit errors.
- Authentication between nodes is mTLS; the Unix adapter is intentionally node-local only.
