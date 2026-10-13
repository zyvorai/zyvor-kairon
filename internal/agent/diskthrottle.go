// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// rootDisk is FluxVM's name for the boot disk.
const rootDisk = "root"

// desiredDiskThrottles is every limit spec asks for: the root's and those of
// spec.disks entries attached to the VM.
func desiredDiskThrottles(m model.Machine, attached []string) map[string]model.DiskThrottle {
	out := map[string]model.DiskThrottle{}
	if io := m.Spec.Image.IO; io != nil && !io.Throttle.IsZero() {
		out[rootDisk] = *io.Throttle
	}
	for _, d := range m.Spec.Disks {
		if d.IO != nil && !d.IO.Throttle.IsZero() && slices.Contains(attached, d.Name) {
			out[d.Name] = *d.IO.Throttle
		}
	}
	return out
}

// reconcileDiskThrottles sets, changes and lifts per-disk limits on a running
// qemu VM. FluxVM keeps them across restarts, so they are re-sent only when
// spec changes or the VM is new to this node (force).
func (a *Agent) reconcileDiskThrottles(ctx context.Context, m model.Machine, rec *fluxvm.Record, attached []string, force bool) ([]model.AppliedDiskThrottle, error) {
	prev := m.Status.AppliedDiskThrottles
	if normalizePhase(rec.Status) != "Running" {
		return prev, nil
	}
	want := desiredDiskThrottles(m, attached)
	if len(want) == 0 && len(prev) == 0 {
		return nil, nil
	}
	if rec.Backend != "" && rec.Backend != "qemu" {
		return prev, fmt.Errorf("disk throttles need the qemu backend; this VM runs on %s", rec.Backend)
	}
	if io := m.Spec.Image.IO; io != nil {
		if err := io.Throttle.Validate(); err != nil {
			return prev, fmt.Errorf("spec.image.io.%w", err)
		}
	}
	have := map[string]model.DiskThrottle{}
	for _, p := range prev {
		have[p.Disk] = p.Throttle
	}
	var errs []error
	for disk, t := range want {
		if cur, ok := have[disk]; ok && cur == t && !force {
			continue
		}
		if err := a.Flux.SetDiskThrottle(ctx, rec.ID(), disk, &t); err != nil {
			errs = append(errs, fmt.Errorf("throttle disk %q: %w", disk, err))
			continue
		}
		have[disk] = t
		a.Log.Info("set disk throttle", "machine", m.Metadata.Name, "disk", disk)
	}
	for disk := range have {
		if _, ok := want[disk]; ok {
			continue
		}
		if disk != rootDisk && !slices.Contains(attached, disk) {
			delete(have, disk)
			continue
		}
		if err := a.Flux.SetDiskThrottle(ctx, rec.ID(), disk, nil); err != nil {
			errs = append(errs, fmt.Errorf("lift throttle on disk %q: %w", disk, err))
			continue
		}
		delete(have, disk)
		a.Log.Info("lifted disk throttle", "machine", m.Metadata.Name, "disk", disk)
	}
	out := make([]model.AppliedDiskThrottle, 0, len(have))
	for disk, t := range have {
		out = append(out, model.AppliedDiskThrottle{Disk: disk, Throttle: t})
	}
	slices.SortFunc(out, func(x, y model.AppliedDiskThrottle) int { return strings.Compare(x.Disk, y.Disk) })
	if len(out) == 0 {
		out = nil
	}
	return out, errors.Join(errs...)
}
