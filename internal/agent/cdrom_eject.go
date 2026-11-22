// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agent

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// cdromsToEject returns the runtime's CD-ROM drives that still hold media
// but are no longer listed in spec.cdroms.
func cdromsToEject(spec []model.MachineCdrom, rec *fluxvm.Record) []string {
	want := make(map[string]bool, len(spec))
	for _, c := range spec {
		want[c.Name] = true
	}
	var out []string
	for _, c := range rec.Request.Cdroms {
		if c.Path != "" && !want[c.Name] {
			out = append(out, c.Name)
		}
	}
	return out
}

// ejectRemovedCdroms ejects the medium of every drive removed from
// spec.cdroms, so an installed VM stops depending on a host-local ISO and
// can live-migrate. FluxVM keeps the empty drive.
func (a *Agent) ejectRemovedCdroms(ctx context.Context, m model.Machine, rec *fluxvm.Record) (*fluxvm.Record, error) {
	for _, name := range cdromsToEject(m.Spec.Cdroms, rec) {
		updated, err := a.Flux.EjectCdrom(ctx, rec.ID(), name)
		if err != nil {
			return rec, fmt.Errorf("eject cdrom %q: %w", name, err)
		}
		a.Log.Info("ejected cdrom", "machine", m.Metadata.Name, "cdrom", name, "runtimeID", rec.ID())
		rec = updated
	}
	return rec, nil
}
