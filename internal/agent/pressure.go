// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// DefaultPressureReportInterval is PressureReportConfig.Interval's fallback.
const DefaultPressureReportInterval = 30 * time.Second

// PressureReportConfig tunes the publisher that writes the host's PSI and
// each local Machine's own stall time to the Node's
// model.AnnotationNodePressure, for DRS's pressure signal.
type PressureReportConfig struct {
	Disabled bool
	Interval time.Duration
}

func (c PressureReportConfig) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultPressureReportInterval
}

// errNoHostPSI: FluxVM answered but the kernel reports no PSI (macOS, or
// Linux booted with psi=0). Publishing zeros would make the node look calm.
var errNoHostPSI = errors.New("FluxVM reports no host PSI")

type pressureState struct {
	mu sync.Mutex
	// failing is set after a failed host sample is logged, so a FluxVM
	// without GET /v1/host/pressure warns once rather than every interval.
	failing bool
}

func (a *Agent) runPressureReport(ctx context.Context) {
	if a.PressureReport.Disabled || a.Kube == nil || a.Flux == nil || a.NodeName == "" {
		return
	}
	t := time.NewTicker(a.PressureReport.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.publishPressure(ctx)
		}
	}
}

// publishPressure samples host PSI and the PSI of every Running Machine on
// this node, and writes them to the Node annotation.
func (a *Agent) publishPressure(ctx context.Context) {
	host, err := a.Flux.GetHostPressure(ctx)
	if err == nil && host.CPUSome == nil && host.MemorySome == nil && host.IOSome == nil {
		err = errNoHostPSI
	}
	a.pressure.mu.Lock()
	wasFailing := a.pressure.failing
	a.pressure.failing = err != nil
	a.pressure.mu.Unlock()
	if err != nil {
		if !wasFailing {
			a.log().Warn("host pressure: FluxVM sample failed; DRS's pressure signal sees this node as unmeasured", "error", err)
		}
		return
	}
	a.memory.mu.Lock()
	machines := a.memory.machines
	a.memory.mu.Unlock()

	p := model.NodePressure{
		SampledAt: time.Now().UTC().Format(time.RFC3339),
		Host:      roundPSI(host.PSI()),
	}
	for _, m := range machines {
		if m.Spec.NodeName != a.NodeName || m.Status.Phase != "Running" || m.Status.RuntimeID == "" || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		vm, err := a.Flux.GetPressure(ctx, m.Status.RuntimeID)
		if err != nil {
			a.log().Debug("host pressure: machine sample failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			continue
		}
		if p.Machines == nil {
			p.Machines = map[string]model.PSI{}
		}
		p.Machines[m.Namespace()+"/"+m.Metadata.Name] = roundPSI(vm.PSI())
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{model.AnnotationNodePressure: p.Encode()}}}
	if err := a.Kube.PatchNode(ctx, a.NodeName, patch); err != nil {
		a.log().Warn("host pressure: node annotation write failed", "node", a.NodeName, "error", err)
		return
	}
	a.Metrics.ObserveNodePSI(a.NodeName, p.Host.CPU, p.Host.Memory, p.Host.IO)
}

func roundPSI(p model.PSI) model.PSI {
	r := func(v float64) float64 { return math.Round(v*10) / 10 }
	return model.PSI{CPU: r(p.CPU), Memory: r(p.Memory), IO: r(p.IO)}
}
