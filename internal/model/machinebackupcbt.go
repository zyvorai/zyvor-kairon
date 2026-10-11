// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Changed-block-tracking (CBT) backups: FluxVM tracks every write to a
// Machine's qcow2 disks in persistent dirty bitmaps, so after one full
// backup each incremental uploads only the clusters written since its
// parent. Chains live in S3 under
//
//	<target.s3.prefix>/<namespace>/<machine>/<chainID>/<sequence>-<backupID>/
//
// and are described by MachineBackup status (chain, kind, manifestKey).

const (
	BackupModeFull          = "full"
	BackupModeIncremental   = "incremental"
	BackupModeSyntheticFull = "synthetic-full"

	BackupCompressionZstd = "zstd"
	BackupEncryptionAES   = "aes256-gcm"

	// FinalizerCBTBackup holds a deleting S3 MachineBackup until kairon-node
	// has deleted its objects (or found them under Object Lock retention).
	FinalizerCBTBackup = "kairon.zyvor.dev/cbt-backup"

	// CBTNodeLabel is published by kairon-node when the node's FluxVM can
	// take incremental backups to S3.
	CBTNodeLabel = "kairon.zyvor.dev/cbt"
	// BackupCredentialsLabel must be "true" on every Secret a backup names:
	// kairon-node refuses to read any other Secret for a backup.
	BackupCredentialsLabel = "kairon.zyvor.dev/backup-credentials"
	// BackupPolicyLabel and BackupMachineLabel mark backups a
	// MachineBackupPolicy created.
	BackupPolicyLabel  = "kairon.zyvor.dev/backup-policy"
	BackupMachineLabel = "kairon.zyvor.dev/backup-machine"
)

type BackupTarget struct {
	S3 *S3BackupTarget `json:"s3,omitempty"`
}

type S3BackupTarget struct {
	// Endpoint is the S3 URL, e.g. https://s3.eu-central-1.amazonaws.com or
	// http://minio.backup:9000.
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region,omitempty"`
	// Prefix goes in front of <namespace>/<machine> in every key.
	Prefix string `json:"prefix,omitempty"`
	// CredentialsSecretRef names a Secret in the backup's namespace with
	// accessKeyId, secretAccessKey and optionally sessionToken, labelled
	// kairon.zyvor.dev/backup-credentials=true.
	CredentialsSecretRef string `json:"credentialsSecretRef"`
	// PathStyle addresses endpoint/bucket/key (the default; MinIO, Ceph)
	// rather than bucket.endpoint/key.
	PathStyle *bool `json:"pathStyle,omitempty"`
}

type BackupEncryption struct {
	// Algorithm is aes256-gcm (the default).
	Algorithm string `json:"algorithm,omitempty"`
	// KeySecretRef names a Secret in the backup's namespace whose "key" is
	// base64 of 32 random bytes, labelled
	// kairon.zyvor.dev/backup-credentials=true. An optional "keyId" names
	// the key in manifests (default: the Secret's name); change it when the
	// key changes, since restores check it.
	KeySecretRef string `json:"keySecretRef"`
}

// BackupChainStatus places a backup in its chain.
type BackupChainStatus struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
	// BackupID is this backup's id inside the chain's manifests.
	BackupID string `json:"backupID"`
	// Parent is the MachineBackup this incremental builds on, or the chain
	// point a synthetic full consolidates.
	Parent            string `json:"parent,omitempty"`
	ParentBackupID    string `json:"parentBackupID,omitempty"`
	ParentManifestKey string `json:"parentManifestKey,omitempty"`
	// Base is the full or synthetic full the chain starts from.
	Base string `json:"base,omitempty"`
}

// IsCBT reports whether the backup goes to S3 as a CBT chain.
func (b MachineBackup) IsCBT() bool {
	return b.Spec.Target != nil && b.Spec.Target.S3 != nil
}

// BackupMode is the backup's mode, full when unset.
func (b MachineBackup) BackupMode() string {
	if b.Spec.Mode == "" {
		return BackupModeFull
	}
	return b.Spec.Mode
}

// ParseBackupDuration accepts Go durations ("720h") and whole days ("30d").
func ParseBackupDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q: use e.g. 720h or 30d", s)
	}
	return d, nil
}

// validKeySegment mirrors FluxVM's object key rule.
func validKeySegment(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// ValidateBackupTarget checks the S3 target, compression, encryption and
// immutability settings shared by MachineBackup and MachineBackupPolicy.
func ValidateBackupTarget(t *BackupTarget, compression string, enc *BackupEncryption, immutableFor string) error {
	if t == nil || t.S3 == nil {
		if immutableFor != "" || enc != nil || compression != "" {
			return fmt.Errorf("compression, encryption and immutableFor need target.s3")
		}
		return nil
	}
	s3 := t.S3
	if !strings.HasPrefix(s3.Endpoint, "https://") && !strings.HasPrefix(s3.Endpoint, "http://") {
		return fmt.Errorf("target.s3.endpoint must be an http(s) URL")
	}
	if s3.Bucket == "" || s3.CredentialsSecretRef == "" {
		return fmt.Errorf("target.s3 needs bucket and credentialsSecretRef")
	}
	if s3.Prefix != "" {
		for _, seg := range strings.Split(s3.Prefix, "/") {
			if !validKeySegment(seg) {
				return fmt.Errorf("target.s3.prefix segment %q: use 1-128 of [A-Za-z0-9._-]", seg)
			}
		}
	}
	if compression != "" && compression != BackupCompressionZstd {
		return fmt.Errorf("compression must be zstd")
	}
	if enc != nil {
		if enc.Algorithm != "" && enc.Algorithm != BackupEncryptionAES {
			return fmt.Errorf("encryption.algorithm must be aes256-gcm")
		}
		if enc.KeySecretRef == "" {
			return fmt.Errorf("encryption.keySecretRef is required")
		}
	}
	if immutableFor != "" {
		if _, err := ParseBackupDuration(immutableFor); err != nil {
			return fmt.Errorf("immutableFor: %w", err)
		}
	}
	return nil
}

// ValidateCBTBackup checks a MachineBackup with spec.target.
func ValidateCBTBackup(b MachineBackup) error {
	if err := ValidateBackupTarget(b.Spec.Target, b.Spec.Compression, b.Spec.Encryption, b.Spec.ImmutableFor); err != nil {
		return err
	}
	if !b.IsCBT() {
		if b.Spec.Mode != "" || b.Spec.Parent != "" {
			return fmt.Errorf("spec.mode and spec.parent need spec.target.s3")
		}
		return nil
	}
	switch b.BackupMode() {
	case BackupModeFull:
		if b.Spec.Parent != "" {
			return fmt.Errorf("a full backup has no parent")
		}
	case BackupModeIncremental:
		if b.Spec.Parent == "" && b.Spec.PolicyName == "" {
			return fmt.Errorf("an incremental backup needs spec.parent or spec.policyName")
		}
	case BackupModeSyntheticFull:
		if b.Spec.Parent == "" {
			return fmt.Errorf("a synthetic-full backup needs spec.parent, the chain point to consolidate")
		}
	default:
		return fmt.Errorf("spec.mode must be full, incremental or synthetic-full")
	}
	return nil
}

// CBTPrefix is the key prefix of a backup's chains.
func CBTPrefix(b MachineBackup) string {
	parts := []string{}
	if b.IsCBT() && b.Spec.Target.S3.Prefix != "" {
		parts = append(parts, b.Spec.Target.S3.Prefix)
	}
	return strings.Join(append(parts, b.Namespace(), b.Spec.MachineName), "/")
}

// CBTChainID names a new chain started by full backup b.
func CBTChainID(b MachineBackup) string {
	return "c-" + strings.TrimPrefix(FluxVMBackupName(b), "kairon-")
}

// SameBackupTarget reports whether two backups write to the same bucket
// and prefix, so one can build on the other.
func SameBackupTarget(a, b MachineBackup) bool {
	if !a.IsCBT() || !b.IsCBT() {
		return false
	}
	x, y := a.Spec.Target.S3, b.Spec.Target.S3
	return x.Endpoint == y.Endpoint && x.Bucket == y.Bucket && CBTPrefix(a) == CBTPrefix(b)
}

// InheritBackupTarget fills an S3 backup's target, compression, encryption
// and immutability from policy (when given) or, for a backup with
// spec.parent, from that parent, unless b already names a target. Callers
// fetch policy and parent; either may be nil.
func InheritBackupTarget(b *MachineBackup, policy *MachineBackupPolicy, parent *MachineBackup) error {
	if policy != nil {
		b.Spec.PolicyName = policy.Metadata.Name
	}
	if b.Spec.Target != nil {
		return nil
	}
	switch {
	case policy != nil:
		target := policy.Spec.Target
		b.Spec.Target, b.Spec.Compression, b.Spec.Encryption, b.Spec.ImmutableFor = &target, policy.Spec.Compression, policy.Spec.Encryption, policy.Spec.ImmutableFor
		if b.Spec.Quiesce == "" {
			b.Spec.Quiesce = policy.Spec.Quiesce
		}
	case parent != nil:
		if !parent.IsCBT() {
			return fmt.Errorf("parent %s is not an S3 backup", parent.Metadata.Name)
		}
		b.Spec.Target, b.Spec.Compression, b.Spec.Encryption, b.Spec.ImmutableFor = parent.Spec.Target, parent.Spec.Compression, parent.Spec.Encryption, parent.Spec.ImmutableFor
		if b.Spec.Mode == "" {
			b.Spec.Mode = BackupModeIncremental
		}
	case b.Spec.Mode != "":
		return fmt.Errorf("mode %s needs an S3 target: give one, a parent or a policy", b.Spec.Mode)
	}
	return nil
}
