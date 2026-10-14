# Runbook: real two-host live-migration test

Everything Kairon's own test suite (`internal/agent`, `internal/controller`,
and the new `internal/integration`, see `docs/runbook-migration-failures.md`)
exercises against fakes and loopback connections. None of it proves a real
QEMU migration across a real network, with real mTLS hostname verification
and a real guest network handoff, actually works. This runbook is that test.

**Executed by hand on 2026-10-14** on a two-host lab with NFS shared storage
(results in `docs/COMPATIBILITY.md`), but not part of this repo's CI. Every
value below (hostnames, IPs, storage transport) is environment-specific.
Use the same QEMU release on both hosts (or migrate from the older to the
newer one): FluxVM refuses to send a newer machine type to an older QEMU.

## Prerequisites

| Requirement | Why |
|---|---|
| Two Linux hosts with KVM + FluxVM installed and reachable over SSH | one source, one destination |
| A **shared filesystem mount at an identical path on both hosts** (NFS, a shared block device, or equivalent) holding the guest's `.qcow2` image | `kairon-migration-adapter-fluxvm` migrates QEMU RAM/state over the network, but does **not** ship the disk image itself -- the destination must already be able to open the same image path. This is genuinely environment-specific; no generic script can set it up. |
| L2/L3 network reachability between hosts for the guest's own network (bridge/VLAN reachable from both) | guest network continuity across cutover |
| A Kubernetes API reachable from both hosts, with a `Node` object already registered for each host's `NODE_NAME` | `kairon-node`/`kairon-controller` need this regardless of migration; not migration-specific setup |
| `openssl` on your local machine (for cert generation) | `scripts/gen-migration-mtls-certs.sh` |

## 1. Generate mTLS material

```
scripts/gen-migration-mtls-certs.sh ./certs vm-host-1=10.0.1.11 vm-host-2=10.0.1.12
```

Replace the two `NAME=IP` pairs with your real hostnames and the real IPs
each host's adapter will advertise (the address QEMU peers actually dial --
see the script's own comments for why the control-plane and data-plane
certs have deliberately different SAN shapes).

## 2. Deploy

```
scripts/multi-host-migration-test-deploy.sh ./certs \
  root@10.0.1.11=vm-host-1 \
  root@10.0.1.12=vm-host-2 \
  --kube-url=https://YOUR-CLUSTER-API:6443 \
  --kube-token=YOUR-TOKEN
```

This wraps `scripts/deploy-remote.sh` **unmodified** for `kairon-node` (+
`kairon-controller` on the first host), then closes the one gap
`deploy-remote.sh` has today (confirmed via `grep -n adapter
scripts/deploy-remote.sh`): it cross-compiles the real
`kairon-migration-adapter-fluxvm` binary, installs
`systemd/kairon-migration-adapter-fluxvm.service`, and stages each host's
own data-plane certs. See `--help`/the script's own header comment for the
full flag list (kube CA, `--fluxvm-url` if not the default
`http://127.0.0.1:7788`, `--arch`, etc.).

Verify both hosts came up:

```
ssh root@10.0.1.11 systemctl status kairon-node kairon-controller kairon-migration-adapter-fluxvm
ssh root@10.0.1.12 systemctl status kairon-node kairon-migration-adapter-fluxvm
kaironctl get nodes      # both Nodes should be visible to the controller
```

## 3. Create a test Machine

Use an existing Machine already scheduled to `vm-host-1` (or `kaironctl
create` one -- see `docs/getting-started.md`), pointed at an image path that
resolves identically on both hosts (the shared mount from the prerequisites
table).

## 4. Run the migration

```
scripts/multi-host-migration-test-run.sh MACHINE_NAME vm-host-2 --timeout=600
```

Polls `machinemigration`'s phase every 3s and exits non-zero (pointing at
`docs/runbook-migration-failures.md`) if it lands `Failed`/`Blocked`/
`NeedsRecovery`, or times out.

## 5. Manual verification checklist

None of these are automated -- confirm each by hand:

- [ ] **RAM transfer cross-check**: the migration's `status.ramTransferred`/
      `ramTotal` (`kaironctl get`/`describe` do not print them, so use
      `kubectl get machinemigration NAME -o yaml`) roughly matches what FluxVM's own
      status API reports for the runtime independently -- Kairon's own
      numbers aren't the only source of truth.
- [ ] **Guest IP/MAC survives cutover**: `ssh` (or console) into the guest
      after migration; its IP and MAC are unchanged from before.
- [ ] **Went through the real Kairon pipeline, not a side channel**: `kubectl
      get events --field-selector involvedObject.name=MACHINE_NAME` and
      `scripts/must-gather.sh` both show the expected sequence
      (Starting/Running/Cutover/Adopting/Succeeded).
- [ ] **`status.dataPlaneEncrypted` is `true`** for the completed migration
      (this deploy script installs the adapter with `--migration-data-tls=true`
      -- see `systemd/kairon-migration-adapter-fluxvm.service`) -- confirms
      the new field from Part II of this work threads through correctly
      against a real adapter, not just the unit tests' fake one.
- [ ] **Deliberate wrong-SAN negative test** (do this once, separately from
      a real migration you care about): re-run
      `scripts/gen-migration-mtls-certs.sh` for a third host name/IP the
      adapter never advertises, install those data-plane certs on
      `vm-host-2` in place of its own, restart
      `kairon-migration-adapter-fluxvm`, and attempt a migration -- it
      **must** fail with a TLS hostname verification error, not silently
      succeed. This is the one property `internal/migration/tls.go`'s unit
      tests can't exercise (they never dial a real socket with a real
      mismatched hostname). Revert to the real certs afterward.

## 6. Teardown

```
scripts/multi-host-migration-test-teardown.sh root@10.0.1.11 root@10.0.1.12
```

Add `--purge` to also remove `/etc/kairon` and the `kairon` system user on
both hosts (matches `deploy-remote.sh --uninstall --purge`'s own semantics).

## Related

- Migration failures and `NeedsRecovery` in general (not multi-host
  specific): `docs/runbook-migration-failures.md`.
- Deliberately forcing a live `NeedsRecovery` on this same two-host setup,
  to rehearse operator recovery: `docs/runbook-recovery-drill.md`.
