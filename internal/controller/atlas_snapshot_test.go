// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

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

type fakeAtlasSnapshots struct {
	mu         sync.Mutex
	snapCalls  []string // volume ids snapshotted
	snapStates []string // popped per poll of snap-job
	noIDInAck  bool
	deletes    []string
	deleteCode int
	restores   []map[string]any
}

func (f *fakeAtlasSnapshots) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, atlas.APIPrefix)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(p, "/volumes/") && strings.HasSuffix(p, "/snapshots"):
			f.snapCalls = append(f.snapCalls, strings.TrimSuffix(strings.TrimPrefix(p, "/volumes/"), "/snapshots"))
			res := map[string]any{"snapshot_id": "snap-1"}
			if f.noIDInAck {
				res = map[string]any{}
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "snap-job", "state": "queued", "resource": res})
		case r.Method == http.MethodGet && p == "/jobs/snap-job":
			state := "running"
			if len(f.snapStates) > 0 {
				state, f.snapStates = f.snapStates[0], f.snapStates[1:]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "snap-job", "state": state, "error": "rbd snap failed", "result": map[string]any{"snapshot_id": "snap-1"}})
		case r.Method == http.MethodDelete && strings.HasPrefix(p, "/snapshots/"):
			f.deletes = append(f.deletes, strings.TrimPrefix(p, "/snapshots/"))
			if f.deleteCode != 0 {
				w.WriteHeader(f.deleteCode)
				_, _ = io.WriteString(w, `{"error":{"code":"conflict","message":"has clones"}}`)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "del-job", "state": "queued", "resource": map[string]any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/restore"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["snapshot"] = strings.TrimSuffix(strings.TrimPrefix(p, "/snapshots/"), "/restore")
			f.restores = append(f.restores, body)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "rest-job", "state": "queued", "resource": map[string]any{"volume_id": "vol-new"}})
		case r.Method == http.MethodGet && p == "/jobs/rest-job":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "rest-job", "state": "succeeded"})
		default:
			t.Errorf("unexpected atlas call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}
}

type fakeKubeSnapshots struct {
	mu              sync.Mutex
	finalizers      [][]string
	snapStatuses    []model.MachineSnapshotStatus
	restoreStatuses []model.MachineSnapshotRestoreStatus
	machinePatches  []map[string]any
	pvcBound        bool
}

func (f *fakeKubeSnapshots) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPatch && strings.Contains(p, "/machinesnapshots/") && strings.HasSuffix(p, "/status"):
			var body struct {
				Status model.MachineSnapshotStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.snapStatuses = append(f.snapStatuses, body.Status)
		case r.Method == http.MethodPatch && strings.Contains(p, "/machinesnapshots/"):
			var body struct {
				Metadata struct {
					Finalizers []string `json:"finalizers"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.finalizers = append(f.finalizers, body.Metadata.Finalizers)
		case r.Method == http.MethodPatch && strings.Contains(p, "/machinesnapshotrestores/"):
			var body struct {
				Status model.MachineSnapshotRestoreStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.restoreStatuses = append(f.restoreStatuses, body.Status)
		case r.Method == http.MethodPatch && strings.Contains(p, "/machines/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.machinePatches = append(f.machinePatches, body)
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/machinesnapshots/snap"):
			_ = json.NewEncoder(w).Encode(atlasSucceededSnapshot())
			return
		case r.Method == http.MethodGet && strings.Contains(p, "/persistentvolumeclaims/"):
			if !f.pvcBound {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(model.PersistentVolumeClaim{
				Metadata: model.ObjectMeta{Name: "restored"},
				Status:   model.PersistentVolumeClaimStatus{Phase: "Bound"},
			})
			return
		default:
			t.Errorf("unexpected kube call %s %s", r.Method, p)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func newAtlasSnapshotController(t *testing.T, fa *fakeAtlasSnapshots, fk *fakeKubeSnapshots) *Controller {
	t.Helper()
	ks := httptest.NewServer(fk.handler(t))
	t.Cleanup(ks.Close)
	as := httptest.NewServer(fa.handler(t))
	t.Cleanup(as.Close)
	kc, err := kube.New(ks.URL, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	ac, err := atlas.New(as.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Atlas: AtlasConfig{Client: ac, TenantID: "kairon"}}
}

func atlasSnapshotMachine(t *testing.T, phase string) map[string]model.Machine {
	t.Helper()
	states, err := json.Marshal(map[string]model.AtlasVolumeState{"root": {VolumeID: "vol-root", Mode: model.AtlasModePVC, Phase: phase}})
	if err != nil {
		t.Fatal(err)
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "default", Annotations: map[string]string{model.AnnotationAtlasVolumes: string(states)}},
		Spec:     model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root", ClaimName: "web-root", Atlas: &model.AtlasVolumeSource{Size: "10Gi"}}}},
	}
	return map[string]model.Machine{"default/web": m}
}

func atlasSucceededSnapshot() model.MachineSnapshot {
	return model.MachineSnapshot{
		Metadata: model.ObjectMeta{Name: "snap", Namespace: "default"},
		Spec:     model.MachineSnapshotSpec{MachineName: "web"},
		Status: model.MachineSnapshotStatus{Phase: "Succeeded", ReadyToUse: true, VolumeSnapshots: []model.VolumeSnapshotReference{
			{VolumeName: "root", VolumeSnapshotName: "x", ReadyToUse: true, AtlasSnapshotID: "snap-1", AtlasJobID: "snap-job"},
		}},
	}
}

func TestAtlasMachineSnapshotLifecycle(t *testing.T) {
	fa := &fakeAtlasSnapshots{snapStates: []string{"running", "succeeded"}}
	fk := &fakeKubeSnapshots{}
	ctl := newAtlasSnapshotController(t, fa, fk)
	ctx := context.Background()
	machines := atlasSnapshotMachine(t, model.AtlasPhaseReady)
	snap := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "snap", Namespace: "default"}, Spec: model.MachineSnapshotSpec{MachineName: "web"}}

	if err := ctl.reconcileSnapshot(ctx, snap, machines); err != nil {
		t.Fatal(err)
	}
	if len(fk.finalizers) != 1 || fk.finalizers[0][0] != model.FinalizerAtlasSnapshots {
		t.Fatalf("expected atlas finalizer first, got %v", fk.finalizers)
	}
	if len(fa.snapCalls) != 1 || fa.snapCalls[0] != "vol-root" {
		t.Fatalf("expected one atlas snapshot of vol-root, got %v", fa.snapCalls)
	}
	st := fk.snapStatuses[len(fk.snapStatuses)-1]
	if st.Phase != "Pending" || len(st.VolumeSnapshots) != 1 || st.VolumeSnapshots[0].AtlasJobID != "snap-job" || st.VolumeSnapshots[0].AtlasSnapshotID != "snap-1" {
		t.Fatalf("job id not persisted: %+v", st)
	}

	snap.Metadata.Finalizers = fk.finalizers[0]
	for i := 0; i < 2; i++ {
		snap.Status = fk.snapStatuses[len(fk.snapStatuses)-1]
		if err := ctl.reconcileSnapshot(ctx, snap, machines); err != nil {
			t.Fatal(err)
		}
	}
	if len(fa.snapCalls) != 1 {
		t.Fatalf("snapshot must not be requested twice, got %v", fa.snapCalls)
	}
	st = fk.snapStatuses[len(fk.snapStatuses)-1]
	if st.Phase != "Succeeded" || !st.ReadyToUse || !st.VolumeSnapshots[0].ReadyToUse {
		t.Fatalf("expected Succeeded, got %+v", st)
	}

	now := time.Now()
	snap.Status = st
	snap.Metadata.DeletionTimestamp = &now
	if err := ctl.reconcileSnapshot(ctx, snap, machines); err != nil {
		t.Fatal(err)
	}
	if len(fa.deletes) != 1 || fa.deletes[0] != "snap-1" {
		t.Fatalf("expected atlas snapshot delete, got %v", fa.deletes)
	}
	if last := fk.finalizers[len(fk.finalizers)-1]; len(last) != 0 {
		t.Fatalf("finalizer not removed: %v", last)
	}
}

func TestAtlasMachineSnapshotIDFromJobResult(t *testing.T) {
	fa := &fakeAtlasSnapshots{noIDInAck: true, snapStates: []string{"succeeded"}}
	fk := &fakeKubeSnapshots{}
	ctl := newAtlasSnapshotController(t, fa, fk)
	machines := atlasSnapshotMachine(t, model.AtlasPhaseReady)
	snap := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "snap", Namespace: "default", Finalizers: []string{model.FinalizerAtlasSnapshots}}, Spec: model.MachineSnapshotSpec{MachineName: "web"}}
	if err := ctl.reconcileSnapshot(context.Background(), snap, machines); err != nil {
		t.Fatal(err)
	}
	snap.Status = fk.snapStatuses[len(fk.snapStatuses)-1]
	if err := ctl.reconcileSnapshot(context.Background(), snap, machines); err != nil {
		t.Fatal(err)
	}
	st := fk.snapStatuses[len(fk.snapStatuses)-1]
	if st.Phase != "Succeeded" || st.VolumeSnapshots[0].AtlasSnapshotID != "snap-1" {
		t.Fatalf("expected snapshot id from job result, got %+v", st)
	}
}

func TestAtlasMachineSnapshotJobFailureKeepsRefs(t *testing.T) {
	fa := &fakeAtlasSnapshots{snapStates: []string{"failed"}}
	fk := &fakeKubeSnapshots{}
	ctl := newAtlasSnapshotController(t, fa, fk)
	machines := atlasSnapshotMachine(t, model.AtlasPhaseReady)
	snap := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "snap", Namespace: "default", Finalizers: []string{model.FinalizerAtlasSnapshots}}, Spec: model.MachineSnapshotSpec{MachineName: "web"}}
	_ = ctl.reconcileSnapshot(context.Background(), snap, machines)
	snap.Status = fk.snapStatuses[len(fk.snapStatuses)-1]
	if err := ctl.reconcileSnapshot(context.Background(), snap, machines); err != nil {
		t.Fatal(err)
	}
	st := fk.snapStatuses[len(fk.snapStatuses)-1]
	if st.Phase != "Failed" || !strings.Contains(st.Message, "rbd snap failed") || len(st.VolumeSnapshots) != 1 || st.VolumeSnapshots[0].AtlasSnapshotID != "snap-1" {
		t.Fatalf("expected Failed with refs kept, got %+v", st)
	}
}

func TestAtlasMachineSnapshotVolumeNotReadyFails(t *testing.T) {
	fa := &fakeAtlasSnapshots{}
	fk := &fakeKubeSnapshots{}
	ctl := newAtlasSnapshotController(t, fa, fk)
	snap := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "snap", Namespace: "default", Finalizers: []string{model.FinalizerAtlasSnapshots}}, Spec: model.MachineSnapshotSpec{MachineName: "web"}}
	if err := ctl.reconcileSnapshot(context.Background(), snap, atlasSnapshotMachine(t, model.AtlasPhaseProvisioning)); err != nil {
		t.Fatal(err)
	}
	if len(fa.snapCalls) != 0 || fk.snapStatuses[len(fk.snapStatuses)-1].Phase != "Failed" {
		t.Fatalf("expected Failed without an atlas call, got calls=%v status=%+v", fa.snapCalls, fk.snapStatuses)
	}
}

func TestAtlasMachineSnapshotDeleteConflictStillReleases(t *testing.T) {
	fa := &fakeAtlasSnapshots{deleteCode: http.StatusConflict}
	fk := &fakeKubeSnapshots{}
	ctl := newAtlasSnapshotController(t, fa, fk)
	snap := atlasSucceededSnapshot()
	now := time.Now()
	snap.Metadata.DeletionTimestamp = &now
	snap.Metadata.Finalizers = []string{model.FinalizerAtlasSnapshots}
	if err := ctl.reconcileSnapshot(context.Background(), snap, atlasSnapshotMachine(t, model.AtlasPhaseReady)); err != nil {
		t.Fatal(err)
	}
	if len(fa.deletes) != 1 || len(fk.finalizers) != 1 || len(fk.finalizers[0]) != 0 {
		t.Fatalf("expected delete attempt then finalizer release, got deletes=%v finalizers=%v", fa.deletes, fk.finalizers)
	}
}

func TestAtlasSnapshotRestore(t *testing.T) {
	fa := &fakeAtlasSnapshots{}
	fk := &fakeKubeSnapshots{}
	ctl := newAtlasSnapshotController(t, fa, fk)
	ctx := context.Background()
	restore := model.MachineSnapshotRestore{
		Metadata: model.ObjectMeta{Name: "r", Namespace: "default"},
		Spec:     model.MachineSnapshotRestoreSpec{SnapshotName: "snap", TargetClaimName: "restored", StorageSize: "20Gi"},
	}
	if err := ctl.reconcileSnapshotRestore(ctx, restore); err != nil {
		t.Fatal(err)
	}
	if len(fa.restores) != 1 || fa.restores[0]["snapshot"] != "snap-1" || fa.restores[0]["name"] != "restored" || fa.restores[0]["namespace"] != "default" || fa.restores[0]["size_bytes"] != float64(20<<30) {
		t.Fatalf("unexpected atlas restore: %v", fa.restores)
	}
	restore.Status = fk.restoreStatuses[len(fk.restoreStatuses)-1]
	if restore.Status.AtlasJobID != "rest-job" {
		t.Fatalf("restore job not recorded: %+v", restore.Status)
	}

	if err := ctl.reconcileSnapshotRestore(ctx, restore); err != nil {
		t.Fatal(err)
	}
	if st := fk.restoreStatuses[len(fk.restoreStatuses)-1]; st.Phase != "Pending" {
		t.Fatalf("expected Pending until the PVC exists, got %+v", st)
	}
	fk.pvcBound = true
	if err := ctl.reconcileSnapshotRestore(ctx, restore); err != nil {
		t.Fatal(err)
	}
	if st := fk.restoreStatuses[len(fk.restoreStatuses)-1]; st.Phase != "Succeeded" || st.RestoredClaimName != "restored" {
		t.Fatalf("expected Succeeded, got %+v", st)
	}
	if len(fa.restores) != 1 {
		t.Fatalf("restore must be requested once, got %d", len(fa.restores))
	}
}

func TestSnapshotVolumesFilter(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web"}, Spec: model.MachineSpec{Volumes: []model.MachineVolume{{Name: "root"}, {Name: "data"}}}}
	all, err := snapshotVolumes(model.MachineSnapshot{}, m)
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %v %v", all, err)
	}
	one, err := snapshotVolumes(model.MachineSnapshot{Spec: model.MachineSnapshotSpec{VolumeNames: []string{"data"}}}, m)
	if err != nil || len(one) != 1 || one[0].Name != "data" {
		t.Fatalf("filter: %v %v", one, err)
	}
	if _, err := snapshotVolumes(model.MachineSnapshot{Spec: model.MachineSnapshotSpec{VolumeNames: []string{"nope"}}}, m); err == nil {
		t.Fatal("expected error for unknown volume")
	}
}
