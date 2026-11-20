# User guide: kaironctl bulk scale by label selector

`kaironctl scale machineset NAME --replicas N` (see
[`machine-sets.md`](machine-sets.md)) always addressed exactly one named
`MachineSet`. That's fine for a single fleet, but scaling every `MachineSet`
that makes up an environment -- say, everything labeled `env=staging` --
down to zero before a maintenance window, or back up together afterward,
meant a shell loop around `kaironctl get -o` output or `kubectl scale -l`.
This adds the same `--selector` convention
[`kaironctl delete`](kaironctl-bulk-delete.md) and
[`kaironctl get`](kaironctl-get-selector.md) already have, to `scale`.

## Usage

```
kaironctl scale machineset --selector k=v [--selector k2=v2 ...] --replicas N [--dry-run] [--namespace NS]
```

`--selector` is repeatable; every key=value pair given must match (logical
AND, never OR), using the exact same label-matching rule
(`model.LabelsMatch`) `delete`/`get`'s own `--selector` already use.
`--replicas` sets the **same** target replica count on every matched
`MachineSet` -- this is deliberately narrower than `edit`'s per-object flags
(see below for why).

```
$ kaironctl scale machineset --selector env=staging --replicas 0 --dry-run
machineset/api (dry-run, not scaled)
machineset/web (dry-run, not scaled)
machineset/worker (dry-run, not scaled)

$ kaironctl scale machineset --selector env=staging --replicas 0
machineset/api scaled to 0 replicas
machineset/web scaled to 0 replicas
machineset/worker scaled to 0 replicas

$ kaironctl scale machineset --selector team=payments --selector tier=nightly --replicas 3
no machinesets matched selector; nothing to scale
```

Only `machineset` is supported with `--selector` (`scale machinepool NAME
--replicas N` exists but takes no selector) -- there is no kind switch to
generalize here the way `delete`'s `matchingNames`/`deleteByKindName` needs
one.

## Why `--selector` was added to `scale` but not to `edit`

`kaironctl edit` (migrationpolicy, snapshotschedule, quota, budget, machine)
each expose several independent fields per kind -- applying the same
`--max-concurrent` or `--priority` value to every object a selector matches
is a much less obviously safe or wanted operation than "scale everything
matching this selector to the same N," since those objects are far more
likely to genuinely need different values from each other. `scale` mutates
exactly one field (`spec.replicas`), so a bulk caller applying one target
value to every match is the natural, common case rather than an edge case
-- the same reasoning `kaironctl-bulk-delete.md` gives for why bulk deletion
made sense for `delete` (a single, uniform action -- "gone") but not
originally for a hypothetical bulk `edit`. `--selector` was intentionally
**not** added to `edit` for this reason.

## Why an empty selector refuses to run, on purpose

```
$ kaironctl scale machineset --selector "" --replicas 0
Error: invalid key=value "": want key=value
```

Same rule as bulk delete: `--selector` must resolve to a genuinely
non-empty key=value map, since `model.LabelsMatch` already treats an
*empty* selector as matching **nothing**, not everything. Without this
refusal, a mistyped or empty `--selector` would silently match zero
`MachineSet`s and look like a successful no-op instead of surfacing the
mistake. `--dry-run` is a second, independent safety net on top of that: it
runs the exact same selector-matching logic and lists precisely what a real
run would scale, without patching anything.

## Real limits today (first cut)

- **No transaction, no rollback.** Each match is patched one object at a
  time by calling the exact same single-object `kc.PatchMachineSet`
  `kaironctl scale machineset NAME --replicas N` already uses, in
  sorted-by-name order. If patching the third of five matches fails (a
  network blip, an RBAC denial, a validating-webhook rejection), the first
  two are already scaled, kaironctl exits non-zero immediately, and the
  remaining two are left untouched -- nothing retries or continues past the
  first error. Re-running the exact same command is the recovery path.
- **No `--all` shortcut.** There is deliberately no way to match "every
  `MachineSet`" without at least one real `--selector` pair -- see above.
- **One target value for every match**, not a per-object delta (e.g. no
  "scale each match by +1"). Matches with different current replica counts
  all end up at the same `N`.
- **Selector matching, not a general query language.** `--selector` is
  still `model.LabelsMatch`'s plain equality-per-key semantics (no
  set-based `in`/`notin`, no `!=`), the same limit every other selector in
  Kairon already has today.
- **No new RBAC.** This reuses exactly the `list`/`patch` verbs on
  `machinesets` `kaironctl scale` already needed -- nothing about the RBAC
  surface changes, and `scripts/check_rbac_coverage.py` was re-run to
  confirm.
