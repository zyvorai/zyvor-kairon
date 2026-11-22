// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package nodeliveness gives kairon-node an independent, per-node
// coordination.k8s.io/v1 Lease it renews every reconcile tick -- a real
// "I am alive and reconciling" signal kaironctl fence can cross-check
// before trusting Kubernetes Node Ready alone.
//
// Without this, fence's only input is the Node Ready condition
// kairon-controller already derives (internal/controller/fencing.go) --
// a pure kubelet-health signal. That's the wrong single source of truth
// for a destructive operator action: kubelet can flap NotReady (a brief
// network blip, an apiserver hiccup) while kairon-node's own reconcile
// loop keeps running fine, in which case fencing would abandon a Machine
// that's still actively, correctly managed. This package doesn't replace
// Node Ready -- it gives fence a second, independent signal to refuse to
// proceed on when the two disagree (see internal/kaironctl's own use of
// IsFresh), rather than silently trusting the kubelet-only signal.
//
// Deliberately NOT internal/leaderelection reused directly: that package
// solves mutual exclusion (multiple kairon-controller replicas contending
// for one shared Lease, with acquire/takeover semantics). A per-node
// liveness Lease has no contention at all -- only node N's own kairon-node
// instance ever legitimately writes kairon-node-<N>'s Lease, so the
// takeover/HolderIdentity-mismatch handling leaderelection.Elector needs
// would be unused complexity here. This package is intentionally smaller:
// create-or-renew, no acquire/lose-leadership state machine.
package nodeliveness

import (
	"context"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// DefaultLeaseDuration is how long a renewed Lease is considered fresh
// without another renewal -- comfortably above a normal reconcile
// interval (a handful of missed ticks, a slow apiserver call) so a
// healthy kairon-node's Lease practically never looks stale by accident.
const DefaultLeaseDuration = 60 * time.Second

// LeaseName is the naming convention both the write side (Renew, called
// by kairon-node) and the read side (IsFresh's caller, kaironctl fence)
// must agree on -- kept in one place so it can't drift between them.
// Kubernetes Node names are already valid Lease name components (both are
// DNS-1123 subdomains), so no sanitization is needed.
func LeaseName(nodeName string) string {
	return "kairon-node-" + nodeName
}

// Renew creates this node's own liveness Lease if absent, or bumps its
// RenewTime if already held by this identity. Called once per reconcile
// tick from kairon-node's own main loop -- best-effort: a transient
// apiserver error here must never block or fail the actual VM
// reconciliation this Lease is only ever secondary to, so callers should
// log and continue rather than treat a non-nil error as fatal.
func Renew(ctx context.Context, kc *kube.Client, namespace, nodeName string, leaseDuration time.Duration) error {
	if leaseDuration <= 0 {
		leaseDuration = DefaultLeaseDuration
	}
	name := LeaseName(nodeName)
	now := model.NewMicroTime(time.Now())
	dur := int32(leaseDuration.Seconds())
	current, err := kc.GetLease(ctx, namespace, name)
	if kube.IsNotFound(err) {
		identity := nodeName
		one := int32(1)
		lease := model.Lease{
			TypeMeta: model.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
			Metadata: model.ObjectMeta{Name: name, Namespace: namespace},
			Spec: model.LeaseSpec{
				HolderIdentity:       &identity,
				LeaseDurationSeconds: &dur,
				AcquireTime:          &now,
				RenewTime:            &now,
				LeaseTransitions:     &one,
			},
		}
		_, err := kc.CreateLease(ctx, namespace, lease)
		if kube.IsConflict(err) {
			// Another kairon-node process for this same node (a restart
			// racing a still-terminating old instance) created it first
			// in between our GET and CREATE -- the next tick's renewal
			// will pick it up cleanly, nothing to do here.
			return nil
		}
		return err
	}
	if err != nil {
		return fmt.Errorf("get lease %s/%s: %w", namespace, name, err)
	}
	current.TypeMeta = model.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}
	identity := nodeName
	current.Spec.HolderIdentity = &identity
	current.Spec.LeaseDurationSeconds = &dur
	current.Spec.RenewTime = &now
	if _, err := kc.UpdateLease(ctx, namespace, current); err != nil {
		if kube.IsConflict(err) {
			// Same benign race as above, from the other direction (two
			// renewals overlapping) -- the next tick corrects it.
			return nil
		}
		return fmt.Errorf("update lease %s/%s: %w", namespace, name, err)
	}
	return nil
}

// IsFresh reports whether lease was renewed recently enough to trust as
// "kairon-node on this node is alive and reconciling" -- true when
// RenewTime is set and within leaseDuration (the Lease's own
// LeaseDurationSeconds when set, falling back to the caller-supplied
// leaseDuration otherwise) of now. A Lease with no RenewTime at all (the
// zero value, never legitimately produced by Renew) is never fresh --
// fail closed, matching every other liveness-adjacent check in this
// project.
func IsFresh(lease model.Lease, leaseDuration time.Duration) bool {
	if lease.Spec.RenewTime == nil {
		return false
	}
	d := leaseDuration
	if lease.Spec.LeaseDurationSeconds != nil {
		d = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}
	if d <= 0 {
		d = DefaultLeaseDuration
	}
	return time.Since(lease.Spec.RenewTime.Time) <= d
}
