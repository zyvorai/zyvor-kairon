# User guide: sandboxes (`spec.sandbox`)

Run a short-lived, ephemeral workload -- e.g. an AI agent's own generated
code -- on FluxVM's own lightweight, fast-boot in-tree hypervisor track,
instead of a full QEMU/Cloud Hypervisor/Firecracker VM. Admin-only,
API-only first cut, no dashboard yet.

## What this is

```yaml
apiVersion: kairon.zyvor.dev/v1
kind: Machine
metadata: {name: agent-run}
spec:
  image: {path: /images/sandbox.qcow2}
  resources: {cpu: "1", memory: 512Mi}
  ttlSeconds: 900           # auto-expire after 15 minutes
  guestAgent: {console: true}   # unlocks Kairon's own file/exec/console UI (see below)
  sandbox: {}                    # opts into FluxVM's sandbox track
```

Or, booting from a pre-built template instead of an image (faster boot,
no `spec.image`/`spec.resources` needed -- FluxVM loads the template's own
pre-baked spec and ignores both entirely):

```yaml
spec:
  sandbox:
    templateName: alpine-agent
```

Under the hood: `kairon-node` calls FluxVM's real `POST /v1/sandboxes`
instead of `POST /v1/vms` -- FluxVM forces the runtime to its own
lightweight `flux-vm` backend and force-enables its own vsock guest agent
regardless of `spec.guestAgent`, whatever that says. Everything else about
a sandbox Machine -- `status.phase`, `spec.powerState`, `spec.ttlSeconds`,
deletion -- works exactly like any other Machine.

## Building a template first

A template is a pre-exported OCI image rootfs FluxVM can boot from
instantly, skipping a full image boot. Build one via the node-scoped admin
API (admin-only -- pulling and exporting an arbitrary OCI image is a real
resource cost):

```bash
curl -X POST https://<kairon-ui>/api/v1/nodes/<node>/templates \
  -H "Authorization: Bearer <token>" \
  -d '{"name": "alpine-agent", "imageRef": "docker.io/library/alpine:3.19"}'
```

List what's already built on a node with `GET
/api/v1/nodes/<node>/templates`. Templates are **per-node** state -- one
built on `worker-1` isn't visible from `worker-2`; a `spec.sandbox.templateName`
referencing a template that doesn't exist on the Machine's assigned node
fails with FluxVM's own clear error.

## Guest access

FluxVM always runs its own vsock guest agent for a sandbox, but Kairon's
own [guest file access](machine-guest-agent-files.md) and
[backend-agnostic guest exec](machine-guest-agent-files.md#related-guest-exec-over-the-same-vsock-agent-api-only)
still gate on `spec.guestAgent.console: true` client-side regardless --
set it anyway to use those against a sandbox. The QEMU-only
[guest exec](machine-guest-exec.md) (`qemu-guest-agent`) and
[interactive text console](machine-text-console.md) work the same way
too, since both ride channels a sandbox's lightweight hypervisor actually
supports (the vsock agent) or don't need QEMU specifically.

## HTTP proxy: reaching a service running inside the sandbox

**`ANY /api/v1/machines/{ns}/{name}/sandbox-http/{port}/{path...}`** relays
an arbitrary HTTP request straight through to a sandbox's own guest HTTP
server on `port` -- method, headers, body, and the guest's response
status/headers/body all pass through. Useful for a webhook receiver or dev
server the sandboxed workload starts on its own. Admin-only: this is a
materially broader capability than guest exec or file access -- whatever
the guest's own web server does with an arbitrary request is between the
guest and the caller, Kairon has no visibility into it.

Requires the Machine to actually have a guest IP (`spec.network` using
`tap`/bridge networking, not `user`/SLIRP -- the proxy dials the guest
directly, the same requirement FluxVM's own proxy route has).

```bash
curl -X POST https://<kairon-ui>/api/v1/machines/default/agent-run/sandbox-http/8080/api/webhook \
  -H "Authorization: Bearer <token>" \
  -d '{"event": "..."}'
```

## Listing sandboxes

**`GET /api/v1/nodes/{node}/sandboxes`** lists every FluxVM sandbox on a
node -- node-scoped, not Machine-scoped, since it reflects FluxVM's own
real state (including a sandbox created directly against FluxVM outside
Kairon entirely), the same posture `GET /api/v1/nodes` itself has for
node visibility. Any authenticated operator, since this is read-only.

## Checking egress policy

**`POST /api/v1/nodes/{node}/egress-check`** (admin-only, body `{"host"}`)
asks a node whether a sandbox's outbound request to `host` would be
allowed -- FluxVM's own stateless check against that node's static
`[sandbox]` config (`egress_allow_domains`, an empty list meaning "allow
everything"). Response: `{"allow", "reason", "wouldInjectCredential"}`.

There is deliberately **no way to change the allowlist through this or
any Kairon API** -- it's read from the node's own FluxVM config file at
daemon startup, not a runtime-mutable resource. And `wouldInjectCredential`
is a boolean, never the actual credential: FluxVM's own response carries
the literal secret value that would be injected for a matching host
(`inject_authorization`), and Kairon deliberately never returns that value
to a caller. See SECURITY.md's "Egress check" section for why.

## Warm pools

FluxVM keeps `size` VMs pre-booted and `Paused`, ready for an instant
claim -- much faster than a cold create, at the cost of keeping those VMs'
resident RAM committed on the node the whole time, whether claimed or not.
Node-scoped admin API, no dashboard yet:

- **`POST /api/v1/nodes/{node}/pools`** (body `{"name", "size", "template"}`,
  `template` a full FluxVM `CreateVmRequest` -- the same shape a sandbox's
  own `spec` field takes) creates a pool. FluxVM immediately starts
  booting `size` VMs to `Paused` in the background; the call itself
  returns as soon as the pool record exists, not once every member is
  ready.
- **`GET /api/v1/nodes/{node}/pools`** / **`GET .../pools/{name}`** list
  pools or show one's current `members` (VM UUIDs, any authenticated
  operator, read-only).
- **`POST .../pools/{name}/claim`** (body `{"name", "ttlSeconds"}`, both
  optional overrides, plus optional `{"createMachine", "namespace",
  "machineName"}` -- see below) pops one ready member, resumes it, and
  returns the now-`Running` VM. FluxVM triggers its own background
  backfill to replace the claimed member; Kairon doesn't wait for that to
  finish. Fails with a clear error if the pool has no ready members right
  now (rather than silently falling back to a slow synchronous create) --
  retry shortly, or make the pool bigger.
- **`DELETE .../pools/{name}`** (admin-only) deletes the pool **and every
  member VM it currently holds**, claimed or not -- a genuinely
  destructive operation, not just removing the pool's own bookkeeping.

**By default, a claimed VM is real FluxVM state, not automatically a
Kairon Machine.** Claiming hands back FluxVM's own VM record (UUID, name,
`guest_ip`, status) directly -- with no `createMachine`, there is no
automatic step that creates a corresponding Kubernetes `Machine` object
for it, and managing it as an ordinary Kairon Machine afterward (power
state, disruption budgets, quota accounting, the dashboard) needs a
manual follow-up you do yourself.

**Set `createMachine: true` (plus a required `machineName`, `namespace`
defaulting to `default`) to close that gap for this claim.** The response
becomes `{"vm": <claimed FluxVM record>, "machine": <created Machine>}`
instead of the bare VM record. Under the hood: the claim's own FluxVM-side
name is forced to `kairon-<namespace>-<machineName>` (overriding any
`name` you also set), matching exactly what `Machine{Name, Namespace}.RuntimeName()`
computes -- so kairon-node's existing adoption path (it already falls back
to looking up a runtime *by that exact name* when a Machine has no
`status.runtimeID` yet) picks up this precise VM on its very next
reconcile tick, instead of creating a second, duplicate one. The new
Machine's spec is built from the **pool's own template** (fetched fresh at
claim time, the same spec every member was actually booted from):
image, CPU/memory and their `maxCpu`/`maxMemory` hotplug headroom,
backend and kernel, cloud-init (including `staticNetwork`),
`numaNode`/`cpuSet`/`hugepages`, `secureBoot`/`tpm`, the guest agents and
`ttlSeconds` (the claim's own `ttlSeconds` wins when set). `spec.nodeName`
is set to the claimed VM's node, so the scheduler can never place the
Machine elsewhere and boot a duplicate. Template fields with no safe
Machine equivalent -- `vfio_devices` (a device can back only one VM), the
raw `network` map (per-VM MACs), `shared_folders`, `data_disks` and a
`storage` override -- are not carried; each one present adds a line to a
`"machineWarnings"` array in the response. If creating the Machine object itself fails (a namespace/name
conflict, an unreachable API server), the response still reports the
successful claim (`"vm"` populated, `"machine": null`, plus a
`"machineError"` explaining what to do next) -- the VM is real, running
state either way; this never pretends the claim didn't happen just
because the follow-up write did.

Deliberately opt-in, not the new default: warm pools exist for fast,
low-ceremony ephemeral VMs, and a full Machine object drags in
finalizer-gated deletion, quota accounting, and webhook validation that
not every caller wants -- omitting `createMachine` is byte-for-byte the
same behavior as before this existed.

**A note on overlap with FluxVM's own Kubernetes integration**: FluxVM
ships its own `MicroVMPool` CRD (part of `fluxvm-microvm`, a node-local
controller that reconciles against the exact same `/v1/pools` API this
section wraps) as part of a separate scheduler-native fleet story. If your cluster also
runs FluxVM's own `fluxvm-microvm` operator against the same nodes,
**don't manage the same pool name from both** -- neither coordinates with
the other, and both would independently create/delete/claim against the
identical underlying FluxVM state.

## Real limits today (first cut)

- **No VFIO passthrough.** `spec.deviceClaims` on a sandbox Machine is
  refused outright at create time with a clear error -- FluxVM's
  lightweight sandbox hypervisor doesn't support PCI passthrough.
- **A template-based sandbox ignores `spec.image`/`spec.resources`/
  `spec.runtime.kernel` entirely.** FluxVM loads the template's own
  pre-baked spec; setting those fields alongside `templateName` has no
  effect and isn't rejected as a conflict -- know that a template wins
  before wondering why a resource change didn't apply.
- **No admin-only NUMA/CPU-set/hugepage support** -- those already require
  the `qemu` backend elsewhere in this project, and a sandbox always
  resolves to `flux-vm`.
- **No dashboard yet.** Every capability here (create, list, HTTP proxy,
  template build/list) is API-only -- `kaironctl` has no dedicated verbs
  either.
- **`claim`'s `createMachine` does not carry VFIO devices, the raw network
  map, shared folders, data disks or a storage override** from the pool
  template -- they come back as `machineWarnings`; add the matching
  Machine fields (`deviceClaims`, `network`, `volumes`, `disks`) yourself.
- **No snapshot listing/deletion for templates**, matching
  [VM-state snapshot/restore](machine-vm-state-snapshot.md)'s own posture
  for tags -- FluxVM owns storage, Kairon has no prune/list-by-age
  mechanism.
- **`SnapshotSandbox`** (FluxVM's own `POST /v1/sandboxes/{id}/snapshot`,
  which exports a sandbox's rootfs state to an explicit file path) isn't
  wired to a Kairon-facing route yet -- `internal/fluxvm.Client.SnapshotSandbox`
  exists but has no admin API/consoleproxy relay in front of it today.
