---
title: kairon-ui dashboard
---

# kairon-ui dashboard

`kairon-ui` serves a single-page web console over **HTTPS** (default port
`:18082`; change with `--ui-port=N`, `KAIRON_UI_LISTEN`, or the chart's
`ui.service.port`; see [HTTPS](#https)). It is a client of the [kairon-ui API](kairon-ui-api.md) and
adds no privileged side channel: every button maps onto an existing route, and
the server enforces the real gates (role, namespace, administrator access,
guest agent, Machine phase).

## Signing in

The sign-in page shows live fleet stats (CRD and API-route counts, fed by
`GET /api/v1/auth/config`) and one username-and-password form under the Zyvor "Z" logo (the username is
pre-filled with `admin`, or the account remembered on this device; "Remember
me" applies to password sign-ins only). The submit handler reads the field
values from the form itself, so browser autofill and password managers work
without any typing events. Servers that still use the legacy shared bearer
token accept it in the password field (it is tried as a token when
username/password sign-in is off or refuses the pair), so there is no separate
token screen. A failed sign-in always reads "Wrong username or password.". SSO appears as an extra button when
configured; see [OIDC](kairon-ui-oidc.md).

- A free-text hint appears under the form when the server provides one
  (`loginHint` in the auth config; it is only sent when at least one named user
  exists). While the seeded admin still has the lab default password it reads
  "Lab default login: admin / Admin@321 - change it before exposing this
  dashboard". An explicit `KAIRON_UI_LOGIN_HINT` replaces that text.
- A fresh Helm install seeds `admin` / `Admin@321` while
  `ui.auth.wellKnownDefault` is `true` (the chart default); an existing
  `kairon-ui-session` password is never replaced, an explicit
  `ui.auth.defaultAdminPassword` wins, and `values-production.yaml` sets it
  to `false` so the password is random. Read the actual value with
  `kubectl -n kairon-system get secret kairon-ui-session -o jsonpath='{.data.defaultAdminPassword}' | base64 -d`.
- `scripts/deploy-remote.sh --with-ui` seeds a lab `admin` account; see the
  "Well-known default password" paragraph in [SECURITY.md](../../SECURITY.md)
  before exposing the port beyond a lab network.
- Signed-in operators change their own password from **Account**
  (`POST /api/v1/auth/password`).
- A session that signed in with the well-known lab password `Admin@321` shows a
  red banner on every page except Account until the password is changed or the
  session ends (the login response carries `defaultPassword: true`; the server
  only reports it while `KAIRON_UI_DEFAULT_ADMIN_PASSWORD` is that exact value
  and the submitted password matches; the flag lives in `sessionStorage`). It is
  a nudge, not a gate: nothing is blocked.

## Namespaces

The dashboard works in one namespace at a time, **`default`** unless you change it. Pick another from the **ns** selector at the right of the top navigation bar; the list is the namespaces you may use (`GET /api/v1/namespaces`, which honours namespace scoping). The choice is remembered in the browser and applies to every namespaced page, action, console, log and usage-CSV download. Before this selector existed the dashboard only ever showed `default`, because it never sent the `namespace` parameter the API scopes lists by. Changing the selection remounts the current page and re-fetches. Empty lists say which namespace they are scoped to and suggest a first command (for example `kaironctl create NAME --image PATH`).

## Tables

Every list page uses the same table. Once a list has more than five rows (or a
filter is active) a **Filter** box appears: it keeps rows whose columns contain
every whitespace-separated word, case-insensitively, shows "N of M", and says
`No rows match "..."` when nothing is left. Column headers sort, and **CSV**
exports the rows currently shown.

### Bulk power actions (Machines)

Tick rows in the Machines table (the header box selects every row currently
shown, after filtering) to get a bar with **Start**, **Stop** and **Clear**.
Start skips Machines that are already Running or Paused; Stop applies only to
Running ones and asks once for the whole batch. Each Machine is sent as its
own `POST /api/v1/machines/{ns}/{name}/{start|stop}`, one at a time, so a
failure never blocks the rest: the toast names the ones that failed and they
stay selected. Works in the all-namespaces view too. Single-row **Stop** and **Halt** also
ask for confirmation first (a Machine delete already did), and every power
action reports its outcome in a toast.

### All namespaces (Machines, Migrations, Snapshots)

Choose **All namespaces** in the nav selector to list Machines, Migrations and
Snapshots from every namespace you may use in one table, with a Namespace
column; row actions (power, delete, priority, console, logs, details, cancel,
recover) act on the row's own namespace. Every other page, and creating
anything, keeps working in the concrete namespace last selected (Migrate and
Snapshot are hidden for Machine rows, and Restore for Snapshot rows, outside
that namespace). The API form is `GET /api/v1/{machines,migrations,snapshots}?namespace=_all`
(see the [API reference](kairon-ui-api.md)).

## Navigation

Navigation is grouped into a frosted top bar with a mega-menu (a sheet on small
screens). Routes are hash-based, so every page and inspector target is
linkable: `#/machines`, `#/machines/default/web01`.

| Group | Pages |
|---|---|
| Overview | Overview |
| Compute | Machines, Machine sets, Instance types, Nodes, Storage, Pools & claims, Images |
| Lifecycle | Migrations, Migration policies, Snapshots, Snapshot schedules, Restores, Backups |
| Policy | Quotas, Disruption budgets, Network policies, Security groups |
| Operations | Fleet operations |
| AI | Assistant |

Press <kbd>Ctrl</kbd>/<kbd>⌘</kbd>+<kbd>K</kbd> for the command palette, a
fuzzy finder over every page. The theme is light by default; choose dark or
follow-the-system from the theme control in the nav bar (the choice is stored
in the browser only).

## Pages added with the v1 API

| Page | Reads | Notes |
|---|---|---|
| Storage | `GET /api/v1/atlas/{path...}` | Read-only Atlas volumes, pools, backups and health. Needs `ui.atlas.*`, and administrator access when namespace scoping is on; see [Atlas storage](machine-storage-atlas.md#dashboard-storage-page). |
| Pools & claims | `GET /api/v1/machinepools`, `/machineclaims` | Warm [MachinePools](machine-pools.md) and the claims bound to them. |
| Backups | `GET /api/v1/machinebackups`, `/machinebackuprestores` | Off-cluster [machine backups](machine-backup.md) and their restores. |
| Images | `GET`/`PUT`/`DELETE /api/v1/images` | Upload and serve images to nodes; upload and delete need an administrator. See [image import](machine-image-import.md). |

The list routes honour namespace scoping exactly like the other `namespace`
routes.

## Machine inspector

Selecting a Machine opens the inspector. Besides phase, placement and
conditions, its **Operations** panel exposes diagnostics and consistency
actions that wrap the per-Machine routes:

- network capture sessions (start, list, download) and effective policy,
- guest-agent command execution (`agent-exec`, see
  [guest exec](machine-guest-exec.md)) and file get/put
  ([guest agent files](machine-guest-agent-files.md)),
- filesystem freeze/thaw and VM-state snapshot/restore,
- guest firewall open/close through the guest agent.

Actions that need the guest agent, an administrator, or a specific phase return
the server's error text in the panel rather than being hidden.

The inspector also shows:

- **Usage**: CPU, memory and disk sparklines for the last 15 minutes. kairon-ui
  samples every 15 s into memory (one hour per Machine), so history is per
  replica and starts empty after a restart. CPU is the lifetime average the
  Machine status reports, not an instantaneous rate.
- **Timeline**: the Machine's Kubernetes Events and condition transitions,
  newest first.
- **Placement** (Pending Machines): which nodes the requested CPU and memory
  would fit, with the reason when they would not.
- **Fork**, **Backup**, **Hot-attach disk** and **Hot-add NIC** forms. They
  write what `kaironctl fork`, `backup`, `disk` and `nic` write; the server
  refuses a Machine that is not Running, or a NIC on a non-`tap` network.

Pause, resume and stop show an **Undo** toast for six seconds.

## Live updates

kairon-ui serves `GET /api/v1/stream`, a Server-Sent Events feed of Machine and
migration changes. The Machines, Migrations, Topology and Network overview
pages refetch within about a second of a change and fall back to polling when
the stream is not connected. The stream pauses while the tab is hidden.

## Overview attention strip

The Overview lists what needs an operator, from fixed rules: failed Machines,
Machines unschedulable for five minutes or more, migrations in `NeedsRecovery`
or in flight for over 30 minutes, nodes not Ready with Machines on them or at
least 90% requested, and quotas at 90% or more. Each finding links to the page
and inspector it concerns. Node findings are hidden from non-administrators
when namespace scoping is on.

## Topology, System and Network overview

- **Topology** shows nodes as lanes, VMs as dots coloured by phase and pods as
  squares. Dragging a Running VM onto another Ready node stages a migration
  that you confirm before anything moves.
- **System** shows Kairon's component health and pods, with a switch to show
  every pod you may see. It needs the `pods` and `events` read rules in the
  `kairon-ui` ClusterRole.
- **Network overview** shows dataplane attach, policy sync, edge identity,
  anti-spoof and conntrack restore per Machine.
- **Nodes** gains requested-versus-allocatable bars and a "Will it fit?"
  what-if for a given vCPU and memory size.

## HTTPS

The dashboard carries logins and bearer tokens, so it serves HTTPS by default
everywhere, with no flag to remember:

| How it runs | Certificate |
|---|---|
| Binary or systemd with no certificate set | a self-signed ECDSA certificate generated at startup (names: localhost, loopback, the hostname, every interface address, and `KAIRON_UI_TLS_SANS`); it changes on every restart |
| `-tls-cert`/`-tls-key` (or `KAIRON_UI_TLS_CERT`/`KAIRON_UI_TLS_KEY`) | yours, reloaded when the files change; TLS 1.2 and later |
| Helm | per-pod self-signed by default; `ui.tls.secretName` mounts a `kubernetes.io/tls` Secret (cert-manager, for example); `ui.tls.sans` extends the generated certificate |
| `scripts/deploy-remote.sh --with-ui` | a certificate generated with openssl for the host's names and addresses, installed to `/etc/kairon/ui-tls/`; `--ui-tls-cert`/`--ui-tls-key` bring your own |

```bash
kairon-ui -tls-cert /etc/kairon/ui-tls/cert.pem -tls-key /etc/kairon/ui-tls/key.pem

scripts/deploy-remote.sh HOST USER --with-ui                              # self-signed
scripts/deploy-remote.sh HOST USER --with-ui --ui-tls-cert=c.pem --ui-tls-key=k.pem

helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set ui.enabled=true --set ui.tls.secretName=dashboard-tls
```

- A certificate without its key (or the reverse) is a startup error, never a
  quiet fall back to HTTP.
- Behind an Ingress the chart annotates the backend as HTTPS
  (`ui.ingress.backendProtocol`, nginx's annotation by default; set your
  controller's equivalent in `ui.ingress.annotations` otherwise) and
  `ui.ingress.tls` terminates TLS for browsers.
- `kaironctl` reaches the dashboard with `KAIRON_UI_URL=https://...`. For a
  self-signed certificate set `KAIRON_UI_CA=/path/to/cert.pem` to trust it
  without disabling verification. `KAIRON_UI_INSECURE=true` skips verification
  and is for throwaway labs only.
- `kaironctl ui` and the connectivity probe reach the dashboard over HTTPS
  through the API server. Its loopback listener (`127.0.0.1`) is the one plain
  HTTP hop that remains: it never leaves your machine, like `kubectl proxy`.

### Opting out

Plain HTTP is something you ask for by name: `-insecure-http` (or
`KAIRON_UI_INSECURE_HTTP=true`), `ui.insecureHttp: true` in Helm,
`--ui-insecure-http` for `deploy-remote.sh`. It is for a mesh or sidecar that
terminates TLS in front of the pod, or a trusted loopback. Logins and tokens are
clear text on that network, startup logs a warning, and `kaironctl connectivity
test` reports an HTTP-only dashboard as a warning. It cannot be combined with a
certificate: that contradiction is refused at startup. Use `kaironctl ui
--backend-http` to reach such an install.

## Self-test

**Operations › Self-test** boots a CirrOS VM from the server and shows a
coloured log in a terminal-style panel, with a live checklist beside it, a
Cancel button and a JSON report download. It needs an administrator account and
runs one test at a time. See [Self-test](selftest.md).

## Not in the dashboard

Fencing a Machine off a dead node stays in `kaironctl fence`: it records
operator attestation and cross-checks the node's liveness lease, which kairon-ui
does not have the configuration for.

## Node tools

The **Nodes** page opens a per-node panel with these tabs: Capabilities, Image
catalog (clone, rename, read-only, export, clean, delete), Warm pools
(create, claim, delete), Templates, Sandboxes and Egress check. All of them
need a session; mutating calls are logged like any other non-`GET` API call.

## Overview and usage export

The Overview page shows KPI tiles (count-up on load) and a **usage CSV**
download backed by `GET /api/v1/usage.csv?namespace=<ns>`.

## Assistant and agent tools

The Assistant page asks questions and diagnoses resources through
`POST /api/v1/agent/ask` and `GET /api/v1/agent/diagnose/...`. An **Agent
tools** panel posts JSON to the read-only agent-plane routes (compile network
policy, explain drops, flow anomalies, CPU label, confidential projection,
gateway binding, claim step). They only return proposals; nothing is applied.
See [agent plane](agent-plane.md).

## Related

- [kairon-ui API reference](kairon-ui-api.md)
- [High availability](kairon-ui-ha.md), [OIDC](kairon-ui-oidc.md),
  [console RBAC](kairon-ui-console-rbac.md)
