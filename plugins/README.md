# Custom Krew index

This directory is the Krew custom-index layout for this repository:

```bash
kubectl krew index add kairon https://github.com/zyvorai/zyvor-kairon.git
kubectl krew install kairon/kairon
```

`kairon.yaml` is intentionally not committed with placeholder checksums:
Krew requires `sha256` to be a 64-character hex digest, so a placeholder
fails manifest validation and `kubectl krew search` / `install`.

After a GitHub Release publishes the `kairon_*.tar.gz` archives, copy the
filled release asset into this directory and merge that commit to the
default branch:

```bash
./scripts/krew-index-sync.sh vX.Y.Z
```

Until `plugins/kairon.yaml` exists on the default branch, `index add`
succeeds but `kubectl krew install kairon/kairon` will not find the plugin.
Install from a release asset or a local package instead; see
[docs/CLI.md](../docs/CLI.md#install-via-krew).

`kubectl krew install kairon` (no prefix) talks to the public default index
and needs a separate PR to [kubernetes-sigs/krew-index](https://github.com/kubernetes-sigs/krew-index).
See [docs/RELEASING.md](../docs/RELEASING.md#public-krew-index).
