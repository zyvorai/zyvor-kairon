# Shipped in v0.5.0: the 24-fix/feature batch recorded in RELEASE_NOTES.md at the time -- no live-cluster verification this round

This file's own convention is specifically **real, live-cluster/live-host verification beyond source-level gates** -- every entry below documents an actual deployment, a real API call against a running FluxVM/Kubernetes, or a bug a real environment surfaced that unit tests alone didn't. Being honest about that bar: the batch of 24 fixes/features described in RELEASE_NOTES.md's newest `## Added`/`## Changed` entries (MachineQuota hotplug self-check through the migration heartbeat/reaper) was verified via `go build`/`go vet`/the full `go test ./...` suite (including `-race` for the migration heartbeat/reaper's concurrency-critical path, run repeatedly), `gofmt`, `helm lint`, `helm template`, and `python3 scripts/validate.py` -- real, and in several cases (the reaper's per-session locking) deliberately adversarial test design proving a specific race can't happen -- but none of it was exercised against an actual running Kubernetes cluster or a live FluxVM host. No live cluster/host was available in this working session. Recorded here plainly rather than either skipping the gap silently or writing an entry that reads like the live-verification bar above was met when it wasn't.

# Pre-v0.10.3 hardening (2026-11-10): what was and was not verified

## Result

**PASS for the items listed under "Verified"; the items under "Not verified" have no live evidence.** This is not a claim that every feature is tested: experimental features keep the status in [`docs/STATUS.md`](docs/STATUS.md) and [`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md).

## Verified

- **Combined `main` after the hardening PRs (#202-#211, #213):** `gofmt`, `go build ./...`, `go vet ./...`, `go test -count=1 ./...` (no failures or panics) and `helm lint charts/kairon` on a Linux x86_64 host (Go 1.27.3). Packages touched by the changes were also run with `-race` where noted below.
- **Fresh install on a real host** (k3s v1.37.2, KVM, Ubuntu 26.04, FluxVM 0.4.18 from the checksum-verified `.deb`, `kaironctl` v0.10.2 from the release with `SHA256SUMS` verified): `kaironctl doctor` 7 OK, `kaironctl selftest --profile standard` 6 passed (control-plane, boot, power, pause-resume, hotplug-cpu, observability) and 2 skipped (console-log needs the dashboard; network could not reach the forwarded port from outside the host). The existing workloads on that cluster were unaffected.
- **Defects found by that install:** the chart's default image tag (`0.10.2`) did not exist (images are `v0.10.2`) and the node image-cache directory must be owned by UID 65532. Both fixed and documented.
- **`kaironctl upgrade` applying the chart's CRDs:** run against the same real cluster (41 Kairon CRDs unchanged, release healthy, `doctor` 7 OK); unit tests in `internal/kaironctl`.
- **Leader election:** `go test -race ./internal/leaderelection` including a test where the renew call hangs (leadership must end within the renew deadline).
- **Security changes** (OIDC subject namespacing and `email_verified`, removed-user sessions, viewer scope, security headers, OIDC login-CSRF cookie, link-local download refusal, symlink-safe image paths, login-state eviction): unit tests in `internal/uiapi` and `internal/agent`.
- **Chart:** rendered with `helm template`; server-side dry-run of the rendered chart accepted by the cluster's API server; the production profile fails with a clear message when the webhook TLS values are missing.

## Verified on the published release candidate (`v0.10.3-rc2`, 2026-11-11)

- **Release pipeline, end to end on a real tag:** all six images passed the HIGH/CRITICAL vulnerability scan (rc1 had been stopped by CVE-2026-78661 in `golang.org/x/net`, fixed in 0.62.0), were pushed and signed, and the chart, CLI binaries, version-pinned manifests tarball and `SHA256SUMS` (11 entries) were published as a GitHub **pre-release**. `latest` and the MCP Registry were not touched. One step failed because the runner lacked the GitHub CLI; fixed on the runner and re-run.
- **Fresh install from the published chart with default image tags** on a real k3s + KVM host: controller and node pulled `v0.10.3-rc2` and were Ready (v0.10.2's default install failed with `ImagePullBackOff`); `doctor` 7 OK; `selftest --profile standard` 6 passed; `kaironctl upgrade` against the published chart OK; the production profile without webhook values fails with a clear message.

## The final v0.10.3 tag (2026-11-11): what the evidence covers

- `v0.10.3` points at the same commit as `main` at release time (`ead3dcf`); the release workflow run for the tag and the CI run on that commit both concluded `success` (GitHub Actions). The release is marked Latest and carries the chart (`kairon-0.10.3.tgz`), the version-pinned manifests tarball, the CLI and `kubectl-kairon` binaries for linux amd64/arm64, `kairon_{linux,darwin}_{amd64,arm64}.tar.gz`, `kairon.yaml` and `SHA256SUMS`.
- `v0.10.3-rc3` and `v0.10.3` differ only in `VERSION`, `charts/kairon/Chart.yaml` and `server.json` (`git diff --stat`). `v0.10.3-rc2`, the build that was installed on the real host, differs from the final tag in `go.mod` (the dependency upgrades of #225 and the Go 1.27.4 bump of #226) and in CI files; nothing under `internal/` or `cmd/` changed between rc2 and the final tag. **No install of the final `v0.10.3` or `rc3` images on a real host has been recorded**; the install evidence above is for rc2.
- After the release, the docs for the install, upgrade and production-profile paths were re-checked against the chart and CLI: `helm lint charts/kairon` passes; `helm template` renders `ghcr.io/zyvorai/kairon-{controller,node}:v0.10.3` when the image tags are empty; the production profile without webhook values stops with the chart's message and renders with them; every `kaironctl` subcommand and flag named in the docs was compared with `kaironctl --help` output built from the same commit. None of this boots a Machine.

## Not verified

- The release workflow changes (`promote-latest`, tag/chart check, pre-release handling): `actionlint` only. Only a real tag push exercises them.
- OIDC against a real identity provider (tests use a fake provider).
- The admission webhook against a real API server (unit tests only), including the new create-only-field warning.
- Dashboard behaviour in a browser after the security-header and session changes.
- Live migration, multi-node, GPU, NVMe-oF, DR and macOS features: unchanged status in `docs/STATUS.md`.
- Backup/restore failure reporting and `qemu-img` timeouts: unit-level only, no live failure injection.
- A real-host install of the final `v0.10.3` images (see above), and an upgrade from a v0.10.2 install (including clearing its image-tag overrides and the one-time OIDC session reset).
- The `kairon-ui-users` Secret editing procedure in the dashboard guide and the `ui.auth.users` first-install-only behaviour: read from the chart template, not run against a cluster.

# Shipped in v0.5.0: guest-agent real guest-IP reporting test report

## Result

**PASS.** `make all` green (coverage 68.1%, threshold 50%). `helm lint` green after fixing a real YAML indentation bug the CRD schema edit introduced (a stray `minLength: 1` line from the pre-existing `deviceClaims` block got orphaned under the new `guestAgent` block by an imprecise edit boundary) -- caught immediately by re-running the gauntlet, not shipped. New unit tests: `fluxvm.BestGuestIP` (picks the first IPv4 on the first non-loopback interface, skips loopback/IPv6-only interfaces, handles no interfaces), `fluxvm.QGANetworkInterfaces` decoding a real response shape, and `internal/agent.projectNetworkStatus`'s three guest-IP-resolution cases (falls back to QGA when no lease IP and guest agent enabled; preserves an already-known IP without calling QGA at all; skips QGA entirely when not enabled).

## What shipped

`spec.guestAgent.enabled` opts a Machine into FluxVM's real `qemu-guest-agent` (virtio-serial) channel; `kairon-node` then resolves `status.guestIP` via FluxVM's `guest-network-get-interfaces` whenever the existing DHCP-lease path comes up empty -- which is *every* network mode except `tap` with `netns: true`, `user`/SLIRP (Kairon's own default) included. This needed a real FluxVM-side addition first: `GET /v1/vms/{id}/qga/network-interfaces`, shipped upstream as [zyvorai/fluxvm#63](https://github.com/zyvorai/fluxvm/pull/63) (merged) after confirming FluxVM already had the full host-side QGA client/REST for `ping`/`exec`/`powershell`/firewall rules, just not this one verb. A related premise was checked and found false before any code was written: FluxVM's `stop`/`delete` already performs a graceful ACPI `system_powerdown` with a wait-then-force-kill fallback for *every* VM regardless of guest-agent presence, so no separate Kairon-side "guest-agent shutdown" wiring was needed at all -- avoided building a redundant mechanism by checking the real behavior first instead of assuming the original gap description was accurate.

## Real verification (beyond source-level gates)

- FluxVM's new endpoint was verified against a real running VM before any Kairon-side code existed: `network.mode: user` (no lease file), a real `qemu-guest-agent` installed via cloud-init, confirmed responsive via `qga/ping`, then `qga/network-interfaces` correctly reported the guest's real SLIRP-assigned address (`10.0.2.15`).
- Upgraded the production `fluxvm` service on the real lab host a second time (to the build containing both merged PRs, #62 and #63) -- confirmed existing VMs survived, same pattern as the hotplug round.
- Deployed the updated `kairon-node` binary and `Machine` CRD schema to the same real cluster.
- Created a real Machine with `spec.guestAgent.enabled: true`, default `user`-mode networking, and `spec.cloudInit.packages: [qemu-guest-agent]`. `status.guestIP` was correctly empty immediately after creation (the guest agent hadn't installed/started yet), then correctly resolved to `10.0.2.15` a couple of reconcile ticks later once it had.
- Independently confirmed via a direct call to FluxVM's own `qga/network-interfaces` endpoint (bypassing Kairon's cached `status.guestIP` entirely) that the guest genuinely reports that address -- the real end-to-end chain, not just Kairon trusting its own prior read.
- Cleaned up the test Machine and all temporary build artifacts on the host afterward.

## Coverage

```text
total (internal/...): 68.1% (threshold 50%)
```

# Shipped in v0.5.0: CPU/memory hotplug test report

## Result

**PASS.** `make all` green (coverage 68.0%, threshold 50%). `helm lint` green; the `Machine` CRD's status schema updated (`appliedVCPUs`/`appliedMemoryMiB`) after confirming it's structural and would otherwise silently prune the new status fields on a real cluster -- caught before shipping, not after. New unit tests in `internal/agent`: a table test for the three cases `reconcileHotplug` must never call FluxVM for (freshly created seeds the baseline, no recorded baseline assumes already-realized, a shrink request is ignored), a growth test asserting the exact delta sent to FluxVM, a failure test proving `status.applied*` doesn't advance on error (so the same delta retries next tick), and a full-`Reconcile` integration test against a fake Kubernetes+FluxVM server pair.

## What shipped

This item needed a real FluxVM-side capability that didn't exist: FluxVM had already reserved CPU/memory hotplug headroom at every VM's boot (`-smp maxcpus=`, `-m slots=/maxmem=`) since `max_vcpus`/`max_memory_mib` were added, but nothing in that repo ever issued the actual `device_add`/`object-add` QMP calls to use it -- confirmed by reading the code rather than assuming, since the obvious-looking `/v1/vms/{id}/resources` endpoint turned out to be host-side cgroup throttling only, not guest-visible hotplug at all. Added real QMP hotplug to FluxVM (`POST /v1/vms/{id}/hotplug/cpu|memory`), shipped as [zyvorai/fluxvm#62](https://github.com/zyvorai/fluxvm/pull/62) (merged), then wired Kairon up to it: `kairon-node` computes the delta between `spec.resources` and the newly added `status.appliedVCPUs`/`appliedMemoryMiB`, calling FluxVM's hotplug endpoints for just that delta. `status.applied*` is the only source of truth for what's been realized, because FluxVM itself has no query endpoint for current live vcpus/memory -- hotplugged state is pure QMP-runtime, never persisted back into FluxVM's own VM record.

## Real verification (beyond source-level gates)

The FluxVM side was independently verified against real KVM before this Kairon-side work even started (see the FluxVM PR's own test report: a real second vCPU thread and a real DIMM, both confirmed via raw QMP queries bypassing the new code's own success reporting). For the Kairon integration specifically:

- Upgraded the **production** `fluxvm` service on the real lab host (built fresh from FluxVM's merged `main`) -- a real, live-infrastructure upgrade, not an isolated side instance this time, done only after confirming already-running QEMU VMs survive a control-plane restart (they do -- hotplug's own verification round had already shown this) and after explicit confirmation given the host runs more than just Kairon.
- Deployed the updated `kairon-node` binary and the updated `Machine` CRD schema to the same real cluster.
- Created a real Machine (`cpu: "1", memory: 512Mi`); confirmed `status.appliedVCPUs=1`/`appliedMemoryMiB=512` seeded correctly at creation, with zero FluxVM hotplug calls made (this is the baseline, not a hotplug event).
- Patched `spec.resources` to `cpu: "2", memory: 768Mi`; `kairon-node`'s next reconcile tick correctly called FluxVM's hotplug endpoints for just the delta (+1 vCPU, +256Mi), and `status.appliedVCPUs=2`/`appliedMemoryMiB=768` reflected the new reality.
- Independently confirmed via a raw QMP query directly against the VM's own `qmp.sock` (bypassing both Kairon's and FluxVM's own status reporting entirely): a genuine second vCPU thread (`qom-path: /machine/peripheral/cpu-hotplug-1`) and a genuine 256MiB DIMM (`dimm-hotplug-0`, `hotplugged: true`) -- the genuine end-to-end chain, not just two systems agreeing with each other.
- Cleaned up the test Machine and all temporary build artifacts on the host afterward.

## Coverage

```text
total (internal/...): 68.0% (threshold 50%)
```

# Shipped in v0.5.0: MachineSnapshotRestore test report

## Result

**PASS.** `make all` green (coverage 68.2%, threshold 50%; `scripts/validate.py` updated for the new `MachineSnapshotRestore` CRD). `helm lint` and `helm template --include-crds` green, confirming the new CRD and `kairon-controller`'s new `persistentvolumeclaims` `get`/`create` RBAC both render. New unit tests in `internal/controller`: `selectSnapshotVolume` (auto-picks the only volume, requires a name when ambiguous, rejects an unknown name) and `reconcileSnapshotRestore` against a fake Kubernetes server (rejects a not-yet-ready `MachineSnapshot`, creates a real-shaped PVC with `spec.dataSource` pointing at the right `VolumeSnapshot` and sized from its `status.restoreSize` when not overridden, reports `Pending` until the PVC is `Bound` then `Succeeded`, is idempotent against an already-existing PVC, is a no-op once already terminal).

## What shipped

`MachineSnapshotRestore` restores one volume of a `Succeeded` `MachineSnapshot` into a brand-new `PersistentVolumeClaim` via the standard CSI `spec.dataSource` flow. A real, load-bearing design finding drove the scope: **there is no such thing as an in-place PVC restore in Kubernetes** -- a bound PVC's `dataSource` can't be changed after the fact, so "restore" always means "create a new PVC," which makes it identical to "clone-from-snapshot." Combined with `Machine.spec.volumes` already being creation-time-only (same as `spec.image`/`spec.resources`), restoring into an *existing* Machine isn't possible either -- so this deliberately restores into a new PVC only, and documents pointing a new `Machine` at it as the second, already-solved step (`docs/guides/machine-storage.md`), rather than duplicating Machine-spec-templating logic inside this CRD. This also means it keeps working in the actual disaster-recovery case that matters (the original Machine is gone), which a design that copied the original Machine's spec wouldn't. New `kaironctl restore SNAPSHOT --target-claim NAME` / `kaironctl get restores`; `kairon-controller`'s ClusterRole gained `get`/`create` on `persistentvolumeclaims`.

## Real verification (beyond source-level gates), and a real environmental limit found along the way

Deployed to the same real lab k3s host used throughout this project's verification, and hit a genuine, useful finding in the process: **this cluster's default StorageClass (Rancher's `local-path-provisioner`) is not a real CSI driver and cannot support VolumeSnapshots at all** -- a real `VolumeSnapshot` created directly against it failed with `cannot find CSI PersistentVolumeSource` / `snapshotting non-CSI volumes is not supported`. This is a pre-existing limitation of that storage class, not a Kairon bug, and it means the success path (an actually-completed snapshot restored for real) can't be exercised on this particular lab environment -- documented plainly in `docs/guides/machine-snapshot-restore.md` and `README.md` rather than glossed over.

What **was** verified for real, end to end through genuine Kubernetes API round-trips:
- Applied the new CRD and RBAC to the live cluster; both took effect immediately (`kubectl get crd` confirmed).
- Created a real `Machine` + `MachineSnapshot` against a real (non-CSI) PVC; `kairon-controller` correctly reconciled the `MachineSnapshot` to `Failed` with the real underlying CSI error.
- Created a real `MachineSnapshotRestore` referencing that failed snapshot; `kairon-controller` correctly reconciled it to `Failed` with `MachineSnapshot default/... is not ready to restore from yet (phase="Failed")` -- the exact real error path this feature is supposed to surface clearly rather than hang or silently retry forever.
- Confirmed `kaironctl restore` and `kaironctl get restores` both work correctly against the live API server under a purpose-built operator identity (same RBAC-boundary-verification pattern as the disruption-budget and quota rounds).
- Cleanup hit one more real, useful finding: the source PVC got stuck `Terminating` behind a `snapshot.storage.kubernetes.io/pvc-as-source-protection` finalizer left orphaned by the failed (non-CSI) snapshot attempt -- confirmed no `VolumeSnapshot`/`VolumeSnapshotContent` objects still referenced it, then removed the finalizer directly, a standard, safe remediation for exactly this class of stuck finalizer.

The success path (real `Succeeded` snapshot restored into a real `Bound` PVC) is covered by the fake-Kubernetes-server unit tests instead, consistent with this project's established practice of being explicit about what's verified on real hardware versus in tests when real hardware genuinely can't exercise a path (e.g. TEST_REPORT.md's v0.3.0 note about no real KVM/multi-host environment in CI).

## Coverage

```text
total (internal/...): 68.2% (threshold 50%)
```

# Shipped in v0.5.0: MachineQuota test report

## Result

**PASS.** `make all` green (coverage 68.6%, threshold 50%; `scripts/validate.py` updated for the new `MachineQuota` CRD, same "must expose v1alpha1 + status" convention as every other Kairon CRD). `helm lint` and `helm template --include-crds` green. New unit tests in `internal/controller`: `buildQuotaTrackers`/`admitQuota` (blocks once `maxMachines` reached, allows-and-spends within one reconcile pass so a burst can't all slip through, enforces `maxTotalCpu`/`maxTotalMemory`, ignores namespaces with no quota, doesn't count unscheduled/Stopped Machines as used, rejects malformed quantities) plus a full-`Reconcile` integration test proving the controller leaves a real over-quota Machine `Pending` with a clear message and patches the `MachineQuota`'s status.

## What shipped

`MachineQuota` (namespace-scoped `maxMachines`/`maxTotalCpu`/`maxTotalMemory`) is enforced entirely inside `kairon-controller`'s existing scheduling loop -- no admission webhook, matching the project's "no vendored operator framework, no webhook cert-management infra" posture and directly reusing the same "block into Pending, don't reject at the API" pattern the migration concurrency quota already established. Scoped by Kubernetes namespace rather than `Machine.spec.tenant`, which turned out to be completely unused in Go code (only ever forwarded to FluxVM's own `tenant` field as `m.Namespace()` anyway) -- a real finding from reading the code before committing to a scoping design, not an assumption. `kairon-controller`'s ClusterRole gained `get/list/watch/patch` on `machinequotas`/`machinequotas/status`. New `kaironctl get quotas` for observability parity with `get machines|migrations|snapshots`.

## A real ordering bug caught before it shipped

The first implementation checked quota *before* calling `Scheduler.Choose`. That's wrong: if a Machine passed the quota check but then had no eligible node (placement/affinity constraints), its quota "spend" was never rolled back -- falsely reserving capacity for the rest of that reconcile pass and starving a later Machine in the same tick that could actually have been scheduled. Caught during implementation review (not by a failing test -- the existing test suite's single-Machine-per-tick tests wouldn't have exposed a multi-Machine-per-pass ordering bug), fixed by moving the quota check to only run once `Choose` has already found an eligible node.

## Real verification (beyond source-level gates)

Deployed to the same real lab k3s host used throughout this project's verification:

- Applied the updated CRD (`machinequotas.kairon.zyvor.dev`, newly created) and RBAC, restarted `kairon-controller`.
- Created a real `MachineQuota` (`maxMachines: 1`) and two real Machines. The first reached `Running` on the real node; the second was correctly left `Pending` with `MachineQuota default/quota-test: maxMachines 1 reached` -- and the `MachineQuota`'s own `status` was patched with `usedMachines: 1, usedTotalCpuCores: 1, usedTotalMemoryMiB: 512`, matching the one real Machine that actually got scheduled.
- Confirmed `kairon-node`'s own service account correctly gets a real `403 Forbidden` reading `machinequotas` (it was deliberately not granted that RBAC -- only `kairon-controller` needs it), then used a purpose-built operator identity (matching how a real human operator's kubeconfig would be scoped) to run `kaironctl get quotas` against the live cluster and confirmed it printed the real quota and usage.
- Cleaned up all test Machines/quota/RBAC afterward.

## Coverage

```text
total (internal/...): 68.6% (threshold 50%)
```

# Shipped in v0.5.0: Machine affinity/anti-affinity + MachineDisruptionBudget test report

## Result

**PASS.** `make all` green (coverage 68.4%, threshold 50%; `scripts/validate.py` updated for the new `MachineDisruptionBudget` CRD). `helm lint` and `helm template --include-crds` green, confirming the new CRD renders (Helm only renders `crds/` content with `--include-crds`, a real gotcha hit while verifying this). New unit tests: scheduler-level table test for affinity/anti-affinity (co-location required/satisfied, self-exclusion, "nothing matches" for both directions), a controller-level integration test proving `Reconcile` respects anti-affinity across two real `Machine` objects end-to-end, model-level tests for `ParseIntOrPercent`/`DesiredHealthy` (plain integer, percentage rounding-up, invalid input, both-or-neither-set rejection), and `kaironctl`-level tests for the budget-gating logic (allows-until-exhausted, in-flight migrations counted against `currentHealthy`, the stricter of multiple matching budgets wins, unmanaged Machines always allowed).

## What shipped

- `spec.placement.affinity`/`antiAffinity`: required (hard) constraints only, evaluated as scheduler filters against every other currently-scheduled Machine (`internal/scheduler.termSatisfied`). `Scheduler.Choose` and `Controller.migrationTarget` both gained a `machines []model.Machine` parameter to make this possible with no new API calls (the full Machine list is already fetched every reconcile tick).
- `MachineDisruptionBudget` CRD + `kaironctl evacuate` gating: `internal/model.MachineDisruptionBudgetSpec.DesiredHealthy` (Kubernetes PDB-style `minAvailable`/`maxUnavailable` resolution), `kube.Client.ListMachineDisruptionBudgets`, and `cmd/kaironctl`'s `loadBudgetStates`/`admitDisruption` (per-budget allowance computed fresh on every `evacuate` invocation, spent across every budget a Machine matches, in-flight migrations counted as already-unavailable so they aren't double-spent).

## Real verification (beyond source-level gates)

Both features were deployed to the same real lab k3s host (`212.8.248.187`) used throughout this project's other real-deployment verification. The host is single-node, so genuine cross-node anti-affinity/evacuate-across-nodes outcomes can't be produced there -- exercised what's honestly exercisable on real infrastructure and said so:

- **Affinity, same-node case, for real**: applied the updated CRD (schema previously didn't declare `placement.affinity`/`antiAffinity` at all -- a real, structural-schema-pruning risk caught before it could silently strip the fields on a real cluster) and the updated `kairon-controller` binary. Created a real anchor `Machine` and a real dependent `Machine` with an `affinity` term requiring co-location via `kubernetes.io/hostname`. The dependent Machine failed to schedule on the first reconcile tick (a real, observed timing window -- the anchor hadn't been assigned `spec.nodeName` yet in that tick's in-memory Machine list) and succeeded on the next tick once the anchor had a node, reaching `Running` with a real `runtimeID` -- this exact timing behavior is now documented in `docs/guides/machine-placement.md` because it was observed, not guessed.
- **`MachineDisruptionBudget`, for real**: applied the new CRD (created cleanly; caught and fixed a `scripts/validate.py` failure -- the script enforces every CRD exposes a `status` subresource, which the new CRD didn't yet, since nothing reconciles it -- fixed by adding an unused `status: {}` subresource for schema consistency with every other Kairon CRD rather than special-casing the validator). Created a real `MachineDisruptionBudget` (`minAvailable: "2"`) and three real `tier: web` Machines, all reaching `Running`. Ran the updated `kaironctl evacuate` against the real node using a purpose-created `ServiceAccount`/`ClusterRole` (a realistic stand-in for an operator's own kubeconfig, since kairon-node's own service account is deliberately *not* granted read access to this new resource -- confirmed by a real `403 Forbidden` when first tried with that identity, proof the RBAC boundary is real). `evacuate` queued exactly 1 of 3 (a real `MachineMigration` object, confirmed via `kubectl get`), correctly skipped the other 2 with a clear per-machine reason, and exited `1`.

## Coverage

```text
total (internal/...): 68.4% (threshold 50%)
```

# Shipped in v0.5.0: PVC-backed boot disk test report

## Result

**PASS.** `make all` green (coverage 68.1%, threshold 50%). `helm lint` and `helm template` green. New unit tests cover `resolveBootDiskPath`/`hostDirForPV` (fallback to `spec.image.path`, missing `claimName`, unbound PVC, `Block` volumeMode rejection, unsupported volume source rejection, `hostPath`/`local` resolution) plus a full-reconcile integration test proving a Machine with only `spec.volumes` set (no `spec.image.path`) reaches FluxVM's create call with the PVC-resolved image path, and that the resolved path is exempt from the `--image-root` allowlist.

## What shipped

`spec.volumes[0]` now actually does something: `kairon-node` resolves the named `PersistentVolumeClaim` (must be `Bound`) to its `PersistentVolume`, accepts only `Filesystem`-mode volumes backed by `hostPath` or `local` sources, and boots the Machine from `<that directory>/disk.img` instead of requiring `spec.image.path`. New `internal/kube.Client.GetPersistentVolumeClaim`/`GetPersistentVolume`; new RBAC (`get` on `persistentvolumeclaims`/`persistentvolumes`) on `kairon-node`'s ClusterRole in both the Helm chart and raw `deploy/rbac.yaml`. The `Machine` CRD's `spec.image` field is no longer schema-required (`deploy/crd.yaml`, `charts/kairon/crds/machines.yaml`) — a Machine can now be created with only `spec.volumes`, validated at the Go level instead (`spec.image.path or spec.volumes[0] is required`).

## Real verification (beyond source-level gates)

Deployed the updated `kairon-node` binary to the same real lab host (`212.8.248.187`, a real k3s cluster) used throughout this project's other real-deployment verification, and ran the feature against **actual dynamic provisioning**, not a hand-crafted PV:

1. Created a real `PersistentVolumeClaim` against the cluster's real default `local-path` StorageClass (Rancher's `local-path-provisioner`, already installed). Its `VolumeBindingMode: WaitForFirstConsumer` meant the PVC stayed `Pending` until a consumer was scheduled — used a disposable trigger Pod (whose own image pull failed, irrelevant -- scheduling alone was enough) to force provisioning, then deleted it.
2. **Real, unscripted discovery**: the provisioned `PersistentVolume` used `spec.local.path`, not `spec.hostPath.path` — confirming the decision to support both sources (not just `hostPath`) in `hostDirForPV` was correct, not a hypothetical.
3. Copied a real qcow2 base image to `<PV path>/disk.img`, applied the updated RBAC and CRD (schema-required `image` removed) to the live cluster, and created a `Machine` with **only `spec.volumes`, no `spec.image.path` at all**.
4. The Machine reached `status.phase: Running` with a real `runtimeID`. Queried FluxVM's own API directly for that VM and confirmed its create-request `image` field was exactly `/data/k3s/storage/pvc-<uid>_default_kairon-storage-test-pvc/disk.img` — the literal PVC-resolved path, not a coincidence — and `status: running`.
5. Cleaned up (`kubectl delete machine`, `kubectl delete pvc`) and confirmed the PV was reclaimed (`Delete` policy) shortly after.

## Coverage

```text
total (internal/...): 68.1% (threshold 50%)
```

# Shipped in v0.5.0: login lockout, console audit/TLS, deploy-remote.sh console support test report

## Result

**PASS.** `make all` green (coverage 68.1%, threshold 50%). `helm lint` green plus `helm template --set console.enabled=true --set console.tls.enabled=true --set console.tls.secretName=my-console-tls` confirming the new TLS Secret/volume/env wiring on both the kairon-node DaemonSet and the kairon-ui Deployment, and confirming zero console-TLS output when `console.tls.enabled` is left at its default `false`. `npm run typecheck && build` green. `bash -n scripts/deploy-remote.sh` clean.

## What shipped

This closes out the five-item gap list from the previous VNC console report, minus two items deliberately scoped out as bigger, separate architecture decisions (self-service password reset — would require `kairon-ui` to gain Kubernetes Secret-write RBAC it doesn't have; OIDC/SSO — already a documented exclusion):

- Login rate limiting/lockout: 5 failed attempts against one username lock it out for 5 minutes (`429` with `Retry-After`), tracked per raw requested username (including unknown ones) so the lockout itself can't be used to enumerate valid accounts. A success clears the counter.
- Console tickets are now bound to the authenticated username at issuance (`issueConsoleTicket`/`consumeConsoleTicket`), and every console session logs a `uiapi console opened`/`uiapi console closed` audit line (username, namespace, name, remote address, duration) — previously only ticket *issuance* was logged, with no record of who actually used a console or for how long.
- The dashboard's "Console" button now hides itself when `console.enabled` is off or the Machine isn't `Running` on a `qemu`/unset/`auto` backend, via a new `GET /api/v1/config` endpoint, instead of only failing with an error after the click.
- One-way TLS on the kairon-ui↔kairon-node console relay hop (`console.tls.enabled`/`console.tls.secretName`, a pre-created Secret with `tls.crt`/`tls.key`/`ca.crt`) — opt-in, mirroring `migration.dataplaneTls`; not mutual TLS, since the existing shared bearer token already authenticates kairon-ui to kairon-node.
- `scripts/deploy-remote.sh --with-console` (`--console-port` to override the auto-picked port): generates and wires `KAIRON_NODE_CONSOLE_TOKEN`/`KAIRON_NODE_CONSOLE_PORT` through both systemd env files, passes `--console-addr` to `kairon-node`, validates `--with-console` requires `--with-ui`, and reports console listener health in `run_status()`/the post-deploy summary.

## A real bug found and fixed along the way

`withAudit` only set up the request-context username holder when `s.Log != nil` — conflating "should this request be logged" with "should downstream handlers see the authenticated username." This meant `handleConsoleTicket` would silently bind tickets to an empty username whenever logging was disabled. Found via `TestHandleConsoleTicketBindsToTheAuthenticatedUsername` failing with `got ""` instead of the expected username; fixed by always attaching the holder in `withAudit` and only skipping the logging work (not holder setup) for GET requests or a nil `Log`.

## Real verification (beyond source-level gates)

- Two new TLS-interop tests (`TestHandleConsoleFullRelayOverTLS`, `TestHandleConsoleTLSRejectsUntrustedCert`) use `httptest.NewUnstartedServer().StartTLS()` for a real, interoperable self-signed certificate — confirming genuine TLS negotiation succeeds with the right CA and fails (`tls: bad certificate`) against an untrusted one, not just that a `tls.Config` field got set.
- **Deployed to the same real lab host used for the original VNC console verification** and driven against a freshly created real QEMU VM end to end: issued a real console ticket through the deployed `kairon-ui`, opened the WebSocket relay through the deployed `kairon-node`, and received the genuine `RFB 003.008\n` protocol banner from the VM's actual QEMU VNC socket — proof the ticket-binding, relay, and (TLS-disabled, this host's configuration) plaintext hop all work together against real infrastructure, not just in-process fakes. The `uiapi console opened`/`uiapi console closed` audit lines were confirmed written for that session.
- **Real login rate-limiter verified in production**: 5 wrong-password attempts against a real account returned `429` with `Retry-After: 5m0s`, and a 6th attempt with the *correct* password still returned `429` — confirming the lockout blocks correct credentials too, not just wrong ones, for the full window.
- **A real port-collision auto-recovery, observed live**: on this shared host, `--console-port`'s default `8090` was already in use (an unrelated Docker container) and the node's own health port also collided (a restart-race self-collision); `deploy-remote.sh`'s `resolve_port()` correctly fell back to a free port (`31315`) and the script's own post-deploy check correctly flagged that the pre-existing env files hadn't picked up the new token/port automatically, requiring (and receiving) a manual `KAIRON_NODE_CONSOLE_PORT` reconciliation — exactly the operator workflow the script's warning describes.
- A real `permission denied` dialing a freshly created VM's `vnc.sock` was hit again on this pass (same root-owned-socket/unprivileged-`kairon`-user prerequisite documented in the previous report) and resolved the same documented way (`setfacl -m u:kairon:rw <path>/vnc.sock`), reconfirming the documented prerequisite still holds rather than having been silently fixed elsewhere.

## Coverage

```text
total (internal/...): 68.1% (threshold 50%)
```

# Shipped in v0.5.0: VNC console + login redesign test report

## Result

**PASS.** `make all` green (coverage 67.4%, up from 66.1%, with the new `internal/consoleproxy` package and `internal/uiapi/console.go` tests). `helm lint` green plus `helm template --set console.enabled=true` confirming the new Secret/env/port wiring, and confirming zero console-related output when `console.enabled` is left at its default `false`. `npm run typecheck && test && build` green with the new `@novnc/novnc` dependency.

## What shipped

- Graphical VNC console (`console.enabled`, off by default): `browser (noVNC) -> kairon-ui -> a new kairon-node listener (internal/consoleproxy) -> the VM's local QEMU VNC socket`. New Go dependency `github.com/coder/websocket` (stdlib has no WebSocket support); new frontend dependency `@novnc/novnc`. Gated by kairon-ui's existing operator auth, a single-use ~30s connection ticket (a browser WebSocket can't carry an Authorization header), and a shared bearer token between kairon-ui and kairon-node (Helm-generated, same `lookup`+`randAlphaNum` pattern as `kairon-ui-session`).
- Login screen visual redesign: a two-column split layout (brand/tagline/animated-gradient-orb panel + card), a per-step CSS transition, a first-letter avatar on the password step, a loading spinner, and a proper error banner. No backend changes.

## Real verification (beyond source-level gates)

- **A real RFB protocol test, not just byte-echoing**: the Go-level integration test (`internal/uiapi/console_test.go`'s `TestHandleConsoleFullRelay`) proves bytes survive the full double-hop relay, but to verify actual noVNC/browser compatibility, a minimal-but-genuine RFB 3.8 server was hand-written (ProtocolVersion handshake, Security(None), ServerInit, and a real `FramebufferUpdate` response with a generated pixel pattern) and wired up behind a real kairon-node + kairon-ui + fake-Kubernetes-API stack. Opening the Console button in a real Chrome browser against this stack rendered the actual generated gradient pattern in the noVNC canvas with a "Connected" status -- proof the whole chain (ticket issuance, WS-to-WS relay, RFB framing) is genuinely compatible with a real VNC client, not just internally consistent.
- Live-browser-verified the login redesign at the same time: the two-column layout, the animated orb, the avatar (first letter of the typed username), and the step transition all render correctly against a freshly built `web/dist`.
- `helm template` diffed with and without `console.enabled` to confirm the feature is entirely absent (zero rendered lines matching "console", case-insensitive) when left at its default off state.
- **Deployed to a real lab host and driven against a real QEMU VM** (not a fake RFB server): real `kairon-node`/`kairon-ui` binaries, a real Machine created via `kubectl apply`, its console opened from a real deployed dashboard in Chrome -- rendered the VM's actual boot console (`Ubuntu 24.04.4 LTS ubuntu tty1`, a live `login:` prompt) and accepted real keystrokes end-to-end. Two real bugs found and fixed this way, neither reachable from in-process tests:
  1. **A console-listener bind failure took down the entire node agent**, not just the optional console feature -- `configureConsole`'s error path called the same `cancel()` used to fail the whole process closed, so a mundane port collision (a Docker container already using the default `:8090` on this shared host) silently stopped all Machine reconciliation, not just VNC. Fixed by switching to an explicit `net.Listen` (checked synchronously, logged and skipped on failure) instead of letting `http.Server.ListenAndServe()`'s failure cascade into `cancel()`.
  2. **FluxVM's `vnc.sock` is root-owned with no `other` write bit**, and `kairon-node` runs as a deliberately unprivileged, capability-less user -- the dial failed with a real, correctly-surfaced `502 permission denied` rather than a silent hang, but revealed an undocumented real-world prerequisite (the node's OS user needs `rw` on FluxVM's per-VM sockets) now written up in SECURITY.md and getting-started.md rather than left for the next operator to rediscover.

## Coverage

```text
total (internal/...): 67.4% (threshold 50%)
```

# Shipped in v0.5.0: VM day-2 ops + kairon-ui login test report

## Result

**PASS.** `make all` (fmt/vet/lint/test-race/cover-check/build/validate/smoke) green on Go 1.27.1 -- coverage rose to 66.1% (from 64.4%) with the new `internal/uiapi/auth.go` and `cmd/kairon-ui` tests. `npm --prefix web run typecheck && test && build` green. `helm lint` green plus three `helm template` scenarios (no `ui.*` values / `ui.auth.users` configured / legacy `ui.token`) each asserting the correct Secret/env wiring. CI green on GitHub for every commit in this cycle, including a real CI failure this cycle caught and fixed (see below).

## What shipped

- `spec.cloudInit` (`hostname`/`user`/`sshAuthorizedKeys`/`packages`/`runCmd`) and `spec.network.forwards` ergonomics via `kaironctl create` flags and the dashboard create form -- both mechanisms already worked end-to-end in FluxVM, they just had no way to set them short of hand-writing a Machine manifest.
- `kairon-ui` real per-operator username/password login: bcrypt accounts (`ui.auth.users`), a two-step "Apple ID style" sign-in screen (with a brand header, a one-line project tagline, and a "Connecting to `<host>`" indicator), signed 12-hour session tokens, working logout, per-operator audit attribution, and a Helm-generated default `admin` account (random password, generated once, persisted across upgrades) when nothing else is configured -- no hardcoded credentials anywhere. The legacy shared `ui.token` keeps working unchanged.

## Real verification (beyond source-level gates)

- **Live browser testing, three times over** (once per iteration of the login feature: initial implementation, the CI-driven behavior fix, and the visual redesign) against `kairon-ui` binaries built fresh from the exact commit being verified, not a stale build: real two-step login with a configured user, the Helm-simulated default-admin path, the legacy raw-token fallback, a deliberately wrong password (confirmed identical error text/timing path to an unknown username), sign-out actually invalidating the session (confirmed via a follow-up 401), and session persistence across a page reload.
- **A real CI failure, caught and fixed in this cycle**: pushing the login feature broke `ci.yml`'s existing "Helm render -- kairon-ui" step, which asserted `helm template` must *fail* without `ui.token` -- exactly the old behavior this feature intentionally replaced with safe default-admin seeding. Fixed by replacing that assertion with three scenario checks (default seeding / configured users / legacy token), verified locally against the real `helm template` output before re-pushing, then confirmed green on GitHub.
- `go build`/`bcrypt.CompareHashAndPassword` round-tripped through the real `kairon-ui -hash-password` CLI mode, not just unit-tested in isolation.

## Coverage

```text
total (internal/...): 66.1% (threshold 50%)
```

# Kairon v0.4.0 test report

## Result

**PASS.** `make all` (fmt/vet/lint/test-race/cover-check/build/validate/smoke) green on Go 1.27.1, `npm --prefix web run typecheck && test && build` green, `helm lint`/`helm template` green including the new `kairon-ui` fail-guard and resource assertions, and CI green on GitHub for every commit in this cycle (not just local checks).

## Commands executed

```text
make all
golangci-lint run ./...
npm --prefix web install && npm --prefix web run typecheck && npm --prefix web test && npm --prefix web run build
helm lint charts/kairon
helm template kairon charts/kairon --namespace kairon-system --set ui.enabled=true --set ui.token=...
```

## Coverage

```text
total (internal/...): 64.4% (threshold 50%)
```

## Real hardware verification (beyond source-level gates)

Unlike the v0.3.0 report below, this cycle's work was additionally verified against two real lab hosts, not just in-process fakes:

- `kairon-node` + `kairon-controller` deployed via `scripts/deploy-remote.sh` to two hosts, each running its own real single-node k3s cluster (`https://127.0.0.1:6443`), with the real CRDs/RBAC applied and real ServiceAccount tokens -- confirmed `kaironctl get machines` working end-to-end against a live Kubernetes API, not a fake one.
- `kairon-ui` deployed the same way (`--with-ui`), loaded in a real Chrome browser, and driven through Overview/Machines/Migrations pages including the `NeedsRecovery` recovery-form UI, with live `/api/v1/...` responses confirmed correct.
- Found and fixed two real bugs this way that no unit test would have caught: a `NetworkAwareDestination.Commit()`-adjacent reflect-based merge-patch fix that over-zeroed pointer fields (internal/integration test helper, not shipped code, but a real test-infra bug), and `deploy-remote.sh` redeploying to an already-running host silently keeping the *old* process alive (`systemctl enable --now` is a no-op on an active unit) while reporting success and printing a URL nothing was listening on -- fixed by switching to `restart`.

Real two-host **live migration** itself, and the `NeedsRecovery` drill, remain documented as runbooks (`docs/runbook-multi-host-migration-test.md`, `docs/runbook-recovery-drill.md`) but were not executed this cycle -- see those runbooks' own scope notes.

---

# Kairon v0.3.0 test report

## Result

**PASS** for the source-level release gate available in this build environment.

Validated on the final v0.3 source tree with Go 1.23 tooling.

## Commands executed

```text
make all
go mod tidy
go test -cover ./...
go clean -testcache
go test -race ./...
```

`make all` includes:

- `gofmt` cleanliness check
- `go vet ./...`
- `go test ./...`
- static `CGO_ENABLED=0` builds of `kairon-controller`, `kairon-node`, and `kaironctl`
- repository/CRD/raw-manifest validation
- version smoke tests for all three binaries

The local environment does not have the Helm binary. A Go-template-compatible render harness was therefore used to render `charts/kairon/templates/all.yaml` with migration disabled and enabled; both rendered outputs were parsed as Kubernetes YAML and the enabled output was checked for the migration TLS arguments, port, and volumes. GitHub Actions installs real Helm and runs `helm lint` plus both render modes.

## Tests

29 Go tests cover the current controller/runtime contract, including:

- Machine creation and FluxVM lifecycle mapping
- finalizer and adopt-only duplicate-boot protection
- image-root enforcement
- DRA allocation checks, PCI BDF normalization, and fail-closed VFIO allowlisting
- scheduling and controlled cold migration
- CSI `VolumeSnapshot` creation/readiness
- migration peer prepare idempotency, identity conflict detection, and local target-node binding
- mode-0600 atomic session persistence and path-traversal rejection
- TLS 1.3 server policy and mandatory client certificate enforcement
- target-first live migration ordering
- unsupported destination blocking before source transfer
- successful transfer -> target commit -> guarded cutover
- source-start failure -> prepared-target abort
- target-commit failure -> `NeedsRecovery`

## Coverage

```text
internal/agent       60.3%
internal/controller  57.4%
internal/fluxvm      55.3%
internal/health      42.1%
internal/kube        26.7%
internal/migration   53.9%
internal/model       45.5%
internal/scheduler   77.1%
```

Command packages are exercised through build/version smoke tests and currently report 0% statement coverage in `go test -cover` because they have no direct unit-test files.

## Important boundary

This environment does not execute KVM/QEMU, a real Kubernetes cluster, a real CSI driver, real DRA hardware, or a FluxVM live-migration backend. The current FluxVM repository does not expose a verified migration/QMP API, so v0.3 intentionally removes the guessed v0.2 FluxVM migration endpoints.

Kairon's target preparation, mTLS peer protocol, durable session state, rollback/fencing state machine, and migration-adapter contract are implemented and tested. **Actual VM memory-state live transfer requires a compatible Kairon migration adapter.** Without one, an explicit live migration is safely blocked before the source transfer begins. Cold migration remains implemented end-to-end at the controller/agent API-contract level.
