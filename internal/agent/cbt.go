// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// S3 (CBT) backups run as FluxVM jobs that survive both FluxVM and
// kairon-node restarts, so status.disk.jobID is polled every tick rather
// than waited on in a goroutine.

type cbtState struct {
	mu   sync.Mutex
	at   time.Time
	seen bool
}

// publishCBTCapability sets kairon.zyvor.dev/cbt=true while this node's
// FluxVM reports incremental backups to S3, and removes it otherwise.
func (a *Agent) publishCBTCapability(ctx context.Context) {
	if a.Kube == nil || a.Flux == nil || a.NodeName == "" {
		return
	}
	s := &a.cbt
	s.mu.Lock()
	due := !s.seen || time.Since(s.at) >= capabilityEvery
	if due {
		s.at, s.seen = time.Now(), true
	}
	s.mu.Unlock()
	if !due {
		return
	}
	caps, err := a.Flux.GetRuntimeCapabilities(ctx)
	if err != nil {
		a.log().Debug("FluxVM capabilities read for cbt label failed", "error", err)
		return
	}
	want := caps.CBTBackup()
	node, err := a.Kube.GetNode(ctx, a.NodeName)
	if err != nil {
		a.log().Debug("node read for cbt label failed", "error", err)
		return
	}
	if (node.Metadata.Labels[model.CBTNodeLabel] == "true") == want {
		return
	}
	var value any
	if want {
		value = "true"
	}
	patch := map[string]any{"metadata": map[string]any{"labels": map[string]any{model.CBTNodeLabel: value}}}
	if err := a.Kube.PatchNode(ctx, a.NodeName, patch); err != nil {
		a.log().Warn("cbt label patch failed", "node", a.NodeName, "error", err)
		return
	}
	a.log().Info("cbt label updated", "node", a.NodeName, "cbt", want)
}

// backupSecret reads a Secret a backup names. Only Secrets labelled
// kairon.zyvor.dev/backup-credentials=true are used, so a backup cannot
// make kairon-node read arbitrary Secrets in its namespace.
func (a *Agent) backupSecret(ctx context.Context, ns, name string) (model.Secret, error) {
	s, err := a.Kube.GetSecret(ctx, ns, name)
	if err != nil {
		return model.Secret{}, fmt.Errorf("read Secret %s/%s: %w", ns, name, err)
	}
	if s.Metadata.Labels[model.BackupCredentialsLabel] != "true" {
		return model.Secret{}, fmt.Errorf("Secret %s/%s is not labelled %s=true", ns, name, model.BackupCredentialsLabel)
	}
	return s, nil
}

// cbtTarget resolves b's S3 target and encryption key from their Secrets.
func (a *Agent) cbtTarget(ctx context.Context, b model.MachineBackup) (fluxvm.CBTTarget, *fluxvm.CBTEncryptionKey, error) {
	if !b.IsCBT() {
		return fluxvm.CBTTarget{}, nil, fmt.Errorf("MachineBackup %s has no spec.target.s3", b.Metadata.Name)
	}
	t := b.Spec.Target.S3
	creds, err := a.backupSecret(ctx, b.Namespace(), t.CredentialsSecretRef)
	if err != nil {
		return fluxvm.CBTTarget{}, nil, err
	}
	target := fluxvm.CBTTarget{
		Type:            "s3",
		Endpoint:        t.Endpoint,
		Bucket:          t.Bucket,
		Region:          t.Region,
		AccessKeyID:     strings.TrimSpace(string(creds.Data["accessKeyId"])),
		SecretAccessKey: strings.TrimSpace(string(creds.Data["secretAccessKey"])),
		SessionToken:    strings.TrimSpace(string(creds.Data["sessionToken"])),
		PathStyle:       t.PathStyle == nil || *t.PathStyle,
	}
	if target.AccessKeyID == "" || target.SecretAccessKey == "" {
		return fluxvm.CBTTarget{}, nil, fmt.Errorf("Secret %s/%s needs accessKeyId and secretAccessKey", b.Namespace(), t.CredentialsSecretRef)
	}
	if b.Spec.Encryption == nil {
		return target, nil, nil
	}
	ks, err := a.backupSecret(ctx, b.Namespace(), b.Spec.Encryption.KeySecretRef)
	if err != nil {
		return fluxvm.CBTTarget{}, nil, err
	}
	key, err := encryptionKey(ks.Data["key"])
	if err != nil {
		return fluxvm.CBTTarget{}, nil, fmt.Errorf("Secret %s/%s: %w", b.Namespace(), b.Spec.Encryption.KeySecretRef, err)
	}
	keyID := strings.TrimSpace(string(ks.Data["keyId"]))
	if keyID == "" {
		keyID = b.Spec.Encryption.KeySecretRef
	}
	return target, &fluxvm.CBTEncryptionKey{KeyID: keyID, Key: key}, nil
}

// encryptionKey accepts the Secret's "key" as 32 raw bytes or as base64 of
// them, and returns base64.
func encryptionKey(raw []byte) (string, error) {
	if len(raw) == 32 {
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	text := strings.TrimSpace(string(raw))
	dec, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(dec) != 32 {
		return "", fmt.Errorf(`"key" must be 32 bytes (raw or base64)`)
	}
	return text, nil
}

func (a *Agent) reconcileCBTBackup(ctx context.Context, b model.MachineBackup, machines map[string]model.Machine) error {
	disk := *b.Status.Disk
	if b.Metadata.DeletionTimestamp != nil {
		return a.deleteCBTBackup(ctx, b, disk)
	}
	switch disk.Phase {
	case model.BackupPending:
		return a.startCBTBackup(ctx, b, disk, machines)
	case model.BackupRunning:
		return a.pollCBTBackup(ctx, b, disk)
	}
	return nil
}

func (a *Agent) startCBTBackup(ctx context.Context, b model.MachineBackup, disk model.DiskBackupStatus, machines map[string]model.Machine) error {
	fail := func(msg string) error {
		disk.Phase, disk.Message = model.BackupFailed, msg
		return a.patchDiskBackup(ctx, b, disk)
	}
	chain := b.Status.Chain
	if chain == nil {
		return fail("status.chain is missing")
	}
	target, key, err := a.cbtTarget(ctx, b)
	if err != nil {
		return fail(err.Error())
	}
	var job fluxvm.CBTJob
	if b.BackupMode() == model.BackupModeSyntheticFull {
		job, err = a.Flux.StartCBTSynthesize(ctx, fluxvm.CBTSynthesizeRequest{
			Target:         target,
			Prefix:         model.CBTPrefix(b),
			ManifestKey:    chain.ParentManifestKey,
			BackupID:       chain.BackupID,
			Encryption:     key,
			RetainUntil:    b.Status.RetentionUntil,
			SyntheticBases: disk.SyntheticBases,
		})
	} else {
		m, ok := machines[b.Namespace()+"/"+b.Spec.MachineName]
		if !ok {
			return fail("machine is no longer on this node")
		}
		rec, cerr := a.current(ctx, m)
		if cerr != nil {
			return fail(fmt.Sprintf("find the FluxVM VM: %v", cerr))
		}
		if rec == nil || rec.ID() == "" {
			return fail("the Machine has no FluxVM VM (powerState Stopped deletes it; use Halted)")
		}
		req := fluxvm.CBTBackupRequest{
			Target:      target,
			Prefix:      model.CBTPrefix(b),
			ChainID:     chain.ID,
			Sequence:    chain.Sequence,
			BackupID:    chain.BackupID,
			Mode:        b.BackupMode(),
			Quiesce:     b.Spec.Quiesce,
			Encryption:  key,
			RetainUntil: b.Status.RetentionUntil,
		}
		if req.Mode == model.BackupModeIncremental {
			req.Parent = &fluxvm.CBTParentRef{BackupID: chain.ParentBackupID, Sequence: chain.Sequence - 1, ManifestKey: chain.ParentManifestKey}
		}
		job, err = a.Flux.StartCBTBackup(ctx, rec.ID(), req)
	}
	if err != nil {
		return fail(err.Error())
	}
	disk.JobID, disk.Phase, disk.Message = job.ID, model.BackupRunning, "FluxVM job "+job.ID+" started"
	return a.patchDiskBackup(ctx, b, disk)
}

func (a *Agent) pollCBTBackup(ctx context.Context, b model.MachineBackup, disk model.DiskBackupStatus) error {
	if disk.JobID == "" {
		disk.Phase, disk.Message = model.BackupFailed, "kairon-node restarted before FluxVM accepted the backup; create a new MachineBackup"
		return a.patchDiskBackup(ctx, b, disk)
	}
	job, err := a.Flux.GetCBTJob(ctx, disk.JobID)
	if errors.Is(err, fluxvm.ErrCBTJobNotFound) {
		disk.Phase, disk.Message = model.BackupFailed, "FluxVM has no record of job "+disk.JobID
		return a.patchDiskBackup(ctx, b, disk)
	}
	if err != nil {
		return err
	}
	if !applyCBTJob(&disk, job) {
		return nil
	}
	return a.patchDiskBackup(ctx, b, disk)
}

// applyCBTJob folds a FluxVM job into disk and reports whether it changed.
func applyCBTJob(disk *model.DiskBackupStatus, job fluxvm.CBTJob) bool {
	before := disk.Phase + "\x00" + disk.Message
	switch job.State {
	case fluxvm.CBTJobRunning:
		disk.Message = cbtProgress(job)
	case fluxvm.CBTJobFailed:
		disk.Phase, disk.Message, disk.FullRequired = model.BackupFailed, job.Error, job.FullRequired
		if job.FullRequired {
			disk.Message += " (the next backup must be a full)"
		}
		return true
	case fluxvm.CBTJobSucceeded:
		disk.Phase, disk.Message = model.BackupSucceeded, ""
		disk.ManifestKey = job.ManifestKey
		disk.StoredBytes, disk.ChangedBytes, disk.Disks, disk.Objects = 0, 0, nil, nil
		if job.ManifestKey != "" {
			disk.Objects = append(disk.Objects, job.ManifestKey)
		}
		if mf := job.Manifest; mf != nil {
			disk.Quiesced = mf.Quiesced
			for _, d := range mf.Disks {
				disk.StoredBytes += d.StoredSize
				disk.ChangedBytes += d.ChangedBytes
				disk.Disks = append(disk.Disks, d.Name)
				disk.Objects = append(disk.Objects, d.Key)
			}
			if !mf.Quiesced && mf.Kind != model.BackupModeSyntheticFull {
				disk.Message = "crash-consistent: the guest agent did not freeze filesystems"
			}
		}
		disk.SizeBytes = disk.StoredBytes
		for _, s := range job.Skipped {
			disk.Message = strings.TrimPrefix(disk.Message+"; skipped "+s.Name+": "+s.Reason, "; ")
		}
		return true
	}
	return before != disk.Phase+"\x00"+disk.Message
}

func cbtProgress(job fluxvm.CBTJob) string {
	if job.BytesTotal > 0 {
		return fmt.Sprintf("%s: %d%% of %d MiB", job.Phase, job.BytesDone*100/job.BytesTotal, job.BytesTotal>>20)
	}
	return job.Phase
}

// deleteCBTBackup deletes b's objects from S3 and releases its finalizer.
// Objects under Object Lock stay; the finalizer stays with them, and the
// delete is retried once the retention date passes.
func (a *Agent) deleteCBTBackup(ctx context.Context, b model.MachineBackup, disk model.DiskBackupStatus) error {
	if !model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerCBTBackup) {
		return nil
	}
	var keys []string
	for _, k := range []string{b.Status.ManifestKey, disk.ManifestKey} {
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if disk.Phase == model.BackupRunning && disk.JobID != "" {
		job, err := a.Flux.GetCBTJob(ctx, disk.JobID)
		switch {
		case errors.Is(err, fluxvm.ErrCBTJobNotFound):
		case err != nil:
			return err
		case job.State == fluxvm.CBTJobRunning:
			return nil
		case job.ManifestKey != "" && !slices.Contains(keys, job.ManifestKey):
			keys = append(keys, job.ManifestKey)
		}
	}
	if len(keys) > 0 {
		held := func(msg string) error {
			if disk.Message == msg {
				return nil
			}
			disk.Message = msg
			return a.patchDiskBackup(ctx, b, disk)
		}
		if until := b.Status.RetentionUntil; until != nil && time.Now().Before(*until) {
			return held("deletion waits for S3 Object Lock retention until " + until.UTC().Format(time.RFC3339))
		}
		target, _, err := a.cbtTarget(ctx, b)
		if err != nil {
			return held("cannot delete the S3 objects: " + err.Error())
		}
		res, err := a.Flux.DeleteCBT(ctx, target, keys)
		if err != nil {
			return fmt.Errorf("delete S3 objects: %w", err)
		}
		if len(res.RetentionHeld) > 0 {
			return held(fmt.Sprintf("%d S3 objects are still under Object Lock retention", len(res.RetentionHeld)))
		}
	}
	finals := model.RemoveFinalizer(b.Metadata.Finalizers, model.FinalizerCBTBackup)
	return a.Kube.PatchMachineBackup(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}})
}

// reconcileCBTRestore restores an S3 backup into a halted Machine on this
// node through a FluxVM job.
func (a *Agent) reconcileCBTRestore(ctx context.Context, r model.MachineBackupRestore, disk model.DiskBackupStatus, machines map[string]model.Machine) error {
	fail := func(msg string) error {
		disk.Phase, disk.Message = model.BackupFailed, msg
		return a.patchDiskRestore(ctx, r, disk)
	}
	switch disk.Phase {
	case model.BackupRunning:
		if disk.JobID == "" {
			return fail("kairon-node restarted before FluxVM accepted the restore; restore again before starting the Machine")
		}
		job, err := a.Flux.GetCBTJob(ctx, disk.JobID)
		if errors.Is(err, fluxvm.ErrCBTJobNotFound) {
			return fail("FluxVM has no record of job " + disk.JobID + "; restore again before starting the Machine")
		}
		if err != nil {
			return err
		}
		if !applyCBTJob(&disk, job) {
			return nil
		}
		if disk.Phase == model.BackupSucceeded {
			disk.ManifestKey, disk.Objects, disk.StoredBytes, disk.ChangedBytes, disk.SizeBytes = r.Status.Disk.ManifestKey, nil, 0, 0, 0
			if job.Manifest == nil {
				disk.Disks = nil
			}
		}
		return a.patchDiskRestore(ctx, r, disk)
	case model.BackupPending:
	default:
		return nil
	}
	m, ok := machines[r.Namespace()+"/"+r.Status.MachineName]
	if !ok {
		return fail("machine is no longer on this node")
	}
	rec, err := a.current(ctx, m)
	if err != nil {
		return fail(fmt.Sprintf("find the FluxVM VM: %v", err))
	}
	if rec == nil || rec.ID() == "" {
		return fail("the Machine has no FluxVM VM to restore into (powerState Stopped deletes it; use Halted)")
	}
	if m.DesiredPowerState() != "Halted" || normalizePhase(rec.Status) != "Stopped" {
		msg := "waiting for the Machine to be halted (spec.powerState: Halted)"
		if disk.Message == msg {
			return nil
		}
		disk.Message = msg
		return a.patchDiskRestore(ctx, r, disk)
	}
	b, err := a.Kube.GetMachineBackup(ctx, r.Namespace(), r.Spec.BackupName)
	if err != nil {
		return fail(fmt.Sprintf("get MachineBackup %s: %v", r.Spec.BackupName, err))
	}
	target, key, err := a.cbtTarget(ctx, b)
	if err != nil {
		return fail(err.Error())
	}
	job, err := a.Flux.StartCBTRestore(ctx, rec.ID(), fluxvm.CBTRestoreRequest{
		Target:         target,
		ManifestKey:    disk.ManifestKey,
		Encryption:     key,
		SyntheticBases: disk.SyntheticBases,
	})
	if err != nil {
		return fail(err.Error())
	}
	disk.JobID, disk.Phase, disk.Message = job.ID, model.BackupRunning, "FluxVM job "+job.ID+" started"
	return a.patchDiskRestore(ctx, r, disk)
}
