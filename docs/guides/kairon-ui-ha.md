# User guide: running more than one kairon-ui replica

`ui.replicaCount` (Helm chart, default `1`) can be raised once real
per-operator login (`ui.auth.users`, or the seeded default admin account)
and/or the VNC console are in use. This is a first cut -- read the whole
page, especially "Real limits today," before relying on it.

## Why this needed real work

`kairon-ui` is a thin, stateless REST wrapper around the Kubernetes API for
almost everything -- Machines, migrations, snapshots all live entirely in
the cluster, never inside `kairon-ui` itself. But three pieces of runtime
state used to live only in one process's memory:

- **Session revocation** (`POST /api/v1/auth/logout`)
- **Login lockout** (5 consecutive failed logins locks a username out for
  5 minutes)
- **Console tickets** (the single-use, 30-second credential
  `POST /api/v1/machines/{ns}/{name}/console/ticket` mints so the real
  session token never has to appear in a WebSocket URL)

With more than one replica behind one Kubernetes `Service`, none of that
"just works" -- a logout served by pod A never invalidates the session on
pod B, a lockout triggered on pod A doesn't stop pod B from accepting more
attempts, and a console ticket minted by pod A is unusable if the
browser's WebSocket upgrade happens to land on pod B (a real, not
hypothetical, failure mode: the ticket-issuing `fetch()` and the
WebSocket upgrade are two separate TCP connections, and a `Service` gives
no session affinity between them by default).

A fourth, subtler gap: `Server.Users` (the account list + password
hashes) is only ever loaded once, at process startup, from
`KAIRON_UI_USERS_JSON`. A password change on one replica
(`persistUsers`) already wrote the new hash back to the `kairon-ui-users`
Secret, but a *different* replica had no way to notice -- it would keep
authenticating logins against the old password indefinitely. That's a
correctness bug specifically for the security-sensitive password-reset
path, so it's fixed by the same mechanism as the rest of this page.

## What was rejected, and why

Redis (or any other new stateful dependency) was the obvious way to get a
real, low-latency, atomic shared store -- and was explicitly rejected.
Taking on a new stateful dependency just for a dashboard's login/session
bookkeeping works against this project's actual design center: Kubernetes
itself is the only store every other component (`kairon-controller`,
`kairon-node`, `kaironctl`) already trusts, and `go.mod` stays
deliberately dependency-light otherwise. So this uses a plain Kubernetes
`ConfigMap` instead: `kairon-ui-shared-state`, created once by the Helm
chart alongside `kairon-ui` (regardless of `ui.replicaCount`, so scaling
up later needs no chart change), read and merge-patched by
`internal/uiapi/sharedstate.go`.

## How it actually works

Every mutation (revoke, a triggered lockout, a password reset, a minted
console ticket) still applies to the replica that served it **immediately
and locally**, exactly as it always did on a single replica -- there is no
behavior change at all with `ui.replicaCount: 1`. The same mutation is
then best-effort mirrored into `kairon-ui-shared-state`, merge-patching
*one key* at a time (`application/merge-patch+json`), never the whole
object -- concurrent writers touching *different* keys never conflict,
because there's no whole-object read-modify-write anywhere in this design.

Every replica also polls that ConfigMap (and, if `ui.auth.users` is
managed by this chart, the `kairon-ui-users` Secret) on a fixed ~15s
interval and merges what it finds into its own local state -- the same
"interval poll, not watch" posture `kairon-controller`/`kairon-node`
already use, rather than a client-go informer for one feature. So a
revocation or lockout triggered on a different replica becomes visible
here within about 15 seconds, not instantly.

**Console tickets are the one exception**, handled synchronously instead:
their 30-second lifetime is shorter than the poll interval, so waiting for
the next poll would defeat the ticket before it ever propagated. Once
`ui.replicaCount > 1` (in practice: once the shared ConfigMap is
configured), a console ticket is *only* ever looked up in the shared
ConfigMap, never trusted from a replica's own local cache -- otherwise the
replica that minted a ticket could keep accepting it even after a
*different* replica already consumed the shared copy, breaking single-use.

Every credential-shaped value (a session token, a console ticket) is
stored under a one-way SHA-256 hash of itself as the ConfigMap *key*, never
as the value or an unhashed key -- so anyone with `get`/`list` RBAC on
ConfigMaps in kairon-ui's namespace can't read out a usable credential,
only confirm one exists (a revoked/consumed value grants nothing anyway).
A username isn't a secret, so it's hex-encoded (reversible), not hashed --
`internal/uiapi/sharedstate.go`'s merge logic needs the literal username
back to key its own username-indexed maps correctly.

## Real limits today (first cut)

- **Propagation is ~15s, not instant**, except console tickets (see
  above). A revoked session or a fresh lockout stays honored on the
  replica that set it immediately, but a *different* replica can accept
  one more request against it for up to one poll interval.
- **Login-lockout's failure count is per-replica, not a cluster-wide
  atomic counter.** `maxLoginAttempts` (5) consecutive failures have to
  land on *one* replica to trigger a lockout -- this is exactly what
  choosing a ConfigMap over Redis costs: no distributed atomic increment.
  Once triggered, though, the resulting lockout itself *is* shared and
  honored everywhere. A determined attacker spreading failed attempts
  evenly across N replicas needs roughly N times as many attempts to
  trigger a lockout as they would against one replica.
- **Concurrent password changes to two *different* usernames on two
  *different* replicas, within the same few seconds, can race.**
  `persistUsers` still replaces the whole `users.json` blob in one Secret
  key, not a per-user key -- whichever replica's write lands last wins in
  full, silently discarding the other's change. Recoverable (redo the
  reset), not catastrophic, but real. A future per-user Secret-key layout
  would close this; out of scope for this first cut.
- **A concurrent console-ticket consume from two replicas within the same
  few milliseconds can still both succeed once.** The shared ConfigMap
  lookup-then-delete isn't atomic (`ConfigMap` merge-patch has no
  compare-and-swap). Narrow, and bounded by the ticket's own 30s TTL and
  the fact that consuming a ticket at all already requires having been
  handed one by an authenticated request.
- Console-ticket lookups against the shared ConfigMap are one extra,
  synchronous Kubernetes API call whenever a ticket is consumed on a
  *different* replica than the one that minted it -- acceptable given
  opening a console session is a manual, infrequent operator action, not
  a hot path.

## What you don't need to do anything for

- The session-signing key (`ui.auth.existingSessionSecret` or the
  chart-generated `kairon-ui-session` Secret) was already the same single
  object every replica's Deployment reads from -- no change needed there.
- Machines, migrations, snapshots, quotas, disruption budgets: none of
  that lived in `kairon-ui` to begin with. This page is specifically
  about the login/session/console-ticket state that did.
