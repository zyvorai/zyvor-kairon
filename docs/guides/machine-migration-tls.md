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

## Remember the other half

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

- `dataplaneTls`/`dataplaneTlsSecretName` control cert *distribution*
  only, not the systemd-unit-side enforcement flag (above) -- the two can
  drift out of sync per node.
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
