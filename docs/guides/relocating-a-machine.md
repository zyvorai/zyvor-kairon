# Relocating a Machine

**Cold** — the proven path, needs shared or identically provisioned images:

```bash
kaironctl migrate demo --strategy cold --target-node worker-2
kaironctl evacuate worker-1          # batch cold/auto migrations off a node, budget-aware
kaironctl evacuate worker-1 --wait   # ...and keep retrying until the node is actually empty, like kubectl drain
```

**Live** — a secure peer handshake; you pick a *node*, never a raw destination:

```bash
kaironctl migrate demo --strategy live --target-node worker-2 \
  --mode pre-copy --bandwidth-mbps 800 --max-downtime-ms 200
```

```text
controller → source node ──mTLS──▶ target :9443
                      prepare + journal
             ◀── opaque transfer endpoint ──
source adapter transfers ──▶ target commit
                      adopt-only cutover
```

Enable it:

```bash
kubectl -n kairon-system create secret generic kairon-migration-tls \
  --from-file=ca.crt --from-file=tls.crt --from-file=tls.key
helm upgrade --install kairon ./charts/kairon -n kairon-system --set migration.enabled=true
```

Certificates need `serverAuth` + `clientAuth` and the chart's migration server name (default `kairon-node`) — see [`../migration-adapter.md`](../migration-adapter.md) for the full contract and fail-closed behavior.

If a commit's outcome is genuinely ambiguous, the migration lands in `NeedsRecovery` instead of a guess. Work through it via [`../runbook-migration-failures.md`](../runbook-migration-failures.md)'s decision tree, or resolve it straight from [getting-started — dashboard](../getting-started.md#deploy-the-web-dashboard).

A live migration that's still healthy but taking too long, or was aimed at the wrong node, doesn't have to run to completion: `kaironctl cancel-migration demo` (or the dashboard's "Cancel migration" button) aborts it while it's still safely reversible — `Starting`/`Running`, strictly before the destination commits — landing it `Cancelled` with the source runtime untouched. It's a no-op, refused locally with a clear error rather than silently ignored, once the migration has moved past `Cutover` (the destination has committed by then) or for a cold-strategy migration (nothing in-flight to abort).

Deleting a Machine while a `MachineMigration` still targets it is refused, not raced: `kairon-node` won't delete the source runtime until that migration reaches a terminal phase, so a delete issued mid-transfer waits rather than pulling the runtime out from under a live RAM/state stream.

A live migration also always resumes the source's own network dataplane when the migration stops short of a destination commit — blocked, an unsupported mode, a transfer that never started, or one that fails or is cancelled mid-flight. The source Machine stays the real, running VM on every one of those paths (only `NeedsRecovery`'s genuinely ambiguous case is left untouched, on purpose), so the network quiesce `kairon-node` takes out on it moments before the transfer begins is always undone again once the attempt is over — never left stranded through every future reconcile tick just because the migration didn't reach a commit.

---

