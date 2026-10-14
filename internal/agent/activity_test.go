// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func writeProcStat(t *testing.T, root string, pid int, utime, stime uint64) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("%d (qemu system x86) S 1 1 1 0 -1 4194560 100 0 0 0 %d %d 0 0 20 0 4 0 100 0 0\n", pid, utime, stime)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadProcCPUTicks(t *testing.T) {
	orig := procRoot
	procRoot = t.TempDir()
	t.Cleanup(func() { procRoot = orig })
	writeProcStat(t, procRoot, 42, 300, 25)
	got, err := readProcCPUTicks(42)
	if err != nil || got != 325 {
		t.Fatalf("ticks = %d, %v", got, err)
	}
	if _, err := readProcCPUTicks(0); err == nil {
		t.Fatal("pid 0 accepted")
	}
}

func TestSampleActive(t *testing.T) {
	t0 := time.Unix(1000, 0)
	base := activitySample{pid: 7, at: t0, cpuTicks: 1000, cpuOK: true, disk: 0, diskOK: true}
	cases := []struct {
		name string
		cur  activitySample
		want bool
	}{
		{"quiet", activitySample{pid: 7, at: t0.Add(10 * time.Second), cpuTicks: 1001, cpuOK: true, disk: 1024, diskOK: true}, false},
		{"cpu busy", activitySample{pid: 7, at: t0.Add(10 * time.Second), cpuTicks: 1100, cpuOK: true, disk: 0, diskOK: true}, true},
		{"disk busy", activitySample{pid: 7, at: t0.Add(10 * time.Second), cpuTicks: 1000, cpuOK: true, disk: 10 << 20, diskOK: true}, true},
		{"new process", activitySample{pid: 8, at: t0.Add(10 * time.Second)}, true},
	}
	for _, tc := range cases {
		if got := sampleActive(base, tc.cur, 50); got != tc.want {
			t.Errorf("%s: active=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestObserveActivityRateLimitsAndSeeds(t *testing.T) {
	orig := procRoot
	procRoot = t.TempDir()
	t.Cleanup(func() { procRoot = orig })
	a := &Agent{}
	m := model.Machine{Spec: model.MachineSpec{Lifecycle: &model.LifecycleSpec{IdleTimeoutSeconds: 300}}, Status: model.MachineStatus{Phase: "Running"}}
	rec := &fluxvm.Record{IDValue: "rt-1", PID: 9}
	t0 := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

	writeProcStat(t, procRoot, 9, 0, 0)
	st := model.MachineStatus{Phase: "Running"}
	a.observeActivity(m, rec, nil, &st, t0)
	if st.Lifecycle == nil || st.Lifecycle.LastActivityTime != t0.Format(time.RFC3339) {
		t.Fatalf("first sample did not seed: %+v", st.Lifecycle)
	}

	writeProcStat(t, procRoot, 9, 500, 0)
	a.observeActivity(m, rec, nil, &st, t0.Add(10*time.Second))
	if st.Lifecycle.LastActivityTime != t0.Format(time.RFC3339) {
		t.Fatalf("write inside the rate limit: %s", st.Lifecycle.LastActivityTime)
	}

	writeProcStat(t, procRoot, 9, 1000, 0)
	t1 := t0.Add(40 * time.Second)
	a.observeActivity(m, rec, nil, &st, t1)
	if st.Lifecycle.LastActivityTime != t1.Format(time.RFC3339) {
		t.Fatalf("busy sample after the interval not recorded: %s", st.Lifecycle.LastActivityTime)
	}

	t2 := t1.Add(5 * time.Minute)
	a.observeActivity(m, rec, nil, &st, t2)
	if st.Lifecycle.LastActivityTime != t1.Format(time.RFC3339) {
		t.Fatalf("idle sample moved the clock: %s", st.Lifecycle.LastActivityTime)
	}

	resumed := m
	resumed.Status.Phase = "Paused"
	t3 := t2.Add(time.Minute)
	a.observeActivity(resumed, rec, nil, &st, t3)
	if st.Lifecycle.LastActivityTime != t3.Format(time.RFC3339) {
		t.Fatalf("resume did not restart the idle clock: %s", st.Lifecycle.LastActivityTime)
	}

	plain := model.MachineStatus{Phase: "Running"}
	a.observeActivity(model.Machine{}, rec, nil, &plain, t2)
	if plain.Lifecycle != nil {
		t.Fatal("Machine without spec.lifecycle got status.lifecycle")
	}
}
