# OpenSSF Best Practices badge

Kairon is registered as project
[15141](https://www.bestpractices.dev/projects/15141) on
[OpenSSF Best Practices](https://www.bestpractices.dev/). Scorecard's
`CII-Best-Practices` check reads that entry: in progress → 2, passing → 5,
silver → 7, gold → 10.

## Where the evidence lives

| Criteria area | Evidence |
|---------------|----------|
| Description, obtain, feedback | [`README.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/README.md), [GitHub Issues](https://github.com/zyvorai/kairon/issues), [Releases](https://github.com/zyvorai/kairon/releases) |
| Contribution process + requirements | [`CONTRIBUTING.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/CONTRIBUTING.md) (PR process, coding standard, test policy) |
| License | [`LICENSE`](https://github.com/zyvorai/zyvor-kairon/blob/main/LICENSE) (Apache-2.0) |
| Interface docs | [`docs/CLI.md`](CLI.md), [`docs/guides/`](guides/), CRDs in [`charts/kairon`](https://github.com/zyvorai/zyvor-kairon/tree/main/charts/kairon) |
| Releases / notes | SemVer tags `vX.Y.Z`, [`RELEASE_NOTES.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/RELEASE_NOTES.md) |
| Vulnerability reporting | [`SECURITY.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/SECURITY.md) (private email + GitHub advisories, 14-day ack) |
| Build / test / CI | `make all`, [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) |
| Warnings / static analysis | `go vet`, `golangci-lint` ([`.golangci.yml`](../.golangci.yml)), CodeQL, `govulncheck`, Trivy |
| Dynamic analysis | `go test -race`, native Go fuzzing ([`.github/workflows/fuzz.yml`](../.github/workflows/fuzz.yml)) |
| Toolchain pins | Go 1.27.2 (`go 1.27.2` in `go.mod`; CI `actions/setup-go` uses `1.27.x`) and golangci-lint v2.14 (`lint` job in [`ci.yml`](../.github/workflows/ci.yml); v2.13 cannot load Go 1.27.2 export data). Action references are pinned to commit SHAs |
| Vulnerability gate | `govulncheck` runs in the `Vulnerability scan` step of the `lint` job in `ci.yml`, over every package except `internal/kaironctl`, `cmd/kaironctl` and `cmd/kubectl-kairon` (Helm SDK advisories without upstream fixes are scoped out; see [`DEPENDENCIES.md`](DEPENDENCIES.md)). The release workflow's `publish` job runs a Trivy scan of each image (`Scan <target> image`) before the push step. `dependency-review.yml` reviews dependency changes in pull requests |
| License-header enforcement | `python3 scripts/check_license_headers.py`, run by the `hygiene` job in [`ci-extra.yml`](../.github/workflows/ci-extra.yml) and by the `sdk` job in [`ecosystem.yml`](../.github/workflows/ecosystem.yml); locally via `make license-headers` (not part of `make all`) |
| Dependency boundary | `scripts/check_stdlib_boundary.py` (deny list for `kairon-controller` and `kairon-node`), in `make all` and the `hygiene` job |
| Ecosystem SDKs and contracts | [`ecosystem.yml`](../.github/workflows/ecosystem.yml), job `sdk`: Python `unittest` for `sdk/python/tests` and `ecosystem/tests`, and `npm ci`, `typecheck` and `test` for `sdk/typescript`, on changes under `sdk/`, `ecosystem/`, `deploy/crd.yaml`, `internal/model/` and `internal/uiapi/` |
| Vulnerability response in practice | v0.7.0 was tagged but never published: its image scan failed on Go standard-library CVE-2026-78667 and CVE-2026-97031. v0.7.1 rebuilt on Go 1.27.2 and was the first published v0.7 release. v0.7.2 raised `golang.org/x/net` to v0.60.0 for the HTTP/2 advisories GO-2026-6610 through GO-2026-6617. Both are described in [`RELEASE_NOTES.md`](https://github.com/zyvorai/zyvor-kairon/blob/main/RELEASE_NOTES.md) |
| Signed delivery | cosign keyless signatures + SBOM attestations ([`release.yml`](../.github/workflows/release.yml)), `SHA256SUMS` |

## Maintained check

Scorecard `Maintained` stays at 0 until the repository is **older than 90
days** (created 2026-09-11 → eligible ~2026-12-10), then needs roughly
weekly commits. No code change can advance that calendar.
