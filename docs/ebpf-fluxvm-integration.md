# eBPF edge and FluxVM integration

This is the control-plane view of the VM-edge contract: what Kairon
already declares and observes, and the next hooks FluxVM (and
PacketWolf) should fill so operators can see *which process* sent or
lost a packet, and how long a flow is taking.

The field-level edge reference remains [`ebpf-edge.md`](ebpf-edge.md).
FluxVM's dataplane, maps and host requirements live in
[FluxVM `docs/vm-edge-contract.md`](https://github.com/zyvorai/zyvor-fluxvm/blob/main/docs/vm-edge-contract.md)
(also linked from older docs as `zyvorai/fluxvm`).

Kairon does **not** own or load BPF programs (`fluxvm_tc.bpf.o`,
XDP/TC/TCX, kprobes). It compiles `Machine` + `MachineNetworkPolicy`
into an `EdgeSpec`, posts it to FluxVM, and surfaces what FluxVM
returns.

## Current capabilities

| Area | What exists today | Where |
| --- | --- | --- |
| Decision engine | Userspace twin of the TC program: anti-spoof, QoS, DNS/SNI/L7, CIDR, default-deny, XDP-eligible flag | `internal/ebpfedge.Decide`, `testdata/vectors.json` |
| Projection | Compiled `EdgeSpec` with a stable FNV-1a identity (survives IP change and live migration) | `internal/agent/edge.go`, `POST /v1/vms/{id}/network/edge` |
| Attributed drops | Policy name, reason, 5-tuple; Prometheus `kairon_net_drops_total{namespace,machine,reason,policy}` | `DropEvent`, `internal/metrics/ebpfedge.go`, `kaironctl network drops` |
| Flows | Bounded `FlowTable`; UI/CLI pass-through of FluxVM `GET …/network/flows` | `internal/ebpfedge/flows.go`, `internal/uiapi/network_observability.go` |
| Latency (migration) | Conntrack export/restore, unlabeled `kairon_net_migration_blackhole_ms` histogram, and last-gap gauge `kairon_net_migration_blackhole_window_ms{namespace,machine}` (status is last window, not a historical max) | `RestoreResult`, `status.network.edge.blackholeWindowMs` |
| Latency (flow RTT) | `kairon_net_flow_rtt_seconds{namespace,machine}` histogram, observed when a flow dump carries latency; `kairon_net_flows_attributed_total{namespace,machine,source}` counts first-seen flows by source (`fluxvm`/`packetwolf`/`guest-agent`/`unknown`/`none`). No PID, comm, or machineset labels | `EdgeRecorder.ObserveFlows`, kairon-node edge tick |
| Capture | Bounded ringbuf/tcpdump tap, max 30s | `kaironctl network capture` |
| Loaders | None in this repo. FluxVM loads the program (`sandbox.dataplane.mode = ebpf` or `cilium`). Kairon talks raw REST, not the Cilium Go SDK | `docs/DEPENDENCIES.md`, `internal/kube/cilium.go` |

`kaironctl network flows|drops|stats` and the dashboard Network panel
now decode those FluxVM payloads into tables. When the dump includes
process or RTT fields (see the contract below), those columns appear
automatically. The raw JSON is still shown in the panel so unknown
keys are not lost. uiapi remains a pass-through: a new FluxVM field
reaches the CLI and dashboard without a Kairon schema change, as long
as it uses one of the accepted aliases.

## Process-level attribution contract

Policy attribution (`reason`, `policyName`) is implemented. Process-level
attribution is an extension FluxVM or PacketWolf *may* attach to a flow
or drop. Kairon never inspects guest memory to fill it.

The Go types are `ebpfedge.ProcessAttr`, `ebpfedge.LatencyAttr` and
`ebpfedge.Attribution` in `internal/ebpfedge/attribution.go`. The
extension point is:

```go
type Attributor interface {
    Name() string
    Lookup(ctx context.Context, key FlowKey) (Attribution, error)
}
```

A miss is a zero `Attribution` and a nil error. `Chain` merges several
sources: non-zero fields already present win, complementary fields
(for example a PacketWolf guest PID plus a FluxVM host vhost thread
and an RTT) are filled in. `AnnotateFlows` / `AnnotateDrops` apply a
lookup onto records that arrived without those fields.

`DecodeFlowRecords`, `DecodeDropEvents` and `DecodeDataplaneStats`
accept both Kairon's camelCase and the snake_case / flattened aliases
a dataplane dump often uses.

### Process fields

| Field | JSON | Aliases | Meaning |
| --- | --- | --- | --- |
| Source | `source` (nested under `process`) or `attrSource` | | `fluxvm`, `packetwolf`, `guest-agent` |
| Scope | `scope` | | `guest` (inside the VM) or `host` (vhost/tap/hypervisor thread) |
| PID / TID | `pid`, `tid` | `process_id`, `thread_id` | |
| Comm / executable | `comm`, `executable` | `comm_name`, `exe`, `binary` | task comm or path |
| Cgroup | `cgroup`, `cgroupID` | `cgroup_path`, `cgroup_id` | path or id |
| Netns / container | `netns`, `container` | `container_id` | |

A host-scoped process on the root of a record is moved to
`hostProcess` so a later guest identity can sit in `process`. A
machine `name` is not treated as a process comm.

Example FluxVM or PacketWolf flow:

```json
{
  "flows": [
    {
      "proto": "tcp",
      "src_ip": "10.0.0.8",
      "src_port": 40000,
      "dst_ip": "1.1.1.1",
      "dst_port": 443,
      "packets": 12,
      "bytes": 8400,
      "process": {
        "source": "packetwolf",
        "scope": "guest",
        "pid": 142,
        "comm": "nginx",
        "cgroup": "/sys/fs/cgroup/machine.slice/web"
      },
      "hostProcess": {
        "source": "fluxvm",
        "scope": "host",
        "pid": 9,
        "comm": "vhost"
      },
      "latency": { "rttUs": 1400, "minRttUs": 800 }
    }
  ]
}
```

Flattened `pid`, `comm`, `scope` and `rtt_us` on the flow itself are
also accepted.

### Latency fields

| Field | JSON | Aliases | Unit |
| --- | --- | --- | --- |
| Smoothed RTT | `rttUs` | `rtt_us`, `srtt_us`, `rttMs` / `rtt_ms` | microseconds (milliseconds when the key says `Ms`) |
| Min / max / variance | `minRttUs`, `maxRttUs`, `rttVarUs` | snake_case forms | microseconds |
| Samples | `samples` | `rtt_samples` | count |

`GET /v1/vms/{id}/network/stats` may put the same keys on the stats
object (`rttMs`, `min_rtt_us`, …). `kaironctl network stats` and the
Network panel Stats tab lift them into an RTT line. Migration
black-hole time is separate and already lives on
`status.network.edge.blackholeWindowMs`.

## Exact next FluxVM / PacketWolf step

Kairon is ready to consume process attribution and flow RTT. The
missing work is on the dataplane: **enrich the existing FluxVM (or
PacketWolf) dumps**. Do not add a new Kairon endpoint, and do not
invent values when the fields are absent.

| | |
| --- | --- |
| **Producer** | FluxVM userspace on the node, filling fields from its TC/TCX maps (or PacketWolf's process correlator). Kairon does not own or load `fluxvm_tc.bpf.o`. |
| **Where to emit** | Prefer `GET /v1/vms/{id}/network/flows` and `GET /v1/vms/{id}/network/drops`. Optionally the same latency keys on `GET /v1/vms/{id}/network/stats`. A new endpoint is not required. |
| **Field contract** | Nested `process` / `hostProcess` (`source`, `scope`, `pid`, `comm`, and optional `executable` / `cgroup` / `container`) and `latency` (`rttUs` or aliases, plus optional min/max/avg/p50/p95/samples). Units are microseconds unless the key ends in `Ms`. `source` is `fluxvm`, `packetwolf`, or `guest-agent`; `scope` is `guest` or `host`. Flattened aliases (`pid`, `comm`, `rtt_us`, `rttMs`, …) are accepted. See the tables above and `internal/ebpfedge/attribution.go`. |
| **Already wired in Kairon** | `internal/fluxvm.Client.GetVMNetworkFlows` / drops, polled each kairon-node edge tick in `internal/agent/edgeevents.go` (`DecodeFlowRecords` → `EdgeRecorder.ObserveFlows`). uiapi stays a raw pass-through, so CLI tables, the Network panel badge, and PROCESS / RTT columns appear as soon as the dump carries the fields. |
| **Optional later hook** | If the dump cannot carry the fields, implement `ebpfedge.Attributor` (typically in `internal/fluxvm`, or a PacketWolf client) and wrap it with `ebpfedge.Chain` next to the decode path in kairon-node. That hook is not wired today because no side-channel lookup API exists. PacketWolf's control plane stays out of this repository ([`network-fabric.md`](network-fabric.md)). |
| **Do not** | Put PID, comm, cgroup, or `machineset` on Prometheus series. RTT is `kairon_net_flow_rtt_seconds{namespace,machine}` (seconds, histogram). Attribution counts are `kairon_net_flows_attributed_total{namespace,machine,source}` with `source` in `fluxvm` / `packetwolf` / `guest-agent` / `unknown` / `none`. Average RTT per MachineSet is the recording rule `kairon:machineset_flow_rtt_seconds:avg`, which joins the histogram to `kairon_machineset_machine_info`. kairon-node does not know MachineSet membership, so a `machineset` label is not added on the histogram. |

## Further integration points

These remain open because they belong to the dataplane, a live two-host
run, or would change CRDs.

1. **Guest-vs-host PID from the guest agent**, when the host only sees
   a vhost thread. Scope must be set; do not overwrite a guest PID
   with a host one.
2. **Agree `vectors.json` with FluxVM's BPF** in CI. The userspace
   engine pins 17 cases; nothing in this repo runs them against
   `fluxvm_tc.bpf.o` yet.
3. **Reachable L7 and XDP policy fields.** `Decide` already models
   `allowL7` and an XDP-eligible drop flag; they are not Machine or
   `MachineNetworkPolicy` fields yet.
4. **Two-host conntrack restore** on a real pair of nodes. Unit tests
   and FluxVM's restore path exist; a two-host run is still missing.
5. **Typed uiapi decoding** is optional. Pass-through is what lets a
   FluxVM field show up immediately; a future schema check can use
   `DecodeFlowRecords` without changing the wire.

## Operator surfaces

| Surface | Behaviour |
| --- | --- |
| `kaironctl network flows MACHINE` | Table with PROTO, SRC, DST, PACKETS, BYTES, and RTT / PROCESS / HOST when present. Falls back to JSON if the payload is not a flow dump. |
| `kaironctl network drops MACHINE` | Table with REASON, POLICY, SRC, DST, PACKETS, plus PROCESS / RTT when present. |
| `kaironctl network stats MACHINE` | Counter summary plus an RTT line (`samples`, min, avg, p50, p95, max) when the stats object carries latency fields or a sample array. Hidden when absent. |
| Dashboard Network panel | Stats cards, and flow / attributed-drop tables with PROCESS, HOST and RTT columns when the dataplane supplies them. A header badge (comm/pid, guest vs host) appears when process data is present. JSON remains below the table. See the [dashboard guide](guides/kairon-ui-dashboard.md#machine-inspector). |
| Dashboard Network overview | Existing attach/policy cards, plus max migration black-hole time from `status.network.edge.blackholeWindowMs`. |

`drops`, `flows` and `stats` still go through kairon-ui: set
`KAIRON_UI_URL` and, if required, `KAIRON_UI_TOKEN`.
