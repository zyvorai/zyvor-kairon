# Runbook: verifying Mac Fabric on several real Macs

Proves the four `not run` rows in [`COMPATIBILITY.md`](COMPATIBILITY.md#mac-fabric)
(discovery, vz boots, MacNetwork over Thunderbolt, Rosetta/nested) with
[`scripts/lab-mac-fabric-hw.sh`](../scripts/lab-mac-fabric-hw.sh). So far
Mac Fabric has only run against simulated Mac Nodes
([`scripts/lab-mac-fabric.sh`](../scripts/lab-mac-fabric.sh)).

## Known limits

- **No live migration between Macs.** The `vz` backend cannot migrate, so this
  runbook cannot prove it. Live migration is proved on the Linux lab instead:
  [`runbook-migration-matrix.md`](runbook-migration-matrix.md).
- Thunderbolt RDMA and macOS guests do not exist, so no row covers them.
- The nested/Rosetta row needs an M3 or later Mac on macOS 15 or later. On
  other Macs it fails, and that failure is the expected result for that Mac.
- Without Thunderbolt cables (no `--fabric`), the network row runs over the LAN
  and the Thunderbolt row stays `not run`.
- **Still never run on real Macs** (as of 2026-11-27). The four rows in
  COMPATIBILITY are `not run`. Every Mac Fabric `pass` there comes from
  simulated Mac Nodes on Linux.
- The overlay is FluxVM's encrypted UDP mesh, not RDMA. Guest addresses come
  from FluxVM inside each Mac's slice. A network holds at most 253 /
  `hostsPerNode` Macs, and a cluster at most 63 MacNetworks
  ([guide limits](guides/mac-fabric.md#real-limits-today)).

## Steps

1. **Prerequisites.** You need two or more Apple-silicon Macs and a cluster
   every Mac can reach. Apply `deploy/crd.yaml`, `deploy/rbac.yaml` and
   `deploy/macos/rbac.yaml`, and run kairon-controller. You also need a
   workstation with `kubectl`, `ssh` and `python3`, and
   `ssh -o BatchMode=yes USER@MAC true` must work for every Mac. Put a raw
   arm64 cloud-init image at the **same path on every Mac** (the
   [`macos-e2e.sh`](../scripts/macos-e2e.sh) one works). For Thunderbolt,
   cable the Macs together and check that `ipconfig getifaddr bridge0`
   prints an IPv4 address on each one.
2. **On each Mac** ([`runbook-macos-node.md`](runbook-macos-node.md),
   [Mac Fabric setup](guides/mac-fabric.md#mac-setup)), start FluxVM with
   the vz backend on `127.0.0.1:7788`, then:

   ```bash
   install -d -m 700 ~/.config/kairon
   openssl rand -hex 32 > ~/.config/kairon/mac-fabric.secret   # first Mac; copy the same file to the others
   chmod 600 ~/.config/kairon/mac-fabric.secret
   cat >> ~/.config/kairon/kairon-node.env <<'EOT'
   NODE_NAME=mac-a
   KAIRON_THUNDERBOLT_FABRIC=lab
   KAIRON_MAC_FABRIC_SECRET_FILE=/Users/you/.config/kairon/mac-fabric.secret
   EOT
   make build && scripts/macos-node.sh install --bin "$PWD/bin/kairon-node"
   ```

   Set `NODE_NAME` to a different value on each Mac. Allow UDP 48192-48254
   between the Macs, and turn off FluxVM fleet overlay sync.
3. **Check the nodes** from the workstation with
   `kubectl get nodes -L kairon.zyvor.dev/apple-chip,kairon.zyvor.dev/thunderbolt-fabric`.
   Every Mac must be Ready.
4. **Run the drill:**

   ```bash
   scripts/lab-mac-fabric-hw.sh --mac=mac-a=you@mac-a.local --mac=mac-b=you@mac-b.local \
     --image="/Users/you/Library/Application Support/FluxVM/images/debian-13-arm64.raw" --fabric=lab
   ```

   It creates `macfab-<timestamp>` and deletes it on exit (`--keep` leaves it
   in place). It exits 0 when no row failed.
5. **Record.** Rerun with `--write-compat` and commit `docs/COMPATIBILITY.md`,
   with the chips and macOS versions in the notes.

## If a row fails

- **Discovery `MISMATCH`:** look at `~/Library/Logs/kairon-node.log`. For
  Thunderbolt, check that `KAIRON_THUNDERBOLT_FABRIC` is set and that `bridge0`
  has an IPv4 address.
- **Machine Pending:** run `kubectl -n macfab-... describe machine mfab-N` to
  see the scheduler's reason.
- **Pings lose everything:** either `status.port` is blocked between the Macs,
  or the fabric secret differs between them.
- **Bridge counters do not move:** the members have LAN addresses. Either
  `--fabric` was not given, or the Macs have different
  `KAIRON_THUNDERBOLT_FABRIC` values.
