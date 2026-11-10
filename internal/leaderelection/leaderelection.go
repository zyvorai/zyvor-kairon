// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package leaderelection provides Lease-based (coordination.k8s.io/v1)
// single-writer coordination for kairon-controller, built on Kairon's own
// hand-rolled internal/kube REST client rather than client-go's own
// k8s.io/client-go/tools/leaderelection -- consistent with this project's
// Go-stdlib-only posture (internal/kube already talks to the Kubernetes API
// over plain net/http; pulling in all of client-go for this one feature
// would be a much larger dependency than the two deliberate exceptions this
// project already carries, for OIDC and CSI). The algorithm mirrors
// client-go's own resourcelock/leaderelection shape -- a Lease object held
// by holderIdentity, renewed periodically, taken over once its
// leaseDurationSeconds has elapsed since the last renewTime -- but is
// intentionally simpler: one goroutine, one Lease, no pluggable lock
// interface, since kairon-controller only ever needs exactly this one lock.
//
// Without this, kairon-controller's reconcile loop (spec.nodeName
// assignment, migration/snapshot/quota reconciliation, status writes) has
// no single-writer guarantee: two replicas would race on the same Machines
// and MachineMigrations. The admission webhook and health/metrics server
// are deliberately NOT gated by an Elector -- they're stateless per-request
// reads against current cluster state, safe to serve from every replica at
// once, the same way a real Kubernetes ValidatingWebhookConfiguration's
// Service load-balances across pods.
//
// Every failure mode here is fail-closed: a GET/PUT error, a decode
// failure, or plain doubt about who currently holds the Lease all resolve
// to "not leader" rather than "assume leadership anyway" -- consistent
// with this project's NeedsRecovery philosophy of naming an uncertain
// outcome instead of guessing.
package leaderelection

import (
	"context"
	"log/slog"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	// DefaultLeaseDuration is how long a held Lease remains valid without
	// renewal before another replica may take it over.
	DefaultLeaseDuration = 15 * time.Second
	// DefaultRenewPeriod is how often the current leader renews its
	// Lease -- well under DefaultLeaseDuration so a handful of missed
	// renewals (a slow apiserver, a GC pause) don't cost leadership.
	DefaultRenewPeriod = 5 * time.Second
	// DefaultRetryPeriod is how often a non-leader checks whether the
	// Lease has become acquirable.
	DefaultRetryPeriod = 5 * time.Second
)

// Elector coordinates exactly one Lease. Zero-value duration fields fall
// back to the Default* constants above.
type Elector struct {
	Kube      *kube.Client
	Namespace string
	Name      string
	// Identity identifies this process as a Lease holder -- must be
	// unique per running instance (e.g. hostname, or hostname+pid if more
	// than one instance can run on one host).
	Identity string
	Log      *slog.Logger

	LeaseDuration time.Duration
	RenewPeriod   time.Duration
	RetryPeriod   time.Duration
}

func (e *Elector) leaseDuration() time.Duration {
	if e.LeaseDuration > 0 {
		return e.LeaseDuration
	}
	return DefaultLeaseDuration
}

func (e *Elector) renewPeriod() time.Duration {
	if e.RenewPeriod > 0 {
		return e.RenewPeriod
	}
	return DefaultRenewPeriod
}

// renewDeadline is how long one renewal attempt may take. A third of the
// lease duration keeps renewPeriod + renewDeadline under leaseDuration for
// the default 15s/5s settings, so a stuck renewal always ends leadership
// before the lease can expire under another replica.
func (e *Elector) renewDeadline() time.Duration {
	return e.leaseDuration() / 3
}

func (e *Elector) retryPeriod() time.Duration {
	if e.RetryPeriod > 0 {
		return e.RetryPeriod
	}
	return DefaultRetryPeriod
}

// Run blocks until ctx is canceled. Whenever this process acquires the
// Lease, onStart is called in its own goroutine with a leaderCtx that Run
// cancels the instant leadership is lost (a renewal is refused or fails) or
// ctx itself is canceled; Run then waits for onStart to return before
// attempting to re-acquire, so two invocations of onStart are never running
// concurrently, even across a leadership flap.
func (e *Elector) Run(ctx context.Context, onStart func(leaderCtx context.Context)) {
	for ctx.Err() == nil {
		if !e.tryAcquireOrRenew(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(e.retryPeriod()):
			}
			continue
		}
		e.Log.Info("leader election: acquired lease", "lease", e.Name, "namespace", e.Namespace, "identity", e.Identity)
		leaderCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			onStart(leaderCtx)
		}()
		e.holdLease(ctx, cancel)
		cancel()
		<-done
		e.Log.Info("leader election: stopped leading", "lease", e.Name, "namespace", e.Namespace, "identity", e.Identity)
	}
}

// holdLease renews the Lease every renewPeriod until a renewal is refused
// or ctx is canceled, calling cancel the instant leadership is lost so the
// caller's leaderCtx-scoped onStart stops promptly.
func (e *Elector) holdLease(ctx context.Context, cancel func()) {
	t := time.NewTicker(e.renewPeriod())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Bound each renewal well below the lease duration: with only the
			// kube client's own HTTP timeout (20s) a hung apiserver call would
			// let this process keep reconciling past the lease's expiry while
			// another replica has already taken over.
			rctx, rcancel := context.WithTimeout(ctx, e.renewDeadline())
			ok := e.renew(rctx)
			rcancel()
			if !ok {
				cancel()
				return
			}
		}
	}
}

// tryAcquireOrRenew is called only while NOT currently holding the Lease
// (at startup, or right after losing it). It reports whether this process
// now holds it.
func (e *Elector) tryAcquireOrRenew(ctx context.Context) bool {
	current, err := e.Kube.GetLease(ctx, e.Namespace, e.Name)
	if kube.IsNotFound(err) {
		return e.create(ctx)
	}
	if err != nil {
		e.Log.Warn("leader election: get lease failed", "lease", e.Name, "error", err)
		return false
	}
	if !e.expired(current) && held(current) && *current.Spec.HolderIdentity != e.Identity {
		return false // someone else legitimately holds it
	}
	return e.takeover(ctx, current)
}

// renew is called every renewPeriod while this process believes it holds
// the Lease. It re-reads first (rather than blindly PUTting) so a Lease
// another instance force-took over after this process stalled past
// leaseDuration is noticed here, not just via a 409 on the write.
func (e *Elector) renew(ctx context.Context) bool {
	current, err := e.Kube.GetLease(ctx, e.Namespace, e.Name)
	if err != nil {
		e.Log.Warn("leader election: renew get failed", "lease", e.Name, "error", err)
		return false
	}
	if !held(current) || *current.Spec.HolderIdentity != e.Identity {
		e.Log.Warn("leader election: lease held by another identity, stepping down", "lease", e.Name)
		return false
	}
	now := model.NewMicroTime(time.Now())
	current.Spec.RenewTime = &now
	if _, err := e.Kube.UpdateLease(ctx, e.Namespace, current); err != nil {
		if !kube.IsConflict(err) {
			e.Log.Warn("leader election: renew failed", "lease", e.Name, "error", err)
		}
		return false
	}
	return true
}

func (e *Elector) create(ctx context.Context) bool {
	now := model.NewMicroTime(time.Now())
	dur := int32(e.leaseDuration().Seconds())
	transitions := int32(1)
	identity := e.Identity
	lease := model.Lease{
		TypeMeta: model.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		Metadata: model.ObjectMeta{Name: e.Name, Namespace: e.Namespace},
		Spec: model.LeaseSpec{
			HolderIdentity:       &identity,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &now,
			RenewTime:            &now,
			LeaseTransitions:     &transitions,
		},
	}
	if _, err := e.Kube.CreateLease(ctx, e.Namespace, lease); err != nil {
		// Most commonly a lost race (another replica created it first,
		// surfaced as 409/AlreadyExists) -- not worth logging as a
		// warning, the next tick's GET will see it.
		return false
	}
	return true
}

// takeover mutates current in place (preserving Metadata.ResourceVersion,
// so UpdateLease's optimistic-concurrency check catches a concurrent
// takeover by another replica) and writes it back claiming this process as
// holder.
func (e *Elector) takeover(ctx context.Context, current model.Lease) bool {
	now := model.NewMicroTime(time.Now())
	dur := int32(e.leaseDuration().Seconds())
	priorHolder := ""
	if current.Spec.HolderIdentity != nil {
		priorHolder = *current.Spec.HolderIdentity
	}
	transitions := int32(0)
	if current.Spec.LeaseTransitions != nil {
		transitions = *current.Spec.LeaseTransitions
	}
	if priorHolder != "" && priorHolder != e.Identity {
		transitions++
	}
	identity := e.Identity
	current.TypeMeta = model.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}
	current.Spec.HolderIdentity = &identity
	current.Spec.LeaseDurationSeconds = &dur
	current.Spec.RenewTime = &now
	current.Spec.AcquireTime = &now
	current.Spec.LeaseTransitions = &transitions
	if _, err := e.Kube.UpdateLease(ctx, e.Namespace, current); err != nil {
		if !kube.IsConflict(err) {
			e.Log.Warn("leader election: takeover failed", "lease", e.Name, "error", err)
		}
		return false
	}
	return true
}

func held(l model.Lease) bool {
	return l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity != ""
}

func (e *Elector) expired(l model.Lease) bool {
	if l.Spec.RenewTime == nil {
		return true
	}
	d := e.leaseDuration()
	if l.Spec.LeaseDurationSeconds != nil {
		d = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return time.Since(l.Spec.RenewTime.Time) > d
}
