// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

const (
	sevCritical = "critical"
	sevWarning  = "warning"

	unschedulableAfter  = 5 * time.Minute
	migrationStuckAfter = 30 * time.Minute
	packedPercent       = 90
)

// insight is one deterministic finding for the dashboard's attention strip.
// Page and Target tell the UI where to take the operator; nothing here is
// model-generated.
type insight struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Page     string `json:"page"`
	Target   string `json:"target,omitempty"` // "namespace/name" for the page's route
}

// insightInput is everything computeInsights needs, already filtered to what
// the caller may see.
type insightInput struct {
	Machines   []model.Machine
	Migrations []model.MachineMigration
	Nodes      []model.Node // empty when the caller may not see nodes
	Quotas     []model.MachineQuota
}

func terminalMigrationPhase(p string) bool {
	switch p {
	case "", "Succeeded", "Completed", "Failed", "Cancelled", "Canceled":
		return true
	}
	return false
}

func percent(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return used / total * 100
}

// computeInsights applies the fixed rule set. It is a pure function of its
// input and the clock so every rule can be tested directly.
func computeInsights(in insightInput, now time.Time) []insight {
	var out []insight

	for _, m := range in.Machines {
		ref := m.Namespace() + "/" + m.Metadata.Name
		if m.Status.Phase == "Failed" {
			out = append(out, insight{
				ID: "machine-failed/" + ref, Severity: sevCritical,
				Title: fmt.Sprintf("%s is Failed", m.Metadata.Name), Detail: m.Status.Message,
				Page: "machines", Target: ref,
			})
		}
		for _, c := range m.Status.Conditions {
			if c.Type == "Scheduled" && c.Status == "False" && !c.LastTransitionTime.IsZero() && now.Sub(c.LastTransitionTime) >= unschedulableAfter {
				detail := c.Reason
				if c.Message != "" {
					detail += ": " + c.Message
				}
				out = append(out, insight{
					ID: "unschedulable/" + ref, Severity: sevWarning,
					Title:  fmt.Sprintf("%s has been unschedulable for %s", m.Metadata.Name, now.Sub(c.LastTransitionTime).Round(time.Minute)),
					Detail: detail, Page: "machines", Target: ref,
				})
			}
		}
	}

	for _, mig := range in.Migrations {
		ref := mig.Namespace() + "/" + mig.Metadata.Name
		switch {
		case mig.Status.Phase == "NeedsRecovery":
			out = append(out, insight{
				ID: "migration-recovery/" + ref, Severity: sevCritical,
				Title:  fmt.Sprintf("Migration %s needs recovery", mig.Metadata.Name),
				Detail: mig.Status.Message, Page: "migrations", Target: ref,
			})
		case !terminalMigrationPhase(mig.Status.Phase) && !mig.Metadata.CreationTimestamp.IsZero() && now.Sub(mig.Metadata.CreationTimestamp) >= migrationStuckAfter:
			out = append(out, insight{
				ID: "migration-stuck/" + ref, Severity: sevWarning,
				Title:  fmt.Sprintf("Migration %s has been %s for %s", mig.Metadata.Name, mig.Status.Phase, now.Sub(mig.Metadata.CreationTimestamp).Round(time.Minute)),
				Detail: mig.Status.Message, Page: "migrations", Target: ref,
			})
		}
	}

	if len(in.Nodes) > 0 {
		load := scheduler.BuildNodeLoad(in.Machines)
		for _, n := range in.Nodes {
			name := n.Metadata.Name
			l := load[name]
			ready := false
			for _, c := range n.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					ready = true
				}
			}
			if !ready && l.Count > 0 {
				out = append(out, insight{
					ID: "node-not-ready/" + name, Severity: sevCritical,
					Title: fmt.Sprintf("Node %s is not Ready with %d Machine(s) assigned", name, l.Count),
					Page:  "nodes", Target: name,
				})
			}
			cpu, mem, ok := scheduler.Allocatable(n)
			if !ok {
				continue
			}
			cpuPct, memPct := percent(float64(l.CPUMilli), float64(cpu)), percent(float64(l.MemoryMiB), float64(mem))
			if cpuPct >= packedPercent || memPct >= packedPercent {
				out = append(out, insight{
					ID: "node-packed/" + name, Severity: sevWarning,
					Title:  fmt.Sprintf("Node %s is nearly full", name),
					Detail: fmt.Sprintf("CPU %.0f%%, memory %.0f%% of allocatable is requested", cpuPct, memPct),
					Page:   "nodes", Target: name,
				})
			}
		}
	}

	for _, q := range in.Quotas {
		ref := q.Namespace() + "/" + q.Metadata.Name
		var hot []string
		if q.Spec.MaxMachines != nil && *q.Spec.MaxMachines > 0 && percent(float64(q.Status.UsedMachines), float64(*q.Spec.MaxMachines)) >= packedPercent {
			hot = append(hot, fmt.Sprintf("machines %d/%d", q.Status.UsedMachines, *q.Spec.MaxMachines))
		}
		if v, err := model.ParseVCPUs(q.Spec.MaxTotalCPU); err == nil && v > 0 && percent(float64(q.Status.UsedTotalCPUCores), float64(v)) >= packedPercent {
			hot = append(hot, fmt.Sprintf("cpu %d/%d cores", q.Status.UsedTotalCPUCores, v))
		}
		if v, err := model.ParseMemoryMiB(q.Spec.MaxTotalMemory); err == nil && v > 0 && percent(float64(q.Status.UsedTotalMemoryMiB), float64(v)) >= packedPercent {
			hot = append(hot, fmt.Sprintf("memory %d/%d MiB", q.Status.UsedTotalMemoryMiB, v))
		}
		if len(hot) > 0 {
			out = append(out, insight{
				ID: "quota-near/" + ref, Severity: sevWarning,
				Title:  fmt.Sprintf("Quota %s is nearly exhausted in %s", q.Metadata.Name, q.Namespace()),
				Detail: fmt.Sprint(hot), Page: "quotas", Target: ref,
			})
		}
	}

	rank := map[string]int{sevCritical: 0, sevWarning: 1}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Severity] != rank[out[j].Severity] {
			return rank[out[i].Severity] < rank[out[j].Severity]
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// handleInsights serves GET /api/v1/insights.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in insightInput
	machines, err := s.Kube.ListMachines(ctx)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	for _, m := range machines {
		if s.authorizedForNamespace(ctx, m.Namespace()) {
			in.Machines = append(in.Machines, m)
		}
	}
	migrations, err := s.Kube.ListMachineMigrations(ctx)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	for _, m := range migrations {
		if s.authorizedForNamespace(ctx, m.Namespace()) {
			in.Migrations = append(in.Migrations, m)
		}
	}
	// Quota and node findings are best-effort: an unlistable set must not
	// hide the Machine and migration findings above.
	if quotas, err := s.Kube.ListMachineQuotas(ctx); err == nil {
		for _, q := range quotas {
			if s.authorizedForNamespace(ctx, q.Namespace()) {
				in.Quotas = append(in.Quotas, q)
			}
		}
	}
	// Nodes are cluster-wide, so scoped non-admins never see node findings
	// (same rule as the /api/v1/nodes routes).
	if !s.NamespaceScopingEnabled || s.isAdminIdentity(ctx, usernameFromContext(ctx)) {
		if nodes, err := s.Kube.ListNodes(ctx); err == nil {
			in.Nodes = nodes
		}
	}
	out := computeInsights(in, time.Now())
	if out == nil {
		out = []insight{}
	}
	writeJSON(w, http.StatusOK, out)
}
