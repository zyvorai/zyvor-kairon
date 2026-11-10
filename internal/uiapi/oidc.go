// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCAuth holds everything kairon-ui needs to run an OpenID Connect
// Authorization Code + PKCE flow against one external identity provider.
// Resolved once at process startup (see cmd/kairon-ui/main.go) -- provider
// discovery is a network call to the issuer's own
// /.well-known/openid-configuration, so a misconfigured issuer fails the
// process at startup, not silently on the first login attempt.
//
// This is the one place in Kairon that breaks the project's Go-stdlib-only
// design guarantee (see README.md's "Why Kairon exists"): real JWT/JWK
// verification (golang.org/x/oauth2, github.com/coreos/go-oidc/v3) is not
// something this codebase hand-rolls. That tradeoff was made deliberately
// and explicitly for this one feature -- see docs/guides/kairon-ui-oidc.md.
type OIDCAuth struct {
	OAuth2   oauth2.Config
	Verifier *oidc.IDTokenVerifier
	// UsernameClaim names the ID token claim kairon-ui reads as the
	// session's username (typically "email") -- the same username space
	// ui.auth.users already occupies, so audit log attribution stays
	// consistent regardless of login method. Kairon has no separate
	// identity/account object for an OIDC-authenticated operator: a
	// session token is all there ever is, and it's issued via signSession
	// exactly like a password login's -- withAuth's verification path
	// never had to change for this.
	UsernameClaim string
	// GroupsClaim names the ID token claim carrying the caller's IdP
	// group membership (typically "groups") -- read into the session
	// token (sessionPayload.Groups) at callback time. Only consulted when
	// AdminGroups is also non-empty; empty (the default, "") means the
	// claims map is never even inspected for it, so an IdP that never
	// returns a groups claim at all costs nothing extra.
	GroupsClaim string
	// AdminGroups, when non-empty, are the IdP group names that map to
	// admin capability -- see Server.isAdminIdentity. Empty (the default)
	// means every OIDC session stays a normal, non-admin operator
	// identity regardless of what groups an IdP reports, exactly Kairon's
	// behavior before this field existed.
	AdminGroups []string
	// NamespaceGroups maps an IdP group name to the Kubernetes namespaces
	// a member of that group may act on, consulted only when
	// Server.NamespaceScopingEnabled is true (see
	// Server.authorizedForNamespace) -- mirrors AdminGroups' own shape
	// exactly: re-checked fresh on every request against current config
	// and the session's own recorded groups (never cached on the token),
	// so a config change takes effect on a caller's very next request
	// without them needing to log in again. Empty (the default) means an
	// OIDC session contributes no namespaces of its own -- only a static
	// ui.auth.users[] account's own User.Namespaces, or admin status,
	// grants anything once scoping is on.
	NamespaceGroups map[string][]string
}

// oidcStateTTL bounds how long an operator has to complete the redirect
// round trip to their identity provider and back before kairon-ui refuses
// the callback -- generous enough for a real login prompt, short enough
// that a leaked/logged state value stops being useful quickly.
const oidcStateTTL = 10 * time.Minute

// oidcStatePayload is round-tripped through the identity provider inside
// the OAuth2 "state" parameter -- kairon-ui keeps no server-side record of
// an in-progress login, the same "no server-side session store" design
// sessionPayload already uses (see auth.go). Signed with a key *derived*
// from SessionSecret (see oidcStateKey), never SessionSecret itself, so a
// state value can never be replayed as a session token or vice versa.
type oidcStatePayload struct {
	CodeVerifier string `json:"cv"`
	Nonce        string `json:"n"`
	Expires      int64  `json:"exp"`
}

func (s *Server) oidcStateKey() []byte {
	mac := hmac.New(sha256.New, s.SessionSecret)
	mac.Write([]byte("kairon-ui-oidc-state-v1"))
	return mac.Sum(nil)
}

func (s *Server) signOIDCState(codeVerifier, nonce string) (string, error) {
	payload, err := json.Marshal(oidcStatePayload{CodeVerifier: codeVerifier, Nonce: nonce, Expires: time.Now().Add(oidcStateTTL).Unix()})
	if err != nil {
		return "", err
	}
	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.oidcStateKey())
	mac.Write([]byte(encPayload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encPayload + "." + sig, nil
}

func (s *Server) verifyOIDCState(state string) (oidcStatePayload, bool) {
	encPayload, sig, found := strings.Cut(state, ".")
	if !found {
		return oidcStatePayload{}, false
	}
	mac := hmac.New(sha256.New, s.oidcStateKey())
	mac.Write([]byte(encPayload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(sig) != len(want) || !hmac.Equal([]byte(sig), []byte(want)) {
		return oidcStatePayload{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return oidcStatePayload{}, false
	}
	var p oidcStatePayload
	if json.Unmarshal(raw, &p) != nil {
		return oidcStatePayload{}, false
	}
	if time.Now().After(time.Unix(p.Expires, 0)) {
		return oidcStatePayload{}, false
	}
	return p, true
}

func randomNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// handleOIDCLogin redirects the browser to the configured identity
// provider's authorization endpoint, beginning an Authorization Code +
// PKCE flow. Unauthenticated by necessity -- this IS how a browser
// authenticates in the first place.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeError(w, http.StatusNotImplemented, "OIDC/SSO is not configured on this server")
		return
	}
	verifier := oauth2.GenerateVerifier()
	nonce, err := randomNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start OIDC login")
		return
	}
	state, err := s.signOIDCState(verifier, nonce)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start OIDC login")
		return
	}
	// Bind this flow to the browser that started it. Without this the callback
	// accepts any validly signed state, so an attacker could start a flow with
	// their own IdP account and hand the victim the callback URL ("login CSRF":
	// the victim ends up signed in as the attacker). SameSite=Lax still sends the
	// cookie on the IdP's top-level redirect back to us.
	http.SetCookie(w, &http.Cookie{
		Name: oidcBindCookie, Value: nonce, Path: "/api/v1/auth/oidc/",
		MaxAge: int(oidcStateTTL.Seconds()), HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	authURL := s.OIDC.OAuth2.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// oidcCallbackPath is the SPA route the browser lands on after this
// completes, whether it succeeded or failed -- see
// web/src/pages/OIDCCallback.tsx, which reads the outcome from the URL
// *fragment*, never the query string and never something logged as part
// of this redirect's own access log line: a fragment is never sent to any
// server on a subsequent request, the same reason implicit-flow redirects
// historically used one for a raw token.
const oidcCallbackPath = "/oidc/callback"

// handleOIDCCallback completes the Authorization Code + PKCE flow: it
// verifies state, exchanges the code, verifies the returned ID token
// (signature, issuer, audience, expiry, and nonce), and -- on success --
// issues a normal kairon-ui session token via signSession, exactly the
// same primitive POST /api/v1/auth/login already uses. An
// OIDC-authenticated username is deliberately never treated as an admin
// account -- that stays exclusively a ui.auth.users[].admin: true
// capability, since there's nothing meaningful for an "OIDC admin" to
// reset via POST /api/v1/users/{username}/password: an OIDC identity has
// no PasswordHash in Kairon to begin with. See
// docs/guides/kairon-ui-oidc.md.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeError(w, http.StatusNotImplemented, "OIDC/SSO is not configured on this server")
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		s.redirectOIDCError(w, r, errParam)
		return
	}
	state, ok := s.verifyOIDCState(r.URL.Query().Get("state"))
	if !ok {
		s.redirectOIDCError(w, r, "invalid or expired login attempt, please try again")
		return
	}
	// The state must belong to the browser presenting it (see handleOIDCLogin).
	if c, err := r.Cookie(oidcBindCookie); err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state.Nonce)) != 1 {
		s.redirectOIDCError(w, r, "invalid or expired login attempt, please try again")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oidcBindCookie, Value: "", Path: "/api/v1/auth/oidc/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
	token, err := s.OIDC.OAuth2.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(state.CodeVerifier))
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi oidc token exchange failed", "error", err)
		}
		s.redirectOIDCError(w, r, "sign-in failed")
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		s.redirectOIDCError(w, r, "identity provider did not return an ID token")
		return
	}
	idToken, err := s.OIDC.Verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("uiapi oidc id token verification failed", "error", err)
		}
		s.redirectOIDCError(w, r, "sign-in failed")
		return
	}
	if idToken.Nonce != state.Nonce {
		if s.Log != nil {
			s.Log.Warn("uiapi oidc nonce mismatch")
		}
		s.redirectOIDCError(w, r, "sign-in failed")
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		s.redirectOIDCError(w, r, "sign-in failed")
		return
	}
	username, _ := claims[s.OIDC.UsernameClaim].(string)
	if username == "" {
		s.redirectOIDCError(w, r, fmt.Sprintf("identity provider did not return a %q claim", s.OIDC.UsernameClaim))
		return
	}
	// An unverified email must not become an identity: anyone can set an
	// unverified address at many IdPs.
	if s.OIDC.UsernameClaim == "email" {
		if verified, present := claims["email_verified"].(bool); present && !verified {
			s.redirectOIDCError(w, r, "the identity provider reports this email address as unverified")
			return
		}
	}
	var groups []string
	// Only bother decoding the groups claim at all when something would
	// actually consult it -- AdminGroups (isAdminIdentity) or
	// NamespaceGroups (Server.authorizedForNamespace); an IdP that never
	// returns a groups claim, or a deployment using neither feature, costs
	// nothing extra.
	if s.OIDC.GroupsClaim != "" && (len(s.OIDC.AdminGroups) > 0 || len(s.OIDC.NamespaceGroups) > 0) {
		groups = stringClaimSlice(claims[s.OIDC.GroupsClaim])
	}
	// The session subject is namespaced so an IdP-supplied name can never equal a
	// local Users entry (and inherit its admin role or namespaces).
	sessionToken, expires, err := signSession(s.SessionSecret, oidcSubjectPrefix+username, groups, sessionTTL)
	if err != nil {
		s.redirectOIDCError(w, r, "sign-in failed")
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi oidc login", "username", username, "remoteAddr", r.RemoteAddr)
	}
	isAdmin := groupsContainAdmin(groups, s.OIDC.AdminGroups)
	values := url.Values{
		"token": {sessionToken}, "username": {username},
		"isAdmin": {strconv.FormatBool(isAdmin)}, "expiresAt": {expires.Format(time.RFC3339)},
	}
	http.Redirect(w, r, oidcCallbackPath+"#"+values.Encode(), http.StatusFound)
}

func (s *Server) redirectOIDCError(w http.ResponseWriter, r *http.Request, message string) {
	values := url.Values{"error": {message}}
	http.Redirect(w, r, oidcCallbackPath+"#"+values.Encode(), http.StatusFound)
}

// oidcSubjectPrefix namespaces OIDC session subjects away from local users.
const oidcSubjectPrefix = "oidc:"

// stringClaimSlice extracts a []string from an ID token claim's own
// decoded any value -- encoding/json always decodes a JSON array into
// []any regardless of its element types, so a real "groups": ["a", "b"]
// claim comes back as []any{"a", "b"}, not []string, and needs this
// per-element type assertion. A non-array claim, a missing claim (nil),
// or an array with non-string elements all safely produce an empty
// slice rather than a panic or a silently-wrong group name.
func stringClaimSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// oidcBindCookie holds the login nonce so the callback can only complete in the
// browser that started the flow.
const oidcBindCookie = "kairon_oidc_login"
