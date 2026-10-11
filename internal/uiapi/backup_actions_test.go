// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestBackupCreateRestoreDelete(t *testing.T) {
	fk := newFakeKube()
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/backups", "", createBackupRequest{Name: "b1", Quiesce: "required"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	b, ok := fk.backups["b1"]
	if !ok || b.Spec.MachineName != "web" || b.Spec.Quiesce != "required" {
		t.Fatalf("backup not stored correctly: %+v", fk.backups)
	}

	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/backups", "", createBackupRequest{Quiesce: "sometimes"}); rr.Code != http.StatusBadRequest {
		t.Errorf("bad quiesce: %d", rr.Code)
	}

	rr = doJSON(t, h, http.MethodPost, "/api/v1/machinebackups/default/b1/restore", "", restoreBackupRequest{Machine: "web2"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	if len(fk.backupRestores) != 1 || fk.backupRestores[0].Spec.BackupName != "b1" || fk.backupRestores[0].Spec.MachineName != "web2" {
		t.Errorf("restore not stored correctly: %+v", fk.backupRestores)
	}

	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/machinebackups/default/b1", "", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := fk.backups["b1"]; ok {
		t.Error("backup still present after delete")
	}
}

func TestS3BackupPoliciesAndChains(t *testing.T) {
	fk := newFakeKube()
	h := newTestServer(t, fk, "").Handler()
	spec := model.MachineBackupPolicySpec{
		Selector:         map[string]string{"tier": "web"},
		IncrementalEvery: "1h",
		Target:           model.BackupTarget{S3: &model.S3BackupTarget{Endpoint: "http://minio:9000", Bucket: "vm", CredentialsSecretRef: "s3"}},
		Retention:        model.BackupRetention{Daily: 7},
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machinebackuppolicies/default", "", createBackupPolicyRequest{Name: "p", Spec: spec}); rr.Code != http.StatusCreated {
		t.Fatalf("create policy: %d %s", rr.Code, rr.Body.String())
	}
	bad := spec
	bad.IncrementalEvery = "1m"
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machinebackuppolicies/default", "", createBackupPolicyRequest{Name: "q", Spec: bad}); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy accepted: %d", rr.Code)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machinebackuppolicies/default/p/suspend", "", map[string]bool{"suspend": true}); rr.Code != http.StatusNoContent || !fk.backupPolicies["p"].Spec.Suspend {
		t.Fatalf("suspend: %d %+v", rr.Code, fk.backupPolicies["p"].Spec)
	}

	// A full from the policy's target, then an incremental inheriting it
	// from its parent.
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/backups", "", createBackupRequest{Name: "f1", Policy: "p", Mode: "full"}); rr.Code != http.StatusCreated {
		t.Fatalf("create full: %d %s", rr.Code, rr.Body.String())
	}
	f1 := fk.backups["f1"]
	if !f1.IsCBT() || f1.Spec.PolicyName != "p" || f1.Spec.Target.S3.Bucket != "vm" {
		t.Fatalf("f1 = %+v", f1.Spec)
	}
	now := time.Now()
	f1.Status = model.MachineBackupStatus{Phase: model.BackupSucceeded, Kind: "full", PointInTime: &now, StoredBytes: 10,
		Chain: &model.BackupChainStatus{ID: "c1", Sequence: 1, BackupID: "f1", Base: "f1"}}
	fk.backups["f1"] = f1
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/backups", "", createBackupRequest{Name: "i1", Parent: "f1"}); rr.Code != http.StatusCreated {
		t.Fatalf("create incremental: %d %s", rr.Code, rr.Body.String())
	}
	if i1 := fk.backups["i1"]; i1.BackupMode() != model.BackupModeIncremental || !i1.IsCBT() {
		t.Fatalf("i1 = %+v", i1.Spec)
	}
	if rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/web/backups", "", createBackupRequest{Name: "x", Mode: "incremental"}); rr.Code != http.StatusBadRequest {
		t.Fatalf("mode without target accepted: %d", rr.Code)
	}

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/web/backup-chains", "", nil)
	var chains []model.BackupChainView
	if err := json.Unmarshal(rr.Body.Bytes(), &chains); err != nil || rr.Code != http.StatusOK || len(chains) != 1 || chains[0].ID != "c1" || chains[0].StoredBytes != 10 {
		t.Fatalf("chains: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/machinebackuppolicies/default/p", "", nil); rr.Code != http.StatusNoContent || len(fk.backupPolicies) != 0 {
		t.Fatalf("delete policy: %d", rr.Code)
	}
}
