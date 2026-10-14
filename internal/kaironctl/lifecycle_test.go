// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestLifecycleSummary(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	m := model.Machine{
		Metadata: model.ObjectMeta{CreationTimestamp: now.Add(-time.Hour), Annotations: map[string]string{
			model.AnnotationLastActivity: now.Add(-2 * time.Minute).Format(time.RFC3339),
		}},
		Spec:   model.MachineSpec{Lifecycle: &model.LifecycleSpec{IdleTimeoutSeconds: 600, IdleAction: model.IdleActionHibernate, WakeOnConnect: true, MaxLifetimeSeconds: 7200}},
		Status: model.MachineStatus{Lifecycle: &model.LifecycleStatus{LastActivityTime: now.Add(-5 * time.Minute).Format(time.RFC3339)}},
	}
	want := "lifecycle: idle 2m0s of 10m0s, then Hibernate, wakes on connect, expires in 1h0m0s"
	if got := lifecycleSummary(m, now); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := lifecycleSummary(model.Machine{}, now); got != "" {
		t.Fatalf("no lifecycle: %q", got)
	}
}
