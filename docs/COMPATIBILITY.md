# Compatibility matrix

Short, dated evidence for Kairon’s differentiating features on real
hardware. Historical narrative belongs in runbooks; this file is the
pass/fail surface operators and releases should cite.

**Do not claim production live migration in README until the live row
below is green from an automated nightly run.**

## Lab profile (Zyvor)

| Component | Expectation |
|---|---|
| Nodes | ≥3 KVM hosts with FluxVM |
| Storage | Shared filesystem or Ceph RBD at identical paths |
| Network | L2/L3 reachability for guest + migration data plane |
| CNI | Cilium (when network-policy / identity tests run) |
| Runner | Self-hosted GitHub Actions runner with lab access |

Automation entry points:

- Runbook: [`docs/runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md)
- Matrix driver: [`scripts/hardware-migration-matrix.sh`](../scripts/hardware-migration-matrix.sh)
- Workflow: [`.github/workflows/hardware-migration.yml`](../.github/workflows/hardware-migration.yml) (`workflow_dispatch` + nightly)
- Failure drills the matrix runs (each also works standalone):
  [`scripts/recovery-drill.sh`](../scripts/recovery-drill.sh) (firewalls the
  destination's control port mid-migration, then runs `kaironctl recover`),
  [`scripts/lab-inject-source-failure.sh`](../scripts/lab-inject-source-failure.sh)
  (stops `kairon-node` on the source during transfer),
  [`scripts/lab-inject-controller-failover.sh`](../scripts/lab-inject-controller-failover.sh)
  (restarts the controller mid-migration), and
  [`scripts/lab-inject-postcopy-source-failure.sh`](../scripts/lab-inject-postcopy-source-failure.sh)
  (kills the source QEMU after the post-copy switch; destroys the guest)

**Lab enablement (current blocker for the multi-host matrix):** the
`matrix` job runs only on `runs-on: [self-hosted, kairon-lab]`. Bring that
runner online and set repository secrets/vars `KAIRON_HW_LAB=1`,
`KAIRON_KUBE_URL`, `KAIRON_KUBE_TOKEN`, `KAIRON_KUBE_CA` (PEM content;
the workflow writes it to a file), `KAIRON_HW_MACHINE`,
`KAIRON_HW_TARGET_NODE`, plus optional `KAIRON_HW_EBPF_MACHINE` (live-eBPF
case), `KAIRON_HW_IMAGE` (real create/stop/start/delete smoke),
`KAIRON_HW_SOURCE_SSH` / `KAIRON_HW_TARGET_SSH` (failure drills; the runner
needs passwordless SSH and sudo for `iptables` / `systemctl`) and
`KAIRON_HW_CONTROLLER_SSH` (systemd controller instead of Pods), and
`KAIRON_UI_URL` (var) / `KAIRON_UI_TOKEN` (secret) for the guest-agent case
and for the live case, which starts a memory-dirtying load in the guest
over the guest agent (`KAIRON_HW_LOAD_MIB`, default 256) and records
`not run` without them. The adaptive cases take `KAIRON_HW_ADAPTIVE_LOAD_MIB`
(default 1024) and `KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS` (default 200), and the
post-copy source-failure drill needs `KAIRON_HW_POSTCOPY_MACHINE`, a
disposable Machine the lab recreates before each run (sudo `pkill` on the
source host). Until then,
push-triggered runs succeed on the `validate` job only and leave every
**migration** row below as `not run`.

## Single-host deploy smoke (manual)

Bare-metal systemd path (`scripts/deploy-remote.sh`), not a substitute for
the multi-host matrix above.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Deploy node+controller+ui (`v0.6.0-24-g0020758`) | pass | 2026-09-22 | Host `80.79.5.173` / `nldw4-4-16-36` (k3s). Health: node `:8081`, controller `:8083` (host `:8080` occupied by `krytond`), ui `:8082` |
| CRD server-side apply | pass | 2026-09-22 | `deploy/crd.yaml` |
| Machine create / Running | pass | 2026-09-22 | `v07-smoke` then deleted |
| Machine stop → start | pass | 2026-09-22 | `deploy-smoke` |
| UI overview + machines API | pass | 2026-09-22 | host `80.79.5.173` port `8082` (recorded before the dashboard served HTTPS by default) |
| Network `effective` (console relay) | pass | 2026-09-22 | Needs matching `KAIRON_NODE_CONSOLE_TOKEN` in both env files; redeploy leaves existing env untouched |
| Network flows/stats/drops | n/a | 2026-09-22 | `deploy-smoke` uses `network.mode: user` — FluxVM eBPF flow/drop/stats need tap+`dataplaneMode: ebpf` |
| VM edge, Kairon-scheduled Machine | pass | 2026-10-04 | Host `175.110.122.71` (k3s, FluxVM `cilium` mode, schema 12). Machine `edge-demo` (tap+netns, `dataplaneMode: ebpf`, `antiSpoof`, `learnIP`, `qos`) Running; FluxVM loaded flags, egress 2000 pps, 3 allow-list names and a 100 Mbit `tbf` from Kairon's edge post; `status.network.edge` projected |
| VM edge enforcement (FluxVM VMs on the same host) | pass | 2026-10-04 | DNS and SNI allow/deny, `spoof_ip` and `spoof_mac` (bridged tap), egress pps `rate_limit`, ingress police drops, learned IP via ARP, live conntrack export (14 entries), and edge + pending conntrack reapplied after FluxVM restart and VM stop/start |
| VM edge packet capture via kairon-ui | pass | 2026-10-04 | `kaironctl network capture edge-demo --filter icmp -o icmp.pcap` through kairon-ui, the node relay (`KAIRON_NODE_CONSOLE_ADDR=:8092`) and FluxVM: pcap with ICMP echo request and reply; `captures` lists `done` with packet counts; unknown token 404 |
| VM edge metrics | pass | 2026-10-04 | kairon-node `/metrics` exports `kairon_net_conntrack_restored_total` and `kairon_net_migration_blackhole_ms`; `kairon_net_drops_total` appears on the first drop (unit-tested) |
| Native macOS node, `vz` Machine (`7867692`) | pass | 2026-10-08 | Apple M4, macOS 27.2: Node Ready, Machine scheduled, Debian 13 booted by FluxVM, guest IP in status, SSH, delete (`scripts/macos-e2e.sh`). Not run: macOS guests, migration, CSI, multi-Mac |
| Policy-only edge, netns MAC default | unit tests | 2026-10-04 | Not yet run against a live Machine |
| Redeploy node+controller keeps ports (`v0.6.0-116-g1b928b9`) | pass | 2026-10-06 | Host `80.79.5.173`. Health: node `:32302`, controller `:32301`, ui `:8082`, console `:8090`; nothing on `:8080` (`krytond`). Needed `kubectl apply -f deploy/rbac.yaml` first: the cluster's ClusterRoles predated `machinebackups` |
| Deploy node+controller+ui with `--apply-crds --apply-rbac` (`v0.7.2-17-g81911e3`) | pass | 2026-10-11 | Host `80.79.5.173` / `nldw4-4-16-36` (k3s). CRDs now `v1` (storage) + `v1beta1`/`v1alpha1` served, deprecated (the `v1alpha1` read prints the deprecation warning). RBAC regenerated from the chart; `kairon-ui` has the Pools/Claims/Backups grants. UI auto-picked `:27231` because `:18082` is held by `zyvor-fabric-agent` (pinned with `--ui-port=27231`) |
| Dashboard API sweep on `v1` | pass | 2026-10-11 | 26 list/diagnostic routes 200 (quotas, budgets, sets, instance types, policies, pools, claims, backups, restores, fleet, usage.csv, node capabilities/catalog/pools/templates/sandboxes); wrong password and unauthenticated calls 401; Images and `/atlas/*` 501 (unconfigured, by design) |
| Machine create + delete through the UI API on `v1` | pass | 2026-10-11 | Created `v1-smoke` (apiVersion `kairon.zyvor.dev/v1`, Pending: node lacks the capability label), delete returned 204 and the object was gone |
| Tenant fence | pass | 2026-10-06 | Two fenced tenants in one namespace: labels projected, `tenant-fence-<tenant>` groups `Applied` by kairon-node, each denying only the other tenant's IPs (`/32`, `/128`); opt-out deleted the group. Guest IPs were patched into status (no booted guests). Admission checks not run: no webhook on this host |
| Preemption by Halt | pass | 2026-10-06 | Pending priority-10 Machine halted an opted-in priority-0 Machine (annotations recorded); deleting the preemptor resumed it; no `preemption-policy` meant no halt. Victim was pinned with `spec.nodeName`; node lacks `kairon.zyvor.dev/capable` so the preemptor stayed unschedulable |
| Hotplug persist across stop/start | pass | 2026-10-06 | Real qemu guest (`node22-agent.qcow2`) 1 vCPU/1Gi, max 2/2Gi: hotplugged to 2/2048, spec lowered to 1/1Gi, Stopped → Running booted `-smp cpus=2` `-m 2048M`; `hotplug-persist=true` raised spec to 2/2048Mi. Found and fixed the `maxmem == memory` boot failure. Halted → Running is the next row |
| Halted resume keeps hotplug | pass | 2026-10-06 | Real qemu guest hotplugged 3/3072 → 4/4096, halted and resumed: FluxVM booted at 3/3072 and kairon-node hotplugged back to 4/4096 (`status.applied*` matched) |
| Pool claim carries template | pass | 2026-10-06 | `createMachine: true` claim from a FluxVM pool: Machine created with template resources, cloud-init, guest agent, TTL and forwards, pinned to the node, adopted by kairon-node; FluxVM's normalised `user` network and `default` storage produced no warnings |
| Snapshot schedule `dailyAt`/jitter/maxAge | pass | 2026-10-06 | CRDs applied (CEL rejects setting both `intervalSeconds` and `dailyAt`); a `dailyAt` schedule fired at its 18:50 UTC slot and projected the next run a day later. The snapshot itself failed (stopped Machine, no volume, no CSI snapshotter here), so `maxAgeSeconds` pruning is unit-tested only |
| CPU-pinning deploy wiring | pass | 2026-10-06 | `deploy-remote.sh --reserved-cpus=0-1`: `kairon-cpustate.path` copied the state file (`root:kairon 0640`), label `2-11` published; `--no-cpu-pinning` removed units, copy and env keys. Helm `node.cpuPinning` rendered only (`helm template`) |
| MCP approval | pass | 2026-10-06 | `delete_machine` under `--allow-write` refused with an ID; `kaironctl approve machine/default/NAME ID` wrote the annotation; the retry consumed it and deleted the Machine; audit outcomes `approval-required` then `approved:<user>` |
| Stale evacuation | pass | 2026-10-06 | Controller with `KAIRON_STALE_EVACUATION=true` and a hand-made stale `kairon-node-<node>` Lease in `kairon-system`: no action until `kaironctl node fence NODE --reason ...`, then the `evacuate=true` Machine got `Fenced=True StaleEvacuation` and lost `spec.nodeName`; the other was only logged; the fenced Machine deleted cleanly; `--clear` removed the annotation. Single node, so nothing was re-placed. Leases outside `kairon-system` are forbidden to the controller by RBAC |
| Discovered pinnable CPUs | pass | 2026-10-06 | 12 CPUs, kubelet `none` policy, `KAIRON_RESERVED_CPUS=0-1`: refused without reserved CPUs and on the root-only state file; with a readable copy published `2-11`; simulated static entry `4-5` gave `2-3_6-11`; removing the file cleared the label; an operator label was kept. Host restored to discovery off |

## Migration matrix

| Case | Last result | Date | Notes |
|---|---|---|---|
| Create / restart / delete smoke (≤100 VMs scaled down in CI) | not run | — | Multi-host matrix; single-host create/stop/start smoke is green above |
| Cold migration | not run | — | |
| Live pre-copy under load | not run | — | |
| Guest agent exec after migration | not run | — | `echo` through kairon-ui → kairon-node → qemu-guest-agent in the migrated Machine; needs `spec.guestAgent.enabled` and `KAIRON_UI_URL`/`KAIRON_UI_TOKEN` |
| Live + eBPF dataplane | not run | — | Machine with `spec.network.dataplaneMode=ebpf`; set `KAIRON_HW_EBPF_MACHINE`. Needs two hosts; also exercises the VM-edge conntrack export/restore |
| Source failure during transfer | not run | — | |
| Ambiguous commit → `NeedsRecovery` | not run | — | |
| Controller failover mid-migration | not run | — | |
| Adaptive: auto-converge under load | not run | — | `--strategy adaptive --auto-converge` with the guest dirtying `KAIRON_HW_ADAPTIVE_LOAD_MIB` through a `KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS` link; needs an `auto-converge`/`throttle` action, no post-copy, and `status.progress` |
| Adaptive: post-copy switch under load | not run | — | `--post-copy --post-copy-after=20s` under the same load; needs a `postcopy` action and `status.adaptive.postCopyStarted` |
| Adaptive: zstd multifd compression | not run | — | `--compression=zstd --compression-level=3` on an idle guest; needs `status.progress.compressionRatioPercent` above 100 |
| Source failure after post-copy → `NeedsRecovery` | not run | — | `lab-inject-postcopy-source-failure.sh` kills the source QEMU after the switch on the disposable `KAIRON_HW_POSTCOPY_MACHINE`; must land `NeedsRecovery` with the source not resumed, then `ForceAbort` |

`hardware-migration-matrix.sh --write-compat` rewrites these rows from a
run (the workflow uploads the result as the `compatibility-md` artifact);
without the flag it prints markdown rows for copy/paste. Every migration
case checks that the Machine ends up Running and Ready on the expected node,
and the script exits non-zero if any case fails. For the eBPF live case, the lab
Machine must set `dataplaneMode: ebpf` and `dataplaneRequired: true` so
migration network quiesce/export/restore exercises FluxVM's TC/eBPF path
(see [`network-fabric.md`](network-fabric.md)).

The four adaptive rows need FluxVM with the adaptive migration controls on
both hosts: each node's migration adapter `GET /v1/capabilities` must report
`tune`, `autoConverge`, `postCopySwitch` and `zstd` in `compression`. Adaptive live migration is Experimental until all
four pass on the nightly workflow alongside the base live rows.

## Incremental S3 backups

Single node plus MinIO, driven by
[`scripts/lab-cbt-backup.sh`](../scripts/lab-cbt-backup.sh) against a
disposable image-booted Machine with `spec.guestAgent.enabled` on a node
labelled `kairon.zyvor.dev/cbt=true` (FluxVM with QEMU 5.1+). `--minio-ssh`
starts MinIO with an Object Lock bucket and creates the labelled Secret; the
script prints these rows. Incremental S3 backups stay Experimental until all
four pass. FluxVM's own QEMU-gated engine test
(`stopped_vm_chain_round_trips_through_qemu`, passed on QEMU 10.2.1) covers
a stopped VM only, through qemu-storage-daemon, without Kairon.

| Case | Last result | Date | Notes |
|---|---|---|---|
| CBT: changed-bytes | not run | — | Full, dirty `--dirty-mib` (64) in the guest, incremental: `changedBytes` within `--dirty-mib` .. `+ --slack-mib` |
| CBT: restore | not run | — | Corrupt the guest file, halt, restore full + incremental, start: sha256 matches |
| CBT: synthesize | not run | — | Synthetic full of the incremental, delete the full (S3 objects too), restore the incremental again: sha256 matches |
| CBT: forced-full | not run | — | SIGKILL the Machine's QEMU (`--node-ssh`): the next incremental fails with `status.disk.fullRequired`, a full succeeds |

## Topology-aware placement

Driven by [`scripts/lab-topology.sh`](../scripts/lab-topology.sh) against one
node with the `kairon.zyvor.dev/pinnable-cpus` label and a FluxVM that
reports `host.topology`; the script prints these rows and reports a case
"not run" when the node's hardware cannot exercise it. Topology-aware
placement stays Experimental until the multi-NUMA, GPU+NIC and SR-IOV rows
pass. FluxVM's QEMU-gated `numa_qemu` test (passed on QEMU 10.2.1) checks
the memory backend's `policy=bind` and vCPU thread affinity without Kairon.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Topology: nodetopology | pass | 2026-10-11 | Single-socket node (12 CPUs, 1 NUMA node, QEMU 10.2.1, k3s 1.35): `NodeTopology` with 1 NUMA node, 3 PCI devices, `numaBinding` true |
| Topology: single-numa | pass | 2026-10-11 | Same node, pinnable CPUs 2-11: guest node 0 on host numa0, vCPU threads on CPUs 2-3, 1024MiB `policy=bind` (`status.appliedTopology`) |
| Topology: unsatisfiable | pass | 2026-10-11 | Same node: a `single-numa-node` Machine asking for an NVIDIA GPU stayed Pending, reason `TopologyUnsatisfiable` (one NUMA node, so the oversize-CPU variant is refused by the plain capacity check first) |
| Topology: multi-numa | not run | 2026-10-11 | Lab host has one NUMA node |
| Topology: gpu-nic | not run | 2026-10-11 | No allowlisted GPU and NIC under one PCIe root in the lab |
| Topology: sriov | not run | 2026-10-11 | No SR-IOV VF in the lab |

## Elastic memory

Driven by [`scripts/lab-elastic-memory.sh`](../scripts/lab-elastic-memory.sh)
against one node with a FluxVM that reports `host.balloon`. Pressure is
forced by setting `KAIRON_MEMORY_LOW_WATERMARK_MIB` above the host's
MemAvailable and restarting kairon-node; relief removes it. Elastic memory
stays Experimental until it has run under real (not forced) memory
pressure and on more than one node, including evacuation, which is not
covered here.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Elastic memory: balloon-device | pass | 2026-10-11 | Single node (31424MiB, QEMU 10.2.1, k3s 1.35, Ubuntu 24.04 guest): 2Gi Machine with min 1Gi; `status.memory` written; QEMU has `virtio-balloon-pci,deflate-on-oom=on,free-page-reporting=on` |
| Elastic memory: inflate | pass | 2026-10-11 | Same node: forced pressure moved the balloon target to 256MiB in the first pass, reason `HostPressure` |
| Elastic memory: exhausted | pass | 2026-10-11 | Same node: target and guest actual 1024MiB (at min), `KaironMemoryPressure=True`; a new Machine pinned to the node stayed unscheduled: "node reports KaironMemoryPressure" |
| Elastic memory: relief | pass | 2026-10-11 | Same node: target back to 0, reason `Relief`, `KaironMemoryPressure=False`; the guest was still deflating (255MiB actual) when the case ended |
| Elastic memory: overcommit | pass | 2026-10-11 | Same node: two 18854Mi Machines (min 512Mi) on 31424Mi allocatable; the second stayed unscheduled ("insufficient memory") until the node got `memory-overcommit-ratio: 1.5`, then scheduled |

## Hibernate

Driven by [`scripts/lab-hibernate.sh`](../scripts/lab-hibernate.sh) against one
node with a FluxVM that reports `host.hibernate`. The guest runs a cloud-init
probe that writes a random marker to `/dev/shm` and prints a tick counter, its
boot id and the marker to the serial console, so the image needs no guest
agent. Hibernate stays Experimental until resume on another node has run on two
real hosts sharing the hibernation directory and an RBD root disk.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Hibernate: hibernate | pass | 2026-10-12 | Single node (QEMU 10.2.1, `pc-q35-10.2`, k3s 1.35, Ubuntu 24.04 guest, 1Gi): Hibernated in 10s, 460 MiB of state, FluxVM record stopped with no pid |
| Hibernate: capacity | pass | 2026-10-12 | Same node: no QEMU process left; host MemAvailable 10494 -> 11140 MiB |
| Hibernate: resume | pass | 2026-10-12 | Same node: Running in 16s with `resumeNode` set to the node; tick 4 -> 6, same boot id and `/dev/shm` marker; `status.hibernation` cleared |
| Hibernate: discard | pass | 2026-10-12 | Same node: `discard-hibernation` annotation gave a new boot id; annotation removed, `status.hibernation` cleared |
| Hibernate: resume on another node | not run | 2026-10-12 | Needs two nodes sharing the hibernation directory and a shared root disk |

## Incident bundles

Driven by [`scripts/lab-incident-bundle.sh`](../scripts/lab-incident-bundle.sh)
against one node, through kairon-ui and the node console relay. The guest gets
two planted secrets through cloud-init (an `api_key=` file and a `password=`
runcmd) and a probe printing a tick counter and its boot id to the serial
console.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Incident bundle: auth | pass | 2026-10-12 | Single node (QEMU 10.2.1, k3s 1.35, Ubuntu 24.04 guest, 1Gi): POST diagnostics without a token is `401` |
| Incident bundle: bundle | pass | 2026-10-12 | Same node: `kaironctl diagnose machine --deep --memory-dump --pcap 5s` in 5s, 43 KB, 56 files; 48 of 52 node sections ok (no balloon device; no eBPF dataplane, so no stats, flows or pcap) |
| Incident bundle: redact | pass | 2026-10-12 | Same node: neither planted secret in any of the 56 files; 3 `redacted:sha256:` fingerprints |
| Incident bundle: memory dump | pass | 2026-10-12 | Same node: 1040 MiB ELF core, guest paused 1099 ms; downloaded with `kaironctl diagnose memory-dump` through kairon-ui, sha256 matches, contains the guest kernel banner |
| Incident bundle: continuity | pass | 2026-10-12 | Same node: probe ticked on (3 -> 9) in the same boot after the dump |
| Incident bundle: busy | pass | 2026-10-12 | Same node: a second start while a dump job runs is `409` |
| Incident bundle: admin session | pass | 2026-10-12 | Same node: an admin login session reads the job |
| Incident bundle: cleanup | pass | 2026-10-12 | Same node: deleting the Machine removed its FluxVM diagnostics directory |
| Incident bundle: packet capture | not run | 2026-10-12 | The lab's Machines are not on the eBPF dataplane, so FluxVM has no interface to capture on |
| Incident bundle: non-admin account refused | not run | 2026-10-12 | The lab has no non-admin kairon-ui account; covered by `internal/uiapi` tests |

## Virtual networks

Driven by [`scripts/lab-virtual-network.sh`](../scripts/lab-virtual-network.sh)
on one node with a FluxVM that reports `host.vnet` and kairon-node started
with `--vnet-uplink`. It creates two `VirtualNetwork`s with the same CIDR
(one with egress SNAT) and three Machines. The guests run a cloud-init probe
loop and report to their serial console, so the image needs no guest agent.
The uplink program ran on the node's SSH uplink ahead of Cilium's
`cil_from_netdev` for the whole run, and was detached when the last network
was removed. FluxVM's root-gated `vnet_netns` test separately routes across
two netns-simulated hosts over Geneve, and checks floating-IP DNAT and
egress, anti-spoofing and SNAT for TCP. Virtual networks stay Experimental
until the cross-host, floating-IP and BGP rows run on real nodes.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Virtual network: ipam | pass | 2026-10-12 | Single node (kernel 7.0, QEMU 10.2.1, k3s 1.35, Ubuntu 24.04 guests, Cilium on the uplink): two networks on 10.88.0.0/24 got VNIs 4096 and 4097; a1 10.88.0.10, a2 10.88.0.11, b1 10.88.0.10 (overlapping) as requested; `status.network.virtualNetwork` set |
| Virtual network: same-host | pass | 2026-10-12 | Same node: a1 and a2 ping each other on VNI 4096 |
| Virtual network: gateway | pass | 2026-10-12 | Same node: a1 pings the distributed gateway 10.88.0.1 |
| Virtual network: isolation | pass | 2026-10-12 | Same node: b1 (VNI 4097, a1's address) failed all 6 pings to a2's 10.88.0.11 while a1 reached it |
| Virtual network: snat | pass | 2026-10-12 | Same node: a1 reached 1.1.1.1 by ping and HTTP, translated to the node's uplink address |
| Virtual network: closed | pass | 2026-10-12 | Same node: b1 (no `egressSNAT`) failed all 6 pings to 1.1.1.1 |
| Virtual network: cleanup | pass | 2026-10-12 | Same node: after the Machines were deleted, kairon-node removed both VNIs from FluxVM and the uplink program was detached |
| Virtual network: cross-host | not run | — | Needs two nodes; Geneve between hosts has only run between netns-simulated hosts (FluxVM `vnet_netns`) |
| Virtual network: floating-ip | not run | — | Needs a public address routed to the node; covered only by FluxVM `vnet_netns` |
| Virtual network: bgp | not run | — | Needs a BGP router; the speaker is unit-tested against a fake peer |

## Component versions (fill per run)

| Piece | Version |
|---|---|
| Kairon | |
| Kubernetes | |
| FluxVM | |
| Cilium | |
| CSI / storage | |
