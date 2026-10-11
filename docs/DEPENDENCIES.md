---
sidebar_position: 6
title: Dependency policy
---

# Dependency policy

Kairon’s control plane is intentionally tiny. Dependencies are allowed only where the alternative is hand-rolling cryptography, gRPC/CSI, or a full Helm engine — and every exception is named here.

## Stdlib-only (hard rule)

| Binary | Rule |
|--------|------|
| `kairon-controller` | Go standard library only |
| `kairon-node` | Go standard library only |

These two are the trust boundary for host-level VM execution. They must stay readable and free of `client-go` / operator frameworks. CI and review should reject new third-party imports in `cmd/kairon-controller`, `cmd/kairon-node`, and the packages only they use for reconcile (`internal/controller`, `internal/agent`, …) unless an ADR explicitly expands this table.

Cilium integration (ExternalWorkload attach, CNP sync) uses **raw REST** against `apis/cilium.io/...` via `internal/kube` — the same pattern as other non-Kairon APIs. Do **not** add the Cilium Go SDK to controller/node.

Atlas storage integration (`internal/controller/atlas.go`) imports `github.com/zyvorai/atlas/clients/go`, the Atlas project's own Go client. It is allowed in the controller because that module itself is Go standard library only (its CI fails on any non-stdlib package in its import graph), so the controller's graph gains no third-party code.

The README “Go-stdlib only” badge refers to **this** control-plane surface, not every binary in the repo.

## Named exceptions

| Component | Extra deps | Why |
|-----------|------------|-----|
| `kaironctl` / `kubectl-kairon` | `github.com/spf13/cobra`, `helm.sh/helm/v3` (+ transitive `client-go` for Helm only) | Hierarchical CLI help; install/upgrade/uninstall of the embedded chart without requiring a separate `helm` binary or chart checkout |
| `kairon-ui` (OIDC) | `golang.org/x/oauth2`, `github.com/coreos/go-oidc/v3` | Real JWT/JWK verification — not something we hand-roll. Opt-in via `ui.oidc.enabled` |
| `kairon-csi-*` | `github.com/container-storage-interface/spec`, gRPC | CSI is a gRPC contract; opt-in via `csiNode.enabled` |

Optional UI features (websocket console, Prometheus metrics client) may pull small libraries already listed in `go.mod`; they do not relax the controller/node rule.

## Direct third-party modules in `go.mod`

Checked against `go.mod` and `go list -deps` for `cmd/kairon-controller` and `cmd/kairon-node` on this tree. The "stdlib-only" rule above is a policy about the *named exceptions* in the previous section; in practice the controller and node link a few small, non-Helm modules, listed here so they are not a surprise.

| Module | Used by | Why |
|--------|---------|-----|
| `github.com/google/go-sev-guest`, `github.com/google/go-tdx-guest` | `internal/attest`, linked into `kairon-controller` and `kairon-node` (`cmd/kairon-node` also imports them) | Confidential-guest SEV-SNP / TDX report parsing and verification. Not something to hand-roll |
| `github.com/coder/websocket` | `internal/consoleproxy` (node console relay), `internal/uiapi` | VNC and text console WebSocket handling |
| `github.com/prometheus/client_golang` | `internal/metrics` (and `internal/agent` tests) | `/metrics` for controller and node |
| `github.com/zyvorai/atlas/clients/go` | `internal/controller` | Atlas storage client; itself stdlib-only (see above) |
| `golang.org/x/sys` | `internal/csinode`, `internal/macnode` | Mounts and host introspection |
| `google.golang.org/grpc`, `google.golang.org/protobuf`, `github.com/container-storage-interface/spec` | `internal/csinode`, `internal/agent`, `cmd/kairon-csi-*` (grpc and the CSI spec also appear in the `kairon-node` graph through `internal/agent`) | CSI client/server; the CSI exception above |
| `golang.org/x/crypto` | `cmd/kairon-ui`, `internal/uiapi` (bcrypt), and the `kairon-controller` graph | Password hashing for kairon-ui |
| `github.com/go-jose/go-jose/v4` | `internal/uiapi` tests only | Signs test ID tokens for the OIDC tests (`go-oidc` also depends on it) |

`scripts/check_stdlib_boundary.py` (run by `make all` through `make stdlib-boundary`, and by the `hygiene` job in `ci-extra.yml`) is a **deny list**, not an allow list. It fails if the `kairon-controller` or `kairon-node` import graph gains `helm.sh/`, `k8s.io/`, `sigs.k8s.io/`, `github.com/spf13/`, `github.com/coreos/go-oidc`, `golang.org/x/oauth2`, `github.com/containerd/` or `oras.land/`. Anything else, including the modules in the table above, is not blocked by the script and is governed by review and this document.

### Version floors and pins

- **`golang.org/x/net` v0.60.0 (floor).** It is an indirect dependency (`// indirect` in `go.mod`). v0.60.0 fixes the HTTP/2 advisories GO-2026-6610 through GO-2026-6617, which are reachable from the FluxVM client, the Kubernetes client and the CSI gRPC path. Do not let it drop below v0.60.0 (v0.59.0, the previous pin, is affected). Landed in v0.7.2.
- **`google.golang.org/grpc` v1.86.0-dev (pre-release pin).** The history is: the repository was moved to a pre-release commit (`v1.85.0-dev.0.20260825072537-93e31b48545e`, commit 533d808) to pick up the fix for GO-2026-6443 (gRPC server panic on a missing `:authority`/Host, reachable from `kairon-csi-controller`) because there was no tagged v1.85.0 yet; Dependabot then moved it to the `v1.86.0-dev` tag (commit 975f704, PR #28). Whether a tagged, non-`-dev` release containing the fix now exists was not checked from this repository; move to the tagged release as soon as one does, and do not roll back below the GO-2026-6443 fix.
- **Go 1.27.2** (`go 1.27.2` in `go.mod`) clears the Go standard-library advisories CVE-2026-78667 and CVE-2026-97031 (v0.7.1); `golangci-lint` v2.14 is required to load it.

## What “embedded Helm” means for the CLI

`kaironctl install` defaults to the chart baked into the binary (`charts` package via `go:embed`) and drives install/upgrade/uninstall through the Helm v3 Go SDK. Operators can still pass `--chart ./charts/kairon`, `--chart oci://REF` (pulled through the Helm SDK) or `--helm-cli` to shell out to a system Helm 3 binary. Dry-run renders manifests offline (no cluster required).

Helm’s SDK imports `github.com/containerd/containerd` and `oras.land/oras-go` for OCI chart pulls, plus a slice of `golang.org/x/crypto` that the UI’s bcrypt path does not use. `govulncheck` in CI covers every other package. It does not fail the build on those Helm-only call graphs: several of the advisories have no upstream fix, and none of them are linked into `kairon-controller`, `kairon-node`, `kairon-ui`, or the CSI images.

## Review checklist

- New import in controller/node path? **Nack** unless this doc is updated first.
- New CLI-only dependency? OK if confined to `internal/kaironctl` / `cmd/kaironctl` / `cmd/kubectl-kairon` and listed above.
- Do not “fix” Aether, KubeVirt operator frameworks, or unrestricted `client-go` into the control plane to make the CLI easier.
