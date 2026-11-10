// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package leaderelection

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// fakeLeaseStore is a minimal in-memory coordination.k8s.io/v1 Lease
// double, just enough to exercise Client.GetLease/CreateLease/UpdateLease
// -- same intent as internal/uiapi's fakeConfigMapStore, scoped to this one
// resource. PUT enforces resourceVersion-based optimistic concurrency (409
// on mismatch), mirroring a real API server closely enough to exercise the
// conflict-detection path this package's takeover/renew logic depends on.
type fakeLeaseStore struct {
	mu    sync.Mutex
	lease *model.Lease
	rv    int
}

func (f *fakeLeaseStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/leases/"):
			if f.lease == nil {
				http.Error(w, `{"status":"Failure","reason":"NotFound"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(*f.lease)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/leases"):
			if f.lease != nil {
				http.Error(w, `{"status":"Failure","reason":"AlreadyExists"}`, http.StatusConflict)
				return
			}
			var l model.Lease
			_ = json.NewDecoder(r.Body).Decode(&l)
			f.rv++
			l.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			f.lease = &l
			_ = json.NewEncoder(w).Encode(*f.lease)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/leases/"):
			var l model.Lease
			_ = json.NewDecoder(r.Body).Decode(&l)
			if f.lease == nil || l.Metadata.ResourceVersion != f.lease.Metadata.ResourceVersion {
				http.Error(w, `{"status":"Failure","reason":"Conflict"}`, http.StatusConflict)
				return
			}
			f.rv++
			l.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			f.lease = &l
			_ = json.NewEncoder(w).Encode(*f.lease)
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

// testLeaseDuration is deliberately >=1s: LeaseSpec.LeaseDurationSeconds is
// a real coordination.k8s.io/v1 int32-seconds field (see model.LeaseSpec),
// so anything sub-second truncates to 0 on the wire and would make every
// lease look immediately expired -- a real API constraint, not a detail
// this package's tests should paper over with a fake finer-grained clock.
const testLeaseDuration = 1 * time.Second

func testElector(kc *kube.Client, identity string) *Elector {
	return &Elector{
		Kube:          kc,
		Namespace:     "kairon-system",
		Name:          "kairon-controller",
		Identity:      identity,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		LeaseDuration: testLeaseDuration,
		RenewPeriod:   200 * time.Millisecond,
		RetryPeriod:   100 * time.Millisecond,
	}
}

func TestTryAcquireCreatesLeaseWhenAbsent(t *testing.T) {
	store := &fakeLeaseStore{}
	e := testElector(mustKubeClientWithHandler(t, store.handler()), "a")
	if !e.tryAcquireOrRenew(context.Background()) {
		t.Fatal("expected acquire to succeed against an absent lease")
	}
	if store.lease == nil || store.lease.Spec.HolderIdentity == nil || *store.lease.Spec.HolderIdentity != "a" {
		t.Fatalf("lease not created with expected holder: %+v", store.lease)
	}
}

func TestTryAcquireFailsAgainstUnexpiredOtherHolder(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	a := testElector(kc, "a")
	if !a.tryAcquireOrRenew(context.Background()) {
		t.Fatal("a should acquire the empty lease")
	}
	b := testElector(kc, "b")
	if b.tryAcquireOrRenew(context.Background()) {
		t.Fatal("b should not acquire a's unexpired lease")
	}
}

func TestTakeoverAfterExpiry(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	a := testElector(kc, "a")
	if !a.tryAcquireOrRenew(context.Background()) {
		t.Fatal("a should acquire")
	}
	time.Sleep(a.leaseDuration() + 50*time.Millisecond)
	b := testElector(kc, "b")
	if !b.tryAcquireOrRenew(context.Background()) {
		t.Fatal("b should take over an expired lease")
	}
	if store.lease.Spec.HolderIdentity == nil || *store.lease.Spec.HolderIdentity != "b" {
		t.Fatalf("holder = %v, want b", store.lease.Spec.HolderIdentity)
	}
	if store.lease.Spec.LeaseTransitions == nil || *store.lease.Spec.LeaseTransitions != 2 {
		t.Fatalf("leaseTransitions = %v, want 2", store.lease.Spec.LeaseTransitions)
	}
}

func TestRenewKeepsLeadershipAndAdvancesRenewTime(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	a := testElector(kc, "a")
	if !a.tryAcquireOrRenew(context.Background()) {
		t.Fatal("acquire failed")
	}
	firstRenew := *store.lease.Spec.RenewTime
	time.Sleep(5 * time.Millisecond)
	if !a.renew(context.Background()) {
		t.Fatal("renew should succeed while still holder")
	}
	if !store.lease.Spec.RenewTime.After(firstRenew.Time) {
		t.Fatal("renewTime did not advance")
	}
}

func TestRenewFailsOnceAnotherIdentityHoldsIt(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	a := testElector(kc, "a")
	a.tryAcquireOrRenew(context.Background())
	time.Sleep(a.leaseDuration() + 50*time.Millisecond)
	b := testElector(kc, "b")
	b.tryAcquireOrRenew(context.Background())
	if a.renew(context.Background()) {
		t.Fatal("a should no longer be able to renew after b's takeover")
	}
}

func TestRunCallsOnStartWhileLeaderAndCancelsOnLoss(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	e := testElector(kc, "a")

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		e.Run(ctx, func(leaderCtx context.Context) {
			close(started)
			<-leaderCtx.Done()
			close(stopped)
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("onStart never called")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("leaderCtx was never canceled after Run's ctx was canceled")
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never returned after ctx cancellation")
	}
}

func TestRunLosesLeadershipWhenAnotherIdentityTakesOverExpiredLease(t *testing.T) {
	store := &fakeLeaseStore{}
	kc := mustKubeClientWithHandler(t, store.handler())
	a := testElector(kc, "a")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	lost := make(chan struct{})
	go a.Run(ctx, func(leaderCtx context.Context) {
		close(started)
		<-leaderCtx.Done()
		close(lost)
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("a never became leader")
	}

	// a's Run loop actively renews every RenewPeriod, so it never sees a
	// naturally expired lease on its own -- simulate an external takeover
	// instead (an operator recreating the Lease, or another replica
	// winning a genuine race), the same effect an expired lease's takeover
	// has on the wire: a different holderIdentity at a newer
	// resourceVersion. a's next renew() should notice and step down.
	store.mu.Lock()
	newHolder := "b"
	store.rv++
	store.lease.Spec.HolderIdentity = &newHolder
	store.lease.Metadata.ResourceVersion = strconv.Itoa(store.rv)
	store.mu.Unlock()

	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("a never noticed it lost leadership to b")
	}
}

// TestHoldLeaseStepsDownWhenRenewHangs: an apiserver that accepts the renew
// request but never answers must end leadership within the renew deadline,
// not after the kube client's much longer HTTP timeout.
func TestHoldLeaseStepsDownWhenRenewHangs(t *testing.T) {
	store := &fakeLeaseStore{}
	var hang atomic.Bool
	inner := store.handler()
	kc := mustKubeClientWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang.Load() {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		inner.ServeHTTP(w, r)
	}))
	a := testElector(kc, "a")
	if !a.tryAcquireOrRenew(context.Background()) {
		t.Fatal("acquire failed")
	}
	hang.Store(true)

	lost := make(chan struct{})
	start := time.Now()
	go a.holdLease(context.Background(), func() { close(lost) })
	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Fatal("leadership was not given up while the renew call hung")
	}
	if d := time.Since(start); d > testLeaseDuration {
		t.Fatalf("stepped down after %v, want less than the %v lease duration", d, testLeaseDuration)
	}
}
