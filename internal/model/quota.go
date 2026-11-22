// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

const KindMachineQuota = "MachineQuota"

// MachineQuota caps how many Machines, and how much total CPU/memory, a
// namespace may have scheduled at once -- Kairon's namespace-scoped
// equivalent of a Kubernetes ResourceQuota. Enforced by kairon-controller's
// existing scheduling loop (internal/controller): a Machine that would push
// its namespace over any MachineQuota it's subject to is left unscheduled
// (status.phase stays Pending with a clear message) rather than rejected at
// creation time -- no admission webhook exists or is needed, mirroring the
// migration concurrency quota's existing "block into Pending/Blocked, not a
// webhook" pattern. Already-scheduled Machines are never evicted
// retroactively if a quota is lowered; only new scheduling is blocked.
type MachineQuota struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta         `json:"metadata"`
	Spec     MachineQuotaSpec   `json:"spec"`
	Status   MachineQuotaStatus `json:"status,omitempty"`
}

func (q MachineQuota) Namespace() string {
	return q.Metadata.Namespace
}

type MachineQuotaList struct {
	TypeMeta `json:",inline"`
	Items    []MachineQuota `json:"items"`
}

type MachineQuotaSpec struct {
	// Each is optional; unset means "no cap on this dimension". At least
	// one should be set for the quota to do anything.
	MaxMachines    *int   `json:"maxMachines,omitempty"`
	MaxTotalCPU    string `json:"maxTotalCpu,omitempty"`
	MaxTotalMemory string `json:"maxTotalMemory,omitempty"`
}

// MachineQuotaStatus mirrors what a real Kubernetes ResourceQuota reports
// (status.used) -- written by kairon-controller every reconcile tick as a
// side effect of the same tallies it already computes to enforce the quota.
type MachineQuotaStatus struct {
	UsedMachines       int    `json:"usedMachines,omitempty"`
	UsedTotalCPUCores  uint32 `json:"usedTotalCpuCores,omitempty"`
	UsedTotalMemoryMiB uint64 `json:"usedTotalMemoryMiB,omitempty"`
}
