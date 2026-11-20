// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"reflect"
	"time"
)

// FindCondition returns the first Condition of the given Type, if any.
func FindCondition(conditions []Condition, condType string) (Condition, bool) {
	for _, c := range conditions {
		if c.Type == condType {
			return c, true
		}
	}
	return Condition{}, false
}

// SetCondition upserts cond by Type, preserving other conditions in order.
// LastTransitionTime is bumped to now (UTC) only when Status, Reason, or
// Message actually change (or the type is new). Unchanged transitions keep
// their prior LastTransitionTime so the field means "when the condition
// changed," not "when we last reconciled."
func SetCondition(conditions []Condition, cond Condition) []Condition {
	prev, found := FindCondition(conditions, cond.Type)
	if found && prev.Status == cond.Status && prev.Reason == cond.Reason && prev.Message == cond.Message {
		cond.LastTransitionTime = prev.LastTransitionTime
	} else if cond.LastTransitionTime.IsZero() {
		cond.LastTransitionTime = time.Now().UTC()
	}
	out := make([]Condition, 0, len(conditions)+1)
	replaced := false
	for _, c := range conditions {
		if c.Type == cond.Type {
			out = append(out, cond)
			replaced = true
			continue
		}
		out = append(out, c)
	}
	if !replaced {
		out = append(out, cond)
	}
	return out
}

// MachineStatusEqualIgnoringVolatile reports whether a and b describe the
// same meaningful Machine status. ResourceUsage is excluded: it is high-
// frequency observational data that must not drive continuous etcd writes.
// Callers that want live usage should publish Prometheus metrics instead;
// kaironctl top / UI node-usage reflect the last meaningful status write.
func MachineStatusEqualIgnoringVolatile(a, b MachineStatus) bool {
	aCopy, bCopy := a, b
	aCopy.ResourceUsage = nil
	bCopy.ResourceUsage = nil
	return reflect.DeepEqual(aCopy, bCopy)
}
