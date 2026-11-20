# User guide: MachineSnapshotSchedule

Periodically creates a `MachineSnapshot` for every `Machine` matching a
label selector -- Kairon's first-cut backup-automation primitive, roughly
analogous to a Kubernetes `CronJob`, but for VM snapshots: either every
`intervalSeconds` or once a day at a fixed UTC time (`dailyAt`), rather
than full cron syntax (see "Real limits today" below for exactly what that
means).

## Example

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: MachineSnapshotSchedule
metadata:
  name: nightly
  namespace: prod
spec:
  selector: {tier: web}
  intervalSeconds: 86400
  keepLast: 7
```

This creates a new `MachineSnapshot` for every `Machine` in the `prod`
namespace labeled `tier: web` roughly once every 86400 seconds (24 hours).
A brand-new schedule fires on its very first reconcile tick after creation
-- it doesn't wait a full interval before its first run. `kaironctl get
snapshotschedules` (or `kubectl get machinesnapshotschedules`) lists what's
configured, including each schedule's `status.lastRunTime`/
`lastRunSnapshotCount`/`nextRunTime` -- see "When will it run next?" below
for exactly what `nextRunTime` does and doesn't promise.

Or, via `kaironctl`:

```console
$ kaironctl create snapshotschedule nightly --selector tier=web --interval-seconds 86400
snapshotschedule/nightly created
$ kaironctl edit snapshotschedule nightly --suspend true
snapshotschedule/nightly updated
$ kaironctl edit snapshotschedule nightly --suspend false --interval-seconds 43200
snapshotschedule/nightly updated
$ kaironctl edit snapshotschedule nightly --keep-last 7
snapshotschedule/nightly updated
$ kaironctl edit snapshotschedule nightly --starting-deadline-seconds 3600
snapshotschedule/nightly updated
```

Or from the dashboard: the **Snapshot schedules** page lists every schedule
in the `default` namespace with its interval, retention, and last-run
status, and a **Suspend**/**Resume** button toggles `spec.suspend` without
needing `kaironctl`/`kubectl` for that one action.

`--selector` is repeatable for a multi-label selector (`--selector tier=web
--selector env=prod`), same as `create migrationpolicy`. `edit` only patches
the flags you actually pass -- omitting `--interval-seconds` on an `edit`
call never resets it.

`spec.volumeSnapshotClassName`, if set, is passed straight through to every
`MachineSnapshot` this schedule creates, mirroring
`MachineSnapshot.spec.volumeSnapshotClassName` exactly.

## Previewing what would fire right now (`kaironctl describe`)

`describe` for every other kind in this project uniformly prints the raw
object as JSON and nothing else. `kaironctl describe snapshotschedule` is
one of only four deliberate exceptions (the others are [`kaironctl describe
migrationpolicy`](migration-policies.md#previewing-what-a-migration-would-get-right-now-kaironctl-describe),
[`kaironctl describe quota`](machine-quotas.md#previewing-usage-right-now-kaironctl-describe),
and [`kaironctl describe
budget`](machine-disruption-budgets.md#previewing-who-counts-right-now-kaironctl-describe)):
it prints that same JSON, then appends a preview of exactly what the *next*
reconcile tick would do with this schedule, right now:

```console
$ kaironctl describe snapshotschedule nightly
{
  "apiVersion": "kairon.zyvor.dev/v1",
  "kind": "MachineSnapshotSchedule",
  ...
}

Matching machines (2) -- due now, the next reconcile tick will snapshot these:
  web-1
  web-2
```

or, for a schedule that isn't due yet:

```console
$ kaironctl describe snapshotschedule nightly
...
Matching machines (2) -- not due yet (next projected run: 2026-09-17T02:00:00Z):
  web-1
  web-2
```

The machine list is exactly which `Machine`s in the schedule's own
namespace currently satisfy `spec.selector` -- the same `LabelsMatch` check
`reconcileMachineSnapshotSchedules` itself uses -- and the due/not-due
verdict comes from calling the schedule's own `Spec.Due(status.lastRunTime,
time.Now())`, the identical pure function the controller calls every
reconcile tick, so this preview can never disagree with what actually
happens next. A selector matching zero Machines prints an explicit `(none
-- check spec.selector against these Machines' own labels)` hint instead of
a bare, unexplained empty list -- useful for catching a typo'd label value
before assuming the schedule is simply working correctly with nothing to
do.

This is a real, useful question to ask before loosening or tightening a
selector, or before shortening/lengthening `intervalSeconds` -- without it,
the only way to find out was to wait for the next tick and check
`status.lastRunSnapshotCount` after the fact. It's a snapshot of *this
instant*, though, not a guarantee: a Machine's own labels, or the
schedule's `selector`/`suspend`/`intervalSeconds` fields, can change
between running `describe` and the actual next reconcile tick.

## Retention (`spec.keepLast`)

Set, `spec.keepLast: N` bounds how many of THIS schedule's own
`MachineSnapshot`s are kept **per Machine**: once a Machine has more than
`N` ready-to-use snapshots this exact schedule created, the oldest (by
`metadata.creationTimestamp`) are deleted right after each due run, once
the run's own new snapshot has been created. Unset (or `0`, the default)
never prunes anything -- snapshots accumulate forever, exactly this
project's behavior before `keepLast` existed.

Two things `keepLast` deliberately never touches, so it's safe to turn on
against an existing schedule with pre-existing snapshots:

- **A `MachineSnapshot` this schedule didn't create** -- one made by hand,
  by a script, or by a *different* `MachineSnapshotSchedule` is never a
  pruning candidate. Every snapshot a schedule creates is stamped with the
  `kairon.zyvor.dev/snapshot-schedule: <schedule-name>` label at creation
  time; pruning only ever counts and deletes snapshots carrying that exact
  label with that exact schedule's own name.
- **A snapshot that isn't `status.readyToUse` yet** -- one still
  `Freezing`/`Thawing`/`Pending` is never counted toward the limit and
  never itself a deletion candidate. This means a `keepLast: 1` schedule
  never has a moment with zero completed backups: the old ready snapshot
  only gets deleted once a newer one has actually finished, never while a
  replacement is still in progress.

Retention is per-Machine, not per-schedule-in-total: a schedule matching 5
Machines with `keepLast: 3` keeps up to 3 snapshots *for each* of those 5
Machines, not 3 total across all of them.

## Daily runs, jitter and age-based retention

```yaml
spec:
  selector: {tier: web}
  dailyAt: "02:30"       # UTC; set this OR intervalSeconds, never both
  jitterSeconds: 1800    # this schedule fires at a fixed point in 02:30-03:00
  keepLast: 14
  maxAgeSeconds: 604800  # also drop this schedule's snapshots older than 7 days
```

- **`dailyAt: "HH:MM"`** (UTC, 24h) fires once a day at that time. A
  brand-new daily schedule waits for the first slot after its creation
  instead of firing immediately. `startingDeadlineSeconds` counts from the
  slot, so a controller that was down at 02:30 can still skip a run it
  found too late.
- **`jitterSeconds: N`** gives each schedule a fixed offset in `[0, N)`,
  derived from a hash of its namespace/name, so it is the same after every
  controller restart and different schedules spread out. With
  `intervalSeconds`, jitter also aligns runs to a fixed grid (Unix epoch +
  offset + k x interval), so late runs never drift schedules back into each
  other; the first run then waits for the next grid slot too. Must be below
  `intervalSeconds`, or below 86400 with `dailyAt`. Without jitter, interval
  schedules behave exactly as before.
- **`maxAgeSeconds: N`** prunes this schedule's ready snapshots older than
  N seconds, alongside `keepLast` (a snapshot goes if either rule says so).
  The newest ready snapshot per Machine is always kept, however old, so a
  stalled schedule never prunes a Machine down to zero backups.

See `examples/snapshotschedule-daily.yaml` for a complete daily schedule using all three.

An invalid combination the CRD schema can't catch (for example
`jitterSeconds` not below `intervalSeconds`) never fires; it shows up as
`status.lastRunError: invalid spec: ...`. `kaironctl create/edit
snapshotschedule` take `--daily-at`, `--jitter-seconds` and
`--max-age-seconds`.

## Running one right now (`kaironctl trigger`)

Every example above waits for `spec.intervalSeconds` to elapse. Sometimes
that's not what you want -- you're about to do something risky to a fleet
of Machines and want one fresh backup first, or you just tightened
`spec.selector` and want to confirm the new selection actually works,
without waiting up to a full `intervalSeconds` (potentially 24 hours or
more) to find out:

```console
$ kaironctl trigger snapshotschedule nightly
snapshotschedule/nightly: manual run requested; kairon-controller will snapshot every matching Machine on its next reconcile tick, regardless of spec.suspend or the normal interval
```

This patches a request annotation
(`kairon.zyvor.dev/trigger-now: <RFC3339 timestamp>`) onto the schedule --
the same durable "ask the controller to do a thing on its next tick, not a
bespoke RPC" pattern this project already uses for guest quiesce around
`MachineSnapshot` itself. `reconcileMachineSnapshotSchedules` notices any
value that doesn't already match `status.lastHandledTriggerTime` as an
unhandled request, fires this schedule exactly like a normal due run on its
very next tick (the same per-Machine create loop, the same `spec.keepLast`
pruning if set), and copies the handled value into
`status.lastHandledTriggerTime` in that same status patch -- so the same
request is never re-fired on a later tick, and a crash between "fired" and
"marked handled" can't happen (either both landed in that one patch, or
neither did, in which case the next tick just retries).

A manually triggered run **bypasses both `spec.suspend` and
`spec.startingDeadlineSeconds`**:

- A schedule you've paused (`kaironctl edit snapshotschedule nightly
  --suspend true`) can still be asked for one snapshot right now, without
  permanently unpausing it first -- you get your one backup, then it stays
  paused exactly as you left it.
- `startingDeadlineSeconds` exists to answer "is this run too late to still
  count as on-schedule" -- a question that has no meaning for a request
  that's explicitly asking to run at this exact instant. A manual trigger
  is never "too late."

It does **not** bypass `spec.selector`: a manually triggered schedule that
currently matches zero Machines still counts as handled (`status` advances
exactly like a normal zero-match tick), it just creates nothing -- the
same "check `spec.selector` against these Machines' own labels" caveat
`describe`'s preview already gives.

A manually triggered run also still advances `status.lastRunTime`/
`nextRunTime` exactly like any other fire -- the schedule's normal interval
countdown restarts from the moment of the manual run rather than getting a
"bonus" run layered on top of the pre-existing schedule. If `nightly` last
fired at midnight on a 24-hour interval and you manually trigger it at
9am, its next automatic run is now expected around 9am the following day,
not still at midnight.

`kaironctl describe snapshotschedule` reports an outstanding, not-yet-handled
manual trigger request as its own distinct preview outcome, ahead of the
usual due/not-due/skipped verdicts (see "Previewing what would fire right
now" above):

```console
$ kaironctl trigger snapshotschedule nightly
snapshotschedule/nightly: manual run requested; ...
$ kaironctl describe snapshotschedule nightly
...
Matching machines (2) -- manual run requested (kaironctl trigger snapshotschedule), the next reconcile tick will snapshot these regardless of spec.suspend or the normal interval:
  web-1
  web-2
```

There's no dashboard or `kubectl`-only equivalent yet -- requesting a
manual run is `kaironctl`-only for now (though `kubectl annotate --overwrite
machinesnapshotschedule nightly kairon.zyvor.dev/trigger-now="$(date
--rfc-3339=seconds)"` works exactly as well; the annotation's value is
never parsed as a real timestamp by `kairon-controller`, only compared for
equality against the last one it handled, so any new, distinct string is a
valid request).

## Missed runs (`spec.startingDeadlineSeconds`)

Every `MachineSnapshotSchedule`'s original behavior is: however overdue a
run is, it always fires as soon as `kairon-controller` notices. Usually
that's exactly right -- a few seconds of reconcile-loop lag between "due"
and "actually ran" doesn't matter for a nightly backup. But if the
controller was down for an extended maintenance window, or this CRD's
`status` was reset by a reinstall, a schedule's next-due window can end up
hours or days in the past by the time reconciliation resumes -- and firing
it at that point isn't really "on schedule" anymore, it's a stale
catch-up run.

`spec.startingDeadlineSeconds`, Kairon's analog of Kubernetes `CronJob`'s
field of the same name, opt-in bounds how late a due run is still allowed
to actually fire:

```yaml
spec:
  selector: {tier: web}
  intervalSeconds: 3600
  startingDeadlineSeconds: 900
```

With this set, a run that's still within 900 seconds of when it first
became due fires normally, exactly as before. A run discovered *more* than
900 seconds late -- controller was down, CRD reinstalled, whatever the
cause -- is skipped instead of fired: no `MachineSnapshot` is created for
that window at all, but the schedule's clock still advances
(`status.lastRunTime` is set to now, so the next check starts counting a
fresh interval rather than re-detecting the same missed window forever) and
`status.lastRunError` records the skip (`"skipped: this run was more than
startingDeadlineSeconds (900s) late"`) so it's visible in `kaironctl get
snapshotschedules`, `kaironctl describe snapshotschedule`, and the
dashboard's own **Last error** column -- not a silent no-op.

Unset (`0`, the default) never skips anything -- an overdue run always
fires, no matter how overdue, exactly this project's original behavior
before this field existed. Enabling it on an existing schedule is purely
opt-in and never changes behavior for a run that's on time.

`kaironctl describe snapshotschedule` extends its "what would fire right
now" preview (see above) to this: a schedule that's `Due` but already past
its own `startingDeadlineSeconds` is reported as a third, distinct outcome
-- `due, but will be SKIPPED` -- rather than folded into either "due now"
(would actually fire) or "not due yet" (hasn't reached its interval at
all), since it's neither: it's overdue *and* too late to catch up.

## When will it run next? (`status.nextRunTime`)

Every schedule's `status` also carries `nextRunTime`, projecting the next
time it's expected to fire -- `kaironctl get snapshotschedules` shows it in
a `NEXTRUN` column, `kubectl get machinesnapshotschedules` has its own
`NextRun` printer column, and the dashboard's **Snapshot schedules** page
has a **Next run** column too.

`nextRunTime` is set once, every time a schedule actually fires, to
`lastRunTime + intervalSeconds` (the next fixed slot instead, for a
`dailyAt` or `jitterSeconds` schedule) -- a simple as-of-last-fire projection, not
a live countdown recomputed on every reconcile tick. This is a deliberate
choice, not an oversight: `MachineSnapshotSchedule`'s own status is only
ever patched when a schedule is `Due` (see "How it's enforced" below) --
adding a second, independent tick that patches `nextRunTime` alone for
every schedule that *isn't* due yet would multiply this CRD's write volume
against the apiserver for a value that's already a pure function of fields
already in `status`/`spec`, for no real benefit.

One consequence of that choice: `nextRunTime` is **not** cleared or
recomputed if a schedule is suspended sometime after it last fired -- the
stored value can point at an already-passed timestamp while
`spec.suspend: true`. Both `kaironctl` and the dashboard handle this
correctly at *display* time rather than trusting the stored field blindly:
each checks the live `spec.suspend` flag first and shows `suspended`
instead of a stale, already-passed timestamp whenever it's set. A schedule
that has never yet fired shows `pending` the same way, rather than a zero/
epoch date. `kubectl get machinesnapshotschedules`' own `NextRun` printer
column is the one place this project can't apply that same live-suspend
check -- it's a bare `jsonPath: .status.nextRunTime` and shows the raw
stored value verbatim, so a `kubectl`-only workflow should also check
`spec.suspend`/the `Suspend` printer column before trusting it. A schedule
that's never fired shows a blank `NextRun` there, not `pending` (`kubectl`
has no such formatting hook either) -- a real, honestly-named limit of the
plain-CRD-printer-columns mechanism itself, not something this project's
own code can paper over.

## How it's enforced

Entirely by `kairon-controller`'s own reconcile loop
(`internal/controller/machinesnapshotschedule.go`) -- there's no admission
webhook, the same reasoning `MigrationPolicy`'s own guide already gives:
this CRD doesn't gate any other object's admission, it only creates new
objects on a timer, so there's nothing for a webhook to validate at
create-time that the CRD's own OpenAPI schema (`intervalSeconds: minimum
60`) doesn't already cover.

Each reconcile tick:

1. Every `MachineSnapshotSchedule` is first checked for an unhandled
   `kairon.zyvor.dev/trigger-now` request (its value differs from
   `status.lastHandledTriggerTime`) -- if so, it's treated as due
   regardless of everything in step 2, ahead of the normal due check
   entirely. See "Running one right now" above.
2. Otherwise, it's checked against `status.lastRunTime`: due if it's never
   run before, or if at least `spec.intervalSeconds` have elapsed since
   the last run, and not `spec.suspend`d.
3. If `spec.startingDeadlineSeconds` is set and this due run is more than
   that many seconds late, it's skipped instead: no `MachineSnapshot` is
   created, but `status.lastRunTime`/`nextRunTime` still advance and
   `status.lastRunError` records the skip -- see "Missed runs" above. (A
   manually triggered run from step 1 never takes this branch --
   `startingDeadlineSeconds` doesn't apply to it.)
4. Otherwise, for each due schedule, every `Machine` in the same namespace
   matching `spec.selector` gets a new `MachineSnapshot`, named
   `<schedule-name>-<machine-name>-<unix-timestamp>` and labeled
   `kairon.zyvor.dev/snapshot-schedule: <schedule-name>`.
5. If `spec.keepLast` is set, right after each successful create the
   schedule's own ready-to-use `MachineSnapshot`s for that same Machine
   (matched by the label above) beyond `keepLast` are deleted, oldest
   first -- see "Retention" above for exactly what does and doesn't count.
6. `status.lastRunTime`/`lastRunSnapshotCount`/`lastRunError`/`nextRunTime`
   are patched once, after every match has been attempted -- a schedule
   matching zero Machines still gets `lastRunTime`/`nextRunTime` patched,
   so it doesn't re-fire every tick forever waiting for a Machine that may
   never appear. `nextRunTime` is set to this tick's `lastRunTime +
   intervalSeconds` (the next fixed slot for `dailyAt`/`jitterSeconds`) -- see "When will it run next?" above for exactly what
   it does and doesn't promise. A failure creating one Machine's snapshot
   is logged and counted
   (`kairon_reconcile_item_errors_total{kind="snapshotschedule"}`) but
   never stops the rest of that schedule's matches from being attempted.
   A run that satisfied step 1's manual trigger also gets
   `status.lastHandledTriggerTime` set to the request's value in this same
   patch, so it's never re-fired on a later tick -- see "Running one right
   now" above.

## Real limits today (first cut)

- **`intervalSeconds` or `dailyAt`, not real cron syntax.** There's no
  day-of-week, multiple-times-a-day or month expression support -- a
  schedule fires every `intervalSeconds` or once a day at `dailyAt` (UTC
  only, no time zones). This is a deliberate simplification
  matching this project's Go-stdlib-only bias (no new cron-parsing
  dependency) and its established pattern of shipping a simpler mechanism
  honestly labeled as such (see `MigrationPolicy`'s own plain
  `bandwidthMbps`/`maxConcurrent` scalars for the same precedent).
- **Stagger is opt-in.** Without `jitterSeconds`, several schedules
  sharing the same interval (or created around the same time) can all
  become due on the same reconcile tick and fire together.
- **Retention is per-Machine and flat (`keepLast`, `maxAgeSeconds`), not
  tiered.** There's no "keep one per day for 7 days, one per week for 4
  weeks"-style retention (the kind a dedicated backup tool would offer).
  Leaving both unset (the default) still accumulates snapshots forever,
  exactly as before these fields existed.
- **Namespace-scoped only** -- a `MachineSnapshotSchedule` only ever
  matches Machines in its own namespace, same as `MigrationPolicy`/
  `MachineDisruptionBudget`.
- **Dashboard is list + suspend/resume only.** The **Snapshot schedules**
  page shows every schedule and its last-run status (including a skipped
  run's `lastRunError`, in the **Last error** column), and can toggle
  `spec.suspend` with a click -- but editing `selector`/`intervalSeconds`/
  `keepLast`/`volumeSnapshotClassName`/`startingDeadlineSeconds`, requesting
  a manual run, or creating/deleting a schedule, still needs
  `kaironctl`/`kubectl`.
- **Manual trigger request is `kaironctl`/`kubectl annotate`-only, no
  dashboard button yet.** `kaironctl trigger snapshotschedule NAME` (or
  hand-annotating with `kubectl annotate --overwrite`) is the only way to
  ask for one today. The request also waits for the next reconcile tick
  like everything else in this CRD -- there's no synchronous "wait for it
  to actually finish" mode; check `status.lastRunTime`/
  `lastHandledTriggerTime` (or just watch for the new `MachineSnapshot`)
  to see it land.
- **`startingDeadlineSeconds` only ever skips a whole due window, never
  partially.** If a schedule matches 5 Machines and the run is found too
  late, all 5 are skipped together for that window -- there's no
  per-Machine deadline, and no "fire for the Machines that are still within
  some grace period, skip the rest" middle ground. The next on-time window
  fires for all matches again, same as always.
- **`status.nextRunTime` is a projection, not a live countdown.** It's only
  ever recomputed when a schedule actually fires (`lastRunTime +
  intervalSeconds` at that moment) -- see "When will it run next?" above.
  `kaironctl`'s and the dashboard's own displays correctly show
  `suspended`/`pending` instead of a stale timestamp by checking the live
  `spec.suspend` flag first, but `kubectl get machinesnapshotschedules`'
  plain `NextRun` printer column can't apply that same logic -- it shows
  the raw stored value (or a blank cell if the schedule has never fired),
  even while suspended. Check `spec.suspend`/the `Suspend` column
  alongside it in a `kubectl`-only workflow.
- **`kaironctl describe snapshotschedule`'s preview is a snapshot of this
  instant, not a guarantee.** It's a plain read-then-compute at the moment
  you ran it -- a Machine's labels, or the schedule's own `selector`/
  `suspend`/`intervalSeconds`, can change before the next actual reconcile
  tick runs, and this preview does nothing to lock or reserve anything.
  There's also no equivalent preview in the dashboard or via `kubectl` --
  it's `kaironctl`-only for now.
