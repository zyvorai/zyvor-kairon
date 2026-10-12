// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Node maintenance mode. An operator sets AnnotationMaintenance=true on a
// Node (kaironctl node maintenance enter); kairon-controller cordons it,
// migrates its Machines away, parks the ones with nowhere to go according
// to AnnotationMaintenanceFallback, and reports progress in
// AnnotationMaintenanceState/Message. Removing the annotation reverses
// what the controller did: it uncordons the node only if it cordoned it,
// and restores the power state of every Machine it parked.
// See docs/guides/node-maintenance-drs.md.
const (
	AnnotationMaintenance         = "kairon.zyvor.dev/maintenance"
	AnnotationMaintenanceFallback = "kairon.zyvor.dev/maintenance-fallback"
	AnnotationMaintenanceStrategy = "kairon.zyvor.dev/maintenance-strategy"
	// Written by kairon-controller.
	AnnotationMaintenanceState    = "kairon.zyvor.dev/maintenance-state"
	AnnotationMaintenanceMessage  = "kairon.zyvor.dev/maintenance-message"
	AnnotationMaintenanceCordoned = "kairon.zyvor.dev/maintenance-cordoned"
	// AnnotationMaintenanceParked is set on a Machine the controller
	// stopped or hibernated for maintenance; its value is a
	// MaintenanceParked JSON object.
	AnnotationMaintenanceParked = "kairon.zyvor.dev/maintenance-parked"
	// AnnotationMaintenanceAttemptedAt is the per-Machine retry cooldown
	// for maintenance migrations, RFC3339 UTC.
	AnnotationMaintenanceAttemptedAt = "kairon.zyvor.dev/maintenance-attempted-at"
)

// Values of AnnotationMaintenanceState.
const (
	MaintenanceStateEntering = "Entering"
	MaintenanceStateReady    = "InMaintenance"
)

// Values of AnnotationMaintenanceFallback: what happens to a Machine no
// other node can take.
const (
	MaintenanceFallbackWait      = "wait"
	MaintenanceFallbackStop      = "stop"
	MaintenanceFallbackHibernate = "hibernate"
)

// MaintenanceRequested reports whether n carries AnnotationMaintenance=true.
func MaintenanceRequested(n Node) bool {
	return strings.EqualFold(strings.TrimSpace(n.Metadata.Annotations[AnnotationMaintenance]), "true")
}

// MaintenanceFallback returns n's fallback, MaintenanceFallbackWait when
// unset, and an error for any other value.
func MaintenanceFallback(n Node) (string, error) {
	v := strings.ToLower(strings.TrimSpace(n.Metadata.Annotations[AnnotationMaintenanceFallback]))
	switch v {
	case "":
		return MaintenanceFallbackWait, nil
	case MaintenanceFallbackWait, MaintenanceFallbackStop, MaintenanceFallbackHibernate:
		return v, nil
	}
	return "", fmt.Errorf("%s %q: want wait|stop|hibernate", AnnotationMaintenanceFallback, v)
}

// MaintenanceStrategy returns n's migration strategy, live when unset.
func MaintenanceStrategy(n Node) (string, error) {
	v := strings.ToLower(strings.TrimSpace(n.Metadata.Annotations[AnnotationMaintenanceStrategy]))
	switch v {
	case "":
		return "live", nil
	case "live", "auto", "cold":
		return v, nil
	}
	return "", fmt.Errorf("%s %q: want live|auto|cold", AnnotationMaintenanceStrategy, v)
}

// MaintenanceParked records a power-state change maintenance made, so
// exiting maintenance restores exactly that change and nothing else.
type MaintenanceParked struct {
	Node string `json:"node"`
	From string `json:"from"`
	To   string `json:"to"`
}

// ParseMaintenanceParked decodes m's AnnotationMaintenanceParked.
func ParseMaintenanceParked(m Machine) (MaintenanceParked, bool) {
	raw, ok := m.Metadata.Annotations[AnnotationMaintenanceParked]
	if !ok || raw == "" {
		return MaintenanceParked{}, false
	}
	var p MaintenanceParked
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Node == "" || p.To == "" {
		return MaintenanceParked{}, false
	}
	return p, true
}

// Encode renders p as the AnnotationMaintenanceParked value.
func (p MaintenanceParked) Encode() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// DRS (automatic placement rebalancing). AnnotationDRS=disabled on a Machine
// keeps the rebalancer from ever moving it; AnnotationDRSMovedAt is the
// per-Machine cooldown the rebalancer writes when it moves one.
const (
	AnnotationDRS        = "kairon.zyvor.dev/drs"
	AnnotationDRSMovedAt = "kairon.zyvor.dev/drs-moved-at"
	// LabelDRSMigration marks MachineMigrations the rebalancer created.
	LabelDRSMigration = "kairon.zyvor.dev/drs"
)
