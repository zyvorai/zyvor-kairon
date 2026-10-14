// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// clockTicks is USER_HZ, 100 on every Linux architecture Kairon targets.
const clockTicks = 100

// activeDiskBytesPerSecond is guest disk I/O above which a Machine is
// active regardless of CPU.
const activeDiskBytesPerSecond = 256 << 10

// activityRetention drops samples for runtimes not seen this long.
const activityRetention = 10 * time.Minute

// procRoot is /proc; tests point it at a fake tree.
var procRoot = "/proc"

type activitySample struct {
	pid      int
	at       time.Time
	cpuTicks uint64
	cpuOK    bool
	disk     uint64
	diskOK   bool
}

// observeActivity records guest activity for a spec.lifecycle Machine in
// status.lifecycle.lastActivityTime. CPU comes from the VMM process's own
// utime+stime (FluxVM's cpu_usage_percent is a lifetime average, useless
// for recent activity), disk from FluxVM's cumulative byte counters. The
// first sample of a runtime seeds the clock; writes are rate-limited to
// model.ActivityWriteInterval so an active Machine costs at most one
// status patch per interval.
func (a *Agent) observeActivity(m model.Machine, rec *fluxvm.Record, usage *fluxvm.Stats, status *model.MachineStatus, now time.Time) {
	a.pruneActivity(now)
	if m.Spec.Lifecycle == nil || m.Spec.Lifecycle.IdleTimeoutSeconds <= 0 || rec == nil || status.Phase != "Running" {
		return
	}
	cur := activitySample{pid: rec.PID, at: now}
	if ticks, err := readProcCPUTicks(rec.PID); err == nil {
		cur.cpuTicks, cur.cpuOK = ticks, true
	}
	if usage != nil {
		cur.disk, cur.diskOK = usage.DiskReadBytes+usage.DiskWriteBytes, true
	}
	prevAny, seen := a.activity.Swap(rec.ID(), cur)
	// Coming back to Running (resume, start, wake) restarts the idle clock.
	active := !seen || m.Status.Phase != "Running"
	if !active {
		active = sampleActive(prevAny.(activitySample), cur, m.Spec.Lifecycle.EffectiveIdleCPUMillicores())
	}
	if !active {
		return
	}
	if status.Lifecycle != nil {
		if last, err := time.Parse(time.RFC3339, status.Lifecycle.LastActivityTime); err == nil && now.Sub(last) < model.ActivityWriteInterval {
			return
		}
	}
	lc := model.LifecycleStatus{}
	if status.Lifecycle != nil {
		lc = *status.Lifecycle
	}
	lc.LastActivityTime = now.UTC().Format(time.RFC3339)
	status.Lifecycle = &lc
}

// sampleActive compares two samples of one runtime. A new VMM process
// (restart, resume from Hibernated) counts as activity.
func sampleActive(prev, cur activitySample, idleMillicores int64) bool {
	if prev.pid != cur.pid {
		return true
	}
	dt := cur.at.Sub(prev.at).Seconds()
	if dt <= 0 {
		return false
	}
	if prev.cpuOK && cur.cpuOK && cur.cpuTicks >= prev.cpuTicks {
		millicores := float64(cur.cpuTicks-prev.cpuTicks) / clockTicks * 1000 / dt
		if millicores >= float64(idleMillicores) {
			return true
		}
	}
	if prev.diskOK && cur.diskOK && cur.disk >= prev.disk {
		if float64(cur.disk-prev.disk)/dt >= activeDiskBytesPerSecond {
			return true
		}
	}
	return false
}

func (a *Agent) pruneActivity(now time.Time) {
	last := a.activityPrune.Load()
	if now.UnixNano()-last < int64(time.Minute) || !a.activityPrune.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	a.activity.Range(func(k, v any) bool {
		if now.Sub(v.(activitySample).at) > activityRetention {
			a.activity.Delete(k)
		}
		return true
	})
}

// readProcCPUTicks returns utime+stime of pid from /proc/PID/stat.
func readProcCPUTicks(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("no pid")
	}
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	// comm (field 2) may contain spaces; fields after the last ')' start
	// at field 3 (state), so utime (14) and stime (15) are [11] and [12].
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 {
		return 0, fmt.Errorf("short stat")
	}
	utime, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(f[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}
