# User guide: AI agents over MCP (Hermes Agent)

`kaironctl mcp serve` is a [Model Context Protocol](https://modelcontextprotocol.io)
server on stdin/stdout. An MCP client such as
[Hermes Agent](https://github.com/NousResearch/hermes-agent) starts it as a
subprocess and can then list and inspect Machines, read VM-edge network data
and, if you allow it, change power state, take snapshots and run packet
captures.

FluxVM has a matching server, `fluxctl mcp serve`, for the VMs on one host
(see FluxVM's `docs/mcp.md`). Use both to give an agent the cluster view and
the host view. For end-to-end setup of both, scoped credentials, other MCP
clients and example workflows, see [AI agent integration](../ai-agents.md).
This page is the `kaironctl mcp serve` reference.

## Tools

Read tools are always offered:

| Tool | What it returns | Backend |
| --- | --- | --- |
| `list_machines` | Machines with phase, power state, node, guest IP, CPU and memory; all namespaces unless `namespace` is set | Kubernetes API |
| `get_machine` | One Machine's metadata, spec and status | Kubernetes API |
| `list_network_policies` | MachineNetworkPolicies | Kubernetes API |
| `machine_network` | `kind` = `network-effective`, `network-stats`, `network-flows`, `network-drops`, `network-drop-reasons` or `network-capture` (capture sessions); `limit` for flows and drops | kairon-ui |
| `machine_edge_identity` | The stable VM-edge identity | computed locally |
| `machine_volumes` | Each `spec.volumes` entry: source (`pvc`, `atlas-pvc`, `atlas-rbd`), claim, size, Atlas phase, backend id, error | Kubernetes API |
| `get_machine_snapshot` | A MachineSnapshot's phase and per-volume snapshots (CSI VolumeSnapshot or Atlas snapshot id) | Kubernetes API |
| `list_machine_pools` | MachinePools with warm size, ready and claimed counts | Kubernetes API |
| `list_backups` | MachineBackups and MachineBackupRestores with phase, node, size, quiesce result and Atlas backup ids | Kubernetes API |
| `list_claims`, `describe_claim` | MachineClaims with pool, tenant, phase and TTL; `describe_claim` adds the live bind/hold/wait/expire decision | Kubernetes API |
| `diagnose` | Ranked causes for a stuck or failed `machine/NAME` or `migration/NAME` from phase, conditions and Warning events, with next steps; plus a model summary when `KAIRON_LLM_URL` is set | Kubernetes API (+ model) |
| `ask` | One validated proposal (egress policy, sealed claim, boot repair or explanation) from the configured model; never applied | `KAIRON_LLM_URL` |
| `replay_audit` | Audit events for a claim, read from the server's verified audit log | local file |
| Agent-plane compilers | `compile_network_policy`, `validate_agent_claim`, `step_agent_claim`, `explain_drops`, `detect_edge_anomalies`, `anomaly_events`, `explain_pending`, `propose_boot_repair`, `project_cpu_label`, `project_confidential`, `bind_gateway`, `migration_claim`: pure functions over the arguments, `apply: false` | computed locally |

Write tools are offered only with `--allow-write`:

| Tool | Effect |
| --- | --- |
| `set_power_state` | Sets `spec.powerState` to `Running`, `Stopped`, `Paused` or `Halted`. |
| `create_snapshot` | Creates a MachineSnapshot (name generated unless `snapshotName` is set). |
| `snapshot_volume` | Snapshots one named volume (MachineSnapshot with `spec.volumeNames`); Atlas volumes use Atlas snapshots. |
| `network_capture` | Runs a 1-30 s tcpdump capture on the Machine's VM edge. With `output`, waits and writes the pcap to that path on the machine running kaironctl; otherwise returns the token. |
| `claim_machine` | Creates a MachineClaim against `pool` and waits up to `waitSeconds` (default 30) for it to bind; returns the Machine name and bind time. Optional `labels`, `retain`, `ttlSeconds`, and `egress` (an allowlist enforced for the claim's lifetime). |
| `release_claim` | Deletes a MachineClaim; its Machine is deleted too unless the claim was made with `retain`. |
| `fork_machine` | Forks a Running flux-vm Machine into `count` live children on the same node and waits up to `waitSeconds` (default 60) for them to run. See [machine-fork.md](machine-fork.md). |
| `machine_disk` | Adds (`attach`, with `claim`) or removes (`detach`) a `spec.disks` entry; kairon-node hot-attaches or unplugs the PVC disk. |
| `machine_nic` | Adds (`add`, with `bridge`) or removes (`remove`) a `spec.network.extraInterfaces` entry; kairon-node hot-adds or unplugs the NIC. |
| `machine_backup` | Creates a `MachineBackup` (`create`), a `MachineBackupRestore` into the halted Machine (`restore`, with `backup`), or deletes a backup (`delete`). |
| `delete_machine` | Deletes a Machine. Refuses MachineSet replicas (the set would recreate them). |
| `create_sealed_claim` | Creates a sealed MachineClaim after validation: tenant, TTL 30-86400, hypervisor, non-empty strict egress allowlist. |
| `apply_network_policy` | Compiles a PolicyIntent with the strict compiler, then creates or patches the MachineNetworkPolicy. Wildcards and empty allowlists are refused. |
| `apply_claim_step` | Runs the claim step against live pool members; only `expire` is applied (the claim is deleted). |
| `audit_record` | Appends one event to the audit log. |

Every write call is recorded in a hash-chained audit log before and after
it runs (`--audit-log`, default `~/.kairon/audit.jsonl`; `--audit-configmap
ns/name` mirrors it). If the log cannot be written, the call is refused.
`kaironctl agent audit-verify` checks the chain.

`KAIRON_MCP_TENANT` scopes the claim tools and `diagnose` to one tenant.

Migrate, exec, and edge changes other than `apply_network_policy` are not exposed.

Every tool takes `namespace` (default: `--namespace`, else `default`) and
`name` where it acts on one Machine. Unknown arguments are rejected, so a
mistyped field comes back as an error instead of being ignored.

## Human approval for destructive tools

With `--allow-write`, `delete_machine`, `fork_machine` and `machine_backup`
with `action: restore` or `delete` also need a human to approve each call
(turn this off with `--require-approval=false`). The first call is refused
with an approval id and the exact command to run:

```text
delete_machine needs a human's approval (id 3f9a1c0b7d2e). Ask them to run:
  kaironctl approve machine/prod/web-1 3f9a1c0b7d2e
then call delete_machine again with exactly the same arguments.
```

`kaironctl approve` writes the `kairon.zyvor.dev/mcp-approval` annotation
on the target Machine (or MachineBackup), valid for `--ttl` (default 10m);
the approver needs `patch` on that resource.
The agent's next identical call consumes it with an atomic test-and-remove
patch, so one approval allows exactly one call. The id is a hash of the
tool, its arguments and `KAIRON_MCP_PRINCIPAL`: different arguments need a
new approval. The audit log records `approval-required`,
`approval-expired` and `approved:<approver>` (`$KAIRON_APPROVER`, else the
approver's local user name).

This only holds while the agent's way in is MCP: the approval is an
annotation, so anything with `patch` on Machines or MachineBackups can
write one. Give the agent's own token no access beyond what the MCP server
needs, and keep shell access to that token away from the agent.

## Configure Hermes

Add the server to `~/.hermes/config.yaml` (or run `hermes mcp add`):

```yaml
mcp_servers:
  kairon:
    command: kaironctl
    args: ["mcp", "serve"]            # add "--allow-write" for power, snapshot, capture
    env:
      KAIRON_KUBE_URL: "https://127.0.0.1:6443"
      KAIRON_KUBE_TOKEN: "..."
      KAIRON_KUBE_INSECURE: "true"    # lab only; prefer KAIRON_KUBE_CA
      KAIRON_UI_URL: "https://127.0.0.1:18082"
      KAIRON_UI_CA: "/path/to/dashboard-cert.pem"   # a self-signed dashboard certificate
      KAIRON_UI_TOKEN: "..."
    timeout: 120
```

Then `/reload-mcp` in a running Hermes session. The tools appear as
`mcp_kairon_list_machines` and so on. A copy of this config is in
[`.hermes/config.example.yaml`](../../.hermes/config.example.yaml).

To keep an agent read-only even if someone adds `--allow-write`, filter the
tools on the Hermes side as well:

```yaml
    tools:
      exclude: [set_power_state, create_snapshot, snapshot_volume, network_capture, claim_machine, release_claim, delete_machine, fork_machine, machine_disk, machine_nic, machine_backup, create_sealed_claim, apply_network_policy, apply_claim_step, audit_record]
```

## Install from the MCP Registry (OCI)

`kaironctl mcp serve` is listed in the MCP Registry as
`io.github.zyvorai/kairon` and ships as the cosign-signed image
`ghcr.io/zyvorai/kairon-mcp`. Registry entries are published from
`server.json` by the `publish-mcp` job in `.github/workflows/release.yml` on
every `v*` tag; the image carries the `io.modelcontextprotocol.server.name`
label the registry uses to verify ownership. The image runs as nonroot and
its entrypoint is `kaironctl mcp serve` with no flags.

```json
{
  "mcpServers": {
    "kairon": {
      "command": "docker",
      "args": ["run", "-i", "--rm",
               "-e", "KAIRON_KUBE_URL", "-e", "KAIRON_KUBE_TOKEN",
               "ghcr.io/zyvorai/kairon-mcp:v0.10.2"],
      "env": { "KAIRON_KUBE_URL": "https://k8s.example:6443", "KAIRON_KUBE_TOKEN": "..." }
    }
  }
}
```

Writes are off by default: the image never bakes in `--allow-write`, so only
the read tools are registered. To opt in, append `--allow-write` after the
image name, and give the container a writable audit log (for example
`-v kairon-audit:/home/nonroot/.kairon`) because a write call is refused
when the audit log cannot be written. Destructive tools still need
`kaironctl approve`. Files named by `KAIRON_KUBE_CA` and `KAIRON_UI_CA` are
paths inside the container, so mount them.

## Credentials

| Variable | Needed for |
| --- | --- |
| `KAIRON_KUBE_URL`, `KAIRON_KUBE_TOKEN`, `KAIRON_KUBE_CA` / `KAIRON_KUBE_INSECURE` | Machine, policy, power and snapshot tools. In a Pod, the service account is used instead. |
| `KAIRON_UI_URL`, `KAIRON_UI_TOKEN` | `machine_network` and `network_capture`. kairon-ui needs diagnostics enabled (`KAIRON_NODE_CONSOLE_TOKEN` on kairon-ui and kairon-node). |
| `KAIRON_LLM_URL`, `KAIRON_LLM_MODEL`, `KAIRON_LLM_API_KEY` | `ask`, and the summary in `diagnose`. Any OpenAI-compatible endpoint (OpenAI, Ollama, vLLM, LiteLLM). |
| `KAIRON_MCP_TENANT`, `KAIRON_MCP_PRINCIPAL` | Tenant scoping for the claim tools, `diagnose` and `ask`; the principal recorded in the audit log (default: OS user). |

The agent can do whatever these credentials allow. Give it a Kubernetes
token bound to a Role that only has the verbs you want (for example `get`
and `list` on `machines`, plus `patch` only if power changes are allowed).
Tokens are read from the environment and never appear in tool results.

## Limits

- Tool output is capped at 64 KB; longer results are cut with a note to
  narrow the request (for example with `limit`).
- Each call times out after 30 s; a capture after its length plus 45 s.
- Only the tools capability is implemented (no MCP resources or prompts).

## Testing without Hermes

The server speaks newline-delimited JSON-RPC, so a pipe is enough:

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_machines","arguments":{}}}' \
  | kaironctl mcp serve
```

Logs go to stderr; stdout carries only protocol messages.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `kubernetes endpoint not configured` | Set `KAIRON_KUBE_URL` and `KAIRON_KUBE_TOKEN` in the server's `env`. |
| `set KAIRON_UI_URL to reach uiapi …` | Set `KAIRON_UI_URL` (and `KAIRON_UI_TOKEN`). |
| `HTTP 501: diagnostics are not enabled` | Set `KAIRON_NODE_CONSOLE_TOKEN` on kairon-node and kairon-ui. |
| A write tool says to start with `--allow-write` | Add `--allow-write` to `args`, then `/reload-mcp`. |
| A destructive tool says it needs a human's approval | Run the `kaironctl approve ...` command it prints, then repeat the call with the same arguments. |
| `audit log unavailable; write refused` | The audit log path is not writable or its chain is broken; fix `--audit-log` or run `kaironctl agent audit-verify`. |
| `no LLM configured` from `ask` | Set `KAIRON_LLM_URL` and `KAIRON_LLM_MODEL` in the server's `env`. |
| Hermes shows no `mcp_kairon_*` tools | Check that `kaironctl` is on Hermes' `PATH` (or use an absolute `command`), and run the pipe test above. |
