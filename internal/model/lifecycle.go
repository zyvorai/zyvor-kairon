// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"time"
)

const (
	// AnnotationLastActivity (RFC3339) is touched by kairon-ui while a
	// relay session (ssh, port-forward, exec, files, console, sandbox
	// HTTP) is using the Machine, so an interactive session counts as
	// activity for spec.lifecycle even when the guest itself is quiet.
	AnnotationLastActivity = "kairon.zyvor.dev/last-activity"
	// AnnotationIdleSuspended (RFC3339) records that kairon-controller
	// applied spec.lifecycle.idleAction. Wake-on-connect and kaironctl
	// start clear it.
	AnnotationIdleSuspended = "kairon.zyvor.dev/idle-suspended"
	// LabelSandbox=true marks an agent sandbox; with kairon-ui's
	// sandboxOperatorExec, non-admin operators may exec into it.
	LabelSandbox = "kairon.zyvor.dev/sandbox"

	IdleActionPause     = "Pause"
	IdleActionHibernate = "Hibernate"
	IdleActionHalt      = "Halt"
	IdleActionStop      = "Stop"
	IdleActionDelete    = "Delete"

	// DefaultIdleCPUMillicores is guest CPU use (thousandths of one core,
	// averaged over a sample window) below which a Machine counts as idle.
	DefaultIdleCPUMillicores = 50
	// DefaultWakeTimeoutSeconds bounds how long a wake-on-connect request
	// waits for the Machine to be Running.
	DefaultWakeTimeoutSeconds = 60
	// MinIdleTimeoutSeconds keeps idle detection above the activity
	// sampling granularity.
	MinIdleTimeoutSeconds = 60
)

// LifecycleSpec bounds how long a Machine lives and what happens when it
// sits idle. Every field is opt-in; a Machine without it is unaffected.
type LifecycleSpec struct {
	// MaxLifetimeSeconds deletes the Machine this long after creation.
	// Unlike spec.ttlSeconds (a FluxVM runtime hint), kairon-controller
	// deletes the Machine object itself.
	MaxLifetimeSeconds int64 `json:"maxLifetimeSeconds,omitempty"`
	// IdleTimeoutSeconds applies IdleAction once the Machine has shown no
	// activity (guest CPU, disk I/O, relay sessions) for this long.
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds,omitempty"`
	// IdleAction is Pause (default), Hibernate, Halt, Stop or Delete.
	IdleAction string `json:"idleAction,omitempty"`
	// IdleCPUMillicores is the guest CPU use below which the Machine
	// counts as idle; 0 means DefaultIdleCPUMillicores.
	IdleCPUMillicores int64 `json:"idleCPUMillicores,omitempty"`
	// WakeOnConnect makes kairon-ui power a Paused, Hibernated or Halted
	// Machine back on when a relay session (ssh, port-forward, exec,
	// files, console, sandbox HTTP) asks for it.
	WakeOnConnect bool `json:"wakeOnConnect,omitempty"`
	// WakeTimeoutSeconds bounds a wake; 0 means DefaultWakeTimeoutSeconds.
	WakeTimeoutSeconds int64 `json:"wakeTimeoutSeconds,omitempty"`
}

// LifecycleStatus is written by kairon-node for spec.lifecycle Machines.
type LifecycleStatus struct {
	// LastActivityTime (RFC3339) is the last time kairon-node saw guest
	// activity. Written at most once per ActivityWriteInterval.
	LastActivityTime string `json:"lastActivityTime,omitempty"`
}

// ActivityWriteInterval rate-limits LastActivityTime status writes.
const ActivityWriteInterval = 30 * time.Second

func (l LifecycleSpec) EffectiveIdleAction() string {
	if l.IdleAction == "" {
		return IdleActionPause
	}
	return l.IdleAction
}

func (l LifecycleSpec) EffectiveIdleCPUMillicores() int64 {
	if l.IdleCPUMillicores <= 0 {
		return DefaultIdleCPUMillicores
	}
	return l.IdleCPUMillicores
}

func (l LifecycleSpec) WakeTimeout() time.Duration {
	if l.WakeTimeoutSeconds <= 0 {
		return DefaultWakeTimeoutSeconds * time.Second
	}
	return time.Duration(l.WakeTimeoutSeconds) * time.Second
}

// IdlePowerState is the spec.powerState IdleAction sets, or "" for Delete.
func (l LifecycleSpec) IdlePowerState() string {
	switch l.EffectiveIdleAction() {
	case IdleActionHibernate:
		return "Hibernated"
	case IdleActionHalt:
		return "Halted"
	case IdleActionStop:
		return "Stopped"
	case IdleActionDelete:
		return ""
	default:
		return "Paused"
	}
}

// ValidateLifecycle rejects values kairon-controller cannot act on.
func ValidateLifecycle(l *LifecycleSpec) error {
	if l == nil {
		return nil
	}
	if l.MaxLifetimeSeconds < 0 || l.IdleTimeoutSeconds < 0 || l.IdleCPUMillicores < 0 || l.WakeTimeoutSeconds < 0 {
		return fmt.Errorf("spec.lifecycle: values must not be negative")
	}
	if l.IdleTimeoutSeconds > 0 && l.IdleTimeoutSeconds < MinIdleTimeoutSeconds {
		return fmt.Errorf("spec.lifecycle.idleTimeoutSeconds must be at least %d", MinIdleTimeoutSeconds)
	}
	switch l.IdleAction {
	case "", IdleActionPause, IdleActionHibernate, IdleActionHalt, IdleActionStop, IdleActionDelete:
	default:
		return fmt.Errorf("spec.lifecycle.idleAction %q: want Pause, Hibernate, Halt, Stop or Delete", l.IdleAction)
	}
	return nil
}

// Suspended reports whether phase is one wake-on-connect can resume.
func Suspended(phase string) bool {
	switch phase {
	case "Paused", "Hibernated", "Halted":
		return true
	}
	return false
}
