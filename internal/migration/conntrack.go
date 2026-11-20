// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package migration

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/fluxvm"
)

// restoreConntrack pushes the source conntrack map onto the destination
// runtime before resume. A missing snapshot is a no-op so older peers
// keep working. An identity mismatch fails closed.
func restoreConntrack(ctx context.Context, flux *fluxvm.Client, session Session, store *RestoreStore) error {
	if flux == nil || len(session.ConntrackSnapshot) == 0 || session.RuntimeID == "" {
		return nil
	}
	snap, err := ebpfedge.UnmarshalSnapshot(session.ConntrackSnapshot)
	if err != nil {
		return fmt.Errorf("conntrack snapshot: %w", err)
	}
	want := ebpfedge.StableIdentity(session.Namespace, session.Machine)
	now := session.UpdatedAt
	if now.IsZero() {
		now = snap.ExportedAt
	}
	res, err := ebpfedge.RestoreConntrack(want, snap, now)
	if err != nil {
		return fmt.Errorf("conntrack restore: %w", err)
	}
	if err := flux.RestoreConntrack(ctx, session.RuntimeID, snap); err != nil {
		return fmt.Errorf("conntrack restore: %w", err)
	}
	store.Put(session.Namespace, session.Machine, res)
	return nil
}
