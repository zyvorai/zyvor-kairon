# User guide: real Kubernetes RBAC for console access

`ui.auth.rbacConsoleCheck` (Helm chart, off by default) layers a real
Kubernetes `SubjectAccessReview` on top of the existing
`kairon.zyvor.dev/console-allowed-users` annotation allowlist
([`docs/guides/machine-disruption-budgets.md`](machine-disruption-budgets.md)
covers the unrelated `MachineDisruptionBudget` guide this one doesn't
touch -- see [architecture.md](../architecture.md) for the VNC console
relay this gates). With it off (the default), console authorization is
exactly as before: an app-level annotation allowlist enforced entirely
inside `kairon-ui`, with no real Kubernetes RBAC involved at all.

## The key fact this relies on

Kairon's `Machine` CRD only declares a `status` subresource
(`charts/kairon/crds/machines.yaml`) -- there is no way for it to actually
*serve* a real `machines/console` endpoint the way a built-in resource
like `pods/exec` does. But that turns out not to matter:
`kubectl create clusterrole --resource=machines/console --verb=get`
already works today, with **zero CRD changes**, because Kubernetes RBAC
resource strings with a subresource suffix (`machines/console`) are just
strings the authorizer compares against a `Role`/`ClusterRole`'s
`resources:` list -- the same way `nodes/proxy` works as a real RBAC
resource even though "proxying a node" isn't a separate REST object
either. The only missing piece was something actually asking a
`SubjectAccessReview` to evaluate that rule -- which is what this feature
adds.

## Setup

1. Grant console access to a real Kubernetes `User`/`Group`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kairon-console-viewer}
rules:
  - apiGroups: ["kairon.zyvor.dev"]
    resources: ["machines/console"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: alice-console-access}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: kairon-console-viewer}
subjects:
  - {kind: User, name: alice, apiGroup: rbac.authorization.k8s.io}
```

2. Enable the check:

```bash
helm upgrade --install kairon ./charts/kairon -n kairon-system \
  --set ui.auth.rbacConsoleCheck=true
```

This grants `kairon-ui`'s own ServiceAccount `create` on
`subjectaccessreviews.authorization.k8s.io` (needed to ask the question at
all) and makes `handleConsoleTicket` additionally require the answer to be
`Allowed` before issuing a console ticket.

## The one real limitation: whose identity is being checked

`kairon-ui`'s own operator identities (`ui.auth.users[]` local accounts, or
an OIDC ID-token claim) are **app-level identities kairon-ui itself
tracks** -- they are not automatically the same thing as a Kubernetes
`User`/`Group` the API server's RBAC authorizer recognizes, unless you've
*also* configured `kube-apiserver`'s own OIDC authentication against the
same identity provider with matching claim mappings (a separate, real
cluster-admin deployment choice, entirely outside this chart's control).

Concretely:

- **A local `ui.auth.users[]` account has no real Kubernetes `User` at
  all.** With `rbacConsoleCheck` on, a `SubjectAccessReview` for that
  username's literal string will find no matching `RoleBinding` (unless
  you've coincidentally created a `User` subject with that exact name) and
  simply come back `Allowed: false` -- denied, fail closed, same as any
  other unrecognized identity.
- **An OIDC-authenticated operator only gets real RBAC enforcement if
  `kube-apiserver`'s own `--oidc-username-claim`/`--oidc-groups-claim`
  point at the same identity provider and claims `ui.oidc.usernameClaim`
  does** ([`docs/guides/kairon-ui-oidc.md`](kairon-ui-oidc.md)). If they
  don't line up, every OIDC operator is denied the same way a local
  account is.

Treat `rbacConsoleCheck` as an **additional** gate for a cluster that has
already wired OIDC identity through to `kube-apiserver` itself, not a
replacement for the annotation allowlist -- both are enforced together
when both are configured; either one denying is enough to deny console
access.
