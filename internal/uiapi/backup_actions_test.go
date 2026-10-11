// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"testing"
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
