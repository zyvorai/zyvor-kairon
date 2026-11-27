// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

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

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
)

// machineSetTestServer is a minimal fake apiserver serving exactly what
// reconcileMachineSet calls: creating/deleting Machines, patching a
// MachineSet's status, and patching a MachineSet's own finalizers. Mirrors
// cordonTestServer's own inline-httptest convention.
type machineSetTestServer struct {
	mu                sync.Mutex
	created           []model.Machine
	deleted           []string
	patched           model.MachineSetStatus
	hadPatch          bool
	finalizerPatches  [][]string
	finalizerPatchErr bool
	// annotationPatches records metadata.annotations merge patches
	// (template revision bookkeeping), kept apart from finalizerPatches.
	annotationPatches []map[string]any
}

func newMachineSetTestController(t *testing.T) (*Controller, *machineSetTestServer) {
	t.Helper()
	fake := &machineSetTestServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines":
			var m model.Machine
			_ = json.NewDecoder(r.Body).Decode(&m)
			fake.created = append(fake.created, m)
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/")
			fake.deleted = append(fake.deleted, name)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesets/ms1/status":
			var body struct {
				Status model.MachineSetStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.patched = body.Status
			fake.hadPatch = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesets/ms1":
			if fake.finalizerPatchErr {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			var body struct {
				Metadata struct {
					Finalizers  []string       `json:"finalizers"`
					Annotations map[string]any `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Metadata.Annotations != nil {
				fake.annotationPatches = append(fake.annotationPatches, body.Metadata.Annotations)
			} else {
				fake.finalizerPatches = append(fake.finalizerPatches, body.Metadata.Finalizers)
			}
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

func testMachineSet(replicas int, strategy, maxUnavailable string) model.MachineSet {
	return model.MachineSet{
		Metadata: model.ObjectMeta{Name: "ms1", Namespace: "prod"},
		Spec: model.MachineSetSpec{
			Replicas:       replicas,
			Strategy:       strategy,
			MaxUnavailable: maxUnavailable,
			Template:       model.MachineTemplate{Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}}},
		},
	}
}

func ownedMachine(name, templateHash, phase string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod", Labels: map[string]string{
			model.LabelMachineSet:             "ms1",
			model.LabelMachineSetTemplateHash: templateHash,
		}},
		Status: model.MachineStatus{Phase: phase},
	}
}

func TestMachineSetTemplateHashIsStableAndSensitiveToChange(t *testing.T) {
	t1 := model.MachineTemplate{Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "1"}}}
	t2 := model.MachineTemplate{Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "2"}}}
	if machineSetTemplateHash(t1) != machineSetTemplateHash(t1) { //nolint:staticcheck // intentional: verifying determinism, not comparing t1 vs t2
		t.Fatal("expected the same template to hash the same way twice")
	}
	if machineSetTemplateHash(t1) == machineSetTemplateHash(t2) {
		t.Fatal("expected different templates to hash differently")
	}
}

func TestReconcileMachineSetCreatesMissingReplicas(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(3, "", "")
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, nil); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	if len(fake.created) != 3 {
		t.Fatalf("expected 3 created Machines, got %d", len(fake.created))
	}
	for _, m := range fake.created {
		hash := machineSetTemplateHash(ms.Spec.Template)
		if m.Metadata.Labels[model.LabelMachineSet] != "ms1" || m.Metadata.Labels[model.LabelMachineSetTemplateHash] != hash {
			t.Fatalf("expected created Machine to carry machineset labels, got %+v", m.Metadata.Labels)
		}
	}
	if !fake.hadPatch || fake.patched.Replicas != 0 {
		t.Fatalf("expected a status patch reflecting the pre-create owned count, got %+v", fake.patched)
	}
}

// TestReconcileMachineSetPublishesScaleSelector covers the contract the
// CRD's scale subresource depends on (labelSelectorPath: .status.selector):
// every status patch carries a selector matching exactly the label stamped
// on owned Machines, so `kubectl scale`, HPA and KEDA can resolve them.
func TestReconcileMachineSetPublishesScaleSelector(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(1, "", "")
	hash := machineSetTemplateHash(ms.Spec.Template)
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, []model.Machine{ownedMachine("ms1-a", hash, "Running")}); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	want := model.LabelMachineSet + "=ms1"
	if !fake.hadPatch || fake.patched.Selector != want {
		t.Fatalf("status.selector = %q, want %q", fake.patched.Selector, want)
	}
	if fake.patched.Replicas != 1 {
		t.Fatalf("status.replicas = %d, want 1 (the scale subresource's statusReplicasPath)", fake.patched.Replicas)
	}
}

func TestReconcileMachineSetScalesDownExcessCurrentReplicas(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(1, "", "")
	hash := machineSetTemplateHash(ms.Spec.Template)
	owned := []model.Machine{ownedMachine("ms1-a", hash, "Running"), ownedMachine("ms1-b", hash, "Running"), ownedMachine("ms1-c", hash, "Running")}
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, owned); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	if len(fake.deleted) != 2 {
		t.Fatalf("expected 2 deletions to scale 3 -> 1, got %d: %v", len(fake.deleted), fake.deleted)
	}
	if len(fake.created) != 0 {
		t.Fatal("expected no creations on a pure scale-down")
	}
}

func TestReconcileMachineSetIgnoresMachinesFromOtherSets(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(1, "", "")
	unrelated := model.Machine{Metadata: model.ObjectMeta{Name: "other", Namespace: "prod", Labels: map[string]string{model.LabelMachineSet: "ms2"}}}
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, []model.Machine{unrelated}); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	if len(fake.created) != 1 || len(fake.deleted) != 0 {
		t.Fatalf("expected exactly 1 create and no deletes (unrelated Machine untouched), got created=%d deleted=%d", len(fake.created), len(fake.deleted))
	}
}

// TestReconcileMachineSetAddsFinalizerOnFirstReconcile proves a MachineSet
// with no FinalizerMachineSet yet gets one patched on, before this project's
// missing ownerReference/garbage-collection mechanism could otherwise let a
// delete of it slip past reconcileMachineSetDeletion's own fail-closed
// cascade entirely.
func TestReconcileMachineSetAddsFinalizerOnFirstReconcile(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(1, "", "")
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, nil); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	if len(fake.finalizerPatches) != 1 || len(fake.finalizerPatches[0]) != 1 || fake.finalizerPatches[0][0] != model.FinalizerMachineSet {
		t.Fatalf("expected exactly one finalizer patch adding %q, got %v", model.FinalizerMachineSet, fake.finalizerPatches)
	}
}

// TestReconcileMachineSetSkipsFinalizerPatchWhenAlreadyPresent proves a
// MachineSet that already carries FinalizerMachineSet (every subsequent
// tick, in practice) never re-patches it -- the same "add once" shape every
// other finalizer-guarded reconcile loop in this project (reconcileSecurityGroup,
// reconcileMachineNetworkPolicy) already follows.
func TestReconcileMachineSetSkipsFinalizerPatchWhenAlreadyPresent(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(1, "", "")
	ms.Metadata.Finalizers = []string{model.FinalizerMachineSet}
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, nil); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	if len(fake.finalizerPatches) != 0 {
		t.Fatalf("expected no finalizer patch when already present, got %v", fake.finalizerPatches)
	}
}

func deletingMachineSet(finalizers ...string) model.MachineSet {
	now := time.Now().UTC()
	return model.MachineSet{
		Metadata: model.ObjectMeta{Name: "ms1", Namespace: "prod", Finalizers: finalizers, DeletionTimestamp: &now},
		Spec:     model.MachineSetSpec{Replicas: 1},
	}
}

// TestReconcileMachineSetDeletionDeletesEveryOwnedMachine proves the core
// cascade: a MachineSet being deleted with FinalizerMachineSet still set
// deletes every Machine carrying its LabelMachineSet label, not just leaves
// them running orphaned once the MachineSet itself is gone.
func TestReconcileMachineSetDeletionDeletesEveryOwnedMachine(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := deletingMachineSet(model.FinalizerMachineSet)
	owned := []model.Machine{ownedMachine("ms1-a", "h", "Running"), ownedMachine("ms1-b", "h", "Running")}
	ctl.reconcileMachineSetDeletion(context.Background(), ms, owned)
	if len(fake.deleted) != 2 {
		t.Fatalf("expected both owned Machines deleted, got %v", fake.deleted)
	}
	// Neither owned Machine actually vanished from this tick's own listing
	// yet (DeleteMachine only requests deletion -- each Machine's own
	// runtime-cleanup finalizer keeps it around until kairon-node tears down
	// its FluxVM VM), so the MachineSet's own finalizer must not clear yet.
	if len(fake.finalizerPatches) != 0 {
		t.Fatalf("expected the finalizer left in place until owned Machines are actually gone, got patch %v", fake.finalizerPatches)
	}
}

// TestReconcileMachineSetDeletionRemovesFinalizerOnceNoOwnedMachinesRemain
// is the completion case: once a fresh Machine listing shows none of this
// MachineSet's owned Machines exist any more, the finalizer clears and the
// object can finally leave Kubernetes.
func TestReconcileMachineSetDeletionRemovesFinalizerOnceNoOwnedMachinesRemain(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := deletingMachineSet(model.FinalizerMachineSet)
	ctl.reconcileMachineSetDeletion(context.Background(), ms, nil)
	if len(fake.deleted) != 0 {
		t.Fatalf("expected no delete calls with no owned Machines left, got %v", fake.deleted)
	}
	if len(fake.finalizerPatches) != 1 || len(fake.finalizerPatches[0]) != 0 {
		t.Fatalf("expected the finalizer removed, got %v", fake.finalizerPatches)
	}
}

// TestReconcileMachineSetDeletionSkipsMachinesAlreadyBeingDeleted proves an
// owned Machine whose own deletion is already in flight (DeletionTimestamp
// already set, e.g. a prior tick's DeleteMachine call, or someone deleted
// it directly) is never re-deleted -- but still counts toward "owned
// Machines remain," keeping the MachineSet's own finalizer in place until
// it's actually gone.
func TestReconcileMachineSetDeletionSkipsMachinesAlreadyBeingDeleted(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := deletingMachineSet(model.FinalizerMachineSet)
	now := time.Now().UTC()
	alreadyDeleting := ownedMachine("ms1-a", "h", "Running")
	alreadyDeleting.Metadata.DeletionTimestamp = &now
	ctl.reconcileMachineSetDeletion(context.Background(), ms, []model.Machine{alreadyDeleting})
	if len(fake.deleted) != 0 {
		t.Fatalf("expected no redundant DeleteMachine call, got %v", fake.deleted)
	}
	if len(fake.finalizerPatches) != 0 {
		t.Fatal("expected the finalizer left in place while the already-deleting Machine is still listed")
	}
}

// TestReconcileMachineSetDeletionKeepsFinalizerOnDeleteError proves the
// fail-closed shape: a genuine DeleteMachine failure must leave
// FinalizerMachineSet in place (no finalizer-removal patch observed)
// rather than let the MachineSet vanish from Kubernetes while an owned
// Machine it never got around to deleting keeps running, unmanaged --
// mirrors TestReconcileSecurityGroupDeletionKeepsFinalizerOnDeleteError
// (internal/agent/network_test.go).
func TestReconcileMachineSetDeletionKeepsFinalizerOnDeleteError(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	fake.mu.Lock()
	fake.finalizerPatchErr = false
	fake.mu.Unlock()
	ms := deletingMachineSet(model.FinalizerMachineSet)
	// Two owned Machines, but only the underlying server accepts deletes
	// for "ms1-a" -- "ms1-b" always 404s, simulating a genuine per-Machine
	// delete failure.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/ms1-a" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	kc, err := kube.New(broken.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = broken.Client()
	ctl.Kube = kc

	owned := []model.Machine{ownedMachine("ms1-a", "h", "Running"), ownedMachine("ms1-b", "h", "Running")}
	ctl.reconcileMachineSetDeletion(context.Background(), ms, owned)
	if len(fake.finalizerPatches) != 0 {
		t.Fatal("expected the finalizer left in place after a genuine delete failure")
	}
}

// TestReconcileMachineSetDeletionIgnoresMachineSetWithoutFinalizer proves a
// MachineSet that never carried FinalizerMachineSet in the first place
// (e.g. deleted in the same tick it was created, before reconcileMachineSet
// ever got to add it) doesn't attempt any cascade -- nothing to fail closed
// on, the same "never had it" no-op reconcileMachineNetworkPolicy's own
// deletion path already takes.
func TestReconcileMachineSetDeletionIgnoresMachineSetWithoutFinalizer(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := deletingMachineSet() // no finalizers at all
	owned := []model.Machine{ownedMachine("ms1-a", "h", "Running")}
	ctl.reconcileMachineSetDeletion(context.Background(), ms, owned)
	if len(fake.deleted) != 0 || len(fake.finalizerPatches) != 0 {
		t.Fatalf("expected no delete or finalizer patch, got deleted=%v finalizerPatches=%v", fake.deleted, fake.finalizerPatches)
	}
}

// TestReconcileMachineSetsCascadesDeletionThroughTopLevelEntryPoint proves
// reconcileMachineSets (not just reconcileMachineSetDeletion directly)
// routes a MachineSet with a DeletionTimestamp to the cascade path instead
// of silently skipping it -- the actual bug this whole cascade closes: the
// previous behavior was a bare `continue` for any MachineSet being deleted.
func TestReconcileMachineSetsCascadesDeletionThroughTopLevelEntryPoint(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := deletingMachineSet(model.FinalizerMachineSet)
	owned := []model.Machine{ownedMachine("ms1-a", "h", "Running")}
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.deleted) != 1 || fake.deleted[0] != "ms1-a" {
		t.Fatalf("expected reconcileMachineSets to cascade-delete the owned Machine, got %v", fake.deleted)
	}
	if fake.hadPatch {
		t.Fatal("a MachineSet being deleted must never receive a status patch (it's not the normal reconcile path)")
	}
}

// TestReconcileMachineSetsObservesMetrics confirms reconcileMachineSets
// wires Metrics.ObserveMachineSets into the same tick that patches
// MachineSet status, and that what lands in kairon_machineset_status
// matches what got patched -- not a second, independently-computed tally
// that could drift from it. Mirrors TestReconcileObservesQuotaMetrics
// (controller_test.go) and TestReconcileDisruptionBudgetsStatusObservesMetrics
// (disruption_test.go).
func TestReconcileMachineSetsObservesMetrics(t *testing.T) {
	ctl, _ := newMachineSetTestController(t)
	rec := metrics.NewRecorder()
	ctl.Metrics = rec

	ms := testMachineSet(3, "", "")
	hash := machineSetTemplateHash(ms.Spec.Template)
	owned := []model.Machine{ownedMachine("ms1-a", hash, "Running"), ownedMachine("ms1-b", hash, "Pending")}

	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)

	rr := httptest.NewRecorder()
	rec.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`kairon_machineset_status{field="replicas",machineset="ms1",namespace="prod"} 2`,
		`kairon_machineset_status{field="ready_replicas",machineset="ms1",namespace="prod"} 1`,
		`kairon_machineset_status{field="updated_replicas",machineset="ms1",namespace="prod"} 2`,
		`kairon_machineset_machine_info{machine="ms1-a",machineset="ms1",namespace="prod"} 1`,
		`kairon_machineset_machine_info{machine="ms1-b",machineset="ms1",namespace="prod"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// TestReconcileMachineSetsObservesMetricsEvenOnStepError confirms the
// replicas/readyReplicas/updatedReplicas tally reconcileMachineSet computed
// from the Machine snapshot still reaches ObserveMachineSets even when
// stepMachineSetToward's own create/delete call fails -- that tally is
// real, already-listed Machine state independent of whether the attempted
// mutation itself succeeded, the same "the count is real even if the write
// wasn't" reasoning reconcileDisruptionBudgetsStatus's own observed slice
// already relies on.
func TestReconcileMachineSetsObservesMetricsEvenOnStepError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every Machine create/delete/status-patch call fails outright, so
		// reconcileMachineSet always returns a non-nil error.
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	rec := metrics.NewRecorder()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: rec}

	ms := testMachineSet(3, "", "") // under-provisioned: 0 owned vs. 3 desired, always attempts a create
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, nil)

	rr := httptest.NewRecorder()
	rec.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `kairon_machineset_status{field="replicas",machineset="ms1",namespace="prod"} 0`) {
		t.Errorf("expected the pre-create owned count (0) to still be observed despite the create failing, got:\n%s", body)
	}
}

func TestStepMachineSetTowardRecreateDeletesOutdatedBeforeCreating(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(2, "Recreate", "")
	current := []model.Machine{ownedMachine("ms1-a", "newhash", "Running")}
	outdated := []model.Machine{ownedMachine("ms1-b", "oldhash", "Running")}
	if err := ctl.stepMachineSetToward(context.Background(), ms, "newhash", "Recreate", 2, current, outdated, 1); err != nil {
		t.Fatalf("stepMachineSetToward: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "ms1-b" {
		t.Fatalf("expected the outdated replica to be deleted first, got %v", fake.deleted)
	}
	if len(fake.created) != 0 {
		t.Fatal("expected no creation until every outdated replica is gone")
	}
}

func TestStepMachineSetTowardRollingUpdateBoundsByMaxUnavailable(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(5, "RollingUpdate", "2")
	var current, outdated []model.Machine
	for i := 0; i < 3; i++ {
		current = append(current, ownedMachine("cur-"+string(rune('a'+i)), "newhash", "Running"))
	}
	for i := 0; i < 2; i++ {
		outdated = append(outdated, ownedMachine("old-"+string(rune('a'+i)), "oldhash", "Running"))
	}
	// readyCurrent=3, outdated=2 -> available=5, maxUnavailable=2 -> canDelete = 5-(5-2) = 2
	if err := ctl.stepMachineSetToward(context.Background(), ms, "newhash", "RollingUpdate", 5, current, outdated, 3); err != nil {
		t.Fatalf("stepMachineSetToward: %v", err)
	}
	if len(fake.deleted) != 2 {
		t.Fatalf("expected exactly 2 outdated replicas deleted (maxUnavailable bound), got %d: %v", len(fake.deleted), fake.deleted)
	}
}

func TestStepMachineSetTowardRollingUpdateMakesProgressWhenOutdatedExceedsMaxUnavailable(t *testing.T) {
	// Regression guard: outdated count (5) exceeding maxUnavailable (1)
	// must not deadlock -- exactly maxUnavailable get deleted this step.
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(5, "RollingUpdate", "1")
	var outdated []model.Machine
	for i := 0; i < 5; i++ {
		outdated = append(outdated, ownedMachine("old-"+string(rune('a'+i)), "oldhash", "Running"))
	}
	if err := ctl.stepMachineSetToward(context.Background(), ms, "newhash", "RollingUpdate", 5, nil, outdated, 0); err != nil {
		t.Fatalf("stepMachineSetToward: %v", err)
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("expected exactly 1 deletion (maxUnavailable=1), got %d: %v", len(fake.deleted), fake.deleted)
	}
}

func TestStepMachineSetTowardRollingUpdateWithholdsDeletionUntilNewReplicaIsReady(t *testing.T) {
	// desired=3, maxUnavailable=1 -> minAvailable=2. One current-template
	// replica already exists but hasn't reached Running yet (readyCurrent=0),
	// and two outdated replicas are still Running. total (3) already
	// matches desired, so the old total-based check would have allowed
	// deleting 1 outdated replica here (3-2=1) -- but doing so would drop
	// actual available capacity (0 ready current + 1 remaining outdated =
	// 1) below minAvailable (2). Gating on readyCurrent must withhold the
	// deletion instead.
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(3, "RollingUpdate", "1")
	current := []model.Machine{ownedMachine("cur-a", "newhash", "Pending")}
	outdated := []model.Machine{ownedMachine("old-a", "oldhash", "Running"), ownedMachine("old-b", "oldhash", "Running")}
	if err := ctl.stepMachineSetToward(context.Background(), ms, "newhash", "RollingUpdate", 3, current, outdated, 0); err != nil {
		t.Fatalf("stepMachineSetToward: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("expected no deletion while the replacement replica isn't Ready yet, got %v", fake.deleted)
	}
}

func TestStepMachineSetTowardRollingUpdateResumesOnceReplicaBecomesReady(t *testing.T) {
	// Same shape as above, but the new replica has now reached Running:
	// readyCurrent=1, outdated=2 -> available=3, minAvailable=2 ->
	// canDelete=1. The rollout should make progress again.
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(3, "RollingUpdate", "1")
	current := []model.Machine{ownedMachine("cur-a", "newhash", "Running")}
	outdated := []model.Machine{ownedMachine("old-a", "oldhash", "Running"), ownedMachine("old-b", "oldhash", "Running")}
	if err := ctl.stepMachineSetToward(context.Background(), ms, "newhash", "RollingUpdate", 3, current, outdated, 1); err != nil {
		t.Fatalf("stepMachineSetToward: %v", err)
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("expected exactly 1 deletion now that the replacement is Ready, got %d: %v", len(fake.deleted), fake.deleted)
	}
}

func TestStepMachineSetTowardUnderProvisionedAlwaysCreatesRegardlessOfStrategy(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(4, "RollingUpdate", "1")
	current := []model.Machine{ownedMachine("cur-a", "newhash", "Running")}
	if err := ctl.stepMachineSetToward(context.Background(), ms, "newhash", "RollingUpdate", 4, current, nil, 1); err != nil {
		t.Fatalf("stepMachineSetToward: %v", err)
	}
	if len(fake.created) != 3 {
		t.Fatalf("expected 3 creations to fill the gap (4 desired - 1 current), got %d", len(fake.created))
	}
}

func TestResolveMaxUnavailableDefaultsToOne(t *testing.T) {
	n, err := resolveMaxUnavailable("", 10)
	if err != nil || n != 1 {
		t.Fatalf("expected default 1, got %d, err %v", n, err)
	}
}

func TestResolveMaxUnavailableParsesPercent(t *testing.T) {
	n, err := resolveMaxUnavailable("50%", 10)
	if err != nil || n != 5 {
		t.Fatalf("expected 5 for 50%% of 10, got %d, err %v", n, err)
	}
}

func TestResolveMaxUnavailableRejectsInvalid(t *testing.T) {
	if _, err := resolveMaxUnavailable("not-a-number", 10); err == nil {
		t.Fatal("expected an error for a malformed maxUnavailable")
	}
}

// TestMachineSetTemplateRevisionPatchFirstSighting: a MachineSet with no
// revision annotations gets the live template recorded as "current" and no
// "previous" keys at all.
func TestMachineSetTemplateRevisionPatchFirstSighting(t *testing.T) {
	ms := testMachineSet(1, "", "")
	hash := machineSetTemplateHash(ms.Spec.Template)
	patch, ok := machineSetTemplateRevisionPatch(ms, hash)
	if !ok {
		t.Fatal("expected a patch on first sighting")
	}
	ann := patch["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[model.AnnotationMachineSetCurrentTemplateHash] != hash {
		t.Errorf("current hash = %v, want %s", ann[model.AnnotationMachineSetCurrentTemplateHash], hash)
	}
	var got model.MachineTemplate
	if err := json.Unmarshal([]byte(ann[model.AnnotationMachineSetCurrentTemplate].(string)), &got); err != nil || machineSetTemplateHash(got) != hash {
		t.Errorf("current template does not round-trip to hash %s: %v", hash, err)
	}
	if _, present := ann[model.AnnotationMachineSetPreviousTemplateHash]; present {
		t.Errorf("no previous revision should be written on first sighting, got %v", ann)
	}
}

// TestMachineSetTemplateRevisionPatchNoopWhenInStep: once the recorded
// hash matches the live template (every steady-state tick, and right
// after a kaironctl rollback), nothing is patched.
func TestMachineSetTemplateRevisionPatchNoopWhenInStep(t *testing.T) {
	ms := testMachineSet(1, "", "")
	hash := machineSetTemplateHash(ms.Spec.Template)
	ms.Metadata.Annotations = map[string]string{model.AnnotationMachineSetCurrentTemplateHash: hash}
	if patch, ok := machineSetTemplateRevisionPatch(ms, hash); ok {
		t.Fatalf("expected no patch, got %v", patch)
	}
}

// TestMachineSetTemplateRevisionPatchShiftsCurrentToPrevious: a template
// change moves the recorded "current" pair to "previous" verbatim and
// records the live template as the new "current".
func TestMachineSetTemplateRevisionPatchShiftsCurrentToPrevious(t *testing.T) {
	old := testMachineSet(1, "", "")
	oldHash := machineSetTemplateHash(old.Spec.Template)
	oldBody, _ := json.Marshal(old.Spec.Template)

	ms := testMachineSet(1, "", "")
	ms.Spec.Template.Spec.Resources.CPU = "2"
	ms.Metadata.Annotations = map[string]string{
		model.AnnotationMachineSetCurrentTemplateHash: oldHash,
		model.AnnotationMachineSetCurrentTemplate:     string(oldBody),
	}
	hash := machineSetTemplateHash(ms.Spec.Template)
	patch, ok := machineSetTemplateRevisionPatch(ms, hash)
	if !ok {
		t.Fatal("expected a patch after a template change")
	}
	ann := patch["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[model.AnnotationMachineSetPreviousTemplateHash] != oldHash || ann[model.AnnotationMachineSetPreviousTemplate] != string(oldBody) {
		t.Errorf("previous = (%v, %v), want (%s, %s)", ann[model.AnnotationMachineSetPreviousTemplateHash], ann[model.AnnotationMachineSetPreviousTemplate], oldHash, oldBody)
	}
	if ann[model.AnnotationMachineSetCurrentTemplateHash] != hash {
		t.Errorf("current hash = %v, want %s", ann[model.AnnotationMachineSetCurrentTemplateHash], hash)
	}
}

// TestMachineSetTemplateRevisionPatchOversizedTemplateHashOnly: a template
// too large for an annotation is recorded by hash only, and its body
// annotation is explicitly deleted (JSON null) rather than left stale.
func TestMachineSetTemplateRevisionPatchOversizedTemplateHashOnly(t *testing.T) {
	ms := testMachineSet(1, "", "")
	ms.Spec.Template.Labels = map[string]string{"big": strings.Repeat("x", model.MaxMachineSetTemplateAnnotationBytes)}
	ms.Metadata.Annotations = map[string]string{
		model.AnnotationMachineSetCurrentTemplateHash: "aaaaaaaaaaaa",
		model.AnnotationMachineSetCurrentTemplate:     `{"spec":{}}`,
	}
	hash := machineSetTemplateHash(ms.Spec.Template)
	patch, _ := machineSetTemplateRevisionPatch(ms, hash)
	b, _ := json.Marshal(patch)
	var decoded struct {
		Metadata struct {
			Annotations map[string]*string `json:"annotations"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(b, &decoded)
	ann := decoded.Metadata.Annotations
	if v, present := ann[model.AnnotationMachineSetCurrentTemplate]; !present || v != nil {
		t.Errorf("oversized current template should be sent as null (delete), got present=%v value=%v", present, v)
	}
	if v := ann[model.AnnotationMachineSetPreviousTemplate]; v == nil || *v != `{"spec":{}}` {
		t.Errorf("previous template should carry the old recorded body, got %v", v)
	}
}

// TestReconcileMachineSetRecordsTemplateRevision drives the whole
// reconcile: the annotation patch is sent even while paused (it creates
// and deletes nothing).
func TestReconcileMachineSetRecordsTemplateRevision(t *testing.T) {
	ctl, fake := newMachineSetTestController(t)
	ms := testMachineSet(1, "", "")
	ms.Spec.Paused = true
	ms.Metadata.Finalizers = []string{model.FinalizerMachineSet}
	if _, err := ctl.reconcileMachineSet(context.Background(), ms, nil); err != nil {
		t.Fatalf("reconcileMachineSet: %v", err)
	}
	if len(fake.annotationPatches) != 1 || fake.annotationPatches[0][model.AnnotationMachineSetCurrentTemplateHash] != machineSetTemplateHash(ms.Spec.Template) {
		t.Fatalf("expected one revision annotation patch, got %v", fake.annotationPatches)
	}
	if len(fake.created) != 0 {
		t.Fatalf("paused MachineSet must not create replicas, got %d", len(fake.created))
	}
}
