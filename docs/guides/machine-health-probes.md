# User guide: machine health probes and auto-restart

`spec.healthCheck` gives a Machine readiness and liveness probes, a `Ready`
condition that reflects them, and an opt-in policy that restarts a hung
Machine. Everything is off unless you set it: a Machine without
`spec.healthCheck` is `Ready` as soon as it is `Running`, as before.

## Spec

```yaml
spec:
  guestAgent:
    enabled: true            # needed for GuestAgent and Exec probes
  healthCheck:
    readiness:               # gates Ready and expose endpoints
      type: TCP
      port: 8080
      initialDelaySeconds: 20
    liveness:                # detects a hung guest
      type: GuestAgent
      periodSeconds: 10
      failureThreshold: 3
    restartPolicy:           # opt in; omit it and nothing is ever restarted
      enabled: true
      maxRestarts: 3
      backoffSeconds: 60
      resetAfterSeconds: 3600
```

Probe types:

| `type` | What runs | Needs |
|--------|-----------|-------|
| `GuestAgent` | a request to the guest agent (a `guest-network-get-interfaces` round trip) | `spec.guestAgent.enabled`, `qemu-guest-agent` running in the guest |
| `TCP` | a TCP connect to `status.guestIP:port` from kairon-node | a guest IP the node can reach |
| `Exec` | `command` (argv, no shell) in the guest; exit 0 is healthy | `spec.guestAgent.enabled` |

Probe fields: `initialDelaySeconds` (from when the Machine starts Running),
`periodSeconds` (default 10), `timeoutSeconds` (default 2, at most 10),
`failureThreshold` (default 3), `successThreshold` (default 1; liveness is
always 1). The admission webhook (and `kaironctl preflight`) reject a probe
with no type, a TCP probe with no port, an Exec probe with no command, a
GuestAgent/Exec probe without `spec.guestAgent.enabled`, and a
`restartPolicy` with no liveness probe.

## What the probes do

kairon-node runs the due probes on its reconcile tick, only while the Machine
is `Running`, and records them in `status.health` (`state` is `Unknown`,
`Passing` or `Failing`, with consecutive counts and the last failure
message). A probe runs on the node's tick, so a probe period shorter than
that tick (about 3 seconds) is rounded up to it, and a slow guest can hold a
tick for up to the probe timeout.

The `Ready` condition on `status.conditions` is `False` when:

- the Machine is not `Running`;
- the readiness probe is `Unknown` (not yet passed) or `Failing`
  (reasons `ReadinessProbePending`, `ReadinessProbeFailed`); or
- the liveness probe is `Failing` (`LivenessProbeFailed`).

`kaironctl expose` endpoints follow it: the EndpointSlice keeps the Machine's
address but marks it `ready: false` while `Ready` is `False`, so Services stop
sending it traffic until it recovers. A Machine with no `Ready` condition at
all (an older kairon-node) is treated as before: `Running` means ready.

## Auto-restart

With `restartPolicy.enabled`, kairon-controller acts when the liveness probe
is `Failing` on a `Running` Machine (not migrating, not being deleted, power
state `Running`). It asks kairon-node to power-cycle it by setting the
existing `kairon.zyvor.dev/restart-requested` annotation (the same request
`kaironctl restart` makes) and records `kairon.zyvor.dev/health-restarts` as
`COUNT@TIME`. It never loops forever:

- **Threshold:** the probe's own `failureThreshold` must be reached first.
- **Backoff:** after restart N it waits `backoffSeconds` times 2^(N-1)
  (default 60 s, capped at 1 h) before another.
- **Cap:** after `maxRestarts` (default 3) it stops, records one `Warning`
  Event (`HealthRestartsExhausted`) and sets
  `kairon.zyvor.dev/health-restarts-exhausted`. The Machine stays not Ready;
  fix it, or delete the two annotations to re-arm the policy.
- **Reset:** once liveness has passed for `resetAfterSeconds` since the last
  restart, the counter is cleared.
- **Events:** each restart records a `Normal` `HealthRestart` Event
  (`kaironctl events -n NS`).

A restart resets the probes: `initialDelaySeconds` counts again from the
restart. Set it long enough for the guest to boot, or the next probe round
will count against the policy.

Readiness failures never restart a Machine. Auto-restart power-cycles the
guest and loses unsaved in-memory state; it is not a substitute for fixing the
guest.

## Inspect

```sh
kaironctl explain machine.spec.healthCheck
kaironctl get machine web -o yaml    # status.health, Ready condition
kaironctl events -n NS               # HealthRestart / HealthRestartsExhausted
```

## Limits

- Probes run from kairon-node. A TCP probe checks reachability from the node,
  not from other pods.
- `GuestAgent` uses the QEMU guest-agent channel only; it is not available on
  the vz backend (`spec.guestAgent.enabled` is rejected there).
- Probes are not run while the Machine is Paused, Stopped, Hibernated or
  migrating; `Ready` is `False` then because the phase is not `Running`.
