---
sidebar_position: 2
title: One command to a host
---

# Deploy to a bare-metal host with one command

`scripts/deploy-remote.sh` installs `kairon-node` (and optionally the controller, dashboard and console
relay) on a Linux host over SSH as systemd services.

```bash
./scripts/deploy-remote.sh user@host --remote-build \
  --with-controller --with-ui --with-console --self-test=quick
```

| Flag | What it does |
|---|---|
| `--remote-build` | Copies a small source tarball and compiles on the host (Go, and Node for the dashboard, are cached under `~/.cache/kairon-build` and checksum-verified). Your machine builds nothing. |
| `--self-test[=quick\|standard\|full]` | Runs `kaironctl selftest` on the host afterwards; the result is the script's exit code. |
| `--image-cache-dir=PATH` | Directory kairon-node downloads images into (the self-test needs one). |
| `--dry-run` | Print the resolved plan; no SSH, no build, no writes. |

## Requirements

- SSH access to a Linux host with FluxVM, and a Kubernetes API the node can reach (k3s works).
  `kairon-node` reports not ready until it can reach one; see `--kube-url`.
- With `--remote-build`, the host needs `curl`, `tar` and network access to go.dev and the Go module proxy.

See [Getting started](../getting-started.md#fastest-path) for the full flag list, the redeploy caveats and
the console TLS options, and the [self-test guide](../guides/selftest.md) for reading the result.
