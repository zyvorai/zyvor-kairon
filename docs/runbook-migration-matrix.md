# Runbook: the multi-host migration matrix, from preflight to a green nightly

The matrix (`scripts/hardware-migration-matrix.sh`) migrates a real Machine
between two lab hosts and records each case in
[`COMPATIBILITY.md`](COMPATIBILITY.md#migration-matrix). This page covers the
entry point that wraps it, the inputs it needs, and what is still missing for
the nightly [`hardware-migration.yml`](../.github/workflows/hardware-migration.yml)
run to come out green. Setting up the two hosts themselves is
[`runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md);
the runner and cluster access are [`runbook-lab-setup.md`](runbook-lab-setup.md).

## Operator checklist

- [ ] Set the missing inputs: `make lab-secrets-checklist` ([checklist](#what-is-still-missing-for-a-green-nightly)).
- [ ] Rebuild the lab's FluxVM with zyvor-fluxvm#417 ([FluxVM receiver race](#fluxvm-receiver-race-fix-still-to-deploy-on-the-lab)).
- [ ] Read [Known limits](#known-limits) before judging a run.
- [ ] Follow [Next lab steps](#next-lab-steps) in order, until a run meets [What green looks like](#what-green-looks-like).

## One entry point

```bash
make build                                     # bin/kaironctl
scripts/migration-matrix.sh env                # every input, and whether it is set here
scripts/migration-matrix.sh run --dry-run --profile=full   # no network: what is missing, the plan
scripts/migration-matrix.sh preflight --env-file=$HOME/.config/kairon/lab.env
scripts/migration-matrix.sh run --profile=core --env-file=$HOME/.config/kairon/lab.env
```

Or with make (`LAB_ENV` is optional; without it the inputs come from the
environment):

```bash
make migration-preflight LAB_ENV=~/.config/kairon/lab.env            # PROFILE=core by default
make migration-matrix    LAB_ENV=~/.config/kairon/lab.env PROFILE=full MATRIX_FLAGS=--write-compat
make migration-dry-run   PROFILE=full                               # inputs only, no network
make migration-audit-ci                                              # workflows vs. the input table
```

`scripts/migration-matrix-preflight.sh` (added with the MachineSet pause
work) is the same thing as `scripts/migration-matrix.sh preflight --dry-run
--profile=full`; it takes the same flags and has no logic of its own.

### Dry run

`--dry-run` (on `preflight` or `run`) reads only the environment and
`--env-file`. It contacts nothing, so it works on a laptop or a PR runner.
Each unset input is printed with its GitHub kind, the rows it gates and the
command that sets it:

```text
MISSING KAIRON_UI_TOKEN  [GitHub secret, core]
        what:  admin kairon-ui bearer token for KAIRON_UI_URL
        gates: live, live-adaptive-converge, live-postcopy-switch, guest-agent
        set:   printf %s "$VALUE" | gh secret set KAIRON_UI_TOKEN   (locally: export KAIRON_UI_TOKEN=..., or put it in --env-file)
```

It then lists every case as `runs`, `recorded "not run" (reason)` or
`blocked`, and for `run` it prints the exact `hardware-migration-matrix.sh`
command. It exits 1 when a required input is missing, or when
`--strict` is set and a row would be "not run". A dry-run pass does not
mean the lab is healthy: run `preflight` without `--dry-run` for that.

`make migration-dry-run` runs `scripts/migration-matrix.sh run --dry-run`
with `PROFILE` (default `core`), `LAB_ENV` (an env file) and `MATRIX_FLAGS`:

```bash
make migration-dry-run                                   # core, inputs from the environment
make migration-dry-run PROFILE=full LAB_ENV=~/.config/kairon/lab.env
make migration-dry-run MATRIX_FLAGS=--strict             # also fail on any "not run" row
```

With nothing set, it prints a `MISSING` block for each of the six required
inputs and each `core` input. Every case is then `blocked`, and make
stops with exit status 2 (the script itself exits 1). This was checked on
2026-11-27 with an empty environment:

```text
dry run (core: cold live live-ebpf): inputs only; no cluster, kairon-ui or SSH contact, nothing migrates
MISSING KAIRON_HW_LAB  [GitHub var, required]
        what:  1 acknowledges a real multi-host lab (safety gate)
        gates: every case (the matrix refuses to start)
        set:   gh variable set KAIRON_HW_LAB --body 1   (locally: export KAIRON_HW_LAB=1)
...                                   (KAIRON_KUBE_URL/TOKEN/CA, KAIRON_HW_MACHINE, KAIRON_HW_TARGET_NODE,
                                       KAIRON_UI_URL, KAIRON_UI_TOKEN, KAIRON_HW_EBPF_MACHINE)
plan:
  cold: blocked (required inputs missing)
  live: blocked (required inputs missing)
  live-ebpf: blocked (required inputs missing)
  checks still needed against the lab: scripts/migration-matrix.sh preflight --profile=core
preflight (core: cold live live-ebpf): NOT READY: 6 failure(s)
would not start: set the required inputs above (MISSING ... required, or FAIL) first
make: *** [Makefile:156: migration-dry-run] Error 1
```

Once the required inputs are set, the run exits 0. Unset optional rows
show as `recorded "not run" (... unset)`, and the last line is
`would run: .../hardware-migration-matrix.sh --case=...`, the exact command
`make migration-matrix` would run.

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

Checked read-only on 2026-11-27 with `gh secret list` and `gh variable list`
on `zyvorai/zyvor-kairon` (names only, repository scope). Already set:
`KAIRON_HW_LAB` and `KAIRON_HW_NAMESPACE` (vars), and `KAIRON_KUBE_URL`,
`KAIRON_KUBE_TOKEN` and `KAIRON_KUBE_CA` (secrets). Every item below is
already read by `hardware-migration.yml`, so setting it is enough. Rerun
`scripts/migration-matrix.sh audit-ci` for the live list (environment-scoped
values are not covered).

Before and after setting values, `make migration-dry-run PROFILE=full`
(or `PROFILE=core`) prints what is still missing and the plan for each case,
without touching the lab. `make migration-preflight` then checks the lab
itself. Replace `VALUE` with the real value. The secret line reads it from
your shell, so it never appears on a command line.

[`scripts/lab-secrets-checklist.sh`](../scripts/lab-secrets-checklist.sh)
prints this checklist from the live repository state: one `gh secret set`
or `gh variable set` command per input that is still unset, with no values.
It takes `--profile=core|full` (default `full`), `--repo=OWNER/NAME`,
`--all` and `--offline`. It reads the same input table as
`migration-matrix.sh` and asks GitHub only for names, read-only.
`make lab-secrets-checklist` runs it; pass flags through `CHECKLIST_FLAGS`,
for example `make lab-secrets-checklist CHECKLIST_FLAGS="--profile=core --offline"`.

**The matrix will not start without these:**

- [ ] `KAIRON_HW_MACHINE` (var): needed for every case. Point it at a qemu
  Machine with `spec.guestAgent.enabled: true` on shared-storage disks.
  `gh variable set KAIRON_HW_MACHINE --body VALUE`
- [ ] `KAIRON_HW_TARGET_NODE` (var): needed for every case. It must be a
  different node from the one the Machine is on.
  `gh variable set KAIRON_HW_TARGET_NODE --body VALUE`

**Green `core` profile (cold, live, live-eBPF):**

- [ ] `KAIRON_UI_URL` (var): unblocks `live`, `guest-agent`,
  `live-adaptive-converge` and `live-postcopy-switch`.
  `gh variable set KAIRON_UI_URL --body VALUE`
- [ ] `KAIRON_UI_TOKEN` (secret): an admin kairon-ui token, needed for the
  same rows as `KAIRON_UI_URL`.
  `printf %s "$VALUE" | gh secret set KAIRON_UI_TOKEN`
- [ ] `KAIRON_HW_EBPF_MACHINE` (var): unblocks `live-ebpf`. It must name a
  Machine with `spec.network.dataplaneMode: ebpf` and
  `dataplaneRequired: true`.
  `gh variable set KAIRON_HW_EBPF_MACHINE --body VALUE`

**Green `full` profile (the nightly), on top of `core`:**

- [ ] `KAIRON_HW_IMAGE` (var): turns `smoke` into the full
  create/stop/start/delete row (without it, smoke passes on a health
  check only). `gh variable set KAIRON_HW_IMAGE --body VALUE`
- [ ] `KAIRON_HW_SOURCE_SSH` (var): unblocks `source-failure`,
  `needs-recovery` and `postcopy-source-failure`. It needs BatchMode SSH
  and passwordless sudo.
  `gh variable set KAIRON_HW_SOURCE_SSH --body VALUE`
- [ ] `KAIRON_HW_TARGET_SSH` (var): needed for the same three rows.
  `gh variable set KAIRON_HW_TARGET_SSH --body VALUE`
- [ ] `KAIRON_HW_POSTCOPY_MACHINE` (var): unblocks
  `postcopy-source-failure`. It must be a disposable Machine that the lab
  recreates before each run.
  `gh variable set KAIRON_HW_POSTCOPY_MACHINE --body VALUE`

**Optional:** set `KAIRON_HW_CONTROLLER_SSH` (var) only if kairon-controller
runs under systemd. Every other optional input has a working default (see
[Inputs](#inputs)). Ten of the optional inputs, such as `KAIRON_HW_TIMEOUT`,
reach the nightly only once the workflow wiring held back from #288 lands.
Until then, the nightly uses their defaults.

Setting every input still leaves two failures that no secret fixes; see
[Known limits](#known-limits).

## Known limits

- **Live pre-copy under load does not converge on the lab link.** On
  2026-10-14 the `live` case ran plain pre-copy while the guest rewrote
  256 MiB (`KAIRON_HW_LOAD_MIB`, default 256). The transfer and the dirty
  rate both came to about 930 Mbps, so it ran the full 600 s without
  converging ([COMPATIBILITY](COMPATIBILITY.md#migration-matrix), "Live
  pre-copy under load"). This is a limit of plain pre-copy at that link
  speed, not missing configuration. Any of these would get the row to
  converge:
  - Use the adaptive strategy. Auto-converge throttles the guest, and
    post-copy switches the guest to the target
    ([migration-policies](guides/migration-policies.md)). On the same lab,
    the `live-adaptive-converge` and `live-postcopy-switch` rows pass with
    1024 MiB of load.
  - Lighten the load: `KAIRON_HW_LOAD_MIB=64` and a larger
    `KAIRON_HW_TIMEOUT`
    ([multi-host test runbook](runbook-multi-host-migration-test.md)).
    The nightly already passes `KAIRON_HW_LOAD_MIB` through, but not
    `KAIRON_HW_TIMEOUT`. That waits on the held-back workflow wiring
    (`make migration-audit-ci` lists it as NOT WIRED).
  - Use a link that is clearly faster than the guest's dirty rate.

  The plain `live` row is meant to measure pre-copy alone, so it stays
  `fail` until the lab's link or load changes.
- **FluxVM receiver race (zyvor-fluxvm#417): the fix is not on the lab
  yet.** The fix is merged on FluxVM `main` but has not been released. Until
  the lab's FluxVM is rebuilt from `main`, every case that makes a live
  migration to a FluxVM receiver fails about 1 run in 10. For the deploy and
  verify steps, see
  [FluxVM receiver race](#fluxvm-receiver-race-fix-still-to-deploy-on-the-lab).
  The fix does not help the pre-copy limit above.

## FluxVM receiver race: fix still to deploy on the lab

Besides the inputs above, the lab's FluxVM needs a fix before the live rows
can be trusted. COMPATIBILITY's "Repeated live migration" check is
`partial`. One of its failures, about 1 in 10 live migrations, was a FluxVM
race, fixed in FluxVM but **not yet deployed to the lab**.

- **What:** FluxVM's scheduler `reconcile()` deletes a VM that has been
  `Creating` for longer than `STUCK_CREATING_GRACE` (300 s).
  `launch_adopt_receiver` inserts a migration receiver's placeholder as
  `Creating`, but with the *source* VM's `created_at`, so the grace period
  has already passed. The reaper skips the placeholder only once its
  receiver file exists, and that file appears seconds later, after QEMU and
  the network are up. A reconcile tick in that window deletes the receiver
  mid-launch. The source then finishes its transfer, the destination commit
  times out, and the destination runtime is `failed`. On the target host,
  each failure matched the journal line `cleaning up a VM stuck in Creating
  status — its creating process likely crashed`.
- **Fix:** [zyvorai/zyvor-fluxvm#417](https://github.com/zyvorai/zyvor-fluxvm/pull/417),
  "scheduler: do not reap a migration receiver placeholder mid-launch",
  merged 2026-10-15 (merge commit `e9f4cc94`). In
  `crates/fluxvm-scheduler/src/lib.rs` the stuck-`Creating` reaper now also
  skips VMs labelled `live_migration::MIGRATING_FROM_LABEL`
  (`fluxvm.dev/migrating-from`), which `launch_adopt_receiver` already sets.
  The change is on zyvor-fluxvm `main` (checked 2026-11-27). No FluxVM
  release tag contains it yet (the only tag is `mac-v0.1.0`), so the lab
  needs a FluxVM built from `main`.
- **Deploy:** on both lab hosts, build and install FluxVM from that `main`
  following FluxVM's own instructions (this repository does not install
  FluxVM), and restart `fluxvm.service` while no migration is in flight.
  Before building, check that the source has the fix:
  `grep -n 'MIGRATING_FROM_LABEL' crates/fluxvm-scheduler/src/lib.rs`
  should show the line in the `VmStatus::Creating` branch.
- **Verify:** run [`scripts/lab-live-soak.sh`](../scripts/lab-live-soak.sh)
  with `KAIRON_SOAK_HOPS=20 KAIRON_SOAK_CONTINUE=1` (plus the
  `KAIRON_HW_NODE_A/B` and `KAIRON_HW_SSH_A/B` inputs in its header). Every
  hop should `Succeed`. If a hop fails, its evidence file holds the last 5
  minutes of `fluxvm` journal on both hosts; a `stuck in Creating` line for
  the receiver means the fix is not running. `journalctl -u fluxvm | grep
  'stuck in Creating'` on the target during the soak should show no
  receiver VMs. Then update the "Repeated live migration" row and its
  "Not shown" note in [`COMPATIBILITY.md`](COMPATIBILITY.md).
- **What it unblocks:** the repeat/soak check, and matrix cases that make a
  live migration to a FluxVM receiver (`live`, `live-ebpf`,
  `live-adaptive-converge`, `live-postcopy-switch`, `live-zstd` and the
  post-copy failure drill). Without the fix, each of these fails about 1 run
  in 10 regardless of the inputs. The fix does not address the `live`
  row's separate 2026-10-14 failure (plain pre-copy not converging under
  load; see [Known limits](#known-limits)).

## What green looks like

**Row counts.** Each case writes one row, with a result of `pass`, `fail` or
`not run`. The cases come from `CORE_CASES` and `FULL_CASES` in
`scripts/migration-matrix.sh`:

| Profile | Cases | Green |
|---|---|---|
| core | 3: `cold`, `live`, `live-ebpf` (ROADMAP's matrix item) | 3 of 3 `pass` |
| full | 12: `smoke`, `cold`, `live`, `guest-agent`, `needs-recovery`, `source-failure`, `controller-failover`, `live-adaptive-converge`, `live-postcopy-switch`, `live-zstd`, `postcopy-source-failure`, `live-ebpf` | 12 of 12 `pass` |

The nightly calls `hardware-migration-matrix.sh` without `--case`. Its
built-in default is the same 12 cases, so today's nightly is the full
profile.

**Output.**
- During the run, each case prints `==> <row title>: <result> <notes>`.
- At the end, under `## Matrix rows`, it prints one
  `| <row title> | <result> | <date> | <notes> |` line per case.
- With `--write-compat`, those lines replace the rows with the same titles
  in [COMPATIBILITY's matrix](COMPATIBILITY.md#migration-matrix), dated
  with the run date. In the nightly, the updated file is the
  `compatibility-md` artifact.
- If any row is `fail`, the script prints `FAILED: N case(s)` to stderr and
  exits 1.

**Exit 0 is not the same as green.**
- A `not run` row doesn't fail the exit code.
- `smoke` without `KAIRON_HW_IMAGE` records `pass` but only checks that the
  Machine is Running and Ready (its note says so).
- `preflight --strict` rejects both.
- A `not run` row means an input is missing. The note names the input, and
  [the checklist](#what-is-still-missing-for-a-green-nightly) maps inputs to
  rows. The one exception is a missing drill script under `scripts/`. With
  every input set, no row should be `not run`.

**Expected failures with everything set, on today's lab**
([Known limits](#known-limits)):
- **`Live pre-copy under load`:** `fail` at the default 256 MiB load on
  the about 930 Mbps link, until the load or the link changes.
- **Until FluxVM #417 is on the lab:** any of the 6 live-to-receiver rows
  (`live`, `live-ebpf`, the 3 adaptive rows and `postcopy-source-failure`)
  can fail about 1 run in 10. In that case the target's `fluxvm` journal has
  a `stuck in Creating` line.

**What counts as green.**
- A run counts as green when every row is `pass` with that run's date,
  there is no `FAILED` line, `KAIRON_HW_IMAGE` is set, and FluxVM #417 is
  deployed and soak-verified. Core green (3 of 3) ticks ROADMAP's matrix
  item; full green (12 of 12) makes the nightly green.
- A full run with 11 `pass` and only `Live pre-copy under load` failing
  without converging is the expected state before the load or link
  changes. That run is **not** green. Commit its rows, but leave ROADMAP's
  "Live pre-copy under load passing" open. The adaptive rows don't count
  in its place.

## Next lab steps

Do these in order to get a green nightly. Each step links to the section
with the details.

- [ ] **Set the missing inputs.** Run `make lab-secrets-checklist` and run
  the `gh secret set` / `gh variable set` lines it prints
  ([checklist](#what-is-still-missing-for-a-green-nightly)).
- [ ] **Rebuild FluxVM from `main` with zyvor-fluxvm#417 on both hosts.**
  Then verify with `scripts/lab-live-soak.sh` at `KAIRON_SOAK_HOPS=20
  KAIRON_SOAK_CONTINUE=1`. Every hop should succeed, with no `stuck in
  Creating` lines for receivers
  ([FluxVM receiver race](#fluxvm-receiver-race-fix-still-to-deploy-on-the-lab)).
- [ ] **Dry run, then preflight.** Run `make migration-dry-run` until it
  reports no missing input ([Dry run](#dry-run)). Then run
  `make migration-preflight MATRIX_FLAGS=--strict` against the lab until it
  prints `READY` ([What preflight checks](#what-preflight-checks)).
- [ ] **Run core, then full.** Run `make migration-matrix` (core). Once that
  is clean, run `make migration-dry-run PROFILE=full`, then
  `make migration-preflight PROFILE=full`, then
  `make migration-matrix PROFILE=full`
  ([Proving live migration](#proving-live-migration-on-the-lab)).
- [ ] **Handle the `live` (pre-copy under load) row.** Expect it to fail at
  the lab's link speed. Lighten the load, use a faster link, or rely on the
  adaptive rows ([Known limits](#known-limits)).
- [ ] **Land the held-back workflow patch from #288.** This needs a token
  with `workflow` scope (`gh auth refresh -s workflow`). Afterwards,
  `make migration-audit-ci` should report no input as NOT WIRED, and the
  nightly takes `-f profile=core -f strict=true` and the optional inputs.
- [ ] **Record the results.** Rerun with
  `MATRIX_FLAGS=--write-compat` (or download the nightly's
  `compatibility-md` artifact). Commit the `docs/COMPATIBILITY.md` diff with
  the run date, and update the "Repeated live migration" row from the soak
  ([After a green run](#after-a-green-run)).

## After a green run

Do this once a run meets [What green looks like](#what-green-looks-like).

1. **Get the rows.**
   - Local run: add `--write-compat`
     (`make migration-matrix PROFILE=full MATRIX_FLAGS=--write-compat`).
     The script rewrites the matching rows in your checkout's
     `docs/COMPATIBILITY.md`.
   - Nightly: the workflow runs `--write-compat` on the runner but commits
     nothing. Download the updated file with
     `gh run download <run-id> -n compatibility-md`, then copy the matrix
     rows into your checkout.
2. **Update [`COMPATIBILITY.md`](COMPATIBILITY.md#migration-matrix).**
   - Only the rows of the cases you ran change. Each one gets the run's UTC
     date, and the other rows keep their old dates.
   - Add the lab details to the notes, as the 2026-10-14 rows do: hosts,
     QEMU and FluxVM versions, link speed.
   - If the soak passed, also update the "Repeated live migration" row and
     its "Not shown" note
     ([FluxVM receiver race](#fluxvm-receiver-race-fix-still-to-deploy-on-the-lab)).
   - Open one PR against `main` per run. The repo has no written
     commit-message convention; recent docs PRs use `docs(<scope>): …`, for
     example `docs(compat): migration matrix results YYYY-MM-DD`. Link the
     run in the PR body.
3. **Tick [`ROADMAP.md`](../ROADMAP.md).** Under "Green multi-host migration
   matrix", tick the lab vars/secrets item once preflight is `READY`.
   - Tick "Live pre-copy under load passing" only on a `pass` of that row.
   - Tick the parent item only on core green (3 of 3).
4. **Keep the logs.**
   - Each nightly uploads two artifacts: `compatibility-md`
     (`docs/COMPATIBILITY.md`) and `hardware-migration-matrix`
     (`matrix-out.txt`, the full console output, drill logs included). The
     run's step summary also shows the pass / fail / not run counts.
   - The repository keeps artifacts and logs for 90 days, the maximum. Fetch
     them with `gh run download <run-id>` before they expire.
   - Locally, nothing goes to a file unless you redirect the output, for
     example `… | tee matrix-out.txt`.
   - `scripts/lab-live-soak.sh` writes one
     `<migration>-<phase>.txt` per failed hop to `KAIRON_SOAK_EVIDENCE_DIR`
     (default `./soak-evidence`).
   - The repo has no archive location, and these paths aren't in
     `.gitignore`, so don't commit them. Attach them to the results PR or
     keep them with the lab's own records.

## Proving live migration on the lab

1. Bring the two hosts up with the adapter: either
   [`runbook-multi-host-migration-test.md`](runbook-multi-host-migration-test.md)
   (systemd units), or the Helm chart with `migration.adapter.enabled=true`
   (DaemonSet) or `migration.adapter.hostEnvFile=true` (systemd unit, Helm-driven)
   (see [migration TLS](guides/machine-migration-tls.md#let-helm-run-the-adapter)).
2. Create `KAIRON_HW_MACHINE` with `spec.guestAgent.enabled: true` on a
   shared-storage image, and `KAIRON_HW_EBPF_MACHINE` with
   `spec.network.dataplaneMode: ebpf` and `dataplaneRequired: true`.
3. `scripts/migration-matrix.sh preflight --profile=core --strict` until it
   says `READY: every 'core' case has its inputs`.
4. `scripts/migration-matrix.sh run --profile=core --write-compat`, review
   the `docs/COMPATIBILITY.md` diff, and commit it with the run's date.
5. Work through the [checklist](#what-is-still-missing-for-a-green-nightly)
   and trigger `hardware-migration` (`gh workflow run hardware-migration`).
   The `-f profile=core -f strict=true` inputs exist only once the workflow
   wiring held back from #288 lands; until then the run takes every case.
6. Check `status.dataPlaneEncrypted: true` on the live migration
   (`kaironctl get migrations -n kairon-lab -o yaml`) when data-plane TLS is on.
