# User guide: kaironctl get --selector

`kaironctl delete RESOURCE --selector k=v` (see
[`kaironctl-bulk-delete.md`](kaironctl-bulk-delete.md)) added label-selector
matching to the write side of `kaironctl`, but the read side was left
behind: `kaironctl get` had no way to narrow its listing by label at all.
Previewing exactly which Machines a selector reaches -- before trusting it
to a bulk delete, or just to find every Machine one load test created --
meant piping `kaironctl get`'s full output through `grep`, or reaching for
`kubectl get -l` instead. This adds `--selector` to `kaironctl get` too, as
the read-side counterpart bulk delete was always missing.

## Usage

```
kaironctl get [RESOURCE] [--selector k=v] [--selector k2=v2 ...] [-n NAMESPACE]
```

`RESOURCE` keeps its existing default of `machine` when omitted (unlike
`delete RESOURCE --selector`, which requires an explicit kind -- see that
guide's own reasoning for why bulk *deletion* is the one place a wrong
default would be dangerous; a mere listing carries no such risk).
`--selector` is optional and repeatable; every key=value pair given must
match (logical AND, never OR), using the exact same `model.LabelsMatch`
rule as `delete --selector`, `MachineDisruptionBudget.spec.selector`, and
`MachineNetworkPolicy.spec.selector`. Omitting `--selector` entirely lists
every resource of that kind, exactly as `kaironctl get` has always behaved.

```
$ kaironctl get machine
NAME         NODE     PHASE     CPU  MEMORY  IP
loadtest-1   node-a   Running   2    4Gi     10.0.1.5
loadtest-2   node-b   Running   2    4Gi     10.0.1.6
web-1        node-a   Running   4    8Gi     10.0.1.7

$ kaironctl get machine --selector purpose=loadtest
NAME         NODE     PHASE     CPU  MEMORY  IP
loadtest-1   node-a   Running   2    4Gi     10.0.1.5
loadtest-2   node-b   Running   2    4Gi     10.0.1.6

$ kaironctl get machine --selector purpose=loadtest --selector env=staging
NAME  NODE  PHASE  CPU  MEMORY  IP
```

That last example matches nothing: `get`, unlike `delete`, prints just its
usual header row for an empty result (the same thing it has always printed
for a namespace with no resources of that kind at all) rather than growing
a special-cased "nothing matched" message of its own -- an empty list
reads the same way here regardless of *why* it's empty.

Every kind `kaironctl get` already lists gets this for free: `machine`,
`migration`, `snapshot`, `restore`, `backup`, `backuprestore`, `quota`,
`budget`, `machineset`, `machinepool`, `machineclaim`, `instancetype`,
`migrationpolicy`, `snapshotschedule`, `networkpolicy`, `securitygroup`, and
`node` (same aliases as always) -- one generic helper
(`selectorFilter` in `internal/kaironctl/helpers.go`), applied once per
kind's existing list-and-print case, not a special case bolted onto one
kind.

## Why this is a generic helper, not one filter function per kind

`kaironctl get`'s kinds each already have their own `List*Namespace`
call and their own per-kind print loop (see the file's own long `switch` in
`cmdGet`); the only thing they all share is that every item's labels live
at `.Metadata.Labels`. Go's generics can't express "any struct with a
`Metadata` field" structurally without reflection, so `selectorFilter[T
any](items []T, selector map[string]string, labels func(T)
map[string]string) []T` takes a small accessor closure instead -- one line
added per case (`items = selectorFilter(items, selector, func(m
model.Machine) map[string]string { return m.Metadata.Labels })`), reusing
the exact same `model.LabelsMatch` bulk delete's `matchingNames` already
calls, rather than either one near-identical filter function per kind or a
reflection-based one.

## Real limits today (first cut)

- **Selector matching, not a general query language.** Same limit
  `delete --selector` already has: `model.LabelsMatch`'s plain
  equality-per-key semantics only, no set-based `in`/`notin`, no `!=`.
- **No `-o wide`/`-o json` output mode.** `--selector` only narrows *which*
  rows print, in the exact same tab-separated columns `kaironctl get`
  already printed for that kind; it does not add a new output format.
- **No new RBAC.** This reuses exactly the same per-kind `list` calls
  `kaironctl get` already needed with no selector at all --
  `scripts/check_rbac_coverage.py` was re-run to confirm nothing changed.
