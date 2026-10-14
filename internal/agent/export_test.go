// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/diskexport"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type exportFixture struct {
	agent  *Agent
	mu     sync.Mutex
	status model.MachineExportStatus
	export model.MachineExport
}

func newExportFixture(t *testing.T, volDir string, export model.MachineExport) *exportFixture {
	t.Helper()
	f := &exportFixture{export: export}
	pvc := model.PersistentVolumeClaim{
		Metadata: model.ObjectMeta{Name: "root-pvc", Namespace: "prod"},
		Spec:     model.PersistentVolumeClaimSpec{VolumeName: "pv-root"},
		Status:   model.PersistentVolumeClaimStatus{Phase: "Bound"},
	}
	pv := model.PersistentVolume{
		Metadata: model.ObjectMeta{Name: "pv-root"},
		Spec:     model.PersistentVolumeSpec{HostPath: &model.HostPathVolumeSource{Path: volDir}},
	}
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machineexports":
			f.mu.Lock()
			e := f.export
			e.Status = f.status
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(model.MachineExportList{Items: []model.MachineExport{e}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machineexports/disk/status":
			var p struct {
				Status model.MachineExportStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.status = p.Status
			f.mu.Unlock()
		case r.URL.Path == "/api/v1/namespaces/prod/persistentvolumeclaims/root-pvc":
			_ = json.NewEncoder(w).Encode(pvc)
		case r.URL.Path == "/api/v1/persistentvolumes/pv-root":
			_ = json.NewEncoder(w).Encode(pv)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	f.agent = &Agent{NodeName: "n1", Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Exports: &diskexport.Registry{}, ExportDir: t.TempDir()}
	return f
}

func (f *exportFixture) reconcile(t *testing.T, phase string) model.MachineExportStatus {
	t.Helper()
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "n1", Volumes: []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}}},
		Status:   model.MachineStatus{Phase: phase},
	}
	if err := f.agent.reconcileExports(context.Background(), map[string]model.Machine{"prod/web": m}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func newExport(format string) model.MachineExport {
	return model.MachineExport{
		Metadata: model.ObjectMeta{Name: "disk", Namespace: "prod", CreationTimestamp: time.Now().UTC()},
		Spec:     model.MachineExportSpec{MachineName: "web", Format: format},
	}
}

func TestReconcileExportServesAStoppedMachineDisk(t *testing.T) {
	volDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(volDir, bootDiskFileName), []byte("raw-disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newExportFixture(t, volDir, newExport(""))
	if st := f.reconcile(t, "Running"); st.Phase != "Pending" {
		t.Fatalf("running machine: %+v", st)
	}
	st := f.reconcile(t, "Stopped")
	if st.Phase != "Ready" || st.Format != "raw" || st.SizeBytes != 14 || st.DownloadPath != "/api/v1/exports/prod/disk/download" || st.ExpiresAt == nil {
		t.Fatalf("stopped machine: %+v", st)
	}
	e, ok := f.agent.Exports.Get("prod/disk")
	if !ok || e.Path != filepath.Join(volDir, bootDiskFileName) || e.Temp {
		t.Fatalf("registry entry %+v ok=%v", e, ok)
	}
	if st := f.reconcile(t, "Running"); st.Phase != "Pending" {
		t.Fatalf("restarted machine: %+v", st)
	}
	if _, ok := f.agent.Exports.Get("prod/disk"); ok {
		t.Fatal("export still served after the Machine started again")
	}
}

func TestReconcileExportConvertsFormat(t *testing.T) {
	volDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(volDir, bootDiskFileName), []byte("raw-disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "qemu-img")
	// The last argument is the output file.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do out=$a; done\nprintf 'QFI\\373converted' > \"$out\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := qemuImg
	qemuImg = fake
	t.Cleanup(func() { qemuImg = orig })

	f := newExportFixture(t, volDir, newExport("qcow2"))
	var st model.MachineExportStatus
	for i := 0; i < 50; i++ {
		st = f.reconcile(t, "Stopped")
		if st.Phase != "Preparing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Phase != "Ready" || st.Format != "qcow2" {
		t.Fatalf("status %+v", st)
	}
	e, _ := f.agent.Exports.Get("prod/disk")
	if !e.Temp {
		t.Fatalf("entry %+v", e)
	}
	f.mu.Lock()
	f.export.Metadata.CreationTimestamp = time.Now().Add(-3 * time.Hour)
	f.mu.Unlock()
	if st := f.reconcile(t, "Stopped"); st.Phase != "Expired" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(e.Path); !os.IsNotExist(err) {
		t.Fatalf("converted copy not removed: %v", err)
	}
}

func TestReconcileExportRefusesStoppedMachineWithoutVolumes(t *testing.T) {
	f := newExportFixture(t, t.TempDir(), newExport(""))
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Stopped"}}
	if err := f.agent.reconcileExports(context.Background(), map[string]model.Machine{"prod/web": m}); err != nil {
		t.Fatal(err)
	}
	if f.status.Phase != "Failed" {
		t.Fatalf("status %+v", f.status)
	}
}
