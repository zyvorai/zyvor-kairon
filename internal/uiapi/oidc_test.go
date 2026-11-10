// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	joseoidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"
)

const testOIDCClientID = "kairon-ui-test"

// fakeOIDCProvider is a minimal OpenID Connect identity provider double:
// just enough discovery/jwks/token surface for oidc.NewProvider and
// IDTokenVerifier.Verify to do real, unmodified signature/issuer/
// audience/expiry verification against it -- not a hand-waved stub. The
// authorization endpoint itself is never actually hit by these tests
// (nothing here drives a real browser redirect); the nonce
// handleOIDCLogin generates is instead read back out of the Location
// header handleOIDCLogin returns, then handed to withIDTokenClaims so the
// fake /token response can embed the exact nonce a real provider would
// have carried through from a real /authorize round trip.
type fakeOIDCProvider struct {
	key        *rsa.PrivateKey
	srv        *httptest.Server
	nextClaims map[string]any
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	p := &fakeOIDCProvider{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"userinfo_endpoint":                     p.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &p.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		idToken, err := p.signIDToken()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// withIDTokenClaims sets the private claims (on top of the standard
// iss/aud/exp/iat/sub the fake /token handler always sets) the NEXT
// /token response's ID token carries -- callers set the nonce read back
// from handleOIDCLogin's redirect, plus whatever username-claim value the
// test wants to exercise.
func (p *fakeOIDCProvider) withIDTokenClaims(claims map[string]any) {
	p.nextClaims = claims
}

func (p *fakeOIDCProvider) signIDToken() (string, error) {
	sig, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       &jose.JSONWebKey{Key: p.key, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	std := jwt.Claims{
		Issuer:   p.srv.URL,
		Subject:  "test-subject",
		Audience: jwt.Audience{testOIDCClientID},
		Expiry:   jwt.NewNumericDate(time.Now().Add(5 * time.Minute)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}
	return jwt.Signed(sig).Claims(std).Claims(p.nextClaims).Serialize()
}

func newTestOIDCAuth(t *testing.T, p *fakeOIDCProvider, usernameClaim string) *OIDCAuth {
	t.Helper()
	ctx := context.Background()
	provider, err := joseoidc.NewProvider(ctx, p.srv.URL)
	if err != nil {
		t.Fatalf("oidc.NewProvider: %v", err)
	}
	return &OIDCAuth{
		OAuth2: oauth2.Config{
			ClientID:    testOIDCClientID,
			Endpoint:    provider.Endpoint(),
			RedirectURL: "http://kairon-ui.example/api/v1/auth/oidc/callback",
			Scopes:      []string{"openid", "email"},
		},
		Verifier:      provider.Verifier(&joseoidc.Config{ClientID: testOIDCClientID}),
		UsernameClaim: usernameClaim,
	}
}

func TestHandleOIDCLoginRedirectsToAuthorizationEndpoint(t *testing.T) {
	p := newFakeOIDCProvider(t)
	s := &Server{OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret")}
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if !strings.HasPrefix(loc.String(), p.srv.URL+"/authorize") {
		t.Fatalf("expected a redirect to the provider's authorization endpoint, got %s", loc.String())
	}
	q := loc.Query()
	for _, param := range []string{"state", "code_challenge", "nonce", "client_id", "redirect_uri"} {
		if q.Get(param) == "" {
			t.Fatalf("expected %q in the authorization URL, got %s", param, loc.String())
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("expected PKCE S256, got code_challenge_method=%q", q.Get("code_challenge_method"))
	}
}

func TestHandleOIDCLoginRefusesWhenUnconfigured(t *testing.T) {
	s := &Server{}
	rr := doJSON(t, s.Handler(), http.MethodGet, "/api/v1/auth/oidc/login", "", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rr.Code)
	}
}

// oidcLoginState drives handleOIDCLogin exactly once and returns the
// state/nonce pair it generated, so a test can then hand the matching
// nonce to the fake provider before completing the callback.
func oidcLoginState(t *testing.T, h http.Handler) (state, nonce string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 from oidc/login, got %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return loc.Query().Get("state"), loc.Query().Get("nonce")
}

func TestOIDCLoginCallbackRoundTripIssuesASession(t *testing.T) {
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := &Server{Kube: mustKubeClientWithHandler(t, fk.handler()), OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret"), Log: slog.New(slog.DiscardHandler)}
	h := s.Handler()

	state, nonce := oidcLoginState(t, h)
	p.withIDTokenClaims(map[string]any{"nonce": nonce, "email": "alice@example.com"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=test-code&state="+url.QueryEscape(state), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, oidcCallbackPath+"#") {
		t.Fatalf("expected a redirect to %s#..., got %s", oidcCallbackPath, loc)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(loc, oidcCallbackPath+"#"))
	if err != nil {
		t.Fatalf("parse callback fragment: %v", err)
	}
	if frag.Get("error") != "" {
		t.Fatalf("expected no error, got %q", frag.Get("error"))
	}
	if frag.Get("username") != "alice@example.com" {
		t.Fatalf("expected username alice@example.com, got %q", frag.Get("username"))
	}
	token := frag.Get("token")
	if token == "" {
		t.Fatal("expected a non-empty session token")
	}
	username, _, _, _, err := verifySession(s.SessionSecret, token)
	if err != nil {
		t.Fatalf("expected the issued token to be a valid kairon-ui session: %v", err)
	}
	if username != oidcSubjectPrefix+"alice@example.com" {
		t.Fatalf("expected namespaced session subject %q, got %q", oidcSubjectPrefix+"alice@example.com", username)
	}

	// The issued session works exactly like a password-login session on
	// every other authenticated route -- withAuth never had to change.
	rr2 := doJSON(t, h, http.MethodGet, "/api/v1/machines", token, nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected the OIDC-issued session to authenticate normally, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

func TestOIDCCallbackRejectsTamperedState(t *testing.T) {
	p := newFakeOIDCProvider(t)
	s := &Server{OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret"), Log: slog.New(slog.DiscardHandler)}
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=test-code&state=not-a-real-state", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rr.Code)
	}
	frag, _ := url.ParseQuery(strings.TrimPrefix(rr.Header().Get("Location"), oidcCallbackPath+"#"))
	if frag.Get("error") == "" {
		t.Fatal("expected an error in the callback fragment for a tampered state")
	}
}

func TestOIDCCallbackRejectsNonceMismatch(t *testing.T) {
	p := newFakeOIDCProvider(t)
	s := &Server{OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret"), Log: slog.New(slog.DiscardHandler)}
	h := s.Handler()

	state, _ := oidcLoginState(t, h)
	// Deliberately embed a DIFFERENT nonce than the one handleOIDCLogin
	// generated and signed into state -- simulates an attacker replaying
	// an ID token from an unrelated login attempt.
	p.withIDTokenClaims(map[string]any{"nonce": "some-other-nonce", "email": "alice@example.com"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=test-code&state="+url.QueryEscape(state), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	frag, _ := url.ParseQuery(strings.TrimPrefix(rr.Header().Get("Location"), oidcCallbackPath+"#"))
	if frag.Get("error") == "" {
		t.Fatal("expected an error in the callback fragment for a nonce mismatch")
	}
	if frag.Get("token") != "" {
		t.Fatal("expected no session token to be issued for a nonce mismatch")
	}
}

func TestOIDCCallbackRejectsMissingUsernameClaim(t *testing.T) {
	p := newFakeOIDCProvider(t)
	s := &Server{OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret"), Log: slog.New(slog.DiscardHandler)}
	h := s.Handler()

	state, nonce := oidcLoginState(t, h)
	p.withIDTokenClaims(map[string]any{"nonce": nonce}) // no "email" claim

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=test-code&state="+url.QueryEscape(state), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	frag, _ := url.ParseQuery(strings.TrimPrefix(rr.Header().Get("Location"), oidcCallbackPath+"#"))
	if frag.Get("error") == "" {
		t.Fatal("expected an error in the callback fragment when the username claim is missing")
	}
}

func TestOIDCCallbackPropagatesUpstreamError(t *testing.T) {
	s := &Server{OIDC: &OIDCAuth{}, SessionSecret: []byte("test-session-secret")}
	rr := doJSON(t, s.Handler(), http.MethodGet, "/api/v1/auth/oidc/callback?error=access_denied", "", nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rr.Code)
	}
	frag, _ := url.ParseQuery(strings.TrimPrefix(rr.Header().Get("Location"), oidcCallbackPath+"#"))
	if frag.Get("error") != "access_denied" {
		t.Fatalf("expected the upstream error to be forwarded, got %q", frag.Get("error"))
	}
}

func TestHandleAuthConfigReportsSSO(t *testing.T) {
	p := newFakeOIDCProvider(t)
	s := &Server{OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret")}
	rr := doJSON(t, s.Handler(), http.MethodGet, "/api/v1/auth/config", "", nil)
	var cfg struct {
		SSOEnabled  bool   `json:"ssoEnabled"`
		SSOLoginURL string `json:"ssoLoginURL"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.SSOEnabled || cfg.SSOLoginURL == "" {
		t.Fatalf("expected ssoEnabled=true and a non-empty ssoLoginURL, got %+v", cfg)
	}
}

func TestWithAuthAcceptsOIDCSessionWithNoUsersConfigured(t *testing.T) {
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := &Server{Kube: mustKubeClientWithHandler(t, fk.handler()), OIDC: newTestOIDCAuth(t, p, "email"), SessionSecret: []byte("test-session-secret"), Log: slog.New(slog.DiscardHandler)}
	h := s.Handler()

	// No ui.auth.users, no ui.token -- an OIDC-only deployment must still
	// require a valid session, not fall through to the "nothing
	// configured, wide open" dev-mode branch.
	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines", "", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected an OIDC-only deployment to require auth, got %d", rr.Code)
	}

	state, nonce := oidcLoginState(t, h)
	p.withIDTokenClaims(map[string]any{"nonce": nonce, "email": "bob@example.com"})
	callbackRR := httptest.NewRecorder()
	h.ServeHTTP(callbackRR, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=c&state="+url.QueryEscape(state), nil))
	frag, _ := url.ParseQuery(strings.TrimPrefix(callbackRR.Header().Get("Location"), oidcCallbackPath+"#"))
	token := frag.Get("token")
	if token == "" {
		t.Fatalf("expected a session token, callback redirected to %s", callbackRR.Header().Get("Location"))
	}

	rr2 := doJSON(t, h, http.MethodGet, "/api/v1/machines", token, nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected the OIDC session to authenticate against an OIDC-only server, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

func newTestOIDCAuthWithAdminGroups(t *testing.T, p *fakeOIDCProvider, groupsClaim string, adminGroups []string) *OIDCAuth {
	t.Helper()
	auth := newTestOIDCAuth(t, p, "email")
	auth.GroupsClaim = groupsClaim
	auth.AdminGroups = adminGroups
	return auth
}

// oidcLogin drives a full login/callback round trip and returns the
// issued session token plus the callback's own isAdmin fragment value --
// shared by every admin-group test below so each one only has to state
// its own claims/config, not repeat the whole OIDC dance.
func oidcLogin(t *testing.T, h http.Handler, p *fakeOIDCProvider, claims map[string]any) (token string, isAdmin bool) {
	t.Helper()
	state, nonce := oidcLoginState(t, h)
	claims["nonce"] = nonce
	p.withIDTokenClaims(claims)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=c&state="+url.QueryEscape(state), nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	frag, err := url.ParseQuery(strings.TrimPrefix(loc, oidcCallbackPath+"#"))
	if err != nil {
		t.Fatalf("parse callback fragment: %v", err)
	}
	if frag.Get("error") != "" {
		t.Fatalf("expected no error, got %q", frag.Get("error"))
	}
	token = frag.Get("token")
	if token == "" {
		t.Fatal("expected a non-empty session token")
	}
	return token, frag.Get("isAdmin") == "true"
}

func TestOIDCSessionWithMatchingAdminGroupIsGrantedAdmin(t *testing.T) {
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := &Server{
		Kube:          mustKubeClientWithHandler(t, fk.handler()),
		OIDC:          newTestOIDCAuthWithAdminGroups(t, p, "groups", []string{"kairon-admins"}),
		SessionSecret: []byte("test-session-secret"),
		Log:           slog.New(slog.DiscardHandler),
	}
	h := s.Handler()

	token, isAdmin := oidcLogin(t, h, p, map[string]any{
		"email": "alice@example.com", "groups": []string{"engineering", "kairon-admins"},
	})
	if !isAdmin {
		t.Fatal("expected the callback's own isAdmin fragment to be true for a member of an admin group")
	}

	// The session token itself carries the groups, not a baked-in bool --
	// verifySession round-trips them so isAdminIdentity can re-derive
	// admin-ness fresh on every request (see its own doc comment).
	_, groups, _, _, err := verifySession(s.SessionSecret, token)
	if err != nil {
		t.Fatalf("verifySession: %v", err)
	}
	if !groupsContainAdmin(groups, s.OIDC.AdminGroups) {
		t.Fatalf("expected the session's own recorded groups %v to contain an admin group", groups)
	}
}

func TestOIDCSessionWithoutMatchingAdminGroupStaysNonAdmin(t *testing.T) {
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := &Server{
		Kube:          mustKubeClientWithHandler(t, fk.handler()),
		OIDC:          newTestOIDCAuthWithAdminGroups(t, p, "groups", []string{"kairon-admins"}),
		SessionSecret: []byte("test-session-secret"),
		Log:           slog.New(slog.DiscardHandler),
	}
	h := s.Handler()

	_, isAdmin := oidcLogin(t, h, p, map[string]any{
		"email": "bob@example.com", "groups": []string{"engineering", "sales"},
	})
	if isAdmin {
		t.Fatal("expected a caller with no matching admin group to stay non-admin")
	}
}

func TestOIDCGroupsClaimIsIgnoredWhenNoAdminGroupsConfigured(t *testing.T) {
	// AdminGroups unset (the default) must mean OIDC sessions stay
	// non-admin regardless of what groups an IdP reports -- exactly
	// Kairon's behavior before this feature existed. Also proves the
	// groups claim is never even inspected in this case (a real IdP
	// returning a "groups" claim with no admin config set must not
	// accidentally grant admin via some other match).
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := &Server{
		Kube:          mustKubeClientWithHandler(t, fk.handler()),
		OIDC:          newTestOIDCAuth(t, p, "email"), // GroupsClaim/AdminGroups both unset
		SessionSecret: []byte("test-session-secret"),
		Log:           slog.New(slog.DiscardHandler),
	}
	h := s.Handler()

	_, isAdmin := oidcLogin(t, h, p, map[string]any{
		"email": "carol@example.com", "groups": []string{"kairon-admins"},
	})
	if isAdmin {
		t.Fatal("expected AdminGroups unset to keep every OIDC session non-admin")
	}
}

func TestOIDCAdminGroupConfigChangeAffectsAnAlreadyIssuedSessionImmediately(t *testing.T) {
	// isAdminIdentity re-checks AdminGroups fresh against current config
	// on every call rather than baking a computed admin bit into the
	// token -- proves an operator changing ui.oidc.adminGroups (and
	// restarting/reconfiguring kairon-ui) takes effect for an
	// already-issued session's very next request, the same freshness
	// property findUser's own live s.Users lookup already has for the
	// static admin path, without the caller needing to log in again.
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := &Server{
		Kube:          mustKubeClientWithHandler(t, fk.handler()),
		OIDC:          newTestOIDCAuthWithAdminGroups(t, p, "groups", []string{"kairon-admins"}),
		SessionSecret: []byte("test-session-secret"),
		Log:           slog.New(slog.DiscardHandler),
	}
	h := s.Handler()

	token, isAdmin := oidcLogin(t, h, p, map[string]any{
		"email": "dave@example.com", "groups": []string{"kairon-admins"},
	})
	if !isAdmin {
		t.Fatal("expected dave to be admin at login time")
	}

	_, groups, _, _, err := verifySession(s.SessionSecret, token)
	if err != nil {
		t.Fatalf("verifySession: %v", err)
	}
	if !groupsContainAdmin(groups, s.OIDC.AdminGroups) {
		t.Fatal("expected dave's session to still count as admin under the original config")
	}

	// An operator removes "kairon-admins" from ui.oidc.adminGroups --
	// dave's already-issued session must lose admin capability on its
	// very next check, without dave logging in again.
	s.OIDC.AdminGroups = []string{"platform-team"}
	if groupsContainAdmin(groups, s.OIDC.AdminGroups) {
		t.Fatal("expected dave's session to lose admin capability once its group is removed from config")
	}
}

func TestOIDCAdminGroupCallerCanResetAnotherOperatorsPassword(t *testing.T) {
	// End-to-end proof the wiring actually reaches a real admin-gated
	// route, not just isAdminIdentity in isolation -- mirrors
	// password_test.go's own TestResetPasswordByAdminRevokesTargetSessions,
	// but the admin caller here is an OIDC session, not a static
	// ui.auth.users[].admin account.
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{
		{Username: "bob", PasswordHash: hashFor(t, "bobs-pw")}, // static account, target of the reset
	}, "test-session-secret")
	s.OIDC = newTestOIDCAuthWithAdminGroups(t, p, "groups", []string{"kairon-admins"})
	s.Log = slog.New(slog.DiscardHandler)
	h := s.Handler()

	adminToken, isAdmin := oidcLogin(t, h, p, map[string]any{
		"email": "admin@example.com", "groups": []string{"kairon-admins"},
	})
	if !isAdmin {
		t.Fatal("expected the OIDC caller to be admin")
	}

	rr := doJSON(t, h, http.MethodPost, "/api/v1/users/bob/password", adminToken, resetPasswordRequest{NewPassword: "new-bob-password"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := login(t, h, "bob", "new-bob-password"); rr.Code != http.StatusOK {
		t.Fatalf("bob's new password should work, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOIDCNonAdminGroupCallerCannotResetAnotherOperatorsPassword(t *testing.T) {
	p := newFakeOIDCProvider(t)
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{
		{Username: "bob", PasswordHash: hashFor(t, "bobs-pw")},
	}, "test-session-secret")
	s.OIDC = newTestOIDCAuthWithAdminGroups(t, p, "groups", []string{"kairon-admins"})
	s.Log = slog.New(slog.DiscardHandler)
	h := s.Handler()

	callerToken, isAdmin := oidcLogin(t, h, p, map[string]any{
		"email": "alice@example.com", "groups": []string{"engineering"},
	})
	if isAdmin {
		t.Fatal("expected alice to not be admin")
	}

	rr := doJSON(t, h, http.MethodPost, "/api/v1/users/bob/password", callerToken, resetPasswordRequest{NewPassword: "new-bob-password"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGroupsContainAdmin(t *testing.T) {
	cases := []struct {
		name        string
		groups      []string
		adminGroups []string
		want        bool
	}{
		{"empty groups", nil, []string{"admins"}, false},
		{"empty admin groups", []string{"admins"}, nil, false},
		{"no overlap", []string{"eng", "sales"}, []string{"admins"}, false},
		{"exact match", []string{"admins"}, []string{"admins"}, true},
		{"match among several", []string{"eng", "admins", "sales"}, []string{"platform", "admins"}, true},
	}
	for _, c := range cases {
		if got := groupsContainAdmin(c.groups, c.adminGroups); got != c.want {
			t.Errorf("%s: groupsContainAdmin(%v, %v) = %v, want %v", c.name, c.groups, c.adminGroups, got, c.want)
		}
	}
}

func TestStringClaimSlice(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"not an array", "just-a-string", nil},
		{"string array", []any{"a", "b"}, []string{"a", "b"}},
		{"mixed types drop non-strings", []any{"a", 1, "b", true}, []string{"a", "b"}},
		{"empty array", []any{}, []string{}},
	}
	for _, c := range cases {
		got := stringClaimSlice(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}
