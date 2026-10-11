// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	defaultReclaimInterval = 10 * time.Second
	defaultPSISomeAvg10    = 10.0
	defaultStepPercent     = 25
	// minBalloonStepMiB is the smallest balloon change worth a QMP call.
	minBalloonStepMiB = 64
	// memoryStatusSlackMiB is how far the guest's balloon may drift from
	// status.memory before kairon-node rewrites it.
	memoryStatusSlackMiB = 64

	reasonHostPressure = "HostPressure"
	reasonRelief       = "Relief"
	reasonRestart      = "Restart"
)

// MemoryReclaimConfig tunes the elastic-memory reclaim loop. Zero values
// take the defaults.
type MemoryReclaimConfig struct {
	Disabled bool
	// LowWatermarkMiB: host MemAvailable below this is pressure. Default
	// 10% of MemTotal.
	LowWatermarkMiB uint64
	// HighWatermarkMiB: reclaim aims for this much MemAvailable, and
	// balloons deflate only above it. Default 20% of MemTotal.
	HighWatermarkMiB uint64
	// PSISomeAvg10: memory PSI some avg10 above this is pressure. Default 10.
	PSISomeAvg10 float64
	// StepPercent caps each change at this share of a Machine's
	// reclaimable memory. Default 25.
	StepPercent uint64
	// Interval between reclaim passes. Default 10s.
	Interval time.Duration
}

func (c MemoryReclaimConfig) watermarks(totalMiB uint64) (low, high uint64) {
	low, high = c.LowWatermarkMiB, c.HighWatermarkMiB
	if low == 0 {
		low = 1024
		if totalMiB > 0 {
			low = totalMiB / 10
		}
	}
	if high == 0 {
		high = 2 * low
		if totalMiB > 0 && c.LowWatermarkMiB == 0 {
			high = totalMiB / 5
		}
	}
	if high < low {
		high = low
	}
	return low, high
}

func (c MemoryReclaimConfig) psiLimit() float64 {
	if c.PSISomeAvg10 > 0 {
		return c.PSISomeAvg10
	}
	return defaultPSISomeAvg10
}

func (c MemoryReclaimConfig) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return defaultReclaimInterval
}

// balloonVM is one elastic-memory Machine as the planner sees it.
type balloonVM struct {
	Key        string
	CurrentMiB uint64
	MinMiB     uint64
	TargetMiB  uint64
}

func (v balloonVM) reclaimable() uint64 {
	if v.CurrentMiB <= v.MinMiB {
		return 0
	}
	return v.CurrentMiB - v.MinMiB
}

func (v balloonVM) headroom() uint64 {
	r := v.reclaimable()
	if v.TargetMiB >= r {
		return 0
	}
	return r - v.TargetMiB
}

func (v balloonVM) step(percent uint64) uint64 {
	s := v.reclaimable() * percent / 100
	if s < minBalloonStepMiB {
		s = minBalloonStepMiB
	}
	return s
}

// reclaimPlan is one reclaim pass's decision.
type reclaimPlan struct {
	// Pressure: MemAvailable below the low watermark or PSI above the limit.
	Pressure bool
	// Exhausted: under pressure with every balloon already at its minimum.
	Exhausted bool
	// Targets holds only the balloon targets that change.
	Targets map[string]uint64
	Reason  string
	Message string
}

// planReclaim decides balloon targets from one host memory sample. Under
// pressure it inflates balloons toward the high watermark, in proportion
// to each Machine's remaining headroom and at most one step each. Above
// the high watermark with calm PSI it deflates, at most one step each and
// no more in total than the memory above the watermark. A target above a
// Machine's reclaimable memory (min was raised) is always clamped.
func planReclaim(c MemoryReclaimConfig, host fluxvm.HostMemory, vms []balloonVM) reclaimPlan {
	plan := reclaimPlan{Targets: map[string]uint64{}}
	if host.AvailableMiB == nil {
		return plan
	}
	avail := *host.AvailableMiB
	var total uint64
	if host.TotalMiB != nil {
		total = *host.TotalMiB
	}
	low, high := c.watermarks(total)
	psiMax := c.psiLimit()
	psiHot := host.PSISomeAvg10 != nil && *host.PSISomeAvg10 > psiMax
	psiCalm := host.PSISomeAvg10 == nil || *host.PSISomeAvg10 <= psiMax/2
	plan.Pressure = avail < low || psiHot
	percent := c.StepPercent
	if percent == 0 || percent > 100 {
		percent = defaultStepPercent
	}

	sorted := append([]balloonVM(nil), vms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	for _, v := range sorted {
		if r := v.reclaimable(); v.TargetMiB > r {
			plan.Targets[v.Key] = r
		}
	}

	switch {
	case plan.Pressure:
		plan.Reason = reasonHostPressure
		var totalHeadroom uint64
		for _, v := range sorted {
			totalHeadroom += v.headroom()
		}
		if totalHeadroom == 0 {
			plan.Exhausted = true
			plan.Message = fmt.Sprintf("host has %dMiB available (low watermark %dMiB) and every elastic-memory Machine is at its minimum", avail, low)
			return plan
		}
		need := uint64(0)
		if avail < high {
			need = high - avail
		}
		for _, v := range sorted {
			h := v.headroom()
			if h == 0 {
				continue
			}
			inc := min(h, v.step(percent))
			if need > 0 {
				share := (need*h + totalHeadroom - 1) / totalHeadroom
				inc = min(inc, max(share, minBalloonStepMiB))
				inc = min(inc, h)
			}
			plan.Targets[v.Key] = v.TargetMiB + inc
		}
		plan.Message = fmt.Sprintf("host has %dMiB available (low watermark %dMiB); inflating balloons toward %dMiB", avail, low, high)
	case avail > high && psiCalm:
		plan.Reason = reasonRelief
		budget := avail - high
		for _, v := range sorted {
			if budget == 0 {
				break
			}
			target := v.TargetMiB
			if t, ok := plan.Targets[v.Key]; ok {
				target = t
			}
			if target == 0 {
				continue
			}
			dec := min(target, v.step(percent), budget)
			plan.Targets[v.Key] = target - dec
			budget -= dec
		}
	}
	return plan
}

// memoryState is the reclaim loop's memory between passes. Reconcile
// stores the Machines it listed; runMemoryReclaim reads them from its own
// goroutine.
type memoryState struct {
	mu        sync.Mutex
	at        time.Time
	machines  []model.Machine
	condition string
	// written is the status.memory last patched per Machine. The listed
	// Machines can be a resync old, so they would hide our own writes.
	written map[string]model.MemoryStatus
}

// lastStatus is the newest status.memory known for m: the one this loop
// wrote, else the listed one.
func (s *memoryState) lastStatus(key string, listed *model.MemoryStatus) *model.MemoryStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.written[key]; ok {
		return &w
	}
	return listed
}

func (s *memoryState) recordStatus(key string, st model.MemoryStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written == nil {
		s.written = map[string]model.MemoryStatus{}
	}
	s.written[key] = st
}

// forget drops written statuses of Machines no longer elastic and running
// here.
func (s *memoryState) forget(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.written {
		if !keep[k] {
			delete(s.written, k)
		}
	}
}

func (s *memoryState) setMachines(ms []model.Machine) {
	s.mu.Lock()
	s.machines = ms
	s.mu.Unlock()
}

// elasticMachine pairs a Machine with its FluxVM balloon report.
type elasticMachine struct {
	m      model.Machine
	report *fluxvm.VMMemoryReport
	vm     balloonVM
}

// runMemoryReclaim runs reclaim passes on their own ticker: Run's resync
// interval is minutes, far too slow for host memory pressure.
func (a *Agent) runMemoryReclaim(ctx context.Context) {
	if a.MemoryReclaim.Disabled {
		return
	}
	t := time.NewTicker(a.MemoryReclaim.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.reclaimMemory(ctx)
		}
	}
}

// reclaimMemory runs one reclaim pass when due: it samples host memory,
// moves balloon targets of this node's running elastic-memory Machines,
// records status.memory, and sets the node's KaironMemoryPressure
// condition while pressure persists with nothing left to reclaim.
func (a *Agent) reclaimMemory(ctx context.Context) {
	if a.MemoryReclaim.Disabled || a.Kube == nil || a.Flux == nil || a.NodeName == "" {
		return
	}
	s := &a.memory
	s.mu.Lock()
	due := time.Since(s.at) >= a.MemoryReclaim.interval()
	if due {
		s.at = time.Now()
	}
	machines := s.machines
	s.mu.Unlock()
	if !due {
		return
	}
	caps, err := a.Flux.GetRuntimeCapabilities(ctx)
	if err != nil || !caps.Host.Balloon {
		return
	}
	host, err := a.Flux.GetHostMemory(ctx)
	if err != nil || host.AvailableMiB == nil {
		if err != nil {
			a.log().Debug("FluxVM host memory read failed", "error", err)
		}
		return
	}

	var elastic []elasticMachine
	for _, m := range machines {
		minMiB, ok := m.Spec.ElasticMinMiB()
		if !ok || m.Spec.NodeName != a.NodeName || m.Metadata.DeletionTimestamp != nil ||
			m.Status.Phase != "Running" || m.Status.RuntimeID == "" {
			continue
		}
		report, err := a.Flux.GetMemoryReport(ctx, m.Status.RuntimeID)
		if err != nil || report.Balloon == nil {
			a.log().Debug("elastic memory: no balloon report", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			continue
		}
		cur := report.Balloon.MemoryMiB
		elastic = append(elastic, elasticMachine{m: m, report: report, vm: balloonVM{
			Key: m.Namespace() + "/" + m.Metadata.Name, CurrentMiB: cur, MinMiB: min(minMiB, cur), TargetMiB: report.Balloon.TargetMiB,
		}})
	}
	vms := make([]balloonVM, 0, len(elastic))
	keep := make(map[string]bool, len(elastic))
	for _, e := range elastic {
		vms = append(vms, e.vm)
		keep[e.vm.Key] = true
	}
	s.forget(keep)
	plan := planReclaim(a.MemoryReclaim, *host, vms)

	for _, e := range elastic {
		target, actual, reason := e.vm.TargetMiB, e.report.Balloon.ActualMiB, ""
		if t, ok := plan.Targets[e.vm.Key]; ok && t != target {
			st, err := a.Flux.SetBalloon(ctx, e.m.Status.RuntimeID, t)
			if err != nil {
				a.log().Warn("elastic memory: balloon change failed", "namespace", e.m.Namespace(), "machine", e.m.Metadata.Name, "target", t, "error", err)
			} else {
				direction := "inflate"
				if t < target {
					direction = "deflate"
				}
				a.Metrics.ObserveReclaimAction(direction)
				a.log().Info("elastic memory: balloon moved", "namespace", e.m.Namespace(), "machine", e.m.Metadata.Name, "from", target, "to", t, "reason", plan.Reason)
				target, actual, reason = t, st.ActualMiB, plan.Reason
			}
		}
		a.Metrics.ObserveMachineBalloon(e.m.Namespace(), e.m.Metadata.Name, target, actual)
		a.writeMemoryStatus(ctx, e, target, actual, reason)
	}

	a.Metrics.ObserveNodeMemoryPressure(a.NodeName, plan.Pressure, plan.Exhausted)
	a.setMemoryPressureCondition(ctx, plan.Pressure && plan.Exhausted, plan.Message)
}

// writeMemoryStatus patches status.memory when the target changed, the
// guest drifted, or the field is missing.
func (a *Agent) writeMemoryStatus(ctx context.Context, e elasticMachine, target, actual uint64, reason string) {
	prev := a.memory.lastStatus(e.vm.Key, e.m.Status.Memory)
	next := model.MemoryStatus{
		CurrentMiB: e.vm.CurrentMiB, MinMiB: e.vm.MinMiB,
		BalloonTargetMiB: target, BalloonActualMiB: actual,
	}
	if g := e.report.Guest; g != nil && g.AvailableMiB != nil {
		v := *g.AvailableMiB
		next.GuestAvailableMiB = &v
	}
	now := time.Now().UTC().Format(time.RFC3339)
	switch {
	case reason != "":
		next.Reason, next.LastChangeTime = reason, now
	case prev != nil && prev.BalloonTargetMiB != target:
		// FluxVM reset the balloon: the VM was relaunched.
		next.Reason, next.LastChangeTime = reasonRestart, now
	case prev != nil:
		next.Reason, next.LastChangeTime = prev.Reason, prev.LastChangeTime
	}
	if prev != nil && reason == "" && prev.BalloonTargetMiB == target && prev.CurrentMiB == next.CurrentMiB &&
		prev.MinMiB == next.MinMiB && absDiff(prev.BalloonActualMiB, actual) < memoryStatusSlackMiB {
		return
	}
	if err := a.Kube.PatchMachineStatusFields(ctx, e.m.Namespace(), e.m.Metadata.Name, map[string]any{"memory": next}); err != nil {
		a.log().Warn("elastic memory: status write failed", "namespace", e.m.Namespace(), "machine", e.m.Metadata.Name, "error", err)
		return
	}
	a.memory.recordStatus(e.vm.Key, next)
}

// setMemoryPressureCondition writes the node's KaironMemoryPressure
// condition when it changes (and once after kairon-node starts).
func (a *Agent) setMemoryPressureCondition(ctx context.Context, on bool, message string) {
	status, reason := "False", "MemoryAvailable"
	if on {
		status, reason = "True", "ReclaimExhausted"
	} else {
		message = "host memory is above the low watermark or elastic-memory Machines can still be reclaimed"
	}
	s := &a.memory
	s.mu.Lock()
	unchanged := s.condition == status
	s.mu.Unlock()
	if unchanged {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	cond := model.NodeCondition{
		Type: model.NodeConditionKaironMemoryPressure, Status: status, Reason: reason, Message: message,
		LastHeartbeatTime: now, LastTransitionTime: now,
	}
	if err := a.Kube.PatchNodeCondition(ctx, a.NodeName, cond); err != nil {
		a.log().Warn("elastic memory: node condition write failed", "node", a.NodeName, "error", err)
		return
	}
	if on {
		a.log().Warn("host memory pressure persists with every balloon at its minimum", "node", a.NodeName, "message", message)
	}
	s.mu.Lock()
	s.condition = status
	s.mu.Unlock()
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}
