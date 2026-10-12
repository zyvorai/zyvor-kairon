# User guide: virtual networks (`VirtualNetwork`)

A `VirtualNetwork` is an isolated IPv4 network for Machines. It has one VNI
(virtual network identifier), one CIDR, a gateway that exists on every node,
optional egress SNAT and per-Machine floating IPs. FluxVM routes it in eBPF
on each host and carries traffic between hosts in Geneve. No Linux bridge,
no per-network kernel device and no iptables rules are involved.

Experimental: see [Real limits today](#real-limits-today) and the lab rows
in [`COMPATIBILITY.md`](../COMPATIBILITY.md#virtual-networks).

This is separate from the fleet `MachineVirtualNetwork`
(`fleet.kairon.zyvor.dev`), which maps a name onto an existing host bridge
and VLAN. That path is unchanged.

## How it fits together

1. **kairon-controller** is the IPAM. For every `VirtualNetwork` it picks a
   VNI, gives each Machine that references it an address and a MAC, and
   records the node the Machine runs on. All of this lives in
   `status.allocations`.
2. **kairon-node** reads the allocation when it creates a Machine and asks
   FluxVM for a tap NIC on that network. Every 10s it pushes each network
   that has a Machine on its node to FluxVM (`PUT
   /v1/network/vnets/{vni}`), with every endpoint in the cluster, and
   removes networks that no longer have one.
3. **FluxVM** loads one eBPF object (`fluxvm_vnet.bpf.o`) per host. It
   attaches a program to each vnet tap, to one Geneve device in
   collect-metadata mode (`fvgnv0`, UDP 6081) and to the uplink. It
   configures the guest's address, gateway and MTU 1450 through cloud-init.
4. **kairon-node's BGP speaker** (opt-in) announces the floating IPs of
   local Machines, and optionally the Service Fabric VIPs, to your routers.

What the datapath does, per packet:

- **Same host:** the tap program answers ARP for every address on the
  network, then routes guest to guest directly (`bpf_redirect`) with the
  gateway MAC as source.
- **Other host:** it sets a Geneve tunnel key (VNI and the remote node IP)
  and redirects to `fvgnv0`. The receiving host's tunnel program delivers
  only to Machines of that VNI on that host.
- **Isolation:** lookups are keyed by VNI, so two networks may use the same
  CIDR. A guest can only send from its allocated address (anti-spoofing).
- **Egress:** with `egressSNAT: true`, traffic leaving the CIDR is
  translated to the node's address on the uplink (ports 61000-65534, above
  Linux's ephemeral range) and the reply is translated back. Without it,
  the network is closed.
- **Floating IPs:** a 1:1 NAT between a public address and the Machine,
  both directions, on the Machine's node.
- The gateway answers ping.

## Creating a network

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: VirtualNetwork
metadata:
  name: app
  namespace: prod
spec:
  cidr: 10.50.0.0/24      # /8 to /29
  # gateway: 10.50.0.1    # default: first host address
  # vni: 100              # default: allocated, 4096 and up
  egressSNAT: true
```

```console
$ kaironctl get vnets -n prod
NAME  CIDR          VNI   GATEWAY    SNAT  MACHINES  PHASE
app   10.50.0.0/24  4096  10.50.0.1  true  2         Ready
```

Rules:

- `cidr` must be IPv4 with no host bits set. `gateway` must be a host
  address inside it.
- VNIs are cluster-wide. A `spec.vni` already used by an older network
  fails the newer one (`phase: Failed`). `spec.vni` cannot change once
  allocated (CEL and controller).

## Attaching a Machine

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web-1
  namespace: prod
spec:
  network:
    virtualNetwork:
      name: app                 # same namespace
      # ip: 10.50.0.20          # default: lowest free address
      # floatingIP: 203.0.113.10
  image:
    path: /var/lib/fluxvm/images/ubuntu-24.04.qcow2
```

Rules (webhook):

- The NIC is a tap with no bridge: leave `mode` at its default or set
  `tap` (`macvtap` is refused). It cannot be combined with `netns`, `bridge`, `extraInterfaces`,
  `ciliumAttach` or `dataplaneMode: legacy`.
- The MAC defaults to the Machine's stable `52:54:00:…` address.
- kairon-node waits for the controller to allocate an address before it
  creates the runtime. `status.network.virtualNetwork` records what the
  runtime was created with.
- The guest image must apply cloud-init network config (FluxVM's static
  `network-config`).

An address is held for the Machine's lifetime and released when the
Machine is deleted or stops referencing the network. A requested `ip`
that is the gateway, outside the CIDR or held by another Machine is
refused. So is a `floatingIP` already used anywhere in the cluster. The
reason is in the network's `status.message`, and the Machine stays
uncreated until it is fixed.

`kaironctl describe vnet app -n prod` prints the allocation table.

## Node setup

kairon-node flags (Helm: `node.virtualNetworks.*`):

| Flag | Default | Meaning |
|---|---|---|
| `--virtual-networks` | `true` | Push networks to the local FluxVM. Needs FluxVM's `host.vnet` capability; without it the node stays quiet until a Machine needs a network, then reports a sync error. |
| `--vnet-uplink` | empty | Host interface for SNAT and floating IPs. Empty keeps every network closed on this node. |
| `--vnet-node-ip` | Node InternalIP | Underlay address other nodes tunnel to, and the SNAT address. |

Nodes must reach each other on UDP 6081. The uplink program is attached
first in the TCX chain (ahead of Cilium's `cil_from_netdev`) and passes
everything that is not a vnet reply or floating IP untouched.

## BGP

kairon-node can announce prefixes to upstream routers. The speaker is
announce-only: it sends IPv4 unicast routes and ignores whatever the peer
sends.

| Flag | Meaning |
|---|---|
| `--bgp-peers` | `addr=asn[,addr:port=asn]`. Empty (the default) disables the speaker. |
| `--bgp-local-asn` | This node's AS (2- or 4-byte). |
| `--bgp-router-id` | Default: `--vnet-node-ip`, else the Node InternalIP. |
| `--bgp-next-hop` | Default: the router ID. |
| `--bgp-advertise-service-vips` | Also announce FluxVM's Service Fabric VIPs marked for advertisement. |
| `--bgp-advertise-prefixes` | Extra IPv4 prefixes to announce. |

Each node announces the floating IPs of its own Machines as /32s, with
itself as next hop. When a Machine moves, the old node withdraws and the
new one announces. Without BGP, route floating IPs to the right node
statically. FluxVM does not proxy-ARP for them.

## Observability

kairon-node metrics: `kairon_node_virtual_networks`,
`kairon_node_virtual_network_endpoints{location=local|remote}`,
`kairon_node_virtual_network_sync_errors_total`,
`kairon_node_bgp_session_up{peer}` and
`kairon_node_bgp_advertised_prefixes{peer}`. FluxVM reports applied
networks at `GET /v1/network/vnets`.

## Real limits today

- IPv4 only. No IPv6 guests, underlay or BGP routes.
- No load balancer inside a virtual network. Service Fabric VIPs can be
  announced over BGP, but Service Fabric does not balance onto vnet
  Machines.
- vnet Machines get no edge policy: `MachineNetworkPolicy`,
  `NetworkSecurityGroup` and VM-edge identity do not apply to them.
  Isolation is per network.
- The host cannot reach vnet guests directly; use a floating IP or another
  Machine on the network.
- SNAT tracks flows in an LRU table with no TCP state machine: a closed
  connection's mapping lingers until the table evicts it, and long-idle
  flows can be evicted when it fills. ICMP echo uses the echo id as the port.
- Routing does not decrement TTL, and IP options are dropped.
- Guest MTU is 1450 to leave room for Geneve on a 1500-byte underlay.
- NIC hotplug (`extraInterfaces`) is refused on vnet Machines.
- The BGP speaker is announce-only and stdlib-only (not GoBGP). There is
  no route reflection, no graceful restart, no authentication and no
  received-route handling.
- Cross-host forwarding has only run between network-namespace-simulated
  hosts, not between real nodes. See the lab rows.
