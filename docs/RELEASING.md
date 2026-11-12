# Releasing Kairon

A tag `vX.Y.Z` runs `.github/workflows/release.yml`: it scans, pushes, signs and attests the images, builds the CLI (which embeds `charts/kairon`), pushes the Helm chart, creates the GitHub Release, publishes the MCP Registry entry, and only then moves `latest`.

## Files that must equal the tag

The first step of the `release-assets` job fails the release unless these match (before anything is built):

| File | Value for tag `v0.10.4` |
|---|---|
| `VERSION` | `v0.10.4` |
| `charts/kairon/Chart.yaml` `version` | `0.10.4` |
| `charts/kairon/Chart.yaml` `appVersion` | `"0.10.4"` |
| `server.json` `version` and the `kairon-mcp` image tag | `0.10.4` / `ghcr.io/zyvorai/kairon-mcp:v0.10.4` |

Image tags are `vX.Y.Z`; the chart prefixes `v` to `appVersion` for its default image tags.

## Dry run with a pre-release tag

A tag containing a hyphen (`v0.10.3-rc1`) is a pre-release: it publishes images, chart and a GitHub **pre-release**, but does **not** publish to the MCP Registry and does **not** move `latest`.

1. Branch from `main`, bump the files above to the rc version (`VERSION=v0.10.3-rc1`, chart `0.10.3-rc1`, `server.json` `0.10.3-rc1`), commit.
2. Tag that commit `v0.10.3-rc1` and push the tag (not the branch to `main`).
3. Install the rc from the published chart on a clean cluster and run `kaironctl selftest --profile standard`.
4. Delete the rc pre-release and its tag if you do not want it listed.

## Final release

1. On `main`, bump the files above to `X.Y.Z` in one commit, merged through a PR.
2. Tag `vX.Y.Z`. Watch every job; `latest` moves last.
3. If a job fails after images were pushed, fix forward with the next patch tag. `latest` still points at the previous release because it moves last.

## Check a published release

A release is complete only when all of these hold. Run them after the workflow (or after a by-hand publication):

```bash
V=vX.Y.Z; C=${V#v}
gh release view $V --json isDraft,isPrerelease,assets -q '{draft:.isDraft,pre:.isPrerelease,assets:[.assets[].name]}'   # 12 assets
curl -fsSL https://github.com/zyvorai/zyvor-kairon/releases/download/$V/SHA256SUMS                                      # 11 lines: every asset but itself
helm pull oci://ghcr.io/zyvorai/charts/kairon --version $C                                                              # chart exists
for t in controller node ui csi-node csi-controller mcp; do
  crane digest ghcr.io/zyvorai/kairon-$t:$V; crane digest ghcr.io/zyvorai/kairon-$t:latest                              # equal once latest has moved
done
```

The 12 assets are `kaironctl-linux-{amd64,arm64}`, `kubectl-kairon-linux-{amd64,arm64}`, `kairon_{darwin,linux}_{amd64,arm64}.tar.gz`, `kairon.yaml` (the Krew manifest), `kairon-X.Y.Z.tgz` (the chart), `kairon-manifests-vX.Y.Z.tgz` and `SHA256SUMS`. Signatures and SBOM attestations are checked with the `cosign verify` commands in [`SECURITY.md`](../SECURITY.md#images).

## When the workflow does not finish

Images, the chart and the MCP Registry entry are published by earlier jobs; `release-assets` creates the GitHub Release as its last step, and `promote-latest` needs all three jobs, so a failure in any of them leaves `latest` on the previous release. If you create the GitHub Release by hand (for example because runners were unavailable), attach the same 12 assets; the workflow's `Create GitHub Release` step will then fail because the release exists, and `promote-latest` is skipped, so `latest` has to be moved separately (`crane tag REPO@DIGEST latest` for each of the six images, using the digest of the version tag, then check `crane digest REPO:latest`). Say in the release description what has and has not been published, and update it when the rest lands: the description is not regenerated. This is what happened for `v0.10.4` ([`TEST_REPORT.md`](../TEST_REPORT.md)).
