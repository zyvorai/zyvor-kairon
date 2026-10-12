# Kairon on macOS (Apple silicon)

Kairon's controller and node agent build and run natively on macOS. A Mac has no kubelet, so `kairon-node` registers the
Mac as a Kubernetes **Node** itself (`internal/macnode`) and runs `Machines` through FluxVM's Apple Virtualization.framework
backend (`backend: vz`, see FluxVM's `docs/macos.md`).

## What is verified

On an Apple M4 running macOS 27.2 with Go 1.27.1:

| Check | Result |
| --- | --- |
| `go vet ./...` and `go build ./...` on darwin/arm64 | clean |
| `go test ./...` on darwin/arm64 | all 36 test packages pass, plus new `macnode` and `fluxvm` tests |
| `scripts/macos-e2e.sh` against a real k3s cluster | **PASS**: the Mac registers as a Ready Node, a `vz` Machine is scheduled to it, FluxVM boots Debian 13, `status.guestIP` is set, SSH works, deleting the Machine removes the VM |

The e2e used a single-node k3s cluster running in a Velora VM on the same Mac as the Kubernetes API.

**Not verified:** macOS guests, live migration, CSI, the VNC console relay, kairon-ui, multi-Mac clusters, and Intel Macs.

## How it works

1. `kairon-node` (on a Mac `--register-node` is on by default) creates a `Node` named after `NODE_NAME` and refreshes it every
   10 s: labels `kubernetes.io/arch=arm64`, `kubernetes.io/os=darwin`, `kairon.zyvor.dev/capable=true`,
   `kairon.zyvor.dev/backend.vz=true` (and `kairon.zyvor.dev/mlx=true` plus an `inference-url` annotation when `--mlx-url` points at
   Velora's MLX runtime), allocatable `cpu` and `memory` (unified memory minus the larger of 3 GiB and a quarter, left to macOS),
   a Ready condition, and the taint `kairon.zyvor.dev/vm-only:NoSchedule` so ordinary pods never land on a node without a kubelet.
2. The controller schedules a Machine to it like to any node: capacity comes from `allocatable`, so memory admission is the
   unified-memory figure. Machines tolerate the taint (`examples/macos-machine.yaml`).
3. `kairon-node` sends the Machine to FluxVM (`--fluxvm-url`, default `http://127.0.0.1:7788`) with `backend: vz`.
   `internal/fluxvm/apple.go` rejects, with a specific message, anything the vz backend cannot honour (tap networking, port
   forwards, NUMA, hugepages, cpuSet, pinning, hotplug, TPM, secure boot, VFIO, data disks, cdroms, the QEMU guest agent).
4. Defaults on a Mac: backend `vz`; image root, migration state and CSI directories under `~/Library/Application Support`
   (see the table below).

## Defaults and settings on a Mac

`kairon-node` picks these defaults per OS (`osDefault` and `macSupport` in `cmd/kairon-node/main.go`). Each flag also reads
the environment variable shown, and an explicit flag wins. If the home directory cannot be resolved, the macOS paths fall
back to `$TMPDIR/kairon/<path>`.

| Flag | Environment variable | Default on macOS | Default on Linux |
| --- | --- | --- | --- |
| `--default-backend` | `KAIRON_DEFAULT_BACKEND` | `vz` | `qemu` |
| `--image-root` | `KAIRON_IMAGE_ROOT` | `~/Library/Application Support/FluxVM/images` | `/var/lib/fluxvm/images` |
| `--migration-state-dir` | `KAIRON_MIGRATION_STATE_DIR` | `~/Library/Application Support/Kairon/migrations` | `/var/run/kairon/migrations` |
| `--csi-staging-dir` | `KAIRON_CSI_STAGING_DIR` | `~/Library/Application Support/Kairon/csi/staging` | `/var/lib/kairon/csi/staging` |
| `--csi-publish-dir` | `KAIRON_CSI_PUBLISH_DIR` | `~/Library/Application Support/Kairon/csi/publish` | `/var/lib/kairon/csi/publish` |

Two more settings are specific to Node registration:

- `KAIRON_REGISTER_NODE` (flag `--register-node`): create and heartbeat this host's Kubernetes Node. The default is `true`
  on macOS and `false` elsewhere. The environment variable is only honoured when its value is exactly `true`; any other
  value, including `1`, turns it off.
- `KAIRON_MLX_URL` (flag `--mlx-url`): a loopback OpenAI-compatible endpoint for this Mac's MLX runtime (Velora). When set,
  the Node gets the `kairon.zyvor.dev/mlx=true` label and a `kairon.zyvor.dev/inference-url` annotation. Empty by default.
- `KAIRON_THUNDERBOLT_FABRIC` (`--thunderbolt-fabric`), `KAIRON_MAC_FABRIC_SECRET_FILE` (`--mac-fabric-secret-file`) and
  `FLUXVM_VZ_OVERLAY_CONFIG` (`--mac-overlay-config`, default `~/.config/fluxvm/overlay.json`): Thunderbolt fabric
  membership and `MacNetwork` overlay sync. See [Mac Fabric](guides/mac-fabric.md).

The Node also carries the Mac's chip, macOS version, Rosetta, nested virtualization and Thunderbolt Bridge as labels
and annotations ([Mac Fabric](guides/mac-fabric.md)).

The migration and CSI directories exist as defaults, but migration and CSI are not verified on macOS (see above).

## Run it

```bash
# 1. A Kubernetes API (any cluster; Velora can host a single-node k3s one)
# 2. FluxVM with the vz backend (../fluxvm, branch feat/macos-apple-backend)
KUBECONFIG=/path/to/kubeconfig scripts/macos-e2e.sh /path/to/arm64-debian.raw
```

`scripts/macos-e2e.sh` needs Apple silicon, Go, Rust, the Xcode command line tools, `kubectl` and a Kubernetes API. It builds
FluxVM from a sibling checkout at `../fluxvm` (relative to the Kairon repository root), which must be on the branch with the
`vz` backend. Environment overrides:

| Variable | Default | Meaning |
| --- | --- | --- |
| `FLUXVM_DIR` | `<repo>/../fluxvm` | Path of the FluxVM checkout to build and run. |
| `FLUXVM_E2E_PORT` | `7798` | Port of the throwaway FluxVM daemon the script starts on `127.0.0.1`. |

The script takes the raw arm64 disk image as its only argument and exits 2 on anything but Apple-silicon macOS. The Node name
is the lower-cased `scutil --get LocalHostName` (or `mac`).

By hand: apply `deploy/crd.yaml`, `deploy/rbac.yaml` and `deploy/macos/rbac.yaml`; run `kairon-controller` and `kairon-node`
natively with `KAIRON_KUBE_URL`, `KAIRON_KUBE_TOKEN` (a ServiceAccount token) and `KAIRON_KUBE_CA`, and `NODE_NAME` for the node.

## Honest limits

- Kairon stores all state in a Kubernetes API, so a Mac still needs a cluster to talk to; this port does not remove that.
- Machines on a Mac get NAT networking, plus a card on each `MacNetwork` in `spec.apple.networks`
  ([Mac Fabric](guides/mac-fabric.md)). The NAT address is in `status.guestIP`; there are no port forwards.
- Memory is accounted from the Node's allocatable figure and Kairon's own Machine requests; macOS can still page under pressure.
- Linux-only features (eBPF edge, migration, CPU pinning, VFIO) remain Linux-only.
