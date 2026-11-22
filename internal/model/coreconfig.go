// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import "time"

// ConfigMap is the minimal core/v1 ConfigMap shape Kairon needs -- not a
// general-purpose client. Used by internal/uiapi.Server to share session
// revocation, login-lockout, and console-ticket state across kairon-ui
// replicas (see internal/uiapi/sharedstate.go); Data is a plain string
// key/value map, merge-patched one key at a time so concurrent writers
// touching different keys never conflict.
type ConfigMap struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta        `json:"metadata"`
	Data     map[string]string `json:"data,omitempty"`
}

// Secret is the minimal core/v1 Secret shape Kairon needs to read back --
// not a general-purpose client. Data values arrive base64-encoded exactly
// as the Kubernetes API stores them; encoding/json decodes a []byte field
// from a base64 JSON string automatically, so callers never handle that
// themselves. Used by internal/uiapi.Server to notice a password change
// (or account list change) made by a different kairon-ui replica.
type Secret struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta        `json:"metadata"`
	Data     map[string][]byte `json:"data,omitempty"`
}

// Lease is the minimal coordination.k8s.io/v1 Lease shape Kairon needs for
// kairon-controller's own leader election (see internal/leaderelection) --
// not a general-purpose client. Pointer fields mirror the real API exactly
// (an absent field there is a meaningful "never held"/"unset", not a zero
// value) and round-trip through encoding/json's omitempty cleanly.
type Lease struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     LeaseSpec  `json:"spec,omitempty"`
}

type LeaseSpec struct {
	HolderIdentity       *string    `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32     `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          *MicroTime `json:"acquireTime,omitempty"`
	RenewTime            *MicroTime `json:"renewTime,omitempty"`
	LeaseTransitions     *int32     `json:"leaseTransitions,omitempty"`
}

// Event is the minimal core/v1 Event shape Kairon needs to record one --
// not a general-purpose client. Used by internal/kube.Client.RecordEvent
// (called from internal/controller's admitQuota blocker branch) so a
// quota-blocked Machine shows up in `kubectl describe machine`'s Events
// tab, not just status.message -- the same object kubectl/the dashboard
// already know how to render, rather than a Kairon-specific notification
// channel of its own.
type Event struct {
	TypeMeta       `json:",inline"`
	Metadata       ObjectMeta      `json:"metadata"`
	InvolvedObject ObjectReference `json:"involvedObject"`
	Reason         string          `json:"reason,omitempty"`
	Message        string          `json:"message,omitempty"`
	Source         EventSource     `json:"source,omitempty"`
	FirstTimestamp time.Time       `json:"firstTimestamp,omitempty"`
	LastTimestamp  time.Time       `json:"lastTimestamp,omitempty"`
	Count          int32           `json:"count,omitempty"`
	Type           string          `json:"type,omitempty"`
}

// ObjectReference is the minimal core/v1 ObjectReference shape Kairon
// needs -- just enough to populate Event.InvolvedObject.
type ObjectReference struct {
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// EventSource identifies the component that reported an Event.
type EventSource struct {
	Component string `json:"component,omitempty"`
}
