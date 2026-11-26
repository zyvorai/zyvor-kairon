# Runbook: the multi-host migration matrix, from preflight to a green nightly

The matrix (`scripts/hardware-migration-matrix.sh`) migrates a real Machine
between two lab hosts and records each case in
[`COMPATIBILITY.md`](COMPATIBILITY.md#migration-matrix). This page covers the
entry point that wraps it, the inputs it needs, and what is still missing for
the nightly [`hardware-migration.yml`](../.github/workflows/hardware-migration.yml)
run to come out green. Setting up the two hosts themselves is
[`runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md);
the runner and cluster access are [`runbook-lab-setup.md`](runbook-lab-setup.md).

## One entry point

```bash
make build                                     # bin/kaironctl
scripts/migration-matrix.sh env                # every input, and whether it is set here
scripts/migration-matrix.sh preflight --env-file=$HOME/.config/kairon/lab.env
scripts/migration-matrix.sh run --profile=core --env-file=$HOME/.config/kairon/lab.env
```

Or with make (`LAB_ENV` is optional; without it the inputs come from the
environment):

```bash
make migration-preflight LAB_ENV=~/.config/kairon/lab.env            # PROFILE=core by default
make migration-matrix    LAB_ENV=~/.config/kairon/lab.env PROFILE=full MATRIX_FLAGS=--write-compat
make migration-audit-ci                                              # workflows vs. the input table
```

| Profile | Cases | Rows it can turn green |
|---|---|---|
| `core` | `cold`, `live`, `live-ebpf` | Cold migration, Live pre-copy under load, Live + eBPF dataplane (the ROADMAP item) |
| `full` | every case the matrix knows | every row of the migration matrix table |

`--case=NAME` (repeatable) picks cases by hand. The env file is plain
`KEY=value` lines; it holds the kube token, so keep it mode 0600:

```bash
KAIRON_HW_LAB=1
KAIRON_KUBE_URL=https://127.0.0.1:6443
KAIRON_KUBE_TOKEN=...            # kairon-lab-drills ServiceAccount token
KAIRON_KUBE_CA=/home/ghrunner/kube-ca.pem
KAIRON_HW_NAMESPACE=kairon-lab
KAIRON_HW_MACHINE=hwm-live
KAIRON_HW_TARGET_NODE=nldw4-4-16-36
KAIRON_HW_EBPF_MACHINE=hwm-ebpf
KAIRON_UI_URL=https://127.0.0.1:27231
KAIRON_UI_TOKEN=...
```

### What preflight checks

Read-only; it creates nothing and migrates nothing.

- `curl`, `python3`, `kaironctl`, and `KAIRON_HW_LAB=1`.
- The Kubernetes API answers `/version` with the token and CA.
- `KAIRON_HW_MACHINE` exists, is Running, Ready, has a guest IP, is not a
  `vz` Machine, and sits on a different node from `KAIRON_HW_TARGET_NODE`;
  both Nodes are Ready.
- kairon-ui accepts `KAIRON_UI_TOKEN` (`GET /api/v1/machines/NS/NAME`) and the
  Machine has `spec.guestAgent.enabled`, which the live and adaptive cases
  need to start the guest memory load.
- `KAIRON_HW_EBPF_MACHINE` has `dataplaneMode: ebpf` (and warns without
  `dataplaneRequired: true`) and is healthy.
- Each `*_SSH` target takes BatchMode SSH and `sudo -n`.
- `KAIRON_HW_POSTCOPY_MACHINE` exists.

A hard failure (the matrix cannot run) exits 1. A case whose inputs are
missing is listed as `skip ... would be recorded "not run"`, which is not a
failure unless `--strict` is given: a strict pass means the profile can come
out fully green. With `--github-summary` the result also goes to the job
summary and each "not run" case becomes a workflow warning.

## Inputs

`scripts/migration-matrix.sh env --markdown` prints this table from the
script itself, so the two cannot drift; `audit-ci` (run by the workflow's
`validate` job) fails when a workflow references a `KAIRON_*` secret or var
that is not in the table, or when a matrix input is not passed to the
`matrix` job.

| Name | GitHub kind | Needed by | Default | Meaning |
|---|---|---|---|---|
| `KAIRON_HW_LAB` | var | required |  | 1 acknowledges a real multi-host lab (safety gate) |
| `KAIRON_KUBE_URL` | secret | required |  | Kubernetes API URL reachable from the runner |
| `KAIRON_KUBE_TOKEN` | secret | required |  | token of the kairon-lab-drills ServiceAccount |
| `KAIRON_KUBE_CA` | secret | required |  | cluster CA: PEM content in GitHub (the workflow writes it to a file), a file path locally |
| `KAIRON_HW_MACHINE` | var | required |  | existing qemu Machine the matrix migrates back and forth |
| `KAIRON_HW_TARGET_NODE` | var | required |  | the other node (must differ from the Machine's node) |
| `KAIRON_HW_NAMESPACE` | var | optional | `default` | namespace of the lab Machines (lab: kairon-lab) |
| `KAIRON_UI_URL` | var | core |  | kairon-ui base URL: guest load for live/adaptive, guest-agent exec |
| `KAIRON_UI_TOKEN` | secret | core |  | admin kairon-ui bearer token for KAIRON_UI_URL |
| `KAIRON_HW_EBPF_MACHINE` | var | core |  | Machine with spec.network.dataplaneMode=ebpf and dataplaneRequired=true (live-ebpf row) |
| `KAIRON_HW_IMAGE` | var | full |  | image path on the nodes for the create/stop/start/delete smoke |
| `KAIRON_HW_SOURCE_SSH` | var | full |  | user@host of the source node (failure drills; passwordless SSH + sudo for iptables/systemctl/pkill) |
| `KAIRON_HW_TARGET_SSH` | var | full |  | user@host of the target node (same requirements) |
| `KAIRON_HW_POSTCOPY_MACHINE` | var | full |  | disposable Machine for the post-copy source-failure drill (recreated before each run) |
| `KAIRON_HW_CONTROLLER_SSH` | var | optional |  | user@host to restart a systemd kairon-controller instead of deleting its Pods |
| `KAIRON_HW_CONTROLLER_NAMESPACE` | var | optional | `kairon-system` | namespace of the controller Pods |
| `KAIRON_HW_CONTROLLER_UNIT` | var | optional | `kairon-controller` | systemd unit for KAIRON_HW_CONTROLLER_SSH |
| `KAIRON_HW_SOURCE_NODE` | var | optional | `Machine's node at start` | first node of the pair |
| `KAIRON_HW_NODE_UNIT` | var | optional | `kairon-node` | kairon-node systemd unit (source-failure drill) |
| `KAIRON_HW_SOURCE_NODE_UNIT` | var | optional | `KAIRON_HW_NODE_UNIT` | kairon-node unit on the source host |
| `KAIRON_HW_TARGET_NODE_UNIT` | var | optional | `KAIRON_HW_NODE_UNIT` | kairon-node unit on the target host |
| `KAIRON_HW_SMOKE_FLAGS` | var | optional |  | extra kaironctl create flags for the smoke Machine (e.g. --guest-agent --runcmd=true) |
| `KAIRON_HW_REQUIRE_GUEST_IP` | var | optional | `1` | healthy also needs status.guestIP |
| `KAIRON_HW_TIMEOUT` | var | optional | `600` | per-migration timeout, seconds |
| `KAIRON_HW_LOAD_MIB` | var | optional | `256` | guest RAM the live case rewrites |
| `KAIRON_HW_ADAPTIVE_LOAD_MIB` | var | optional | `1024` | guest RAM the adaptive cases rewrite |
| `KAIRON_HW_ADAPTIVE_BANDWIDTH_MBPS` | var | optional | `200` | starting bandwidth cap for the adaptive cases |
| `KAIRON_MIGRATION_PORT` | var | optional | `9443` | kairon-node migration port the recovery drill firewalls |

Ten of these were read by the scripts but never passed by the workflow, so
the nightly always ran with their defaults: `KAIRON_HW_SOURCE_NODE`,
`KAIRON_HW_SMOKE_FLAGS`, `KAIRON_HW_REQUIRE_GUEST_IP`, `KAIRON_HW_TIMEOUT`,
`KAIRON_HW_NODE_UNIT`, `KAIRON_HW_SOURCE_NODE_UNIT`,
`KAIRON_HW_TARGET_NODE_UNIT`, `KAIRON_HW_CONTROLLER_NAMESPACE`,
`KAIRON_HW_CONTROLLER_UNIT` and `KAIRON_MIGRATION_PORT`. They are wired now
(all optional; unset keeps the default).

## What is still missing for a green nightly

Recorded state (from [`COMPATIBILITY.md`](COMPATIBILITY.md#lab-profile-zyvor)
and [`runbook-lab-setup.md`](runbook-lab-setup.md), not re-read from GitHub
when this page was written: listing repository secrets needs a signed-in
`gh` with admin rights, so run `scripts/migration-matrix.sh audit-ci` to get
the live list):

| Status | Names |
|---|---|
| Set | `KAIRON_HW_LAB` (var), `KAIRON_KUBE_URL`, `KAIRON_KUBE_TOKEN`, `KAIRON_KUBE_CA` (secrets) |
| **Missing, the nightly fails without them** | `KAIRON_HW_MACHINE`, `KAIRON_HW_TARGET_NODE` (vars): the matrix exits before the first case |
| **Missing for a green `core` profile** | `KAIRON_UI_URL` (var), `KAIRON_UI_TOKEN` (secret), `KAIRON_HW_EBPF_MACHINE` (var), plus `spec.guestAgent.enabled` on `KAIRON_HW_MACHINE` and an eBPF Machine to point at |
| **Missing for a green `full` profile** | the `core` ones, plus `KAIRON_HW_IMAGE`, `KAIRON_HW_SOURCE_SSH`, `KAIRON_HW_TARGET_SSH`, `KAIRON_HW_POSTCOPY_MACHINE` (vars) and a lab job that recreates the post-copy Machine before each run |
| Documented, value not recorded | `KAIRON_HW_NAMESPACE` (`kairon-lab` per the lab runbook) |
| Optional | `KAIRON_HW_CONTROLLER_SSH` only if the controller runs under systemd; every other optional input has a working default |

Not a secret, and not fixed by setting one: the **Live pre-copy under load**
row failed on 2026-10-14 (plain pre-copy with 256 MiB rewritten at about the
link's own throughput never converged). With the inputs set, the `live` case
will still fail on that lab until the load (`KAIRON_HW_LOAD_MIB`), the link,
or the case's strategy changes; the adaptive rows pass on the same lab.

Set values by piping, never on a command line, for example
`printf %s "$TOKEN" | gh secret set KAIRON_UI_TOKEN` and
`gh variable set KAIRON_HW_MACHINE --body hwm-live`.

## Proving live migration on the lab

1. Bring the two hosts up with the adapter: either
   [`runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md)
   (systemd units), or the Helm chart with `migration.adapter.mode=sidecar`
   (see [migration TLS](guides/machine-migration-tls.md)).
2. Create `KAIRON_HW_MACHINE` with `spec.guestAgent.enabled: true` on a
   shared-storage image, and `KAIRON_HW_EBPF_MACHINE` with
   `spec.network.dataplaneMode: ebpf` and `dataplaneRequired: true`.
3. `scripts/migration-matrix.sh preflight --profile=core --strict` until it
   says `READY: every 'core' case has its inputs`.
4. `scripts/migration-matrix.sh run --profile=core --write-compat`, review
   the `docs/COMPATIBILITY.md` diff, and commit it with the run's date.
5. Set the same values as repository vars/secrets and trigger
   `hardware-migration` (`gh workflow run hardware-migration -f profile=core -f strict=true`);
   the nightly uses `profile=full`.
6. Check `status.dataPlaneEncrypted: true` on the live migration
   (`kaironctl get migrations -n kairon-lab -o yaml`) when data-plane TLS is on.
