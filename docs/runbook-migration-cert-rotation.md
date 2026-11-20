# Runbook: rotating the live-migration certificates

Live migration uses two sets of TLS material per host, both issued by one
private CA (see [`guides/machine-migration-tls.md`](guides/machine-migration-tls.md)):

| Files in `ETC_DIR/migration/` | Used by |
|---|---|
| `ca.pem`, `cert.pem`, `key.pem` | the node agent's peer-to-peer migration control plane (mTLS, server name `kairon-node`) |
| `adapter-ca.pem`, `adapter-cert.pem`, `adapter-key.pem` | the migration adapter's RAM/state data stream (per-host SAN) |

`scripts/gen-migration-mtls-certs.sh` issues a **10-year CA** and **90-day
leaf certificates**. Rotate the leaves well before they expire; the CA does
not change, so trust on every host stays as it is.

## What happens at expiry

New migrations fail with a TLS error (`certificate has expired`, visible in
the node agent's journal and as a failed or `Blocked` MachineMigration).
Running VMs are not affected. Replacing the files and restarting the two
services fixes it.

## Check the expiry

```bash
for h in nldw3 nldw4; do
  ssh "$h" 'for f in cert adapter-cert; do
    printf "%s %s: " "$(hostname)" "$f"
    sudo openssl x509 -in /etc/kairon/migration/$f.pem -noout -enddate
  done'
done
```

Alert on anything under 30 days. (A Prometheus rule on
`kairon_migration_*` does not cover this; the certificate dates are only on
disk.)

## Rotate the leaves (same CA)

Keep the CA directory (`ca.pem` + `ca-key.pem`) offline and bring it to
the machine you rotate from. Nothing else needs the CA key.

```bash
# 1. nothing in flight (the script also refuses if kaironctl sees one)
kaironctl get migrations

# 2. issue and verify the new leaves, change nothing
scripts/rotate-migration-certs.sh --ca-dir /path/to/ca --dry-run \
  user@10.0.0.11=host-a,/etc/kairon,kairon-node,kairon-migration-adapter-fluxvm \
  user@10.0.0.12=host-b,/etc/kairon,kairon-node,kairon-migration-adapter-fluxvm

# 3. rotate: same arguments without --dry-run
```

For each host the script copies `migration/` to `migration.bak-<timestamp>`,
installs the six files, then restarts the adapter and the node agent. A
restart does not stop VMs.

The `NAME` in each HOST_SPEC must match the name the data-plane certificate
was issued for (`gen-migration-mtls-certs.sh OUT NAME=IP ...`), because the
adapter's certificate carries `DNS:NAME` and `IP:IP` SANs.

## Verify

```bash
# the peer port answers mTLS with the new certificate
sudo openssl s_client -connect 10.0.0.12:9443 -servername kairon-node \
  -CAfile /etc/kairon/migration/ca.pem \
  -cert /etc/kairon/migration/cert.pem -key /etc/kairon/migration/key.pem </dev/null \
  | grep -E 'Verification|Verify return'

# and an actual move
kaironctl migrate MACHINE --strategy live --target-node host-b
kaironctl get migrations
```

## Roll back

```bash
sudo rm -rf /etc/kairon/migration && sudo mv /etc/kairon/migration.bak-<timestamp> /etc/kairon/migration
sudo systemctl restart kairon-migration-adapter-fluxvm kairon-node
```

## Replace the CA itself

Only needed if the CA key is lost or exposed, or the CA is near its end.
Generate a new CA with `gen-migration-mtls-certs.sh`, then rotate **all**
hosts in a single run of `rotate-migration-certs.sh` with `--ca-dir` pointing
at the new CA. Between the first and last host restart the hosts do not trust
each other, so do it with no migration in flight and expect any migration
started in that window to fail.

## Hosts with two node services

A host that runs a second node agent for another cluster (a lab host that
serves two clusters, for example) has a separate directory and unit names for
each, such as `/etc/kairon-lab` with `kairon-node-lab`. Pass one HOST_SPEC per
instance, with that instance's `ETC_DIR` and units.
