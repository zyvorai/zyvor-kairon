# User guide: kaironctl bulk delete by label selector

Every `kaironctl` verb before this one addresses exactly one named object:
`kaironctl delete machine my-vm`, `kaironctl delete snapshot nightly-1`, and
so on. That's fine for a single mistake, but cleaning up after, say, a
finished load test that created fifty `Machine`s all labeled
`purpose=loadtest`, or every `MachineSnapshot` a since-deleted
`MachineSnapshotSchedule` left behind, meant either scripting a shell loop
around `kaironctl get`'s output or reaching for `kubectl delete -l`. This
adds the same capability directly to `kaironctl delete`.

## Usage

```
kaironctl delete RESOURCE --selector k=v [--selector k2=v2 ...] [--dry-run] [-n NAMESPACE]
```

`RESOURCE` is required and explicit here — unlike `kaironctl delete NAME`'s
own "one positional arg defaults to `machine`" convenience (see
`resourceKindAndName`), bulk deletion is exactly the place a wrong default
would be most dangerous, so there is no default kind for this form.
`--selector` is repeatable; every key=value pair given must match (logical
AND, never OR) for a resource to be deleted, using the exact same
label-matching rule (`model.LabelsMatch`) `MachineDisruptionBudget.spec.selector`,
`MachineNetworkPolicy.spec.selector` and `TopologySpreadConstraint`'s own
`labelSelector` already use everywhere else in Kairon.

```
$ kaironctl delete machine --selector purpose=loadtest --dry-run
machine/loadtest-1 (dry-run, not deleted)
machine/loadtest-2 (dry-run, not deleted)
machine/loadtest-3 (dry-run, not deleted)

$ kaironctl delete machine --selector purpose=loadtest
machine/loadtest-1 deleted
machine/loadtest-2 deleted
machine/loadtest-3 deleted

$ kaironctl delete snapshotschedule --selector team=payments --selector tier=nightly
no resources matched selector; nothing to delete
```

Every kind `kaironctl delete NAME` already supports gets this for free:
`machine`, `migration`, `snapshot`, `restore`, `backup`, `backuprestore`,
`quota`, `budget`, `machineset`, `machinepool`, `machineclaim`,
`instancetype`, `migrationpolicy`, `snapshotschedule`, `networkpolicy`, and
`securitygroup` (same aliases as always, e.g. `vm`/`vms` for `machine`,
`securitygroups`/`networksecuritygroups` for `securitygroup`) — one
dispatch (`matchingNames`/`deleteByKindName` in
`internal/kaironctl/delete.go`), not a special case bolted onto one
kind.

## Why an empty selector refuses to run, on purpose

```
$ kaironctl delete machine --selector ""
Error: invalid key=value "": want key=value

$ kaironctl delete machine --selector
flag needs an argument: -selector
```

Once `--selector` is present it must resolve to a genuinely non-empty
key=value map (without it, `kaironctl delete machine` is the single-object
form, deleting a Machine literally named `machine`). This isn't just argument-count pickiness: `model.LabelsMatch`
already treats an *empty* selector as matching **nothing**, not
everything — the same fail-closed rule `MachineNetworkPolicy`/
`NetworkSecurityGroup` selectors rely on so a policy object with a blank
selector can never accidentally apply to every Machine in the namespace.
Without the explicit refusal above, a mistyped or empty `--selector` on
`delete` would silently match zero resources and look like a successful
no-op — easy to miss in a script — rather than surfacing the mistake
immediately as an error. `--dry-run` is a second, independent safety net
on top of that, not a replacement for it: it runs the exact same
selector-matching logic and lists precisely what a real run would delete,
without deleting anything.

## Real limits today (first cut)

- **No transaction, no rollback.** Each match is deleted one object at a
  time by calling the same single-object delete `kaironctl delete NAME`
  already uses, in sorted-by-name order, exactly as if you'd run
  `kaironctl delete KIND NAME` in a loop by hand. If deleting the third of
  five matches fails (a network blip, a finalizer stuck for an unrelated
  reason, an RBAC denial), the first two are already gone, kaironctl exits
  non-zero immediately, and the remaining two are left untouched — nothing
  retries or continues past the first error. Re-running the exact same
  command is the recovery path: everything already deleted no longer
  matches, so a re-run only ever retries what's left.
- **No `--all` shortcut.** There is deliberately no way to match "every
  object of a kind" without at least one real `--selector` pair — see
  above.
- **Selector matching, not a general query language.** `--selector` is
  still `model.LabelsMatch`'s plain equality-per-key semantics (no
  set-based `in`/`notin`, no `!=`), the same limit every other selector in
  Kairon already has today.
- **No new RBAC.** This reuses exactly the `get`/`list`/`delete` verbs
  `kaironctl` already needed per kind — nothing about the RBAC surface
  changes, and `scripts/check_rbac_coverage.py` was re-run to confirm.
