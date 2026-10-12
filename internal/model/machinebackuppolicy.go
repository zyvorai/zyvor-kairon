// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"sort"
	"time"
)

const KindMachineBackupPolicy = "MachineBackupPolicy"

// MachineBackupPolicy takes scheduled CBT backups of every Machine matching
// Selector: a full every FullEvery, incrementals every IncrementalEvery in
// between, and prunes them by grandfather-father-son retention. A backup
// that a kept one still depends on is only deleted once a synthetic full
// has replaced it, which the policy requests itself.
type MachineBackupPolicy struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                `json:"metadata"`
	Spec     MachineBackupPolicySpec   `json:"spec"`
	Status   MachineBackupPolicyStatus `json:"status,omitempty"`
}

func (p MachineBackupPolicy) Namespace() string { return p.Metadata.Namespace }

type MachineBackupPolicyList struct {
	TypeMeta `json:",inline"`
	Items    []MachineBackupPolicy `json:"items"`
}

type MachineBackupPolicySpec struct {
	// Selector matches Machines in the policy's namespace; empty matches
	// nothing.
	Selector map[string]string `json:"selector"`
	// Mode is incremental (the default) or full (every run is a full).
	Mode string `json:"mode,omitempty"`
	// FullEvery starts a new chain this often (default 168h).
	FullEvery string `json:"fullEvery,omitempty"`
	// IncrementalEvery is the backup interval, e.g. "1h".
	IncrementalEvery string            `json:"incrementalEvery"`
	Target           BackupTarget      `json:"target"`
	Compression      string            `json:"compression,omitempty"`
	Encryption       *BackupEncryption `json:"encryption,omitempty"`
	ImmutableFor     string            `json:"immutableFor,omitempty"`
	Quiesce          string            `json:"quiesce,omitempty"`
	Retention        BackupRetention   `json:"retention,omitempty"`
	Suspend          bool              `json:"suspend,omitempty"`
}

// BackupRetention keeps the newest backup of each of the last Hourly hours,
// Daily days and Monthly months (UTC). The newest backup is always kept.
// All zero keeps everything.
type BackupRetention struct {
	Hourly  int `json:"hourly,omitempty"`
	Daily   int `json:"daily,omitempty"`
	Monthly int `json:"monthly,omitempty"`
}

// Window is how far back the longest GFS tier reaches.
func (r BackupRetention) Window() time.Duration {
	return max(time.Duration(r.Hourly)*time.Hour, time.Duration(r.Daily)*24*time.Hour, time.Duration(r.Monthly)*31*24*time.Hour)
}

type MachineBackupPolicyStatus struct {
	Machines []PolicyMachineStatus `json:"machines,omitempty"`
}

type PolicyMachineStatus struct {
	MachineName     string     `json:"machineName"`
	ChainID         string     `json:"chainID,omitempty"`
	LastFull        *time.Time `json:"lastFull,omitempty"`
	LastIncremental *time.Time `json:"lastIncremental,omitempty"`
	NextRun         *time.Time `json:"nextRun,omitempty"`
	// Skipped says why the policy does not back this Machine up.
	Skipped   string `json:"skipped,omitempty"`
	LastError string `json:"lastError,omitempty"`
	// RetentionHeld lists backups retention would delete but Object Lock
	// still protects.
	RetentionHeld []string `json:"retentionHeld,omitempty"`
}

const defaultFullEvery = 7 * 24 * time.Hour

// Intervals returns the incremental and full intervals.
func (s MachineBackupPolicySpec) Intervals() (incremental, full time.Duration, err error) {
	incremental, err = ParseBackupDuration(s.IncrementalEvery)
	if err != nil {
		return 0, 0, fmt.Errorf("incrementalEvery: %w", err)
	}
	if incremental < 5*time.Minute {
		return 0, 0, fmt.Errorf("incrementalEvery must be at least 5m")
	}
	full = defaultFullEvery
	if s.FullEvery != "" {
		if full, err = ParseBackupDuration(s.FullEvery); err != nil {
			return 0, 0, fmt.Errorf("fullEvery: %w", err)
		}
	}
	if full < incremental {
		return 0, 0, fmt.Errorf("fullEvery must not be shorter than incrementalEvery")
	}
	return incremental, full, nil
}

func (s MachineBackupPolicySpec) Validate() error {
	if len(s.Selector) == 0 {
		return fmt.Errorf("selector must not be empty")
	}
	if s.Mode != "" && s.Mode != BackupModeIncremental && s.Mode != BackupModeFull {
		return fmt.Errorf("mode must be incremental or full")
	}
	if !ValidBackupQuiesce(s.Quiesce) {
		return fmt.Errorf("quiesce must be auto, required or never")
	}
	if s.Target.S3 == nil {
		return fmt.Errorf("target.s3 is required")
	}
	if s.Retention.Hourly < 0 || s.Retention.Daily < 0 || s.Retention.Monthly < 0 {
		return fmt.Errorf("retention counts must not be negative")
	}
	if _, _, err := s.Intervals(); err != nil {
		return err
	}
	return ValidateBackupTarget(&s.Target, s.Compression, s.Encryption, s.ImmutableFor)
}

func backupDone(b MachineBackup) bool {
	return b.Status.Phase == BackupSucceeded || b.Status.Phase == BackupFailed
}

func pointTime(b MachineBackup) time.Time {
	if b.Status.PointInTime != nil {
		return *b.Status.PointInTime
	}
	return b.Metadata.CreationTimestamp
}

// PolicyRun is what a policy should do for one Machine now.
type PolicyRun struct {
	// Mode is empty when nothing is due.
	Mode   string
	Parent string
	// NextRun is when the next backup is due (zero: now or unknown).
	NextRun time.Time
	// Waiting names the backup still in flight.
	Waiting string
}

// PlanPolicyRun picks the next backup for one Machine from the backups the
// policy already made for it: none while one is in flight; a full when
// there is no chain yet, FullEvery has passed, the policy is full-only or
// FluxVM reported the last incremental impossible (FullRequired);
// otherwise an incremental on the newest successful backup.
func PlanPolicyRun(spec MachineBackupPolicySpec, backups []MachineBackup, now time.Time) (PolicyRun, error) {
	incrEvery, fullEvery, err := spec.Intervals()
	if err != nil {
		return PolicyRun{}, err
	}
	var lastAttempt, lastOK, lastFull, lastSynth *MachineBackup
	for i := range backups {
		b := &backups[i]
		if b.Metadata.DeletionTimestamp != nil {
			continue
		}
		if b.BackupMode() == BackupModeSyntheticFull {
			if b.Status.Phase == BackupSucceeded && (lastSynth == nil || pointTime(*b).After(pointTime(*lastSynth))) {
				lastSynth = b
			}
			continue
		}
		if !backupDone(*b) {
			return PolicyRun{Waiting: b.Metadata.Name}, nil
		}
		if lastAttempt == nil || b.Metadata.CreationTimestamp.After(lastAttempt.Metadata.CreationTimestamp) {
			lastAttempt = b
		}
		if b.Status.Phase != BackupSucceeded {
			continue
		}
		if lastOK == nil || pointTime(*b).After(pointTime(*lastOK)) {
			lastOK = b
		}
		if b.Status.Kind == BackupModeFull && (lastFull == nil || pointTime(*b).After(pointTime(*lastFull))) {
			lastFull = b
		}
	}
	for i := range backups {
		if b := backups[i]; b.BackupMode() == BackupModeSyntheticFull && !backupDone(b) && b.Metadata.DeletionTimestamp == nil {
			return PolicyRun{Waiting: b.Metadata.Name}, nil
		}
	}
	if spec.Suspend {
		return PolicyRun{}, nil
	}
	// Retention may consolidate a chain into a synthetic full and delete the
	// backups it replaced, the newest point included. The synthetic full is
	// then the chain's base and newest point, and incrementals go on from it.
	if lastSynth != nil && lastSynth.Status.Chain != nil {
		if lastOK == nil || pointTime(*lastSynth).After(pointTime(*lastOK)) {
			lastOK = lastSynth
		}
		if lastFull == nil || lastFull.Status.Chain == nil ||
			lastFull.Status.Chain.ID != lastSynth.Status.Chain.ID && pointTime(*lastSynth).After(pointTime(*lastFull)) {
			lastFull = lastSynth
		}
	}
	if lastAttempt == nil {
		if lastOK == nil {
			return PolicyRun{Mode: BackupModeFull}, nil
		}
		lastAttempt = lastOK
	}
	fullRequired := lastAttempt.Status.Phase == BackupFailed && lastAttempt.Status.Disk != nil && lastAttempt.Status.Disk.FullRequired
	next := lastAttempt.Metadata.CreationTimestamp.Add(incrEvery)
	if lastAttempt == lastSynth {
		next = pointTime(*lastSynth).Add(incrEvery)
	}
	if !fullRequired && now.Before(next) {
		return PolicyRun{NextRun: next}, nil
	}
	if fullRequired || lastOK == nil || lastFull == nil || spec.Mode == BackupModeFull ||
		!now.Before(pointTime(*lastFull).Add(fullEvery)) || lastOK.Status.Chain == nil ||
		lastFull.Status.Chain == nil || lastOK.Status.Chain.ID != lastFull.Status.Chain.ID {
		return PolicyRun{Mode: BackupModeFull}, nil
	}
	return PolicyRun{Mode: BackupModeIncremental, Parent: lastOK.Metadata.Name}, nil
}

// GFSKeep returns the chain points (backup names) grandfather-father-son
// retention keeps: the newest point of each of the newest r.Hourly hours,
// r.Daily days and r.Monthly months that have points, plus the newest
// point overall. All counts zero keeps every point.
func GFSKeep(points map[string]time.Time, r BackupRetention) map[string]bool {
	keep := map[string]bool{}
	if len(points) == 0 {
		return keep
	}
	names := make([]string, 0, len(points))
	for n := range points {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if !points[names[i]].Equal(points[names[j]]) {
			return points[names[i]].After(points[names[j]])
		}
		return names[i] > names[j]
	})
	if r.Hourly == 0 && r.Daily == 0 && r.Monthly == 0 {
		for _, n := range names {
			keep[n] = true
		}
		return keep
	}
	keep[names[0]] = true
	bucketed := func(count int, bucket func(time.Time) string) {
		seen := map[string]bool{}
		for _, n := range names {
			if len(seen) >= count {
				return
			}
			k := bucket(points[n].UTC())
			if !seen[k] {
				seen[k] = true
				keep[n] = true
			}
		}
	}
	bucketed(r.Hourly, func(t time.Time) string { return t.Format("2006-01-02T15") })
	bucketed(r.Daily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucketed(r.Monthly, func(t time.Time) string { return t.Format("2006-01") })
	return keep
}

// RetentionPlan is what retention does for one Machine's backups.
type RetentionPlan struct {
	// Delete are backups no kept point needs any more.
	Delete []string
	// Held would be deleted but are under Object Lock retention.
	Held []string
	// Synthesize, when set, is a kept point whose chain still reaches back
	// into a full or synthetic full that retention wants gone; a synthetic
	// full there frees it.
	Synthesize string
}

// PlanRetention applies r to one Machine's backups from one policy. Chain
// points are successful fulls and incrementals; a synthetic full stands
// for the point it consolidates. A kept point needs every backup on its
// path back to a base, where the path stops early at a successful
// synthetic full of the point it reaches.
func PlanRetention(backups []MachineBackup, r BackupRetention, now time.Time) RetentionPlan {
	byName := map[string]MachineBackup{}
	syntheticAt := map[string]string{}
	for _, b := range backups {
		byName[b.Metadata.Name] = b
		if b.BackupMode() == BackupModeSyntheticFull && b.Status.Phase == BackupSucceeded && b.Metadata.DeletionTimestamp == nil {
			if cur, ok := syntheticAt[b.Spec.Parent]; !ok || b.Metadata.Name > cur {
				syntheticAt[b.Spec.Parent] = b.Metadata.Name
			}
		}
	}
	path := func(name string) []string {
		var out []string
		for range 10000 {
			if s, ok := syntheticAt[name]; ok {
				return append(out, s)
			}
			b, ok := byName[name]
			if !ok {
				return out
			}
			if b.BackupMode() == BackupModeSyntheticFull {
				return append(out, name)
			}
			out = append(out, name)
			if b.BackupMode() != BackupModeIncremental || b.Status.Chain == nil {
				return out
			}
			name = b.Status.Chain.Parent
		}
		return out
	}

	points := map[string]time.Time{}
	for _, b := range backups {
		if b.Status.Phase != BackupSucceeded || b.Metadata.DeletionTimestamp != nil {
			continue
		}
		point := b.Metadata.Name
		if b.BackupMode() == BackupModeSyntheticFull {
			point = b.Spec.Parent
			if _, live := byName[point]; live {
				continue
			}
		}
		points[point] = pointTime(b)
	}
	keep := GFSKeep(points, r)
	need := map[string]bool{}
	for p := range keep {
		for _, n := range path(p) {
			need[n] = true
		}
	}
	// Whatever is in flight builds on its parent.
	for _, b := range backups {
		if !backupDone(b) && b.Spec.Parent != "" {
			for _, n := range path(b.Spec.Parent) {
				need[n] = true
			}
			need[b.Spec.Parent] = true
		}
	}

	var plan RetentionPlan
	for _, b := range backups {
		n := b.Metadata.Name
		if need[n] || b.Metadata.DeletionTimestamp != nil || !backupDone(b) {
			continue
		}
		if b.Status.RetentionUntil != nil && now.Before(*b.Status.RetentionUntil) {
			plan.Held = append(plan.Held, n)
			continue
		}
		plan.Delete = append(plan.Delete, n)
	}
	sort.Strings(plan.Delete)
	sort.Strings(plan.Held)

	// A base no kept point wants, still needed through some kept point's
	// path: consolidate the oldest such kept point. Only bases older than
	// the retention window count; one inside it was merely superseded in
	// its GFS bucket, and consolidating it would rewrite a full layer (and
	// delete the chain a DR standby may be restoring from) on every
	// incremental.
	horizon := now.Add(-r.Window())
	var oldest string
	for p := range keep {
		start, ok := byName[p]
		if !ok || start.BackupMode() == BackupModeSyntheticFull || syntheticAt[p] != "" {
			continue
		}
		for _, n := range path(p) {
			b := byName[n]
			point := n
			if b.BackupMode() == BackupModeSyntheticFull {
				point = b.Spec.Parent
			}
			if keep[point] || b.BackupMode() == BackupModeIncremental {
				continue
			}
			if !pointTime(b).Before(horizon) {
				break
			}
			if oldest == "" || pointTime(start).Before(pointTime(byName[oldest])) {
				oldest = p
			}
			break
		}
	}
	plan.Synthesize = oldest
	return plan
}
