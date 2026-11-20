// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fleet

import (
	"context"
	"fmt"
	"math"

	"github.com/zyvorai/kairon/internal/model"
)

func (e *Engine) meter(_ context.Context, o *model.FleetResource, machines []model.Machine) error {
	s, _ := decode[LedgerSpec](o.Spec)
	now := e.now()
	if o.Status.Totals == nil {
		o.Status.Totals = map[string]float64{}
	}
	if o.Status.Watermark == nil {
		o.Status.Watermark = &now
		e.status(o, "Recording", "meter initialized")
		return nil
	}
	seconds := now.Sub(*o.Status.Watermark).Seconds()
	if seconds <= 0 {
		return nil
	}
	if seconds > float64(s.MaxGapSeconds) {
		o.Status.Totals["unobserved_seconds"] += seconds
		o.Status.Watermark = &now
		e.status(o, "Gap", "observation gap recorded; resource charges were not invented")
		return nil
	}
	for _, m := range machines {
		if !selected(*o, s.Selector, m) || m.Spec.NodeName == "" || !contains([]string{"Running", "Paused"}, m.Status.Phase) {
			continue
		}
		// These are provisioned-resource hours sampled at reconcile time, not
		// a claim about instantaneous CPU utilization or billable storage traffic.
		cpu, err := model.ParseVCPUs(m.Spec.Resources.CPU)
		if err != nil {
			return err
		}
		mem, err := model.ParseMemoryMiB(m.Spec.Resources.Memory)
		if err != nil {
			return err
		}
		o.Status.Totals["vcpu_hours"] += float64(cpu) * seconds / 3600
		o.Status.Totals["memory_gib_hours"] += float64(mem) / 1024 * seconds / 3600
		o.Status.Totals["machine_hours"] += seconds / 3600
	}
	for _, value := range o.Status.Totals {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("invalid usage total")
		}
	}
	o.Status.Watermark = &now
	e.status(o, "Recording", "durable provisioned-resource showback; status writes use resourceVersion CAS")
	return nil
}
