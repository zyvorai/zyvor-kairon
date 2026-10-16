# Runbook: the Kairon lab (runner, cluster access, adding a node)

The nightly workflows (`hardware-migration.yml`, `lab-features.yml`) run on a
self-hosted runner labelled `kairon-lab`. This page records how the lab is
wired and the traps found while setting it up. Evidence rules (what a green
row has to be before a feature stops being experimental) are in
[`STABILITY.md`](STABILITY.md); the rows themselves are in
[`COMPATIBILITY.md`](COMPATIBILITY.md).

## Layout

| Part | Where |
|---|---|
| Runner `kairon-lab-a` (label `kairon-lab`, group `zyvor-build`) | control-plane host, user `ghrunner`, `~/runners/kairon-lab-a` |
| Cluster | k3s on the control-plane host (agent on the second host) |
| Drill identity | ServiceAccount `kairon-lab-drills` in `kairon-system` and a token Secret |
| Scratch namespace | `kairon-lab` (the only namespace where the drills may touch Secrets and ConfigMaps) |

Both hosts also run CI runners. Never drain the host that runs CI while a
workflow is in flight.

## Repository variables and secrets

| Name | Kind | Meaning |
|---|---|---|
| `KAIRON_HW_LAB` | var | `1` on a real lab. The matrix **fails** (not green-with-warning) when unset on the lab runner. |
| `KAIRON_LAB_NODE`, `KAIRON_LAB_NODE_B` | var | Kubernetes node names for single- and two-node drills |
| `KAIRON_HW_NAMESPACE` | var | `kairon-lab` |
| `KAIRON_KUBE_URL` | secret | API URL reachable from the runner (`https://127.0.0.1:6443` when co-located) |
| `KAIRON_KUBE_TOKEN` | secret | token of `kairon-lab-drills` |
| `KAIRON_KUBE_CA` | secret | PEM of the cluster CA |
| `KAIRON_LAB_IMAGE`, `KAIRON_LAB_PRESSURE_CMD`, `KAIRON_LAB_RELIEF_CMD`, `KAIRON_LAB_CBT_MACHINE`, `KAIRON_LAB_MINIO_SSH` | var | per-drill inputs; a drill missing its input is reported `skipped`, never `pass` |

Set secrets by piping, never on a command line:
`kubectl -n kairon-system get secret kairon-lab-drills-token -o jsonpath='{.data.token}' | base64 -d | gh secret set KAIRON_KUBE_TOKEN`.
Take the CA from the same Secret's `ca.crt`. An empty value is accepted by
`gh secret set` without error, so check the first run if auth fails.

## Registering the runner

1. Create the runners directory as root if it does not exist and give it to
   the runner user: `sudo mkdir -p /home/ghrunner/runners && sudo chown ghrunner: /home/ghrunner/runners`.
2. Register with `--labels kairon-lab --runnergroup zyvor-build`, install and
   start the service.
3. A scheduled run may pick the runner up immediately; that is expected.

## Adding a node to the k3s cluster

- Copy the same `k3s` binary version as the server; skewed versions are not supported.
- Put the agent token in a root-only env file (`/etc/systemd/system/k3s-agent.service.env`, mode 600), not in the unit.
- Start with `k3s agent --server https://<server>:6443`. **Do not pass
  `--disable-kube-proxy` to the agent**: it is a server flag, the agent exits
  with `flag provided but not defined`. Kube-proxy replacement is set on the
  server (Cilium) and applies to the whole cluster.
- Check `kubectl get nodes` shows `Ready`, then that Cilium is Running on it.

## Resetting the lab to clean

Delete workloads, not the platform: uninstall the Helm releases, then delete
the application namespaces (`netra-system`, `veyron-*`, `envoy-gateway-system`).
Their PersistentVolumes use the `Delete` reclaim policy, so their data goes with
them. Keep k3s, Cilium, the Kairon host services, the runners, and the
`kairon-lab` ServiceAccount, RBAC and repo secrets: removing the cluster
invalidates `KAIRON_KUBE_TOKEN` and `KAIRON_KUBE_CA`. CRDs and cluster-scoped
roles of the removed apps outlive their namespaces; delete them by name.
