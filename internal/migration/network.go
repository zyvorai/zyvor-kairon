// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"context"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// NetworkAwareDestination restores/resumes FluxVM network state around peer prepare/commit.
type NetworkAwareDestination struct {
	Inner DestinationDriver
	Flux  *fluxvm.Client
	// Restores receives conntrack restore results for status projection.
	Restores *RestoreStore
}

func (d NetworkAwareDestination) Prepare(ctx context.Context, session Session) (PrepareResult, error) {
	res, err := d.Inner.Prepare(ctx, session)
	if err != nil {
		return res, err
	}
	if !res.TransferSupported || d.Flux == nil || session.RuntimeID == "" {
		return res, nil
	}
	// A separate destination runtime only becomes a VM at commit, so its
	// network state is restored then (see Commit).
	if res.DestinationRuntimeID != "" {
		return res, nil
	}
	if err := d.restoreNetwork(ctx, session, session.RuntimeID); err != nil {
		_ = d.Inner.Abort(ctx, session)
		return PrepareResult{}, err
	}
	return res, nil
}

func (d NetworkAwareDestination) restoreNetwork(ctx context.Context, session Session, runtimeID string) error {
	if len(session.NetworkSnapshot) > 0 {
		if err := d.Flux.NetworkMigrationRestore(ctx, runtimeID, session.NetworkSnapshot); err != nil {
			return fmt.Errorf("network migration restore: %w", err)
		}
	}
	session.RuntimeID = runtimeID
	return restoreConntrack(ctx, d.Flux, session, d.Restores)
}

func (d NetworkAwareDestination) Commit(ctx context.Context, session Session) error {
	if err := d.Inner.Commit(ctx, session); err != nil {
		return err
	}
	if d.Flux == nil {
		return nil
	}
	runtimeID := session.RuntimeID
	if session.DestinationRuntimeID != "" {
		runtimeID = session.DestinationRuntimeID
		if err := d.restoreNetwork(ctx, session, runtimeID); err != nil {
			return err
		}
	}
	if len(session.NetworkSnapshot) == 0 || runtimeID == "" {
		return nil
	}
	if err := d.Flux.NetworkMigrationResume(ctx, runtimeID); err != nil {
		return fmt.Errorf("network migration resume: %w", err)
	}
	return nil
}

func (d NetworkAwareDestination) Abort(ctx context.Context, session Session) error {
	return d.Inner.Abort(ctx, session)
}

// Diagnose reports the destination's own durable session-store phase
// (session.Phase, as loaded by the caller from Store) alongside live ground
// truth from the destination's FluxVM, looked up by the deterministic
// runtime name Machine.RuntimeName() produces -- the same mechanism the
// destination agent already uses to discover an incoming runtime after
// cutover (docs/architecture.md). session.Phase == "Prepared" is NOT proof
// the destination never committed (see Commit's own doc comment above), so
// callers making a NeedsRecovery decision must use both fields, not just
// SessionPhase.
func (d NetworkAwareDestination) Diagnose(ctx context.Context, session Session) (DiagnosisResult, error) {
	result := DiagnosisResult{
		SessionPhase: session.Phase,
		ObservedAt:   time.Now().UTC(),
	}
	if d.Flux == nil {
		return result, nil
	}
	runtimeName := model.Machine{Metadata: model.ObjectMeta{Namespace: session.Namespace, Name: session.Machine}}.RuntimeName()
	rec, err := d.Flux.LookupByName(ctx, runtimeName)
	if err != nil {
		return result, fmt.Errorf("looking up destination runtime %q: %w", runtimeName, err)
	}
	if rec == nil {
		return result, nil
	}
	result.DestinationRuntimeFound = true
	result.DestinationRuntimeStatus = rec.Status
	result.DestinationRuntimeID = rec.ID()
	result.DestinationGuestIP = rec.GuestIP
	return result, nil
}
