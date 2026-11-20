# Runbook: running kairon-node on a Mac

Procedure for operating `kairon-node` on an Apple-silicon Mac. How the port
works and what has and has not been verified is in [macos.md](macos.md); the
multi-Mac picture is in [macos-cluster.md](macos-cluster.md). This repository
ships no launchd plist or service wrapper, so process supervision (start at
login, restart on crash) is up to you.

## Prerequisites

- A reachable Kubernetes API, with `deploy/crd.yaml`, `deploy/rbac.yaml` and
  `deploy/macos/rbac.yaml` applied. The last one grants the `kairon-node`
  ServiceAccount `get/list/create/patch` on `nodes` and `patch` on
  `nodes/status`, which a Mac needs because it has no kubelet.
- A FluxVM daemon with the `vz` backend listening on the URL given to
  `--fluxvm-url` (default `http://127.0.0.1:7788`, or `$FLUXVM_URL`;
  `$FLUXVM_TOKEN` is sent as its token if set).
- A raw arm64 Linux disk image under the image root (default
  `~/Library/Application Support/FluxVM/images`).

## Start

```bash
export KAIRON_KUBE_URL=https://API:6443
export KAIRON_KUBE_TOKEN=...        # ServiceAccount token for kairon-node
export KAIRON_KUBE_CA=/path/to/ca.pem
export NODE_NAME=mac-studio-1       # required; kairon-node exits 2 without it
kairon-node                         # --register-node is on by default on macOS
```

`scripts/macos-e2e.sh` shows a complete working invocation, including the
extra flags it passes for a throwaway run (`--interval=3s`,
`--health-addr=127.0.0.1:32392`, `--console-addr=`).

Optional: `--mlx-url` / `KAIRON_MLX_URL` for a loopback MLX endpoint (adds the
`kairon.zyvor.dev/mlx=true` label and an `inference-url` annotation to the
Node).

## Health, logs, metrics

- Health listener: `--health-addr`, default `:32302` (all interfaces).
  `GET /healthz` returns `{"ok":true}` while the process is up. `GET /readyz`
  returns 503 with `{"ready":false}` until FluxVM's own ready check passes
  (re-checked every 5 s). `GET /metrics` serves the Prometheus metrics.

  ```bash
  curl -s localhost:32302/readyz
  ```

- Logs: JSON lines on stdout. Redirect them yourself when running under a
  supervisor. Useful messages: `registering this host as a Kubernetes Node`
  (startup) and `node registration` (an error from the 10 s heartbeat; it is
  retried on the next tick).

## Restart and stop

`SIGTERM` or `SIGINT` stops kairon-node cleanly. On start it looks the Node
up by `NODE_NAME`; if it exists it refreshes labels (and the inference-url
annotation if set) and then publishes status, otherwise it creates the Node.
Stopping kairon-node does not delete the Node object. To remove a Mac from
the pool for good, drain or delete its Machines and then
`kubectl delete node NODE_NAME`.

## Verify the Node

```bash
kubectl get node $NODE_NAME -o wide
kubectl get node $NODE_NAME --show-labels
kubectl get node $NODE_NAME -o jsonpath='{.spec.taints}'
kubectl get node $NODE_NAME -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```

Expect:

- Ready condition `True`, reason `KaironNodeReady`, with `lastHeartbeatTime`
  advancing every 10 s.
- Labels `kubernetes.io/arch=arm64`, `kubernetes.io/os=darwin`,
  `kairon.zyvor.dev/capable=true`, `kairon.zyvor.dev/backend.vz=true`
  (and `kairon.zyvor.dev/mlx=true` if `--mlx-url` is set).
- Taint `kairon.zyvor.dev/vm-only=true:NoSchedule`.
- Allocatable `cpu` equal to the Mac's CPU count and `memory` equal to total
  memory minus the larger of 3 GiB and a quarter of RAM; `pods` is `0`.

Machines must tolerate the taint and should select the `vz` backend; see
`examples/macos-machine.yaml`.

## Common failures

| Symptom | Likely cause and fix |
| --- | --- |
| `NODE_NAME is required` | Export `NODE_NAME` before starting. |
| `kubernetes endpoint not configured` | Set `KAIRON_KUBE_URL` (and token and CA) or run in cluster. |
| `node registration` errors mentioning 403 | `deploy/macos/rbac.yaml` not applied, or the token is not the `kairon-node` ServiceAccount's. |
| Node never appears | Check `--register-node` has not been set false (`KAIRON_REGISTER_NODE=false`), and the logs for registration errors. |
| `/readyz` returns 503 | FluxVM is not reachable or not ready at `--fluxvm-url`; start it or fix the URL/token. |
| Machine stays Pending | It lacks the `kairon.zyvor.dev/vm-only` toleration, or no node matches its `nodeSelector` / `architecture: arm64`. |
| Machine fails with an "unsupported" message | The `vz` backend rejects tap networking, port forwards, NUMA, hugepages, cpuSet, pinning, hotplug, TPM, secure boot, VFIO, data disks, cdroms and the QEMU guest agent; remove the field. |
| Image path refused | The image must be under `--image-root` (default `~/Library/Application Support/FluxVM/images`). |

Not covered here because it has not been verified: macOS guests, live
migration, CSI volumes, the VNC console relay and Intel Macs (see
[macos.md](macos.md)).
