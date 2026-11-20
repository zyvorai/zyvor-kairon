// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestSetOwnPasswordChangesCredentialsAndPersists(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, "test-session-secret")
	h := s.Handler()

	rr := login(t, h, "alice", "correct-horse")
	var out struct{ Token string }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)

	rr = doJSON(t, h, http.MethodPost, "/api/v1/auth/password", out.Token, setPasswordRequest{
		CurrentPassword: "correct-horse",
		NewPassword:     "new-password-123",
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Old password must stop working, new one must work.
	if rr := login(t, h, "alice", "correct-horse"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("old password should be rejected after change, got %d", rr.Code)
	}
	if rr := login(t, h, "alice", "new-password-123"); rr.Code != http.StatusOK {
		t.Fatalf("new password should work, got %d: %s", rr.Code, rr.Body.String())
	}

	// Persisted back into the Secret, not just in-memory.
	persisted, ok := fk.secrets["default/kairon-ui-users"]
	if !ok {
		t.Fatal("expected kairon-ui-users Secret to have been patched")
	}
	var persistedUsers []User
	if err := json.Unmarshal([]byte(persisted["users.json"]), &persistedUsers); err != nil {
		t.Fatalf("decode persisted users.json: %v", err)
	}
	if len(persistedUsers) != 1 || bcrypt.CompareHashAndPassword([]byte(persistedUsers[0].PasswordHash), []byte("new-password-123")) != nil {
		t.Fatalf("persisted users.json does not reflect the new password: %+v", persistedUsers)
	}
}

func TestSetOwnPasswordRejectsWrongCurrentPassword(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, "test-session-secret")
	h := s.Handler()
	rr := login(t, h, "alice", "correct-horse")
	var out struct{ Token string }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)

	rr = doJSON(t, h, http.MethodPost, "/api/v1/auth/password", out.Token, setPasswordRequest{
		CurrentPassword: "totally-wrong",
		NewPassword:     "new-password-123",
	})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if rr := login(t, h, "alice", "correct-horse"); rr.Code != http.StatusOK {
		t.Fatal("original password should still work after a rejected change")
	}
}

func TestSetOwnPasswordRejectsLegacyTokenCaller(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, "test-session-secret")
	s.Token = "legacy-shared-token"
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/auth/password", "legacy-shared-token", setPasswordRequest{
		CurrentPassword: "correct-horse",
		NewPassword:     "new-password-123",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no per-operator identity to attach a password change to), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetOwnPasswordRefusedWithoutPersistenceConfigured(t *testing.T) {
	// newUserTestServer (unlike newUserTestServerWithFake) leaves
	// UsersSecretName empty -- same as a deployment where
	// ui.auth.existingSecret is set, or a bare-metal install with no
	// Kubernetes Secret at all.
	s := newUserTestServer(t, []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, "test-session-secret")
	h := s.Handler()
	rr := login(t, h, "alice", "correct-horse")
	var out struct{ Token string }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)

	rr = doJSON(t, h, http.MethodPost, "/api/v1/auth/password", out.Token, setPasswordRequest{
		CurrentPassword: "correct-horse",
		NewPassword:     "new-password-123",
	})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := login(t, h, "alice", "correct-horse"); rr.Code != http.StatusOK {
		t.Fatal("password must be unchanged when persistence isn't configured")
	}
}

func TestResetPasswordByAdminRevokesTargetSessions(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{
		{Username: "admin", PasswordHash: hashFor(t, "admin-pw"), IsAdmin: true},
		{Username: "bob", PasswordHash: hashFor(t, "bobs-pw")},
	}, "test-session-secret")
	h := s.Handler()

	adminRR := login(t, h, "admin", "admin-pw")
	var adminOut struct{ Token string }
	_ = json.Unmarshal(adminRR.Body.Bytes(), &adminOut)

	bobRR := login(t, h, "bob", "bobs-pw")
	var bobOut struct{ Token string }
	_ = json.Unmarshal(bobRR.Body.Bytes(), &bobOut)
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines", bobOut.Token, nil); rr.Code != http.StatusOK {
		t.Fatalf("bob's session should work before reset, got %d", rr.Code)
	}

	rr := doJSON(t, h, http.MethodPost, "/api/v1/users/bob/password", adminOut.Token, resetPasswordRequest{NewPassword: "new-bob-password"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}

	// Bob's pre-reset session must now be rejected...
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines", bobOut.Token, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected bob's old session to be invalidated by the reset, got %d", rr.Code)
	}
	// ...and his old password must no longer work, only the new one.
	if rr := login(t, h, "bob", "bobs-pw"); rr.Code != http.StatusUnauthorized {
		t.Fatal("bob's old password should no longer work")
	}
	if rr := login(t, h, "bob", "new-bob-password"); rr.Code != http.StatusOK {
		t.Fatalf("bob's new password should work, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResetPasswordRejectsNonAdminCaller(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{
		{Username: "alice", PasswordHash: hashFor(t, "alice-pw")}, // not an admin
		{Username: "bob", PasswordHash: hashFor(t, "bobs-pw")},
	}, "test-session-secret")
	h := s.Handler()

	aliceRR := login(t, h, "alice", "alice-pw")
	var aliceOut struct{ Token string }
	_ = json.Unmarshal(aliceRR.Body.Bytes(), &aliceOut)

	rr := doJSON(t, h, http.MethodPost, "/api/v1/users/bob/password", aliceOut.Token, resetPasswordRequest{NewPassword: "new-bob-password"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResetPasswordAllowsLegacySharedToken(t *testing.T) {
	// The legacy shared token is already root-equivalent for every other
	// route (see withAuth); resetPassword treats it the same way rather
	// than introducing a second, inconsistent authorization tier.
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{{Username: "bob", PasswordHash: hashFor(t, "bobs-pw")}}, "test-session-secret")
	s.Token = "legacy-shared-token"
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/users/bob/password", "legacy-shared-token", resetPasswordRequest{NewPassword: "new-bob-password"})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResetPasswordRejectsUnknownUser(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{
		{Username: "admin", PasswordHash: hashFor(t, "admin-pw"), IsAdmin: true},
	}, "test-session-secret")
	h := s.Handler()
	rr := login(t, h, "admin", "admin-pw")
	var out struct{ Token string }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)

	rr = doJSON(t, h, http.MethodPost, "/api/v1/users/no-such-user/password", out.Token, resetPasswordRequest{NewPassword: "whatever123"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetOwnPasswordRejectsShortPassword(t *testing.T) {
	fk := newFakeKube()
	s := newUserTestServerWithFake(t, fk, []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, "test-session-secret")
	h := s.Handler()
	rr := login(t, h, "alice", "correct-horse")
	var out struct{ Token string }
	_ = json.Unmarshal(rr.Body.Bytes(), &out)

	rr = doJSON(t, h, http.MethodPost, "/api/v1/auth/password", out.Token, setPasswordRequest{
		CurrentPassword: "correct-horse",
		NewPassword:     "short",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a too-short password, got %d", rr.Code)
	}
}
