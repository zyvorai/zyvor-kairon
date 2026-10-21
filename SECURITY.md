# Security policy

Kairon is Apache-2.0 open-source software from [Zyvor](https://zyvor.dev). It controls host-level VM execution. Treat the controller, node agents, FluxVM and any migration adapter as privileged infrastructure even when the Kairon process itself runs as a non-root container.

## Reporting a vulnerability

Please report security issues privately to **security@zyvor.dev** or through [GitHub private vulnerability reporting](https://github.com/zyvorai/kairon/security/advisories/new). Do not open a public GitHub issue for unfixed vulnerabilities.

Include the affected version or commit, the component (`kairon-controller`, `kairon-node`, `kairon-ui`, `kaironctl`, CSI, Helm chart), reproduction steps, and impact.

- We acknowledge reports within **14 days** (normally much sooner).
- We aim to release a fix for medium-or-higher severity issues within **60 days** of the report, and faster for critical ones.
- Fixes ship in a new release; the release notes and a GitHub Security Advisory identify the vulnerability (CVE where assigned) and credit the reporter unless they ask otherwise.

Security fixes are made on `main` and released in the next tagged version; only the latest release is supported.

## v0.3 migration security

- Users cannot submit a raw QEMU/TCP/exec migration destination. The target is a Kubernetes node selected by Kairon.
- Node-to-node migration control uses TLS 1.3 and `RequireAndVerifyClientCert`.
- Peer API responses use `Cache-Control: no-store`.
- The destination session ID is deterministic but contains no secret. Transfer endpoints remain inside the mTLS peer exchange and are not copied into CRD status.
- Destination session files are mode `0600` and written using fsync + atomic rename.
- Reusing a session ID with different immutable VM/source/target identity returns HTTP 409.
- A target that has no adapter returns `Unsupported`; the source VM is not touched.
- Source transfer failure aborts the prepared target.
- Commit ambiguity produces `NeedsRecovery`; Kairon does not automatically restart/cut over and risk split brain.
- The post-commit adopt-only guard refuses to create a fresh target runtime if the expected incoming VM cannot be found.

The baseline Helm deployment uses a shared cluster migration certificate with both server/client authentication EKUs for the control-plane peer connection (`migration.tlsSecretName`) -- deliberately: peers are addressed by Kubernetes node InternalIP, not a stable per-host name, so this proves "is this a Kairon node," not "is this host X.Y.Z.W" (see `internal/migration/tls.go`). For stronger node identity there, use per-node credentials or SPIFFE-style workload identity and configure the expected server name appropriately.

The **data plane** -- the actual QEMU RAM/state stream (`migration.dataplaneTls`) -- is different: real per-connection hostname verification already exists (the source adapter passes the real destination host as the expected TLS name), so a shared identity there is a real, closeable weakness, not a deliberate tradeoff. `migration.dataplaneTlsSecretName` (opt-in, `scripts/gen-migration-mtls-certs.sh` generates both the certs and a ready-to-apply Secret in the right shape) gives every node its own distinguishable cert instead: each `kairon-node` pod's init container picks out only its own node's three files from the shared Secret (matched via the Downward API's `spec.nodeName`), and fails the pod outright rather than falling back to a shared or wrong identity if its own entry is missing. Left unset (the default), `dataplaneTls` still falls back to staging the shared control-plane cert onto the data plane too -- weaker, but exactly this project's behavior before `dataplaneTlsSecretName` existed, so no existing deployment's `helm upgrade` changes anything on its own. See `docs/guides/machine-migration-tls.md`.

## DRA / VFIO

A `ResourceClaim` must be allocated before Kairon considers it. Resolved PCI BDFs are syntax checked and must be present in the node administrator's explicit allowlist. Empty allowlists deny passthrough.

## Images

`--image-root` constrains Machine image paths. Keep VM image directories non-writable by untrusted workloads. CI builds all five container images (`controller`, `node`, `ui`, `csi-node`, `csi-controller`), pins their base images to a resolved digest rather than a floating tag (`gcr.io/distroless/static-debian12` for the first three; `debian:12-slim` for `csi-node`/`csi-controller`, which need a real iSCSI initiator/`targetcli`/filesystem tooling distroless can't provide -- see the Dockerfile's own comments for why), and runs both `govulncheck` against the Go dependency graph (every package except the Helm CLI, see `docs/DEPENDENCIES.md`) and a Trivy image-layer scan (failing on HIGH/CRITICAL) on every push. Since a digest pin freezes whatever package versions were current the day it was built, `csi-node`/`csi-controller`'s own Dockerfile stages additionally run `apt-get upgrade` before installing anything, pulling in Debian security patches published since -- the first real run of this scan (previously blocked by an unrelated CI issue) caught 3 real HIGH-severity `libpcre2-8-0` CVEs this closes. A separate tag-triggered workflow (`.github/workflows/release.yml`) publishes to `ghcr.io/zyvorai/kairon-*` -- this repository's CI is now the one place that mints those tags; it scans the exact bits it's about to push (not a separate build) and refuses to push anything the scan flags.

Every published image is also SBOM'd and signed: `anchore/sbom-action` (wrapping `syft`) generates a real SPDX SBOM from the exact pushed image (by digest, not tag -- a tag is mutable, a digest isn't), `cosign` signs that digest and attests the SBOM to it, keylessly -- no private key this project generates, stores, or can leak; the GitHub Actions job's own short-lived OIDC token (`id-token: write`) is exchanged for a Fulcio-issued certificate scoped to this exact workflow/repo/ref, and the signature is recorded in Sigstore's public Rekor transparency log. Verify either with `cosign`:

```bash
CERT_ID='^https://github.com/zyvorai/kairon/\.github/workflows/release\.yml@refs/tags/.*$'
ISSUER='https://token.actions.githubusercontent.com'

cosign verify --certificate-identity-regexp "$CERT_ID" --certificate-oidc-issuer "$ISSUER" \
  ghcr.io/zyvorai/kairon-controller:vX.Y.Z

cosign verify-attestation --type spdxjson --certificate-identity-regexp "$CERT_ID" --certificate-oidc-issuer "$ISSUER" \
  ghcr.io/zyvorai/kairon-controller:vX.Y.Z
```

Confirms two different things: `verify` proves the image byte-for-byte is what `release.yml` actually built and pushed for that tag (not tampered with, not substituted); `verify-attestation` additionally proves the attached SBOM is the real one generated from that same image, not a hand-edited or stale one. Each SBOM is also uploaded as a plain build artifact on the release workflow run, for anyone who just wants to read it without `cosign` at all. First shipped for the next tagged release after `v0.4.0` -- earlier tags were never signed or SBOM'd retroactively.

## `kairon-controller` leader election (`controller.leaderElection`)

`controller.replicaCount > 1` is coordinated by a `coordination.k8s.io/v1` Lease (`internal/leaderelection`), so only the elected leader's reconcile loop runs -- otherwise two unsynchronized replicas would race writing `spec.nodeName` and migration/snapshot/quota status. On by default (even at `replicaCount: 1`, where it's a no-op single-writer race) via a namespaced `Role`/`RoleBinding` this chart creates, scoped to `get/list/watch/create/update` on `leases` in `kairon-controller`'s own namespace only -- not cluster-wide, and not a resource any other component touches. The admission webhook and health/metrics endpoints are deliberately *not* gated by this: both are stateless per-request reads, safe to serve from every replica regardless of who holds the Lease.

Every failure mode here (a `Lease` read/write error, ambiguity about who currently holds it) resolves to "not leader" rather than reconciling anyway -- the same fail-closed posture the webhook's TLS cert/key pairing already has. `cmd/kairon-controller`'s own `-leader-elect` flag defaults to `false` (the opposite of the Helm chart's default) specifically so an existing bare-metal/systemd deployment's binary upgrade never silently starts refusing to boot over a newly-required namespace/RBAC it doesn't yet have -- see [`docs/guides/kairon-controller-ha.md`](docs/guides/kairon-controller-ha.md).

## MachineQuota / MachineDisruptionBudget admission

Both are enforced twice now: in-process (`kairon-controller`'s reconcile loop for quota, `kaironctl evacuate` for disruption budgets -- unchanged from before) and, when `webhook.enabled` is set, by a real validating admission webhook on `kairon-controller` that rejects a `Machine` create that would immediately exceed a `MachineQuota`, or a `MachineMigration` create that would violate a `MachineDisruptionBudget` -- closing the direct-API-write bypass both previously had. It reuses the exact same decision functions the reconcile loop and `kaironctl` already used (`internal/controller/quota.go`, `internal/controller/disruption.go`), not a second implementation to keep in sync.

Off by default: like `migration.tlsSecretName`/`console.tls.secretName`, this chart does not mint the webhook's serving certificate or `webhook.caBundle` for you -- both are required to enable it. `webhook.failurePolicy` defaults to `Fail`: a webhook outage blocks all `Machine`/`MachineMigration` writes cluster-wide rather than silently letting the bypass back in; set it to `Ignore` to trade that guarantee for availability. `MachineDisruptionBudget` enforcement, and `MachineQuota`'s Machine-`CREATE` check, only ever evaluate `CREATE`, matching the reconcile loop's own semantics exactly (`internal/controller/disruption.go`, `quota.go`) -- deliberately never stricter than the enforcement they backstop. `MachineQuota` also evaluates `UPDATE` now: `validateMachineResize` (`internal/controller/webhook.go`) denies growing an already-scheduled Machine's `spec.resources` (a hotplug resize) past its namespace's quota. This one *is* new enforcement, not just closing a bypass -- `kairon-node`'s hotplug reconciliation (`internal/agent/hotplug.go`) is per-node with no cluster-wide `MachineQuota` visibility of its own, so with `webhook.enabled` false (the default) a resize past quota still isn't caught anywhere at all.

## MachineNetworkPolicy / NetworkSecurityGroup admission

Unlike the two sections above, this one closes a gap with no pre-existing in-process decision function to reuse: until now, nothing anywhere in Kairon validated the free-form `AllowCidrs`/`DenyCidrs`/`AllowPorts` string fields of a `MachineNetworkPolicy`/`NetworkSecurityGroup`'s `spec.policy` at write time. FluxVM's own eBPF dataplane does validate them (`crates/fluxvm-network/src/ebpf.rs`'s `validate_policy`, called from its `UpsertNetworkGroup`/`SetVMNetworkPolicy` handlers) -- but only once `kairon-node`'s agent actually tries to apply the policy to a real Machine, by which point a syntax error (a CIDR missing its `/prefix`, an unsupported L4 protocol, `maxEgressMbps: 0`) just parks the object in `status.phase: Error` and gets retried, forever, on every subsequent reconcile tick, on every node the policy selected a Machine on -- a mistake that can never self-heal on its own, since the same malformed spec is reapplied unchanged each time.

When `webhook.enabled` is set, `kairon-controller`'s admission webhook now rejects both CRDs' `CREATE` and `UPDATE` outright with a specific, actionable message (`model.ValidateVmNetworkPolicy`, `internal/model/network.go`) -- reimplementing FluxVM's own CIDR/port-rule grammar in Go rather than importing it, the same small-helper-duplication convention `internal/controller/webhook.go`'s `validateImageSource` already follows for `internal/agent`'s own `validateImageSource`. `UPDATE` is checked here (unlike `MachineMigration` above, which only ever needs `CREATE`): both CRDs are routinely edited in place with `kubectl edit`, and the agent reapplies whatever `spec.policy` currently holds on every tick regardless of which operation last changed it.

Off by default along with the rest of `webhook.enabled` -- with it off, a malformed policy still isn't caught until an agent tries to apply it, same as always.

## Machine / MachineQuota resource-quantity admission

A third, unrelated gap of the same *kind* as the one above: `Machine.spec.resources.{cpu,memory,maxCpu,maxMemory}` and `MachineQuota.spec.{maxTotalCpu,maxTotalMemory}` are all free-form strings parsed by `model.ParseVCPUs`/`model.ParseMemoryMiB` (`internal/model/quantity.go`), and until now nothing checked their syntax at write time either. For a `Machine`, a malformed value sailed through `kubectl apply` and only ever failed once `kairon-controller` actually tried to create the VM (`internal/fluxvm`'s `buildCreateRequest`) -- and in the meantime, `MachineFootprint`'s own parse calls (`internal/controller/quota.go`) silently treat the same malformed value as a *zero* footprint, so the Machine evaded `MachineQuota` accounting the entire time it sat stuck. For a `MachineQuota`, the blast radius is worse: `BuildQuotaTrackers` parses every quota's `maxTotalCpu`/`maxTotalMemory` on every reconcile tick, and a single malformed one anywhere in the cluster aborts quota enforcement and Machine scheduling cluster-wide, not just for its own namespace.

When `webhook.enabled` is set, `kairon-controller`'s admission webhook now rejects a malformed `Machine` `CREATE` or resize `UPDATE` (`validateResourceQuantities`, called from both `validateMachineCreate` and `validateMachineResize`) and a malformed `MachineQuota` `CREATE`/`UPDATE` (`validateMachineQuotaObject`, a new `/validate-machinequota` route -- deliberately not folded into the existing `machinequota.kairon.zyvor.dev` webhook entry, which validates *Machines* against quota limits, not a `MachineQuota` object's own syntax). `Machine.spec.resources.cpu`/`memory` are only checked when non-empty: a `Machine` naming `spec.instanceTypeName` instead is expected to leave `spec.resources` entirely unset until `kairon-controller`'s `resolveInstanceTypes` (`internal/controller/instancetype.go`) fills it in from the referenced `MachineInstanceType` -- rejecting that here would break a working, documented pattern, not close a gap.

Off by default along with the rest of `webhook.enabled`, and admission-time prevention only: an already-existing malformed `MachineQuota` (created before this existed, or with the webhook disabled) still aborts cluster-wide quota enforcement exactly as before this change -- containing that blast radius for an object that already slipped through would mean changing `BuildQuotaTrackers`'s own error contract, a separate, more invasive change left out of scope here.

## Machine network MAC-address admission

A fourth instance of the same family of gap, but for a field with no FluxVM-specific grammar to mirror at all: `Machine.spec.network.mac` is a free-form string passed straight through, unparsed, to FluxVM's `CreateVmRequest.network.mac` field (see `docs/network-fabric.md`'s field-mapping table -- it's valid on both VM create and edit), and until now nothing on either side of Kairon ever checked its syntax. A malformed value (a wrong separator, non-hex characters, the wrong number of octets) sailed through `kubectl apply` and only ever failed once `kairon-node` actually asked FluxVM to attach the NIC, at VM create or edit time -- the same "stuck forever with no self-heal" failure mode the two admission checks above already close for CIDR/port and CPU/memory syntax.

When `webhook.enabled` is set, `kairon-controller`'s admission webhook now rejects a malformed `spec.network.mac` on both a `Machine` `CREATE` and an `UPDATE` (`validateNetworkMAC`, called from both `validateMachineCreate` and `validateMachineResize` -- reusing the existing `machinequota.kairon.zyvor.dev` webhook entry and its `/validate-machine` route rather than adding a new one, since it already covers every Machine `CREATE`/`UPDATE`). `net.ParseMAC` (Go's standard library) also accepts an 8-byte EUI-64 form and a 20-byte InfiniBand link-layer form; only the 6-byte EUI-48 form is a valid Ethernet NIC address, which is what every one of FluxVM's network modes (`user`/`tap`/`macvtap`) actually attaches, so those other two widths are rejected too, not silently accepted as some other kind of "valid" hardware address. An unset `spec.network.mac` (FluxVM assigns one itself) is always allowed, unconditionally.

## CRD conversion webhook (scaffold, not yet live)

The same `kairon-controller` webhook server also exposes `POST /convert/machinequotas`, implementing the `apiextensions.k8s.io/v1` `ConversionReview` contract (`internal/conversion`) -- on the identical TLS listener, certificate, and `Service` as the admission webhook above, so it introduces no new trust boundary or attack surface beyond what `webhook.enabled` already opens up. All served API versions share one schema (the default `None` conversion strategy applies), so the real API server has no reason to ever call this route today; it exists and is tested (a real worked `MachineQuota` field-rename conversion, verified through the real HTTP envelope, not just the converter function in isolation) so that cutting a schema-changing version later is "wire an existing, proven route into a CRD's `spec.conversion`," not "build conversion webhook machinery from scratch." See [`docs/guides/crd-versioning.md`](docs/guides/crd-versioning.md).

## `kairon-ui` (dashboard)

**Transport.** `kairon-ui` serves HTTPS by default (TLS 1.2 or later): the sign-in form, session tokens and the legacy bearer token never cross the network in clear text unless an operator opts out by name. With no certificate configured it generates a self-signed ECDSA certificate at startup (browsers warn, and it changes on every restart; names cover localhost, loopback, the hostname, interface addresses and `KAIRON_UI_TLS_SANS`). Supply `-tls-cert`/`-tls-key` (Helm `ui.tls.secretName`, `deploy-remote.sh --ui-tls-cert/--ui-tls-key`) for a stable certificate; the pair is reloaded when the files change, and one without the other is a startup error. Plain HTTP requires `-insecure-http` / `KAIRON_UI_INSECURE_HTTP=true` (Helm `ui.insecureHttp`, `--ui-insecure-http`), logs a warning, is refused together with a certificate, and is reported as a warning by `kaironctl connectivity test`; use it only where a mesh or proxy terminates TLS in front of the pod. When the certificate is self-signed, uploaded image sources carry `insecureSkipTLSVerify` (the pinned sha256 still guarantees the bytes; only the server's identity is unverified). Clients should trust the certificate (`KAIRON_UI_CA`, `curl --cacert`) rather than disable verification. Not covered by this default and tracked separately: the kairon-ui to kairon-node console relay (`console.tls.enabled`, below), node health/metrics ports, and FluxVM's loopback API.

`kairon-ui` supports two auth modes, and accepts either whenever both are configured:

- **Real per-operator username/password login** (`ui.auth.users`, or a pre-created Secret via `ui.auth.existingSecret`): each account is a username plus a bcrypt hash of its password (`golang.org/x/crypto/bcrypt`; generate one with `kairon-ui -hash-password 'the password'` -- passwords are never stored or logged in plaintext). A successful login issues a signed, 12-hour session token (`base64url(payload).base64url(HMAC-SHA256(payload, sessionSecret))`, verified with `hmac.Equal`); `POST /api/v1/auth/logout` revokes it early via a small in-memory revocation list, applied immediately on the replica that served the logout and, once `ui.replicaCount > 1`, mirrored to every other replica within ~15s via a shared `ConfigMap` -- see `docs/guides/kairon-ui-ha.md`. A wrong password and an unknown username return the identical response, in the same time (a dummy bcrypt compare runs either way), to avoid leaking valid usernames. Every mutating request is now attributed to the authenticated username in the audit log, not just logged as an anonymous event -- the gap this section used to describe as unsolved.
- **Legacy shared bearer token** (`ui.token`/`ui.existingSecret`, compared in constant time via `crypto/subtle.ConstantTimeCompare`): kept working unchanged for existing deployments. **This token is still shared by every operator** -- no per-user identity, no expiry, no rotation short of a manual `helm upgrade --set ui.token=...` followed by a redeploy. Treat it like a shared root password and prefer migrating to `ui.auth.users` when you can.

Fail-closed by default: the binary's startup check refuses to run unless at least one of a token, a configured user, or `-allow-unauthenticated` (local development only) is present. The Helm chart goes further -- if an install sets none of `ui.token`, `ui.existingSecret`, `ui.auth.users`, `ui.auth.existingSecret`, or `ui.allowUnauthenticated`, it now **seeds one default `admin` account with a random password it generates once and persists across upgrades** (see `helm install`/`upgrade` NOTES for the retrieval command), rather than either failing the install or coming up unauthenticated. Setting any of those values explicitly opts out of the generated default entirely.

**Well-known default password (lab default on fresh evaluation installs).** The Helm chart's seeded `admin` account now uses the documented lab password `Admin@321` on a *fresh* install when nothing else sets a password (`ui.auth.wellKnownDefault: true`, the default); the sign-in page then says so. It never replaces the password of an existing `kairon-ui-session` Secret on upgrade, an explicit `ui.auth.defaultAdminPassword` always wins, and `values-production.yaml` sets `wellKnownDefault: false` (random password). Treat any install that uses the lab default as reachable-by-anyone-who-can-reach-the-port. The paragraph below describes the same default for `scripts/deploy-remote.sh`.

**Well-known default password.** `scripts/deploy-remote.sh --with-ui` seeds the dashboard's built-in `admin` account with the documented default password `Admin@321` (override with `--ui-admin-password=PW`; use `--reseed-ui-auth` to apply it to an existing `kairon-ui.env`), plus a generated `KAIRON_UI_SESSION_SECRET`. The Helm chart exposes the same convenience as `ui.auth.defaultAdminPassword` but leaves it empty by default (random password, as above). The `kairon-ui` binary never contains a built-in password and still refuses to start without a token, a user, or `-allow-unauthenticated`. A published default is guessable by anyone who can reach the dashboard port: the 5-failures/5-minutes lockout slows but does not stop that, so change it (or set `--ui-admin-password`) before exposing the dashboard beyond a lab network, and prefer named `ui.auth.users` entries for real deployments.

The dashboard's ClusterRole grants exactly the verbs its handlers use (`create`/`delete`/`patch` on Machines, `create`/`patch` on MachineMigrations, `get`/`list`/`watch` elsewhere) -- it cannot do anything through the Kubernetes API that isn't already reachable through `kaironctl` with the same privilege level.

`GET /metrics`, when `Server.Metrics` is configured, is unauthenticated by design -- same convention as `/healthz`/`/readyz`, since a Prometheus scrape config never carries the bearer token either. Unlike `kairon-controller`/`kairon-node`'s own `/metrics` (served on a separate health-check port, typically not the same one exposed for browser access), kairon-ui serves it on the *same* listen port as the dashboard itself -- so if that port is reachable from wherever operators reach the dashboard (an Ingress, a LoadBalancer), `/metrics` is reachable there too. The data it exposes is aggregate request counts/latencies by method and status class, not per-request detail (no paths, no usernames, no request bodies) -- low sensitivity, but still real information disclosure to anyone who can reach the dashboard's URL. Restrict at the network layer (`ui.networkPolicy`, an Ingress rule scoping `/metrics` to your monitoring namespace) if that's a concern for your deployment.

Repeated failed logins against one username are rate-limited: 5 failures lock that username out for 5 minutes (`Retry-After` header on the `429`), tracked per raw requested username -- including unknown ones -- so the lockout behavior itself never leaks which usernames are real. A successful login clears the counter.

Separately, every route (`-rate-limit-rps`/`-rate-limit-burst`, default 20 req/s sustained with bursts to 40, on by default) is now rate-limited per remote address, via a small hand-rolled token-bucket limiter (`internal/ratelimit` -- stdlib-only, so this didn't need a third exception to this project's Go-stdlib-only design guarantee). This is a different, broader protection than the login lockout above: it bounds request *volume* from one client address against anything -- including an already-authenticated client hammering an unrelated route, or unauthenticated traffic against `/api/v1/auth/login`/`/api/v1/auth/config` itself -- not just repeated failed passwords against one username. Applied outermost, ahead of routing, so it protects `/healthz`/`/readyz`/`/metrics` too. `-rate-limit-rps=0` disables it.

Keyed by `r.RemoteAddr` alone by default: behind a Kubernetes `Service`/`Ingress`/load balancer with no forwarded-header trust configured, every client can appear as the same address, collapsing everyone's buckets into one. `-trusted-proxy-header`/`-trusted-proxy-cidrs` (Helm: `ui.rateLimit.trustedProxyHeader`/`.trustedProxyCidrs`, both empty by default -- unchanged behavior) close this: when both are set, `kairon-ui` reads the real client address from the named header (e.g. `X-Forwarded-For`, taking its leftmost entry -- the first hop's own claim about the original client) instead of the TCP peer, but *only* when that TCP peer itself falls inside `trustedProxyCidrs`. That check is load-bearing, not optional ceremony: without it, any client talking directly to `kairon-ui` -- bypassing the load balancer, or reaching a route the load balancer doesn't otherwise gate -- could set the header itself to an arbitrary value, either evading its own rate limit entirely (a fresh, never-throttled key every request) or deliberately colliding with another real client's bucket to throttle them out. Startup refuses (fails closed) if only one of the two flags is set, or if a `-trusted-proxy-cidrs` entry doesn't parse as a CIDR.

Password changes no longer require a redeploy: a signed-in operator can change their own password (`POST /api/v1/auth/password`, re-proving the current one first), and an admin account (`ui.auth.users[].admin: true`, or the legacy shared token, which is already root-equivalent for every other route) can reset another operator's password (`POST /api/v1/users/{username}/password`) -- which also immediately invalidates that operator's outstanding sessions, since there's no server-side list of every issued token to individually revoke otherwise. Both persist the change back into the `kairon-ui-users` Secret via a Kubernetes API write kairon-ui now has (scoped by a namespaced `Role` to `get`/`patch` on exactly that Secret, nothing broader), and the chart's `kairon-ui-users` template preserves whatever is already in that Secret across a `helm upgrade` rather than resetting it from `ui.auth.users` every time. This is only wired up when the chart itself owns that Secret (not `ui.auth.existingSecret`, which may be managed by something else) and only for named `ui.auth.users` accounts -- the Helm-seeded random-password default `admin` account still can't change its own password in place; graduate to a named account for that.

Known limitations of the username/password mode: with `ui.replicaCount > 1`, session revocation and login lockout propagate across replicas within ~15s (not instantly), via a shared Kubernetes `ConfigMap` rather than an external store like Redis -- see `docs/guides/kairon-ui-ha.md` for the full model and its real limits.

**OIDC/SSO** (`ui.oidc.enabled`, off by default): an Authorization Code + PKCE flow against an external identity provider, alongside -- not instead of -- the two modes above. This is the one place in Kairon that breaks the project's Go-stdlib-only design guarantee (`golang.org/x/oauth2`, `github.com/coreos/go-oidc/v3`): real JWT/JWK signature verification isn't something this codebase hand-rolls.

**`kaironctl` (Cobra + Helm SDK):** the CLI embeds the Kairon Helm chart (`go:embed` in package `charts`) and uses `helm.sh/helm/v3` (and transitive `client-go`) for install/upgrade/uninstall. Controller and node stay stdlib-only. Full policy: [`docs/DEPENDENCIES.md`](docs/DEPENDENCIES.md). See also [`docs/CLI.md`](docs/CLI.md).

**Cilium CRDs (opt-in):** when `network.ciliumAttach.enabled` or `network.ciliumPolicySync.enabled` is set, `kairon-controller`'s ClusterRole gains create/patch/delete on `cilium.io` `ciliumexternalworkloads` and/or `ciliumnetworkpolicies`. Those calls are raw REST (`internal/kube`); the Cilium Go SDK is not a dependency. Both flags default off, so a cluster without Cilium grants nothing extra. Admission rejects `ciliumAttach` unless `mode` is `tap` and `netns` is true.

`GET /api/v1/auth/oidc/login` redirects to the provider with a random PKCE code verifier and nonce, both signed into the OAuth2 `state` parameter (HMAC-derived from `sessionSecret`, domain-separated from real session tokens -- see `internal/uiapi/oidc.go`) rather than kept in any server-side store. The callback verifies the ID token's signature, issuer, audience, expiry, and nonce before issuing a normal kairon-ui session token -- from there on, an OIDC-authenticated session is indistinguishable from a password one to every other route. Admin capability defaults to the same as before OIDC existed -- exclusively a `ui.auth.users[].admin: true` local account -- but `ui.oidc.adminGroups` (opt-in, empty by default) can now also grant it: an OIDC session whose ID token carries one of the configured groups (`ui.oidc.groupsClaim`, recorded on the session at login time) is treated as admin, re-checked against the *current* `ui.oidc.adminGroups` config on every request via `Server.isAdminIdentity` -- so a config change takes effect on that session's very next request, without a re-login, the same freshness the static `ui.auth.users[].admin` path already had. This does let an OIDC-admin session call `POST /api/v1/users/{username}/password` to reset another operator's password, same as any other admin identity -- an OIDC identity still has no password of its own in Kairon to reset. See `docs/guides/kairon-ui-oidc.md`.

## VNC console (`console.enabled`)

Graphical VNC access to a Machine's QEMU display is a relay, not a new capability FluxVM itself exposes remotely: `browser (noVNC) -> kairon-ui -> kairon-node -> <workspace>/vnc.sock`. Understand the trust chain before enabling this on a shared cluster:

- **FluxVM's own VNC socket has no authentication or encryption of its own** -- it's a bare Unix-domain RFB server (`-vga std -vnc unix:<workspace>/vnc.sock`, no `password-secret`, no TLS). Anyone who can reach that socket file gets a full graphical session, full stop. This is a FluxVM-side property Kairon cannot change; the entire security of the feature rests on the layers around it.
- **kairon-ui's browser-facing WebSocket is gated by a single-use, ~30-second ticket**, not a bearer header (a native browser `WebSocket` can't set one). The ticket is minted only after normal operator auth succeeds (`POST .../console/ticket`, behind the same `withAuth` as everything else) and is consumed on first use -- it's useless if intercepted a moment later. `issueConsoleTicket` fails the request outright (`500`, no ticket issued) if its own `crypto/rand` read ever fails, rather than risk minting a partially- or entirely-zero-value (and therefore guessable) ticket -- `crypto/rand.Read` only guarantees a full, unpredictable fill when it returns no error.
- **kairon-ui's hop to kairon-node is a shared bearer token** (`KAIRON_NODE_CONSOLE_TOKEN`, one value cluster-wide, generated by Helm the same way `kairon-ui-session` is), carried over **one-way TLS by default** when the console is enabled: `console.tls.enabled` is true, the chart generates a private CA and a server certificate for the fixed name `kairon-node` (Secret `kairon-console-tls`, kept across upgrades), mounts it into kairon-node, and has kairon-ui verify that name (`KAIRON_NODE_CONSOLE_SERVER_NAME`), so one certificate serves every node whatever its address. Bring your own with `console.tls.secretName` (`tls.crt`/`tls.key` for the node, `ca.crt` for kairon-ui; set `console.tls.serverName` if its names differ from the dialled address). It is not mutual TLS, since the shared token above already authenticates kairon-ui to kairon-node. Plain HTTP is `console.tls.enabled: false` (or `deploy-remote.sh --console-insecure-http`), for a hop already protected another way. Treat the token like the migration mTLS material regardless: a cluster-internal secret, not one to expose outside the cluster network.
- **Every console ticket is bound to the username that requested it** at issuance time (not just logged as an anonymous event), and each console session logs a `uiapi console opened`/`uiapi console closed` audit line (username, namespace, name, remote address, duration) -- closing the "no session/audit trail for console usage" gap from the first pass. A session opened via the legacy shared `ui.token` still logs with an empty username, the same limitation that token has everywhere else.
- **The dashboard hides the Console button proactively** when `console.enabled` is off or the Machine isn't `Running` on a `qemu` (or unset/`auto`) backend, via `GET /api/v1/config`, rather than only failing after the click.
- **Per-Machine authorization is opt-in** via the `kairon.zyvor.dev/console-allowed-users` annotation, a comma-separated list of kairon-ui operator usernames. Unset (the default) is unchanged from before this existed: every authenticated operator may open the console. Set, `handleConsoleTicket` (`internal/uiapi/console.go`) denies ticket issuance with a `403` to anyone not on the list, except an admin identity (a `ui.auth.users[].admin` account, or an OIDC session in `ui.oidc.adminGroups` -- see the OIDC section above), which always bypasses it for break-glass access. A caller with no real per-operator identity -- the legacy shared `ui.token`, or fully unauthenticated dev mode -- is denied outright whenever a Machine sets this annotation, since there's no identity to check against an allowlist: fail closed rather than silently allow. The ticket itself is also now bound to the one Machine it was issued for (not just the requesting operator), so a ticket minted for a Machine the caller can access can't be replayed against a different Machine's console within its 30-second TTL.
- **Real Kubernetes RBAC is also available, opt-in** (`ui.auth.rbacConsoleCheck`) -- layered alongside the annotation allowlist above, a `SubjectAccessReview` against the `machines/console` subresource must also allow it. Only meaningful for an operator identity `kube-apiserver`'s own RBAC recognizes (an OIDC claim mapped into `kube-apiserver`'s own OIDC config); a local `ui.auth.users[]` account has no real Kubernetes `User` to check and is denied outright when this is on. See [`docs/guides/kairon-ui-console-rbac.md`](docs/guides/kairon-ui-console-rbac.md).
- **QEMU-only**: Cloud Hypervisor and Firecracker Machines have no display device at all (confirmed in FluxVM's own source) and are refused with a clear error, not a silent no-op.
- **kairon-node's OS user needs real filesystem access to `vnc.sock`**: FluxVM creates it root-owned, mode `0755` (no `other` write bit) -- confirmed against a real deployment, where `kairon-node`'s deliberately unprivileged `kairon` service user got `permission denied` dialing it, a clear 502 rather than a silent hang, but a real prerequisite operators must satisfy (e.g. a POSIX ACL granting the node's user `rw` on FluxVM's per-VM socket files, or running FluxVM and kairon-node under a shared group) before `console.enabled` actually works, not just renders.

Off by default (`console.enabled: false`); enabling it is a deliberate trade of convenience for the trust-chain above, appropriate for a trusted operator team, not a multi-tenant or hostile-network environment. The bare-metal install path (`scripts/deploy-remote.sh --with-ui --with-console`) generates and wires the same token end to end; `--console-tls` now automates TLS for that hop too -- by default it generates a private CA + server certificate locally and installs it on the remote host (SAN list is a best-effort guess at the host's own addresses, since kairon-ui actually dials whatever Kubernetes reports as the Machine's Node's InternalIP, which the script can't always know in advance -- a TLS verification error names the mismatch, fix it with an explicit `--console-tls-cert`/`-key`/`-ca`), or accepts your own cert/key/CA via those same flags.

**Firewall the relay port on bare metal.** `kairon-node` listens on every interface for its console relay (`--console-port`, default 8090). That port reaches guest exec, files, the console and the TCP tunnel behind the one shared bearer token, so it must only be reachable from `kairon-ui` and any other in-cluster client. Pin the port (`deploy-remote.sh --console-port=N`; it otherwise moves on redeploy) and drop everything else at the host firewall, for example with an `INPUT` chain that accepts loopback, the node's own address and the pod/`kairon-ui` range and drops the rest. A relay port found open to the internet on a test host was being scanned within hours: if yours was, rotate `KAIRON_NODE_CONSOLE_TOKEN` as well.

## Guest exec (`spec.guestAgent.enabled`)

Running a command inside a guest -- `browser -> kairon-ui -> kairon-node -> FluxVM's real qemu-guest-agent guest-exec` -- rides the same relay infrastructure as the VNC console above (`internal/consoleproxy`, the same `KAIRON_NODE_CONSOLE_TOKEN`/`console.tls` trust chain), but is a meaningfully bigger capability -- arbitrary code execution in the guest, not just viewing its screen -- so it's gated more strictly:

- **Admin-only, with no finer-grained permission yet**: `handleExec` (`internal/uiapi/exec.go`) requires the caller pass `Server.isAdminIdentity` outright, denying every non-admin identity regardless of the `kairon.zyvor.dev/console-allowed-users` annotation (which, for console access, defaults to allowing any authenticated operator). This is a deliberate, documented limit for a first cut, not a place to loosen without adding a real per-operator exec permission first.
- **An OIDC-only identity can pass this check, but only via `ui.oidc.adminGroups`** (opt-in, empty by default) -- see the OIDC section above. Without it configured, the prior behavior is unchanged: an OIDC-authenticated operator needs a *local* `ui.auth.users[]` admin record too, not just a successful OIDC login.
- **`ui.auth.rbacConsoleCheck`, when enabled, still applies on top** of the admin check (both `consoleAuthorized`'s per-Machine annotation allowlist and, if configured, its `SubjectAccessReview` against `machines/console` must also pass) -- an admin account is never exempt from a real Kubernetes RBAC denial.
- **Requires `spec.guestAgent.enabled`** (the same flag used for guest-IP resolution and `MachineSnapshot` quiesce) -- guest-exec is real qemu-guest-agent, QEMU-only in practice since that's the only backend with a functioning QGA channel today.
- **No command-line auditing**: `uiapi exec requested` logs who ran something, on what Machine, and when -- deliberately not *what* was run, since a command's own arguments can carry secrets (e.g. a token passed on the command line). If you need a full command transcript for compliance, that's a real, open gap, not something this logs today.
- **A stalled command has no way to attach to it after the fact**: FluxVM's own guest-exec does its own guest-exec-status polling internally and returns exactly once, on completion or its own timeout (60s default, `timeoutSeconds` overridable) -- there is no session to reconnect to, no partial output streamed as it happens.
- **Same VNC-socket-permission caveat as the console above doesn't apply here** -- guest-exec goes over vsock/virtio-serial via FluxVM's own process, not a filesystem socket kairon-node dials directly, so there's no equivalent local-file-permission prerequisite.

Off by default in practice (no admin account exists until an operator creates one, and `spec.guestAgent.enabled` is itself opt-in); once both are true, treat exec the same way you'd treat `kubectl exec` into a privileged pod -- convenient for a trusted admin team, not something to expose to less-trusted operators without a finer permission model this project doesn't have yet.

## Text console (`spec.guestAgent.console`)

An interactive shell inside the guest -- `browser (xterm.js) -> kairon-ui -> kairon-node -> FluxVM's own WebSocket-upgraded console endpoint` -- rides the exact same relay infrastructure and authorization model as the VNC console above (same ticket flow, same `consoleAuthorized` per-Machine allowlist, same `KAIRON_NODE_CONSOLE_TOKEN`/`console.tls` trust chain), just a different upstream on kairon-node's side and a different in-guest channel:

- **A completely different guest-side dependency from every other guest-agent feature in this project.** VNC needs nothing in the guest at all; guest-exec/`MachineSnapshot` quiesce/guest-IP resolution need the standard, widely-packaged `qemu-guest-agent`. Text console needs FluxVM's own **proprietary** `fluxvm-guest-agent` binary and systemd service baked into the guest image -- a real, new operational requirement this project has never asked of an operator before enabling this. See `docs/guides/machine-text-console.md` for what that actually involves.
- **Not gated by the exec's admin-only requirement above** -- text console reuses `consoleAuthorized`'s existing VNC-console model (any authenticated operator by default, restrictable via `kairon.zyvor.dev/console-allowed-users`), the same reasoning applies here as for VNC: an interactive shell is roughly the same trust level as a graphical display into the same guest.
- **Works on every backend**, unlike VNC (QEMU-only) -- FluxVM's own vsock agent isn't tied to a display device.
- **The vsock channel's own authentication token is entirely FluxVM's problem, not Kairon's**: FluxVM auto-generates a random per-VM token and burns it into the guest's own disk before boot whenever `spec.guestAgent.console` is set -- Kairon never generates, stores, or transmits this secret itself, and it never appears in a Kairon-controlled log or API response.
- **A ticket now carries a `kind`** (`"vnc"` or `"text"`, `?kind=text` when requesting one) -- the same single-use, 30-second, per-Machine-bound ticket mechanism as VNC, just naming which of the two relay routes it authorizes.

Off by default (`spec.guestAgent.console: false`); the console relay itself (`console.enabled`) must also be configured, the same deployment-level gate VNC and guest-exec both already share.

## Guest file access (`spec.guestAgent.console`)

Reading or writing a file inside a running guest -- `browser -> kairon-ui -> kairon-node -> FluxVM's own bespoke vsock guest agent` -- rides the same `fluxvm-guest-agent` channel the text console above uses (`spec.guestAgent.console`, not `spec.guestAgent.enabled`), relayed through the same `internal/consoleproxy` infrastructure as a plain JSON request/response, the same shape guest exec already has:

- **Admin-only, same posture as guest exec, stricter than text console.** Arbitrary guest file read/write is at least as sensitive as arbitrary command execution -- `handleAgentPutFile`/`handleAgentGetFile` (`internal/uiapi/agentfile.go`) require an admin identity outright (a `ui.auth.users[].admin` account, or an OIDC session in `ui.oidc.adminGroups` -- see the OIDC section above), unlike the text console's any-authenticated-operator-by-default model.
- **Requires `spec.guestAgent.console`**, not `spec.guestAgent.enabled` -- this is FluxVM's proprietary vsock agent, the same dependency the text console needs (`fluxvm-guest-agent` baked into the guest image), not standard `qemu-guest-agent`.
- **No path restriction of any kind.** An admin who can reach this can read or write any path the guest agent process itself can access inside the guest -- there is no allowlist/denylist of sensitive paths (`/etc/shadow`, guest-side credentials, etc.). Treat this exactly like `kubectl exec`/`cp` into a privileged pod: convenient for a trusted admin team, not something to expose more broadly without a finer permission model this project doesn't have yet.
- **Response size is capped at kairon-ui's own 4MiB HTTP-response limit** (`internal/fluxvm.Client`'s general response cap) -- a file whose base64 encoding exceeds that (roughly a 3MB file) fails to decode with a clear error rather than returning silently-truncated content.
- **No content-type or binary-safety guarantee on the dashboard's own UI** -- the browser encodes/decodes content as UTF-8 text; a real binary file only round-trips correctly if the operator handles the base64 payload directly against the API rather than through the dashboard's text-only panel. See `docs/guides/machine-guest-agent-files.md`.
- **A sibling command-exec route rides this exact same channel and posture**: `POST /api/v1/machines/{ns}/{name}/agent-exec` (`internal/uiapi/agentexec.go`) runs a shell command over the vsock agent -- same admin-only gate, same `spec.guestAgent.console` requirement, same `internal/consoleproxy` relay shape. Genuinely distinct from "Guest exec" above despite the similar name: this one is backend-agnostic (no QEMU/qemu-guest-agent dependency at all), so it's the only guest-exec path available for a Cloud Hypervisor/Firecracker/FluxVm-sandbox-backed Machine. Same audit-line posture as guest exec (who/when logged, never the command itself, since arguments can carry secrets).

## VM-state snapshot/restore

Checkpointing and restoring a Machine's full hypervisor state -- `browser/API
client -> kairon-ui -> kairon-node -> FluxVM's own POST /v1/vms/{id}/snapshot`,
`/start-from-snapshot`, and `/stop` -- relayed through the same
`internal/consoleproxy` infrastructure as guest exec and guest file access,
the same plain JSON request/response shape. Distinct from `MachineSnapshot`
(CSI's disk-content-only snapshot, `internal/csinode`) -- this captures RAM,
CPU, and device state via FluxVM's real QEMU `savevm`/Cloud Hypervisor
snapshot, restoring back into the *same* Machine rather than a new PVC.

- **Admin-only, same posture as guest exec and guest file access.**
  `handleVMSnapshot`/`handleVMRestoreSnapshot`
  (`internal/uiapi/vmsnapshot.go`) require a `ui.auth.users[].admin`
  account outright; the same "no group-to-admin claim mapping" OIDC
  limitation applies.
- **No guest-agent dependency at all** -- unlike every other guest-agent
  feature in this list, this operates entirely at the QEMU/Cloud
  Hypervisor level against FluxVM itself, so it needs neither
  `spec.guestAgent.enabled` nor `spec.guestAgent.console`.
- **Restoring is genuinely destructive to the Machine's current running
  state**: `RestoreSnapshot` always stops the Machine first (FluxVM's own
  `start-from-snapshot` silently ignores the requested tag on an
  already-running VM), then starts it back up from the checkpoint --
  whatever the Machine was doing at the moment of the call is interrupted,
  not preserved. If the restore's own start step fails after the stop
  already succeeded, `internal/agent`'s own reconcile loop notices the
  runtime is FluxVM-stopped while `spec.powerState` still wants it running
  and starts it back up from its last-known-good disk state as a safety
  net -- not a second automatic restore attempt, and not the snapshot
  itself.
- **No snapshot enumeration or deletion, and no path/size restriction on
  tags** -- FluxVM itself owns tag storage; Kairon has no endpoint to list
  or prune what's been saved on a Machine. See
  `docs/guides/machine-vm-state-snapshot.md`.

## Sandboxes (`spec.sandbox`)

FluxVM's own agent-sandbox track -- a lightweight, fast-boot in-tree
hypervisor for short-lived ephemeral workloads -- adds three new trust
boundaries beyond the VM lifecycle every other Machine already goes
through:

- **Template building is admin-only and a real resource cost.**
  `POST /api/v1/nodes/{node}/templates` (`internal/uiapi/sandboxes.go`)
  pulls and exports an arbitrary caller-named OCI image on the target
  node (via FluxVM's own `skopeo`/`umoci`-backed `build_oci_template`) --
  same admin-only gate FluxVM's own `build_template` handler already
  enforces, kept here too rather than relying on FluxVM alone.
- **The HTTP proxy relay (`.../sandbox-http/{port}/{path...}`) is the
  broadest capability in this list.** It passes an arbitrary HTTP
  method/path/body/headers straight through to whatever the guest's own
  web server does with them -- materially more than guest exec (one
  command, synchronous, logged who/when) or file access (one path at a
  time). Admin-only, and deliberately *not* gated on
  `spec.guestAgent.console`, since it never touches the vsock agent at
  all -- it dials the guest's own network IP directly, so it needs
  `spec.network` to actually give the guest a real IP (`tap`/bridge, not
  `user`/SLIRP). Whatever the guest's own web application does with a
  proxied request (auth, input validation, rate limiting) is entirely the
  guest's own responsibility -- Kairon relays bytes, it doesn't inspect
  them.
- **`spec.deviceClaims` is refused outright for a sandbox Machine** at
  creation time (`internal/fluxvm.Client.CreateSandboxForMachine`) --
  FluxVM's lightweight sandbox hypervisor has no VFIO passthrough support,
  so silently ignoring a device claim here would be worse than a clear
  upfront error.

See `docs/guides/machine-sandboxes.md`.

## Image catalog (`spec.image.catalogName`)

FluxVM's own node-local image catalog -- a named, checksummed (and
optionally signed) alias `spec.image.catalogName` or any `CreateVmRequest.image`
field can reference instead of a raw path:

- **Every mutating operation is admin-only**: registering, removing,
  renaming, cloning, exporting, or toggling read-only on a catalog entry
  (`internal/uiapi/catalog.go`) -- the same posture sandbox template
  building already has, and for the same reason: registering an entry
  triggers a real image download and checksum pass on the target node, a
  real resource cost an operator shouldn't trigger freely. Listing
  (`GET`) is any-authenticated-operator, read-only visibility.
- **No path restriction on `source` (register) or `path` (export)** --
  an admin who can reach this can point FluxVM at any local path or
  `http(s)://` URL the FluxVM process itself can reach/write, the same
  trust level as `kubectl exec`/`cp` into a privileged pod.
- **Kairon never generates or verifies signatures itself** -- FluxVM's
  own catalog signing (`fluxvm catalog sign`, run out of band on the
  node) is the only way an entry gets one; Kairon only ever relays
  whatever FluxVM already reports (`signatureValid`), the same "not
  Kairon's problem" posture the text console's vsock-agent token already
  has.
- **`spec.image.catalogName` bypasses `--image-root` path fencing
  entirely** -- deliberately, since it was never a filesystem path
  `validateImagePath` could meaningfully fence in the first place (the
  same exemption `spec.image.source`-resolved paths already have, for
  the analogous reason).

See `docs/guides/machine-image-catalog.md`.

## Egress check (`POST /api/v1/nodes/{node}/egress-check`)

A stateless diagnostic against a node's static sandbox egress config
(`internal/uiapi/egress.go`) -- FluxVM's own `POST /v1/egress/check`,
checking whether an outbound request to a given host would be allowed,
with no auth requirement at all on FluxVM's own side. Kairon adds two
layers on top:

- **Admin-only.** Even though the check itself is read-only, confirming
  which hosts are allowlisted (or that a credential is configured for one
  at all) is real information about a node's sandbox egress policy.
- **The credential itself is never returned.** FluxVM's own response
  carries `inject_authorization` -- the literal secret value it would
  inject into a matching request, not a boolean -- when a credential-vault
  entry matches the checked host. `handleEgressCheck` deliberately
  discards that value entirely and reports only a boolean
  (`wouldInjectCredential`) instead; returning the raw value to any admin
  who can guess or enumerate a configured host would defeat the point of
  having a vault at all. Covered by a dedicated regression test
  (`TestHandleEgressCheckRedactsCredential`) that fails loudly if the
  secret ever leaks into the response body again.

There is no way to change the underlying allowlist/credential vault
through this or any Kairon API -- it's static, node-local FluxVM config,
not a runtime-mutable resource.

## Warm pools (`POST/GET/DELETE /api/v1/nodes/{node}/pools/...`)

FluxVM's own node-local warm-VM pools -- `size` VMs pre-booted and kept
`Paused`, ready for an instant claim -- add one trust boundary beyond
what a sandbox itself already has:

- **Creating or deleting a pool is admin-only** (`internal/uiapi/pool.go`,
  reusing `requireCatalogAdmin`'s exact gate) -- creating one commits real,
  ongoing node resources (`size` resident VMs, whether ever claimed or
  not); deleting one destroys every member VM it currently holds, claimed
  or not, not just the pool's own bookkeeping. Listing is any
  authenticated operator (read-only visibility).
- **Claiming is admin-only too**, even though it just resumes an
  already-authorized, already-built VM -- the claimed VM's `name`/
  `ttlSeconds` overrides are attacker-influenceable inputs the same way
  any other Machine-creation-adjacent field is.
- **A claimed VM is not automatically a Kairon-managed `Machine`** --
  Kairon hands back FluxVM's own raw VM record; there is no automatic
  admission/ownership step, so a claimed VM sits outside every other
  admission-time guard (`MachineQuota`, the webhook, RBAC subresource
  checks) until an operator explicitly creates a corresponding `Machine`
  themselves. Not a bypass of anything -- FluxVM's own claim isn't gated
  by Kairon's admission path in the first place -- but worth knowing
  before assuming pool claims are already subject to the same policy
  Machine creation is.
- **If FluxVM's own `fluxvm-microvm` operator (`MicroVMPool` CRD) is also
  deployed against the same nodes**, it reconciles against the identical
  `/v1/pools` state this API wraps, with no coordination between the two.
  Don't manage the same pool name from both.

See `docs/guides/machine-sandboxes.md`'s "Warm pools" section.

## Runtime diagnostics (capabilities, pressure, cpuset, freeze/thaw)

Four small FluxVM diagnostics (`internal/uiapi/diagnostics.go`):

- **Runtime capabilities and pressure/cpuset are any-authenticated-operator,
  read-only visibility** -- a node's static capability manifest and a
  Machine's own PSI/cpuset readings carry no secrets and reveal nothing
  an operator with normal dashboard access couldn't already infer.
- **Freeze/thaw are admin-only**, unlike the read-only three above --
  halting a Machine's entire cgroup at the kernel level (not just its
  guest CPUs, the way `spec.powerState: Paused` does) is a materially
  more disruptive operation, reusing the same admin-gate posture guest
  exec and VM-state snapshot/restore already have for comparably
  impactful actions.
- **No interaction guard against freezing a Machine mid-migration or
  mid-hotplug** -- the same "between you and FluxVM's own semantics"
  posture already documented for `Paused`.

See `docs/guides/machine-diagnostics.md`.

## VM-edge packet capture (`.../network-capture`)

`POST /api/v1/machines/{ns}/{name}/network-capture` starts a FluxVM
`tcpdump` capture on the Machine's dataplane interface; `GET` lists
sessions and `GET .../network-capture/{token}` downloads the pcap
(`internal/uiapi/network_observability.go`).

- **Same authorization as the console and diagnostics**
  (`requireDiagnosticsAccess`): any operator allowed to open this
  Machine's console. This is weaker than guest exec, which needs an
  admin. A pcap holds packet payloads (up to 1,600 bytes per frame), so
  unencrypted guest traffic is readable by whoever can capture it.
  Restrict console access on Machines whose traffic is sensitive: the
  `kairon.zyvor.dev/console-allowed-users` annotation, or
  `KAIRON_UI_RBAC_CONSOLE_CHECK=true` (a SubjectAccessReview on
  `machines/console`).
- **Bounded by FluxVM**: 1-30 seconds, 20,000 packets, one capture per
  Machine at a time, the newest 16 kept, all deleted with the VM. The
  token is restricted to `[A-Za-z0-9_-]` because it names the file, and
  the filter is passed to tcpdump as a single argument after `--`, so it
  cannot inject options.
- **Off unless diagnostics are enabled** (`KAIRON_NODE_CONSOLE_TOKEN` on
  kairon-node and kairon-ui); otherwise 501. Calling FluxVM directly
  requires its admin role.

## Incident bundles (`.../diagnostics`, `kaironctl diagnose machine --deep`)

`POST /api/v1/machines/{ns}/{name}/diagnostics` starts a FluxVM bundle job;
`GET .../diagnostics/{token}` polls it and `.../{token}/bundle` and
`.../{token}/memory-dump` download the files (`internal/uiapi/incident.go`,
relayed through `internal/consoleproxy`).

- **Admin only, stricter than packet capture.** A memory dump holds
  everything in guest RAM (keys, tokens, plaintext), and `--no-redact` keeps
  cloud-init and secret env. The routes need namespace access and either a
  `ui.auth.users[].admin` account or the shared `KAIRON_UI_TOKEN`, which is
  already root-equivalent on every route; other accounts get 403.
- **Redaction is pattern-based and on by default.** Values under
  credential-like keys and Machine `spec.cloudInit` are fingerprinted
  (`redacted:sha256:<16 hex>`), and text after `password=`, `token:`,
  `Bearer ` and similar is replaced. A secret the guest prints without such a
  key word, packet captures and memory dumps are not redacted. Review a bundle
  before sharing it.
- **Memory dumps stay on the node** under FluxVM's `diagnostics_dir`
  (owner-only), capped by `diagnostics_max_memory_dump_mib`, refused for
  confidential VMs, and removed with the VM. kairon-ui logs every start and
  every dump download; FluxVM audits starts, finishes and deletes. The guest
  is paused while the dump is written.
- **Integrity**: kaironctl checks the node bundle and the dump against the
  sha256 FluxVM recorded and drops a node part that does not match.
- **Off unless diagnostics are enabled** (`KAIRON_NODE_CONSOLE_TOKEN` on
  kairon-node and kairon-ui); otherwise 501.

## MCP server for AI agents (`kaironctl mcp serve`)

Full tool list and setup: [`docs/guides/hermes-mcp.md`](docs/guides/hermes-mcp.md).

- **No new privilege.** It runs as a local subprocess of the agent and
  acts with whatever `KAIRON_KUBE_*` and `KAIRON_UI_TOKEN` credentials it
  is given; there is no listener and no Kairon-side identity. Scope those
  credentials (a Kubernetes Role with only the verbs the agent should
  have, a non-admin kairon-ui account) rather than relying on the tool
  list.
- **Writes are opt-in.** Without `--allow-write`, no write tool is listed
  or callable. With it, the write set is: `set_power_state`,
  `create_snapshot`, `snapshot_volume`, `network_capture`, `claim_machine`,
  `release_claim`, `fork_machine`, `machine_disk`, `machine_nic`,
  `machine_backup` (create, restore, delete), `delete_machine`,
  `create_sealed_claim`, `apply_network_policy`, `apply_claim_step` and
  `audit_record`. Machine delete is therefore reachable through MCP (it
  refuses MachineSet replicas). Migrate, exec, and edge changes other than
  `apply_network_policy` are not exposed.
- **Human approval for destructive tools.** With `--allow-write`,
  `--require-approval` (default `true`) makes `delete_machine`,
  `fork_machine` and `machine_backup` with `action: restore` or `delete`
  refuse until a human approves that exact call. `--require-approval=false`
  turns it off.
  - Default (annotation) mode: the refused call returns an approval id and
    the command `kaironctl approve machine/NS/NAME ID` (or `machinebackup/...`).
    That writes the `kairon.zyvor.dev/mcp-approval` annotation
    (`<id>;<expiry>;<approver>`) on the target Machine or MachineBackup,
    valid for `--ttl` (default 10 minutes). The approver needs `patch` on
    that resource. The agent's next identical call consumes it with an
    atomic test-and-remove patch, so an approval is **single-use** and is
    spent before the tool runs (a failed call needs a new one). The id is a
    12-hex-character SHA-256 prefix over tool, canonicalised arguments and
    `KAIRON_MCP_PRINCIPAL` (default: the OS user), so changing any of them
    needs a new approval. The approver is recorded from `KAIRON_APPROVER`,
    else the local user name.
  - Resource mode (`KAIRON_MCP_APPROVAL_MODE=resource`): the approval is a
    separate Kubernetes object bound to the authenticated principal, the
    target UID and generation, and an expiry, created by a different
    identity with `kaironctl fleet approve-action REQUEST.json`; see
    "Fleet trusted approvals" below.
  - Limit: in annotation mode anything with `patch` on Machines or
    MachineBackups can write the annotation, including the agent's own
    credential if it has that verb. Approval only constrains an agent whose
    way in is MCP; keep the agent's token to the verbs the server needs and
    keep it away from shell access.
- **Audit log.** Every write call is recorded before and after it runs in
  a hash-chained JSONL log (`--audit-log`, default `~/.kairon/audit.jsonl`;
  `--audit-configmap ns/name` mirrors records into an existing ConfigMap).
  Each record carries the previous record's hash, so an edited, reordered or
  removed line breaks the chain; `kaironctl agent audit-verify` checks it. If
  the log cannot be written the write is refused. Outcomes include
  `approval-required`, `approval-expired` and `approved:<approver>`. The
  chain detects tampering after the fact; it does not stop someone with
  write access to the file from truncating it or from rewriting the whole
  chain.
- **Tenant scoping.** `KAIRON_MCP_TENANT` scopes the claim tools and
  `diagnose` to one tenant, and is recorded in audit records. It is an
  environment setting of the local process, not an authorization decision
  made by the cluster.
- **Prompt injection.** Tool results include data a guest or tenant can
  influence (Machine messages, flow and drop records, console-derived
  status). An agent that reads them and also has write tools can be
  steered; keep agents that process untrusted Machines read-only.
- **Local file writes.** `network_capture` with `output` writes a pcap to
  that path with the server process's permissions.
- Tokens come from the environment and are never included in tool
  results; tool output is capped at 64 KB.

## Tenant fence (`kairon.zyvor.dev/tenant-fence`)

See [`docs/guides/tenant-fence.md`](docs/guides/tenant-fence.md). It is a
best-effort deny list, not tenant isolation:

- It is not a VRF. The controller denies the guest IPs it has observed on
  other tenants' Machines in the same namespace; an address Kubernetes has
  not observed yet, or one outside the namespace, is not denied. An address
  shared with a same-tenant Machine is not denied.
- A user `MachineNetworkPolicy` is merged with, not replaced by, the fence.
  Node-wide default-deny still wins on `defaultAllow` when enabled.
- Without the admission webhook (`webhook.enabled`) nothing stops a tenant
  from being renamed or a selector crossing tenants; the fence itself still
  works.
- `spec.tenant` drives this fence only; it is not an authorization boundary
  for the Kubernetes API, kairon-ui or the console.
- Verified on one live k3s host (2026-10-06) for deny-list content and group
  cleanup; actual packet isolation between tenants is not claimed here.

## Scoped `kairon-ui` authorization (opt-in)

`ui.auth.namespaceScoping.enabled` (kairon-ui `-namespace-scoping-enabled`,
default `false`; the production Helm profile enables it). When on, a
non-admin operator may use a namespace only if it is in their
`ui.auth.users[].namespaces` or reachable through `ui.oidc.namespaceGroups`;
other namespaces return 403. The check runs per request against current
config (no re-login needed). Admins stay unrestricted. In scoped mode the
legacy shared token and unauthenticated callers have no per-caller identity
and are denied (`internal/uiapi/auth.go`, `actionauth.go`); the overview
counts only visible namespaces and reports the node count only for admins.
Routes under `/api/v1/nodes` expose cross-namespace data and require an
admin when scoping is on (`internal/uiapi/actionauth.go`).
Off by default, so an upgrade changes nothing: with it off every
authenticated operator sees every namespace. The dashboard's own
ClusterRole is still cluster-wide, so a kairon-ui compromise is not bounded
by this setting.

## Fleet trusted approvals and Redfish fencing (experimental)

See [`docs/guides/enterprise-fleet.md`](docs/guides/enterprise-fleet.md).

- **Trusted approvals.** With `KAIRON_MCP_APPROVAL_MODE=resource`, the agent
  gets the `kairon-action-consumer` role and a separate human credential
  gets `kairon-action-approver` (namespace RoleBindings; neither is bound
  automatically). Admission binds the approver to Kubernetes `userInfo` and
  rejects self-approval, excessive expiry and replay or reset; status moves
  once to Consumed, only by the bound principal. Consumption happens before
  the action, so a failed action needs a new approval, and there is still a
  consume-to-mutation race. Administrative Kubernetes credentials bypass
  RBAC and must stay outside the agent's trust boundary. Legacy annotation
  mode remains the default.
- **Redfish fencing.** BMC origins are an administrator allowlist
  (`fleet.redfishOrigins`, HTTPS only), credentials come from Secrets
  (`username`, `password`, optional `ca.crt`), TLS verification stays on and
  redirects are rejected. ForceOff is bound to the Node UID and the host
  must be confirmed Off before ownership is released. Tests run against
  simulated servers; physical power fencing and vendor coverage are not
  verified. Automatic recovery leaves hosts cordoned.

## Node fencing is an operator attestation

`kaironctl fence MACHINE --reason ...`, `kaironctl node fence NODE --reason
...` and the controller's `--stale-evacuation` do not verify that a node is
dead. The `--reason` (or the `kairon.zyvor.dev/node-fenced` annotation) is
the operator's claim, and the controller then lets the scheduler start the
Machine elsewhere. If the old node was only partitioned and FluxVM is still
running the VM, two copies run against the same storage (split-brain).
Mitigations are partial: the optional kairon-node liveness Lease makes
`kaironctl fence` refuse while the agent still looks alive, and stale
evacuation additionally needs a per-Machine `kairon.zyvor.dev/evacuate=true`
opt-in and respects MachineDisruptionBudgets. Anyone who can `patch` Nodes
can set the attestation and anyone who can annotate Machines can opt them in,
so restrict both verbs to the people and power-fencing tooling that need
them. Procedure: [`docs/runbook-node-fence.md`](docs/runbook-node-fence.md).

## macOS node trust model

See [`docs/macos.md`](docs/macos.md) and
[`docs/runbook-macos-node.md`](docs/runbook-macos-node.md).

- **Self-registration.** A Mac has no kubelet, so `kairon-node` creates and
  heartbeats its own Node. `deploy/macos/rbac.yaml` grants the `kairon-node`
  ServiceAccount `get`, `list`, `create` and `patch` on `nodes` and `patch`
  on `nodes/status`, cluster-wide and without `resourceNames`. Whoever holds
  that token (it sits in the Mac's environment, `KAIRON_KUBE_TOKEN`) can
  create or modify any Node object, including labels, capacity, the Ready
  condition and the `node-fenced` annotation. Because the binding is to the
  shared ServiceAccount, applying that file extends these verbs to every
  kairon-node using it, not only Macs. Use a dedicated ServiceAccount or
  token for Mac nodes if that matters to you.
- **vm-only taint.** The Mac Node carries `kairon.zyvor.dev/vm-only:NoSchedule`
  so ordinary pods are not scheduled to a node without a kubelet. Machines
  opt in with a toleration. It is a scheduling convention, not isolation.
- **Isolation boundary.** Guest isolation is FluxVM's `vz` backend on Apple's
  Virtualization.framework; Kairon adds none of its own. `internal/fluxvm/apple.go`
  rejects features the backend cannot honour, but that is a compatibility
  check, not a security control. The FluxVM URL defaults to loopback
  (`http://127.0.0.1:7788`, optional `FLUXVM_TOKEN`); kairon-node's health
  listener defaults to `:32302` on all interfaces and serves `/healthz`,
  `/readyz` and `/metrics` without authentication.
- **Not verified on macOS:** macOS guests, live migration, CSI, the VNC
  console relay, kairon-ui, multi-Mac clusters and Intel Macs. Do not rely
  on this document's other sections for those cases. The end-to-end run used
  a single-node k3s cluster on the same Mac.

## Network observability (`.../network-effective`, `.../network-stats`, `.../network-flows`, `.../network-drop-reasons`)

Four read-only network troubleshooting endpoints (`internal/uiapi/network_observability.go`):

- **Any authenticated operator**, same posture as the pressure/cpuset
  diagnostics above -- flow/drop-reason/stats data for a Machine an
  operator can already view is not more sensitive than the Machine's own
  status.
- **Raw JSON passthrough, not a Kairon-defined schema** -- FluxVM's own
  handlers return a dynamic `serde_json::Value` here, so there is no
  fixed Rust struct this project could mirror even if it wanted a typed
  response; Kairon relays bytes, it doesn't validate or reshape them.
- **Deliberately narrow scope.** FluxVM's own network dataplane exposes a
  much larger Cilium-style surface (CNP/identity/ipcache management,
  Hubble flow observability, per-service health/stats/telemetry/conntrack
  state transfer, L7 Envoy contracts) discovered during the same route
  audit that added these four -- not wrapped here, since most of it is
  either internal node-to-node coordination machinery, not an
  operator-facing capability at all, or substantial enough (Hubble) to
  need its own dedicated design pass. See
  `docs/guides/network-policy.md`'s "Troubleshooting" section for the
  full reasoning.

## CSI node plugin (`csiNode.enabled`)

Kairon's own first-cut CSI driver (`csi.kairon.zyvor.dev`, iSCSI only -- see [`docs/guides/machine-storage-csi.md`](docs/guides/machine-storage-csi.md)) is a real, larger trust boundary than every other Kairon component, inherent to what it does, not a design oversight:

- **`kairon-csi-node` runs `privileged: true`**, not just an added capability list -- logging in to an iSCSI target, creating a filesystem, and mounting it all genuinely need this. It's also the one Kairon-built container image *not* based on the minimal distroless base every other component uses: it needs a real `iscsiadm`/`blkid`/`mkfs.*` environment around it, which means a real package manager, shell, and OS userland inside the image, a meaningfully larger attack surface than `kairon-controller`/`kairon-node`/`kairon-ui`'s single static binary each.
- **`hostNetwork: true`**, the same reason `kairon-node` itself runs that way -- direct L3 reachability to whatever host:port an iSCSI target's `portal` names, taken straight from a `PersistentVolume`'s `spec.csi.volumeAttributes`. Anyone who can create a `PersistentVolume` in the cluster can point this driver at an arbitrary network destination; treat "who can create PVs naming `csi.kairon.zyvor.dev`" as equivalent to "who can make this node's iSCSI initiator connect anywhere," the same posture as `kaironctl create --image` already requires trusting whoever can create a `Machine`.
- **No Kubernetes API access at all** -- `kairon-csi-node` is a pure local gRPC server (`internal/csinode`) and never calls `kube.Client`; its ServiceAccount has `automountServiceAccountToken: false`, one less credential sitting in an already-privileged pod.
- **CHAP on `kairon-node`'s consumption path is opt-in and namespace-allowlisted** (`node.csi.chap.enabled`) — Secrets may only be read from the release namespace, never cluster-wide (see the CSI guide's "Real limits").
- **iSCSI login/session state is per-node, in the container's own writable root filesystem** (`/etc/iscsi`, `/var/lib/iscsi`) -- not persisted across a pod restart beyond what `iscsiadm`'s own idempotent `--op=new`/`--login` calls reconstruct on the next reconcile.

Off by default (`csiNode.enabled: false`); enabling it is a deliberate trade -- real network-block storage for Machines -- against a meaningfully larger blast radius than any other optional Kairon component. Appropriate once you've decided to trust whoever can create `PersistentVolume`/`Machine` objects with node-level `iscsiadm` access, the same way `hostNetwork`/privileged DaemonSets always require that trust.

## Third-party CSI client (`node.thirdPartyCSIDrivers`)

`kairon-node` can act as its own CSI client against a driver it doesn't ship -- see [`docs/guides/machine-storage-thirdparty-csi.md`](docs/guides/machine-storage-thirdparty-csi.md) for the feature itself. The trust model is deliberately the same shape as VFIO passthrough's allowlist above, applied to a new domain:

- **Explicit operator allowlist, not automatic discovery.** `node.thirdPartyCSIDrivers` maps a driver name to a socket path; a driver name not present is refused outright, fail-closed the same way an unlisted VFIO BDF is refused. `kairon-node` never dials a socket it wasn't explicitly told about, and never attempts kubelet's own `plugins_registry/` discovery mechanism.
- **Secrets from one namespace only, opt-in.** `nodeStageSecretRef`/`nodePublishSecretRef` resolve only with `node.thirdPartyCSISecrets.enabled`, which grants `get` on Secrets in the release namespace and nowhere else; a ref outside it fails before any CSI call. Anyone who can create a PV can then name any Secret in that namespace, so keep only CSI credentials there.
- **VolumeAttachment RBAC.** Allowlisting a driver grants `kairon-node` `get` on `csidrivers` and `get`/`create`/`delete` on cluster-scoped `volumeattachments`. A compromised node could therefore attach or detach volumes on any node through that driver's external-attacher, comparable to kubelet's own attach rights before NodeRestriction. Kairon only touches attachments it names `kairon-…` for its own node.
- **Read-only host mount, broader than the sockets actually used.** Enabling this mounts the node's entire `/var/lib/kubelet/plugins` directory read-only into the `kairon-node` container, rather than templating a mount per allowlisted driver. `kairon-node` itself only ever connects to the specific socket path configured per driver name -- nothing in this codebase lists or reads anything else under that mount -- but the mount's own blast radius (what else lives under that host directory) is broader than strictly necessary; a future cut could narrow this to per-driver mounts.
- **Staging/publish paths are Kairon-synthesized**, not kubelet's Pod-UID-keyed convention -- a deviation a third-party driver's own internal assumptions could theoretically depend on; see the guide's "Real limits" for why this is named as a compatibility risk rather than a security one (no privilege implication, just a correctness one).
- **Not validated against a live third-party driver.** Request-building and the allowlist-fail-closed behavior have real unit test coverage against a fake gRPC CSI server, but no live Ceph-CSI/RBD (or other) deployment exists in this project's own test environment. Treat this as unvalidated against your specific driver until you've tested it yourself.

Off by default (`node.thirdPartyCSIDrivers: {}`); enabling it for a given driver is a deliberate, per-driver trust decision -- the same category of tradeoff as `csiNode.enabled` above, scoped to whichever driver sockets you explicitly name.
