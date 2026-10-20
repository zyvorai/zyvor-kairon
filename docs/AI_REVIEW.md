# AI code review (kairon)

_Generated 2026-10-20 by claude-opus-5-5 from a snapshot of tracked sources; verify before acting._

# Code review: Kairon

## Summary
Kairon is a Kubernetes-native VM orchestrator. It has a mostly stdlib Go control plane (`kairon-controller` and `kairon-node`) that drives FluxVM (QEMU, Cloud Hypervisor, Firecracker, `vz`) through REST. Around that core it ships a CLI and MCP server (`kaironctl`), a React dashboard (`kairon-ui`), CSI plugins, a Helm chart, and a developer ecosystem kit (Python and TypeScript SDKs, a GitHub Action, catalog, sweeper and Terraform composition). The review covers the files provided, which are a subset of the repository.

## Bugs and correctness risks

**Diagnostics script**
- **`scripts/must-gather.sh` event collection is broken.**
  - `--field-selector reason=Fenced,FailedScheduling,...` is not an OR list. Commas join separate selector terms, so `FailedScheduling` is parsed as a malformed term and kubectl errors.
  - The `|| true` hides the failure, so `events.txt` silently contains only the error.
  - Fix: run one query per reason, or filter client-side.

**Storage and CSI**
- **`internal/csinode/statfs_linux.go` may compile on some Linux targets but not others.**
  - It computes `int64(buf.Blocks) * bsize` with `bsize := buf.Bsize`. `Statfs_t.Bsize` is not `int64` on every Linux arch (for example 32-bit arm), so this can fail to build there.
  - Per statfs(2), block counts are in units of `f_frsize`, not `f_bsize`, so sizes may be wrong on some filesystems.
- **`internal/csinode/mount.go`: `isMountPointFromReader` compares raw mountinfo fields.**
  - mountinfo octal-escapes spaces and similar characters (`\040`).
  - A target path containing such characters will never match, which breaks the CSI idempotency guarantee the comment promises.

**Kubernetes client**
- **`internal/kube/rehearsal.go` (`SetRehearsalReservation`) may wipe a Node's other annotations.**
  - It issues `{"op":"add","path":"/metadata/annotations","value":annotations}`, which replaces the whole map.
  - Unless every caller passes the full merged map, this erases unrelated annotations such as `kairon.zyvor.dev/node-fenced` or kubeadm/k3s annotations.
  - Callers are not shown, so this is unverified. Patching individual keys (with `~1` escaping) is safer.

**Fleet metering**
- **`internal/fleet/meter.go` can silently stop recording usage.**
  - `s, _ := decode[LedgerSpec](o.Spec)` discards the decode error.
  - If `MaxGapSeconds` is 0 (spec unset or invalid), every positive interval counts as a "Gap", so no usage is ever recorded.
  - A single Machine with an unparsable CPU or memory value returns an error and aborts accounting for the whole ledger. The watermark then does not advance, so the next tick likely falls into the gap path.

**Agent plane**
- **`internal/agentplane/image.go`: `AdmitImage` checks length but not content.**
  - The comment and error say `<64 hex>`, but only the length is checked. A 64-character non-hex string passes.
- **`internal/agentplane/audit.go`: `Record` can produce colliding IDs.**
  - It defaults a zero `At` to the Unix epoch, and the ID is a truncated hash of the fields.
  - Two identical events without timestamps therefore get identical IDs.
  - `Log` is also unbounded in memory.
- **`internal/agentplane/doc.go`: the package doc comment is in the wrong place.**
  - It sits after the `package` clause, so godoc ignores it.

**Dashboard**
- **`web/src/lib/route.ts`: `parseHash` can crash routing.**
  - It calls `decodeURIComponent` without a try/catch, so a malformed `%` in the URL hash throws during render.
- **`web/src/lib/undo.ts` maps `hibernate` to `start`, but this is untested.**
  - Whether `start` actually resumes a Hibernated Machine, rather than cold-booting it, is unclear from the code.

**Dependencies and manifests**
- **`go.mod` pins `google.golang.org/grpc v1.86.0-dev`.** A `-dev` version is not a tagged release and should not ship in a release build.
- **`deploy/node.yaml` likely cannot work as written.**
  - It runs `kairon-node` with `readOnlyRootFilesystem`, `runAsNonRoot`, all capabilities dropped, and no volume mounts.
  - Yet it passes `--image-root=/var/lib/fluxvm/images` and a VFIO allowlist.
  - That configuration probably cannot reach host image paths or the FluxVM socket/state. The Helm chart may differ; this was not verified.
  - `deploy/controller.yaml` and `deploy/node.yaml` both use the `:latest` image tag.
- **`charts/kairon/templates/pdb.yaml` blocks drains by default.**
  - `maxUnavailable: 0` with the default single replica means any `kubectl drain` of the node running controller or UI hangs.
  - This is documented as deliberate, but it surprises operators doing node maintenance.

## Security concerns

- **Cluster-wide Secret read for every node.**
  - `deploy/rbac-cbt-backup.yaml` gives `kairon-node` (a DaemonSet on every capable node) `get` on all Secrets cluster-wide.
  - Label filtering happens only in process. One compromised node therefore reads every Secret in the cluster. This deserves stronger warnings or a narrower design, such as a controller-mediated fetch.
- **Credential vault secret crosses the node-to-UI hop.**
  - `internal/consoleproxy/egress.go` relays FluxVM's `inject_authorization` (a real vault secret) to `kairon-ui`, relying on `uiapi` to redact it.
  - If console TLS is off (ARCHITECTURE.md says plaintext is the default), the secret crosses the network in cleartext.
  - The request body is also decoded without a size limit (`http.MaxBytesReader`).
- **Viewer role and GET-based routes in `internal/uiapi/actionauth.go`.**
  - The viewer role allows every `GET`/`HEAD`. It also allows any method under `/api/v1/auth/`.
  - The VNC console is a GET WebSocket upgrade (see `web/vite.config.ts`). Unless it is gated elsewhere, a viewer may open an interactive console.
  - Users not found in `ui.auth.users`, such as OIDC sessions, get no role restriction at all.
  - Please confirm both cases are intended.
- **Long-lived DR peer token.** `deploy/dr-peer-rbac.yaml` recommends a 1-year token (`--duration=8760h`); rotation guidance is missing.
- **Plaintext S3 example.** `examples/machinebackuppolicy.yaml` sends S3 credentials over plain `http://`. It is an example, but it may be copied verbatim.
- **Vulnerability suppression without a review date.** `osv-scanner.toml` permanently ignores GO-2026-5932. The justification is reasonable, but it needs a review date or a tracking issue.

## Test and CI gaps

- **Workflow coverage is uneven.**
  - `macos.yml` runs only on `pull_request` with path filters, never on push to `main`.
  - `dependency-review.yml` has `continue-on-error: true`, so it never blocks a merge.
- **Dependabot misses the SDKs.** `dependabot.yml` covers gomod, `/web`, `/website` and actions, but not `sdk/typescript` (npm) or `ecosystem/` and `sdk/python` (pip).
- **The coverage floor is low.** `COVERAGE_THRESHOLD` is 50%, and `make all` skips license headers, the frontend and the SDKs, so local runs diverge from CI.
- **Several modules have no tests among the provided files.**
  - `meter.go` (zero/invalid `MaxGapSeconds`).
  - `SetRehearsalReservation` (annotation preservation).
  - `AdmitImage` (non-hex input).
  - `parseHash` (malformed escapes).
  - The `hibernate` undo path.
  - `actionauth.go` (viewer vs. console, OIDC users with no account).
- **Test hygiene.**
  - The `FuzzSessionValidate` escape check (`strings.HasPrefix(rel, "..")`) false-positives on IDs like `..foo`.
  - `restorestore_test.go` contains a dead `_ = time.Now()`.
- **Shellcheck enforcement is not visible.** CONTRIBUTING requires shellcheck for scripts, but no shellcheck step appears in the workflows shown (`ci.yml` was not provided).

## Documentation gaps

- **Inconsistent repository name.**
  - `zyvorai/zyvor-kairon` is used in docs and website links.
  - `zyvorai/kairon` appears in `docs/README.md` (the ecosystem link), the CONTRIBUTING issues URL, the raw values URL in `docs/deploy/helm.md`, and the Go module path.
  - At least one form is a broken link.
- **Contradictory TLS defaults.**
  - ARCHITECTURE.md says the console relay is plaintext with opt-in TLS.
  - `docs/deploy/helm.md` and `docs/README.md` say console TLS is on by default.
  - ARCHITECTURE.md's "you supply the certificate" statement conflicts with the UI's self-signed default.
- **Stdlib-only claims conflict.**
  - CONTRIBUTING says `kairon-controller`/`kairon-node` "stay stdlib-only".
  - ARCHITECTURE.md says `kairon-node` links grpc.
  - The controller serves Prometheus `client_golang` metrics via `internal/metrics` and `health.Server.Metrics`.
  - `DEPENDENCIES.md` should list these exceptions explicitly.
- **Stale comment in `internal/model/quota.go`.** It says "no admission webhook exists or is needed", which contradicts `docs/guides/admission-webhook.md`.
- **Version mismatch in `docs/README.md`.** It says "
