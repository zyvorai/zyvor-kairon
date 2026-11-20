# Agent plane

Kairon does not put a model in the reconcile loop. `internal/agentplane` compiles proposals. Admission and MCP call it. Apply stays with the operator or a later controller that only accepts the compiled object.

## Opt-in

A Machine is unchanged until it sets one of:

| Annotation | Effect |
| --- | --- |
| `kairon.zyvor.dev/agent-pool=true` | Requires `spec.tenant`, a sha256 image digest, `kairon.zyvor.dev/image-signature=cosign:sha256:<64 hex>`, `kairon.zyvor.dev/egress-allowlist`, and `dataplaneMode: ebpf` (defaulted when empty). |
| `kairon.zyvor.dev/require-tenant=true` | Requires a DNS-label `spec.tenant`. |
| `kairon.zyvor.dev/confidential` | `sev-snp` or `tdx` only. Node report verification is a separate call. |
| `kairon.zyvor.dev/gpu-count` | Whole-GPU claim. `kairon.zyvor.dev/live-migrate=true` is refused. |
| `kairon.zyvor.dev/gateway` | Name recorded for a Gateway binding built from `spec.network.forwards`. |

`KAIRON_MCP_TENANT`, when set, scopes `compile_network_policy` and `validate_agent_claim` to that tenant.

## MCP tools

Read tools, always offered:

- `compile_network_policy` — strict allowlist, `defaultAllow: false`, no bare `*`, no `0.0.0.0/0`.
- `explain_drops` — `spoof_ip`, `dns_deny`, `sni_deny`, `rate_limit`. `apply` is false.
- `explain_pending` — repeats scheduler reasons. Does not reschedule.
- `detect_edge_anomalies` — beacon, long DNS qname, SNI spread, deny burst.
- `validate_agent_claim` — TTL 30–86400, egress required, guest cannot hold `delete_machine`, `fork_machine`, `claim_machine`, `release_claim`, `set_power_state`.
- `propose_boot_repair` — virtio, VMware tools, disk names, ssh, cloud-init, virtio-win. Does not write a disk.
- `migration_claim` — green only when cold, live, live-ebpf, source-failure and controller-failover passed.

Write tool, `--allow-write` only:

- `audit_record` — hashes principal, tool, tenant, claim and diff into an id for replay.

### Cluster tools

Read, scoped to `KAIRON_MCP_TENANT` when set:

- `list_claims` — claims with pool, tenant, phase, bound Machine and TTL.
- `describe_claim` — one claim plus the `StepClaim` decision against live pool members.

Write, `--allow-write` only and audited:

- `create_sealed_claim` — runs `ValidateClaim` (tenant, TTL 30–86400, hypervisor, non-empty strict egress) before creating the MachineClaim. `snapshotOnRelease` sets the release snapshot annotation.
- `apply_network_policy` — compiles a `PolicyIntent` and creates or updates the MachineNetworkPolicy. A raw policy is never accepted.
- `apply_claim_step` — applies `expire` by deleting the claim. `bind`, `hold` and `wait` stay with kairon-controller.

The existing `list_machine_pools`, `claim_machine` and `release_claim` tools are unchanged.

### Audit log

With `--allow-write`, every write tool call is recorded twice in an append-only JSONL file: once before it runs (`outcome: intent`) and once after (`ok` or the error). If the intent record cannot be written, the call is refused. Each record carries `prevHash` and `hash`, so an edited, reordered or removed line fails verification, and every append is fsynced.

```
kaironctl mcp serve --allow-write --audit-log ~/.kairon/audit.jsonl [--audit-configmap kairon-system/kairon-audit]
kaironctl agent audit-verify [--claim job-1]
```

The principal is `KAIRON_MCP_PRINCIPAL`, else the OS user. `replay_audit` without `events` reads the verified file. `--audit-configmap` mirrors each record into an existing ConfigMap, keyed by record id.

## CLI

```
kaironctl agent compile-policy --file intent.json
kaironctl agent explain-drops --file drops.json
kaironctl agent migration-claim --file matrix.json
```

Nothing in these commands writes to the apiserver.

## Claim step

`StepClaim` decides the next tick for an existing `MachineClaim`. It does not bind the Machine itself.

| Action | When |
| --- | --- |
| `bind` | Pending, and a Running warm member matches tenant and hypervisor. |
| `wait` | Pending, nothing eligible. |
| `hold` | Bound, TTL still open. |
| `expire` | Bound, TTL elapsed. `snapshot` is set when `kairon.zyvor.dev/snapshot-on-release=true`. Reclaim is Delete or Retain. |

Empty claim egress is refused. A compiled policy name is returned when egress is set.

`kaironctl agent step-claim --file claim.json` and MCP `step_agent_claim` return the decision. MCP `anomaly_events` returns Warning events and does not emit them.

## Dashboard

Read-only routes on kairon-ui, no namespace param, no apiserver write:

- `POST /api/v1/agent/compile-policy`
- `POST /api/v1/agent/explain-drops`
- `POST /api/v1/agent/anomalies`
- `POST /api/v1/agent/cpu-label`

CPU label projection uses `kairon.zyvor.dev/pinnable-cpus` and only reports `changed` when the projected list differs. Confidential status is sealed only when the node report kind matches.

## Apply path

The controller now uses this package on the claim path:

- A warm Machine is bound only if `EligibleWarm` matches tenant and hypervisor.
- Release creates `MachineSnapshot/<claim>-release` when `kairon.zyvor.dev/snapshot-on-release=true`, then deletes or retains the Machine. A 409 on create is treated as already done.

Status without a CRD change is annotations from `StatusAnnotations`: confidential sealed/reason, gateway name, gpu count.

MCP tools that still do not write: `project_cpu_label`, `project_confidential`, `bind_gateway`, `replay_audit`.

CLI: `kaironctl agent cpu-label --effective 0-7 --reserved 0-1` and `kaironctl agent gateway --name agents --guest-port 22 --host-port 2201`.

Dashboard adds `POST /api/v1/agent/claims/step`, `/confidential`, `/gateway`.

## Assistant

Any OpenAI-compatible endpoint (OpenAI, Ollama, vLLM, LiteLLM) can propose agent-plane objects. Configure it with environment variables on whatever runs the request (kaironctl, `mcp serve`, or kairon-ui):

```
KAIRON_LLM_URL=http://localhost:11434/v1   # API base; /chat/completions is appended
KAIRON_LLM_MODEL=qwen2.5
KAIRON_LLM_API_KEY=...                      # optional
```

The model answers with one JSON proposal: `policy` (a `PolicyIntent`), `claim` (a `ClaimRequest`), `repair` (boot findings), or `explain`. The proposal goes through `CompilePolicy`, `ValidateClaim` or `ProposeRepair`. An invalid answer is sent back once with the validator error, and a second invalid answer is refused. The caller's tenant and namespace are forced onto the proposal, and a proposal naming another tenant is refused. `apply` is always `false`. Applying it is a separate step through an audited MCP write tool or an operator.

- `kaironctl agent ask "let job-7 reach pypi for 2h" --tenant acme --ns ml [--facts drops.json]`
- MCP `ask`, scoped by `KAIRON_MCP_TENANT`.
- `POST /api/v1/agent/ask` on kairon-ui (503 without a model), and the dashboard's Assistant page.

Text passed as facts is labeled as data in the prompt, but it is still model input: review a proposal before applying it.

## Diagnose

`Diagnose` ranks likely causes for a stuck or failed Machine or MachineMigration without a model: phase first, then failing conditions (an `Unschedulable` condition gets `ExplainPending` hints), then Warning events grouped by reason and sorted by count, then edge drops and boot findings. Each cause carries evidence and proposed next steps. When a model is configured it adds `aiSummary`; the ranked causes never change and nothing is applied.

- `kaironctl agent diagnose machine/job-7 --ns ml [--no-ai]`
- MCP `diagnose` (read-only, `ref` is `machine/NAME` or `migration/NAME`), scoped by `KAIRON_MCP_TENANT` through the Machine's tenant label.
- `GET /api/v1/agent/diagnose/{namespace}/{machine|migration}/{name}` on kairon-ui, namespace-checked like other namespaced routes (`?ai=false` skips the model), and the Diagnose card on the Assistant page.

## Image signatures

With `kairon-controller -cosign-public-key cosign.pub` (or `KAIRON_COSIGN_PUBLIC_KEY`, set by the chart from `agentPlane.cosignPublicKeySecret`), the admission webhook verifies every agent-pool Machine's image:

- The image must be `spec.image.source.oci` with `spec.image.digest`.
- The webhook reads the `sha256-<hex>.sig` tag from the same repository and checks each simple-signing layer's signature against the key (ECDSA, Ed25519 or RSA). The signed payload must name the exact digest.
- Anonymous registry pulls only, an 8 second budget, and verified digests are cached for 10 minutes.

Without a key the webhook keeps the `cosign:sha256:<hex>` shape check and the controller logs a warning at startup. Keyless signatures (Fulcio and Rekor) are not verified.

## Events and status

kairon-node turns each tick's attributed drop increments and recent flows into `Warning` Events on the Machine: `EdgeDrop` for spoof, DNS and SNI denies, and `EdgeAnomaly` for findings from `Detect`. An identical Event for the same Machine is suppressed for 10 minutes. kairon-node needs `create` on `events` for this; the chart and `deploy/rbac.yaml` grant it.

`EdgeAnomaly` also carries `baseline_deviation` findings. kairon-node keeps an exponentially weighted mean and variance per Machine (keyed by tenant, so tenants never share a baseline) of bytes, flow count, distinct destinations, distinct SNI and deny rate. After 30 samples, a metric more than 4 standard deviations above its mean is flagged; a flagged sample is folded in at a quarter weight so sustained exfiltration does not quickly become normal. This is statistics, not a model. `--edge-baseline-file` (`KAIRON_EDGE_BASELINE_FILE`) snapshots the state every 5 minutes so a restart keeps the warm-up; the systemd unit uses `/var/lib/kairon-node/edge-baseline.json`, and the default is memory only. Machines silent for an hour are dropped.

kairon-controller writes `kairon.zyvor.dev/confidential-sealed` and `kairon.zyvor.dev/confidential-reason` on Machines that request confidential compute. Sealed is `true` only when the node carries `kairon.zyvor.dev/confidential-capable=<kind>` and the attestation verifier has set `kairon.zyvor.dev/attestation-verified=<kind>` on the Machine. Only changed keys are patched.

## Confidential attestation

A Machine asking for `kairon.zyvor.dev/confidential: sev-snp|tdx` is sealed only after its report verifies:

1. kairon-node reads `/sys/module/kvm_amd/parameters/sev_snp` and `/sys/module/kvm_intel/parameters/tdx` every 10 minutes and sets or removes the node label `kairon.zyvor.dev/confidential-capable` (this needs `patch` on `nodes`, granted in the chart and `deploy/rbac.yaml`).
2. kairon-controller writes a random `kairon.zyvor.dev/attestation-nonce` on the Machine. The guest must put `SHA-512("kairon-attestation\0" + machine UID + "\0" + nonce)` in REPORT_DATA, so a report can neither seal another Machine nor be replayed.
3. kairon-node runs the attest tool in the guest through the guest agent (`spec.guestAgent.console: true`) and puts the base64 report on `kairon.zyvor.dev/attestation-report`. The default command is the `attest` tool from go-sev-guest or go-tdx-guest; `--attest-guest-command` (`KAIRON_ATTEST_GUEST_COMMAND`) replaces it, with `{kind}` and `{data}` (hex REPORT_DATA) filled in. Anyone may write the report annotation; it is only data until verified.
4. kairon-controller verifies with go-sev-guest (VCEK, ASK and ARK chain; missing certificates come from AMD KDS and are cached for a day) or go-tdx-guest (PCK chain in the quote against Intel's root), checks REPORT_DATA, and refuses debug-enabled guests. Success sets `kairon.zyvor.dev/attestation-verified=<kind>` and clears the nonce and report. Failure writes `kairon.zyvor.dev/attestation-error` and rotates the nonce. If the node loses the capability label, `attestation-verified` is removed.

Flags on kairon-controller: `-attestation-verify` (default true), `-attestation-allow-debug` (lab only), `-attestation-writers`. With the webhook enabled, only the kairon-controller service account (or the listed writers) may set `attestation-verified` or `attestation-nonce`; without the webhook, anyone who can patch the Machine can forge them, so enable it where sealing matters. TDX collateral (TCB info, QE identity) and CRLs are not fetched.

## Still outside this repo

A green live-migration claim still needs the Zyvor lab matrix. The attestation path is tested against the sample SEV-SNP report and TDX quote shipped with go-sev-guest and go-tdx-guest; a live run needs SNP or TDX hardware and a guest image with the attest tool.
