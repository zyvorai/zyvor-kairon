// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package uiapi is the HTTP backend for kairon-ui: a thin REST wrapper
// around internal/kube.Client, one method-per-intent handler per route (no
// generic PATCH passthrough), serving the built web/ SPA from a directory
// alongside its own /api/v1/... routes. Every handler here is read-only
// with respect to business logic -- it never encodes a decision
// internal/controller or internal/agent don't already make; it only
// translates HTTP requests into the same kube.Client calls kaironctl's own
// subcommands use.
package uiapi

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/ratelimit"
)

// Server wires a *kube.Client into the /api/v1/... route table and,
// optionally, static file serving for the built SPA.
type Server struct {
	Kube  *kube.Client
	Log   *slog.Logger
	Token string
	// WebDir, when set, serves the built web/dist SPA for any request
	// that doesn't match an /api/v1/... route -- KAIRON_UI_WEB_DIR, not
	// go:embed, so `go build`/the dep-free Go CI job never needs Node.
	WebDir string
	// Users, when non-empty, enables real per-operator username/password
	// login (see auth.go) alongside (not instead of) the legacy Token
	// above -- either credential is accepted. Mutated at runtime by
	// setOwnPassword/resetPassword, so every access goes through usersMu
	// (see auth.go's userCount/findUser helpers) rather than reading the
	// field directly.
	Users   []User
	usersMu sync.RWMutex
	// SessionSecret signs/verifies session tokens issued by
	// POST /api/v1/auth/login. Required whenever Users is non-empty.
	SessionSecret []byte

	// AtlasURL/AtlasToken enable the read-only Zyvor Atlas storage views
	// (see atlas.go). AtlasConsoleURL is an optional "Open in Atlas" link.
	AtlasURL        string
	AtlasToken      string
	AtlasConsoleURL string

	// Version is the build version shown (unauthenticated) on the sign-in
	// page's stat band.
	Version string

	// history backs GET .../metrics; filled by RunMetricsSampler.
	history     *metricsHistory
	historyOnce sync.Once

	// apiRoutes is the number of /api/v1 routes registered by Handler.
	apiRoutes int

	// LoginHint is optional free text shown under the sign-in form (lab
	// installs use it to advertise the well-known default login).
	LoginHint string
	// DefaultPassword, when non-empty, is a well-known lab password
	// (the chart's "Admin@321"). A login that succeeds with exactly this
	// password gets "defaultPassword": true in its response so the
	// dashboard can nag until it is changed. It never gates anything.
	DefaultPassword string
	// UsersSecretNamespace/UsersSecretName/UsersSecretKey tell
	// persistUsers (auth.go) which Kubernetes Secret to write an updated
	// Users list back into after a password change, so it survives a pod
	// restart. UsersSecretName empty means runtime password changes are
	// refused (see errPersistenceNotConfigured) -- set only when the Helm
	// chart itself owns the kairon-ui-users Secret (not when an operator
	// supplies ui.auth.existingSecret, which some external tool may manage
	// and which kairon-ui must not silently overwrite).
	UsersSecretNamespace string
	UsersSecretName      string
	UsersSecretKey       string
	// SharedStateNamespace/SharedStateConfigMapName point at the ConfigMap
	// kairon-ui uses to make session revocation, login lockout, and
	// console-ticket state visible across replicas -- see
	// internal/uiapi/sharedstate.go and docs/guides/kairon-ui-ha.md. Empty
	// SharedStateConfigMapName (the default) disables cross-replica sync
	// entirely: every mutation below still applies to this process's own
	// in-memory state exactly as before, correct for the single-replica
	// deployment this chart still defaults to. Password changes propagate
	// separately, via UsersSecretName above, independent of this field.
	SharedStateNamespace     string
	SharedStateConfigMapName string
	// OIDC, when set, enables "Sign in with SSO" (see oidc.go) alongside
	// -- not instead of -- Token and Users above. This is the one place
	// in Kairon that breaks the project's Go-stdlib-only design guarantee
	// (see README.md); nil (the default) means every request path
	// behaves exactly as it did before this field existed.
	OIDC *OIDCAuth
	// revoked backs POST /api/v1/auth/logout; zero value (an empty
	// sync.Map) is ready to use.
	revoked sync.Map
	// loginAttempts backs handleLogin's per-username rate limiting; zero
	// value is ready to use.
	loginAttempts sync.Map
	// passwordChangedAt backs resetPassword's forced logout of a reset
	// account's outstanding sessions (see passwordChangedAfter in
	// auth.go); zero value is ready to use.
	passwordChangedAt sync.Map
	// ConsoleToken/ConsolePort configure the VNC console relay (see
	// console.go): the shared bearer token kairon-ui presents to a
	// kairon-node's console listener, and the port that listener runs on.
	// Either empty disables the console feature entirely (handleConsole
	// returns 501).
	ConsoleToken string
	ConsolePort  string
	// ConsoleTLS, when set, dials kairon-node's console relay over
	// wss:// with this TLS config (verifying its server certificate)
	// instead of plaintext ws://. One-way TLS is enough here -- the
	// shared ConsoleToken already authenticates kairon-ui to kairon-node,
	// so a client certificate would be redundant.
	ConsoleTLS *tls.Config
	// RBACConsoleCheck, when true, additionally requires a real
	// Kubernetes SubjectAccessReview (verb "get" on the machines/console
	// subresource) to allow console access -- layered alongside, never
	// instead of, the kairon.zyvor.dev/console-allowed-users annotation
	// allowlist consoleAuthorized already enforces. False (the default)
	// is today's unchanged, annotation-only behavior. Only meaningful for
	// an OIDC-authenticated identity whose claims are also mapped into
	// kube-apiserver's own OIDC config -- a local ui.auth.users[] account
	// has no real Kubernetes User to check RBAC against, so it's denied
	// outright when this is true (fail closed, matching consoleAuthorized's
	// own existing "no real identity" precedent for the empty-username
	// case). See docs/guides/kairon-ui-console-rbac.md.
	RBACConsoleCheck bool
	// consoleTickets backs the console feature's single-use WebSocket
	// tickets; zero value is ready to use.
	consoleTickets sync.Map
	// Metrics, when set, serves /metrics (unauthenticated, same convention
	// as /healthz/readyz) and observes every request's method/status/
	// duration -- see withMetrics. Optional, nil-checked; without it
	// nothing here changes.
	Metrics *metrics.Recorder
	// RateLimit, when set, bounds request volume per remote address across
	// every route this Handler serves -- see withRateLimit. Distinct from,
	// and in addition to, the per-username login lockout above (auth.go):
	// that one only throttles repeated failed passwords against one
	// username; this throttles request volume from one client address
	// against anything, including a successfully-authenticated client
	// hammering an unrelated route. Optional, nil-checked; without it
	// nothing here changes.
	RateLimit *ratelimit.Limiter
	// TrustedProxyHeader, when set alongside TrustedProxyCIDRs, is the
	// header (e.g. "X-Forwarded-For") withRateLimit reads the real client
	// address from instead of r.RemoteAddr -- closes the gap README.md's
	// Production-gaps section documented honestly: behind a Service/
	// Ingress/load balancer with no forwarded-header trust configured,
	// every real client collapses into that one proxy's address, so one
	// noisy client can throttle everyone. Empty (the default) is
	// unchanged prior behavior: always key by r.RemoteAddr directly. See
	// clientIP.
	TrustedProxyHeader string
	// TrustedProxyCIDRs bounds which immediate peer addresses
	// TrustedProxyHeader is ever trusted from -- required alongside it,
	// fails closed (falls back to r.RemoteAddr) for a request whose
	// direct r.RemoteAddr doesn't fall inside one of these. Without this
	// check, TrustedProxyHeader alone would let *any* direct client spoof
	// an arbitrary address in that header and evade rate limiting (or
	// collapse its bucket onto an unrelated real client's), the opposite
	// of what this exists to fix -- it must only ever be trusted from the
	// operator's own known proxy/load-balancer hop.
	TrustedProxyCIDRs []*net.IPNet
	// NamespaceScopingEnabled turns on per-namespace authorization
	// (authorizedForNamespace, auth.go): once true, a non-admin operator
	// (session-token auth only -- see authorizedForNamespace) may only act
	// on a namespace listed in their own User.Namespaces or reachable via
	// an OIDCAuth.NamespaceGroups entry for a group their ID token
	// carries; every other namespace 403s. False (the default) is
	// today's unchanged behavior -- kairon-ui's own HTTP layer has no
	// namespace authorization axis at all, only isAdminIdentity's
	// admin-vs-not split, and every existing deployment must keep
	// behaving exactly that way unless this is deliberately opted into.
	// Flipping it on for a cluster with non-admin ui.auth.users/OIDC
	// sessions that have no Namespaces/NamespaceGroups configured moves
	// them from "sees every namespace" to "sees none" -- fail-closed, not
	// a silent no-op, so review ui.auth.users[].namespaces and
	// ui.oidc.namespaceGroups before enabling this in
	// charts/kairon/values.yaml. Overview is filtered; node-scoped proxy routes require an admin
	// in scoped deployments. Legacy shared tokens are refused when scoped.
	NamespaceScopingEnabled bool
	// ImageStoreDir enables the uploaded-image store (images.go): empty
	// disables every /api/v1/images route and /images/ blob serving.
	ImageStoreDir string
	// ImageStorePublicURL is the base URL nodes download uploaded images
	// from; empty means the upload request's own scheme and host.
	ImageStorePublicURL string
	// ImageStoreMaxBytes caps one upload; 0 means no limit.
	ImageStoreMaxBytes int64
}

// Handler returns the full mux: auth-gated /api/v1/... routes plus, if
// WebDir is set, static SPA serving for everything else. Auth is a single
// static bearer token (crypto/subtle.ConstantTimeCompare, constant-time
// against timing attacks) -- Token empty means unauthenticated dev mode,
// which cmd/kairon-ui refuses to start with unless explicitly opted into
// (see main.go's -allow-unauthenticated flag), the same secure-by-default
// posture as every other kairon component's fail-closed validation.
func (s *Server) Handler() http.Handler {
	top := http.NewServeMux()

	// Unauthenticated: Kubernetes readiness/liveness probes never carry
	// the bearer token, same convention as every other kairon component's
	// internal/health.Server.
	top.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	top.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	// Unauthenticated for the same reason: a Prometheus scrape config
	// never carries the bearer token either, same convention as
	// internal/health.Server's own /metrics route.
	if s.Metrics != nil {
		top.Handle("GET /metrics", s.Metrics.Handler())
	}

	api := &routeMux{ServeMux: http.NewServeMux(), n: &s.apiRoutes}
	// Overview filters every namespaced object using the caller's current grants.
	api.HandleFunc("GET /api/v1/overview", s.handleOverview)
	api.HandleFunc("GET /api/v1/namespaces", s.handleNamespaces)
	api.HandleFunc("GET /api/v1/stream", s.handleStream)
	api.HandleFunc("GET /api/v1/pods", s.handlePods)
	api.HandleFunc("GET /api/v1/health", s.handleHealth)
	api.HandleFunc("GET /api/v1/fleet/{resource}", s.requireNamespace(namespaceParam, s.handleListFleet))
	api.HandleFunc("POST /api/v1/fleet/{resource}", s.requireNamespace(namespaceParam, s.handleCreateFleet))
	api.HandleFunc("DELETE /api/v1/fleet/{resource}/{namespace}/{name}", s.requireNamespace(namespaceFromPath, s.handleDeleteFleet))
	api.HandleFunc("GET /api/v1/usage.csv", s.requireNamespace(namespaceParam, s.handleLedgerCSV))
	s.mountAgentPlane(api)

	api.HandleFunc("GET /api/v1/machines", s.orAllNamespaces(s.requireNamespace(namespaceParam, s.handleListMachines), s.handleListAllMachines))
	api.HandleFunc("POST /api/v1/machines", s.requireNamespace(namespaceParam, s.handleCreateMachine))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}", s.requireNamespace(namespaceFromPath, s.handleGetMachine))
	api.HandleFunc("DELETE /api/v1/machines/{namespace}/{name}", s.requireNamespace(namespaceFromPath, s.handleDeleteMachine))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/start", s.requireNamespace(namespaceFromPath, s.handlePowerMachine("Running")))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/stop", s.requireNamespace(namespaceFromPath, s.handlePowerMachine("Stopped")))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/pause", s.requireNamespace(namespaceFromPath, s.handlePowerMachine("Paused")))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/resume", s.requireNamespace(namespaceFromPath, s.handlePowerMachine("Running")))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/halt", s.requireNamespace(namespaceFromPath, s.handlePowerMachine("Halted")))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/priority", s.requireNamespace(namespaceFromPath, s.handleSetMachinePriority))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/fork", s.requireNamespace(namespaceFromPath, s.handleForkMachine))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/disks", s.requireNamespace(namespaceFromPath, s.handleHotplugDisk))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/nics", s.requireNamespace(namespaceFromPath, s.handleHotplugNIC))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/console/ticket", s.requireNamespace(namespaceFromPath, s.handleConsoleTicket))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/exec", s.requireNamespace(namespaceFromPath, s.handleExec))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/agent-file/put", s.requireNamespace(namespaceFromPath, s.handleAgentPutFile))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/agent-file/get", s.requireNamespace(namespaceFromPath, s.handleAgentGetFile))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/agent-exec", s.requireNamespace(namespaceFromPath, s.handleAgentExec))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/qga/fsfreeze-status", s.requireNamespace(namespaceFromPath, s.handleQGAFsfreezeStatus))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/qga/firewall/open", s.requireNamespace(namespaceFromPath, s.handleQGAFirewallOpen))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/qga/firewall/close", s.requireNamespace(namespaceFromPath, s.handleQGAFirewallClose))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/logs", s.requireNamespace(namespaceFromPath, s.handleLogs))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/timeline", s.requireNamespace(namespaceFromPath, s.handleMachineTimeline))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/metrics", s.requireNamespace(namespaceFromPath, s.handleMachineMetrics))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/vm-snapshot", s.requireNamespace(namespaceFromPath, s.handleVMSnapshot))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/vm-restore-snapshot", s.requireNamespace(namespaceFromPath, s.handleVMRestoreSnapshot))

	api.HandleFunc("GET /api/v1/migrations", s.orAllNamespaces(s.requireNamespace(namespaceParam, s.handleListMigrations), s.handleListAllMigrations))
	api.HandleFunc("POST /api/v1/migrations", s.requireNamespace(namespaceParam, s.handleCreateMigration))
	api.HandleFunc("GET /api/v1/migrations/{namespace}/{name}", s.requireNamespace(namespaceFromPath, s.handleGetMigration))
	// Evacuation authorizes the full batch before its first write.
	api.HandleFunc("POST /api/v1/migrations/evacuate", s.handleEvacuate)
	api.HandleFunc("POST /api/v1/migrations/{namespace}/{name}/recover", s.requireNamespace(namespaceFromPath, s.handleRecoverMigration))
	api.HandleFunc("POST /api/v1/migrations/{namespace}/{name}/cancel", s.requireNamespace(namespaceFromPath, s.handleCancelMigration))

	api.HandleFunc("GET /api/v1/snapshots", s.orAllNamespaces(s.requireNamespace(namespaceParam, s.handleListSnapshots), s.handleListAllSnapshots))
	api.HandleFunc("POST /api/v1/snapshots", s.requireNamespace(namespaceParam, s.handleCreateSnapshot))
	api.HandleFunc("GET /api/v1/restores", s.requireNamespace(namespaceParam, s.handleListRestores))
	api.HandleFunc("POST /api/v1/restores", s.requireNamespace(namespaceParam, s.handleCreateRestore))
	api.HandleFunc("DELETE /api/v1/restores/{namespace}/{name}", s.requireNamespace(namespaceFromPath, s.handleDeleteRestore))

	api.HandleFunc("GET /api/v1/quotas", s.requireNamespace(namespaceParam, s.handleListQuotas))
	api.HandleFunc("GET /api/v1/machinepools", s.requireNamespace(namespaceParam, s.handleListMachinePools))
	api.HandleFunc("GET /api/v1/machineclaims", s.requireNamespace(namespaceParam, s.handleListMachineClaims))
	api.HandleFunc("GET /api/v1/machinebackups", s.requireNamespace(namespaceParam, s.handleListMachineBackups))
	api.HandleFunc("GET /api/v1/machinebackuprestores", s.requireNamespace(namespaceParam, s.handleListMachineBackupRestores))
	api.HandleFunc("GET /api/v1/disruption-budgets", s.requireNamespace(namespaceParam, s.handleListBudgets))
	api.HandleFunc("GET /api/v1/machinesets", s.requireNamespace(namespaceParam, s.handleListMachineSets))
	api.HandleFunc("DELETE /api/v1/machinesets/{namespace}/{name}", s.requireNamespace(namespaceFromPath, s.handleDeleteMachineSet))
	api.HandleFunc("PATCH /api/v1/machinesets/{namespace}/{name}/scale", s.requireNamespace(namespaceFromPath, s.handleScaleMachineSet))
	api.HandleFunc("GET /api/v1/instancetypes", s.requireNamespace(namespaceParam, s.handleListInstanceTypes))
	api.HandleFunc("GET /api/v1/migration-policies", s.requireNamespace(namespaceParam, s.handleListMigrationPolicies))
	api.HandleFunc("GET /api/v1/snapshot-schedules", s.requireNamespace(namespaceParam, s.handleListMachineSnapshotSchedules))
	api.HandleFunc("PATCH /api/v1/snapshot-schedules/{namespace}/{name}/suspend", s.requireNamespace(namespaceFromPath, s.handleSuspendMachineSnapshotSchedule))
	api.HandleFunc("POST /api/v1/snapshot-schedules/{namespace}/{name}/trigger", s.requireNamespace(namespaceFromPath, s.handleTriggerSnapshotSchedule))
	api.HandleFunc("GET /api/v1/network-policies", s.requireNamespace(namespaceParam, s.handleListNetworkPolicies))
	api.HandleFunc("GET /api/v1/security-groups", s.requireNamespace(namespaceParam, s.handleListSecurityGroups))

	// Node-scoped routes below (path carries {node}, not {namespace}) are
	// deliberately NOT wrapped by requireNamespace: Node isn't a
	// namespaced kairon object at all (see model.Node), so there is no ns
	// to check -- an honest model limit of this feature, not an
	// oversight. The one exception is the sandbox-http proxy route just
	// below, whose path is /api/v1/machines/{namespace}/{name}/..., not
	// /api/v1/nodes/{node}/... -- it IS namespace-scoped and IS wrapped.
	api.HandleFunc("GET /api/v1/nodes", s.handleListNodes)
	api.HandleFunc("GET /api/v1/nodes/usage", s.handleNodeUsage)
	api.HandleFunc("GET /api/v1/nodes/capacity", s.handleNodeCapacity)
	api.HandleFunc("GET /api/v1/nodes/{node}/sandboxes", s.handleListNodeSandboxes)
	api.HandleFunc("GET /api/v1/nodes/{node}/templates", s.handleListTemplates)
	api.HandleFunc("POST /api/v1/nodes/{node}/templates", s.handleBuildTemplate)
	api.HandleFunc("/api/v1/machines/{namespace}/{name}/sandbox-http/{port}/{rest...}", s.requireNamespace(namespaceFromPath, s.handleSandboxHTTPProxy))
	api.HandleFunc("GET /api/v1/nodes/{node}/catalog", s.handleListCatalog)
	api.HandleFunc("POST /api/v1/nodes/{node}/catalog", s.handleAddCatalogEntry)
	api.HandleFunc("DELETE /api/v1/nodes/{node}/catalog/{name}", s.handleRemoveCatalogEntry)
	api.HandleFunc("POST /api/v1/nodes/{node}/catalog/{name}/rename", s.handleRenameCatalogEntry)
	api.HandleFunc("POST /api/v1/nodes/{node}/catalog/{name}/clone", s.handleCloneCatalogEntry)
	api.HandleFunc("POST /api/v1/nodes/{node}/catalog/{name}/export", s.handleExportCatalogEntry)
	api.HandleFunc("POST /api/v1/nodes/{node}/catalog/{name}/read-only", s.handleSetCatalogReadOnly)
	api.HandleFunc("POST /api/v1/nodes/{node}/catalog/clean", s.handleCleanCatalogDownloads)
	api.HandleFunc("POST /api/v1/nodes/{node}/egress-check", s.handleEgressCheck)
	api.HandleFunc("GET /api/v1/nodes/{node}/pools", s.handleListPools)
	api.HandleFunc("POST /api/v1/nodes/{node}/pools", s.handleCreatePool)
	api.HandleFunc("GET /api/v1/nodes/{node}/pools/{name}", s.handleGetPool)
	api.HandleFunc("DELETE /api/v1/nodes/{node}/pools/{name}", s.handleDeletePool)
	api.HandleFunc("POST /api/v1/nodes/{node}/pools/{name}/claim", s.handleClaimPool)
	api.HandleFunc("GET /api/v1/nodes/{node}/capabilities", s.handleRuntimeCapabilities)
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/pressure", s.requireNamespace(namespaceFromPath, s.handlePressure))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/cpuset", s.requireNamespace(namespaceFromPath, s.handleCPUSet))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/freeze", s.requireNamespace(namespaceFromPath, s.handleFreeze))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/thaw", s.requireNamespace(namespaceFromPath, s.handleThaw))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/frozen", s.requireNamespace(namespaceFromPath, s.handleFrozen))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-effective", s.requireNamespace(namespaceFromPath, s.handleNetworkEffective))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-stats", s.requireNamespace(namespaceFromPath, s.handleNetworkStats))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-flows", s.requireNamespace(namespaceFromPath, s.handleNetworkFlows))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-drop-reasons", s.requireNamespace(namespaceFromPath, s.handleNetworkDropReasons))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-drops", s.requireNamespace(namespaceFromPath, s.handleNetworkDrops))
	api.HandleFunc("POST /api/v1/machines/{namespace}/{name}/network-capture", s.requireNamespace(namespaceFromPath, s.handleNetworkCapture))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-capture", s.requireNamespace(namespaceFromPath, s.handleNetworkCaptures))
	api.HandleFunc("GET /api/v1/machines/{namespace}/{name}/network-capture/{token}", s.requireNamespace(namespaceFromPath, s.handleNetworkCaptureFile))
	// GET /api/v1/config reports server-wide feature flags (e.g.
	// consoleEnabled), not per-namespace state -- not namespace-scoped.
	api.HandleFunc("GET /api/v1/config", s.handleConfig)
	api.HandleFunc("GET /api/v1/atlas/{path...}", s.handleAtlas)

	// Neither of these two is namespace-scoped: {username} is not
	// {namespace}, and a password is a per-account credential, not a
	// per-namespace one.
	api.HandleFunc("GET /api/v1/images", s.handleListImages)
	api.HandleFunc("PUT /api/v1/images/{name}", s.handleUploadImage)
	api.HandleFunc("DELETE /api/v1/images/{name}", s.handleDeleteImage)

	api.HandleFunc("POST /api/v1/auth/password", s.handleSetOwnPassword)
	api.HandleFunc("POST /api/v1/users/{username}/password", s.handleResetPassword)

	top.Handle("/api/v1/", s.withAudit(s.withAuth(s.withActionAuthorization(api))))

	// Unauthenticated by necessity: login must be reachable without a
	// token to be useful at all; config/logout follow the same
	// unauthenticated convention as /healthz above.
	top.HandleFunc("GET /api/v1/auth/config", s.handleAuthConfig)
	top.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	top.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	// OIDC/SSO (see oidc.go) -- unauthenticated for the same reason as
	// auth/login above: this IS how a browser authenticates.
	top.HandleFunc("GET /api/v1/auth/oidc/login", s.handleOIDCLogin)
	top.HandleFunc("GET /api/v1/auth/oidc/callback", s.handleOIDCCallback)
	// Also unauthenticated at this layer by necessity: a browser's native
	// WebSocket API can't send an Authorization header, so this route is
	// gated by the single-use ticket handleConsoleTicket issues instead
	// (that ticket-issuing call *is* behind the normal auth above).
	top.HandleFunc("GET /api/v1/machines/{namespace}/{name}/console", s.handleConsole)
	// Unauthenticated: kairon-node downloads uploaded images by digest and
	// verifies them itself -- see handleServeImageBlob.
	top.HandleFunc("GET /images/sha256/{digest}", s.handleServeImageBlob)
	// The SPA route is intentionally unauthenticated (same as netra's own
	// serveWeb registration) -- it serves static JS/CSS/HTML, not data;
	// every actual data fetch the page makes goes through the auth-gated
	// /api/v1/... routes above.
	top.HandleFunc("/", s.serveWeb)
	return s.withMetrics(top, api, s.withRateLimit(top))
}

// routePattern reports the registered mux pattern a request matches --
// e.g. "/api/v1/machines/{namespace}/{name}" -- never the raw request
// path, so withMetrics's route label stays bounded regardless of how
// many distinct {namespace}/{name} values ever get requested (the same
// reasoning status_class already applies to the full HTTP status range).
// top's own patterns cover /healthz, /readyz, /metrics, the
// unauthenticated auth/oidc/console routes, and the "/" SPA catch-all
// directly; everything under the api sub-mux (mounted at top's own
// "/api/v1/" prefix pattern) needs a second lookup against api itself to
// get the specific route rather than that one prefix -- a sub-lookup
// that finds nothing there is a genuine 404 inside /api/v1/, reported as
// "unmatched" rather than falsely collapsing onto the outer "/api/v1/"
// prefix pattern (which would otherwise conflate every unknown API path
// with the different, real case of something actually registered at
// exactly that prefix). A leading "METHOD " a pattern may carry (Go's
// ServeMux includes it verbatim in the pattern string for method-scoped
// registrations) is stripped -- method is already its own separate
// label, so keeping it here would only duplicate it inside route.
func routePattern(top, api *http.ServeMux, r *http.Request) string {
	_, pattern := top.Handler(r)
	if pattern == "/api/v1/" {
		_, sub := api.Handler(r)
		if sub == "" {
			return "unmatched"
		}
		pattern = sub
	}
	if pattern == "" {
		return "unmatched"
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	return pattern
}

// withRateLimit rejects a request with 429 (Retry-After set, same
// convention handleLogin's own per-username lockout already uses) once
// its remote address has exceeded RateLimit's configured rate --
// deliberately outermost (ahead of withAuth, even ahead of routing to
// /healthz/readyz), the same reasoning withAudit/withMetrics already
// document: request volume from one address against an unauthenticated or
// rejected route is exactly what this needs to bound, not just successful
// authenticated calls. No-op wrapper when RateLimit isn't configured, so
// this changes nothing about behavior or performance without it.
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	if s.RateLimit == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.clientIP(r)
		if allowed, retryAfter := s.RateLimit.Allow(key); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			if s.Log != nil {
				s.Log.Warn("uiapi rate limited", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			}
			writeError(w, http.StatusTooManyRequests, "too many requests; try again later")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// remoteIP strips the port from r.RemoteAddr (host:port) so the rate
// limiter's per-key bucket is keyed by address alone -- falls back to the
// raw value unchanged if it doesn't parse as host:port (defensive; Go's
// own net/http always sets RemoteAddr in that shape for a real
// connection).
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// clientIP returns the address withRateLimit should key its bucket by:
// r.RemoteAddr (the TCP peer) unchanged, unless TrustedProxyHeader and
// TrustedProxyCIDRs are both configured AND that peer itself falls inside
// one of those CIDRs -- only then is TrustedProxyHeader's value trusted
// at all, since without that check any direct client could set the
// header itself to spoof or collide with another client's bucket. When
// trusted, takes the leftmost entry of a comma-separated header value
// (X-Forwarded-For's own convention: each proxy appends to the right, so
// the leftmost entry is what the first hop -- the real client -- sent),
// falling back to r.RemoteAddr if the header is absent or doesn't parse
// as an IP.
func (s *Server) clientIP(r *http.Request) string {
	direct := remoteIP(r.RemoteAddr)
	if s.TrustedProxyHeader == "" || len(s.TrustedProxyCIDRs) == 0 {
		return direct
	}
	peer := net.ParseIP(direct)
	if peer == nil {
		return direct
	}
	trusted := false
	for _, cidr := range s.TrustedProxyCIDRs {
		if cidr.Contains(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return direct
	}
	header := r.Header.Get(s.TrustedProxyHeader)
	if header == "" {
		return direct
	}
	forwarded := strings.TrimSpace(strings.SplitN(header, ",", 2)[0])
	if net.ParseIP(forwarded) == nil {
		return direct
	}
	return forwarded
}

// withMetrics wraps every route (auth-gated or not, including /healthz/
// readyz/metrics themselves) with request count/latency observation --
// deliberately outermost, the same reasoning withAudit already documents
// for wrapping outside withAuth: request volume/latency on a rejected or
// unauthenticated call (a failed login, a probe) is real signal too, not
// just successful API calls. No-op wrapper when Metrics isn't configured,
// so this changes nothing about behavior or performance without it.
// top/api are passed through only to resolve the request's route label
// (see routePattern) -- next is the already-composed rate-limit/mux
// chain those two build, still what actually serves the request.
func (s *Server) withMetrics(top *http.ServeMux, api *routeMux, next http.Handler) http.Handler {
	if s.Metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		route := routePattern(top, api.ServeMux, r)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.Metrics.ObserveHTTPRequest(r.Method, route, rec.status, time.Since(start))
	})
}

// serveWeb serves the built web/dist SPA from WebDir. Path safety comes
// from http.Dir.Open (Clean + reject ".."), which CodeQL treats as a
// sanitizer for go/path-injection. Missing paths fall back to
// index.html for SPA client-side routing.
func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	if s.WebDir == "" {
		if r.URL.Path == "/" {
			writeJSON(w, http.StatusOK, map[string]any{"name": "kairon-ui", "api": "/api/v1/overview"})
			return
		}
		http.NotFound(w, r)
		return
	}
	fsys := http.Dir(s.WebDir)
	name := path.Clean("/" + r.URL.Path)
	if name == "/" {
		name = "/index.html"
	}
	f, err := fsys.Open(name)
	needIndex := err != nil
	if err == nil {
		st, stErr := f.Stat()
		if stErr != nil || st.IsDir() {
			_ = f.Close()
			needIndex = true
		}
	}
	if needIndex {
		f, err = fsys.Open("/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		name = "/index.html"
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "static file not seekable", http.StatusInternalServerError)
		return
	}
	if ext := path.Ext(name); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), rs)
}

// withAudit logs every mutating request (anything but GET) that reaches
// /api/v1/... -- method, path, remote address, and the resulting status
// code, including auth failures (it wraps outside withAuth deliberately,
// so a rejected token attempt against a destructive route is itself part
// of the trail). This is a partial fix for kairon-ui having no audit
// trail at all: it does not attribute an action to an individual operator
// (auth today is one shared bearer token -- see cmd/kairon-ui/main.go),
// but "some trail" is a real improvement over "none," and doing better
// than that needs a real per-operator auth model, a separate decision.
func (s *Server) withAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// withUsernameHolder lets withAuth, deep inside next, report back
		// which operator authenticated (session-token auth only -- the
		// legacy shared token has no identity to report). Always attached
		// -- not just when this handler goes on to log -- since other
		// handlers downstream (e.g. handleConsoleTicket, binding a ticket
		// to its issuer) rely on reading it back via usernameFromContext
		// regardless of method or whether a logger is configured.
		r, holder := withUsernameHolder(r)
		// withGroupsHolder mirrors withUsernameHolder exactly, one level
		// down: an OIDC session's own groups, so isAdminIdentity can read
		// them back via groupsFromContext downstream.
		r, _ = withGroupsHolder(r)
		if r.Method == http.MethodGet || s.Log == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		fields := []any{"method", r.Method, "path", r.URL.Path, "remoteAddr", r.RemoteAddr, "status", rec.status}
		if *holder != "" {
			fields = append(fields, "user", *holder)
		}
		s.Log.Info("uiapi request", fields...)
	})
}

// statusRecorder captures the status code a handler actually wrote, since
// http.ResponseWriter itself doesn't expose it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush lets statusRecorder satisfy http.Flusher by delegating to the
// ResponseWriter it wraps, when that one supports it -- without this, a
// streaming handler behind withMetrics (e.g. handleLogs' follow=true
// tail) would silently lose incremental flushing: a `w.(http.Flusher)`
// type assertion on a bare *statusRecorder never succeeds on its own,
// since embedding the http.ResponseWriter *interface* only promotes that
// interface's own methods, not Flush (a separate, optional interface),
// regardless of what the concrete value underneath actually implements.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withAuth accepts either credential: the legacy shared static token
// (constant-time compared, as before) or a signed, unexpired, unrevoked
// session token from POST /api/v1/auth/login -- whichever Server has
// configured (Token, Users, or both). Neither configured means
// unauthenticated dev mode, unchanged from before.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" && s.userCount() == 0 && s.OIDC == nil {
			next.ServeHTTP(w, r)
			return
		}
		tok := bearerToken(r)
		if tok != "" {
			if s.Token != "" && len(tok) == len(s.Token) && subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			// Gated on SessionSecret being configured, not on userCount()
			// -- a session token verifies the same way regardless of
			// whether it came from POST /api/v1/auth/login (Users) or the
			// OIDC callback (OIDC); both require SessionSecret to be set
			// at startup (see cmd/kairon-ui/main.go), and an OIDC-only
			// deployment (no ui.auth.users at all) must still verify its
			// own sessions here, not fall through to the "wide open"
			// branch above.
			if len(s.SessionSecret) > 0 {
				if username, groups, _, issuedAt, err := verifySession(s.SessionSecret, tok); err == nil && !s.isSessionRevoked(tok) && !s.passwordChangedAfter(username, issuedAt) {
					setContextUsername(r.Context(), username)
					setContextGroups(r.Context(), groups)
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token")
	})
}

func namespaceParam(r *http.Request) string {
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		return ns
	}
	return "default"
}

// allNamespaces is the "?namespace=" value that asks a list route for every
// namespace the caller may see instead of one. Only routes registered through
// orAllNamespaces honour it; everywhere else it is just a namespace name that
// holds no objects.
const allNamespaces = "_all"

// orAllNamespaces dispatches "?namespace=_all" to all (which does its own
// per-namespace authorization) and every other request to scoped (normally
// the requireNamespace-wrapped single-namespace handler).
func (s *Server) orAllNamespaces(scoped, all http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") == allNamespaces {
			all(w, r)
			return
		}
		scoped(w, r)
	}
}

// allNamespacesAllowed applies the rule shared by every ?namespace=_all list:
// with namespace scoping on, a caller without a per-operator identity is
// refused (the same rule requireNamespace applies). It writes the 403 and
// reports false in that case.
func (s *Server) allNamespacesAllowed(w http.ResponseWriter, r *http.Request) bool {
	if s.NamespaceScopingEnabled && usernameFromContext(r.Context()) == "" {
		writeError(w, http.StatusForbidden, "listing every namespace requires a per-operator identity when namespace scoping is on")
		return false
	}
	return true
}

// namespaceFromPath is namespaceParam's counterpart for a route registered
// with a literal "{namespace}" path segment (Go 1.22+ ServeMux pattern
// syntax) -- e.g. "GET /api/v1/machines/{namespace}/{name}". Safe to call
// from inside a handler wrapped by requireNamespace below: by the time
// that wrapper's own next(w, r) call reaches this, the mux has already
// matched and dispatched to this specific registered pattern, so
// r.PathValue("namespace") is already populated.
func namespaceFromPath(r *http.Request) string {
	return r.PathValue("namespace")
}

// requireNamespace wraps a namespaced route's handler so it 403s a caller
// authorizedForNamespace (auth.go) rejects, before next ever runs. getNS
// extracts the namespace this particular route's request carries --
// namespaceParam for a query-param route, namespaceFromPath for a
// {namespace} path-segment route.
//
// This wraps each route's own handler, registered directly in api's mux,
// rather than being one outer middleware layered above api as a whole
// (the way withAuth/withAudit/withMetrics are) -- deliberately, because
// it can't work that way here. Handler() below mounts api as
// `top.Handle("/api/v1/", s.withAudit(s.withAuth(s.withActionAuthorization(api))))`: withAuth wraps
// api from *outside*, and Go's http.ServeMux only populates
// r.PathValue(...) once it has matched and dispatched to the specific
// registered handler *inside* api -- which hasn't happened yet at that
// outer layer. A namespaceFromPath call made from an outer wrapper would
// always see an empty string. Per-route wrapping at registration, as
// done throughout Handler() below, is what makes r.PathValue("namespace")
// (and namespaceParam's query-string read) actually available when
// getNS(r) runs.
func (s *Server) requireNamespace(getNS func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := getNS(r)
		if !s.authorizedForNamespace(r.Context(), ns) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("not authorized for namespace %q", ns))
			return
		}
		next(w, r)
	}
}

// defaultMaxRequestBodyBytes bounds every ordinary JSON request body
// decodeJSON reads -- closing a real gap nothing in kairon-ui's HTTP stack
// previously had at all: no handler, and no shared http.Server field
// either, ever capped request body size, so a caller (even a legitimately
// authenticated one) could send an arbitrarily large body and have
// json.Decoder buffer all of it into memory before ever getting to
// DisallowUnknownFields's own rejection. 1MiB comfortably covers every
// ordinary JSON body this API accepts (Machine/MachineSet specs, exec
// commands, pool/catalog requests, etc.) -- none of which legitimately
// approach that size. decodeJSONWithLimit is the override for the one
// real exception, guest file writes -- see agentfile.go.
const defaultMaxRequestBodyBytes = 1 << 20 // 1MiB

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	return decodeJSONWithLimit(w, r, out, defaultMaxRequestBodyBytes)
}

// decodeJSONWithLimit is decodeJSON with an explicit body-size cap in
// place of defaultMaxRequestBodyBytes. limit must be reachable by
// http.MaxBytesReader, which requires w to satisfy net/http's internal
// requestTooLarge hook -- true for every ResponseWriter this package ever
// hands it, all real *http.response values from the standard mux.
func decodeJSONWithLimit(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("request body exceeds the %d byte limit", limit)
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeUpstreamError maps a kube.Client error to an HTTP status: a real
// Kubernetes 404 becomes a 404 here (not a generic 502), everything else
// is a 502 -- the UI backend has no opinion of its own on Kubernetes
// object state, it only relays what the API server actually said.
func writeUpstreamError(w http.ResponseWriter, err error) {
	if kube.IsNotFound(err) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

var invalidResourceName = regexp.MustCompile(`[^a-z0-9-]+`)

// resourceName mirrors kaironctl's own sanitizer (cmd/kaironctl/main.go)
// exactly, so a name auto-generated by this API and one auto-generated by
// the CLI for the same input collide the same way (both are deterministic
// functions of the same inputs, not random).
func resourceName(s string) string {
	s = strings.ToLower(s)
	s = invalidResourceName.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// routeMux counts registrations so the sign-in page can honestly brag about
// the API surface (see handleAuthConfig's stats) without a hand-kept number.
type routeMux struct {
	*http.ServeMux
	n *int
}

func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	*m.n++
	m.ServeMux.HandleFunc(pattern, h)
}
