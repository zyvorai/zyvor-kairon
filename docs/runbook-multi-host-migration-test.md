# Runbook: real two-host live-migration test

Everything Kairon's own test suite (`internal/agent`, `internal/controller`,
and the new `internal/integration`, see `docs/runbook-migration-failures.md`)
exercises against fakes and loopback connections. None of it proves a real
QEMU migration across a real network, with real mTLS hostname verification
and a real guest network handoff, actually works. This runbook is that test.

**Not executed as part of shipping this runbook** -- it requires a second
real (or nested-virtualization) KVM host beyond this project's one lab host,
and every value below (hostnames, IPs, storage transport) is
environment-specific. Follow it, adjusting the bracketed values, when a
second host is available.

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

## Lessons from the first real two-host run (2026-10-14)

Hosts `80.79.5.173` (FluxVM 0.3.0, QEMU 10.2.1) and `175.110.122.71`
(FluxVM 0.4.8, QEMU 8.2.2), 175 exporting `/srv/kairon-shared` over NFS to 80.
Check these before blaming Kairon for a stalled or `Blocked` live migration:

- **Same FluxVM on both hosts, new enough for `storage: shared`.** A live
  migration is refused with `live migration needs the disk on shared storage
  (storage: shared ...)`, and a Machine create fails with `storage: unknown
  variant 'shared'` (HTTP 422), when a host's FluxVM predates that mode.
- **The Machine must declare shared storage.** `spec.image.path` is not
  enough: boot from `spec.volumes[0]` on a `hostPath` PersistentVolume
  annotated `kairon.zyvor.dev/shared-storage: "true"` (path = a directory
  holding `disk.img`, identical on both hosts) with a Bound claim. Storage is
  creation-time-only, so recreate Machines created the old way.
- **TCP 9443 must be reachable from every host to every other.** The source
  node's `prepare target` call is an HTTPS POST to the target's `:9443`;
  a blocked port shows as `context deadline exceeded` on the source and
  nothing on the target. Orphaned CNI BPF programs (for example Cilium's
  `cil_from_netdev` left on the NIC after uninstalling k3s) can drop it
  silently; a reboot clears them.
- **Adapter ordering.** After a reboot, `kairon-migration-adapter-fluxvm` can
  fail with "Dependency failed" if `fluxvm.service` was not up yet; check
  `systemctl is-active` on both hosts before starting.
- **Console relay port.** kairon-ui's `KAIRON_NODE_CONSOLE_PORT` must equal
  every kairon-node's `KAIRON_NODE_CONSOLE_ADDR` port, and both must use the
  same TLS setting; otherwise guest-agent exec (used by the load cases)
  fails with "connect to node exec relay".
- **Self-signed kairon-ui.** The certificate regenerates on every restart;
  re-pin it (`openssl s_client`) before running the matrix with
  `CURL_CA_BUNDLE`. macOS `/usr/bin/curl` rejects a self-signed leaf used as
  its own CA; use Homebrew curl.
- **`kaironctl` build.** Use a `kaironctl` built from the tree under test;
  a stale `bin/kaironctl` lacks `--guest-agent`.

## Related

- Migration failures and `NeedsRecovery` in general (not multi-host
  specific): `docs/runbook-migration-failures.md`.
- Deliberately forcing a live `NeedsRecovery` on this same two-host setup,
  to rehearse operator recovery: `docs/runbook-recovery-drill.md`.
