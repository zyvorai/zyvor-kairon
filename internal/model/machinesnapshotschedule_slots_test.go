// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"testing"
	"time"
)

func dailySchedule(at string, jitter int, created time.Time) MachineSnapshotSchedule {
	return MachineSnapshotSchedule{
		Metadata: ObjectMeta{Name: "nightly", Namespace: "prod", CreationTimestamp: created},
		Spec:     MachineSnapshotScheduleSpec{Selector: map[string]string{"a": "b"}, DailyAt: at, JitterSeconds: jitter},
	}
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDailyAtDueAroundMidnight(t *testing.T) {
	s := dailySchedule("00:15", 0, utc("2026-10-06T10:00:00Z"))
	cases := []struct {
		lastRun, now string
		due          bool
	}{
		{"", "2026-10-06T23:59:00Z", false},
		{"", "2026-10-07T00:15:00Z", true},
		{"2026-10-07T00:15:03Z", "2026-10-07T12:00:00Z", false},
		{"2026-10-07T00:15:03Z", "2026-10-08T00:14:59Z", false},
		{"2026-10-07T00:15:03Z", "2026-10-08T00:15:00Z", true},
	}
	for _, tc := range cases {
		var last time.Time
		if tc.lastRun != "" {
			last = utc(tc.lastRun)
		}
		if got := s.Due(last, utc(tc.now)); got != tc.due {
			t.Errorf("lastRun=%q now=%s: Due=%v, want %v", tc.lastRun, tc.now, got, tc.due)
		}
	}
	if got := s.NextRunAfter(utc("2026-10-07T23:50:00Z")); !got.Equal(utc("2026-10-08T00:15:00Z")) {
		t.Fatalf("NextRunAfter just before midnight = %s", got)
	}
	if got := s.NextRunAfter(utc("2026-10-07T00:15:00Z")); !got.Equal(utc("2026-10-08T00:15:00Z")) {
		t.Fatalf("NextRunAfter exactly on the slot must be the next day, got %s", got)
	}
}

func TestDailyAtDeadline(t *testing.T) {
	s := dailySchedule("02:00", 0, utc("2026-10-01T00:00:00Z"))
	s.Spec.StartingDeadlineSeconds = 600
	last := utc("2026-10-06T02:00:01Z")
	if s.DeadlineExceeded(last, utc("2026-10-07T02:09:00Z")) {
		t.Fatal("9 minutes late is within a 10 minute deadline")
	}
	if !s.DeadlineExceeded(last, utc("2026-10-07T02:11:00Z")) {
		t.Fatal("11 minutes late exceeds a 10 minute deadline")
	}
}

func TestJitterOffsetStableAndBounded(t *testing.T) {
	s := dailySchedule("01:00", 900, time.Time{})
	a, b := s.JitterOffset(), s.JitterOffset()
	if a != b || a < 0 || a >= 900*time.Second {
		t.Fatalf("offset %s/%s not stable or out of range", a, b)
	}
	other := s
	other.Metadata.Name = "nightly-2"
	if other.JitterOffset() == a {
		t.Logf("two names hashed to the same offset; allowed but unlikely")
	}
	next := s.NextRunAfter(utc("2026-10-07T00:00:00Z"))
	if want := utc("2026-10-07T01:00:00Z").Add(a); !next.Equal(want) {
		t.Fatalf("NextRunAfter = %s, want %s", next, want)
	}
}

func TestIntervalWithJitterUsesFixedGrid(t *testing.T) {
	s := MachineSnapshotSchedule{
		Metadata: ObjectMeta{Name: "hourly", Namespace: "prod"},
		Spec:     MachineSnapshotScheduleSpec{IntervalSeconds: 3600, JitterSeconds: 600},
	}
	off := s.JitterOffset()
	next := s.NextRunAfter(utc("2026-10-07T10:30:00Z"))
	if next.Sub(time.Unix(0, 0).Add(off))%time.Hour != 0 {
		t.Fatalf("next run %s is not on the epoch+offset hourly grid (offset %s)", next, off)
	}
	if !next.After(utc("2026-10-07T10:30:00Z")) || next.Sub(utc("2026-10-07T10:30:00Z")) > time.Hour {
		t.Fatalf("next run %s not within one interval", next)
	}
	// A late fire must not push later runs off the grid.
	late := s.NextRunAfter(next.Add(7 * time.Second))
	if late.Sub(next) != time.Hour {
		t.Fatalf("grid drifted: %s then %s", next, late)
	}
}

func TestIntervalWithoutJitterKeepsLegacyBehavior(t *testing.T) {
	s := MachineSnapshotSchedule{Spec: MachineSnapshotScheduleSpec{IntervalSeconds: 3600}}
	if !s.Due(time.Time{}, utc("2026-10-07T10:00:00Z")) {
		t.Fatal("a never-run interval schedule is due immediately")
	}
	last := utc("2026-10-07T10:00:07Z")
	if got := s.NextRunAfter(last); !got.Equal(last.Add(time.Hour)) {
		t.Fatalf("NextRunAfter = %s", got)
	}
}

func TestSnapshotScheduleValidate(t *testing.T) {
	cases := []struct {
		name string
		spec MachineSnapshotScheduleSpec
		ok   bool
	}{
		{"interval", MachineSnapshotScheduleSpec{IntervalSeconds: 60}, true},
		{"daily", MachineSnapshotScheduleSpec{DailyAt: "23:59"}, true},
		{"neither", MachineSnapshotScheduleSpec{}, false},
		{"both", MachineSnapshotScheduleSpec{IntervalSeconds: 60, DailyAt: "01:00"}, false},
		{"bad time", MachineSnapshotScheduleSpec{DailyAt: "24:00"}, false},
		{"short time", MachineSnapshotScheduleSpec{DailyAt: "1:00"}, false},
		{"jitter too big", MachineSnapshotScheduleSpec{IntervalSeconds: 60, JitterSeconds: 60}, false},
		{"daily jitter", MachineSnapshotScheduleSpec{DailyAt: "01:00", JitterSeconds: 3600}, true},
	}
	for _, tc := range cases {
		if err := tc.spec.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestScheduledSnapshotsToPrune(t *testing.T) {
	now := utc("2026-10-07T12:00:00Z")
	snap := func(name string, age time.Duration) MachineSnapshot {
		return MachineSnapshot{Metadata: ObjectMeta{Name: name, CreationTimestamp: now.Add(-age)}}
	}
	snaps := []MachineSnapshot{snap("d3", 72*time.Hour), snap("d0", time.Hour), snap("d1", 24*time.Hour), snap("d2", 48*time.Hour)}
	names := func(in []MachineSnapshot) []string {
		out := make([]string, 0, len(in))
		for _, s := range in {
			out = append(out, s.Metadata.Name)
		}
		return out
	}
	if got := names(ScheduledSnapshotsToPrune(snaps, 0, 36*3600, now)); len(got) != 2 || got[0] != "d2" || got[1] != "d3" {
		t.Fatalf("maxAge 36h pruned %v, want [d2 d3]", got)
	}
	if got := names(ScheduledSnapshotsToPrune(snaps, 3, 0, now)); len(got) != 1 || got[0] != "d3" {
		t.Fatalf("keepLast 3 pruned %v, want [d3]", got)
	}
	if got := names(ScheduledSnapshotsToPrune(snaps, 3, 12*3600, now)); len(got) != 3 {
		t.Fatalf("keepLast 3 + maxAge 12h pruned %v, want 3", got)
	}
	old := []MachineSnapshot{snap("only", 1000*time.Hour)}
	if got := ScheduledSnapshotsToPrune(old, 0, 3600, now); len(got) != 0 {
		t.Fatalf("the newest snapshot must never be pruned, got %v", names(got))
	}
}
