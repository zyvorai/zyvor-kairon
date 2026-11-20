// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"encoding/json"
	"time"
)

const FleetAPIVersion = "fleet.kairon.zyvor.dev/v1alpha1"

// FleetKinds is the complete REST allowlist. Fleet uses a separate API group
// so adding it does not change the existing Machine wire contract.
var FleetKinds = map[string]string{
	"machinehaprofiles":       "MachineHAProfile",
	"nodefencerequests":       "NodeFenceRequest",
	"machineactionapprovals":  "MachineActionApproval",
	"machinebalancepolicies":  "MachineBalancePolicy",
	"machineautoscalers":      "MachineAutoscaler",
	"machinerecoveryplans":    "MachineRecoveryPlan",
	"machinebackupgroups":     "MachineBackupGroup",
	"machineimportplans":      "MachineImportPlan",
	"machinetemplateversions": "MachineTemplateVersion",
	"machinetemplateclaims":   "MachineTemplateClaim",
	"machinevirtualnetworks":  "MachineVirtualNetwork",
	"machinenetworkclaims":    "MachineNetworkClaim",
	"machineusageledgers":     "MachineUsageLedger",
}

// FleetResource is the common Kubernetes envelope; specs are decoded into
// typed, validated contracts by internal/fleet before any side effect.
type FleetResource struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta      `json:"metadata"`
	Spec     json.RawMessage `json:"spec"`
	Status   FleetStatus     `json:"status,omitempty"`
}

type FleetChild struct {
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

type FleetStatus struct {
	ObservedGeneration  int64              `json:"observedGeneration"`
	Phase               string             `json:"phase"`
	Message             string             `json:"message"`
	LastActionTime      *time.Time         `json:"lastActionTime,omitempty"`
	LastSampleTime      *time.Time         `json:"lastSampleTime,omitempty"`
	RecommendationSince *time.Time         `json:"recommendationSince,omitempty"`
	DesiredReplicas     int                `json:"desiredReplicas,omitempty"`
	Children            []FleetChild       `json:"children,omitempty"`
	Completed           []string           `json:"completed,omitempty"`
	Allocations         map[string]string  `json:"allocations,omitempty"`
	Totals              map[string]float64 `json:"totals,omitempty"`
	Watermark           *time.Time         `json:"watermark,omitempty"`
	RecoveryStartedAt   *time.Time         `json:"recoveryStartedAt,omitempty"`
	RecoverySeconds     float64            `json:"recoverySeconds,omitempty"`
	DataAgeSeconds      float64            `json:"dataAgeSeconds,omitempty"`
}
