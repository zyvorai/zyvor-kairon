// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"sort"
	"time"
)

// BackupChainPoint is one S3 MachineBackup in a chain view.
type BackupChainPoint struct {
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`
	Phase          string     `json:"phase"`
	Sequence       int64      `json:"sequence"`
	Parent         string     `json:"parent,omitempty"`
	Depth          int        `json:"depth"`
	PointInTime    *time.Time `json:"pointInTime,omitempty"`
	StoredBytes    int64      `json:"storedBytes"`
	ChangedBytes   int64      `json:"changedBytes"`
	RetentionUntil *time.Time `json:"retentionUntil,omitempty"`
	Deleting       bool       `json:"deleting,omitempty"`
}

// BackupChainView is one chain of a Machine's S3 backups, oldest first.
type BackupChainView struct {
	ID          string             `json:"id"`
	StoredBytes int64              `json:"storedBytes"`
	Points      []BackupChainPoint `json:"points"`
}

// BackupChains groups machine's S3 backups by chain, newest chain first.
// Depth counts incrementals back to the chain's full; a synthetic full
// sits at its own depth 0, beside the point it consolidates.
func BackupChains(backups []MachineBackup, machine string) []BackupChainView {
	byChain := map[string][]MachineBackup{}
	for _, b := range backups {
		if b.Spec.MachineName != machine || !b.IsCBT() || b.Status.Chain == nil {
			continue
		}
		byChain[b.Status.Chain.ID] = append(byChain[b.Status.Chain.ID], b)
	}
	out := make([]BackupChainView, 0, len(byChain))
	for id, items := range byChain {
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i], items[j]
			if a.Status.Chain.Sequence != b.Status.Chain.Sequence {
				return a.Status.Chain.Sequence < b.Status.Chain.Sequence
			}
			return a.Metadata.CreationTimestamp.Before(b.Metadata.CreationTimestamp)
		})
		depth := map[string]int{}
		view := BackupChainView{ID: id}
		for _, b := range items {
			d := 0
			if b.BackupMode() == BackupModeIncremental {
				if pd, ok := depth[b.Status.Chain.Parent]; ok {
					d = pd + 1
				} else {
					d = 1
				}
			}
			depth[b.Metadata.Name] = d
			p := BackupChainPoint{
				Name:           b.Metadata.Name,
				Kind:           b.BackupMode(),
				Phase:          b.Status.Phase,
				Sequence:       b.Status.Chain.Sequence,
				Parent:         b.Status.Chain.Parent,
				Depth:          d,
				PointInTime:    b.Status.PointInTime,
				StoredBytes:    b.Status.StoredBytes,
				ChangedBytes:   b.Status.ChangedBytes,
				RetentionUntil: b.Status.RetentionUntil,
				Deleting:       b.Metadata.DeletionTimestamp != nil,
			}
			view.StoredBytes += p.StoredBytes
			view.Points = append(view.Points, p)
		}
		out = append(out, view)
	}
	newest := func(v BackupChainView) time.Time {
		var t time.Time
		for _, p := range v.Points {
			if p.PointInTime != nil && p.PointInTime.After(t) {
				t = *p.PointInTime
			}
		}
		return t
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := newest(out[i]), newest(out[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID < out[j].ID
	})
	return out
}
