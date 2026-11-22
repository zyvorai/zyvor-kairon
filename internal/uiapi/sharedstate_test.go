// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const sharedStateConfigMapName = "kairon-ui-shared-state"

// fakeConfigMapStore is a minimal in-memory core/v1 ConfigMap double, just
// enough to exercise Client.GetConfigMap/PatchConfigMapData -- the same
// intent as uiapi_test.go's fakeKube, scoped to this one resource. PATCH
// decodes a nil value as a key deletion (RFC 7386 merge-patch semantics),
// mirroring what a real API server does for `application/merge-patch+json`.
type fakeConfigMapStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newFakeConfigMapStore() *fakeConfigMapStore {
	return &fakeConfigMapStore{data: map[string]string{}}
}

func (f *fakeConfigMapStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configmaps/"):
			data := make(map[string]string, len(f.data))
			for k, v := range f.data {
				data[k] = v
			}
			_ = json.NewEncoder(w).Encode(model.ConfigMap{Metadata: model.ObjectMeta{Name: sharedStateConfigMapName}, Data: data})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/configmaps/"):
			var patch struct {
				Data map[string]*string `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			for k, v := range patch.Data {
				if v == nil {
					delete(f.data, k)
				} else {
					f.data[k] = *v
				}
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

// fakeSecretStore is the same idea as fakeConfigMapStore, for
// Client.GetSecret/PatchSecretStringData -- exercising
// syncUsersFromSecret's read path, which uiapi_test.go's fakeKube doesn't
// need (it only ever writes).
type fakeSecretStore struct {
	mu   sync.Mutex
	data map[string]string
}

func (f *fakeSecretStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/secrets/"):
			data := make(map[string][]byte, len(f.data))
			for k, v := range f.data {
				data[k] = []byte(v)
			}
			_ = json.NewEncoder(w).Encode(model.Secret{Metadata: model.ObjectMeta{Name: "kairon-ui-users"}, Data: data})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/secrets/"):
			var patch struct {
				StringData map[string]string `json:"stringData"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			for k, v := range patch.StringData {
				f.data[k] = v
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func mustKubeClientWithHandler(t *testing.T, h http.Handler) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	return kc
}

func TestSharedStateWriteNoOpsWithoutConfigMapConfigured(t *testing.T) {
	s := &Server{Kube: mustKubeClientWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))}
	s.writeSharedState(context.Background(), "rev-whatever", sharedRevocationEntry{Expires: time.Now()})
	s.deleteSharedState(context.Background(), "rev-whatever")
}

func TestRevokeSessionPropagatesAcrossServerInstances(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	writer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	writer.revokeSession(ctx, "sometoken", time.Now().Add(time.Hour))

	reader := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	if reader.isSessionRevoked("sometoken") {
		t.Fatal("expected a token revoked on a different Server instance not to be visible before a sync")
	}
	reader.syncSharedConfigMap(ctx)
	if !reader.isSessionRevoked("sometoken") {
		t.Fatal("expected the revoked token to be visible on a different Server instance after a sync")
	}
}

func TestRecordLoginResultLockoutPropagatesAndClearsOnSuccess(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	writer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	for i := 0; i < maxLoginAttempts; i++ {
		writer.recordLoginResult(ctx, "bob", false)
	}

	reader := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	reader.syncSharedConfigMap(ctx)
	if remaining := reader.loginLockedFor("bob"); remaining <= 0 {
		t.Fatal("expected bob to be locked out on a different Server instance after a sync")
	}

	// A success anywhere clears the shared lockout, not just the local one.
	writer.recordLoginResult(ctx, "bob", true)
	reader.syncSharedConfigMap(ctx)
	// The reader's own local lockout (set by the earlier sync) only clears
	// once the shared key is gone AND its own local state is re-derived;
	// deleteSharedState best-effort removes the shared key, which the next
	// GET on the writer's side confirms directly.
	store.mu.Lock()
	_, stillLocked := store.data[sharedLockoutKey("bob")]
	store.mu.Unlock()
	if stillLocked {
		t.Fatal("expected a successful login to clear the shared lockout entry")
	}
}

func TestMergeLockoutNeverMovesBackward(t *testing.T) {
	s := &Server{}
	later := time.Now().Add(10 * time.Minute)
	earlier := time.Now().Add(time.Minute)
	s.mergeLockout("alice", later)
	s.mergeLockout("alice", earlier)
	if remaining := s.loginLockedFor("alice"); remaining < 9*time.Minute {
		t.Fatalf("expected the later lockedUntil to win over an earlier merge, got %s remaining", remaining)
	}
}

func TestSyncSharedConfigMapPrunesExpiredEntries(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	encoded, err := json.Marshal(sharedRevocationEntry{Expires: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	store.mu.Lock()
	store.data["rev-deadbeef"] = string(encoded)
	store.mu.Unlock()

	s := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	s.syncSharedConfigMap(ctx)

	store.mu.Lock()
	_, stillThere := store.data["rev-deadbeef"]
	store.mu.Unlock()
	if stillThere {
		t.Fatal("expected an already-expired shared-state entry to be pruned on sync")
	}
}

func TestConsumeConsoleTicketFallsBackToSharedConfigMap(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	issuer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	ticket := mustIssueConsoleTicket(t, issuer, ctx, "alice", "default", "vm1", "vnc")

	consumer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	username, namespace, name, _, ok := consumer.consumeConsoleTicket(ctx, ticket)
	if !ok {
		t.Fatal("expected a ticket minted on a different Server instance to be consumable via the shared ConfigMap")
	}
	if username != "alice" {
		t.Fatalf("expected the ticket to be bound to alice, got %q", username)
	}
	if namespace != "default" || name != "vm1" {
		t.Fatalf("expected the ticket to be bound to default/vm1, got %q/%q", namespace, name)
	}
	if _, _, _, _, ok := consumer.consumeConsoleTicket(ctx, ticket); ok {
		t.Fatal("expected the ticket to be rejected the second time, even across replicas")
	}
	if _, _, _, _, ok := issuer.consumeConsoleTicket(ctx, ticket); ok {
		t.Fatal("expected the ticket to be rejected on the issuing replica too, once consumed elsewhere")
	}
}

func TestSyncSharedConfigMapPreservesUnexpiredTicket(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	issuer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	ticket := mustIssueConsoleTicket(t, issuer, ctx, "alice", "default", "vm1", "vnc")

	// A periodic sync landing inside the ticket's own lifetime must not
	// prune it -- it isn't part of the recognized rev-/lock-/pwc- merge
	// categories, but that doesn't make it garbage.
	s := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	s.syncSharedConfigMap(ctx)

	consumer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	username, _, _, _, ok := consumer.consumeConsoleTicket(ctx, ticket)
	if !ok {
		t.Fatal("expected the ticket to still be consumable after an intervening periodic sync")
	}
	if username != "alice" {
		t.Fatalf("expected the ticket to be bound to alice, got %q", username)
	}
}

func TestSyncSharedConfigMapMergesPasswordChangedAt(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	changedAt := time.Now().Add(-time.Minute)
	writer := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	writer.writeSharedState(ctx, sharedPasswordChangeKey("alice"), sharedPasswordChangeEntry{ChangedAt: changedAt})

	reader := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}
	if reader.passwordChangedAfter("alice", changedAt.Add(-time.Hour)) {
		t.Fatal("expected no passwordChangedAt entry before a sync")
	}
	reader.syncSharedConfigMap(ctx)
	if !reader.passwordChangedAfter("alice", changedAt.Add(-time.Hour)) {
		t.Fatal("expected the shared password-change entry to be visible after a sync")
	}
	if reader.passwordChangedAfter("alice", changedAt.Add(time.Hour)) {
		t.Fatal("expected a session issued well after the reset not to be rejected")
	}
}

func TestRunSharedStateSyncNoOpsWhenUnconfigured(t *testing.T) {
	s := &Server{Kube: mustKubeClientWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))}
	// Returns immediately without ever touching Kube -- if this hangs, the
	// test's own timeout catches it.
	s.RunSharedStateSync(context.Background(), time.Millisecond)
}

func TestRunSharedStateSyncStopsOnContextCancel(t *testing.T) {
	store := newFakeConfigMapStore()
	kc := mustKubeClientWithHandler(t, store.handler())
	s := &Server{Kube: kc, SharedStateConfigMapName: sharedStateConfigMapName}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunSharedStateSync(ctx, time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected RunSharedStateSync to return once its context is cancelled")
	}
}

func TestSyncUsersFromSecretPicksUpChangeFromAnotherReplica(t *testing.T) {
	store := &fakeSecretStore{data: map[string]string{}}
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()

	writer := &Server{
		Kube: kc, Users: []User{{Username: "alice", PasswordHash: "old-hash"}},
		UsersSecretNamespace: "kairon-system", UsersSecretName: "kairon-ui-users", UsersSecretKey: "users.json",
	}
	if err := writer.persistUsers(ctx); err != nil {
		t.Fatalf("persistUsers: %v", err)
	}
	if err := writer.setPasswordHash(ctx, "alice", "a-new-password"); err != nil {
		t.Fatalf("setPasswordHash: %v", err)
	}

	reader := &Server{
		Kube: kc, Users: []User{{Username: "alice", PasswordHash: "old-hash"}},
		UsersSecretNamespace: "kairon-system", UsersSecretName: "kairon-ui-users", UsersSecretKey: "users.json",
	}
	reader.syncUsersFromSecret(ctx)

	u, found := reader.findUser("alice")
	if !found {
		t.Fatal("expected alice to still be found after syncing from the backing secret")
	}
	if u.PasswordHash == "old-hash" {
		t.Fatal("expected the reader's in-memory password hash to be refreshed from the backing secret")
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("a-new-password")) != nil {
		t.Fatal("expected the refreshed hash to verify the password set on a different Server instance")
	}
}

func TestUsernameFromSharedKeyRoundTrips(t *testing.T) {
	for _, username := range []string{"alice", "alice.bob", "has spaces", "üñïçødé"} {
		key := sharedLockoutKey(username)
		got, ok := usernameFromSharedKey(sharedStateLockoutPrefix, key)
		if !ok || got != username {
			t.Fatalf("username %q: round-trip got (%q, %v)", username, got, ok)
		}
	}
}
