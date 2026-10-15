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
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// snapshotTestServer is a minimal fake apiserver serving exactly what
// reconcileSnapshot's quiesce/volume-snapshot machinery calls -- mirrors
// the inline-httptest-server convention this package's other test files
// already use.
type snapshotTestServer struct {
	mu                 sync.Mutex
	machineAnnos       map[string]string
	volumeSnapshots    map[string]model.VolumeSnapshot
	snapshotStatus     model.MachineSnapshotStatus
	snapshotFinalizers []string
}

func newSnapshotTestController(t *testing.T, machineAnnos map[string]string) (*Controller, *snapshotTestServer) {
	t.Helper()
	if machineAnnos == nil {
		machineAnnos = map[string]string{}
	}
	fake := &snapshotTestServer{machineAnnos: machineAnnos, volumeSnapshots: map[string]model.VolumeSnapshot{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/vm-1":
			var patch struct {
				Metadata struct {
					Annotations map[string]any `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			for k, v := range patch.Metadata.Annotations {
				if v == nil {
					delete(fake.machineAnnos, k)
					continue
				}
				fake.machineAnnos[k] = v.(string)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots/snap-root":
			if vs, ok := fake.volumeSnapshots["snap-root"]; ok {
				_ = json.NewEncoder(w).Encode(vs)
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots":
			var vs model.VolumeSnapshot
			_ = json.NewDecoder(r.Body).Decode(&vs)
			fake.volumeSnapshots[vs.Metadata.Name] = vs
			_ = json.NewEncoder(w).Encode(vs)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/snap/status":
			var body struct {
				Status model.MachineSnapshotStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.snapshotStatus = body.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/snap":
			var body struct {
				Metadata struct {
					Finalizers []string `json:"finalizers"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.snapshotFinalizers = body.Metadata.Finalizers
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fake
}

func snapshotWithPhase(phase string) model.MachineSnapshot {
	return model.MachineSnapshot{
		Metadata: model.ObjectMeta{Name: "snap", Namespace: "prod"},
		Spec:     model.MachineSnapshotSpec{MachineName: "vm-1"},
		Status:   model.MachineSnapshotStatus{Phase: phase},
	}
}

func quiesceMachine(annos map[string]string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "prod", Annotations: annos},
		Spec: model.MachineSpec{
			GuestAgent: model.GuestAgentSpec{Enabled: true},
			Volumes:    []model.MachineVolume{{Name: "root", ClaimName: "root-pvc"}},
		},
	}
}

func machinesByKey(ms ...model.Machine) map[string]model.Machine {
	out := map[string]model.Machine{}
	for _, m := range ms {
		out[m.Namespace()+"/"+m.Metadata.Name] = m
	}
	return out
}

func TestReconcileSnapshotRequestsFreezeOnFirstTickWhenGuestAgentEnabled(t *testing.T) {
	ctl, fake := newSnapshotTestController(t, nil)
	snapshot := snapshotWithPhase("")
	machine := quiesceMachine(nil)
	machine.Metadata.UID = "recorded-source-uid"
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if _, _, ok := model.ParseQuiesceRef(fake.machineAnnos[model.AnnotationQuiesceRequest]); !ok {
		t.Fatalf("expected a quiesce request annotation, got %+v", fake.machineAnnos)
	}
	if fake.snapshotStatus.SourceUID != machine.Metadata.UID || fake.snapshotStatus.SourceSpec == nil {
		t.Fatal("snapshot did not retain its source identity and spec")
	}
	if fake.snapshotStatus.Phase != "Freezing" {
		t.Fatalf("expected phase Freezing, got %q", fake.snapshotStatus.Phase)
	}
	if len(fake.volumeSnapshots) != 0 {
		t.Fatal("expected no VolumeSnapshot to be created before the freeze is confirmed")
	}
}

func TestReconcileSnapshotSkipsQuiesceWhenGuestAgentDisabled(t *testing.T) {
	ctl, fake := newSnapshotTestController(t, nil)
	snapshot := snapshotWithPhase("")
	machine := quiesceMachine(nil)
	machine.Spec.GuestAgent.Enabled = false
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if len(fake.machineAnnos) != 0 {
		t.Fatalf("expected no quiesce annotations for a Machine without guestAgent.enabled, got %+v", fake.machineAnnos)
	}
	if len(fake.volumeSnapshots) != 1 {
		t.Fatalf("expected a VolumeSnapshot to be created directly, got %d", len(fake.volumeSnapshots))
	}
}

func TestReconcileSnapshotProceedsOnceFreezeConfirmed(t *testing.T) {
	ref := model.FormatQuiesceRef("snap", time.Now())
	ctl, fake := newSnapshotTestController(t, map[string]string{
		model.AnnotationQuiesceRequest: ref,
		model.AnnotationQuiesceStatus:  ref,
	})
	snapshot := snapshotWithPhase("Freezing")
	machine := quiesceMachine(fake.machineAnnos)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if len(fake.volumeSnapshots) != 1 {
		t.Fatalf("expected the VolumeSnapshot to be created once freeze is confirmed, got %d", len(fake.volumeSnapshots))
	}
	// Every volume's create call has been issued -- should request thaw
	// immediately rather than wait for CSI readiness.
	if fake.snapshotStatus.Phase != "Thawing" {
		t.Fatalf("expected phase Thawing after issuing creates, got %q", fake.snapshotStatus.Phase)
	}
	if _, ok := fake.machineAnnos[model.AnnotationQuiesceRequest]; ok {
		t.Fatal("expected the quiesce request annotation to be cleared (thaw requested)")
	}
}

func TestReconcileSnapshotFreezeTimesOutAndFallsBackToCrashConsistent(t *testing.T) {
	ref := model.FormatQuiesceRef("snap", time.Now().Add(-time.Hour)) // long past quiesceFreezeTimeout
	ctl, fake := newSnapshotTestController(t, map[string]string{model.AnnotationQuiesceRequest: ref})
	snapshot := snapshotWithPhase("Freezing")
	machine := quiesceMachine(fake.machineAnnos)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if _, ok := fake.machineAnnos[model.AnnotationQuiesceRequest]; ok {
		t.Fatal("expected the timed-out request annotation to be cleared")
	}
	if len(fake.volumeSnapshots) != 1 {
		t.Fatalf("expected the snapshot to proceed crash-consistently after timeout, got %d volume snapshots", len(fake.volumeSnapshots))
	}
}

func TestReconcileSnapshotWaitsForThawConfirmationBeforeSucceeding(t *testing.T) {
	ref := model.FormatQuiesceRef("snap", time.Now())
	ctl, fake := newSnapshotTestController(t, map[string]string{model.AnnotationQuiesceStatus: ref}) // still frozen, no request (thaw pending)
	fake.volumeSnapshots["snap-root"] = model.VolumeSnapshot{
		Metadata: model.ObjectMeta{Name: "snap-root", Namespace: "prod"},
		Status:   model.VolumeSnapshotStatus{ReadyToUse: readyBoolPtr(true)},
	}
	snapshot := snapshotWithPhase("Thawing")
	machine := quiesceMachine(fake.machineAnnos)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if fake.snapshotStatus.Phase == "Succeeded" {
		t.Fatal("expected the snapshot to stay non-terminal while the guest is still unconfirmed-thawed")
	}
}

func TestReconcileSnapshotSucceedsOnceThawConfirmedAndVolumesReady(t *testing.T) {
	ctl, fake := newSnapshotTestController(t, nil) // no quiesce annotations left -- thaw already confirmed
	fake.volumeSnapshots["snap-root"] = model.VolumeSnapshot{
		Metadata: model.ObjectMeta{Name: "snap-root", Namespace: "prod"},
		Status:   model.VolumeSnapshotStatus{ReadyToUse: readyBoolPtr(true)},
	}
	snapshot := snapshotWithPhase("Thawing")
	machine := quiesceMachine(nil)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if fake.snapshotStatus.Phase != "Succeeded" {
		t.Fatalf("expected phase Succeeded, got %q", fake.snapshotStatus.Phase)
	}
}

func snapshotDeleting(phase string, finalizers []string) model.MachineSnapshot {
	s := snapshotWithPhase(phase)
	now := time.Now()
	s.Metadata.DeletionTimestamp = &now
	s.Metadata.Finalizers = finalizers
	return s
}

func TestReconcileSnapshotAddsQuiesceFinalizerOnFirstFreezeRequest(t *testing.T) {
	ctl, fake := newSnapshotTestController(t, nil)
	snapshot := snapshotWithPhase("")
	machine := quiesceMachine(nil)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if !model.HasFinalizerList(fake.snapshotFinalizers, model.FinalizerSnapshotQuiesce) {
		t.Fatalf("expected the quiesce finalizer to be added before requesting a freeze, got %+v", fake.snapshotFinalizers)
	}
}

func TestReconcileSnapshotDeletionWithoutFinalizerIsNoop(t *testing.T) {
	// A snapshot that never enabled guest quiesce (or hasn't reached
	// requestGuestFreeze yet) never has the finalizer -- deletion must not
	// make any API call at all, matching prior (pre-finalizer) behavior
	// byte for byte. Any unexpected call here 404s and fails the test.
	ctl, _ := newSnapshotTestController(t, nil)
	snapshot := snapshotDeleting("", nil)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(quiesceMachine(nil))); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
}

func TestReconcileSnapshotDeletionRequestsThawWhileStillFrozenOnItsBehalf(t *testing.T) {
	ref := model.FormatQuiesceRef("snap", time.Now())
	ctl, fake := newSnapshotTestController(t, map[string]string{
		model.AnnotationQuiesceRequest: ref,
		model.AnnotationQuiesceStatus:  ref,
	})
	snapshot := snapshotDeleting("Freezing", []string{model.FinalizerSnapshotQuiesce})
	machine := quiesceMachine(fake.machineAnnos)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if _, ok := fake.machineAnnos[model.AnnotationQuiesceRequest]; ok {
		t.Fatal("expected the quiesce request annotation to be cleared (thaw requested) before deletion proceeds")
	}
	if fake.snapshotFinalizers != nil {
		t.Fatal("expected the finalizer to still be present -- thaw isn't confirmed yet")
	}
}

func TestReconcileSnapshotDeletionWaitsForThawConfirmationBeforeRemovingFinalizer(t *testing.T) {
	ref := model.FormatQuiesceRef("snap", time.Now())
	ctl, fake := newSnapshotTestController(t, map[string]string{model.AnnotationQuiesceStatus: ref}) // request already cleared, thaw not yet confirmed
	snapshot := snapshotDeleting("Thawing", []string{model.FinalizerSnapshotQuiesce})
	machine := quiesceMachine(fake.machineAnnos)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if fake.snapshotFinalizers != nil {
		t.Fatal("expected the finalizer to still be present while the guest thaw is unconfirmed -- never abandon a frozen guest")
	}
}

func TestReconcileSnapshotDeletionRemovesFinalizerOnceGuestIsNoLongerFrozenOnItsBehalf(t *testing.T) {
	ctl, fake := newSnapshotTestController(t, nil) // no quiesce annotations left -- thaw already confirmed
	snapshot := snapshotDeleting("Thawing", []string{model.FinalizerSnapshotQuiesce})
	machine := quiesceMachine(nil)
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey(machine)); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if model.HasFinalizerList(fake.snapshotFinalizers, model.FinalizerSnapshotQuiesce) {
		t.Fatalf("expected the finalizer to be removed once the guest is confirmed not frozen on this snapshot's behalf, got %+v", fake.snapshotFinalizers)
	}
}

func TestReconcileSnapshotDeletionRemovesFinalizerWhenTargetMachineIsGone(t *testing.T) {
	ctl, fake := newSnapshotTestController(t, nil)
	snapshot := snapshotDeleting("Freezing", []string{model.FinalizerSnapshotQuiesce})
	if err := ctl.reconcileSnapshot(context.Background(), snapshot, machinesByKey()); err != nil {
		t.Fatalf("reconcileSnapshot: %v", err)
	}
	if model.HasFinalizerList(fake.snapshotFinalizers, model.FinalizerSnapshotQuiesce) {
		t.Fatalf("expected the finalizer to be removed once the target Machine is gone too, got %+v", fake.snapshotFinalizers)
	}
}

func readyBoolPtr(b bool) *bool { return &b }
