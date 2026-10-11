// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User is one operator account: a username plus a bcrypt hash of their
// password, never the password itself. Server.Users is seeded at startup
// (see cmd/kairon-ui/main.go) from $KAIRON_UI_USERS_JSON and, optionally,
// one bcrypt-hashed-at-startup entry for a Helm-generated default admin
// account (see main.go's $KAIRON_UI_DEFAULT_ADMIN_PASSWORD) -- and can be
// mutated afterward via POST /api/v1/auth/password and
// POST /api/v1/users/{username}/password (see setOwnPassword/resetPassword
// below), which is why every read of Server.Users goes through usersMu.
type User struct {
	Username string `json:"username"`
	// Role optionally restricts a named account to read-only access.
	Role         string `json:"role,omitempty"`
	PasswordHash string `json:"passwordHash"`
	// IsAdmin gates POST /api/v1/users/{username}/password -- only an admin
	// account may reset another operator's password. The Helm-seeded
	// default admin account is always IsAdmin (see loadUsers in
	// cmd/kairon-ui/main.go); everyone else defaults to false unless
	// ui.auth.users[].admin is set.
	IsAdmin bool `json:"admin,omitempty"`
	// Namespaces is the explicit allowlist of Kubernetes namespaces this
	// account may act on, consulted only when
	// Server.NamespaceScopingEnabled is true (see authorizedForNamespace)
	// -- ignored entirely otherwise, and ignored for an IsAdmin account
	// regardless (an admin is always unrestricted). Empty/nil means "no
	// namespaces," not "all namespaces," once scoping is on for a
	// non-admin account -- an operator upgraded onto a freshly-enabled
	// ui.auth.namespaceScoping.enabled without an explicit list here sees
	// nothing, deliberately fail-closed rather than fail-open.
	Namespaces []string `json:"namespaces,omitempty"`
}

// dummyHash is compared against on an unknown username so a "no such user"
// response takes the same time as a "wrong password" one -- otherwise
// handleLogin would leak which usernames exist via response timing. It is
// never a real credential.
var dummyHash = mustBcryptHash("kairon-dummy-password-for-constant-time-compare")

func mustBcryptHash(pw string) []byte {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}

type contextKey string

// usernameHolderKey holds a *string set by withAudit before calling
// withAuth, so a successful session-token auth (deep inside withAuth) can
// report the authenticated username back out to the audit log -- without
// this indirection, withAudit (which must wrap *outside* withAuth so a
// rejected auth attempt is itself logged, see withAudit's own doc comment)
// would have no way to see what withAuth learned about the caller.
const usernameHolderKey contextKey = "kairon-ui-username-holder"

func withUsernameHolder(r *http.Request) (*http.Request, *string) {
	holder := new(string)
	return r.WithContext(context.WithValue(r.Context(), usernameHolderKey, holder)), holder
}

func setContextUsername(ctx context.Context, username string) {
	if holder, ok := ctx.Value(usernameHolderKey).(*string); ok {
		*holder = username
	}
}

// usernameFromContext reads back what withAuth (or a handler downstream of
// it) recorded via setContextUsername -- e.g. handleConsoleTicket uses
// this to bind an issued ticket to the operator who requested it, so the
// console's own audit log can attribute a session to a person, not just
// "someone with a valid ticket."
func usernameFromContext(ctx context.Context) string {
	if holder, ok := ctx.Value(usernameHolderKey).(*string); ok {
		return *holder
	}
	return ""
}

// groupsHolderKey mirrors usernameHolderKey exactly, one level down: an
// OIDC-issued session token carries the caller's own IdP group membership
// (see sessionPayload.Groups below), and isAdminIdentity needs it to
// re-check "is any of these groups currently in ui.oidc.adminGroups" fresh
// against config on every request -- the same freshness property
// findUser's own live s.Users lookup already gives the static admin path,
// rather than baking a computed true/false admin bit into the token at
// login time (which would go stale until the caller logs in again if
// ui.oidc.adminGroups changes). Always empty for a password-login session
// (Groups is only ever set by the OIDC callback).
const groupsHolderKey contextKey = "kairon-ui-groups-holder"

func withGroupsHolder(r *http.Request) (*http.Request, *[]string) {
	holder := new([]string)
	return r.WithContext(context.WithValue(r.Context(), groupsHolderKey, holder)), holder
}

func setContextGroups(ctx context.Context, groups []string) {
	if holder, ok := ctx.Value(groupsHolderKey).(*[]string); ok {
		*holder = groups
	}
}

func groupsFromContext(ctx context.Context) []string {
	if holder, ok := ctx.Value(groupsHolderKey).(*[]string); ok {
		return *holder
	}
	return nil
}

// sessionPayload is the signed, base64url-encoded JSON body of a session
// token. It carries no secret material -- forging one requires the HMAC
// key, not just reading a token, so the payload itself doesn't need to be
// hidden, only the signature needs to be unforgeable.
type sessionPayload struct {
	Subject string `json:"sub"`
	Expires int64  `json:"exp"`
	// IssuedAt lets withAuth reject a session issued before the subject's
	// most recent password reset (see Server.passwordChangedAt /
	// resetPassword below), without needing a server-side list of every
	// outstanding token.
	IssuedAt int64 `json:"iat"`
	// Groups is only ever set by the OIDC callback (handleOIDCCallback),
	// from the ID token's own OIDC.GroupsClaim -- empty/omitted for a
	// password-login session. isAdminIdentity re-checks these against
	// OIDC.AdminGroups fresh on every request, not baked into a computed
	// admin bit here, so a config change to adminGroups takes effect on
	// the caller's very next request rather than requiring re-login. See
	// groupsHolderKey's own doc comment.
	Groups []string `json:"groups,omitempty"`
}

const sessionTTL = 12 * time.Hour

// signSession builds "base64url(payload).base64url(HMAC-SHA256(payload,key))"
// -- a minimal hand-rolled equivalent of a JWT HS256 token, without adding a
// JWT library dependency for a handful of claims. groups is nil for a
// password-login session; see sessionPayload.Groups.
func signSession(key []byte, username string, groups []string, ttl time.Duration) (token string, expires time.Time, err error) {
	now := time.Now()
	expires = now.Add(ttl)
	payload, err := json.Marshal(sessionPayload{Subject: username, Expires: expires.Unix(), IssuedAt: now.Unix(), Groups: groups})
	if err != nil {
		return "", time.Time{}, err
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encPayload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encPayload + "." + sig, expires, nil
}

// verifySession checks the signature and expiry and returns the subject,
// its OIDC groups (nil for a password-login session; see
// sessionPayload.Groups), and when the token was issued (see
// sessionPayload.IssuedAt).
func verifySession(key []byte, token string) (username string, groups []string, expires time.Time, issuedAt time.Time, err error) {
	encPayload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", nil, time.Time{}, time.Time{}, errors.New("malformed session token")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encPayload))
	wantSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(sig) != len(wantSig) || !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return "", nil, time.Time{}, time.Time{}, errors.New("invalid session signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return "", nil, time.Time{}, time.Time{}, err
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, time.Time{}, time.Time{}, err
	}
	expires = time.Unix(p.Expires, 0)
	if time.Now().After(expires) {
		return "", nil, time.Time{}, time.Time{}, errors.New("session expired")
	}
	return p.Subject, p.Groups, expires, time.Unix(p.IssuedAt, 0), nil
}

// revokeSession/isSessionRevoked back POST /api/v1/auth/logout with a
// small in-memory, best-effort revocation list: sessions are otherwise
// stateless (no server-side session store), so logout needs *something* to
// actually invalidate the token rather than merely asking the client to
// forget it. Applied to this replica's own state immediately regardless of
// SharedStateConfigMapName -- a logout on the replica that served it is
// enforced instantly. Also best-effort mirrored into the shared ConfigMap
// (see sharedstate.go) so every OTHER replica picks it up within
// sharedStateSyncInterval; with SharedStateConfigMapName unset (the
// default), it stays exactly the single-replica-only behavior this always
// was. Keyed by a one-way hash of the token, not the token itself, in
// both places -- a revoked token grants nothing, but there's no reason to
// ever write a bearer-credential-shaped value into a shared object.
// Entries are lazily dropped once their own expiry passes, so this never
// grows without bound.
func (s *Server) revokeSession(ctx context.Context, token string, expires time.Time) {
	s.revoked.Store(sha256Hex(token), expires)
	s.writeSharedState(ctx, sharedRevocationKey(token), sharedRevocationEntry{Expires: expires})
}

func (s *Server) isSessionRevoked(token string) bool {
	key := sha256Hex(token)
	v, ok := s.revoked.Load(key)
	if !ok {
		return false
	}
	expires, _ := v.(time.Time)
	if time.Now().After(expires) {
		s.revoked.Delete(key)
		return false
	}
	return true
}

// passwordChangedAfter reports whether username's password was reset (via
// resetPassword) at or after sessionIssuedAt -- used by withAuth to reject
// a session token issued before that reset, since there's no server-side
// list of every outstanding token to individually revoke. Propagates to
// other replicas the same way revokeSession does -- see sharedstate.go.
func (s *Server) passwordChangedAfter(username string, sessionIssuedAt time.Time) bool {
	v, ok := s.passwordChangedAt.Load(username)
	if !ok {
		return false
	}
	changedAt, _ := v.(time.Time)
	return !sessionIssuedAt.After(changedAt)
}

func (s *Server) userCount() int {
	s.usersMu.RLock()
	defer s.usersMu.RUnlock()
	return len(s.Users)
}

func (s *Server) findUser(username string) (User, bool) {
	s.usersMu.RLock()
	defer s.usersMu.RUnlock()
	for _, u := range s.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

// isAdminIdentity reports whether username (as already resolved by the
// caller via usernameFromContext) currently counts as an admin -- either
// a static ui.auth.users[].admin account (findUser, unchanged), or an
// OIDC session whose own recorded groups (groupsFromContext -- empty for
// a password-login session or when ctx carries none) currently intersect
// s.OIDC.AdminGroups. Both halves are re-checked fresh on every call
// against current s.Users/s.OIDC state, never cached on the token or
// context -- an operator changing ui.auth.users[].admin or
// ui.oidc.adminGroups takes effect on a caller's very next request,
// matching findUser's own existing freshness property, not requiring
// re-login either way.
//
// Deliberately does NOT special-case an empty username (the legacy
// shared-token caller) -- findUser("") already returns found=false and no
// OIDC session exists for that caller either, so this returns false for
// it, the same as every admin-gated route's own pre-existing
// `!found || !user.IsAdmin` check already did. Call sites that want the
// legacy shared token treated as admin (see handleResetPassword's own
// comment on why) keep doing that themselves, unchanged, around a call to
// this function -- not every admin-gated route in this codebase makes
// that same choice today, and this helper doesn't paper over or change
// that existing inconsistency.
func (s *Server) isAdminIdentity(ctx context.Context, username string) bool {
	if user, found := s.findUser(username); found && user.IsAdmin {
		return true
	}
	if s.OIDC == nil {
		return false
	}
	return groupsContainAdmin(groupsFromContext(ctx), s.OIDC.AdminGroups)
}

// authorizedForNamespace reports whether the caller identified by ctx (the
// same identity isAdminIdentity reads, via usernameFromContext/
// groupsFromContext) may act on namespace ns.
//
// Always true when Server.NamespaceScopingEnabled is false -- today's
// unchanged behavior, and the default; flipping every existing non-admin
// operator to "sees nothing" the moment this field existed would be a
// real break, so scoping is opt-in and this is its single off-switch.
// Admin identities remain unrestricted. Scoped deployments reject shared
// tokens and unauthenticated callers because they have no tenant identity.
//
// Otherwise: true iff ns is in the union of the caller's static
// User.Namespaces and every OIDCAuth.NamespaceGroups entry for a group
// the caller's ID token currently carries. Both halves are re-derived
// fresh on every call against current s.Users/s.OIDC state and the
// request's own groupsFromContext -- never cached on the token or
// context, exactly like isAdminIdentity, so a config change to
// ui.auth.users[].namespaces or ui.oidc.namespaceGroups takes effect on a
// caller's very next request rather than requiring re-login.
func (s *Server) authorizedForNamespace(ctx context.Context, ns string) bool {
	if !s.NamespaceScopingEnabled {
		return true
	}
	username := usernameFromContext(ctx)
	if username == "" {
		return false // Scoped deployments require an authenticated identity.
	}
	if s.isAdminIdentity(ctx, username) {
		return true
	}
	if user, found := s.findUser(username); found {
		for _, allowed := range user.Namespaces {
			if allowed == ns {
				return true
			}
		}
	}
	if s.OIDC != nil {
		for _, g := range groupsFromContext(ctx) {
			for _, allowed := range s.OIDC.NamespaceGroups[g] {
				if allowed == ns {
					return true
				}
			}
		}
	}
	return false
}

// groupsContainAdmin is isAdminIdentity's OIDC-half, factored out so
// handleOIDCCallback (internal/uiapi/oidc.go) can also compute the same
// answer directly from a just-decoded claims groups slice, before that
// slice is ever recorded on a session token/context -- used to report
// isAdmin back to the frontend in the callback's own redirect, the same
// way handleLogin's JSON response already does for a password session.
func groupsContainAdmin(groups, adminGroups []string) bool {
	for _, g := range groups {
		for _, admin := range adminGroups {
			if g == admin {
				return true
			}
		}
	}
	return false
}

// persistUsers writes the current in-memory Users list back to the
// Kubernetes Secret backing it (see cmd/kairon-ui/main.go's
// UsersSecretNamespace/UsersSecretName wiring), so a password change
// survives a pod restart instead of silently reverting to whatever
// $KAIRON_UI_USERS_JSON was at startup. Returns an error the caller should
// treat as fatal to the request: an in-memory-only change that can't be
// persisted would silently vanish on the next restart, which is worse than
// refusing the request.
func (s *Server) persistUsers(ctx context.Context) error {
	if s.UsersSecretName == "" {
		return errPersistenceNotConfigured
	}
	s.usersMu.RLock()
	usersJSON, err := json.Marshal(s.Users)
	s.usersMu.RUnlock()
	if err != nil {
		return err
	}
	key := s.UsersSecretKey
	if key == "" {
		key = "users.json"
	}
	return s.Kube.PatchSecretStringData(ctx, s.UsersSecretNamespace, s.UsersSecretName, map[string]string{key: string(usersJSON)})
}

var errPersistenceNotConfigured = errors.New("runtime password changes are not persisted on this deployment (ui.auth.users is set via an externally-managed existingSecret)")

// loginAttemptState tracks failed logins for one requested username.
// Keyed on the raw username the caller supplied, not whether it resolves
// to a real account -- otherwise a nonexistent username never locking out
// would itself leak which usernames exist, the same anti-enumeration
// concern handleLogin's constant-time dummy-hash compare already guards.
type loginAttemptState struct {
	mu          sync.Mutex
	count       int
	lockedUntil time.Time
}

const (
	maxLoginAttempts = 5
	loginLockoutFor  = 5 * time.Minute
)

func (s *Server) loginState(username string) *loginAttemptState {
	v, _ := s.loginAttempts.LoadOrStore(username, &loginAttemptState{})
	return v.(*loginAttemptState)
}

// loginLockedFor returns how much longer username is locked out, or 0 if
// it may attempt a login now.
func (s *Server) loginLockedFor(username string) time.Duration {
	st := s.loginState(username)
	st.mu.Lock()
	defer st.mu.Unlock()
	if remaining := time.Until(st.lockedUntil); remaining > 0 {
		return remaining
	}
	return 0
}

// recordLoginResult clears a username's failure count on success, or
// increments it on failure -- locking the username out for
// loginLockoutFor once maxLoginAttempts consecutive failures accumulate.
// The failure *count* itself stays purely local to this replica (a
// distributed atomic counter is exactly the kind of machinery choosing a
// Kubernetes object over Redis deliberately avoids taking on) -- so
// maxLoginAttempts is reached by consecutive failures landing on ONE
// replica, not accumulated across all of them. Once that happens, though,
// the resulting lockedUntil IS mirrored into the shared ConfigMap (see
// sharedstate.go), so every other replica honors it within
// sharedStateSyncInterval -- and a success clears it everywhere, not just
// here. With SharedStateConfigMapName unset (the default), this behaves
// exactly as it always did on a single replica.
func (s *Server) recordLoginResult(ctx context.Context, username string, success bool) {
	st := s.loginState(username)
	st.mu.Lock()
	if success {
		st.count = 0
		st.lockedUntil = time.Time{}
		st.mu.Unlock()
		s.deleteSharedState(ctx, sharedLockoutKey(username))
		return
	}
	st.count++
	newlyLocked := false
	if st.count >= maxLoginAttempts {
		st.lockedUntil = time.Now().Add(loginLockoutFor)
		st.count = 0
		newlyLocked = true
	}
	lockedUntil := st.lockedUntil
	st.mu.Unlock()
	if newlyLocked {
		s.writeSharedState(ctx, sharedLockoutKey(username), sharedLockoutEntry{LockedUntil: lockedUntil})
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
		return strings.TrimPrefix(h, prefix)
	}
	return ""
}

// crdKinds is the number of CRDs deploy/crd.yaml ships (kairon.zyvor.dev and
// fleet.kairon.zyvor.dev); TestCRDKindsMatchBundle keeps it honest.
const crdKinds = 30

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleAuthConfig tells the frontend which login UI to render, without it
// having to guess from a 401: the two-step username/password form when
// Users is configured, the legacy raw-token box when only Token is, and an
// "SSO" option whenever OIDC is -- these are independent, not mutually
// exclusive, so a deployment can offer more than one at once.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"loginEnabled": s.userCount() > 0,
		"tokenEnabled": s.Token != "",
		"ssoEnabled":   s.OIDC != nil,
	}
	// Non-sensitive product facts for the sign-in page's stat band: nothing
	// tenant-scoped (no machine or node counts) on an unauthenticated route.
	resp["stats"] = map[string]any{
		"version":     s.Version,
		"apiRoutes":   s.apiRoutes,
		"crdKinds":    crdKinds,
		"hypervisors": 4,
	}
	if s.LoginHint != "" && s.userCount() > 0 {
		resp["loginHint"] = s.LoginHint
	}
	if s.OIDC != nil {
		resp["ssoLoginURL"] = "/api/v1/auth/oidc/login"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.userCount() == 0 {
		writeError(w, http.StatusNotImplemented, "username/password login is not configured on this server")
		return
	}
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if remaining := s.loginLockedFor(req.Username); remaining > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(remaining.Seconds())+1))
		if s.Log != nil {
			s.Log.Warn("uiapi login rate-limited", "username", req.Username, "remoteAddr", r.RemoteAddr, "retryAfter", remaining.Round(time.Second).String())
		}
		writeError(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
		return
	}
	user, found := s.findUser(req.Username)
	hash := dummyHash
	if found {
		hash = []byte(user.PasswordHash)
	}
	// Always run the bcrypt compare, even for an unknown username, so a
	// "no such user" response takes the same time as a "wrong password"
	// one -- avoids leaking valid usernames via response timing.
	credentialsOK := bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) == nil
	s.recordLoginResult(r.Context(), req.Username, found && credentialsOK)
	if !found || !credentialsOK {
		if s.Log != nil {
			s.Log.Warn("uiapi login failed", "username", req.Username, "remoteAddr", r.RemoteAddr)
		}
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token, expires, err := signSession(s.SessionSecret, user.Username, nil, sessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue session")
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi login", "username", user.Username, "remoteAddr", r.RemoteAddr)
	}
	resp := map[string]any{"token": token, "username": user.Username, "isAdmin": user.IsAdmin, "expiresAt": expires}
	if s.DefaultPassword != "" && req.Password == s.DefaultPassword {
		resp["defaultPassword"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := bearerToken(r); tok != "" {
		if _, _, expires, _, err := verifySession(s.SessionSecret, tok); err == nil {
			s.revokeSession(r.Context(), tok, expires)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

const minPasswordLength = 8

type setPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handleSetOwnPassword implements POST /api/v1/auth/password: an
// authenticated operator changes their own password, re-proving their
// current one first (a "confirm password" step for a sensitive action,
// same idea as GitHub/GitLab's own account settings). Only meaningful for
// session-token auth -- the legacy shared token has no per-user identity
// to attach a new password to.
func (s *Server) handleSetOwnPassword(w http.ResponseWriter, r *http.Request) {
	username := usernameFromContext(r.Context())
	if username == "" {
		writeError(w, http.StatusBadRequest, "changing a password requires a per-operator session login, not the shared token")
		return
	}
	if s.UsersSecretName == "" {
		writeError(w, http.StatusNotImplemented, errPersistenceNotConfigured.Error())
		return
	}
	var req setPasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.NewPassword) < minPasswordLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("new password must be at least %d characters", minPasswordLength))
		return
	}
	user, found := s.findUser(username)
	if !found || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.CurrentPassword)) != nil {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if err := s.setPasswordHash(r.Context(), username, req.NewPassword); err != nil {
		if s.Log != nil {
			s.Log.Error("uiapi password change failed", "username", username, "error", err)
		}
		writeError(w, http.StatusInternalServerError, "failed to change password: "+err.Error())
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi password changed", "username", username, "remoteAddr", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

type resetPasswordRequest struct {
	NewPassword string `json:"newPassword"`
}

// handleResetPassword implements POST /api/v1/users/{username}/password:
// an admin sets another operator's password without needing to hand-craft
// a bcrypt hash and redeploy. Immediately invalidates that operator's
// outstanding sessions (see Server.passwordChangedAt) -- resetting
// someone's password is often exactly because their account may be
// compromised, so the reset should also end any session they (or an
// attacker) currently hold.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	caller := usernameFromContext(r.Context())
	if caller != "" {
		// Session-token caller: must be a real admin account (static or,
		// now, OIDC-group-derived -- see isAdminIdentity). A legacy
		// shared-token caller (caller == "") falls through -- that token
		// is already root-equivalent for every other route (see
		// withAuth), so it's treated as admin here too rather than
		// introducing a second, inconsistent authorization tier.
		if !s.isAdminIdentity(r.Context(), caller) {
			writeError(w, http.StatusForbidden, "only an admin account may reset another operator's password")
			return
		}
	}
	if s.UsersSecretName == "" {
		writeError(w, http.StatusNotImplemented, errPersistenceNotConfigured.Error())
		return
	}
	target := r.PathValue("username")
	if _, found := s.findUser(target); !found {
		writeError(w, http.StatusNotFound, "no such user")
		return
	}
	var req resetPasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.NewPassword) < minPasswordLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("new password must be at least %d characters", minPasswordLength))
		return
	}
	if err := s.setPasswordHash(r.Context(), target, req.NewPassword); err != nil {
		if s.Log != nil {
			s.Log.Error("uiapi password reset failed", "username", target, "error", err)
		}
		writeError(w, http.StatusInternalServerError, "failed to reset password: "+err.Error())
		return
	}
	changedAt := time.Now()
	s.passwordChangedAt.Store(target, changedAt)
	s.writeSharedState(r.Context(), sharedPasswordChangeKey(target), sharedPasswordChangeEntry{ChangedAt: changedAt})
	if s.Log != nil {
		s.Log.Info("uiapi password reset", "username", target, "resetBy", caller, "remoteAddr", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

// setPasswordHash bcrypt-hashes newPassword, updates the in-memory Users
// entry for username under usersMu, and persists the whole list back to
// the backing Secret (persistUsers) -- rolling the in-memory change back
// if persistence fails, so a restart can never silently lose it.
func (s *Server) setPasswordHash(ctx context.Context, username, newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.usersMu.Lock()
	var previous string
	idx := -1
	for i, u := range s.Users {
		if u.Username == username {
			idx = i
			previous = u.PasswordHash
			break
		}
	}
	if idx < 0 {
		s.usersMu.Unlock()
		return fmt.Errorf("user %q not found", username)
	}
	s.Users[idx].PasswordHash = string(hash)
	s.usersMu.Unlock()

	if err := s.persistUsers(ctx); err != nil {
		s.usersMu.Lock()
		s.Users[idx].PasswordHash = previous
		s.usersMu.Unlock()
		return err
	}
	return nil
}
