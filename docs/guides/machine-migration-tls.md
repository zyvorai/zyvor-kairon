# User guide: migration TLS -- control plane vs. data plane

Live migration has **two separate TLS hops**, verified differently, and
this page is about the difference -- read it before assuming "TLS is on"
means the same thing for both.

## The two hops

1. **Control plane** (`migration.tlsSecretName`, always mTLS when
   `migration.enabled`): the `prepare`/`transfer`/`commit` RPCs between two
   `kairon-node` peers. Deliberately **one shared cluster-wide cert** for
   every node -- peers are addressed by Kubernetes node InternalIP, not a
   stable per-host name, so this cert answers "is this a Kairon node,"
   never "is this host `10.0.1.12` specifically." See
   `internal/migration/tls.go` and `scripts/gen-migration-mtls-certs.sh`'s
   own header comment for the full reasoning.
2. **Data plane** (`migration.dataplaneTls`, opt-in, off by default): the
   actual QEMU RAM/state stream between the two hosts' FluxVM processes,
   via `kairon-migration-adapter-fluxvm`'s own `-migration-data-tls`. This
   is architecturally separate from the control plane -- a different
   binary, different cert material, and (unlike the control plane) it
   already does real per-connection hostname verification: the source
   adapter passes the destination's real advertised host as the expected
   TLS name, and QEMU checks it. A shared identity here is a real,
   closeable weakness, not a deliberate tradeoff the way it is for the
   control plane.

`status.dataPlaneEncrypted` (and the `kairon_migration_dataplane_encrypted`
metric) only ever reports on the **data plane** -- the control-plane RPCs
are always mTLS-encrypted regardless, so there's nothing to report there.

## Real per-node data-plane identity

```bash
./scripts/gen-migration-mtls-certs.sh ./certs \
  worker-1=10.0.1.11 worker-2=10.0.1.12 worker-3=10.0.1.13
kubectl apply -f ./certs/data-plane/kairon-migration-dataplane-tls.secret.yaml
```

```yaml
migration:
  dataplaneTls: true
  dataplaneTlsSecretName: kairon-migration-dataplane-tls
```

The `HOST_NAME` arguments must be each node's **real Kubernetes node
name** (`kubectl get nodes`), since the init container matches against
`spec.nodeName` exactly, not an arbitrary label. The script writes one
Secret bundling every node's own cert (keys `<name>-ca.crt`/
`<name>-tls.crt`/`<name>-tls.key`) -- a single object every `kairon-node`
pod mounts, each one picking out only its own three keys via the Downward
API before staging them onto `adapterHostPath` for the host-level adapter
systemd unit to read. If a node's own entry is missing from the Secret,
that pod's init container fails outright (never falls back to a shared or
wrong identity) -- verified against a real cluster: a Secret missing the
scheduled node's own entry produces a clear `FATAL: no per-node
data-plane cert for node <name> ...` and the pod never comes up, rather
than silently succeeding with the wrong cert.

Leaving `dataplaneTlsSecretName` unset (the default) falls back to
staging the shared `tlsSecretName` control-plane cert onto the data plane
too -- exactly this project's behavior before this field existed, so
existing deployments' `helm upgrade` changes nothing on its own.

## Let Helm run the adapter

By default the host-level adapter is a bare-host systemd unit Helm can't
see (next section). Set `migration.adapter.enabled: true` and the chart
runs `kairon-migration-adapter-fluxvm` itself, as a `kairon-migration-adapter`
DaemonSet on the same nodes as `kairon-node`, from the `kairon-node`
image. One set of values then enables encrypted migration end to end:

```yaml
migration:
  enabled: true
  # Host directory, writable by UID 65532 on every node. Holds the staged
  # certs (tls/) and the adapter's socket.
  adapterHostPath: /var/lib/kairon/migration-adapter
  dataplaneTls: true
  adapter:
    enabled: true
    # fluxvmTokenSecret: {name: fluxvm-token, key: token}  # if FluxVM needs auth
  # Per-node certs: either a gen-migration-mtls-certs.sh bundle ...
  dataplaneTlsSecretName: kairon-migration-dataplane-tls
  # ... or cert-manager (needs its CRDs; mutually exclusive with the above):
  # dataplaneCertManager:
  #   enabled: true
  #   createSelfSignedCA: true          # or issuerRef: {name: my-ca, kind: ClusterIssuer}
  #   nodes:
  #     - {name: worker-1, ip: 10.0.1.11}
  #     - {name: worker-2, ip: 10.0.1.12}
  # Then, once every migration reports status.dataPlaneEncrypted=true:
  # requireEncryptedDataPlane: true
```

What the chart wires up:

- The adapter listens on `<adapterHostPath>/migration-adapter.sock`, and
  `kairon-node`'s `--migration-adapter-socket` defaults to it (an explicit
  `migration.adapterSocket` still wins).
- `--advertise-host` is the node's `status.hostIP`, the value the systemd
  unit's `__ADVERTISE_HOST__` placeholder was filled with by hand. With
  `dataplaneCertManager` each node's `ip` must be that same address, since
  QEMU verifies it.
- With `dataplaneTls: true` the adapter gets `--migration-data-tls=true`
  plus `<adapterHostPath>/tls/{ca.crt,tls.crt,tls.key}`, the files
  `kairon-node`'s existing init container stages there. The adapter passes
  those paths to FluxVM, which opens them on the host, so the DaemonSet
  mounts `adapterHostPath` at its real host path and runs `hostNetwork`
  (FluxVM's API is on the host's loopback, `node.fluxvmURL`).
- `dataplaneCertManager` creates one cert-manager `Certificate` per listed
  node (SAN = node name + IP, client and server auth, ECDSA P-256, the same
  shape the script issues), optionally from a chart-made private CA.
  `kairon-node` mounts the resulting Secrets as one projected volume in the
  same `<node>-ca.crt`/`-tls.crt`/`-tls.key` layout, so the per-node init
  container keeps failing closed for a node that isn't listed or whose
  cert isn't issued yet. The issuer must fill `ca.crt` (CA-type issuers
  do; ACME does not).
- The adapter needs no Kubernetes API access: no ServiceAccount token, no
  RBAC.

Before turning it on for a node, stop and disable that node's
`kairon-migration-adapter-fluxvm` systemd unit (`systemctl disable --now
kairon-migration-adapter-fluxvm`). The image must be one built with this
change (the `kairon-node` image ships `/kairon-migration-adapter-fluxvm`
from the release after it); override `migration.adapter.image` otherwise.

## Host systemd adapter driven by Helm

On nodes that keep the bare-host `kairon-migration-adapter-fluxvm` unit,
set `migration.adapter.hostEnvFile: true` instead of `adapter.enabled`.
Helm then controls the unit's settings through a file instead of its
`ExecStart`:

```yaml
migration:
  enabled: true
  adapterHostPath: /var/lib/kairon/migration-adapter   # the drop-in's path
  dataplaneTls: true
  dataplaneTlsSecretName: kairon-migration-dataplane-tls   # or dataplaneCertManager
  dataplaneTlsRefreshSeconds: 300                          # optional, see below
  adapter:
    hostEnvFile: true
```

`kairon-node`'s `migration-adapter-tls` container stages the certs and
writes `<adapterHostPath>/adapter.env` with the socket,
`KAIRON_MIGRATION_ADVERTISE_HOST` (the node's `status.hostIP`),
`KAIRON_MIGRATION_DATA_TLS` (from `dataplaneTls`), the cert paths and
`migrationNetworks`. `kairon-node` dials `<adapterHostPath>/migration-adapter.sock`.
The adapter reads those variables as flag defaults (an explicit flag still
wins). A value of `KAIRON_MIGRATION_DATA_TLS` that is not a bool stops it
instead of falling back to cleartext.

One-time setup per node, as root, with the adapter binary from this release
or later at `/usr/bin/kairon-migration-adapter-fluxvm` and the `kairon` user
present:

```bash
install -d -o 65532 -g 65532 -m 0770 /var/lib/kairon/migration-adapter
install -m 0644 systemd/kairon-migration-adapter-fluxvm.service \
  systemd/kairon-migration-adapter-fluxvm-env.path \
  systemd/kairon-migration-adapter-fluxvm-env.service /etc/systemd/system/
install -D -m 0644 systemd/kairon-migration-adapter-fluxvm.service.d/helm-env.conf \
  /etc/systemd/system/kairon-migration-adapter-fluxvm.service.d/helm-env.conf
systemctl daemon-reload
systemctl enable kairon-migration-adapter-fluxvm.service
systemctl enable --now kairon-migration-adapter-fluxvm-env.path
```

The drop-in replaces `ExecStart`, so the base unit installs as shipped with
no placeholder edits. It loads `adapter.env`, runs the adapter with group
65532 so `kairon-node` (UID/GID 65532) can use its 0660 socket, and does
not start until the file exists. The `.path` unit restarts the adapter
whenever the chart changes the file. The chart rewrites the file only when
its content changes, so after this setup `helm upgrade` alone turns
encryption on or off. A different `adapterHostPath` needs the same path in
`helm-env.conf` and the `.path` unit.

Rollout order still matters. Every node must have data-plane TLS on before
`requireEncryptedDataPlane`. The chart refuses
`requireEncryptedDataPlane` with `hostEnvFile` or `adapter.enabled` while
`dataplaneTls` is off.

## Renewed certificates

By default the certs are staged once, when `kairon-node` starts. Set
`migration.dataplaneTlsRefreshSeconds` (for example 300) and the staging
container becomes a native sidecar (Kubernetes 1.29+) that re-copies them
on that interval. A file is replaced atomically and only when its content
changed. `kairon-node` still waits for the first good copy, and a node
whose own cert is missing still never starts. If a later copy fails, the
previous files stay in place and the container logs a warning. The adapter
hands FluxVM the cert paths for each migration, so neither the adapter nor
`kairon-node` needs a restart. A migration that is already running keeps
the cert it started with.

## Remember the other half (systemd adapter)

With the default `migration.adapter.enabled: false` (and `hostEnvFile: false`),
`migration.dataplaneTls`/`dataplaneTlsSecretName` only control what cert
material gets *staged* onto `adapterHostPath`. The actual
`-migration-data-tls=true` enforcement flag lives in each node's
`kairon-migration-adapter-fluxvm` **systemd unit** `ExecStart`, entirely
outside Helm's control -- flipping the Helm values alone does not turn
data-plane encryption on cluster-wide. See
[`docs/runbook-migration-failures.md`](../runbook-migration-failures.md#unencrypted-migration-data-plane)
for the full two-places-at-once checklist, and verify
`status.dataPlaneEncrypted=true` on a real in-flight migration before
trusting it's actually on.

## Real limits today

- With a hand-configured systemd adapter, `dataplaneTls`/`dataplaneTlsSecretName`
  control only cert *distribution*, not the unit's enforcement flag (above),
  and the two can drift out of sync per node. Two settings close that gap:
  `migration.adapter.enabled` (DaemonSet) and `migration.adapter.hostEnvFile`
  (unit plus drop-in). `hostEnvFile` still needs the one-time unit install
  above; Helm cannot write host systemd units.
- `hostEnvFile` only rewrites `adapter.env` while `kairon-node` runs on the
  node. If you uninstall the chart, or turn off both `dataplaneTls` and
  `hostEnvFile`, the last file stays on the host. Delete it, or disable the
  unit, yourself.
- Without `dataplaneTlsRefreshSeconds`, staged certs are copied once, at
  `kairon-node` pod start. A renewed cert (from cert-manager or a re-applied
  Secret) then reaches FluxVM only after `kairon-node` restarts on that node
  (`kubectl rollout restart ds/kairon-node`).
- Whether FluxVM rereads the files for each migration is up to FluxVM. The
  adapter passes the paths with every request, but no two-host run has yet
  confirmed that a renewed cert is used without a FluxVM restart.
- `gen-migration-mtls-certs.sh`'s default 30-day CA validity is meant for
  the [two-host migration test runbook](../runbook-multi-host-migration-test.md),
  not long-lived production issuance -- override `CERT_DAYS` (and rotate
  manually; nothing here automates renewal) if you use it for a real
  deployment.
- The control-plane cert stays shared cluster-wide regardless -- this
  page doesn't change that, and doing so would need new peer-identity
  verification code that doesn't exist today (see SECURITY.md).
- Verified end-to-end against a real cluster for cert *selection*
  (a real pod picking its own node's files out of a multi-node Secret,
  and failing closed when its own entry is missing) -- not for actual
  cross-host QEMU migration traffic flowing encrypted, which needs two
  real hosts this project's own lab environment doesn't have (the same
  limitation documented for real two-host live migration generally).
