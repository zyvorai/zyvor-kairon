---
title: kairon-ui dashboard
---

# kairon-ui dashboard

`kairon-ui` serves a single-page web console on its HTTP port (default
`:18082`; change with `--ui-port=N`, `KAIRON_UI_LISTEN`, or the chart's
`ui.service.port`). It is a client of the [kairon-ui API](kairon-ui-api.md) and
adds no privileged side channel: every button maps onto an existing route, and
the server enforces the real gates (role, namespace, administrator access,
guest agent, Machine phase).

## Signing in

The sign-in page shows live fleet stats (CRD and API-route counts, fed by
`GET /api/v1/auth/config`) and one username-and-password form (the username is
pre-filled with `admin`, or the account remembered on this device). Servers
that still use the legacy shared bearer token accept it in the password field,
so there is no separate token screen. SSO appears as an extra button when
configured; see [OIDC](kairon-ui-oidc.md).

- A free-text hint appears under the form when the server provides one
  (`loginHint` in the auth config; it is only sent when at least one named user
  exists). While the seeded admin still has the lab default password it reads
  "Lab default login: admin / Admin@321".
- `scripts/deploy-remote.sh --with-ui` seeds a lab `admin` account; see the
  "Well-known default password" paragraph in [SECURITY.md](../../SECURITY.md)
  before exposing the port beyond a lab network.
- Signed-in operators change their own password from **Account**
  (`POST /api/v1/auth/password`).

## Namespaces

The dashboard works in one namespace at a time, **`default`** unless you change it. Pick another from the **Namespace** selector in the account menu (top right); the list is the namespaces you may use (`GET /api/v1/namespaces`, which honours namespace scoping). The choice is remembered in the browser and applies to every namespaced page, action, console, log and usage-CSV download. Before this selector existed the dashboard only ever showed `default`, because it never sent the `namespace` parameter the API scopes lists by.

### Bulk power actions (Machines)

Tick rows in the Machines table (the header box selects every row currently
shown, after filtering) to get a bar with **Start**, **Stop** and **Clear**.
Start skips Machines that are already Running or Paused; Stop applies only to
Running ones and asks once for the whole batch. Each Machine is sent as its
own `POST /api/v1/machines/{ns}/{name}/{start|stop}`, one at a time, so a
failure never blocks the rest: the toast names the ones that failed and they
stay selected. Works in the all-namespaces view too.

### All namespaces (Machines)

Choose **All (Machines)** in the nav selector to list Machines from every
namespace you may use in one table, with a Namespace column; row actions
(power, delete, priority, console, logs, details) act on the row's own
namespace. Only the Machines list has this view: every other page, and
creating a Machine, keeps working in the concrete namespace last selected.
The API form is `GET /api/v1/machines?namespace=_all` (see the
[API reference](kairon-ui-api.md)).

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
