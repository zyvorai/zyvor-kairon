# VM-edge eBPF

Kairon declares a per-Machine network edge: a stable identity,
anti-spoof, guest-IP learning, rate limits, DNS and TLS SNI allow
lists, attributed drops, and conntrack that moves with a live
migration. FluxVM enforces it in the VM's TC/TCX program
(`fluxvm_tc.bpf.o`). Kairon does not own or load BPF programs.

FluxVM's side (routes, wire format, BPF maps, host requirements) is in
[fluxvm docs/vm-edge-contract.md](https://github.com/zyvorai/fluxvm/blob/main/docs/vm-edge-contract.md).

## What Kairon projects

| Feature | Where you set it | FluxVM endpoint |
| --- | --- | --- |
| Stable identity | derived from namespace/name; `status.network.edge.identity` | `POST /v1/vms/{id}/network/edge` |
| Anti-spoof | `Machine.spec.network.antiSpoof` | same |
| Learn IP | `Machine.spec.network.learnIP` | `GET /v1/vms/{id}/network/learned-ip` |
| QoS | `Machine.spec.network.qos`, or `maxIngress*` / `maxEgress*` on the selecting policy | `POST …/network/edge` |
| SNI / DNS allow | `allowSNI`, `allowDNS` (or `allowFqdns`) on the selecting MachineNetworkPolicy | `POST …/network/edge` |
| Attributed drops | `kaironctl network drops` | `GET /v1/vms/{id}/network/drops` |
| Conntrack move | migration session `conntrackSnapshot` | `GET` / `POST /v1/vms/{id}/network/conntrack` |
| Packet capture | `kaironctl network capture [--output FILE]`, `kaironctl network captures` | `POST` / `GET /v1/vms/{id}/network/capture`, `GET …/capture/{token}` (max 30s) |
| Metrics | kairon-node `/metrics` | `kairon_net_drops_total`, `kairon_net_conntrack_restored_total`, `kairon_net_migration_blackhole_ms` |

## Requirements

- FluxVM with dataplane schema 12 and its eBPF dataplane
  (`sandbox.dataplane.mode = "ebpf"` or `"cilium"` in `/etc/fluxvm.toml`).
  In `legacy` mode FluxVM rejects an edge that enforces anything.
- `dataplaneMode` is passed to FluxVM per Machine; an empty value uses the
  host's `[sandbox.dataplane]` setting. Attachment strictness (`required`)
  is always the host's. Current FluxVM treats an omitted table as
  `mode = "ebpf"`, `required = true` (older releases used `legacy` with
  optional attach). On those hosts every `mode: tap` or `mode: macvtap`
  Machine fails to create or start if the BPF objects are missing or BPF
  can't be loaded, even with `dataplaneMode` empty. Before upgrading FluxVM, run its
  `scripts/network-fabric-preflight.sh --require-bpf` on each node, or pin
  `mode = "legacy"` in `/etc/fluxvm.toml` on nodes that can't load BPF.
  `user` and `none` networking have no host edge and are unaffected.
- A `mode: tap` Machine. `user` and `macvtap` networking have no edge
  hook.
- A netns Machine (`netns: true`) without `spec.network.mac` gets a stable
  generated one: `52:54:00:` followed by three bytes of an FNV-1a hash of
  namespace and name. It stays the same across restarts and migrations.
- On AppArmor hosts, FluxVM's current AppArmor profile; an older
  profile stops every VM from attaching to the eBPF dataplane.

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata:
  name: web
  namespace: default
  labels:
    app: web
spec:
  # image, resources, ...
  network:
    mode: tap
    netns: true
    mac: "52:54:00:ed:9e:01"
    dataplaneMode: ebpf
    dataplaneRequired: true
    antiSpoof: true
    learnIP: true
    qos:
      ingressMbps: 100
      egressMbps: 50
      egressPps: 2000
---
apiVersion: kairon.zyvor.dev/v1
kind: MachineNetworkPolicy
metadata:
  name: web-egress
  namespace: default
spec:
  selector:
    app: web
  policy:
    defaultAllow: true
    allowSNI: ["*.example.com", "example.com"]
    allowDNS: ["*.example.com", "example.com"]
```

With this, the guest can resolve and open TLS connections to
`example.com` and its subdomains only. A forged source address drops as
`spoof_ip`, and guest egress above 50 Mbit/s or 2000 packets/s drops as
`rate_limit`.

## Machine fields

| Field | Effect |
| --- | --- |
| `antiSpoof` | Drop guest frames whose source IP (and, on a bridged tap, MAC) is not the Machine's. |
| `learnIP` | Fill the guest IP from what the guest announces (ARP, IPv6 ND) when Kairon has no address for it. |
| `qos.egressMbps`, `qos.egressPps` | Guest-to-network limits, enforced in the TC program. |
| `qos.ingressMbps`, `qos.ingressPps` | Network-to-guest limits, enforced by qdiscs on the host interface. |
| `mac` | The MAC anti-spoof expects. Generated for a netns Machine that omits it. |
| `dataplaneMode: ebpf` | Requests the eBPF dataplane and turns the edge on even with no other edge field. |
| `dataplaneRequired` | Fail the Machine if the edge post fails, instead of logging a warning. |

QoS fields are pointers: absent means no limit, and an explicit `0` is
rejected.

The edge is posted when the Machine sets `antiSpoof`, `learnIP`, `qos`,
or `dataplaneMode: ebpf`, or when it is a `mode: tap` Machine selected by
a MachineNetworkPolicy that sets `allowSNI`, `allowDNS`,
`maxIngressMbps` or `maxIngressPps`. So a policy's allow lists also apply
to a Machine with no edge fields, including `dataplaneMode: cilium`. A
policy-only edge has `dataplaneRequired` off: if FluxVM rejects it, the
node logs a warning and the Machine keeps running. The policy list comes
from the same per-tick fetch the network reconcile already makes.

## Policy fields

The first MachineNetworkPolicy in the namespace that selects the
Machine (by `machineName` or `selector`) is merged into the edge:

| Policy field | Edge field |
| --- | --- |
| `metadata.name` | `policyName` (shown on drops) |
| `allowSNI` | `allowSNI` |
| `allowDNS`, else `allowFqdns` | `allowDNS` |
| `maxIngressMbps`, `maxEgressMbps`, `maxIngressPps`, `maxEgressPps` | `qos`, only where the Machine did not set that limit |
| `defaultAllow`, `allowCidrs`, `denyCidrs`, `allowPorts`, `allowIcmp` | copied, but enforced through the VM network policy, not the edge |

### Name matching

| Entry | Matches |
| --- | --- |
| `example.com` | exactly `example.com` |
| `*.example.com` | `a.example.com`, `a.b.example.com`; not `example.com` |

Matching is case-insensitive and ignores a trailing dot. Names can be up
to 128 bytes. Kairon rejects entries with spaces, `/`, `:`, `\`, an
empty label, or a `*` anywhere except a leading `*.`.

What is checked:

- DNS: the query name of each query sent to port 53, over UDP or TCP.
- SNI: the `server_name` of a TLS ClientHello sent to TCP 443. A
  ClientHello without SNI, or split across TCP segments, is denied while
  an SNI list is set.

What is not: DNS over HTTPS or TLS, QUIC (UDP 443), TLS on other
ports, Encrypted Client Hello, and HTTP bodies. To close those paths,
restrict ports and resolvers with the same policy's `allowPorts` and
`allowCidrs`.

When the policy is in `auditMode`, DNS and SNI denials are recorded as
`audit` and the traffic is let through. Anti-spoof and rate limits
always drop.

## Identity

Identity is FNV-1a over `namespace`, a zero byte, and the name, masked
to 24 bits and never below 256 (`kaironctl network identity MACHINE`
prints it). It does not change when the guest IP changes, so a live
migration or a DHCP re-lease does not flap `MachineNetworkPolicy`.
FluxVM checks it on every conntrack restore.

## Reconcile

On every tick, for each Machine that requests the edge, `kairon-node`:

1. builds the edge from the Machine spec and its guest IP;
2. merges the selecting MachineNetworkPolicy;
3. validates names and CIDRs and stamps the identity;
4. if `learnIP` is set and there is no guest IP yet, asks FluxVM for the
   learned address and uses it;
5. posts the edge to FluxVM;
6. writes `status.network.edge`.

FluxVM applies the edge immediately when the VM is attached, persists
it, and reapplies it on every later attach (VM restart, FluxVM restart,
dataplane repair). A failed post is a warning unless `dataplaneRequired`
is set, in which case the Machine fails closed. A FluxVM that does not
have the endpoint returns 404 and is treated the same way.

Anti-spoof uses the guest IP Kairon knows. If the guest changes address,
its traffic drops as `spoof_ip` until the next tick posts the new one.

## Status

```yaml
status:
  network:
    guestIP: 169.254.0.19
    dataplane:
      attached: true
      interface: vhe94c7e6f
      mode: cilium
      schemaVersion: 12
      policySynced: true
    edge:
      identity: 1606429
      antiSpoof: true
      policyName: web-egress
      guestIPSource: agent
```

| Field | Meaning |
| --- | --- |
| `edge.identity` | The stable identity above. |
| `edge.antiSpoof` | Anti-spoof is requested. |
| `edge.policyName` | The MachineNetworkPolicy merged into the edge. |
| `edge.guestIPSource` | Where the guest IP came from: `agent` (Kairon already knew it, from FluxVM's VM record or the guest agent), `arp` or `nd` (learned by the datapath), `dhcp` (FluxVM's DHCP lease). |
| `edge.conntrackRestored` | Conntrack entries restored on this host by the last live migration. |
| `edge.blackholeWindowMs` | Milliseconds between conntrack export on the source and restore here. |
| `dataplane.attached`, `schemaVersion` | From FluxVM `network/status`. Schema 12 is needed for the edge. |

`dataplane.attached` can read `false` for one tick right after the
Machine starts; it follows FluxVM on the next tick.

## Drops

`kaironctl network drops MACHINE` lists drops recorded by the datapath:

```json
{"items": [
  {"namespace": "default", "machine": "web", "reason": "dns_deny",
   "policyName": "web-egress", "srcIP": "169.254.0.19", "dstIP": "169.254.0.18",
   "proto": "udp", "dstPort": 53, "direction": "egress", "action": "drop",
   "packets": 4}
]}
```

| `reason` | Cause |
| --- | --- |
| `spoof_mac` | Source or ARP sender MAC is not the Machine's (bridged tap only). |
| `spoof_ip` | Source IP is not the Machine's. |
| `dns_deny` | DNS query for a name not on `allowDNS`. |
| `sni_deny` | TLS SNI not on `allowSNI`. |
| `rate_limit` | Over a QoS limit. `direction: ingress` comes from the host qdiscs and has no addresses. |
| `policy_deny` | A CIDR, port, Pod-policy or UDP rule of the VM network policy. |
| `default_deny` | No allow rule matched and `defaultAllow` is false. |
| `malformed` | Unparsable, fragmented, or non-IP traffic under policy. |
| `migration` | New flow rejected while the VM was quiescing or restoring. |

There is one entry per reason and flow, with its packet count, sorted by
packets. `--limit` caps the list (default 10).

## Live migration

1. When a live migration starts, the source exports FluxVM's live
   conntrack table, stamps the stable identity and an export time, and
   puts the snapshot on the migration session (`conntrackSnapshot`). A
   failed export is logged and the migration continues without it.
2. Before resume, the destination checks that the snapshot identity is
   the Machine's identity. A mismatch fails the restore.
3. The destination posts the snapshot to FluxVM, which writes the flows
   into the new VM's table with a fresh idle timeout (or keeps them until
   the VM attaches).
4. The result is recorded and projected as `edge.conntrackRestored` and
   `edge.blackholeWindowMs` on the next tick.

Established TCP and UDP flows keep passing on the destination without
waiting for new connections to be allowed again.

## Capture

```bash
export KAIRON_UI_URL=https://kairon-ui:18082 KAIRON_UI_TOKEN=...
export KAIRON_UI_CA=/path/to/dashboard-cert.pem   # trust a self-signed dashboard certificate
kaironctl network capture web --seconds 10 --filter "udp port 53" --output dns.pcap
kaironctl network captures web
tcpdump -nr dns.pcap
```

`capture` builds a session (1-30 seconds, random token) and posts it
through kairon-ui and the node to FluxVM, which runs `tcpdump` on the
Machine's dataplane interface inside its network namespace. With
`--output`, kaironctl waits `seconds`, then polls the download (FluxVM
answers 409 while the capture runs) and writes the pcap. Without
`KAIRON_UI_URL` the session is only printed.

FluxVM limits: one capture per Machine at a time, 20,000 packets,
1,600-byte frames, a filter of at most 512 bytes, and the newest 16
captures kept. A bad filter fails the request with tcpdump's message.
`captures` lists each session's `state` (`running`, `done`, `failed`,
`interrupted`) and packet count. The node host needs `tcpdump`.

| kairon-ui route | Node relay | FluxVM |
| --- | --- | --- |
| `POST /api/v1/machines/{ns}/{name}/network-capture` | `POST /network-capture/{runtimeID}` | `POST /v1/vms/{id}/network/capture` |
| `GET /api/v1/machines/{ns}/{name}/network-capture` | `GET /network-capture/{runtimeID}` | `GET /v1/vms/{id}/network/capture` |
| `GET /api/v1/machines/{ns}/{name}/network-capture/{token}` | `GET /network-capture/{runtimeID}/{token}` | `GET /v1/vms/{id}/network/capture/{token}` |

All three need diagnostics enabled (`KAIRON_NODE_CONSOLE_TOKEN` set on
kairon-node and kairon-ui, matching `KAIRON_NODE_CONSOLE_PORT` on the UI
if the node uses `KAIRON_NODE_CONSOLE_ADDR`); otherwise kairon-ui
returns 501. 404 and 409 from FluxVM pass through unchanged.

## Metrics

kairon-node registers these on its health address (`--health-addr`,
`/metrics`):

| Metric | Type | Labels | Source |
| --- | --- | --- | --- |
| `kairon_net_drops_total` | counter | `namespace`, `machine`, `reason`, `policy` | FluxVM's attributed drops, read after each edge apply (up to 256 flows). |
| `kairon_net_conntrack_restored_total` | counter | — | Entries restored on a migration destination. |
| `kairon_net_migration_blackhole_ms` | histogram | — | Milliseconds between conntrack export and restore. |

Drop counters are per flow in FluxVM; kairon-node adds the increase
since the last read. A counter that goes down (VM restart or re-attach)
is counted from zero, and a flow that leaves the top 256 is forgotten.
`policy` is `-` for drops not tied to a policy (anti-spoof, rate
limits). `kairon_net_drops_total` has no series until the first drop.

```promql
sum by (namespace, machine, reason) (rate(kairon_net_drops_total[5m]))
```

## CLI

| Command | What it shows |
| --- | --- |
| `kaironctl network status MACHINE` | Dataplane attach, schema, Cilium and policy sync. |
| `kaironctl network identity MACHINE` | The stable identity. Works offline. |
| `kaironctl network drops MACHINE [--limit N]` | Attributed drops. |
| `kaironctl network drop-reasons MACHINE` | Raw kernel drop reasons. |
| `kaironctl network flows MACHINE` | Recent flows. |
| `kaironctl network capture MACHINE [--output FILE]` | Starts a capture; with `--output`, waits and writes the pcap. |
| `kaironctl network captures MACHINE` | Capture sessions and their state. |

`drops`, `drop-reasons`, `flows`, `capture` and `captures` go through kairon-ui:
set `KAIRON_UI_URL` and, if required, `KAIRON_UI_TOKEN`.

## Netns Machines

A netns Machine sits behind a router in its network namespace, and
FluxVM hooks the host side of that router. There:

- IP anti-spoof, the DNS and SNI allow lists, and QoS apply;
- MAC anti-spoof and ARP learning do not (every frame carries the
  router's MAC), so `guestIPSource` is usually `agent` or `dhcp`;
- the guest may log IPv6 duplicate-address warnings, because the
  namespace bridge and the guest share the MAC. IPv4 is unaffected.

Use a bridged tap (`netns: false`, `bridge: …`) when MAC anti-spoof or
ARP learning matters.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| No `status.network.edge` | The Machine sets none of `antiSpoof`, `learnIP`, `qos`, `dataplaneMode: ebpf`, and no selecting policy sets `allowSNI`, `allowDNS` or `maxIngress*` (or the Machine is not `mode: tap`). |
| `edge apply failed; continuing` in kairon-node logs | FluxVM rejected the edge: legacy mode, BPF objects older than schema 12, or an invalid MAC or name. The FluxVM error is in the log line. |
| Machine fails with `netns networking requires an explicit MAC address` | kairon-node is older than the MAC default; upgrade it or set `spec.network.mac`. |
| `kaironctl network capture` returns HTTP 501 | Diagnostics are off: set `KAIRON_NODE_CONSOLE_TOKEN` on kairon-node and kairon-ui. |
| Capture returns 400 `a capture is already running` | One capture per Machine; wait for it. |
| Capture returns 400 `spawn tcpdump` | Install `tcpdump` on the node, and FluxVM's current AppArmor profile. |
| `dataplane.attached: false` on every Machine of a node | FluxVM cannot load BPF; on AppArmor hosts, install FluxVM's current profile. |
| Allowed name is denied | List the apex as well as `*.apex`; check the client does not use DoH or QUIC. |
| Traffic drops as `spoof_ip` after an IP change | Wait one tick, or check `status.network.guestIP`. |
| `kaironctl network drops` says to set `KAIRON_UI_URL` | Point it at kairon-ui. |

To look at the datapath directly on the node, see "Inspecting a VM" in
FluxVM's `docs/vm-edge-contract.md`.

## Limits

- Drop metrics are sampled from FluxVM's top 256 flows on each tick,
  so a very wide spread of dropped flows is under-counted.
- Verified on a single host. The conntrack move across two hosts is
  covered by unit tests and FluxVM's restore path, not yet by a two-host
  run.

## Non-goals

- A Hubble UI inside kairon-ui (that is Paqtra).
- Kairon-owned `.bpf.c`.
- HTTP body inspection. SNI and DNS query names only.

## Installing the CRDs

`deploy/crd.yaml` is generated from `charts/kairon/crds/` by `make crds`,
and `make validate` fails if the two differ. Either can be applied.
