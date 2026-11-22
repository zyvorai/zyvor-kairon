// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeBackupFlux struct {
	mu       sync.Mutex
	vmStatus string
	stored   []map[string]any
	calls    []string
	bodies   []map[string]any
}

func (f *fakeBackupFlux) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vm-1", "status": f.vmStatus})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/backup":
		f.calls = append(f.calls, "backup")
		f.bodies = append(f.bodies, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": body["name"], "size_bytes": 42, "quiesced": true, "created_at": "now",
			"disks": []map[string]any{{"name": "root"}, {"name": "data"}}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/backups":
		_ = json.NewEncoder(w).Encode(map[string]any{"items": f.stored})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/backups/"):
		f.calls = append(f.calls, "delete "+strings.TrimPrefix(r.URL.Path, "/v1/backups/"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/restore-backup":
		f.calls = append(f.calls, "restore "+body["name"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"restored": []string{"root"}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

type fakeBackupKube struct {
	mu         sync.Mutex
	disk       []model.DiskBackupStatus
	finalizers [][]string
}

func (f *fakeBackupKube) lastDisk() model.DiskBackupStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.disk) == 0 {
		return model.DiskBackupStatus{}
	}
	return f.disk[len(f.disk)-1]
}

func newBackupAgent(t *testing.T, ff *fakeBackupFlux, fk *fakeBackupKube) *Agent {
	t.Helper()
	fs := httptest.NewServer(http.HandlerFunc(ff.handler))
	t.Cleanup(fs.Close)
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fk.mu.Lock()
		defer fk.mu.Unlock()
		var body struct {
			Status struct {
				Disk model.DiskBackupStatus `json:"disk"`
			} `json:"status"`
			Metadata struct {
				Finalizers []string `json:"finalizers"`
			} `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
			fk.disk = append(fk.disk, body.Status.Disk)
		case r.Method == http.MethodPatch:
			fk.finalizers = append(fk.finalizers, body.Metadata.Finalizers)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(ks.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	return &Agent{NodeName: "n1", Kube: kc, Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func waitIdle(t *testing.T, a *Agent, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.backups.busy(key) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still running", key)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func backupTestMachine(power string) map[string]model.Machine {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"}}
	m.Spec.NodeName = "n1"
	m.Spec.PowerState = power
	m.Status.RuntimeID = "vm-1"
	return map[string]model.Machine{"prod/web": m}
}

func TestDiskBackupRunsInBackgroundWithDeterministicName(t *testing.T) {
	ff := &fakeBackupFlux{vmStatus: "running"}
	fk := &fakeBackupKube{}
	a := newBackupAgent(t, ff, fk)
	b := model.MachineBackup{
		Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod", UID: "0123456789"},
		Spec:     model.MachineBackupSpec{MachineName: "web", Quiesce: model.BackupQuiesceRequired},
		Status:   model.MachineBackupStatus{NodeName: "n1", Disk: &model.DiskBackupStatus{Phase: model.BackupPending}},
	}
	if err := a.reconcileDiskBackup(context.Background(), b, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a, "backup/prod/b1")

	if fk.disk[0].Phase != model.BackupRunning || fk.disk[0].Name != "kairon-prod-b1-01234567" {
		t.Fatalf("first patch = %+v", fk.disk[0])
	}
	got := fk.lastDisk()
	if got.Phase != model.BackupSucceeded || !got.Quiesced || got.SizeBytes != 42 || strings.Join(got.Disks, ",") != "root,data" {
		t.Fatalf("final patch = %+v", got)
	}
	body := ff.bodies[0]
	if body["name"] != "kairon-prod-b1-01234567" || body["all_disks"] != true || body["quiesce"] != "required" {
		t.Fatalf("backup request = %v", body)
	}
}

func TestDiskBackupRecoversAfterNodeRestart(t *testing.T) {
	ff := &fakeBackupFlux{stored: []map[string]any{{"name": "done", "created_at": "t", "size_bytes": 7}}}
	fk := &fakeBackupKube{}
	a := newBackupAgent(t, ff, fk)
	b := model.MachineBackup{
		Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod"},
		Status:   model.MachineBackupStatus{NodeName: "n1", Disk: &model.DiskBackupStatus{Phase: model.BackupRunning, Name: "done"}},
	}
	if err := a.reconcileDiskBackup(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupSucceeded || got.SizeBytes != 7 {
		t.Fatalf("recovered = %+v", got)
	}

	b.Status.Disk.Name = "gone"
	if err := a.reconcileDiskBackup(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupFailed || !strings.Contains(got.Message, "restarted") {
		t.Fatalf("lost = %+v", got)
	}
}

func TestDeletingBackupDeletesFluxVMCopyAndReleasesFinalizer(t *testing.T) {
	ff := &fakeBackupFlux{}
	fk := &fakeBackupKube{}
	a := newBackupAgent(t, ff, fk)
	now := time.Now()
	b := model.MachineBackup{
		Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod", DeletionTimestamp: &now, Finalizers: []string{"other", model.FinalizerFluxVMBackup}},
		Status:   model.MachineBackupStatus{NodeName: "n1", Disk: &model.DiskBackupStatus{Phase: model.BackupSucceeded, Name: "kairon-prod-b1"}},
	}
	if err := a.reconcileDiskBackup(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ff.calls, ";") != "delete kairon-prod-b1" {
		t.Fatalf("flux calls = %v", ff.calls)
	}
	if len(fk.finalizers) != 1 || strings.Join(fk.finalizers[0], ",") != "other" {
		t.Fatalf("finalizer patches = %v", fk.finalizers)
	}
}

func TestDiskRestoreWaitsForHaltedMachine(t *testing.T) {
	ff := &fakeBackupFlux{vmStatus: "running"}
	fk := &fakeBackupKube{}
	a := newBackupAgent(t, ff, fk)
	r := model.MachineBackupRestore{
		Metadata: model.ObjectMeta{Name: "r1", Namespace: "prod"},
		Status:   model.MachineBackupRestoreStatus{NodeName: "n1", MachineName: "web", Disk: &model.DiskBackupStatus{Phase: model.BackupPending, Name: "kairon-prod-b1"}},
	}
	if err := a.reconcileDiskRestore(context.Background(), r, backupTestMachine("")); err != nil {
		t.Fatal(err)
	}
	if got := fk.lastDisk(); got.Phase != model.BackupPending || !strings.Contains(got.Message, "Halted") {
		t.Fatalf("waiting patch = %+v", got)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("restored a running machine: %v", ff.calls)
	}

	ff.vmStatus = "stopped"
	if err := a.reconcileDiskRestore(context.Background(), r, backupTestMachine("Halted")); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, a, "restore/prod/r1")
	if got := fk.lastDisk(); got.Phase != model.BackupSucceeded || strings.Join(got.Disks, ",") != "root" {
		t.Fatalf("final patch = %+v", got)
	}
	if strings.Join(ff.calls, ";") != "restore kairon-prod-b1" {
		t.Fatalf("flux calls = %v", ff.calls)
	}
}
