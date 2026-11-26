# Runbook: verifying Mac Fabric on several real Macs

Everything Mac Fabric does across Macs has so far run only against simulated
Mac Nodes on a Linux cluster ([`scripts/lab-mac-fabric.sh`](../scripts/lab-mac-fabric.sh)).
Four rows in [`COMPATIBILITY.md`](COMPATIBILITY.md#mac-fabric) need real
Macs and stay `not run` until this runbook is done:

| Row | What proves it |
|---|---|
| Capability and Thunderbolt discovery on real Macs | each Node's chip, chip generation, macOS and Thunderbolt labels match what the Mac reports over SSH |
| vz Machines booting on several Macs | one `vz` Machine pinned to each Mac is Running with a guest IP and answers SSH |
| MacNetwork traffic between Macs over Thunderbolt | guests on different Macs ping each other's MacNetwork address with no loss, and the Thunderbolt Bridge byte counters on the Macs move by at least the ping payload |
| Rosetta and nested virtualization inside a guest | on an M3 or later Mac with macOS 15 or later, the guest has `/dev/kvm` and Rosetta (a binfmt entry or the `rosetta` virtiofs share) |

[`scripts/lab-mac-fabric-hw.sh`](../scripts/lab-mac-fabric-hw.sh) runs all
four and prints the rows; `--write-compat` writes them into
`COMPATIBILITY.md`.

**Out of scope:** live migration between Macs does not exist (the `vz`
backend has no migration; see [Mac Fabric](guides/mac-fabric.md#real-limits-today)),
and neither do Thunderbolt RDMA or macOS guests. Live migration is proved on
the Linux lab: [`runbook-migration-matrix.md`](runbook-migration-matrix.md).

## What you need

- Two or more Apple-silicon Macs. For the nested row, at least one M3 or
  later on macOS 15 or later with Rosetta installed
  (`softwareupdate --install-rosetta --agree-to-license`).
- For the Thunderbolt row, the Macs cabled together with Thunderbolt, and
  the Thunderbolt Bridge given an IPv4 address on each (System Settings >
  Network > Thunderbolt Bridge; link-local `169.254.x.x` is fine). Check with
  `ipconfig getifaddr bridge0` on each Mac.
- A Kubernetes API every Mac can reach, with `deploy/crd.yaml`,
  `deploy/rbac.yaml` and `deploy/macos/rbac.yaml` applied, and kairon-controller
  running somewhere (in the cluster or on a host).
- A workstation (one of the Macs is fine) with `kubectl` for
  that cluster, `ssh`, `ssh-keygen` and `python3`, and key-based SSH to every
  Mac (`ssh -o BatchMode=yes USER@MAC true` must work). The drill uses each
  Mac as the jump host to its own guests, so the Macs' sshd must allow TCP
  forwarding (the macOS default).
- A raw arm64 Linux disk image with cloud-init (the one
  [`scripts/macos-e2e.sh`](../scripts/macos-e2e.sh) uses works) at the
  **same path on every Mac**, under kairon-node's image root (default
  `~/Library/Application Support/FluxVM/images`). The guest needs `ping`
  and `ip`; for the nested row, passwordless `sudo` for the cloud-init user
  if Rosetta is not already registered.

## 1. Set up each Mac

On every Mac, as in [`runbook-macos-node.md`](runbook-macos-node.md), plus
the Mac Fabric settings from [Mac Fabric](guides/mac-fabric.md#mac-setup):

```bash
# 1. FluxVM with the vz backend, listening on 127.0.0.1:7788 (FluxVM's own docs).
# 2. The same fabric secret (32+ characters) on every Mac, readable only by you.
#    Generate it once and copy it to the other Macs over SSH.
install -d -m 700 ~/.config/kairon
openssl rand -hex 32 > ~/.config/kairon/mac-fabric.secret    # first Mac only
chmod 600 ~/.config/kairon/mac-fabric.secret
# 3. kairon-node as a LaunchAgent, with the fabric settings in its env file.
cat >> ~/.config/kairon/kairon-node.env <<'EOT'
NODE_NAME=mac-a                                   # unique per Mac
KAIRON_THUNDERBOLT_FABRIC=lab                     # same on every cabled Mac
KAIRON_MAC_FABRIC_SECRET_FILE=/Users/you/.config/kairon/mac-fabric.secret
EOT
chmod 600 ~/.config/kairon/kairon-node.env
make build && scripts/macos-node.sh install --bin "$PWD/bin/kairon-node"
scripts/macos-node.sh status
```

Allow UDP 48192-48254 between the Macs (if the macOS firewall is on, allow
incoming connections for FluxVM), and do not run FluxVM's fleet agent
with overlay sync on the same Macs (it rewrites `overlay.json`).

From the workstation, check that every Mac is a Ready Node with its labels:

```bash
kubectl get nodes -L kairon.zyvor.dev/apple-chip,kairon.zyvor.dev/macos-version,kairon.zyvor.dev/thunderbolt-fabric
kaironctl mac nodes
```

## 2. Run the drill

```bash
scripts/lab-mac-fabric-hw.sh \
  --mac=mac-a=you@mac-a.local \
  --mac=mac-b=you@mac-b.local \
  --image="/Users/you/Library/Application Support/FluxVM/images/debian-13-arm64.raw" \
  --fabric=lab
```

`--mac` takes the Kubernetes node name and an SSH target for that Mac. Leave
`--fabric` out for Macs that are not cabled: the network row then runs over
the LAN and the Thunderbolt row is recorded `not run`, with the LAN result in
its notes. The drill creates the namespace `macfab-<timestamp>`, a
`MacNetwork` called `fab`, Machines `mfab-0..N` (one per Mac) and
`mfab-nested`, and deletes the namespace on exit (`--keep` leaves it for
debugging). A Machine that does not boot within `--timeout` (600 s) fails its
row with its phase and first non-True condition.

The exit code is 0 when no row failed. Review the printed table, then rerun
with `--write-compat` (or paste the rows) and commit `docs/COMPATIBILITY.md`
with the Macs' chips and macOS versions in the notes. When all four rows
pass, the "Multi-Mac cluster verification" item in [`ROADMAP.md`](../ROADMAP.md)
can be ticked for these four parts; Thunderbolt RDMA and macOS guests stay
open.

## 3. If a row fails

| Symptom | Look at |
|---|---|
| Discovery `MISMATCH` on chip or macOS | kairon-node's log (`~/Library/Logs/kairon-node.log`); labels refresh every 10 s, so a just-restarted node may need a moment |
| Discovery `MISMATCH` on Thunderbolt | `networksetup -listallhardwareports` and `ipconfig getifaddr bridge0` on the Mac; `KAIRON_THUNDERBOLT_FABRIC` must be set and the bridge must have an IPv4 address |
| A Machine stays Pending | `kubectl -n macfab-... describe machine mfab-N`: the scheduler gives the per-node reason (not a member of the MacNetwork, missing vz label, capacity) |
| Running but no guest IP, or SSH fails | the image's cloud-init; `ssh MAC` then `ssh USER@GUEST_IP` by hand; FluxVM's log on that Mac |
| No `10.89.x.y` address in a guest | `kaironctl mac network fab -n macfab-...` for the member slices; `~/.config/fluxvm/overlay.json` on the Mac must list the network; kairon-node needs `KAIRON_MAC_FABRIC_SECRET_FILE` to write it |
| Pings lose packets | UDP port of the network (`status.port`) blocked between the Macs; the fabric secret differs between Macs (keys are derived from it, so a mismatch drops every packet) |
| Pings work but the bridge counters do not move | the members' addresses in `kaironctl mac network fab` are LAN addresses: the network has no `spec.fabric`, or the Macs were not started with the same `KAIRON_THUNDERBOLT_FABRIC` |
| Nested row: `/dev/kvm no` | the Mac is below M3 or macOS 15, or FluxVM did not enable nested virtualization for the VM |
| Nested row: `Rosetta no` | Rosetta not installed on the Mac, or the guest image cannot mount virtiofs (`modprobe virtiofs`) |
