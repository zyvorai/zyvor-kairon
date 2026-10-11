// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/ratelimit"
	"github.com/zyvorai/kairon/internal/uiapi"
)

var version = "dev"

// sharedStateSyncInterval bounds how often this replica polls the shared
// ConfigMap and the Users-backing Secret for state a different replica
// wrote -- see uiapi.Server.RunSharedStateSync's own doc comment. Same
// shape as kairon-node/kairon-controller's own tlsReloadInterval consts:
// the interval lives with the caller, not the reusable package.
const sharedStateSyncInterval = 15 * time.Second

func main() {
	os.Exit(run())
}

// run returns the process exit code rather than calling os.Exit directly,
// so every deferred cleanup (e.g. cancel()) actually runs before exit.
func run() int {
	if len(os.Args) >= 2 && os.Args[1] == "-hash-password" {
		return hashPassword(os.Args[2:])
	}
	atlasURL := flag.String("atlas-url", os.Getenv("KAIRON_UI_ATLAS_URL"), "Zyvor Atlas storage gateway base URL (e.g. http://atlas.atlas-system:5110); enables the read-only Storage views. Empty disables them")
	atlasTokenFile := flag.String("atlas-token-file", os.Getenv("KAIRON_UI_ATLAS_TOKEN_FILE"), "file holding the Atlas bearer token (falls back to KAIRON_UI_ATLAS_TOKEN)")
	atlasConsoleURL := flag.String("atlas-console-url", os.Getenv("KAIRON_UI_ATLAS_CONSOLE_URL"), "optional browser URL of the Atlas console, shown as an \"Open in Atlas\" link")
	listenAddr := flag.String("listen", env("KAIRON_UI_LISTEN", ":18082"), "HTTP listen address (serves both /api/v1/... and the built web UI)")
	webDir := flag.String("web-dir", env("KAIRON_UI_WEB_DIR", ""), "directory containing the built web/dist SPA; empty serves API-only")
	token := flag.String("token", os.Getenv("KAIRON_UI_TOKEN"), "static bearer token required on every /api/v1/... request (default: $KAIRON_UI_TOKEN)")
	allowUnauthenticated := flag.Bool("allow-unauthenticated", env("KAIRON_UI_ALLOW_UNAUTHENTICATED", "false") == "true", "start without a token -- local development only, refused by default")
	rateLimitRPS := flag.Float64("rate-limit-rps", 20, "requests per second allowed per remote address across every route, sustained (0 disables rate limiting entirely) -- distinct from and in addition to the per-username login lockout, which only throttles repeated failed passwords")
	rateLimitBurst := flag.Int("rate-limit-burst", 40, "requests a remote address may burst above -rate-limit-rps before throttling kicks in")
	trustedProxyHeader := flag.String("trusted-proxy-header", env("KAIRON_UI_TRUSTED_PROXY_HEADER", ""), "header (e.g. X-Forwarded-For) to read the real client address from for rate limiting, instead of the immediate TCP peer -- only trusted from a peer matching -trusted-proxy-cidrs; empty (the default) is unchanged behavior")
	trustedProxyCIDRs := flag.String("trusted-proxy-cidrs", env("KAIRON_UI_TRUSTED_PROXY_CIDRS", ""), "comma-separated CIDRs (e.g. your Ingress/load-balancer's pod or node network) that -trusted-proxy-header is ever trusted from; required alongside it, otherwise any direct client could spoof that header")
	namespaceScopingEnabled := flag.Bool("namespace-scoping-enabled", env("KAIRON_UI_NAMESPACE_SCOPING_ENABLED", "false") == "true", "restrict each non-admin session-token operator to the namespaces listed in their own ui.auth.users[].namespaces entry or reachable via ui.oidc.namespaceGroups; false (the default) is today's unchanged behavior -- every authenticated operator sees and acts on every namespace")
	imageStoreDir := flag.String("image-store-dir", env("KAIRON_UI_IMAGE_STORE_DIR", ""), "directory for images uploaded with kaironctl image upload; empty disables the image store")
	imageStorePublicURL := flag.String("image-store-public-url", env("KAIRON_UI_IMAGE_STORE_PUBLIC_URL", ""), "base URL kairon-node downloads uploaded images from (e.g. http://kairon-ui.kairon-system.svc:18082); empty uses the upload request's host")
	imageStoreMaxBytes := flag.Int64("image-store-max-bytes", envInt64("KAIRON_UI_IMAGE_STORE_MAX_BYTES", 64<<30), "largest accepted upload in bytes (0 = unlimited)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return 0
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	atlasToken := os.Getenv("KAIRON_UI_ATLAS_TOKEN")
	if *atlasTokenFile != "" {
		b, err := os.ReadFile(*atlasTokenFile)
		if err != nil {
			log.Error("read -atlas-token-file", "error", err)
			os.Exit(1)
		}
		atlasToken = strings.TrimSpace(string(b))
	}

	users, err := loadUsers(os.Getenv("KAIRON_UI_USERS_JSON"), os.Getenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD"))
	if err != nil {
		log.Error("loading users", "error", err)
		return 1
	}

	// OIDC/SSO config is all-or-nothing: issuer, client ID, and redirect
	// URL together, or none at all -- same "fail closed on a half-set
	// group of flags" posture as -migration-data-tls's own three flags.
	// kairon-ui can't safely guess its own externally-reachable HTTPS URL
	// (proxies, ingress hostnames vary), so the redirect URL is always
	// explicit, never inferred from the incoming request.
	oidcIssuerURL := os.Getenv("KAIRON_UI_OIDC_ISSUER_URL")
	oidcClientID := os.Getenv("KAIRON_UI_OIDC_CLIENT_ID")
	oidcRedirectURL := os.Getenv("KAIRON_UI_OIDC_REDIRECT_URL")
	oidcConfigured := oidcIssuerURL != "" || oidcClientID != "" || oidcRedirectURL != ""
	if oidcConfigured && (oidcIssuerURL == "" || oidcClientID == "" || oidcRedirectURL == "") {
		log.Error("secure startup refused", "reason", "OIDC/SSO requires $KAIRON_UI_OIDC_ISSUER_URL, $KAIRON_UI_OIDC_CLIENT_ID, and $KAIRON_UI_OIDC_REDIRECT_URL together")
		return 1
	}

	if (len(users) > 0 || oidcConfigured) && os.Getenv("KAIRON_UI_SESSION_SECRET") == "" {
		log.Error("secure startup refused", "reason", "$KAIRON_UI_SESSION_SECRET is required whenever username/password login or OIDC/SSO is configured")
		return 1
	}

	// Secure startup refusal, same posture as every other kairon
	// component's fail-closed validation (e.g. -migration-data-tls
	// requiring all three cert flags together): kairon-ui is a
	// human-facing dashboard capable of creating/deleting Machines and
	// forcing migration recovery actions, so it must not silently come up
	// wide open just because an operator forgot to set a token, a user,
	// or OIDC/SSO.
	if *token == "" && len(users) == 0 && !oidcConfigured && !*allowUnauthenticated {
		log.Error("secure startup refused", "reason", "-token (or $KAIRON_UI_TOKEN), $KAIRON_UI_USERS_JSON/$KAIRON_UI_DEFAULT_ADMIN_PASSWORD, or OIDC/SSO is required unless -allow-unauthenticated is set")
		return 1
	}
	if *token == "" && len(users) == 0 && !oidcConfigured {
		log.Warn("unauthenticated mode enabled -- every /api/v1/... route is open to anyone who can reach this server")
	}

	kc, err := kube.FromEnvironment()
	if err != nil {
		log.Error("kubernetes client", "error", err)
		return 1
	}
	rec := metrics.NewUIRecorder()
	kc.Observe = rec.ObserveAPIRequest

	consoleTLS, err := consoleTLSConfig(os.Getenv("KAIRON_NODE_CONSOLE_CA"))
	if err != nil {
		log.Error("console TLS", "error", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var rateLimiter *ratelimit.Limiter
	if *rateLimitRPS > 0 {
		rateLimiter = ratelimit.New(*rateLimitRPS, *rateLimitBurst)
		go rateLimiter.Run(ctx, 5*time.Minute, 30*time.Minute)
	}

	// -trusted-proxy-header/-trusted-proxy-cidrs are all-or-nothing, same
	// "fail closed on a half-set group of flags" posture OIDC's three
	// flags already use above -- a header configured with no trusted
	// CIDRs (or vice versa) is silently never applied by uiapi.Server
	// itself (see clientIP), so refusing startup here surfaces the
	// misconfiguration immediately instead of an operator wondering why
	// rate limiting still collapses behind their load balancer.
	trustedProxyConfigured := *trustedProxyHeader != "" || *trustedProxyCIDRs != ""
	if trustedProxyConfigured && (*trustedProxyHeader == "" || *trustedProxyCIDRs == "") {
		log.Error("secure startup refused", "reason", "-trusted-proxy-header and -trusted-proxy-cidrs must both be set together, or neither")
		return 1
	}
	var trustedProxyNets []*net.IPNet
	for _, cidr := range splitNonEmpty(*trustedProxyCIDRs, ",") {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Error("secure startup refused", "reason", "invalid -trusted-proxy-cidrs entry", "cidr", cidr, "error", err)
			return 1
		}
		trustedProxyNets = append(trustedProxyNets, ipNet)
	}

	namespaceGroups, err := loadNamespaceGroups(os.Getenv("KAIRON_UI_OIDC_NAMESPACE_GROUPS"))
	if err != nil {
		log.Error("parsing $KAIRON_UI_OIDC_NAMESPACE_GROUPS", "error", err)
		return 1
	}

	var oidcAuth *uiapi.OIDCAuth
	if oidcConfigured {
		discoverCtx, discoverCancel := context.WithTimeout(ctx, 15*time.Second)
		oidcAuth, err = newOIDCAuth(discoverCtx, oidcIssuerURL, oidcClientID, os.Getenv("KAIRON_UI_OIDC_CLIENT_SECRET"), oidcRedirectURL,
			env("KAIRON_UI_OIDC_USERNAME_CLAIM", "email"), env("KAIRON_UI_OIDC_GROUPS_CLAIM", "groups"),
			strings.Split(env("KAIRON_UI_OIDC_SCOPES", "openid,profile,email"), ","), splitNonEmpty(env("KAIRON_UI_OIDC_ADMIN_GROUPS", ""), ","), namespaceGroups)
		discoverCancel()
		if err != nil {
			log.Error("OIDC/SSO setup failed", "error", err)
			return 1
		}
		log.Info("OIDC/SSO configured", "issuer", oidcIssuerURL, "clientID", oidcClientID)
	}

	srv := &uiapi.Server{
		Kube:            kc,
		Log:             log,
		Token:           *token,
		WebDir:          *webDir,
		Users:           users,
		SessionSecret:   []byte(os.Getenv("KAIRON_UI_SESSION_SECRET")),
		LoginHint:       loginHint(),
		DefaultPassword: wellKnownPassword(),
		AtlasURL:        *atlasURL,
		AtlasToken:      atlasToken,
		AtlasConsoleURL: *atlasConsoleURL,
		Version:         version,
		// UsersSecretName empty (the default) means POST
		// /api/v1/auth/password and POST /api/v1/users/{username}/password
		// are refused -- set only when the Helm chart owns the
		// kairon-ui-users Secret it seeded Users from (see
		// charts/kairon/templates/all.yaml); a bare-metal deploy
		// (scripts/deploy-remote.sh) has no Kubernetes Secret to write
		// back to and leaves these unset, which is the correct fallback,
		// not a bug.
		UsersSecretNamespace: os.Getenv("KAIRON_UI_NAMESPACE"),
		UsersSecretName:      os.Getenv("KAIRON_UI_USERS_SECRET_NAME"),
		UsersSecretKey:       env("KAIRON_UI_USERS_SECRET_KEY", "users.json"),
		// SharedStateConfigMapName empty (the default) means every
		// mutation below stays purely in this process's own memory --
		// correct for a single replica. Set only once ui.replicaCount > 1
		// (see the Helm chart), so session revocation, login lockout, and
		// console tickets stay correct across replicas too -- see
		// docs/guides/kairon-ui-ha.md.
		SharedStateNamespace:     os.Getenv("KAIRON_UI_NAMESPACE"),
		SharedStateConfigMapName: os.Getenv("KAIRON_UI_SHARED_STATE_CONFIGMAP_NAME"),
		OIDC:                     oidcAuth,
		// ConsoleToken/ConsolePort must match the value every kairon-node
		// is configured with (KAIRON_NODE_CONSOLE_TOKEN/-console-addr);
		// either empty disables the VNC console feature (see console.go).
		ConsoleToken: os.Getenv("KAIRON_NODE_CONSOLE_TOKEN"),
		ConsolePort:  env("KAIRON_NODE_CONSOLE_PORT", "8090"),
		ConsoleTLS:   consoleTLS,
		// RBACConsoleCheck false (the default) is unchanged, annotation-
		// only console authorization -- see uiapi.Server's own doc comment.
		RBACConsoleCheck:        env("KAIRON_UI_RBAC_CONSOLE_CHECK", "false") == "true",
		Metrics:                 rec,
		RateLimit:               rateLimiter,
		TrustedProxyHeader:      *trustedProxyHeader,
		TrustedProxyCIDRs:       trustedProxyNets,
		NamespaceScopingEnabled: *namespaceScopingEnabled,
		ImageStoreDir:           *imageStoreDir,
		ImageStorePublicURL:     *imageStorePublicURL,
		ImageStoreMaxBytes:      *imageStoreMaxBytes,
	}
	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	go srv.RunSharedStateSync(ctx, sharedStateSyncInterval)

	log.Info("kairon-ui listening", "address", *listenAddr, "webDir", *webDir, "authenticated", *token != "" || len(users) > 0, "loginUsers", len(users))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http server", "error", err)
		return 1
	}
	return 0
}

// consoleTLSConfig builds the TLS config kairon-ui uses to verify a
// kairon-node's console-relay server certificate, from a CA PEM file path
// ($KAIRON_NODE_CONSOLE_CA). Empty caPath means every kairon-node's
// console listener is plaintext (ws://) -- the default, backward-compatible
// posture. One-way TLS only: the shared KAIRON_NODE_CONSOLE_TOKEN already
// authenticates kairon-ui to kairon-node, so no client certificate is
// needed here.
func consoleTLSConfig(caPath string) (*tls.Config, error) {
	if caPath == "" {
		return nil, nil
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read console CA %s: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("console CA %s contains no certificates", caPath)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}

// newOIDCAuth resolves an OIDC provider's discovery document
// (/.well-known/openid-configuration) and builds the uiapi.OIDCAuth
// oidc.go's handlers use for the rest of the process's life. A network
// call, deliberately made once at startup rather than lazily on first
// login -- a misconfigured issuer fails the container immediately (a
// visible CrashLoopBackOff), not a confusing 500 the first time an
// operator actually tries to sign in.
func newOIDCAuth(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL, usernameClaim, groupsClaim string, scopes, adminGroups []string, namespaceGroups map[string][]string) (*uiapi.OIDCAuth, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery against %s: %w", issuerURL, err)
	}
	return &uiapi.OIDCAuth{
		OAuth2: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  redirectURL,
			Scopes:       scopes,
		},
		// ClientSecret above may legitimately be empty -- a public client
		// relying on PKCE alone (oidc.go's handleOIDCLogin always sends a
		// PKCE challenge) is a normal, common OIDC pattern, not a
		// misconfiguration this needs to refuse.
		Verifier:      provider.Verifier(&oidc.Config{ClientID: clientID}),
		UsernameClaim: usernameClaim,
		// GroupsClaim/AdminGroups both empty (the default) means every
		// OIDC session stays a normal, non-admin operator identity
		// regardless of what groups the IdP reports -- exactly Kairon's
		// behavior before these existed. See uiapi.Server.isAdminIdentity.
		GroupsClaim: groupsClaim,
		AdminGroups: adminGroups,
		// NamespaceGroups empty (the default) means an OIDC session
		// contributes no namespaces of its own when
		// ui.auth.namespaceScoping.enabled is on -- see
		// uiapi.Server.authorizedForNamespace and loadNamespaceGroups
		// below.
		NamespaceGroups: namespaceGroups,
	}, nil
}

// namespaceGroupEntry is $KAIRON_UI_OIDC_NAMESPACE_GROUPS's own wire
// shape -- a JSON array (not a map) because it mirrors
// ui.oidc.namespaceGroups' own Helm values shape exactly
// (charts/kairon/values.yaml), which is itself a list of {group,
// namespaces} entries rather than a map, for the same reason
// ui.auth.users is a list rather than a map keyed by username: Helm
// values.yaml lists are easier to append to/override piecemeal than map
// keys are.
type namespaceGroupEntry struct {
	Group      string   `json:"group"`
	Namespaces []string `json:"namespaces"`
}

// loadNamespaceGroups parses $KAIRON_UI_OIDC_NAMESPACE_GROUPS (a JSON
// array of namespaceGroupEntry) into the map[string][]string shape
// uiapi.OIDCAuth.NamespaceGroups actually uses at request time. Empty
// input (the default -- ui.oidc.namespaceGroups unset) returns a nil map,
// same as every other namespace-scoping field defaulting to "off."
func loadNamespaceGroups(raw string) (map[string][]string, error) {
	if raw == "" {
		return nil, nil
	}
	var entries []namespaceGroupEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parse $KAIRON_UI_OIDC_NAMESPACE_GROUPS: %w", err)
	}
	out := make(map[string][]string, len(entries))
	for _, e := range entries {
		out[e.Group] = append(out[e.Group], e.Namespaces...)
	}
	return out, nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt64(k string, d int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil {
		return v
	}
	return d
}

// splitNonEmpty splits a comma-separated list and trims/drops empty
// entries -- unlike a bare strings.Split, an empty input produces an
// empty (nil) slice rather than []string{""}, so
// $KAIRON_UI_OIDC_ADMIN_GROUPS unset means len(AdminGroups) == 0
// (OIDC-derived admin disabled entirely), not a slice holding one
// spurious empty-string "group".
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// loadUsers builds the operator account list from $KAIRON_UI_USERS_JSON
// (pre-hashed accounts an operator configured directly) plus, if set, one
// additional account bcrypt-hashed right here from
// $KAIRON_UI_DEFAULT_ADMIN_PASSWORD -- the Helm-generated default admin
// password (see charts/kairon/templates/all.yaml). This is the only place
// a plaintext password ever exists in this process; every
// operator-configured account arrives already hashed.
func loadUsers(usersJSON, defaultAdminPassword string) ([]uiapi.User, error) {
	var users []uiapi.User
	if usersJSON != "" {
		if err := json.Unmarshal([]byte(usersJSON), &users); err != nil {
			return nil, fmt.Errorf("parse $KAIRON_UI_USERS_JSON: %w", err)
		}
	}
	if defaultAdminPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(defaultAdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash default admin password: %w", err)
		}
		users = append(users, uiapi.User{Username: "admin", PasswordHash: string(hash), IsAdmin: true})
	}
	return users, nil
}

// hashPassword implements `kairon-ui -hash-password PASSWORD`: prints a
// bcrypt hash suitable for ui.auth.users' passwordHash field and exits,
// without starting the server. A positional arg (not a stdin prompt) to
// avoid a TTY dependency; documented as better piped from a file/heredoc
// than typed, to avoid shell history for a real password.
func hashPassword(args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: kairon-ui -hash-password PASSWORD")
		return 2
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(args[0]), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(string(hash))
	return 0
}

const wellKnownDefaultPassword = "Admin@321"

// wellKnownPassword returns the chart's well-known lab password while it is
// the configured admin password, so logins using it can be flagged.
func wellKnownPassword() string {
	if os.Getenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD") == wellKnownDefaultPassword {
		return wellKnownDefaultPassword
	}
	return ""
}

// loginHint is the text shown under the dashboard sign-in form. An explicit
// KAIRON_UI_LOGIN_HINT wins; otherwise the well-known lab default is
// advertised only while the admin password is still that exact value.
func loginHint() string {
	if h := strings.TrimSpace(os.Getenv("KAIRON_UI_LOGIN_HINT")); h != "" {
		return h
	}
	if os.Getenv("KAIRON_UI_DEFAULT_ADMIN_PASSWORD") == "Admin@321" {
		return "Lab default login: admin / Admin@321 \u2014 change it before exposing this dashboard."
	}
	return ""
}
