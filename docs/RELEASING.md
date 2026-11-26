# Releasing Kairon

A tag `vX.Y.Z` runs `.github/workflows/release.yml`: it scans, pushes, signs and attests the images, builds the CLI (which embeds `charts/kairon`), pushes the Helm chart, creates the GitHub Release, publishes the MCP Registry entry, and only then moves `latest`.

## Files that must equal the tag

The first step of the `release-assets` job fails the release unless these match (before anything is built):

| File | Value for tag `v0.10.6` |
|---|---|
| `VERSION` | `v0.10.6` |
| `charts/kairon/Chart.yaml` `version` | `0.10.6` |
| `charts/kairon/Chart.yaml` `appVersion` | `"0.10.6"` |
| `server.json` `version` and the `kairon-mcp` image tag | `0.10.6` / `ghcr.io/zyvorai/kairon-mcp:v0.10.6` |

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

## Custom Krew index

This repository itself can be added as a Krew custom index (`plugins/` at the repo root). `plugins/kairon.yaml` is committed after the first release sync, not with placeholder checksums:

```bash
kubectl krew index add kairon https://github.com/zyvorai/zyvor-kairon.git
kubectl krew install kairon/kairon
```

After each GitHub Release, add or update `plugins/kairon.yaml` with the filled manifest whose sha256 sums match the published `kairon_*.tar.gz` assets, and merge that commit to the default branch. Do not commit placeholder checksums (Krew rejects them). Copy the release asset, or:

```bash
./scripts/krew-index-sync.sh vX.Y.Z    # downloads the release's kairon.yaml
# or, if dist/krew/kairon.yaml was built from the archives attached to that tag:
make krew-index
```

Do not commit a locally built manifest unless those archives are the ones on the Release (tarball checksums include file timestamps and will not match a later rebuild).

## Public Krew index

`kubectl krew install kairon` (no custom-index prefix) works only after a plugin manifest is merged into [kubernetes-sigs/krew-index](https://github.com/kubernetes-sigs/krew-index) at `plugins/kairon.yaml`. That is a separate pull request, opened against krew-index, not this repository. Do not open it as part of an ordinary Kairon release.

Krew's [first-release checklist](https://krew.sigs.k8s.io/docs/developer-guide/release/new-plugin/) requires the plugin source to be open source under an OSI-approved licence, and the plugin archive to include a `LICENSE` file that Krew extracts on install (`make krew-package` already packs `LICENSE`). This repository is offered under the Zyvor Production Licence v2.0 (`LICENSE`), which is not an OSI-approved licence. A krew-index PR may be rejected until the plugin is also offered under an OSI-approved licence; resolve that before submitting.

When the licence and a published release with `kairon_{darwin,linux}_{amd64,arm64}.tar.gz` plus a filled `kairon.yaml` are in place:

1. Review the [plugin naming guide](https://krew.sigs.k8s.io/docs/developer-guide/develop/naming-guide/) and [development best practices](https://krew.sigs.k8s.io/docs/developer-guide/develop/best-practices/). The plugin name `kairon` matches `kubectl kairon` and `metadata.name`.
2. Take the filled release asset `kairon.yaml` (not the placeholder template in `deploy/krew/`). Confirm `version`, `uri` and `sha256` match the tag and `SHA256SUMS`.
3. Test locally: `kubectl krew install --manifest=kairon.yaml --archive=kairon_<os>_<arch>.tar.gz` and `kubectl kairon version`.
4. Open a pull request to `kubernetes-sigs/krew-index` that adds `plugins/kairon.yaml`. The first version must be submitted by hand; later version bumps that only change `version`, `uri` and `sha256` can be automated.
5. After that PR is merged, users can run `kubectl krew install kairon`. Optionally add a step to `.github/workflows/release.yml` after the assets are published, using [`krew-release-bot`](https://github.com/rajatjindal/krew-release-bot) and the template at [`deploy/krew/kairon.krew.yaml`](../deploy/krew/kairon.krew.yaml):

```yaml
- name: Update new version in krew-index
  uses: rajatjindal/krew-release-bot@v0.0.50
  with:
    krew_template_file: deploy/krew/kairon.krew.yaml
```

Do not enable that Action before the first krew-index entry exists: the bot only updates an existing plugin, and a pre-release GitHub Release is skipped. The template can be dry-run with the bot's `template` command as documented in the krew-release-bot README.
