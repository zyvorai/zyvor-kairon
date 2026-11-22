// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package nodeliveness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// fakeLeaseStore is a minimal in-memory coordination.k8s.io/v1 Lease
// store, just enough to exercise Client.GetLease/CreateLease/UpdateLease,
// including resourceVersion-based optimistic concurrency (409 on
// mismatch) so Renew's own conflict-handling paths are exercised against
// realistic API server behavior, not assumed. Keyed by lease name (unlike
// internal/leaderelection's own same-shaped test double, which only ever
// needs one lease name per test) since this package's own tests need to
// prove two different nodes' Leases are genuinely independent objects.
type fakeLeaseStore struct {
	mu     sync.Mutex
	leases map[string]*model.Lease
	rv     int
}

func (f *fakeLeaseStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.leases == nil {
			f.leases = map[string]*model.Lease{}
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/leases/"):
			l, ok := f.leases[name]
			if !ok {
				http.Error(w, `{"status":"Failure","reason":"NotFound"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(*l)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/leases"):
			var l model.Lease
			_ = json.NewDecoder(r.Body).Decode(&l)
			if _, exists := f.leases[l.Metadata.Name]; exists {
				http.Error(w, `{"status":"Failure","reason":"AlreadyExists"}`, http.StatusConflict)
				return
			}
			f.rv++
			l.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			f.leases[l.Metadata.Name] = &l
			_ = json.NewEncoder(w).Encode(l)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/leases/"):
			var l model.Lease
			_ = json.NewDecoder(r.Body).Decode(&l)
			current, ok := f.leases[name]
			if !ok || l.Metadata.ResourceVersion != current.Metadata.ResourceVersion {
				http.Error(w, `{"status":"Failure","reason":"Conflict"}`, http.StatusConflict)
				return
			}
			f.rv++
			l.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			f.leases[name] = &l
			_ = json.NewEncoder(w).Encode(l)
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

// testLeaseDuration is deliberately >=1s -- LeaseDurationSeconds is a real
// int32-seconds wire field, so a sub-second value truncates to 0 and would
// make every lease look immediately stale, the same reasoning
// internal/leaderelection's own test constant documents.
const testLeaseDuration = 2 * time.Second

func TestLeaseNameConvention(t *testing.T) {
	if got, want := LeaseName("node-a"), "kairon-node-node-a"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenewCreatesLeaseWhenAbsent(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	if err := Renew(context.Background(), kc, "kairon-system", "node-a", testLeaseDuration); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	lease, err := kc.GetLease(context.Background(), "kairon-system", LeaseName("node-a"))
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "node-a" {
		t.Fatalf("unexpected holder identity: %+v", lease.Spec.HolderIdentity)
	}
	if lease.Spec.RenewTime == nil {
		t.Fatal("expected RenewTime to be set")
	}
	if !IsFresh(lease, testLeaseDuration) {
		t.Fatal("freshly created lease should be fresh")
	}
}

func TestRenewBumpsRenewTimeOnAnExistingLease(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()
	if err := Renew(ctx, kc, "kairon-system", "node-a", testLeaseDuration); err != nil {
		t.Fatalf("first Renew: %v", err)
	}
	first, err := kc.GetLease(ctx, "kairon-system", LeaseName("node-a"))
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := Renew(ctx, kc, "kairon-system", "node-a", testLeaseDuration); err != nil {
		t.Fatalf("second Renew: %v", err)
	}
	second, err := kc.GetLease(ctx, "kairon-system", LeaseName("node-a"))
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if !second.Spec.RenewTime.After(first.Spec.RenewTime.Time) {
		t.Fatalf("expected RenewTime to advance: first=%v second=%v", first.Spec.RenewTime, second.Spec.RenewTime)
	}
}

func TestRenewIsIndependentPerNode(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	ctx := context.Background()
	if err := Renew(ctx, kc, "kairon-system", "node-a", testLeaseDuration); err != nil {
		t.Fatalf("Renew node-a: %v", err)
	}
	// A second node's own Lease is a distinct object (different name) --
	// GetLease for node-b must not find node-a's, confirming the naming
	// convention actually isolates them.
	if _, err := kc.GetLease(ctx, "kairon-system", LeaseName("node-b")); !kube.IsNotFound(err) {
		t.Fatalf("expected node-b's lease to not exist yet, got err=%v", err)
	}
	if err := Renew(ctx, kc, "kairon-system", "node-b", testLeaseDuration); err != nil {
		t.Fatalf("Renew node-b: %v", err)
	}
	b, err := kc.GetLease(ctx, "kairon-system", LeaseName("node-b"))
	if err != nil {
		t.Fatalf("GetLease node-b: %v", err)
	}
	if b.Spec.HolderIdentity == nil || *b.Spec.HolderIdentity != "node-b" {
		t.Fatalf("node-b's lease has the wrong holder identity: %+v", b.Spec.HolderIdentity)
	}
	// node-a's lease must be untouched by node-b's Renew call.
	a, err := kc.GetLease(ctx, "kairon-system", LeaseName("node-a"))
	if err != nil {
		t.Fatalf("GetLease node-a: %v", err)
	}
	if a.Spec.HolderIdentity == nil || *a.Spec.HolderIdentity != "node-a" {
		t.Fatalf("node-a's lease was clobbered: %+v", a.Spec.HolderIdentity)
	}
}

func TestIsFreshFalseWhenRenewTimeUnset(t *testing.T) {
	if IsFresh(model.Lease{}, testLeaseDuration) {
		t.Fatal("a lease with no RenewTime must never be considered fresh")
	}
}

func TestIsFreshFalseAfterLeaseDurationElapses(t *testing.T) {
	stale := model.NewMicroTime(time.Now().Add(-10 * time.Second))
	dur := int32(2)
	lease := model.Lease{Spec: model.LeaseSpec{RenewTime: &stale, LeaseDurationSeconds: &dur}}
	if IsFresh(lease, testLeaseDuration) {
		t.Fatal("a lease renewed 10s ago with a 2s duration must be stale")
	}
}

func TestIsFreshTrueWithinLeaseDuration(t *testing.T) {
	recent := model.NewMicroTime(time.Now().Add(-1 * time.Second))
	dur := int32(30)
	lease := model.Lease{Spec: model.LeaseSpec{RenewTime: &recent, LeaseDurationSeconds: &dur}}
	if !IsFresh(lease, testLeaseDuration) {
		t.Fatal("a lease renewed 1s ago with a 30s duration should be fresh")
	}
}
