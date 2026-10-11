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
  (stops `kairon-node` on the source during transfer), and
  [`scripts/lab-inject-controller-failover.sh`](../scripts/lab-inject-controller-failover.sh)
  (restarts the controller mid-migration)

**Lab enablement (current blocker for the multi-host matrix):** the
`matrix` job runs only on `runs-on: [self-hosted, kairon-lab]`. Bring that
runner online and set repository secrets/vars `KAIRON_HW_LAB=1`,
`KAIRON_KUBE_URL`, `KAIRON_KUBE_TOKEN`, `KAIRON_KUBE_CA` (PEM content;
the workflow writes it to a file), `KAIRON_HW_MACHINE`,
`KAIRON_HW_TARGET_NODE`, plus optional `KAIRON_HW_EBPF_MACHINE` (live-eBPF
case), `KAIRON_HW_IMAGE` (real create/stop/start/delete smoke),
`KAIRON_HW_SOURCE_SSH` / `KAIRON_HW_TARGET_SSH` (failure drills; the runner
needs passwordless SSH and sudo for `iptables` / `systemctl`) and
`KAIRON_HW_CONTROLLER_SSH` (systemd controller instead of Pods). Until then,
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

`hardware-migration-matrix.sh --write-compat` rewrites these rows from a
run (the workflow uploads the result as the `compatibility-md` artifact);
without the flag it prints markdown rows for copy/paste. Every migration
case checks that the Machine ends up Running and Ready on the expected node,
and the script exits non-zero if any case fails. For the eBPF live case, the lab
Machine must set `dataplaneMode: ebpf` and `dataplaneRequired: true` so
migration network quiesce/export/restore exercises FluxVM's TC/eBPF path
(see [`network-fabric.md`](network-fabric.md)).

## Component versions (fill per run)

| Piece | Version |
|---|---|
| Kairon | |
| Kubernetes | |
| FluxVM | |
| Cilium | |
| CSI / storage | |
