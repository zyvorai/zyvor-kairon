# User guide: Machine networking

How to configure `Machine.spec.network` and related status fields. Kairon maps
these into FluxVM create/status APIs; it does not run Multus or own BPF
programs. Boundary details: [network-fabric.md](../network-fabric.md).

## Modes

| `mode` | When to use | Key fields |
|---|---|---|
| `user` (default) | Lab / NAT without a host bridge | `forwards[]` for host→guest ports |
| `tap` | Bridged or isolated edge (Fabric/eBPF path) | `netns`, `bridge`, `tapName`, `mac` |
| `macvtap` | Direct MAC on a parent NIC | `parent`, `macvtapMode`, `mac` |

### User mode forwards

```yaml
spec:
  network:
    mode: user
    forwards:
      - hostPort: 8080
        guestPort: 80
        protocol: tcp   # tcp|udp; default tcp
```

FluxVM receives `host_port` / `guest_port` / `protocol` on the create payload,
bound to `0.0.0.0` so it's reachable off-host (e.g. `ssh -p 8080 <node-ip>`
reaching guest port 22 if forwarded). `kaironctl create --forward=8080:80`
(repeatable) and the dashboard's create form set this without hand-writing a
manifest.

**Forwards are only applied at Machine creation.** Editing `spec.network.forwards`
on an already-running Machine has no effect -- `kairon-node` only reads it
inside the one-time FluxVM create call, never on later reconcile ticks. Delete
and recreate the Machine to apply a change. (Growing `spec.resources` on a
running Machine is different -- it hotplugs; see
[machine-hotplug.md](machine-hotplug.md).)

### TAP + netns (recommended for Network Fabric)

```yaml
spec:
  network:
    mode: tap
    netns: true
    staticNetwork: true   # cloud-init static address (tap+netns only)
    tapName: tap-web      # optional; FluxVM may allocate
    mac: "52:54:00:12:34:56"
    dataplaneRequired: true
    dataplaneMode: cilium   # optional: legacy|ebpf|cilium (empty = FluxVM default)
```

- `netns: true` — per-VM network namespace (isolation + known guest address).
- `staticNetwork: true` — sets FluxVM `cloud_init.static_network` so the guest
  does not depend on DHCP.
- `dataplaneRequired: true` — node agent fail-closes if eBPF/cilium attach is unhealthy.
- `dataplaneMode` — requests FluxVM `network.dataplane_mode` on create. For a
  cluster-wide Cilium dataplane default, also set `/etc/fluxvm.toml`
  `sandbox.dataplane.mode = "cilium"` on each node (Helm `network.ciliumDataplane`
  documents this; it does not rewrite the TOML itself).
- `mac` — optional. A `netns: true` Machine that omits it gets a stable
  generated MAC (`52:54:00:` plus three bytes of an FNV-1a hash of
  namespace and name), which FluxVM requires for netns networking. Must be
  a standard 6-octet Ethernet address (colon- or
  hyphen-separated hex, like the example above). With
  `webhook.enabled` set, a malformed `mac` is rejected immediately on
  `kubectl apply`/edit instead of only failing once `kairon-node` asks
  FluxVM to attach the NIC -- see [SECURITY.md](https://github.com/zyvorai/kairon/blob/main/SECURITY.md#machine-network-mac-address-admission).

### VM edge: anti-spoof, learn-IP, QoS

```yaml
spec:
  network:
    mode: tap
    netns: true
    mac: "52:54:00:12:34:56"
    dataplaneMode: ebpf
    antiSpoof: true
    learnIP: true
    qos:
      ingressMbps: 100   # network -> guest
      egressMbps: 50     # guest -> network
      ingressPps: 20000
      egressPps: 2000
```

- `antiSpoof` — FluxVM drops guest traffic whose source IP (and, on a
  bridged tap, source MAC) is not the Machine's; drops show up as
  `spoof_ip` / `spoof_mac` in `kaironctl network drops`.
- `learnIP` — when Kairon has no guest IP, it uses the address FluxVM
  learned from the guest's ARP or IPv6 neighbor advertisements, or its
  DHCP lease (`status.network.edge.guestIPSource`).
- `qos` — each limit is optional; an explicit `0` is rejected. Egress is
  enforced in the eBPF program, ingress by qdiscs on the host interface.
  A selecting MachineNetworkPolicy's `maxIngress*` / `maxEgress*` fill
  any limit left unset here.

Setting any of these, or `dataplaneMode: ebpf`, makes `kairon-node` post
the VM edge to FluxVM each tick. A `mode: tap` Machine with none of them
also gets an edge when a selecting MachineNetworkPolicy sets `allowSNI`,
`allowDNS` or `maxIngress*`. FluxVM needs its
eBPF dataplane and dataplane schema 12. On a netns Machine, MAC
anti-spoof and ARP learning do not apply; use a bridged tap if you need
them. Full reference: [ebpf-edge.md](../ebpf-edge.md).

### Cilium cluster network (ExternalWorkload)

```yaml
spec:
  network:
    mode: tap
    netns: true
    ciliumAttach: true
    dataplaneMode: cilium
    dataplaneRequired: true
```

Requires Helm `network.ciliumAttach.enabled=true`. The controller creates a
cluster-scoped `CiliumExternalWorkload` named `kairon-<ns>-<name>`, projects
`status.network.cilium`, and sets `spec.network.podUID` from the CEW UID.

```yaml
spec:
  network:
    mode: macvtap
    parent: eth0
    macvtapMode: bridge   # bridge|vepa|private|passthru
```

### Pod identity (Secure Containers)

```yaml
spec:
  network:
    mode: tap
    netns: true
    podUID: "8f3c2e1a-…"   # CRI sandbox UID → FluxVM pod_uid
```

## Cloud-init guest customization

`spec.cloudInit` forwards operator-supplied first-boot customization into
FluxVM's own cloud-init NoCloud seed image -- SSH keys, hostname, a guest
username, packages, and first-boot commands, without needing a custom-baked
image:

```yaml
spec:
  cloudInit:
    hostname: web-01
    user: ops
    sshAuthorizedKeys:
      - "ssh-ed25519 AAAA... ops@laptop"
    packages:
      - curl
    runCmd:
      - curl -fsSL -o /usr/local/bin/envoy https://github.com/envoyproxy/envoy/releases/download/v1.40.0/envoy-1.40.0-linux-x86_64
      - chmod 0755 /usr/local/bin/envoy
      - systemctl enable --now envoy
    writeFiles:
      - path: /etc/systemd/system/envoy.service
        content: |
          [Service]
          ExecStart=/usr/local/bin/envoy -c /etc/envoy/envoy.yaml
          Restart=on-failure
          [Install]
          WantedBy=multi-user.target
        permissions: "0644"
      - path: /etc/envoy/envoy.yaml
        content: |
          static_resources:
            listeners:
            - name: app
              address: {socket_address: {address: 0.0.0.0, port_value: 8080}}
              filter_chains:
              - filters:
                - name: envoy.filters.network.http_connection_manager
                  typed_config:
                    "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                    stat_prefix: app
                    route_config:
                      virtual_hosts:
                      - name: app
                        domains: ["*"]
                        routes:
                        - match: {prefix: "/"}
                          route: {cluster: app}
                    http_filters:
                    - name: envoy.filters.http.router
                      typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router}
            clusters:
            - name: app
              type: STATIC
              load_assignment:
                cluster_name: app
                endpoints:
                - lb_endpoints:
                  - endpoint: {address: {socket_address: {address: 127.0.0.1, port_value: 3000}}}
        permissions: "0644"
```

`writeFiles` drops a file into the guest before first boot via
cloud-init's own `write_files` module -- e.g. a systemd unit or an app
config -- without needing a custom-baked image. `permissions` is an octal
mode string (defaults to cloud-init's own `0644` when unset).

Equivalent flags: `kaironctl create ... --hostname web-01 --user ops --ssh-key "ssh-ed25519 AAAA..." --package curl --runcmd "systemctl enable --now envoy"`
(`--ssh-key`/`--package`/`--runcmd` are repeatable) -- `writeFiles` has no
`kaironctl create` flag yet, only the YAML/kubectl path. The dashboard's
create form exposes a hostname field and a single SSH key field.

Like `forwards` above, `cloudInit` only takes effect when the FluxVM runtime
is first created -- it has no effect on an already-running Machine. It
requires a cloud-init-aware image (most cloud/server distro images are);
FluxVM applies it regardless of image, so a non-cloud-init image simply
ignores the seed data.

## Service Fabric membership

Declare VIP backends after the guest IP is known. The named service must
already exist in FluxVM; Kairon merges this Machine as a backend.

```yaml
spec:
  serviceFabric:
    services:
      - name: web-vip
        port: 8080
        weight: 1
```

Membership is removed again the moment this Machine's guest stops being
reachable: deleting the Machine, or setting `spec.powerState` to `Stopped`
or `Halted`, all deregister its backend entry from every VIP listed here
before the operation completes. This is fail-closed, not best-effort — a
Machine stuck unable to reach FluxVM to deregister stays around (deletion)
or keeps reporting its last real status (stop/halt) rather than silently
finishing while a dead backend, or one a completely different Machine's
DHCP-reused guest IP later inherits, is left registered against the VIP.

The same fail-closed pruning also runs on every reconcile tick while the
Machine keeps running, not just at delete/stop/halt — so two other real
cases are covered too, neither of which needs the Machine to ever stop:
removing an entry from `serviceFabric.services` while the Machine keeps
running, and the guest's IP address itself changing (a DHCP re-lease, or
a reboot landing on a different lease). Before this pruning existed, an
IP change in particular just registered the new address as an *additional*
backend and left the VIP still routing traffic at the old one forever —
Kairon now tracks exactly which `(service, port, guestIP)` triples it has
applied in `status.appliedServiceFabricMemberships` precisely so it can
notice and retract one that's no longer current.

## Status

| Field | Meaning |
|---|---|
| `status.guestIP` | Guest address from FluxVM |
| `status.network.guestIP` / `tapName` | Same, nested for Fabric Dataplane tab |
| `status.network.dataplane.attached` | TC/eBPF hook attached |
| `status.network.dataplane.mode` | `legacy` / `ebpf` / `cilium` |
| `status.network.dataplane.identity` | Dataplane identity |
| `status.network.dataplane.schemaVersion` | BPF schema |
| `status.network.dataplane.policyFingerprint` | Committed policy fingerprint |
| `status.network.dataplane.policySynced` | Maps match durable policy |
| `status.network.edge.identity` | Stable VM-edge identity (survives IP changes and migration) |
| `status.network.edge.antiSpoof` / `policyName` | Edge anti-spoof requested; MachineNetworkPolicy merged into the edge |
| `status.network.edge.guestIPSource` | `agent`, `arp`, `nd` or `dhcp` |
| `status.network.edge.conntrackRestored` / `blackholeWindowMs` | Conntrack entries moved by the last live migration, and the gap between export and restore |
| `status.appliedServiceFabricMemberships` | Ground truth of which `(service, port, guestIP)` backends Kairon has actually registered -- used to prune stale ones, see above |

## Node readiness

`kairon-node`'s readiness always tracks FluxVM `/readyz` (polled every 5s;
FluxVM itself fail-closes when `sandbox.dataplane.required` is enabled), so
the agent stays NotReady until it succeeds. There is no separate node-side
env var for this; per-Machine strictness is `spec.network.dataplaneRequired`.

## Live migration and network state

On live migrate, the source agent quiesces and exports network state; the
target restores after prepare and resumes after commit. Snapshots travel on
the mTLS peer session — not in CRD status. Requires a working migration
adapter for memory transfer; see [migration-adapter.md](../migration-adapter.md).

For Machines with the VM edge, the source also exports FluxVM's live
conntrack table onto the migration session and the destination restores
it before resume, so established connections keep flowing. A snapshot
whose identity does not match the Machine is rejected. See
[ebpf-edge.md](../ebpf-edge.md#live-migration).

## Examples

- Minimal TAP: [`examples/linux-machine.yaml`](../../examples/linux-machine.yaml)
- Full fabric stack: [`examples/network-fabric-machine.yaml`](../../examples/network-fabric-machine.yaml)
- Hands-on: [tutorials/network-fabric.md](../tutorials/network-fabric.md)
