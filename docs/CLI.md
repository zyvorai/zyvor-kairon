---
sidebar_position: 5
title: CLI
---

# CLI (`kaironctl` / `kubectl kairon`)

`kaironctl` is a [Cobra](https://github.com/spf13/cobra)-based CLI with Cilium-style colored tables, emoji progress for install/uninstall, and hierarchical help.

Commands that talk to `kairon-ui` (image upload, network capture, guest operations) call the routes in the [kairon-ui API reference](guides/kairon-ui-api.md).

**Dependency exception:** the CLI embeds the Helm chart (`go:embed`) and drives install/upgrade/uninstall through `helm.sh/helm/v3`. `kairon-controller` and `kairon-node` remain Go-stdlib-only — see [DEPENDENCIES.md](DEPENDENCIES.md).

```bash
kaironctl --help
kaironctl install --help
```

Global flags: `-n/--namespace`, `--kubeconfig PATH`, `--context NAME` (see [Meta](#meta) for cluster selection) and `--color=auto|always|never` (also respects `NO_COLOR`). Helm-backed commands (`install`, `upgrade`, `uninstall`, `status`, `config`, `history`, `rollback`, `doctor`, `sysdump`) default `--namespace` to `kairon-system`.

## Install via Krew

```bash
# From a release (once assets are published):
kubectl krew install kairon

# Local smoke-test from this repo:
make krew-package
kubectl krew install --manifest=dist/krew/kairon.yaml \
  --archive=dist/krew/kairon_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/').tar.gz

kubectl kairon version
kubectl kairon install --dry-run
```

Manifest: [`deploy/krew/kairon.yaml`](../deploy/krew/kairon.yaml). Packaging script: [`scripts/krew-package.sh`](../scripts/krew-package.sh).

## Lifecycle (embedded Helm SDK)

No separate `helm` binary or `./charts/kairon` checkout required by default — the chart is baked into the binary. Pass `--helm-cli` to shell out to Helm 3 on `$PATH`, or `--chart PATH` / `$KAIRON_CHART` for a filesystem chart.

```text
kaironctl install [--chart embedded|PATH|oci://REF] [--version V] [--profile evaluation|production]
                 [--namespace kairon-system] [--set k=v] [-f values.yaml]
                 [--wait] [--timeout 5m] [--dry-run] [--helm-cli]
                 [--helm-release-name kairon] [--create-namespace]
kaironctl upgrade  [same chart/profile/set/values/wait flags as install]
                 [--reset-values | --reuse-values] [--check]
kaironctl config view [--all] [-o yaml|json]
kaironctl config get KEY [-o yaml|json]   # dotted path, e.g. ui.enabled
kaironctl config set KEY=VALUE [KEY=VALUE...] [--chart PATH] [--wait] [--timeout 5m] [--dry-run] [--helm-cli]
kaironctl config unset KEY [KEY...] [same flags as config set]
kaironctl history [-o table|json|yaml]
kaironctl rollback [REVISION] [--wait] [--timeout 5m] [--dry-run]
kaironctl uninstall [--namespace kairon-system] [--force] [--wait] [--timeout 5m] [--dry-run] [--helm-cli]
kaironctl status [--namespace kairon-system] [--wait] [--timeout 5m] [--interactive] [-o json|yaml]
kaironctl version [--server] [-o text|json|yaml]
```

- **`upgrade` keeps your settings.** It reuses the values the release was last installed with (Helm's reset-then-reuse: the new chart's defaults, then your previous overrides, then this call's `--set`/`-f`). `--reset-values` restores the old reset-to-defaults behaviour; `--reuse-values` reuses the previous computed values verbatim. `install` re-runs keep Helm's reset semantics. `--dry-run` renders offline, so it cannot show previously stored values.
- **`config set`/`unset`** change single values through an in-place upgrade. They refuse when the chart this binary would apply is not the chart version the release runs; run `kaironctl upgrade` first.
- **`--profile production`** layers the chart's `values-production.yaml` under your `-f` and `--set`. Rollback restores the chart and values of a revision; it does not downgrade CRDs already applied.
- **`--chart oci://ghcr.io/zyvorai/charts/kairon --version 0.9.1`** installs the published chart. Apply CRDs from the same tag first when upgrading ([CRD versioning](guides/crd-versioning.md#upgrading-an-existing-cluster)).
- **`upgrade --check`** compares the installed chart, the chart this kaironctl would apply and the latest published release (GitHub, or `KAIRON_RELEASE_API`), prints the CRD-first reminder when a minor version is crossed, and changes nothing.
- **`config view`** prints the values you supplied; `--all` adds the chart defaults (computed values). **`config get`** reads one dotted path from the computed values. **`history`** lists the release revisions and **`rollback`** returns to `REVISION` (the previous one when omitted).
- **`status`** shows the release and exits non-zero while the control plane is not ready (with `--wait`, until it is or the timeout expires). **`version`** prints only the client version; `--server` adds the Helm release and the controller, node and UI images.

`uninstall` refuses while any Machine objects still exist unless `--force` is set. `--dry-run` on install renders manifests offline (no cluster needed).

## Diagnostics

```text
kaironctl doctor [--pre-install] [--strict] [-o json|yaml]
kaironctl selftest [--profile quick|standard|full] [--only IDS] [--skip IDS] [--list] [--no-vm]
                 [--image PATH | --image-url URL --image-digest sha256:...] [--bridge NAME] [--pvc NAME]
                 [--ui-url URL --ui-token TOKEN] [--keep] [--cleanup] [--strict] [--log-file PATH] [-o json]
                 # boots a CirrOS VM and checks it end to end: guides/selftest.md
kaironctl connectivity test [-n kairon-system] [--timeout 30s] [-o json|yaml]
kaironctl connectivity test --machine --image PATH [--test-namespace default] [--cpu 1] [--memory 512Mi]
                 [--backend qemu] [--network user] [--machine-timeout 3m] [--keep]
kaironctl logs controller|node|ui|csi-node|csi-controller [--node N] [--pod P] [-c C] [--tail N] [--since 15m] [--previous] [-f]
kaironctl events [-n NS | -A] [--warnings] [-w] [-o json|yaml]
kaironctl sysdump [-o FILE] [--tail N] [--no-logs] [--no-crs]
kaironctl diagnose machine NAME [--deep [--pcap 10s] [--pcap-filter F] [--memory-dump] [--no-redact] [--log-lines N] [-o FILE] [--timeout 20m]]
kaironctl diagnose memory-dump MACHINE TOKEN [-o FILE]
```

- **`doctor`** is read-only and prints a hint for every problem: API server, capable nodes (`kairon.zyvor.dev/capable=true`), the 19 core CRDs (present, `v1` storage), the Helm release, controller / node agent / dashboard workloads with `/readyz` probed on every pod through the API server proxy, the admission webhook (CA bundle, `failurePolicy`) and migrations parked in `NeedsRecovery`. It exits non-zero on any failure (`--strict`: on warnings too). `--pre-install` runs only the checks that apply before Kairon exists.
- **`connectivity test`** requests `/healthz` and `/readyz` of every Kairon pod (and the dashboard Service) through the API server, so it needs no port-forward. It does not exercise live migration. With `--machine --image PATH` it also runs an opt-in, **disruptive** end-to-end check: create a small Machine labelled `kairon.zyvor.dev/connectivity-test=true`, wait for `Running`, delete it and wait for the deletion. It schedules a real VM and counts against quotas; the Machine is deleted even when the check fails (`--keep` leaves it). `spec.ttlSeconds` is set as a best-effort safety net that FluxVM enforces on the node, so look for leftovers with `kaironctl get machines --selector kairon.zyvor.dev/connectivity-test=true`.
- **`logs`** streams every pod of a component; with several pods (the node agent runs one per node) lines are prefixed with the pod name.
- **Binary/systemd installs** (`scripts/deploy-remote.sh`) have no controller Deployment or node DaemonSet in the namespace, so `doctor` reports those components as warnings ("cannot verify from the API", with a `systemctl status` hint) rather than failures, and `connectivity test` skips pod probes. Machine lifecycle Events (`PhaseRunning`, `PhaseStopped`, ...) are recorded in the Machine's own namespace: read them with `kaironctl events -n <namespace>` or `-A`.
- **`sysdump`** writes a tar.gz with Helm values and manifest, workload and pod specs, events, nodes, Kairon resources, doctor output and recent logs. Values under credential-like keys, Secret data, container env values whose name looks secret, and Machine `spec.cloudInit` are replaced or omitted, and Secret documents are dropped from the manifest. Pod logs are included as written; review the bundle before sharing. It never overwrites an existing file.
- **`diagnose machine`** prints the Machine's ranked causes. With `--deep` it writes an incident bundle: the Machine, its events, migrations, Node and NodeTopology, plus FluxVM's bundle from the node (QMP state, cgroup and pressure, console log, dataplane, optional pcap) fetched through kairon-ui. `--memory-dump` also dumps guest memory on the node (QEMU; pauses the guest); `diagnose memory-dump` downloads it and checks its sha256. Needs `KAIRON_UI_URL` and an administrator token; secrets are redacted unless `--no-redact`. See [guides/machine-incident-bundle.md](guides/machine-incident-bundle.md).

## Dashboard

```text
kaironctl ui [--port N] [--no-open] [-n kairon-system] [--service kairon-ui] [--service-port 18082] [--backend-http]
```

Starts a proxy on `127.0.0.1` that forwards to the `kairon-ui` Service through the Kubernetes API server and opens it in a browser. No port-forward, Ingress or cluster-internal address is needed, and your kubeconfig credentials never reach the browser. The proxy is confined to that one Service path and refuses requests whose `Host` is not the loopback address; the dashboard still requires its own sign-in ([dashboard guide](guides/kairon-ui-dashboard.md)).

## Resources & power

```text
kaironctl get [machines|migrations|snapshots|restores|quotas|budgets|machinesets|machinepools|machineclaims|instancetypes|migrationpolicies|snapshotschedules|networkpolicies|securitygroups|backups|backuprestores|protectionplans|macnetworks|nodes|nodetopologies|acceleratorprofiles] [--selector k=v]
kaironctl describe [RESOURCE] NAME   # machine: adds an elastic-memory line from status.memory
kaironctl create NAME --image PATH [--cpu N] [--memory SIZE] [--backend qemu|…]
                 [--forward hostPort:guestPort[/proto]] [--hostname NAME] [--user NAME]
                 [--ssh-key KEY] [--package PKG] [--runcmd CMD] [--priority N]
kaironctl create machineset|machinepool|instancetype|migrationpolicy|snapshotschedule|quota|budget|networkpolicy|securitygroup NAME …
kaironctl create snapshotschedule NAME --selector k=v (--interval-seconds N | --daily-at HH:MM)
                 [--jitter-seconds N] [--max-age-seconds N]   # exactly one of interval / daily-at
kaironctl delete [RESOURCE] NAME
kaironctl delete RESOURCE --selector k=v [--dry-run]
kaironctl edit [machine|machineset|migrationpolicy|snapshotschedule|quota|budget|networkpolicy|securitygroup] NAME …
kaironctl scale machineset NAME --replicas N
kaironctl scale machineset --selector k=v --replicas N
kaironctl scale machinepool NAME --replicas N
kaironctl import ova SOURCE [--url URL] [--name NAME] [--no-repair] [--dry-run] [create flags]
kaironctl claim POOL [NAME] [--label k=v] [--retain] [--ttl 1h] [--allow-fqdn H] [--allow-sni N] [--allow-cidr C] [--allow-port P] [--allow-dns N] [--allow-icmp] [--wait 60s | --no-wait]
kaironctl fork MACHINE [--count N] [--prefix P] [--wait 60s | --no-wait]   # see guides/machine-fork.md
kaironctl start|stop|pause|resume|halt|hibernate MACHINE   # hibernate: guides/machine-hibernate.md
kaironctl disk attach|detach MACHINE NAME [--claim PVC]   # live, see guides/machine-hotplug.md
kaironctl disk list MACHINE
kaironctl nic add|remove MACHINE NAME [--bridge BR] [--mac MAC]
kaironctl nic list MACHINE
kaironctl top [machines|nodes] [--selector k=v] [-o json|yaml]
kaironctl trigger snapshotschedule NAME
kaironctl network status MACHINE [--flows] [--drop-reasons] [--limit N]
kaironctl network policies   # same as get networkpolicies
kaironctl network flows|drop-reasons|drops|stats|effective MACHINE [--limit N]
kaironctl network identity MACHINE
kaironctl network capture MACHINE [--seconds 1-30] [--filter EXPR] [--output FILE]
kaironctl network captures MACHINE
kaironctl network observe [-A] [--by reason|policy|machine] [--top N] [--limit N] [--follow] [--interval 5s] [-o json|yaml]
```

`get RESOURCE`, `describe RESOURCE NAME` and `top` accept `-o json|yaml` (`get` also `-o name`); without `-o` the output is unchanged. With `-o`, `describe` prints the stored object for every kind, including those whose default `describe` is a derived view.

`network observe` ranks the attributed drops of every Running Machine (reason, policy or Machine) from kairon-ui; it needs `KAIRON_UI_URL` (and `KAIRON_UI_TOKEN` when kairon-ui authenticates). It defaults to `--by reason --top 10 --limit 200` (`--limit` is the drop records requested per Machine); `--follow` repeats every `--interval` until interrupted. The drop payload is FluxVM's: `reason`, `policy` and `count`/`packets` fields are recognised, anything else counts as one drop with an unknown reason. Use `network drops MACHINE` for the raw records.

### Network status

`kaironctl network status MACHINE` prints emoji lines for FluxVM dataplane
attach/mode, Cilium ExternalWorkload identity/IP (when `ciliumAttach` is set),
and any matching `MachineNetworkPolicy` with `spec.cilium.sync`. `--flows` /
`--drop-reasons` call the existing uiapi pass-through when `KAIRON_UI_URL`
(and optionally `KAIRON_UI_TOKEN`) is set.

### VM edge

- `kaironctl network drops MACHINE` — drops recorded by FluxVM's VM edge,
  with Kairon's reason names (`spoof_ip`, `spoof_mac`, `dns_deny`,
  `sni_deny`, `rate_limit`, `policy_deny`, `default_deny`, `malformed`,
  `migration`), the policy name, flow and packet count. `--limit`
  defaults to 10. Needs `KAIRON_UI_URL`.
- `kaironctl network identity MACHINE` — the Machine's stable edge
  identity (FNV-1a of namespace and name). Computed locally; no cluster
  access needed.
- `kaironctl network capture MACHINE --seconds 15 [--filter EXPR] [--output FILE]`
  — starts a tcpdump capture (1-30 seconds) on the Machine's VM edge in
  FluxVM. With `--output`, waits for it to finish and writes the pcap.
  Needs `KAIRON_UI_URL`; without it the session is only printed.
- `kaironctl network captures MACHINE` — capture sessions with their
  state (`running`, `done`, `failed`, `interrupted`) and packet counts.

See [ebpf-edge.md](ebpf-edge.md).

## Migrate / recover / fence / snapshot

```text
kaironctl migrate MACHINE --strategy auto|cold|live --target-node NODE
kaironctl evacuate NODE [--strategy cold|auto] [--wait] [--timeout 15m] [--poll-interval 10s]
kaironctl recover MIGRATION --action ACTION --diagnosis DIAGNOSIS --reason REASON
kaironctl cancel-migration MIGRATION
kaironctl fence MACHINE --reason REASON
kaironctl node fence NODE --reason REASON | --clear   # attestation for --stale-evacuation
kaironctl node topology NODE                         # NUMA nodes, devices and the Machines holding them
kaironctl snapshot MACHINE [--name NAME] [--class CLASS] [--volume NAME]...
kaironctl restore SNAPSHOT --target-claim NAME
kaironctl volumes MACHINE [-n NS] [-o table|json]
kaironctl backup create MACHINE [--name NAME] [--quiesce auto|required|never] [--atlas] [--atlas-bucket ID] [--keep N]
kaironctl backup list
kaironctl backup restore BACKUP [--machine NAME] [--storage-class NAME]   # Machine must be halted
kaironctl backup delete NAME
kaironctl backup create MACHINE --s3-endpoint URL --s3-bucket NAME --s3-credentials SECRET [--encryption-key SECRET] [--immutable-for 30d]
kaironctl backup create MACHINE --parent BACKUP [--mode incremental|synthetic-full]   # S3 settings from the parent
kaironctl backup create MACHINE --policy POLICY [--mode full|incremental]
kaironctl backup chain MACHINE
kaironctl backuppolicy create NAME --selector k=v --incremental-every 1h --s3-endpoint URL --s3-bucket NAME --s3-credentials SECRET [--hourly N] [--daily N] [--monthly N]
kaironctl backuppolicy get [NAME] | describe NAME | suspend NAME | resume NAME | delete NAME
```

`get`, `describe` and `delete` also take `backup`, `backuprestore` and
`backuppolicy`. See [guides/machine-backup.md](guides/machine-backup.md) and,
for incremental S3 backups,
[guides/machine-backup-incremental.md](guides/machine-backup-incremental.md).

### Cross-cluster DR (experimental)

```text
kaironctl dr get [PLAN] [-n NS]
kaironctl dr status PLAN [-n NS]                 # restore points, runs, last/next test, DNS, conditions
kaironctl dr failover PLAN [--planned] [-n NS]   # on the standby; --planned also fails back
kaironctl dr test PLAN [-n NS]                   # copies on the isolated test bridge
kaironctl dr test-end PLAN [-n NS]
```

See [guides/cross-cluster-dr.md](guides/cross-cluster-dr.md).

### Macs (experimental)

```text
kaironctl mac nodes                       # chip, cores, memory, macOS, Rosetta, nested, Thunderbolt, fabric
kaironctl mac networks [-n NS]            # MacNetworks: subnet, port, MTU, key epoch, members
kaironctl mac network NAME [-n NS]        # each member's host slice, address and underlay
```

See [guides/mac-fabric.md](guides/mac-fabric.md).

## Images

```bash
kaironctl image upload FILE [--name NAME] [--format qcow2|raw|ova|vmdk|vhd|vhdx] [--replace]
kaironctl image list
kaironctl image delete NAME
```

`upload` streams a disk image to kairon-ui's image store
(`ui.imageStore.enabled`) and prints the `spec.image` block to boot it.
Needs `KAIRON_UI_URL` and an admin `KAIRON_UI_TOKEN` session token. See
[guides/machine-image-import.md](guides/machine-image-import.md#uploading-images).

`create snapshotschedule` takes exactly one of `--interval-seconds` (minimum 60)
or `--daily-at HH:MM` (UTC). `--jitter-seconds N` adds a stable per-schedule
offset in `[0, N)` so many schedules do not fire together, and
`--max-age-seconds N` prunes the schedule's own snapshots older than N seconds
(the newest is always kept). See
[guides/machine-snapshot-schedules.md](guides/machine-snapshot-schedules.md).

## Fleet automation (experimental)

```text
kaironctl fleet resources                     # list the fleet resource kinds
kaironctl fleet get RESOURCE                  # list one kind
kaironctl fleet create FILE.json              # create/apply one fleet object
kaironctl fleet delete RESOURCE NAME
kaironctl fleet approve-action REQUEST.json   # approve an exact MCP action with your Kubernetes identity
kaironctl fleet release-address NETWORK CLAIM-UID   # release a bridge-backed IPAM address
```

Opt-in; see [guides/enterprise-fleet.md](guides/enterprise-fleet.md) and the
per-kind [reference](guides/enterprise-fleet-reference.md).

## AI agents (MCP)

```bash
kaironctl mcp serve                 # read tools only
kaironctl mcp serve --allow-write   # also power, snapshot, capture, and gated destructive tools
kaironctl mcp serve --allow-write --require-approval=false   # skip human approval (default: required)
        [--audit-log PATH] [--audit-configmap NAMESPACE/NAME]
kaironctl approve KIND/[NAMESPACE/]NAME ID [--ttl 10m]       # KIND: machine | machinebackup
```

`kaironctl mcp serve` is a Model Context Protocol server on stdin/stdout for
[Hermes Agent](https://github.com/NousResearch/hermes-agent) and other MCP
clients. Read tools: `list_machines`, `get_machine`, `list_network_policies`,
`machine_network`, `machine_edge_identity`, `machine_volumes`,
`get_machine_snapshot`, `list_machine_pools`, `list_backups`, `list_claims`,
`describe_claim`, `diagnose`, `ask`, `replay_audit`, and the agent-plane
compilers (`compile_network_policy`, `validate_agent_claim`, `explain_drops`
and others; they never apply). Write tools, only with `--allow-write`:
`set_power_state`, `create_snapshot`, `snapshot_volume`, `network_capture`,
`claim_machine`, `release_claim`, `fork_machine`, `delete_machine`, `machine_disk`, `machine_nic`,
`machine_backup`, `create_sealed_claim`, `apply_network_policy`,
`apply_claim_step`, `audit_record`. Write calls go to a hash-chained audit
log (`--audit-log`, default `~/.kairon/audit.jsonl`; `--audit-configmap
ns/name` mirrors it) and are refused if it cannot be written.
`delete_machine`, `fork_machine` and `machine_backup` restore/delete also need
a human's `kaironctl approve KIND/NAMESPACE/NAME ID` per call (off with
`--require-approval=false`). `approve` writes a single-use
`kairon.zyvor.dev/mcp-approval` annotation that the agent's identical retry
consumes; it expires after `--ttl` (default 10m). The approver is
`$KAIRON_APPROVER`, else the local user name. Other environment variables:
`KAIRON_MCP_TENANT` scopes claim tools to a tenant, `KAIRON_MCP_PRINCIPAL`
names the caller in the audit log, and `KAIRON_MCP_APPROVAL_MODE=resource`
switches to resource-bound approvals. It
uses `KAIRON_KUBE_*` for Machines and `KAIRON_UI_URL`/`KAIRON_UI_TOKEN` for
network data. See [ai-agents.md](ai-agents.md) for setup with Hermes and
other clients, and [guides/hermes-mcp.md](guides/hermes-mcp.md) for the reference.

## Agent plane

```bash
kaironctl agent compile-policy --file intent.json   # strict egress allowlist -> MachineNetworkPolicy
kaironctl agent explain-drops --file drops.json     # explain attributed eBPF drops
kaironctl agent step-claim --file claim.json        # bind / hold / wait / expire decision
kaironctl agent ask "let job-7 reach pypi for 2h" --tenant acme --ns ml
kaironctl agent diagnose machine/job-7 --ns ml [--no-ai]
kaironctl agent audit-verify [--file F] [--claim NAME] [--show]
```

Every `agent` command prints a proposal or a decision; none of them writes
to the cluster. `ask` and the summary in `diagnose` use any
OpenAI-compatible endpoint set by `KAIRON_LLM_URL`, `KAIRON_LLM_MODEL` and
optionally `KAIRON_LLM_API_KEY`; the model's answer is validated by the
same compilers before it is printed. `kaironctl agent --help` lists the
rest (`migration-claim`, `cpu-label`, `gateway`). The MCP server also offers the read-only `detect_edge_anomalies` (beacon, DNS-tunnel, SNI-spread and deny-burst findings) and `project_confidential` (sealed or not, from a node attestation report); neither applies anything. See
[guides/agent-plane.md](guides/agent-plane.md).

## Meta

```text
kaironctl version [--server]
kaironctl completion bash|zsh|fish|powershell
```

Shell completion is dynamic: after `get`, `describe`, `delete`, `edit` it completes resource kinds and then object names from the cluster (honouring `-n` and `--context`), and Machine names after `start`, `stop`, `network flows` and similar. Failures complete nothing rather than printing errors. Example: `source <(kaironctl completion zsh)`.

`create`/`scale`/`edit` cover the common flag-friendly fields only — richer fields still need `kubectl apply`/YAML.

Point at a cluster with, in order of precedence:

1. `--kubeconfig PATH` and/or `--context NAME` (global flags; accepted before or after the verb), which use any kubeconfig auth (client certificates, tokens, exec plugins);
2. `KAIRON_KUBE_URL` (+ `KAIRON_KUBE_TOKEN`, `KAIRON_KUBE_CA`, `KAIRON_KUBE_INSECURE`), e.g. after `kubectl proxy`;
3. the in-cluster service account;
4. the default kubeconfig (`$KUBECONFIG`, `~/.kube/config`).

Every command, including `get`, `status` and the Helm-backed `install`/`upgrade`, uses the same selection, so they always target one cluster.

**`kubectl kairon ...`** works identically once `kubectl-kairon` is on `$PATH` (Krew or `make build`).

---
