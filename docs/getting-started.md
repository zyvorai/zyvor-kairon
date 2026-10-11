---
sidebar_position: 2
title: Getting started
---

# Getting started

## Prerequisites

- Kubernetes cluster and Kairon-capable nodes.
- KVM and a reachable FluxVM service on each VM node.
- VM image paths available below `--image-root`.
- CSI snapshot stack for `MachineSnapshot`.
- Kubernetes DRA plus administrator-approved BDFs for VFIO.

## Install

Preferred: `kaironctl` embeds the Helm chart, so no separate `helm` binary or chart checkout is required.

```bash
kaironctl install
kubectl label node worker-1 kairon.zyvor.dev/capable=true
kubectl label node worker-2 kairon.zyvor.dev/capable=true
kaironctl status
kaironctl doctor     # health checks with a hint for every problem
kaironctl selftest   # boots a tiny CirrOS VM and checks that it all works
```

See [Self-test](guides/selftest.md) for what it checks and how to read the result.

### Evaluation vs production

Default chart values are an **evaluation** profile (webhook and namespace isolation off; image tags default to `.Chart.AppVersion`). For a hardened install, use the production overlay (`kaironctl install --profile production`, or with Helm directly):

```bash
kaironctl install --profile production
# or
helm install kairon charts/kairon -n kairon-system --create-namespace \
  -f charts/kairon/values-production.yaml
```

That enables admission webhooks, namespace-scoped UI authorization, network default-deny, and migration dataplane TLS requirements. See [`charts/kairon/values-production.yaml`](../charts/kairon/values-production.yaml) and [`docs/COMPATIBILITY.md`](COMPATIBILITY.md).

### Install from the published chart (GHCR)

Tagged releases publish the Helm chart and the images to GitHub Container Registry, so no repository checkout is needed:

```bash
helm upgrade --install kairon oci://ghcr.io/zyvorai/charts/kairon \
  --version 0.9.1 -n kairon-system --create-namespace \
  -f https://raw.githubusercontent.com/zyvorai/kairon/v0.9.1/charts/kairon/values-production.yaml
```

- Images are `ghcr.io/zyvorai/kairon-{controller,node,ui,csi-node,csi-controller}:<version>`; the chart's image tags default to its `appVersion`, so the chart version selects the images.
- Drop the `-f` overlay for the evaluation profile.
- `kaironctl` installs the same published chart without Helm: `kaironctl install --chart oci://ghcr.io/zyvorai/charts/kairon --version 0.9.1 --profile production`.
- Helm applies `crds/` only on first install. When upgrading, apply the CRDs from the same tag first (`kubectl apply --server-side -f https://raw.githubusercontent.com/zyvorai/kairon/v0.9.1/deploy/crd.yaml`); see [CRD versioning](guides/crd-versioning.md#upgrading-an-existing-cluster).
- Fetch the matching `kaironctl` from the release: `https://github.com/zyvorai/kairon/releases/download/v0.9.1/kaironctl-linux-amd64`, or `kubectl krew install kairon`.

Later, `kaironctl upgrade` keeps the values you installed with (`--reset-values` returns to chart defaults), `kaironctl upgrade --check` compares your release with the latest one without changing anything, `kaironctl config set KEY=VALUE` changes a single value, and `kaironctl history` / `kaironctl rollback` list and restore revisions. Add `--context NAME` or `--kubeconfig PATH` to target a specific cluster. See [CLI](CLI.md).

Raw manifests still work:

```bash
kubectl apply -f deploy/crd.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/controller.yaml
kubectl apply -f deploy/node.yaml
```

## Create a Machine

```bash
kubectl apply -f examples/linux-machine.yaml
kubectl get machines -A -w
```

Or via `kaironctl`, with an SSH key and port forward set at creation (both
only take effect at creation -- see [guides/machine-network.md](guides/machine-network.md)):

```bash
kaironctl create demo --image /var/lib/fluxvm/images/ubuntu-24.04.qcow2 \
  --ssh-key "$(cat ~/.ssh/id_ed25519.pub)" --forward=2222:22
ssh -p 2222 <node-ip>
```

## Cold migrate

```bash
kaironctl migrate demo --strategy cold --target-node worker-2
```

## Enable the secure live-migration peer

Use the Helm chart and provide `kairon-migration-tls` with `ca.crt`, `tls.crt`, and `tls.key`, then enable `migration.enabled=true`. The chart credential must have `serverAuth` + `clientAuth` EKUs and DNS SAN `kairon-node` unless `migration.tlsServerName` is changed.

A real live transfer additionally requires a Kairon migration adapter on each node (`cmd/kairon-migration-adapter-fluxvm`; not installed automatically today -- see [`runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md)). Without one configured, an explicit live request is safely blocked before source transfer.

```bash
kaironctl migrate demo --strategy live --target-node worker-2 --mode pre-copy
```

## Snapshot

```bash
kaironctl snapshot database --name database-before-upgrade --class csi-snapclass
kaironctl get snapshots
```

## Deploy the web dashboard

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system --set ui.enabled=true
kubectl -n kairon-system port-forward svc/kairon-ui 18082:18082
```

The dashboard serves **HTTPS** by default. With nothing configured each pod
generates a self-signed certificate at startup, so your browser warns once and
the certificate changes on every restart: fine to evaluate. For a stable
certificate set `ui.tls.secretName` to a `kubernetes.io/tls` Secret (for
example from cert-manager), or `ui.tls.sans` to add the names it should cover.
Plain HTTP is an explicit opt-out (`ui.insecureHttp: true`, only behind a
proxy that terminates TLS); see the [dashboard guide](guides/kairon-ui-dashboard.md#https).

No `ui.*` auth values needed: with nothing else configured, a fresh install
seeds a default `admin` account with the documented lab password
**`Admin@321`** (change it before exposing the dashboard; the production
profile generates a random one instead). Confirm or retrieve the actual
password any time with:

```bash
kubectl -n kairon-system get secret kairon-ui-session -o jsonpath='{.data.defaultAdminPassword}' | base64 -d; echo
```

Open `https://127.0.0.1:18082` (accept the self-signed certificate warning) and sign in as `admin` with that password. Or skip the port-forward: `kaironctl ui` opens the dashboard through the API server.

| Page | What it shows |
|---|---|
| **Overview** | Fleet tiles by phase, a prominent warning whenever anything is parked in `NeedsRecovery` |
| **Machines** | Table with filter box and row selection (bulk Start/Stop) + create form (mirrors `kaironctl create`) + Start / Stop / Console / Migrate / Snapshot / Delete; the nav namespace selector switches namespace or lists every namespace |
| **Migrations** | List with phase badges, a "New Migration" form, an "Evacuate Node" action, `status.dataPlaneEncrypted` badge, a "Cancel migration" action while `Starting`/`Running`, and the recovery workflow |
| **Snapshots** | List + create form |
| **Nodes** | Real Kubernetes `Node` list — `Ready`, `spec.unschedulable`, taints, addresses, plus per-node Machines/CPU/Memory usage rollup; read-only |
| **Storage** | Read-only Atlas volumes, pools, backups and health; needs `ui.atlas.*` ([Atlas storage](guides/machine-storage-atlas.md#dashboard-storage-page)) |
| **Pools & claims**, **Backups**, **Images** | Warm [MachinePools](guides/machine-pools.md) and claims, [machine backups](guides/machine-backup.md) and restores, uploaded images |
| **Machine sets**, **Restores**, **Snapshot schedules**, **Quotas**, **Policies** and more | The remaining resource pages; the full list is in the [dashboard guide](guides/kairon-ui-dashboard.md) |
| **Account** | Change your own password; an admin account can reset another operator's |

For real, named per-operator accounts instead of the single generated admin,
set `ui.auth.users` -- generate each password's hash first:

```bash
kairon-ui -hash-password 'a real password'   # prints a bcrypt hash; pipe from a file/heredoc, don't type a real password on the command line
```

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set ui.enabled=true \
  --set ui.auth.users[0].username=alice \
  --set ui.auth.users[0].passwordHash='$2a$10$...'
```

The legacy single shared token (`ui.token`, or `ui.allowUnauthenticated=true`
for local development) still works unchanged for existing deployments, and
is accepted alongside `ui.auth.users` if both are set.

Repeated failed logins against one username are rate-limited (5 failures
locks that username out for 5 minutes, `429` with `Retry-After`) -- tracked
per requested username, including unknown ones, so the lockout itself
can't be used to enumerate valid accounts.

### VNC console

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system --set ui.enabled=true --set console.enabled=true
```

Adds a "Console" button per Machine in the dashboard -- a real graphical
VNC session in the browser, for QEMU-backend Machines only (the button
hides itself for ineligible Machines, or entirely when `console.enabled`
is off). Read [SECURITY.md](https://github.com/zyvorai/kairon/blob/main/SECURITY.md)'s "VNC console" section first:
FluxVM's own VNC socket has no auth of its own, so this feature's security
rests on kairon-ui's operator auth, a single-use connection ticket bound
to the requesting username (every session is audit-logged), and a shared
token between kairon-ui and every kairon-node -- appropriate for a trusted
operator team, not a hostile-network or multi-tenant deployment. That last
hop can optionally run over one-way TLS instead of plaintext HTTP -- see
`console.tls.enabled`/`console.tls.secretName` in `values.yaml`.

**Prerequisite confirmed against a real deployment**: FluxVM creates
`vnc.sock` root-owned with no `other` write bit, and `kairon-node` runs as
an unprivileged, deliberately capability-less user -- without OS-level
access to that socket, the console fails with a clear 502
(`dial vnc socket: ... permission denied`), not a silent hang. Grant
`kairon-node`'s user read/write access to FluxVM's per-VM socket files
(a POSIX ACL, or a shared group between the FluxVM and kairon-node service
users) before expecting `console.enabled` to actually work end-to-end.

Bare-metal alternative:

```bash
scripts/deploy-remote.sh sus@80.79.5.173 --with-controller --with-ui --with-console
```

Installs `kairon-ui` as a systemd service alongside `kairon-node`/`kairon-controller`; a dashboard token is auto-generated and printed once at the end of the run. Requires `npm` locally to build `web/dist` -- the only place this script needs Node.js. `--with-console` (optionally `--console-port=N`, otherwise auto-picked if the default collides) generates the shared console token and wires it into both services' systemd env files. Add `--console-tls` to also enable TLS on that hop -- with no `--console-tls-cert`/`-key`/`-ca`, a private CA and server certificate are generated locally and installed on the remote host automatically (SAN list is a best-effort guess at the host's own addresses; if kairon-ui's console button fails with a TLS error afterward, regenerate with explicit `--console-tls-cert`/`-key`/`-ca` covering the right one), or pass those three yourself to bring your own.

**Redeploy caveats (confirmed on `80.79.5.173`):**

- Existing `/etc/kairon/kairon-node.env` and `kairon-ui.env` are **left untouched**. `--with-console` / `--ui-token` / `--kube-*` do not rewrite them — add matching `KAIRON_NODE_CONSOLE_TOKEN` (and `KAIRON_NODE_CONSOLE_PORT=8090` on the UI side) by hand, then `systemctl restart kairon-node kairon-ui`. Without that shared token, dashboard **Network** / diagnostics / VNC return `501 diagnostics are not enabled`.
- Health ports default to controller `:32301`, node `:32302`, UI `:18082` (change with `--ui-port=N`, `KAIRON_UI_LISTEN`, or the chart's `ui.service.port`). Do not use `:8080` for kairon; it belongs to `krytond` on this host. If a default is busy, the script picks a random free port, or you can pin `--controller-port=N` / `--node-port=N`.
- A port held by the kairon service being redeployed counts as free, so a redeploy keeps the same ports without stopping the services first.
- `--reserved-cpus=LIST` sets `KAIRON_RESERVED_CPUS` in the node env file; adding `--cpu-pinning` (which requires reserved CPUs) turns on `pinnable-cpus` discovery by also installing `kairon-cpustate.path`, which copies kubelet's root-only `cpu_manager_state` to where kairon-node can read it. `--no-cpu-pinning` undoes both. See [CPU pinning](guides/machine-cpu-pinning.md#turning-on-discovery).
- Opt-in controller features read env keys from `/etc/kairon/kairon-controller.env`, for example `KAIRON_STALE_EVACUATION=true` and `KAIRON_NODE_LIVENESS_LEASE_NAMESPACE=kairon-system` for [stale evacuation](guides/machine-fencing.md#automatic-still-attested---stale-evacuation); restart `kairon-controller` after editing.

- kairon-node keeps its per-Machine traffic baseline in `/var/lib/kairon-node/edge-baseline.json` (systemd `StateDirectory`), so a restart keeps the warm-up.
- Network **flows / stats / drop-reasons** need FluxVM's eBPF dataplane (`mode: tap`, `netns: true`, `dataplaneMode: ebpf`). A Machine on `network.mode: user` can still return `network-effective`, but flow/drop/stats relays typically 502.

## Network Fabric (eBPF edge)

Recommended production dataplane (FluxVM TC/eBPF — Kairon declares policy, FluxVM owns BPF):

```yaml
spec:
  network:
    mode: tap
    netns: true
    dataplaneMode: ebpf
    dataplaneRequired: true
```

```bash
kubectl apply -f examples/network-fabric-machine.yaml
kaironctl network status web
kaironctl network flows web --limit 20
kaironctl network drop-reasons web
```

On a cluster that already runs Cilium as the CNI, three **additional** opt-in paths exist (all off by default):

| Path | What you set |
|---|---|
| FluxVM cilium dataplane | `/etc/fluxvm.toml` `sandbox.dataplane.mode = "cilium"` and/or `spec.network.dataplaneMode: cilium` |
| Cluster network attach | Helm `network.ciliumAttach.enabled=true` plus `spec.network.ciliumAttach: true` (`mode: tap`, `netns: true`) |
| Policy visible to Hubble | Helm `network.ciliumPolicySync.enabled=true` plus `spec.cilium.sync: true` on a `MachineNetworkPolicy` |

- Tutorial: [tutorials/network-fabric.md](tutorials/network-fabric.md)
- User guides: [guides/machine-network.md](guides/machine-network.md), [guides/network-policy.md](guides/network-policy.md)
- Reference: [network-fabric.md](network-fabric.md)
