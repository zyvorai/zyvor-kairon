// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CBT backups: FluxVM runs each backup, restore or synthesize as a job that
// outlives the request (and kairon-node), polled at GET /v1/cbt-jobs/{id}.
// Credentials and keys travel with every request; FluxVM never stores them.

// CBTTarget is FluxVM's backup target (only S3 from Kairon).
type CBTTarget struct {
	Type            string `json:"type"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region,omitempty"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
	PathStyle       bool   `json:"pathStyle"`
}

type CBTEncryptionKey struct {
	KeyID string `json:"keyId"`
	Key   string `json:"key"`
}

type CBTParentRef struct {
	BackupID    string `json:"backupId"`
	Sequence    int64  `json:"sequence"`
	ManifestKey string `json:"manifestKey"`
}

type CBTBackupRequest struct {
	Target      CBTTarget         `json:"target"`
	Prefix      string            `json:"prefix"`
	ChainID     string            `json:"chainId"`
	Sequence    int64             `json:"sequence"`
	BackupID    string            `json:"backupId"`
	Mode        string            `json:"mode"`
	Parent      *CBTParentRef     `json:"parent,omitempty"`
	Quiesce     string            `json:"quiesce,omitempty"`
	Encryption  *CBTEncryptionKey `json:"encryption,omitempty"`
	RetainUntil *time.Time        `json:"retainUntil,omitempty"`
}

type CBTRestoreRequest struct {
	Target         CBTTarget         `json:"target"`
	ManifestKey    string            `json:"manifestKey"`
	Encryption     *CBTEncryptionKey `json:"encryption,omitempty"`
	SyntheticBases []string          `json:"syntheticBases,omitempty"`
}

type CBTSynthesizeRequest struct {
	Target         CBTTarget         `json:"target"`
	Prefix         string            `json:"prefix"`
	ManifestKey    string            `json:"manifestKey"`
	BackupID       string            `json:"backupId"`
	Encryption     *CBTEncryptionKey `json:"encryption,omitempty"`
	RetainUntil    *time.Time        `json:"retainUntil,omitempty"`
	SyntheticBases []string          `json:"syntheticBases,omitempty"`
}

type CBTDeleteResult struct {
	Deleted       []string `json:"deleted"`
	NotFound      []string `json:"notFound"`
	RetentionHeld []string `json:"retentionHeld"`
}

type CBTDiskLayer struct {
	Name         string `json:"name"`
	Key          string `json:"key"`
	VirtualSize  int64  `json:"virtualSize"`
	StoredSize   int64  `json:"storedSize"`
	ChangedBytes int64  `json:"changedBytes"`
}

type CBTManifest struct {
	BackupID    string         `json:"backupId"`
	ChainID     string         `json:"chainId"`
	Sequence    int64          `json:"sequence"`
	Kind        string         `json:"kind"`
	Quiesced    bool           `json:"quiesced"`
	CreatedAt   time.Time      `json:"createdAt"`
	Disks       []CBTDiskLayer `json:"disks"`
	RetainUntil *time.Time     `json:"retainUntil,omitempty"`
}

type CBTSkippedDisk struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

const (
	CBTJobRunning   = "running"
	CBTJobSucceeded = "succeeded"
	CBTJobFailed    = "failed"
)

type CBTJob struct {
	ID           string           `json:"id"`
	Kind         string           `json:"kind"`
	State        string           `json:"state"`
	Phase        string           `json:"phase"`
	BytesDone    int64            `json:"bytesDone"`
	BytesTotal   int64            `json:"bytesTotal"`
	Error        string           `json:"error,omitempty"`
	FullRequired bool             `json:"fullRequired"`
	ManifestKey  string           `json:"manifestKey,omitempty"`
	Manifest     *CBTManifest     `json:"manifest,omitempty"`
	Skipped      []CBTSkippedDisk `json:"skipped,omitempty"`
}

// ErrCBTJobNotFound: FluxVM has no such job (its state dir was wiped).
var ErrCBTJobNotFound = fmt.Errorf("FluxVM has no such CBT job")

func (c *Client) cbtJob(ctx context.Context, path string, body any) (CBTJob, error) {
	data, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return CBTJob{}, err
	}
	var job CBTJob
	if err := json.Unmarshal(data, &job); err != nil {
		return CBTJob{}, fmt.Errorf("decode FluxVM CBT job: %w", err)
	}
	return job, nil
}

func (c *Client) StartCBTBackup(ctx context.Context, vmID string, req CBTBackupRequest) (CBTJob, error) {
	return c.cbtJob(ctx, "/v1/vms/"+url.PathEscape(vmID)+"/cbt-backup", req)
}

func (c *Client) StartCBTRestore(ctx context.Context, vmID string, req CBTRestoreRequest) (CBTJob, error) {
	return c.cbtJob(ctx, "/v1/vms/"+url.PathEscape(vmID)+"/cbt-restore", req)
}

func (c *Client) StartCBTSynthesize(ctx context.Context, req CBTSynthesizeRequest) (CBTJob, error) {
	return c.cbtJob(ctx, "/v1/cbt/synthesize", req)
}

func (c *Client) GetCBTJob(ctx context.Context, id string) (CBTJob, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/cbt-jobs/"+url.PathEscape(id), nil)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return CBTJob{}, ErrCBTJobNotFound
		}
		return CBTJob{}, err
	}
	var job CBTJob
	if err := json.Unmarshal(data, &job); err != nil {
		return CBTJob{}, fmt.Errorf("decode FluxVM CBT job: %w", err)
	}
	return job, nil
}

// DeleteCBT deletes the objects of each manifest (and the manifest).
func (c *Client) DeleteCBT(ctx context.Context, target CBTTarget, manifestKeys []string) (CBTDeleteResult, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/cbt/delete", map[string]any{"target": target, "manifestKeys": manifestKeys})
	if err != nil {
		return CBTDeleteResult{}, err
	}
	var out CBTDeleteResult
	if err := json.Unmarshal(data, &out); err != nil {
		return CBTDeleteResult{}, fmt.Errorf("decode FluxVM CBT delete: %w", err)
	}
	return out, nil
}
