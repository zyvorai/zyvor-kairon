# User guide: Mac Fabric (experimental)

Mac Fabric turns several Apple silicon Macs into one pool of vz Machines.

- **Capabilities:** each Mac publishes its chip, macOS version, Rosetta, nested virtualization and Thunderbolt Bridge
  addresses on its Node.
- **Scheduling:** a Machine asks for the features it needs in `spec.apple`, and the scheduler places it on a Mac that has
  them.
- **Networks:** a `MacNetwork` gives Machines on different Macs one private Layer-2 network, carried between the Macs by
  FluxVM's encrypted overlay, over Thunderbolt when the Macs are cabled together.

Start with [macos.md](../macos.md) (one Mac as a Node) and [macos-cluster.md](../macos-cluster.md) (why and how to
build a Mac cluster). This page covers what changes with more than one Mac.

![Animated: a Machine asking for an M4 lands on mac-a, a Machine on a MacNetwork lands on mac-b, and the two VMs talk over the Thunderbolt link](../ux/anim/mac-fabric.svg)

## How it fits together

1. **Discovery.** On each Mac, kairon-node reads `machdep.cpu.brand_string`, `hw.model`, the performance and
   efficiency core counts and the macOS version through `sysctl`. It finds the Thunderbolt Bridge with
   `networksetup -listallhardwareports`. It asks FluxVM (`GET /v1/host/apple`) whether Rosetta is installed and
   whether nested virtualization is supported. It refreshes all of this every 10 s, so a cable plugged in or a Rosetta
   install shows up without a restart.
2. **Node labels and annotations.** Labels the scheduler uses:

   | Label | Example |
   |---|---|
   | `kairon.zyvor.dev/apple-chip` | `m4-max` |
   | `kairon.zyvor.dev/apple-chip-generation` | `4` |
   | `kairon.zyvor.dev/macos-version` | `26` (major) |
   | `kairon.zyvor.dev/vz.rosetta` | `true` |
   | `kairon.zyvor.dev/vz.nested-virtualization` | `true` |
   | `kairon.zyvor.dev/thunderbolt` | `true` when the bridge has an IPv4 address |
   | `kairon.zyvor.dev/thunderbolt-fabric` | the `--thunderbolt-fabric` name, only with a Thunderbolt address |

   Annotations for people and tools: `kairon.zyvor.dev/apple-model` (`Mac16,9`), `kairon.zyvor.dev/apple-cores`
   (`12P+4E`), `kairon.zyvor.dev/thunderbolt-interface` (`bridge0`) and `kairon.zyvor.dev/thunderbolt-addresses`.
   A label that stops being true is removed.
3. **Backend isolation.**
   - A Machine with `runtime.backend: vz`, or with `spec.apple`, is placed only on a Mac
     (`kairon.zyvor.dev/backend.vz=true`), and tolerates the Macs' `kairon.zyvor.dev/vm-only` taint without saying so.
   - A Machine that names any other backend is never placed on a Mac.
   - The API rejects `spec.apple` with a backend other than `vz` or `auto`.
4. **Capability scheduling.** `spec.apple` minimums and features are hard filters. When nothing fits, the Machine stays
   `Pending` with the reason per node (for example `spec.apple.minChipGeneration 5, node chip "m4-max"`).
5. **MacNetworks.** kairon-controller gives each `MacNetwork`:
   - a subnet `10.89.N.0/24` (N from 192 to 254, above the range a FluxVM fleet central uses) and UDP port `48000+N`;
   - each member Mac its own slice of host numbers (30 by default), so no two guests on different Macs share an
     address.

   Members keep their slice when other Macs join or leave. A network with `spec.fabric` has only the Macs labelled
   with that fabric, which reach each other over their Thunderbolt addresses; a network without one has every Mac,
   over their InternalIP.
6. **Overlay on each Mac.** With `--mac-fabric-secret-file`, kairon-node writes the networks its Mac belongs to into
   FluxVM's `overlay.json` (`~/.config/fluxvm/overlay.json`, or `FLUXVM_VZ_OVERLAY_CONFIG`). Each entry has the subnet,
   the slice, the listen port and the other members as peers. Each network's key goes in `overlay-keys/<name>.key`,
   mode 0600.
   - **Keys never touch the Kubernetes API.** Each Mac derives them as HMAC-SHA256 of the network's UID and key epoch
     under the fabric secret every Mac holds, so Mac Nodes need no Secret access.
   - FluxVM's switch re-reads the peers and keys every 10 s.
   - Entries and key files Kairon did not write (names not of the form `k<8 hex>-...`) are left alone.
7. **Joining a network.** `spec.apple.networks` lists MacNetworks in the Machine's namespace. The scheduler places the
   Machine only on a member Mac, and kairon-node asks FluxVM for one extra network card per network
   (`apple.networks[].name`). FluxVM's switch gives the guest an address from that Mac's slice.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MacNetwork
metadata:
  name: shard-net
  namespace: ai
spec:
  fabric: studio-rack      # only Macs started with --thunderbolt-fabric=studio-rack, over Thunderbolt
  mtu: 9000                # 1280-9000, default 1400
  hostsPerNode: 30         # default 30
---
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: shard-0
  namespace: ai
spec:
  image:
    path: /Users/you/Library/Application Support/FluxVM/images/debian-13-arm64.raw
  resources:
    cpu: "8"
    memory: 48Gi
  runtime:
    backend: vz
  powerState: Running
  apple:
    minChipGeneration: 3          # M3 or later
    minMacOSVersion: 15
    rosetta: true                 # x86-64 Linux binaries through Rosetta
    nestedVirtualization: true    # KVM inside the guest (M3 and macOS 15 or later)
    networks: [shard-net]
```

[`examples/macos-fleet.yaml`](../../examples/macos-fleet.yaml) has a two-Mac version.

## Mac setup

On every Mac, in addition to [macos.md](../macos.md):

1. For a Thunderbolt fabric, cable the Macs and give the Thunderbolt Bridge an IPv4 address in System Settings
   (link-local `169.254.x.x` works). Start kairon-node with `--thunderbolt-fabric=NAME`
   (`KAIRON_THUNDERBOLT_FABRIC`). Every Mac on the same cables uses the same name.
2. Put the same secret, at least 32 characters, in a file readable only by the user running kairon-node, and pass
   `--mac-fabric-secret-file=PATH` (`KAIRON_MAC_FABRIC_SECRET_FILE`). Without it, kairon-node still publishes
   capabilities but writes no overlay.
3. Apply `deploy/macos/rbac.yaml` again: kairon-node now reads `macnetworks`.
4. Allow UDP ports 48192-48254 between the Macs on the underlay (the Thunderbolt Bridge or the LAN).
5. Do not also run FluxVM's fleet agent with overlay sync on a Kairon Mac. It rewrites `overlay.json` from its central
   and drops Kairon's networks. Run it with `--no-overlay-sync`, or let one of the two own the Mac's networks.

To rotate a network's key, raise `spec.keyEpoch`. Status keeps the previous epoch, and each Mac's key file lists the new
key first and the previous one second, so Macs that have not synced yet still talk.

## Inspecting

```text
kaironctl mac nodes                       # chip, cores, memory, macOS, Rosetta, nested, Thunderbolt, fabric, Machines
kaironctl mac networks -n ai              # subnet, port, MTU, key epoch, members
kaironctl mac network shard-net -n ai     # each member's host slice, address and underlay
kaironctl get macnetworks -n ai           # also kmn
```

The dashboard's Nodes page shows a Platform column (`Mac · M4 Max · macOS 26 · TB studio-rack`, or `Linux amd64`)
and the Mac details in the node inspector.

## Real limits today

- **Not run on Macs.** The lab has no Macs. [`scripts/lab-mac-fabric.sh`](../../scripts/lab-mac-fabric.sh) checks
  scheduling, network allocation and the overlay files on a Linux cluster with simulated Mac Nodes
  ([COMPATIBILITY.md](../COMPATIBILITY.md#mac-fabric)). None of these has run on real Macs yet:
  - booting vz Machines on more than one Mac;
  - Rosetta and KVM inside a guest;
  - traffic over the overlay between Macs;
  - discovery on real hardware.
- **Not RDMA.** The overlay is FluxVM's encrypted UDP mesh. Thunderbolt only makes the underlay fast; it is not
  Thunderbolt RDMA, and sharded inference traffic between Macs does not go through Kairon.
- **Guest addresses.** FluxVM's switch assigns them inside the Mac's slice; Kairon does not reserve a fixed address per
  Machine.
- **Moving between Macs.** A Machine on a MacNetwork is placed once, like any Machine. Live migration between Macs does
  not exist.
- **Capacity.** At most 63 MacNetworks per cluster (one /24 each), and at most 253 / `hostsPerNode` Macs per network.
