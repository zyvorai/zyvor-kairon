---
title: kairon-ui API reference
---

# kairon-ui API reference

Every route `kairon-ui` serves, generated from the router in
`internal/uiapi/server.go`. The dashboard and `kaironctl` (for the
UI-backed commands) are clients of this API; it adds no privileged side channel
over the Kubernetes API.

## Transport

Every route below is served over HTTPS by default, on the dashboard's port
(18082 unless configured). With no certificate configured the server generates a
self-signed one, so a client either trusts it (`curl --cacert cert.pem`,
`KAIRON_UI_CA` for `kaironctl`) or is given a real certificate
(`ui.tls.secretName`). Plain HTTP exists only as the explicit `-insecure-http`
opt-out; see [HTTPS](kairon-ui-dashboard.md#https). The examples in this guide
omit the scheme and host for that reason.

## Access model

| Access | Meaning |
|---|---|
| none | Reachable without a token: probes, `/metrics`, the login and OIDC routes, and the image blob route. |
| console ticket | The VNC console WebSocket cannot send an `Authorization` header, so it is gated by a single-use ticket issued by an authenticated call. |
| session | Needs a valid session token (`Authorization: Bearer ...`). |
| namespace | Needs a session and passes the namespace check below. |

- **Roles.** An account whose role is `viewer` is limited to `GET`/`HEAD` (plus the auth routes). Other roles are `operator` and the administrator identity.
- **Namespace scoping** is opt-in. When it is on, a session without a user name is refused, `namespace` routes only see the namespaces granted to the user (`ui.auth.users[].namespaces`, or OIDC group mappings), and `/api/v1/nodes*` needs administrator access. `GET /api/v1/namespaces` returns the namespaces the caller may use.
- **Image blobs are unauthenticated by design.** `GET /images/sha256/{digest}` is how `kairon-node` downloads an uploaded image: it has no UI credentials and verifies the bytes against `spec.image.digest`. Anyone who can reach `kairon-ui` and knows a digest can download that image. Image upload and delete (`PUT`/`DELETE /api/v1/images/{name}`) need an administrator session; listing (`GET /api/v1/images`) needs only a session. See [machine-image-import.md](machine-image-import.md).
- Non-`GET` calls to `/api/v1/*` are logged by the server (method, path, remote address, status and, for session tokens, the user) when a logger is configured. This is request logging, not the hash-chained MCP audit log.

### Probes, metrics and image blobs

| Method | Path | Access |
|---|---|---|
| GET | `/healthz` | none |
| GET | `/images/sha256/{digest}` | none |
| GET | `/metrics` | none |
| GET | `/readyz` | none |

### agent

Read-only agent-plane routes (see [agent plane](agent-plane.md)). The bodies
are proposals or facts; nothing is applied to the cluster.

| Method | Path | Access |
|---|---|---|
| POST | `/api/v1/agent/anomalies` | session |
| POST | `/api/v1/agent/ask` | session |
| POST | `/api/v1/agent/claims/step` | session |
| POST | `/api/v1/agent/compile-policy` | session |
| POST | `/api/v1/agent/confidential` | session |
| POST | `/api/v1/agent/cpu-label` | session |
| GET | `/api/v1/agent/diagnose/{namespace}/{kind}/{name}` | namespace |
| POST | `/api/v1/agent/explain-drops` | session |
| POST | `/api/v1/agent/gateway` | session |

### atlas

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/atlas/{path...}` | session |

An allowlisted, read-only proxy to the Atlas gateway for the dashboard Storage
page. Returns `501` unless `ui.atlas.url` is set, and `403` for non-administrators
when namespace scoping is on (Atlas data spans tenants). Only an allowlisted
set of read paths is proxied (anything else is `404`); the Atlas token never
reaches the browser. See [Atlas storage](machine-storage-atlas.md#dashboard-storage-page).

### auth

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/auth/config` | none |
| POST | `/api/v1/auth/login` | none |
| POST | `/api/v1/auth/logout` | none |
| GET | `/api/v1/auth/oidc/callback` | none |
| GET | `/api/v1/auth/oidc/login` | none |
| POST | `/api/v1/auth/password` | session |

`POST /api/v1/auth/login` returns `{token, username, isAdmin, expiresAt}`, plus
`"defaultPassword": true` when the submitted password is the well-known lab
password `Admin@321` and the server was started with
`KAIRON_UI_DEFAULT_ADMIN_PASSWORD` set to exactly that value; the dashboard
uses it to show a change-your-password banner. It never blocks the login.
`GET /api/v1/auth/config` may include `loginHint` (only when at least one named
user exists).

### config

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/config` | session |

### disruption-budgets

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/disruption-budgets` | namespace |

### fleet

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/fleet/{resource}` | namespace |
| POST | `/api/v1/fleet/{resource}` | namespace |
| DELETE | `/api/v1/fleet/{resource}/{namespace}/{name}` | namespace |

### images

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/images` | session |
| DELETE | `/api/v1/images/{name}` | session |
| PUT | `/api/v1/images/{name}` | session |

### instancetypes

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/instancetypes` | namespace |

### machines

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/machines` | namespace |
| POST | `/api/v1/machines` | namespace |
| DELETE | `/api/v1/machines/{namespace}/{name}` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/agent-exec` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/agent-file/get` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/agent-file/put` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/console` | console ticket |
| POST | `/api/v1/machines/{namespace}/{name}/console/ticket` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/cpuset` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/exec` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/freeze` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/frozen` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/halt` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/logs` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-capture` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/network-capture` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-capture/{token}` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-drop-reasons` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/timeline` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/metrics` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/fork` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/backups` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/disks` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/nics` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-drops` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-effective` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-flows` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/network-stats` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/pause` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/pressure` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/priority` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/qga/firewall/close` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/qga/firewall/open` | namespace |
| GET | `/api/v1/machines/{namespace}/{name}/qga/fsfreeze-status` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/resume` | namespace |
| ANY | `/api/v1/machines/{namespace}/{name}/sandbox-http/{port}/{rest...}` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/start` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/stop` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/thaw` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/vm-restore-snapshot` | namespace |
| POST | `/api/v1/machines/{namespace}/{name}/vm-snapshot` | namespace |

`GET /api/v1/machines`, `GET /api/v1/migrations` and `GET /api/v1/snapshots`
also accept `?namespace=_all`: every object of that kind in every namespace the
caller may see. With namespace scoping on, a caller without a per-operator
identity gets `403`, and everyone else is filtered per namespace
(administrators see all). No other route treats `_all` specially.

### machinesets

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/machinesets` | namespace |
| DELETE | `/api/v1/machinesets/{namespace}/{name}` | namespace |
| PATCH | `/api/v1/machinesets/{namespace}/{name}/scale` | namespace |

### machinebackuprestores

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/machinebackuprestores` | namespace |

### machinebackups

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/machinebackups` | namespace |
| DELETE | `/api/v1/machinebackups/{namespace}/{name}` | namespace |
| POST | `/api/v1/machinebackups/{namespace}/{name}/restore` | namespace |

### machineclaims

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/machineclaims` | namespace |

### machinepools

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/machinepools` | namespace |

### migration-policies

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/migration-policies` | namespace |

### migrations

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/migrations` | namespace |
| POST | `/api/v1/migrations` | namespace |
| POST | `/api/v1/migrations/evacuate` | session |
| GET | `/api/v1/migrations/{namespace}/{name}` | namespace |
| POST | `/api/v1/migrations/{namespace}/{name}/cancel` | namespace |
| POST | `/api/v1/migrations/{namespace}/{name}/recover` | namespace |

### namespaces

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/namespaces` | session |

### network-policies

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/network-policies` | namespace |

### nodes

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/nodes` | session |
| GET | `/api/v1/nodes/usage` | session |
| GET | `/api/v1/nodes/capacity` | session |
| GET | `/api/v1/nodes/{node}/capabilities` | session |
| GET | `/api/v1/nodes/{node}/topology` | session |
| GET | `/api/v1/nodes/{node}/catalog` | session |
| POST | `/api/v1/nodes/{node}/catalog` | session |
| POST | `/api/v1/nodes/{node}/catalog/clean` | session |
| DELETE | `/api/v1/nodes/{node}/catalog/{name}` | session |
| POST | `/api/v1/nodes/{node}/catalog/{name}/clone` | session |
| POST | `/api/v1/nodes/{node}/catalog/{name}/export` | session |
| POST | `/api/v1/nodes/{node}/catalog/{name}/read-only` | session |
| POST | `/api/v1/nodes/{node}/catalog/{name}/rename` | session |
| POST | `/api/v1/nodes/{node}/egress-check` | session |
| GET | `/api/v1/nodes/{node}/pools` | session |
| POST | `/api/v1/nodes/{node}/pools` | session |
| DELETE | `/api/v1/nodes/{node}/pools/{name}` | session |
| GET | `/api/v1/nodes/{node}/pools/{name}` | session |
| POST | `/api/v1/nodes/{node}/pools/{name}/claim` | session |
| GET | `/api/v1/nodes/{node}/sandboxes` | session |
| GET | `/api/v1/nodes/{node}/templates` | session |
| POST | `/api/v1/nodes/{node}/templates` | session |

### overview

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/overview` | session |
| GET | `/api/v1/insights` | session |
| GET | `/api/v1/selftest` | session |
| POST | `/api/v1/selftest` | session |
| GET | `/api/v1/selftest/scenarios` | session |
| GET | `/api/v1/selftest/{id}` | session |
| DELETE | `/api/v1/selftest/{id}` | session |
| GET | `/api/v1/pods` | session |
| GET | `/api/v1/health` | session |
| GET | `/api/v1/stream` | session |

### quotas

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/quotas` | namespace |

### restores

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/restores` | namespace |
| POST | `/api/v1/restores` | namespace |
| DELETE | `/api/v1/restores/{namespace}/{name}` | namespace |

### security-groups

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/security-groups` | namespace |

### snapshot-schedules

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/snapshot-schedules` | namespace |
| PATCH | `/api/v1/snapshot-schedules/{namespace}/{name}/suspend` | namespace |
| POST | `/api/v1/snapshot-schedules/{namespace}/{name}/trigger` | namespace |

### snapshots

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/snapshots` | namespace |
| POST | `/api/v1/snapshots` | namespace |

### usage.csv

| Method | Path | Access |
|---|---|---|
| GET | `/api/v1/usage.csv` | namespace |

### users

| Method | Path | Access |
|---|---|---|
| POST | `/api/v1/users/{username}/password` | session |

## Related

- [CLI reference](../CLI.md): the `kaironctl` commands that call these routes.
- [Dashboard guide](kairon-ui-dashboard.md): the pages that call these routes.
- [kairon-ui high availability](kairon-ui-ha.md), [OIDC](kairon-ui-oidc.md).
- [VM edge](../ebpf-edge.md): `network-*` routes, packet capture.
- [Enterprise fleet reference](enterprise-fleet-reference.md): `/api/v1/fleet/*`.

## Live data and history

- **`GET /api/v1/stream`** is a Server-Sent Events feed of Machine and MachineMigration changes the caller may see. Each `change` event carries `type` (`ADDED`, `MODIFIED`, `DELETED`), `kind`, `namespace`, `name` and, when known, `phase`, `nodeName` and `message`. A `: ping` comment arrives every 20 s. Read it with `fetch` streaming, because it needs the `Authorization` header.
- **`GET .../timeline`** merges the Machine's Kubernetes Events with its status conditions, newest first.
- **`GET .../metrics?range=15m`** returns the usage samples (CPU percent, memory, disk read and write) kairon-ui recorded every 15 s, up to one hour. The history is in memory, per replica, and lost on restart; `cpuPercent` is the lifetime average `status.resourceUsage` reports.
- **`GET /api/v1/insights`** returns deterministic findings for the Overview's attention strip: failed Machines, Machines unschedulable for 5 minutes or more, migrations in `NeedsRecovery` or in flight for over 30 minutes, nodes that are not Ready with Machines assigned or at least 90% requested, and quotas at 90% or more. Each finding names the `page` and `target` to open. Node findings are omitted for non-administrators when namespace scoping is on.
- **`GET /api/v1/nodes/{node}/topology`** returns the node's `NodeTopology` status (`null` until kairon-node publishes one) and the `spec.topologyAllocation`/`status.appliedTopology` of each Machine on it the caller may see. See [topology-aware placement](machine-topology.md).
- **`GET /api/v1/nodes/capacity`** reports allocatable against requested CPU and memory per node using the scheduler's own accounting. With `?cpu=4&memory=8Gi` each row also says whether a Machine of that size would fit.
- **`GET /api/v1/pods`** lists Kairon's own pods (`app.kubernetes.io/name=kairon-*`); add `scope=all` for every pod the caller may see and `node=NAME` to filter. **`GET /api/v1/health`** rolls those pods up per component. Both need the `kairon-ui` ClusterRole's `pods` and `events` read rules, which the chart and `deploy/rbac.yaml` now grant.

## Actions

- **`POST .../fork`** with `{"count": 1-32, "prefix": "..."}` forks a Running Machine without volumes, disks or device claims, as `kaironctl fork` does (without waiting).
- **`POST .../disks`** with `{"action": "attach"|"detach", "name": "...", "claim": "pvc"}` and **`POST .../nics`** with `{"action": "add"|"remove", "name": "...", "bridge": "br0"}` edit `spec.disks` and `spec.network.extraInterfaces`; kairon-node applies them live. NICs need `spec.network.mode: tap`.
- **`POST .../snapshot-schedules/.../trigger`** requests an immediate run, bypassing `spec.suspend`, as `kaironctl trigger` does.
- **`POST .../machines/{ns}/{name}/backups`** with `{"quiesce": "auto"|"required"|"never", "name": "..."}` creates a `MachineBackup` (Atlas options stay in `kaironctl backup` and YAML). **`POST .../machinebackups/{ns}/{name}/restore`** with `{"machine": "...", "storageClass": "..."}` creates a `MachineBackupRestore`; its disk half waits until the target Machine is halted. **`DELETE .../machinebackups/{ns}/{name}`** removes a backup. These need the `create`/`delete` rules the chart and `deploy/rbac.yaml` now grant on `machinebackups` and `machinebackuprestores`.

## Self-test

The dashboard's self-test is a job API over the same engine as `kaironctl selftest` (see [Self-test](selftest.md)). Starting, listing, polling and cancelling need a named **administrator** account, because a run creates and deletes real Machines; the scenario catalogue is readable by any session.

- **`POST /api/v1/selftest`** with `{"profile": "quick|standard|full", "only": [...], "skip": [...], "noVm": false, "keep": false}` returns `202` and `{"id": "..."}`. Only one run executes at a time (`409` otherwise). A run is limited to 25 minutes.
- **`GET /api/v1/selftest/{id}?after=N`** returns `state` (`running`, `passed`, `failed`, `cancelled`), the `steps` with their status, reason and hint, the log `lines` from index `N`, `next` (use it as the next `after`) and, once finished, a `summary`.
- **`DELETE /api/v1/selftest/{id}`** cancels a run; the engine still deletes the Machines it created.
- **`GET /api/v1/selftest`** lists the last five runs without their lines.
- Runs are kept in memory, per replica, and are lost when kairon-ui restarts.
