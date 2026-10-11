// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

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

	atlas "github.com/zyvorai/atlas/clients/go"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const backupAPI = "/apis/kairon.zyvor.dev/v1/"

// backupKube keeps MachineBackups and MachineBackupRestores in memory and
// applies top-level status/metadata merge patches the way the apiserver does
// for the keys this code sends.
type backupKube struct {
	mu       sync.Mutex
	backups  map[string]*model.MachineBackup
	restores map[string]*model.MachineBackupRestore
	nodes    map[string]model.Node
	policies map[string]*model.MachineBackupPolicy
}

func mergeInto(dst any, patch json.RawMessage) {
	cur, _ := json.Marshal(dst)
	var m map[string]any
	_ = json.Unmarshal(cur, &m)
	if m == nil {
		m = map[string]any{}
	}
	var p map[string]any
	_ = json.Unmarshal(patch, &p)
	for k, v := range p {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, dst)
}

func (f *backupKube) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, backupAPI)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/nodes/"):
			n, ok := f.nodes[strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
				return
			}
			_ = json.NewEncoder(w).Encode(n)
		case r.Method == http.MethodGet && p == "machinebackuppolicies":
			items := make([]model.MachineBackupPolicy, 0, len(f.policies))
			for _, x := range f.policies {
				items = append(items, *x)
			}
			_ = json.NewEncoder(w).Encode(model.MachineBackupPolicyList{Items: items})
		case r.Method == http.MethodPatch && strings.HasPrefix(p, "namespaces/prod/machinebackuppolicies/"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "namespaces/prod/machinebackuppolicies/"), "/status")
			f.policies[name].Status = model.MachineBackupPolicyStatus{}
			mergeInto(&f.policies[name].Status, body["status"])
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && (p == "machinebackups" || p == "namespaces/prod/machinebackups"):
			items := make([]model.MachineBackup, 0, len(f.backups))
			for _, b := range f.backups {
				items = append(items, *b)
			}
			_ = json.NewEncoder(w).Encode(model.MachineBackupList{Items: items})
		case r.Method == http.MethodGet && p == "machinebackuprestores":
			items := make([]model.MachineBackupRestore, 0, len(f.restores))
			for _, x := range f.restores {
				items = append(items, *x)
			}
			_ = json.NewEncoder(w).Encode(model.MachineBackupRestoreList{Items: items})
		case r.Method == http.MethodPost && p == "namespaces/prod/machinebackups":
			raw, _ := json.Marshal(body)
			var b model.MachineBackup
			_ = json.Unmarshal(raw, &b)
			b.Metadata.CreationTimestamp = time.Now().UTC()
			f.backups[b.Metadata.Name] = &b
			_ = json.NewEncoder(w).Encode(b)
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "namespaces/prod/machinebackups/"):
			name := strings.TrimPrefix(p, "namespaces/prod/machinebackups/")
			if b, ok := f.backups[name]; ok && len(b.Metadata.Finalizers) > 0 {
				now := time.Now()
				b.Metadata.DeletionTimestamp = &now
			} else {
				delete(f.backups, name)
			}
			_, _ = io.WriteString(w, `{}`)
		case strings.HasPrefix(p, "namespaces/prod/machinebackups/"):
			rest := strings.TrimPrefix(p, "namespaces/prod/machinebackups/")
			name, sub, _ := strings.Cut(rest, "/")
			b, ok := f.backups[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
				return
			}
			switch {
			case r.Method == http.MethodGet:
				_ = json.NewEncoder(w).Encode(b)
				return
			case sub == "status":
				mergeInto(&b.Status, body["status"])
			default:
				mergeInto(&b.Metadata, body["metadata"])
			}
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasPrefix(p, "namespaces/prod/machinebackuprestores/"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "namespaces/prod/machinebackuprestores/"), "/status")
			mergeInto(&f.restores[name].Status, body["status"])
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected kube call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

type backupAtlas struct {
	mu       sync.Mutex
	backups  []atlas.BackupRequest
	restores []atlas.RestoreRequest
	polls    int
}

func (f *backupAtlas) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, atlas.APIPrefix)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && p == "/backup-jobs":
			var req atlas.BackupRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.backups = append(f.backups, req)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "job-b", "state": "queued", "resource": map[string]any{}})
		case r.Method == http.MethodPost && p == "/restore-jobs":
			var req atlas.RestoreRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.restores = append(f.restores, req)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "job-r", "state": "queued", "resource": map[string]any{"volume_id": "vol-new"}})
		case r.Method == http.MethodGet && p == "/jobs/job-b":
			f.polls++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-b", "state": "succeeded", "result": map[string]any{"backup_id": "bk-1"}})
		case r.Method == http.MethodGet && p == "/jobs/job-r":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-r", "state": "succeeded", "result": map[string]any{"volume_id": "vol-new", "pvc": "restored-data"}})
		default:
			t.Errorf("unexpected atlas call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

func newBackupTestController(t *testing.T, fk *backupKube, fa *backupAtlas) *Controller {
	t.Helper()
	ks := httptest.NewServer(fk.handler(t))
	t.Cleanup(ks.Close)
	kc, err := kube.New(ks.URL, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	c := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if fa != nil {
		as := httptest.NewServer(fa.handler(t))
		t.Cleanup(as.Close)
		ac, err := atlas.New(as.URL)
		if err != nil {
			t.Fatal(err)
		}
		c.Atlas = AtlasConfig{Client: ac, BackupBucketID: "bucket-default"}
	}
	return c
}

func backupMachines(ms ...model.Machine) map[string]model.Machine {
	out := map[string]model.Machine{}
	for _, m := range ms {
		out[m.Namespace()+"/"+m.Metadata.Name] = m
	}
	return out
}

func imageMachine(name, node string) model.Machine {
	m := model.Machine{Metadata: model.ObjectMeta{Name: name, Namespace: "prod"}}
	m.Spec.NodeName = node
	m.Spec.Image.Path = "/var/lib/kairon/images/u.qcow2"
	return m
}

func TestMachineBackupHandsDiskHalfToNodeAndFoldsResult(t *testing.T) {
	fk := &backupKube{backups: map[string]*model.MachineBackup{
		"b1": {Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod", UID: "1234-5678-9abc"}, Spec: model.MachineBackupSpec{MachineName: "web"}},
	}}
	c := newBackupTestController(t, fk, nil)
	machines := backupMachines(imageMachine("web", "n1"))
	ctx := context.Background()

	if err := c.reconcileMachineBackups(ctx, machines); err != nil {
		t.Fatal(err)
	}
	b := fk.backups["b1"]
	if b.Status.Phase != model.BackupRunning || b.Status.NodeName != "n1" || b.Status.Disk == nil || b.Status.Disk.Phase != model.BackupPending {
		t.Fatalf("status after start = %+v disk=%+v", b.Status, b.Status.Disk)
	}
	if !model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerFluxVMBackup) {
		t.Fatalf("finalizers = %v", b.Metadata.Finalizers)
	}

	// Nothing changes while kairon-node is still copying.
	if err := c.reconcileMachineBackups(ctx, machines); err != nil {
		t.Fatal(err)
	}
	if b.Status.Phase != model.BackupRunning {
		t.Fatalf("phase = %s", b.Status.Phase)
	}

	b.Status.Disk = &model.DiskBackupStatus{Phase: model.BackupSucceeded, Name: "kairon-prod-b1-12345678", Quiesced: true}
	if err := c.reconcileMachineBackups(ctx, machines); err != nil {
		t.Fatal(err)
	}
	if b.Status.Phase != model.BackupSucceeded || b.Status.CompletionTime == nil || !strings.Contains(b.Status.Message, "kairon-prod-b1-12345678") {
		t.Fatalf("status after node success = %+v", b.Status)
	}
}

func TestMachineBackupDeletionWithoutNodeBackupDropsFinalizer(t *testing.T) {
	now := time.Now()
	fk := &backupKube{backups: map[string]*model.MachineBackup{
		"b1": {
			Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod", DeletionTimestamp: &now, Finalizers: []string{model.FinalizerFluxVMBackup}},
			Spec:     model.MachineBackupSpec{MachineName: "web"},
			Status:   model.MachineBackupStatus{Phase: model.BackupRunning, NodeName: "n1", Disk: &model.DiskBackupStatus{Phase: model.BackupPending}},
		},
	}}
	c := newBackupTestController(t, fk, nil)
	if err := c.reconcileMachineBackups(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fk.backups["b1"].Metadata.Finalizers) != 0 {
		t.Fatalf("finalizers = %v", fk.backups["b1"].Metadata.Finalizers)
	}

	// Once kairon-node named a FluxVM backup, only kairon-node may release it.
	fk.backups["b1"].Metadata.Finalizers = []string{model.FinalizerFluxVMBackup}
	fk.backups["b1"].Status.Disk.Name = "kairon-prod-b1"
	if err := c.reconcileMachineBackups(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(fk.backups["b1"].Metadata.Finalizers) != 1 {
		t.Fatalf("controller dropped the node's finalizer: %v", fk.backups["b1"].Metadata.Finalizers)
	}
}

func TestMachineBackupRefusesWhenNothingApplies(t *testing.T) {
	fk := &backupKube{backups: map[string]*model.MachineBackup{
		"b1": {Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod"}, Spec: model.MachineBackupSpec{MachineName: "web"}},
	}}
	c := newBackupTestController(t, fk, nil)
	m := imageMachine("web", "n1")
	m.Spec.Volumes = []model.MachineVolume{{Name: "root", ClaimName: "web-root"}}
	if err := c.reconcileMachineBackups(context.Background(), backupMachines(m)); err != nil {
		t.Fatal(err)
	}
	if st := fk.backups["b1"].Status; st.Phase != model.BackupFailed || !strings.Contains(st.Message, "nothing to back up") {
		t.Fatalf("status = %+v", st)
	}
}

func atlasBackupMachine() model.Machine {
	m := imageMachine("db", "n1")
	m.Spec.Image.Path = ""
	m.Spec.Volumes = []model.MachineVolume{{Name: "root", Atlas: &model.AtlasVolumeSource{Size: "10Gi"}}}
	states, _ := json.Marshal(map[string]model.AtlasVolumeState{"root": {VolumeID: "vol-db", Phase: model.AtlasPhaseReady}})
	m.Metadata.Annotations = map[string]string{model.AnnotationAtlasVolumes: string(states)}
	return m
}

func TestMachineBackupAtlasVolumesBackUpAndRestoreThroughJobs(t *testing.T) {
	fk := &backupKube{
		backups: map[string]*model.MachineBackup{
			"b1": {Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod"}, Spec: model.MachineBackupSpec{MachineName: "db", Atlas: &model.MachineBackupAtlas{Keep: 3}}},
		},
		restores: map[string]*model.MachineBackupRestore{},
	}
	fa := &backupAtlas{}
	c := newBackupTestController(t, fk, fa)
	machines := backupMachines(atlasBackupMachine())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := c.reconcileMachineBackups(ctx, machines); err != nil {
			t.Fatal(err)
		}
	}
	b := fk.backups["b1"]
	if b.Status.Phase != model.BackupSucceeded || b.Status.Disk != nil {
		t.Fatalf("backup status = %+v", b.Status)
	}
	if len(fa.backups) != 1 || fa.backups[0].VolumeID != "vol-db" || fa.backups[0].BucketID != "bucket-default" || fa.backups[0].Keep != 3 {
		t.Fatalf("atlas backup requests = %+v", fa.backups)
	}
	if v := b.Status.Volumes; len(v) != 1 || v[0].AtlasBackupID != "bk-1" {
		t.Fatalf("volumes = %+v", v)
	}

	fk.restores["r1"] = &model.MachineBackupRestore{
		Metadata: model.ObjectMeta{Name: "r1", Namespace: "prod", CreationTimestamp: time.Now()},
		Spec:     model.MachineBackupRestoreSpec{BackupName: "b1", StorageClassName: "ceph"},
	}
	for i := 0; i < 3; i++ {
		if err := c.reconcileMachineBackups(ctx, machines); err != nil {
			t.Fatal(err)
		}
	}
	r := fk.restores["r1"]
	if r.Status.Phase != model.BackupSucceeded || r.Status.Disk != nil {
		t.Fatalf("restore status = %+v", r.Status)
	}
	if len(fa.restores) != 1 || fa.restores[0].BackupID != "bk-1" || fa.restores[0].StorageClass != "ceph" || fa.restores[0].Mode != atlas.RestoreData {
		t.Fatalf("atlas restore requests = %+v", fa.restores)
	}
	if v := r.Status.Volumes; len(v) != 1 || v[0].AtlasVolumeID != "vol-new" || v[0].ClaimName != "restored-data" {
		t.Fatalf("restored volumes = %+v", v)
	}
}

func TestMachineBackupRestoreRefusesAnotherNode(t *testing.T) {
	fk := &backupKube{
		backups: map[string]*model.MachineBackup{
			"b1": {
				Metadata: model.ObjectMeta{Name: "b1", Namespace: "prod"},
				Spec:     model.MachineBackupSpec{MachineName: "web"},
				Status:   model.MachineBackupStatus{Phase: model.BackupSucceeded, NodeName: "n1", Disk: &model.DiskBackupStatus{Phase: model.BackupSucceeded, Name: "kairon-prod-b1"}},
			},
		},
		restores: map[string]*model.MachineBackupRestore{
			"r1": {Metadata: model.ObjectMeta{Name: "r1", Namespace: "prod"}, Spec: model.MachineBackupRestoreSpec{BackupName: "b1", MachineName: "other"}},
			"r2": {Metadata: model.ObjectMeta{Name: "r2", Namespace: "prod"}, Spec: model.MachineBackupRestoreSpec{BackupName: "b1"}},
		},
	}
	c := newBackupTestController(t, fk, nil)
	machines := backupMachines(imageMachine("web", "n1"), imageMachine("other", "n2"))
	if err := c.reconcileMachineBackups(context.Background(), machines); err != nil {
		t.Fatal(err)
	}
	if st := fk.restores["r1"].Status; st.Phase != model.BackupFailed || !strings.Contains(st.Message, `stored on node "n1"`) {
		t.Fatalf("r1 status = %+v", st)
	}
	st := fk.restores["r2"].Status
	if st.Phase != model.BackupRunning || st.NodeName != "n1" || st.MachineName != "web" || st.Disk == nil || st.Disk.Name != "kairon-prod-b1" || st.Disk.Phase != model.BackupPending {
		t.Fatalf("r2 status = %+v disk=%+v", st, st.Disk)
	}
}

func TestFluxVMBackupNameIsStableAndSafe(t *testing.T) {
	b := model.MachineBackup{Metadata: model.ObjectMeta{Name: "Nightly_1", Namespace: "prod", UID: "abcd-ef01-2345"}}
	got := model.FluxVMBackupName(b)
	if got != "kairon-prod-nightly-1-abcdef01" || got != model.FluxVMBackupName(b) {
		t.Fatalf("name = %q", got)
	}
}
