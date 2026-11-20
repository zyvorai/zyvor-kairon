// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package integration drives kairon's controller and agent reconcile loops
// together, in sequence, against one shared mutable fake Kubernetes backend
// -- unlike internal/controller and internal/agent's own tests, which only
// exercise each reconciler in isolation. This is still a pure in-process
// httptest test (no real Kubernetes/QEMU), so it's covered by the existing
// `go test ./...`/`make test-race`/`make cover-check` targets with no new
// CI job needed.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/agent"
	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// --- shared, mutable fake Kubernetes backend ---

type fakeCluster struct {
	mu        sync.Mutex
	machine   model.Machine
	migration model.MachineMigration
	nodes     []model.Node
}

func mergePatch(dst map[string]any, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if patchMap, ok := v.(map[string]any); ok {
			dstMap, ok := dst[k].(map[string]any)
			if !ok {
				dstMap = map[string]any{}
			}
			mergePatch(dstMap, patchMap)
			dst[k] = dstMap
			continue
		}
		dst[k] = v
	}
}

// applyMergePatch decodes r's body as a JSON merge-patch (RFC 7386) and
// applies it onto *obj, round-tripping through map[string]any so a single
// generic function handles every shape controller.go/agent.go actually
// sends (spec field replace, metadata.annotations with null-deletes,
// metadata.finalizers list replace, and full status replaces).
func applyMergePatch(r *http.Request, obj any) error {
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		return err
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	var dst map[string]any
	if err := json.Unmarshal(raw, &dst); err != nil {
		return err
	}
	mergePatch(dst, patch)
	merged, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	// json.Unmarshal into an already-populated map field merges rather
	// than replaces (existing keys absent from the new JSON are left
	// alone) -- zero every map reachable from *obj first so a
	// merge-patch delete (an absent key after mergePatch) actually
	// takes effect. Pointer/struct fields are left untouched (only
	// recursed into): several callers here pass a pointer that aliases
	// a field inside a live struct (e.g. &c.migration.Status), and
	// json.Unmarshal's own pointer handling -- reuse the existing
	// pointee rather than allocate a new one -- is what makes that
	// aliasing observe the update. Zeroing the pointer itself would
	// make Unmarshal allocate a fresh value instead, silently orphaning
	// the caller's original field.
	zeroMaps(reflect.ValueOf(obj))
	return json.Unmarshal(merged, obj)
}

func zeroMaps(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			zeroMaps(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				zeroMaps(f)
			}
		}
	case reflect.Map:
		if !v.IsNil() {
			v.Set(reflect.Zero(v.Type()))
		}
	}
}

func (c *fakeCluster) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{c.machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			_ = json.NewEncoder(w).Encode(c.machine)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: c.nodes})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{c.migration}})
		case r.Method == http.MethodGet && (r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots" ||
			r.URL.Path == "/apis/kairon.zyvor.dev/v1/networksecuritygroups" ||
			r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinenetworkpolicies"):
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db/status":
			if err := applyMergePatch(r, &struct {
				Status *model.MachineStatus `json:"status"`
			}{&c.machine.Status}); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			if err := applyMergePatch(r, &c.machine); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			if err := applyMergePatch(r, &struct {
				Status *model.MachineMigrationStatus `json:"status"`
			}{&c.migration.Status}); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db":
			if err := applyMergePatch(r, &c.migration); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func readyCapableNode(name string) model.Node {
	var n model.Node
	n.Metadata.Name = name
	n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
	n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
	return n
}

// --- stateful test doubles (duplicated from internal/agent's own unexported
// fakes -- can't import unexported test types across packages) ---

type statefulSourceMigrator struct {
	mu          sync.Mutex
	pollResults []migration.TransferStatus // successive Status() calls advance through this; last one repeats
	idx         int
	starts      int
}

func (f *statefulSourceMigrator) Start(context.Context, migration.SourceRequest) (migration.TransferStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	return f.pollResults[0], nil
}
func (f *statefulSourceMigrator) Status(context.Context, migration.Session, string) (migration.TransferStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.idx
	if i >= len(f.pollResults) {
		i = len(f.pollResults) - 1
	} else {
		f.idx++
	}
	return f.pollResults[i], nil
}
func (f *statefulSourceMigrator) Abort(context.Context, migration.Session, string) error { return nil }

type fakePeerDestination struct {
	result    migration.PrepareResult
	commitErr error
}

func (f *fakePeerDestination) Prepare(context.Context, migration.Session) (migration.PrepareResult, error) {
	return f.result, nil
}
func (f *fakePeerDestination) Commit(context.Context, migration.Session) error { return f.commitErr }
func (f *fakePeerDestination) Abort(context.Context, migration.Session) error  { return nil }

// fluxVMRunning returns a handler answering the FluxVM REST calls
// reconcileMachine/projectNetworkStatus make for a VM that's simply running
// -- GET /v1/vms/{id} and the by-name lookup used at Adopting-completion.
func fluxVMRunning(id, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/"+id:
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: id, Name: name, Status: "Running"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms" && r.URL.Query().Get("name") == name:
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{{UUID: id, Name: name, Status: "Running"}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

// --- the tests ---

func TestMigrationPipelineEndToEnd(t *testing.T) {
	cluster := &fakeCluster{
		machine: model.Machine{
			Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer}},
			Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Image: model.ImageSpec{Path: "/images/db.qcow2"}, Runtime: model.RuntimeSpec{Backend: "qemu"}},
			Status:   model.MachineStatus{NodeName: "worker-1", Phase: "Running", RuntimeID: "vm-1"},
		},
		migration: model.MachineMigration{
			Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod", UID: "migration-uid-1"},
			Spec:     model.MachineMigrationSpec{MachineName: "db", Strategy: "live"},
		},
		nodes: []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2")},
	}
	ks := httptest.NewServer(cluster.handler())
	defer ks.Close()

	destination := &fakePeerDestination{result: migration.PrepareResult{TransferSupported: true, Endpoint: "opaque://incoming/session", Backend: "test-adapter"}}
	peerServer := httptest.NewServer((&migration.Server{NodeName: "worker-2", Store: migration.NewFileStore(t.TempDir()), Driver: destination}).Handler())
	defer peerServer.Close()

	sourceFlux := httptest.NewServer(fluxVMRunning("vm-1", "kairon-prod-db"))
	defer sourceFlux.Close()
	destFlux := httptest.NewServer(fluxVMRunning("vm-2", "kairon-prod-db"))
	defer destFlux.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kcSource, _ := kube.New(ks.URL, "", "", false)
	kcSource.HTTP = ks.Client()
	fcSource := fluxvm.New(sourceFlux.URL, "")
	fcSource.HTTP = sourceFlux.Client()

	source := &statefulSourceMigrator{idx: 1, pollResults: []migration.TransferStatus{
		{TransferID: "xfer-1", Phase: "running", RAMTotal: 4096, RAMTransferred: 1024},
		{TransferID: "xfer-1", Phase: "completed", RAMTotal: 4096, RAMTransferred: 4096},
	}}
	ag1 := &agent.Agent{
		NodeName: "worker-1", Kube: kcSource, Flux: fcSource, DefaultBackend: "qemu",
		MigrationPeer:    migration.NewClient(peerServer.Client()),
		SourceMigrator:   source,
		MigrationPeerURL: func(context.Context, string) (string, error) { return peerServer.URL, nil },
		Log:              log,
	}
	ctl := &controller.Controller{Kube: kcSource, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: log}

	// 1. Pending -> Starting (controller resolves target/strategy).
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Starting" {
		t.Fatalf("after step 1: phase=%q", cluster.migration.Status.Phase)
	}

	// 2. Starting: agent prepares the peer, starts the transfer -> Running.
	if err := ag1.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Running" {
		t.Fatalf("after step 2: phase=%q message=%q", cluster.migration.Status.Phase, cluster.migration.Status.Message)
	}
	if destination.result.Endpoint == "" || source.starts != 1 {
		t.Fatalf("expected prepare+start to have run: starts=%d", source.starts)
	}

	// 3. Running: agent polls status -> completed -> commits -> Cutover.
	if err := ag1.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Cutover" {
		t.Fatalf("after step 3: phase=%q message=%q", cluster.migration.Status.Phase, cluster.migration.Status.Message)
	}

	// 4. Cutover: controller flips Machine to the target node, adopt-only -> Adopting.
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Adopting" {
		t.Fatalf("after step 4: phase=%q", cluster.migration.Status.Phase)
	}
	if cluster.machine.Spec.NodeName != "worker-2" {
		t.Fatalf("expected Machine reassigned to worker-2, got %q", cluster.machine.Spec.NodeName)
	}

	// 5. The destination node's own agent reconciles the adopt-only Machine
	// (simulating the destination's completed QEMU incoming migration).
	kcDest, _ := kube.New(ks.URL, "", "", false)
	kcDest.HTTP = ks.Client()
	fcDest := fluxvm.New(destFlux.URL, "")
	fcDest.HTTP = destFlux.Client()
	ag2 := &agent.Agent{NodeName: "worker-2", Kube: kcDest, Flux: fcDest, DefaultBackend: "qemu", Log: log}
	if err := ag2.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.machine.Status.NodeName != "worker-2" || cluster.machine.Status.Phase != "Running" {
		t.Fatalf("expected destination agent to adopt the Machine, got status=%+v", cluster.machine.Status)
	}

	// 6. Adopting -> Succeeded (controller sees the Machine fully adopted).
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Succeeded" {
		t.Fatalf("final phase=%q, want Succeeded", cluster.migration.Status.Phase)
	}
	if _, ok := cluster.machine.Metadata.Annotations[model.AnnotationAdoptOnly]; ok {
		t.Errorf("expected adopt-only annotation to be cleared on Succeeded, got annotations=%+v", cluster.machine.Metadata.Annotations)
	}
}

func TestMigrationPipelineNeedsRecovery(t *testing.T) {
	cluster := &fakeCluster{
		machine: model.Machine{
			Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer}},
			Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Image: model.ImageSpec{Path: "/images/db.qcow2"}, Runtime: model.RuntimeSpec{Backend: "qemu"}},
			Status:   model.MachineStatus{NodeName: "worker-1", Phase: "Running", RuntimeID: "vm-1"},
		},
		migration: model.MachineMigration{
			Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod", UID: "migration-uid-1"},
			Spec:     model.MachineMigrationSpec{MachineName: "db", Strategy: "live"},
		},
		nodes: []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2")},
	}
	ks := httptest.NewServer(cluster.handler())
	defer ks.Close()

	destination := &fakePeerDestination{
		result:    migration.PrepareResult{TransferSupported: true, Endpoint: "opaque://incoming/session"},
		commitErr: fmt.Errorf("commit failed: destination unreachable"),
	}
	peerServer := httptest.NewServer((&migration.Server{NodeName: "worker-2", Store: migration.NewFileStore(t.TempDir()), Driver: destination}).Handler())
	defer peerServer.Close()

	sourceFlux := httptest.NewServer(fluxVMRunning("vm-1", "kairon-prod-db"))
	defer sourceFlux.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kcSource, _ := kube.New(ks.URL, "", "", false)
	kcSource.HTTP = ks.Client()
	fcSource := fluxvm.New(sourceFlux.URL, "")
	fcSource.HTTP = sourceFlux.Client()

	source := &statefulSourceMigrator{idx: 1, pollResults: []migration.TransferStatus{
		{TransferID: "xfer-1", Phase: "running", RAMTotal: 4096, RAMTransferred: 1024},
		{TransferID: "xfer-1", Phase: "completed", RAMTotal: 4096, RAMTransferred: 4096},
	}}
	ag := &agent.Agent{
		NodeName: "worker-1", Kube: kcSource, Flux: fcSource, DefaultBackend: "qemu",
		MigrationPeer:    migration.NewClient(peerServer.Client()),
		SourceMigrator:   source,
		MigrationPeerURL: func(context.Context, string) (string, error) { return peerServer.URL, nil },
		Log:              log,
	}
	ctl := &controller.Controller{Kube: kcSource, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: log}

	if err := ctl.Reconcile(context.Background()); err != nil { // Pending -> Starting
		t.Fatal(err)
	}
	if err := ag.Reconcile(context.Background()); err != nil { // Starting: prepare+start -> Running
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Running" {
		t.Fatalf("expected Running before the commit attempt, got %q", cluster.migration.Status.Phase)
	}
	if err := ctl.Reconcile(context.Background()); err != nil { // controller no-ops on Running
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "Running" {
		t.Fatalf("controller must not touch a Running migration, got %q", cluster.migration.Status.Phase)
	}

	if err := ag.Reconcile(context.Background()); err != nil { // Running: completed -> commit fails -> NeedsRecovery
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "NeedsRecovery" {
		t.Fatalf("expected NeedsRecovery, got %q message=%q", cluster.migration.Status.Phase, cluster.migration.Status.Message)
	}

	// A further agent tick must refresh the diagnosis but never advance the
	// phase on its own (reconcileNeedsRecovery never auto-resolves).
	if err := ag.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "NeedsRecovery" {
		t.Fatalf("expected NeedsRecovery to remain parked, got %q", cluster.migration.Status.Phase)
	}
	if cluster.migration.Status.Recovery == nil || cluster.migration.Status.Recovery.DiagnosedAt == nil {
		t.Fatal("expected status.recovery diagnosis to be populated")
	}

	if err := ctl.Reconcile(context.Background()); err != nil { // controller no-ops on NeedsRecovery too
		t.Fatal(err)
	}
	if cluster.migration.Status.Phase != "NeedsRecovery" {
		t.Fatalf("controller must not touch NeedsRecovery, got %q", cluster.migration.Status.Phase)
	}
}
