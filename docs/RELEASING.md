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
