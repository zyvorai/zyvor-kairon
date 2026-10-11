// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strings"
	"time"
)

const (
	KindMachineBackup        = "MachineBackup"
	KindMachineBackupRestore = "MachineBackupRestore"

	// FinalizerFluxVMBackup holds a deleting MachineBackup until kairon-node
	// has deleted the FluxVM backup it wrote on status.nodeName.
	FinalizerFluxVMBackup = "kairon.zyvor.dev/fluxvm-backup"

	BackupQuiesceAuto     = "auto"
	BackupQuiesceRequired = "required"
	BackupQuiesceNever    = "never"

	BackupPending   = "Pending"
	BackupRunning   = "Running"
	BackupSucceeded = "Succeeded"
	BackupFailed    = "Failed"
)

// A MachineBackup copies a Machine's disks somewhere that outlives it. Two
// independent halves can apply:
//
//   - The disk half runs on the Machine's node: FluxVM writes the root disk
//     and every FluxVM-owned data disk to its backups directory, freezing
//     guest filesystems through the guest agent around the snapshot. It
//     applies to Machines that boot from spec.image (FluxVM owns the root
//     disk); PVC-backed volumes belong to MachineSnapshot.
//   - The Atlas half runs in the controller: one Atlas S3 backup job per
//     spec.volumes[].atlas volume, when spec.atlas is set.
//
// The controller writes status.phase, status.message and status.volumes;
// kairon-node writes only status.disk once the controller has set it to
// Pending, so neither side's merge patches clobber the other's.
type MachineBackup struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta          `json:"metadata"`
	Spec     MachineBackupSpec   `json:"spec"`
	Status   MachineBackupStatus `json:"status,omitempty"`
}

func (b MachineBackup) Namespace() string { return b.Metadata.Namespace }

type MachineBackupList struct {
	TypeMeta `json:",inline"`
	Items    []MachineBackup `json:"items"`
}

type MachineBackupSpec struct {
	MachineName string `json:"machineName"`
	// Quiesce is auto (freeze when the guest agent answers), required
	// (fail without it) or never. Applies to the disk half.
	Quiesce string `json:"quiesce,omitempty"`
	// Atlas, when set, also backs up the Machine's Atlas volumes to S3.
	Atlas *MachineBackupAtlas `json:"atlas,omitempty"`

	// Target, when set, sends the disk half to S3 as a changed-block-tracking
	// chain instead of FluxVM's node-local backups directory. See
	// machinebackupcbt.go.
	Target *BackupTarget `json:"target,omitempty"`
	// Mode is full, incremental or synthetic-full (with Target only).
	Mode string `json:"mode,omitempty"`
	// Parent is the MachineBackup an incremental builds on, or the chain
	// point a synthetic-full consolidates. An incremental created by a
	// policy may leave it empty: the newest successful backup of that
	// policy and Machine is used.
	Parent string `json:"parent,omitempty"`
	// PolicyName is the MachineBackupPolicy that created this backup.
	PolicyName string `json:"policyName,omitempty"`
	// Compression of the uploaded layers; zstd is the only choice.
	Compression string            `json:"compression,omitempty"`
	Encryption  *BackupEncryption `json:"encryption,omitempty"`
	// ImmutableFor sets S3 Object Lock (COMPLIANCE) retention on every
	// object, e.g. "720h" or "30d". The bucket must have Object Lock on.
	ImmutableFor string `json:"immutableFor,omitempty"`
}

type MachineBackupAtlas struct {
	// BucketID defaults to the controller's --atlas-backup-bucket.
	BucketID string `json:"bucketID,omitempty"`
	// VolumeNames limits the backup to these Atlas volumes; empty means all.
	VolumeNames []string `json:"volumeNames,omitempty"`
	// Keep and MaxAgeSeconds are passed to Atlas, which prunes older
	// backups of the same volume. Kairon never deletes Atlas backups itself.
	Keep          int64 `json:"keep,omitempty"`
	MaxAgeSeconds int64 `json:"maxAgeSeconds,omitempty"`
}

type MachineBackupStatus struct {
	Phase string `json:"phase,omitempty"`
	// No omitempty: merge patches must be able to clear it.
	Message string `json:"message"`
	// NodeName is where the disk half runs and where its files stay.
	NodeName       string               `json:"nodeName,omitempty"`
	Disk           *DiskBackupStatus    `json:"disk,omitempty"`
	Volumes        []VolumeBackupStatus `json:"volumes,omitempty"`
	CompletionTime *time.Time           `json:"completionTime,omitempty"`

	// The rest is set for backups with spec.target.
	Chain *BackupChainStatus `json:"chain,omitempty"`
	// Kind is full, incremental or synthetic-full.
	Kind string `json:"kind,omitempty"`
	// PointInTime is the instant this backup restores to; a synthetic full
	// carries the point it consolidates.
	PointInTime    *time.Time `json:"pointInTime,omitempty"`
	ManifestKey    string     `json:"manifestKey,omitempty"`
	StoredBytes    int64      `json:"storedBytes,omitempty"`
	ChangedBytes   int64      `json:"changedBytes,omitempty"`
	Objects        []string   `json:"objects,omitempty"`
	RetentionUntil *time.Time `json:"retentionUntil,omitempty"`
}

// DiskBackupStatus is the FluxVM half of a backup or restore.
type DiskBackupStatus struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
	// Name is the FluxVM backup name (GET /v1/backups on status.nodeName).
	Name      string   `json:"name,omitempty"`
	SizeBytes int64    `json:"sizeBytes,omitempty"`
	Quiesced  bool     `json:"quiesced,omitempty"`
	Disks     []string `json:"disks,omitempty"`

	// CBT (spec.target) fields. JobID is FluxVM's pollable job, which
	// outlives kairon-node restarts.
	JobID        string `json:"jobID,omitempty"`
	ManifestKey  string `json:"manifestKey,omitempty"`
	StoredBytes  int64  `json:"storedBytes,omitempty"`
	ChangedBytes int64  `json:"changedBytes,omitempty"`
	// FullRequired: FluxVM could not run the incremental (dirty bitmap
	// missing or inconsistent, e.g. after an unclean stop, a restore or a
	// new disk); the next backup must be a full.
	FullRequired bool     `json:"fullRequired,omitempty"`
	Objects      []string `json:"objects,omitempty"`
	// SyntheticBases are the chain's synthetic fulls a restore or synthesize
	// may read instead of layers they replaced.
	SyntheticBases []string `json:"syntheticBases,omitempty"`
}

// VolumeBackupStatus is one Atlas volume's backup or restore job.
type VolumeBackupStatus struct {
	VolumeName    string `json:"volumeName"`
	Phase         string `json:"phase"`
	Message       string `json:"message,omitempty"`
	AtlasJobID    string `json:"atlasJobID,omitempty"`
	AtlasBackupID string `json:"atlasBackupID,omitempty"`
	// AtlasVolumeID and ClaimName are set on restore: the new volume.
	AtlasVolumeID string `json:"atlasVolumeID,omitempty"`
	ClaimName     string `json:"claimName,omitempty"`
}

// MachineBackupRestore restores a Succeeded MachineBackup. The disk half is
// copied back in place into a halted Machine on the backup's node (the
// backup's own Machine by default); a backup in S3 (spec.target) restores
// on any node that runs a CBT-capable FluxVM. Atlas volumes restore into new Atlas
// volumes, reported in status.volumes, since a bound PVC can't be swapped
// in place.
type MachineBackupRestore struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                 `json:"metadata"`
	Spec     MachineBackupRestoreSpec   `json:"spec"`
	Status   MachineBackupRestoreStatus `json:"status,omitempty"`
}

func (r MachineBackupRestore) Namespace() string { return r.Metadata.Namespace }

type MachineBackupRestoreList struct {
	TypeMeta `json:",inline"`
	Items    []MachineBackupRestore `json:"items"`
}

type MachineBackupRestoreSpec struct {
	BackupName string `json:"backupName"`
	// MachineName defaults to the backup's Machine. It must run on the
	// backup's node and have spec.powerState Halted before the disk half
	// proceeds (Stopped deletes the VM there would be to restore into).
	MachineName string `json:"machineName,omitempty"`
	// StorageClassName is used for restored Atlas volumes.
	StorageClassName string `json:"storageClassName,omitempty"`
}

type MachineBackupRestoreStatus struct {
	Phase          string               `json:"phase,omitempty"`
	Message        string               `json:"message"`
	NodeName       string               `json:"nodeName,omitempty"`
	MachineName    string               `json:"machineName,omitempty"`
	Disk           *DiskBackupStatus    `json:"disk,omitempty"`
	Volumes        []VolumeBackupStatus `json:"volumes,omitempty"`
	CompletionTime *time.Time           `json:"completionTime,omitempty"`
}

// ValidBackupQuiesce reports whether q is a known quiesce mode ("" is auto).
func ValidBackupQuiesce(q string) bool {
	switch q {
	case "", BackupQuiesceAuto, BackupQuiesceRequired, BackupQuiesceNever:
		return true
	}
	return false
}

// FluxVMBackupName is the deterministic FluxVM backup name for a
// MachineBackup, so kairon-node can find a backup again after a restart.
func FluxVMBackupName(b MachineBackup) string {
	base := strings.Trim(dnsSafe(b.Namespace()+"-"+b.Metadata.Name), "-")
	if len(base) > 80 {
		base = base[:80]
	}
	uid := strings.ReplaceAll(b.Metadata.UID, "-", "")
	if len(uid) > 8 {
		uid = uid[:8]
	}
	if uid == "" {
		return "kairon-" + base
	}
	return fmt.Sprintf("kairon-%s-%s", base, uid)
}

// RestoredVolumeName names the Atlas volume a restore creates.
func RestoredVolumeName(r MachineBackupRestore, volume string) string {
	return AtlasVolumeName(r.Namespace(), "restore-"+r.Metadata.Name, volume)
}

func dnsSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// BackupHalvesDone folds a disk half and Atlas volume halves into an overall
// phase: Failed if any failed, Succeeded once all succeeded, else Running.
func BackupHalvesDone(disk *DiskBackupStatus, volumes []VolumeBackupStatus) (phase, message string) {
	var failed []string
	done := true
	if disk != nil {
		switch disk.Phase {
		case BackupFailed:
			failed = append(failed, "disks: "+disk.Message)
		case BackupSucceeded:
		default:
			done = false
		}
	}
	for _, v := range volumes {
		switch v.Phase {
		case BackupFailed:
			failed = append(failed, fmt.Sprintf("volume %s: %s", v.VolumeName, v.Message))
		case BackupSucceeded:
		default:
			done = false
		}
	}
	switch {
	case len(failed) > 0 && done:
		return BackupFailed, strings.Join(failed, "; ")
	case len(failed) > 0:
		return BackupRunning, "waiting for the remaining parts after a failure: " + strings.Join(failed, "; ")
	case done:
		return BackupSucceeded, ""
	}
	return BackupRunning, ""
}
