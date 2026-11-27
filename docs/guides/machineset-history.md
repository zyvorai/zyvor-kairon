# MachineSet template history and rollback

kairon-controller keeps one step of `spec.template` history on every
MachineSet, in four annotations:

| Annotation | Value |
| --- | --- |
| `kairon.zyvor.dev/current-template-hash` | hash of the template the controller last saw |
| `kairon.zyvor.dev/current-template` | that template, as JSON |
| `kairon.zyvor.dev/previous-template-hash` | hash of the template before it |
| `kairon.zyvor.dev/previous-template` | that template, as JSON |

The hash is the same value replicas carry in the
`kairon.zyvor.dev/machineset-template-hash` label. When the live
`spec.template` stops matching `current-template-hash`, the controller moves
the "current" pair to "previous" and records the live template as
"current". This also happens while the MachineSet is paused. A MachineSet
gets its first "current" on its next reconcile, and has no "previous" until
its template changes once.

## `kaironctl machineset history NAME`

```console
$ kaironctl machineset history web -n prod
NAME  NAMESPACE  CURRENT       PREVIOUS      STORED
web   prod       d797194f40bf  ae62905a23fa  yes
```

`STORED` says whether `rollback` can restore the previous template:

- `yes`: a copy is stored and hashes to `PREVIOUS`.
- `hash-only`: only the hash was recorded, either because the template was
  over 64 KiB of JSON or because it was recorded by a controller that kept
  hashes only. It can't be rolled back to.
- `invalid`: the stored copy doesn't decode or doesn't hash to `PREVIOUS`
  (edited by hand). `rollback` refuses it.
- `-`: no previous revision yet.

`CURRENT` shows the live template's hash with `(unrecorded)` after it when
you've just edited the template and the controller hasn't caught up yet.

## `kaironctl machineset rollback NAME [--dry-run]`

```bash
kaironctl machineset rollback web -n prod --dry-run   # prints old -> new hash
kaironctl machineset rollback web -n prod
```

`rollback` restores the previous template and swaps the current/previous
annotation pairs in one PATCH. The PATCH carries the object's
`resourceVersion`, so a concurrent edit makes it fail with a conflict
instead of being overwritten. Fields and labels the restored template
doesn't have are removed, not left over from the newer one. The controller
finds the annotations already in step, and the replicas are replaced by the
normal rollout (`strategy`, `maxUnavailable`, `maxSurge`). Running
`rollback` again goes forward to the template you just left.

It refuses, without changing anything, when:

- no previous revision is recorded, or only its hash (`STORED` is `-` or
  `hash-only`);
- the controller hasn't recorded the live template yet (`(unrecorded)` in
  `history`); wait one reconcile interval and retry;
- the stored copy doesn't hash to its recorded hash.

## Limits

- One step of history only: there is no "roll back two revisions".
- Kubernetes caps an object's annotations at 256 KiB in total, so each
  stored template is capped at 64 KiB of JSON; a larger one is recorded by
  hash only. Annotations were chosen over a ConfigMap or revision object
  because MachineTemplates are small, nothing else in Kairon keeps revision
  objects, and the history then goes away with the MachineSet.
- On a paused MachineSet, `rollback` changes the template but no replica is
  replaced until you unpause it.
