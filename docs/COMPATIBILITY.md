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
| Create / restart / delete smoke (≤100 VMs scaled down in CI) | pass | 2026-10-14 | hwsmoke-032055 from /srv/kairon-shared/labsmoke/disk.img: create → healthy → stop → start → delete |
| Cold migration | pass | 2026-10-14 | `hw-cold-labm1` 80 ↔ 175 both directions on an NFS-shared `/srv/kairon-shared` (80 NFS client of 175) |
| Live pre-copy under load | pass | 2026-10-14 | Two-node k3s: 175.110.122.71 (QEMU 8.2.2, FluxVM 0.4.8) and 80.79.5.173 (QEMU 10.2.1, FluxVM 0.4.8), shared `/srv/kairon-shared` over NFS (hostPath PV, `shared-storage: "true"`), 1 Gbit link: `hw-live-labm1` 175 → 80 with 64 MiB dirtied, downtime 253 ms (263 ms on a repeat), total 883 s, data plane encrypted. At the 256 MiB default the guest dirtied (961 Mbps) about as fast as the link sent (930 Mbps) and took ~10 min, past the matrix's 600 s timeout; set `KAIRON_HW_LOAD_MIB`/`KAIRON_HW_TIMEOUT` for slow links |
| Guest agent exec after migration | pass | 2026-10-14 | `labm1` on 80 after the live migration: `echo` through kairon-ui → node console relay → qemu-guest-agent, exit 0 |
| Live + eBPF dataplane | not run | — | Machine with `spec.network.dataplaneMode=ebpf`; set `KAIRON_HW_EBPF_MACHINE`. Needs two hosts; also exercises the VM-edge conntrack export/restore |
| Source failure during transfer | pass | 2026-10-14 | `lab-inject-source-failure.sh`: source kairon-node stopped 20 s mid-transfer, restarted; migration Succeeded and `labm1` healthy on 80 |
| Ambiguous commit → `NeedsRecovery` | not run | 2026-10-14 | `recovery-drill.sh` arms its DROP on the destination's :9443 six seconds after the migration starts; an idle 2 GiB guest on this link finished and committed first, so the migration landed Succeeded (twice). Needs a longer transfer (guest load) or a DROP armed before commit |
| Controller failover mid-migration | pass | 2026-10-14 | `lab-inject-controller-failover.sh` 175 → 80: controller restarted mid-migration; migration Succeeded |
| Adaptive: auto-converge under load | pass | 2026-10-14 | Two-node k3s: 175.110.122.71 (QEMU 8.2.2, FluxVM 0.4.8) and 80.79.5.173 (QEMU 10.2.1, FluxVM 0.4.8), shared `/srv/kairon-shared` over NFS (hostPath PV, `shared-storage: "true"`), 1 Gbit link: 1024 MiB dirtied, 200 Mbps cap; downtime 60 ms, total 713 s |
| Adaptive: post-copy switch under load | pass | 2026-10-14 | Same hosts: 1024 MiB dirtied, 200 Mbps; switched to post-copy, downtime 28 ms, total 31 s |
| Adaptive: zstd multifd compression | pass | 2026-10-14 | Same hosts: downtime 24 ms, total 1.3 s, compression ratio 301% |
| Source failure after post-copy → `NeedsRecovery` | pass | 2026-10-14 | `labpc` 175 → 80: source QEMU killed after the switch; `NeedsRecovery`, then `ForceAbort` ended it Failed |

**Direction matters between mixed QEMU releases.** QEMU only migrates to the same or a newer release. With 175 on 8.2.2 and 80 on 10.2.1, every 80 → 175 attempt failed on the target with `error while loading state for instance 0x0 of device 'cpu'`; a Machine booted on 80 also gets the `pc-q35-10.2` machine type, which 8.2.2 refuses outright. The rows above boot Machines on 175 and migrate 175 → 80.

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

## Agent sandboxes

Driven by [`scripts/bench-sandbox.sh`](../scripts/bench-sandbox.sh) on one
node with an image that runs qemu-guest-agent (see
[agent-sandboxes.md](guides/agent-sandboxes.md)). It needs kairon-ui with the
console relay configured (exec, wake on connect, `vnc screenshot` and
`guestfs` go through it), kairon-node `--image-cache-dir` (clone and cdrom)
and libguestfs-tools on the node for `guestfs`.

Run on 2026-10-14 against `nldw4-4-16-36` (k3s 1.36, kairon-hw Ubuntu guest,
1 vCPU / 1Gi, image on NFS). That node's FluxVM build has no hibernate or fork
endpoint and its kairon-node had no libguestfs or image cache during the run,
so those rows are environment failures, not Kairon results. The existing
[Hibernate](#hibernate) rows passed on the other lab node.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Sandbox: idle pause | pass | 2026-10-14 | `--idle-timeout 1m`: Paused by kairon-controller 212s after the agent came up (the guest stayed busy for its first 2.5 minutes; last activity 60s before the pause) |
| Sandbox: wake on connect (Paused) | pass | 2026-10-14 | `kaironctl guest exec` on the Paused Machine returned in 1.6s including the resume; phase and powerState Running afterwards |
| Sandbox: wake on connect (Hibernated) | not run | 2026-10-14 | This node's FluxVM returns 404 for `POST /v1/vms/{id}/hibernate` |
| Sandbox: pool claim, Running pool | pass | 2026-10-14 | `status.bindMillis` 2267 against a warm Running member |
| Sandbox: pool claim, Paused pool | pass | 2026-10-14 | `status.bindMillis` 4193 against a warm Paused member (includes the resume) |
| Sandbox: fork | not run | 2026-10-14 | This node's FluxVM returns 404 for `POST /v1/vms/{id}/fork` |
| Sandbox: lifetime | pass | 2026-10-14 | `--max-lifetime 1m` Machine deleted by kairon-controller (seen within 4 minutes in an earlier partial run; the script now times it) |
| Sandbox: freeze auto-thaw | pass | 2026-10-14 | `freeze --unfreeze-timeout 1m` confirmed in 4.2s; kairon-node thawed it 63s after the request |
| Sandbox: freeze/unfreeze | pass | 2026-10-14 | Round trip confirmed in 3.8s; the guest writes again afterwards |
| Sandbox: vnc screenshot | fail | 2026-10-14 | kairon-node got `permission denied` on FluxVM's `vnc.sock` on this node; same relay path as `kaironctl vnc` |
| Sandbox: create clone from Halted | pass | 2026-10-14 | `qemu-img convert` of the source disk took 117s (675 MB image on NFS), the clone booted and its guest agent answered 40s later |
| Sandbox: guestfs read-only | not run | 2026-10-14 | libguestfs-tools is not installed on this node |
| Sandbox: cdrom insert/eject | partial | 2026-10-14 | Stop cleared `status.runtimeID` in 13s and `cdrom insert` was admitted; the start then needs `--image-cache-dir`, which this node lacked; `cdrom eject` removed the entry and the Machine then started. Live eject not exercised |

## Agent sessions

Needs a Mac node running kairon-node natively against FluxVM 0.4.8 or later
(FluxVM creates agent sessions only on its Apple Virtualization backend),
kairon-ui with the console relay, and `ui.agentSessions.previews: true` for
the preview row. The lab has no Mac, so nothing below has run on real
hardware. Unit tests cover the same paths against a fake FluxVM (see
[agent-sessions.md](guides/agent-sessions.md)).

| Case | Last result | Date | Notes |
|---|---|---|---|
| Session: capability label | not run | - | Mac node gets `kairon.zyvor.dev/agent-sessions=true`; a Linux node with FluxVM 0.4.8 does not |
| Session: run with git workspace | not run | - | `kaironctl session run` reaches Running; `status.workspace.cloned` true |
| Session: offline policy | not run | - | `--preset offline`: outbound request from `session exec` fails; `status.policy.offline` true |
| Session: allow-host policy | not run | - | Only the allowed host answers; `status.policy.unenforced` empty on vz |
| Session: exec and jobs | not run | - | `session exec` exit code matches; `bg` + `logs -f` streams to the end |
| Session: files and git | not run | - | `put`/`cat` round trip; `git commit` shows in `git log` |
| Session: checkpoint and restore | not run | - | A file written after `checkpoint` is gone after `restore` |
| Session: fork and promote | not run | - | Each child becomes an AgentSession on the parent's node; `promote` applies a child's change to the parent |
| Session: rollout | not run | - | `--count 4`: four results, children deleted afterwards |
| Session: preview | not run | - | URL opens through kairon-ui; CSP sandbox header present; 404 after `unexpose` |
| Session: OTLP export | not run | - | Trace shows in the collector with one span per step |
| Session: approvals | not run | - | `--process-ask 'git push*'`: exec answers 428; an operator's `approve` gets 403; after an admin approves, `--approval-id` runs it once; note names the approver |
| Session: chunked upload | not run | - | `put` of a 50 MiB file: same sha256 in the guest; an interrupted put leaves no file |
| Session: shell | not run | - | `session shell` echoes input, follows a window resize, exits with the shell's code; refused when the session has process rules |
| Session: probe | not run | - | Lists the guest's git and node; `ready_for.git` true |
| Session: screen | not run | - | FluxVM after 0.4.8: `session screenshot` saves a PNG of the guest display; `input click` and `type` change it; `session_screenshot` returns an image to an MCP client; the audit log has no typed text |
| Session: engine | not run | - | `--engine agent` with a stock image: `Failed` with FluxVM's 409; with `agent-micro`: Running and `status.capabilities.engine` is `agent` |
| Session: fork clone report | not run | - | Fork on APFS: `files_copied` 0, no warning |
| Session: credential grant | not run | - | `grant --host api.github.com`: an authenticated API call from the guest succeeds, the guest's environment has no token; fails after `revoke-grant` |
| Session: quota | not run | - | A session over `MachineQuota` stays Pending with the quota's reason |
| Session: delete | not run | - | FluxVM session gone; finalizer released |

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
| Incident bundle: packet capture | pass | 2026-10-14 | 80.79.5.173 (FluxVM 0.4.8, default eBPF dataplane), Machine `mode: tap`, `netns`, `dataplaneMode: ebpf`: `kaironctl diagnose machine --deep --pcap 5s` through kairon-ui; `node/network/capture.pcap` held 8 packets (guest ARP requests and replies for 169.254.0.1) |
| Incident bundle: non-admin account refused | pass | 2026-10-14 | Two-node k3s (80, kairon-ui self-signed TLS): non-admin `viewer` (`KAIRON_UI_USERS_JSON`, `admin:false`) `POST …/diagnostics` → `403 incident bundles require an admin account`, `GET …/diagnostics/<token>` → 403, plain machine read still 200; the admin account's same POST → 202 |

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

## NVMe-oF storage

Driven by [`scripts/lab-nvmeof.sh`](../scripts/lab-nvmeof.sh) on one node,
which is both the storage node and the Machine host. A dummy interface
carries two tcp portals, and soft-RoCE (`rdma_rxe`) on the same interface
carries an rdma portal. The first four cases drive the CSI Controller and
Node servers in-process against the real kernel target and initiator. The
rest go through `kairon-csi-node`, a host-installed `kairon-node` and
FluxVM. The Machine downloads its qcow2 image from a loopback HTTP server
(`spec.image.source`), and kairon-node writes it onto the empty Block
volume with `qemu-img`. The guest prints a tick counter, its root device
and its disk sizes to the serial console, so the image needs no guest
agent.

| Case | Last result | Date | Notes |
|---|---|---|---|
| NVMe-oF: tcp | pass | 2026-10-12 | Single node (kernel 7.0.0-34, `nvme_core.multipath=Y`): Filesystem volume over 2 tcp portals, ext4, online expand to 463 MiB; 2 of 2 paths live, ANA optimized |
| NVMe-oF: ana | pass | 2026-10-12 | Same node: one portal's ANA group set inaccessible showed 1 of 2 paths usable; a 1 MiB write+fsync went through |
| NVMe-oF: failover | pass | 2026-10-12 | Same node: volume unexported from one portal; a 1 MiB write+fsync took 2 ms over the other path; the path reconnected on re-export |
| NVMe-oF: rdma | pass | 2026-10-12 | Same node: raw Block volume over nvme-rdma (soft-RoCE); a 4 KiB pattern at 64 MiB written through the published device read back with O_DIRECT |
| NVMe-oF: block-boot | pass | 2026-10-12 | Same node (QEMU 10.2.1, k3s 1.35, Ubuntu 24.04 guest): 4 GiB nvme-tcp Block PV seeded from a qcow2 image; QEMU root on `/dev/nvme0n1` (FluxVM `storage: shared`), guest root `/dev/vda1` |
| NVMe-oF: io | pass | 2026-10-12 | Same node: over QMP, the virtio-blk data disk has an iothread, `num-queues=2` and `cache.direct` (cache none), and FluxVM records `aio: io_uring` (QMP does not report aio). The vhost-user-blk disk has `num-queues=2` on one `qemu-storage-daemon`. The guest sees both 1 GiB disks |
| NVMe-oF: paths | pass | 2026-10-12 | Same node: `StoragePaths` True, then `PathsDegraded` with one portal unexported, then True after re-export; the guest kept ticking |
| NVMe-oF: raw image onto a Block volume | pass | 2026-10-14 | `lab-nvmeof.sh` on 80 (QEMU 10.2.1, kernel 7.0.0-34), `--image` a 3.5 GiB raw disk: kairon-node wrote it onto an empty 4 GiB nvme-tcp Block PV without `qemu-img`; QEMU root `/dev/nvme0n1` (FluxVM `storage: shared`), guest root `/dev/vda1`; io and paths cases also passed |
| NVMe-oF: rdma on a real RDMA NIC | not run | 2026-10-12 | soft-RoCE only in this lab |
| NVMe-oF: in-cluster DaemonSets | not run | 2026-10-12 | The lab ran host-installed binaries; the chart is rendered and linted only |
| NVMe-oF: two nodes | not run | 2026-10-12 | Storage node and Machine host were the same machine |

## Disk QoS

Driven by [`scripts/lab-qos.sh`](../scripts/lab-qos.sh) on one node with
host-installed kairon-controller, kairon-node and FluxVM from this change.
The Machine's data disk is a 1 GiB raw file behind a hostPath PV. The guest
measures 4 KiB direct I/O on both disks for one second and prints the rates
to the serial console. QEMU's default burst lets one-second samples run
5–15% over the limit. All cases below passed in one run, with load average
24–32 from other workloads on the host. In an earlier run the
`ch-virtiofs` case failed once: Cloud Hypervisor exited with `Cannot create
virtio-rng device: Operation not permitted`. Two manual retries and the
next full run passed.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Disk QoS: crd-validation | pass | 2026-10-13 | Single node (kernel 7.0.0-34, QEMU 10.2.1, k3s 1.35): `iopsTotal` with `iopsRead` and a throttle on a vhost-user-blk disk both rejected with 422 |
| Disk QoS: fluxvm-boot | pass | 2026-10-13 | Same node, FluxVM API only: QMP shows root 300 IOPS and 50 MiB/s after create; a live PUT to 350 IOPS and a hot-added data disk with 150 read / 120 write IOPS both survive stop/start (new QEMU pid) |
| Disk QoS: ch-virtiofs | pass | 2026-10-13 | Same node, Cloud Hypervisor v53.0: a VM with a virtiofs share stays up after 20 s and `ch-remote info` lists `fs0@virtiofs-0.sock`. Before this change FluxVM passed `dax=off`, which v53 rejects, and still reported the VM running |
| Disk QoS: boot | pass | 2026-10-13 | Same node: Machine with root 200 IOPS and data disk 10 MiB/s; `status.appliedDiskThrottles` and QMP match; guest 207 IOPS on root, 11151 KiB/s on the data disk |
| Disk QoS: live-change | pass | 2026-10-13 | Same node: raised to 400 IOPS and 20 MiB/s; same QEMU pid; guest 428 IOPS, 22288 KiB/s |
| Disk QoS: lift | pass | 2026-10-13 | Same node: `image.io: null` lifts the root limit in QMP and status; guest root 17997 IOPS |
| Disk QoS: recreate | pass | 2026-10-13 | Same node: root limit back, then `Stopped` → `Running`; the new FluxVM runtime's create request carries `root_disk_io.throttle` and QMP shows 200 IOPS |
| Disk QoS: limits across live migration | not run | 2026-10-13 | One lab node; QEMU does not migrate throttles, so kairon-node reapplies them on the target (unit-tested only) |
| virtiofs DAX | not supported | 2026-10-13 | QEMU 10.2.1 `vhost-user-fs-pci` has no `cache-size`; Cloud Hypervisor v53 `--fs` has no `dax` |

## Node maintenance and DRS

Driven by [`scripts/lab-maintenance.sh`](../scripts/lab-maintenance.sh) on
one node, against a host-installed kairon-controller (run with
`-drs-mode=recommend -drs-interval=10s` through a temporary systemd drop-in)
and FluxVM. Enter and exit go through `kaironctl node maintenance`. One QEMU
Machine prints a tick counter, its boot id and a `/dev/shm` marker to the
serial console. With one node there is never a migration target, so these
cases exercise the cordon, the fallbacks and exit, not migration.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Node maintenance: drs | pass | 2026-10-12 | Single node (k3s 1.35, QEMU 10.2.1): recommend mode exported `kairon_drs_node_utilization_percent` (8% reserved) and `kairon_drs_imbalance_percent` 0; `kaironctl drs plan` reported no moves with 1 eligible node |
| Node maintenance: wait | pass | 2026-10-12 | Same node: cordoned and `Entering` in 17s with "1 with no eligible target node"; the Machine stayed Running, no MachineMigration was created, `MaintenanceEntering` Event recorded |
| Node maintenance: stop | pass | 2026-10-12 | Same node: `--fallback stop` stopped the Machine and parked it (`from Running to Stopped`); its QEMU process was gone; `InMaintenance` in 15s with a `MaintenanceReady` Event and `kairon_node_maintenance{state="InMaintenance"} 1` |
| Node maintenance: exit | pass | 2026-10-12 | Same node: exit uncordoned the node and the Machine was Running again in 69s in a fresh boot; parked annotation removed, `MaintenanceExited` Event |
| Node maintenance: hibernate | pass | 2026-10-12 | Same node: `--fallback hibernate` hibernated the Machine and reached `InMaintenance` in 27s; exit resumed it in the same boot (same boot id and `/dev/shm` marker, tick 4 -> 12) |
| Node maintenance: cordon | pass | 2026-10-12 | Same node: on a node the operator had cordoned, maintenance reached `InMaintenance` without marking the cordon as its own, and the node stayed cordoned after exit |
| Node maintenance: migrate Machines to another node | not run | 2026-10-12 | Needs two nodes |
| DRS: recommend or automatic moves | not run | 2026-10-12 | Needs two nodes |
| Node maintenance and DRS: in-cluster chart RBAC | pass | 2026-10-14 | Chart rendered (`helm template`) and applied under a throwaway ServiceAccount: controller role can get/list/watch/patch nodes (PATCH of a node annotation with its own token → 200) and not update nodes, delete namespaces, create clusterrolebindings or list secrets/pods cluster-wide; node role can get/list/patch, not watch. Objects removed afterwards. Controller binary itself still ran with `deploy/rbac.yaml`, not the chart |

The pressure signal is driven by
[`scripts/lab-drs-pressure.sh`](../scripts/lab-drs-pressure.sh) on the same
node, with host-installed FluxVM, kairon-node and kairon-controller from
this change. Two Machines run on the node: one burns both vCPUs, one idles.
Their QEMU threads are pinned to host CPUs 10 and 11, which two host busy
loops keep loaded for 150 s. Without that contention a burning guest gets
all the CPU it wants and does not stall. The script creates a second,
synthetic Node with no kubelet or kairon-node, and keeps its heartbeat,
lease and a calm pressure annotation fresh. The lab Machines tolerate the
`not-ready`, `unreachable` and Cilium `agent-not-ready` taints such a node
gets. kairon-controller runs `-drs-mode=recommend -drs-signal=pressure`
through a temporary systemd drop-in, so nothing is migrated to the synthetic
node. Host PSI on this shared node was 20–55% in CPU or IO across runs, from
other workloads.

| Case | Last result | Date | Notes |
|---|---|---|---|
| DRS pressure: publish | pass | 2026-10-13 | Single node (kernel 7.0.0-34, QEMU 10.2.1, k3s 1.35): a 5 s old Node annotation had cpu 52.0, memory 0.1, io 18.7; `/proc/pressure` avg60 read right after was 52.58, 0.13, 19.26 |
| DRS pressure: machine-stall | pass | 2026-10-13 | Same node, CPUs 10–11 contended: the burning Machine's cgroup CPU PSI was 5.4% against 2.3% for the idle one. Without contention both read about 0.2% |
| DRS pressure: plan | pass | 2026-10-13 | Same node: `kaironctl drs plan --signal pressure` planned the burning Machine from the real node (cpu 52.0%, threshold 25%) to the synthetic node (0.0%), "this Machine stalls 5.4%" |
| DRS pressure: controller-recommend | pass | 2026-10-13 | Same node: kairon-controller recorded a `DRSRecommendation` Event on the burning Machine: "cpu pressure on nldw3-4-32-26 is 51.1% (threshold 25%); … this Machine stalls 4.1%" |
| DRS pressure: stale | pass | 2026-10-13 | Same node: with the synthetic node's sample backdated 10 minutes, the plan left it out ("no fresh sample") and planned no moves |
| DRS pressure: automatic move between two real nodes | not run | 2026-10-13 | One lab node; the synthetic node only receives recommendations |

## GPU and vGPU

Driven by [`scripts/lab-vgpu.sh`](../scripts/lab-vgpu.sh) on one node with
the kernel's `mbochs` sample mdev driver standing in for a vGPU host driver:
a virtual parent with three display profiles (4, 16 and 64 MiB) sharing a
256 MiB pool. Host-installed kairon-controller, kairon-node and FluxVM. The
guest prints the count of `1234:1111` display functions and the size of
BAR0 to the serial console. KubeVirt also runs on the lab cluster, and its
virt-handler removed instances it had not created, so the run had its
`DisableMDEVConfiguration` feature gate on (reverted afterwards).

| Case | Last result | Date | Notes |
|---|---|---|---|
| vGPU: inventory | pass | 2026-10-12 | Single node (kernel 7.0.0-34, QEMU 10.2.1, k3s 1.35): FluxVM `/v1/host/mdev` listed mbochs-small 4 MiB/64 free, medium 16/16, large 64/4; kairon-node published the same in `NodeTopology` `mediatedDevices` |
| vGPU: health | pass | 2026-10-12 | Same node: FluxVM judged the onboard VGA `0000:04:00.0` healthy and `NodeTopology` carried `Healthy` on the device |
| vGPU: boot | pass | 2026-10-12 | Same node: a Machine asking for `profile: mbochs-small` was Running in 76s with allocation `mbochs`/UUID; FluxVM `vfio_devices` held the UUID, QEMU had `sysfsdev=/sys/bus/mdev/devices/<uuid>`, and the guest saw one display function with a 4 MiB BAR0 |
| vGPU: accounting | pass | 2026-10-12 | Same node: a second Machine asking for 4 mbochs-large stayed Pending with `TopologyUnsatisfiable: needs 4 gpu profile mbochs-large, 3 free`; `kairon_vgpu_available_instances{mbochs-small}` 63, `kairon_gpu_vram_mib` allocated 4 of 256 |
| vGPU: stop | pass | 2026-10-12 | Same node: stopping removed the instance; mbochs-small back to 64 free, mbochs-large 4 |
| vGPU: start | pass | 2026-10-12 | Same node: starting re-created the same UUID, QEMU held it and the guest saw the 4 MiB vGPU again |
| vGPU: delete | pass | 2026-10-12 | Same node: deleting the Machine removed the instance; mbochs-small 64 free |
| vGPU: NVIDIA vGPU profiles on a real GPU | not run | 2026-10-12 | Needs an NVIDIA GPU with the vGPU host driver |
| vGPU: MIG-backed profiles, nvidia-smi health and VRAM | not run | 2026-10-12 | Needs an NVIDIA MIG-capable GPU |
| GPU health: Unhealthy GPU skipped by the scheduler | not run | 2026-10-12 | Needs a GPU with AER or ECC errors; unit-tested |

## Accelerator profiles

Driven by [`scripts/lab-accel.sh`](../scripts/lab-accel.sh) on the same
node and `mbochs` driver as the GPU and vGPU rows: four 64 MiB
`mbochs-large` instances fit in its 256 MiB pool. Host-installed
kairon-controller from this change, kairon-node and FluxVM. KubeVirt's
`DisableMDEVConfiguration` feature gate was on for the run and reverted
afterwards. An earlier run without it failed: virt-handler removed one of a
running Machine's two instances from sysfs. The guest probe then never
printed, and a later run counted the missing instance as still pending, so
the profile reported capacity 0 instead of 1.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Accelerator profile: profile-status | pass | 2026-10-12 | Single node (kernel 7.0.0-34, QEMU 10.2.1, k3s 1.35): pair profile (2 `mbochs-large`, `p2pRequired`, `sameNuma`) `Ready=True`, `Available=True`, capacity 2 on the node; `migration.required` profile `Ready=False/LiveMigrationUnsupported`; 1Gi-per-device profile `Available=False/NoCapacity`: `needs 1 gpu profile mbochs-large with 1Gi, 0 free` |
| Accelerator profile: crd-validation | pass | 2026-10-12 | Same node: class `accelerator` with `memory.minimumPerDevice` refused with 422 |
| Accelerator profile: expand-boot | pass | 2026-10-12 | Same node: a Machine with `accelerators: [{name: vga, profile: <pair>}]` got annotation `vga=<pair>@1` and one `spec.devices` entry (2 `mbochs-large`, `sameNuma` + `samePcieRoot`), two instances on `mbochs`, Running in 78s; the guest saw 2 display functions with a 64 MiB BAR0 |
| Accelerator profile: capacity | pass | 2026-10-12 | Same node: the pair profile then had capacity 1 and 1 Machine, in status and in `kairon_accelerator_profile_capacity` / `_machines` |
| Accelerator profile: unavailable | pass | 2026-10-12 | Same node: Machines naming a missing profile and the live-migration profile stayed Pending with `AcceleratorProfileUnavailable` and no node |
| Accelerator profile: unsatisfiable | pass | 2026-10-12 | Same node: a Machine with the 1Gi profile was expanded and stayed Pending with `TopologyUnsatisfiable: ... needs 1 gpu profile mbochs-large with 1Gi, 0 free` |
| Accelerator profile: snapshot | pass | 2026-10-12 | Same node: changing the pair profile's count from 2 to 1 (generation 2) left the running Machine's `spec.devices`, annotation and both instances as they were |
| Accelerator profile: delete | pass | 2026-10-12 | Same node: deleting the Machine removed both instances; the profile (now count 1) reported capacity 4 and 0 Machines |
| Accelerator profile: whole GPUs with `p2pRequired` under one PCIe root | not run | 2026-10-12 | Needs two allowlisted GPUs; unit-tested |
| Accelerator profile: RDMA NIC next to the GPUs | not run | 2026-10-12 | Needs a GPU and a NIC under one PCIe root; unit-tested |
| Accelerator profile: `sameNuma` on a multi-NUMA host | not run | 2026-10-12 | Lab host has one NUMA node |
| Accelerator profile: NVIDIA vGPU or MIG profile | not run | 2026-10-12 | Needs an NVIDIA GPU |

## Cross-cluster DR

Driven by [`scripts/lab-dr.sh`](../scripts/lab-dr.sh) on one node against a
host-installed kairon-controller (with `--dr-secret-namespace` and
`--dr-test-bridges=drtest0`), kairon-node and FluxVM. The two sites are two
namespaces, `dr-a` and `dr-b`, on the same k3s cluster: each one's peer is
the same API server, reached with a `kairon-dr-peer` ServiceAccount token.
Both share one bucket on versitygw 1.8.0 (POSIX backend). Site loss is
simulated by unlabelling the peer credentials Secret, not by a network
partition. Site A runs db and web (web depends on db) on a VirtualNetwork
`app` (10.77.0.0/24, same name and CIDR on both sites); web is created
first, so A's addresses (web .2, db .3) are the reverse of what B would
allocate in failover order. Each guest fsyncs a generation and timestamp
every 2s, so a restored guest reports the instant its disk returns to. The
plans set `network.preserveIP` and DNS records for both Machines; the lab
installs a minimal external-dns `DNSEndpoint` CRD and runs no external-dns.

| Case | Last result | Date | Notes |
|---|---|---|---|
| DR: replicate | pass | 2026-10-12 | Single node (k3s 1.35, QEMU 10.2.1), policy every 5m: dr-b imported 2 restore points 65s after the plan was created; RPO 49s, `RPOMet=True` |
| DR: network readiness and DNS on the active site | pass | 2026-10-12 | Same node: dr-a published `DNSEndpoint` dr-shop (db.shop.drlab.example=10.77.0.3, web.shop.drlab.example=10.77.0.2), dr-b none; dr-b's mirrored specs pinned db=10.77.0.3, web=10.77.0.2 and reported `NetworkReady=True` |
| DR: test | pass | 2026-10-12 | Same node: `kaironctl dr test` started copies of db and web on the isolated bridge `drtest0` in 194s; their last writes were within 1s of the restore points; dr-a kept running; `dr test-end` removed the copies |
| DR: scheduled test | pass | 2026-10-12 | Same node, `test.every: 5m` after a manual test: dr-b started a scheduled test by itself, created and restored both copies (Running in 173s), ended it and removed the copies by itself; `lastTest.succeeded=True`, `LastTestSucceeded=True` |
| DR: consolidate | pass | 2026-10-12 | Same node: after a policy synthetic full of web, the next replication was an incremental on it and retention made no further synthetic full |
| DR: failover | pass | 2026-10-12 | Same node, sites cut off: db then web Running on dr-b, RTO 208s (objective 10m), data loss 63s; restored disks within 3s of the restore points; dr-a kept running while cut off and was fenced 7s after reconnecting (Machines Halted, policy deleted, epoch 2) |
| DR: preserve VirtualNetwork addresses | pass | 2026-10-12 | Same node: db and web came up on dr-b with dr-a's addresses (db 10.77.0.3, web 10.77.0.2) although dr-b created db first |
| DR: DNS follows the active site | pass | 2026-10-12 | Same node: after the failover the `DNSEndpoint` was on dr-b only and the fenced dr-a had deleted its own; after the failback it was on dr-a only again |
| DR: failback | pass | 2026-10-12 | Same node: planned failover back to dr-a in 164s; dr-b halted and took final backups, db's last write on dr-b (generation 378) was on dr-a, data loss 0s; dr-b standby at epoch 3 |
| DR: two real clusters | not run | 2026-10-12 | Needs a second cluster; the lab's sites share one API server |
| DR: separate or replicated buckets per site | not run | 2026-10-12 | Both sites used one bucket |
| DR: real network partition between sites | not run | 2026-10-12 | Site loss was simulated by unlabelling the peer Secret |
| DR: external-dns writing to a real zone | not run | 2026-10-12 | No external-dns ran; only the `DNSEndpoint` objects were checked |
| DR: floating IP moving between clusters | not run | 2026-10-12 | Floating IPs are unique per cluster, so the one-cluster lab cannot carry one over; needs two clusters with BGP |

## Mac Fabric

Driven by [`scripts/lab-mac-fabric.sh`](../scripts/lab-mac-fabric.sh) on the one-node Linux lab cluster with a
host-installed kairon-controller. The lab has no Macs. The script creates three Mac Nodes through the API, with the
labels, annotations, vm-only taint and Ready heartbeat kairon-node publishes:

- mac-a: M4 Max, macOS 26, Rosetta, nested virtualization, Thunderbolt fabric `lab`.
- mac-b: M3, macOS 15, Rosetta, nested virtualization, fabric `lab`.
- mac-c: M1, macOS 14, no Rosetta, no Thunderbolt.

No agent runs for them, so nothing boots. The overlay cases run `macnode.OverlaySync`, the code kairon-node runs on a
Mac, once for each simulated Mac, writing into temporary directories.

| Case | Last result | Date | Notes |
|---|---|---|---|
| Mac Fabric: capability scheduling | pass | 2026-10-12 | k3s 1.35: `minChipGeneration: 4` placed on mac-a; M3+, macOS 15+ and nested placed on mac-b; `minChipGeneration: 5` and Rosetta pinned to mac-c stayed Pending with the per-node reason |
| Mac Fabric: backend isolation | pass | 2026-10-12 | Same cluster: the API rejected `spec.apple` on qemu (422); a vz Machine pinned to the Linux node stayed Pending ("the vz backend needs a Mac"); a qemu Machine pinned to a Mac stayed Pending ("a Mac runs only the vz backend", or the vm-only taint without a toleration) |
| Mac Fabric: MacNetwork allocation | pass | 2026-10-12 | Same cluster: a fabric network got 10.89.192.0/24, port 48192, mac-a .2-.31 and mac-b .32-.61 over their Thunderbolt addresses; a plain one got 10.89.193.0/24, port 48193 and all three Macs over InternalIP, 20 hosts each |
| Mac Fabric: membership scheduling | pass | 2026-10-12 | Same cluster: a Machine on the fabric network was placed on a member; pinned to mac-c it stayed Pending ("not a member of MacNetwork fab"); a missing network stayed Pending ("not found") |
| Mac Fabric: overlay files | pass | 2026-10-12 | Same cluster: the three Macs' `overlay.json` had the right subnet, slice, port and the other members as peers; key files matched between Macs and were 0600; an entry and key Kairon did not write were kept |
| Mac Fabric: key rotation | pass | 2026-10-12 | Same cluster: `spec.keyEpoch: 1` gave status `keyEpoch` 1 and `previousKeyEpoch` 0; both members' key files listed the new key first and the unchanged epoch-0 key second |
| Mac Fabric: a Mac leaving | pass | 2026-10-12 | Same cluster: removing mac-b's fabric label left mac-a the only member with the same slice and subnet; mac-b's entry and key were removed. This run found `"peers": null` for a lone member, which FluxVM's parser rejects; fixed to `[]` |
| Mac Fabric: `kaironctl mac` | pass | 2026-10-12 | Same cluster: `mac nodes` listed chip, cores, macOS, Rosetta, nested, Thunderbolt and fabric; `mac network` showed subnet, port and members |
| Mac Fabric: vz Machines booting on several Macs | not run | 2026-10-12 | Needs Macs |
| Mac Fabric: Rosetta and nested virtualization inside a guest | not run | 2026-10-12 | Needs an M3 or later Mac with macOS 15 or later |
| Mac Fabric: MacNetwork traffic between Macs over Thunderbolt | not run | 2026-10-12 | Needs two cabled Macs running FluxVM |
| Mac Fabric: capability and Thunderbolt discovery on real Macs | not run | 2026-10-12 | `discover_darwin.go` is vetted for darwin/arm64 but has not run on a Mac since this change |

## Component versions (fill per run)

| Piece | Version |
|---|---|
| Kairon | |
| Kubernetes | |
| FluxVM | |
| Cilium | |
| CSI / storage | |
