// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package facts reads the cluster state agentplane.Diagnose needs. It
// only reads; agentplane itself stays free of apiserver calls.
package facts

import (
	"context"
	"fmt"
	"strings"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/kube"
)

// maxEvents keeps the newest events so one noisy object cannot flood a
// diagnosis or a model prompt.
const maxEvents = 50

const labelTenant = agentplane.LabelTenant

// Subject is what Gather read: the facts plus the owning tenant (the
// Machine's tenant label, also for a migration) for scoping callers.
type Subject struct {
	Facts  agentplane.DiagnoseFacts
	Tenant string
}

// Gather reads one Machine or MachineMigration and its Events.
func Gather(ctx context.Context, kc *kube.Client, kind, namespace, name string) (Subject, error) {
	var s Subject
	f := &s.Facts
	f.Namespace, f.Name = namespace, name
	var eventKind string
	switch strings.ToLower(kind) {
	case "machine", "machines", "vm":
		m, err := kc.GetMachine(ctx, namespace, name)
		if err != nil {
			return s, err
		}
		f.Kind, eventKind = "Machine", "Machine"
		f.Phase, f.Message = m.Status.Phase, m.Status.Message
		for _, c := range m.Status.Conditions {
			f.Conditions = append(f.Conditions, agentplane.FactCondition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message})
		}
		s.Tenant = m.Metadata.Labels[labelTenant]
	case "migration", "migrations", "machinemigration":
		mig, err := kc.GetMachineMigration(ctx, namespace, name)
		if err != nil {
			return s, err
		}
		f.Kind, eventKind = "Migration", "MachineMigration"
		f.Phase, f.Message = mig.Status.Phase, mig.Status.Message
		s.Tenant = mig.Metadata.Labels[labelTenant]
		if s.Tenant == "" && mig.Spec.MachineName != "" {
			if m, err := kc.GetMachine(ctx, namespace, mig.Spec.MachineName); err == nil {
				s.Tenant = m.Metadata.Labels[labelTenant]
			}
		}
	default:
		return s, fmt.Errorf("kind must be machine or migration, not %q", kind)
	}
	events, err := kc.ListEventsFor(ctx, namespace, eventKind, name)
	if err != nil && !kube.IsNotFound(err) {
		return s, err
	}
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}
	for _, e := range events {
		f.Events = append(f.Events, agentplane.FactEvent{Type: e.Type, Reason: e.Reason, Message: e.Message, Count: int(e.Count)})
	}
	return s, nil
}

// ParseRef splits "machine/name" or "migration/name"; a bare name is a
// Machine.
func ParseRef(ref string) (kind, name string) {
	if k, n, ok := strings.Cut(ref, "/"); ok {
		return k, n
	}
	return "machine", ref
}
