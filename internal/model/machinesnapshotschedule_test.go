// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"testing"
	"time"
)

func TestMachineSnapshotScheduleSpecDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		spec    MachineSnapshotScheduleSpec
		lastRun time.Time
		want    bool
	}{
		{
			name:    "never run yet is immediately due",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 3600},
			lastRun: time.Time{},
			want:    true,
		},
		{
			name:    "not yet due",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 3600},
			lastRun: now.Add(-30 * time.Minute),
			want:    false,
		},
		{
			name:    "exactly at the interval boundary is due",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 3600},
			lastRun: now.Add(-1 * time.Hour),
			want:    true,
		},
		{
			name:    "well past the interval is due",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60},
			lastRun: now.Add(-24 * time.Hour),
			want:    true,
		},
		{
			name:    "suspended never-run-yet is not due",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, Suspend: true},
			lastRun: time.Time{},
			want:    false,
		},
		{
			name:    "suspended long-overdue is still not due",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, Suspend: true},
			lastRun: now.Add(-24 * time.Hour),
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.Due(tc.lastRun, now); got != tc.want {
				t.Errorf("Due(%v, %v) = %v, want %v", tc.lastRun, now, got, tc.want)
			}
		})
	}
}

func TestMachineSnapshotScheduleSpecDeadlineExceeded(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		spec    MachineSnapshotScheduleSpec
		lastRun time.Time
		want    bool
	}{
		{
			name:    "unset deadline never exceeded even when very overdue",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60},
			lastRun: now.Add(-24 * time.Hour),
			want:    false,
		},
		{
			name:    "never run yet is never a missed deadline",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, StartingDeadlineSeconds: 60},
			lastRun: time.Time{},
			want:    false,
		},
		{
			name: "due window still within the deadline",
			// due at now-30s (interval 60s, lastRun 90s ago) -- 30s late, deadline 60s -- on time.
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, StartingDeadlineSeconds: 60},
			lastRun: now.Add(-90 * time.Second),
			want:    false,
		},
		{
			name: "exactly at the deadline boundary is still on time",
			// due at now-60s (interval 60s, lastRun 120s ago) -- exactly 60s late, deadline 60s.
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, StartingDeadlineSeconds: 60},
			lastRun: now.Add(-120 * time.Second),
			want:    false,
		},
		{
			name: "one second past the deadline boundary is exceeded",
			// due at now-61s (interval 60s, lastRun 121s ago) -- 61s late, deadline 60s.
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, StartingDeadlineSeconds: 60},
			lastRun: now.Add(-121 * time.Second),
			want:    true,
		},
		{
			name:    "far overdue past the deadline is exceeded",
			spec:    MachineSnapshotScheduleSpec{IntervalSeconds: 60, StartingDeadlineSeconds: 300},
			lastRun: now.Add(-24 * time.Hour),
			want:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.DeadlineExceeded(tc.lastRun, now); got != tc.want {
				t.Errorf("DeadlineExceeded(%v, %v) = %v, want %v", tc.lastRun, now, got, tc.want)
			}
		})
	}
}

func TestTriggerNowRequested(t *testing.T) {
	tests := []struct {
		name            string
		annotationValue string
		lastHandled     string
		want            bool
	}{
		{
			name:            "no annotation at all is never a request",
			annotationValue: "",
			lastHandled:     "",
			want:            false,
		},
		{
			name:            "annotation set, never handled before is a request",
			annotationValue: "2026-01-01T12:00:00Z",
			lastHandled:     "",
			want:            true,
		},
		{
			name:            "annotation matches the last handled value is already handled",
			annotationValue: "2026-01-01T12:00:00Z",
			lastHandled:     "2026-01-01T12:00:00Z",
			want:            false,
		},
		{
			name:            "annotation changed to a new value after a previous one was handled is a new request",
			annotationValue: "2026-01-02T09:00:00Z",
			lastHandled:     "2026-01-01T12:00:00Z",
			want:            true,
		},
		{
			name:            "annotation removed after being handled is never a request",
			annotationValue: "",
			lastHandled:     "2026-01-01T12:00:00Z",
			want:            false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TriggerNowRequested(tc.annotationValue, tc.lastHandled); got != tc.want {
				t.Errorf("TriggerNowRequested(%q, %q) = %v, want %v", tc.annotationValue, tc.lastHandled, got, tc.want)
			}
		})
	}
}

func TestMachineSnapshotScheduleSpecNextRunAfter(t *testing.T) {
	firedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		interval int
		want     time.Time
	}{
		{
			name:     "one hour interval projects one hour past the firing time",
			interval: 3600,
			want:     firedAt.Add(time.Hour),
		},
		{
			name:     "one minute interval projects one minute past the firing time",
			interval: 60,
			want:     firedAt.Add(time.Minute),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := MachineSnapshotScheduleSpec{IntervalSeconds: tc.interval}
			if got := spec.NextRunAfter(firedAt); !got.Equal(tc.want) {
				t.Errorf("NextRunAfter(%v) = %v, want %v", firedAt, got, tc.want)
			}
		})
	}
}
