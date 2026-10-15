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
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

func TestReconcileSchedulesMachine(t *testing.T) {
	var patchedNode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{{
				Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
				Spec:     model.MachineSpec{PowerState: "Running"},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			var p map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			patchedNode = p["spec"]["nodeName"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if patchedNode != "worker-1" {
		t.Fatalf("scheduled node = %q", patchedNode)
	}
}

// TestRunRecordsReconcileMetrics confirms Run's per-tick timing/error
// observation actually reaches Metrics -- kairon_reconcile_duration_seconds
// should have at least one sample after a couple of ticks.
func TestRunRecordsReconcileMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	rec := metrics.NewRecorder()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: rec}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = ctl.Run(ctx, 10*time.Millisecond)

	rr := httptest.NewRecorder()
	rec.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "kairon_reconcile_duration_seconds_count") {
		t.Errorf("missing reconcile duration histogram in:\n%s", body)
	}
}

// TestReconcileObservesQuotaMetrics confirms Reconcile wires
// Metrics.ObserveQuotas into the same tick that patches MachineQuota
// status, and that what lands in kairon_quota_resource matches what got
// patched -- not a second, independently-computed tally that could drift
// from it.
func TestReconcileObservesQuotaMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{{
				Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
				Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinequotas":
			max := 5
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: []model.MachineQuota{{
				Metadata: model.ObjectMeta{Name: "team-a", Namespace: "prod"},
				Spec:     model.MachineQuotaSpec{MaxMachines: &max, MaxTotalCPU: "10"},
			}}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinequotas/team-a/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	rec := metrics.NewRecorder()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: rec}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	rec.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`kairon_quota_resource{namespace="prod",quota="team-a",resource="machines",type="hard"} 5`,
		`kairon_quota_resource{namespace="prod",quota="team-a",resource="machines",type="used"} 1`,
		`kairon_quota_resource{namespace="prod",quota="team-a",resource="cpu_cores",type="hard"} 10`,
		`kairon_quota_resource{namespace="prod",quota="team-a",resource="cpu_cores",type="used"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestReconcileRespectsAntiAffinityAcrossRealMachines(t *testing.T) {
	primary := model.Machine{
		Metadata: model.ObjectMeta{Name: "primary", Namespace: "prod", Labels: map[string]string{"role": "db-primary"}},
		Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Running"},
	}
	replica := model.Machine{
		Metadata: model.ObjectMeta{Name: "replica", Namespace: "prod"},
		Spec: model.MachineSpec{
			PowerState: "Running",
			Placement: model.PlacementSpec{
				AntiAffinity: []model.MachineAffinityTerm{{LabelSelector: map[string]string{"role": "db-primary"}, TopologyKey: "kubernetes.io/hostname"}},
			},
		},
	}
	var patchedNode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{primary, replica}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var w1, w2 model.Node
			w1.Metadata.Name = "worker-1"
			w1.Metadata.Labels = map[string]string{model.CapableLabel: "true", "kubernetes.io/hostname": "worker-1"}
			w1.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			w2.Metadata.Name = "worker-2"
			w2.Metadata.Labels = map[string]string{model.CapableLabel: "true", "kubernetes.io/hostname": "worker-2"}
			w2.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{w1, w2}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/replica":
			var p map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			patchedNode = p["spec"]["nodeName"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if patchedNode != "worker-2" {
		t.Fatalf("scheduled replica onto %q, want worker-2 (worker-1 shares a hostname with the anti-affinity target)", patchedNode)
	}
}

func TestReconcileBlocksSchedulingOnceMachineQuotaExceeded(t *testing.T) {
	existing := model.Machine{
		Metadata: model.ObjectMeta{Name: "existing", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	pending := model.Machine{
		Metadata: model.ObjectMeta{Name: "pending", Namespace: "prod"},
		Spec:     model.MachineSpec{PowerState: "Running", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "prod-quota", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(1)},
	}
	var pendingStatus model.MachineStatus
	var quotaStatusPatched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{existing, pending}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinequotas":
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: []model.MachineQuota{quota}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/pending/status":
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			pendingStatus = p.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinequotas/prod-quota/status":
			quotaStatusPatched = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/prod/events":
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pendingStatus.Phase != "Pending" || !strings.Contains(pendingStatus.Message, "MachineQuota") {
		t.Fatalf("expected the second machine blocked by quota with a clear message, got %+v", pendingStatus)
	}
	if !quotaStatusPatched {
		t.Fatal("expected MachineQuota.status to be patched with observed usage")
	}
}

// TestReconcileBlockedQuotaRecordsEvent confirms admitQuota's blocker
// branch calls RecordEvent exactly once for a quota-blocked Machine, with
// the same blocker reason string already patched into status.Message --
// purely observing a decision already made, not a second independent
// computation of it.
func TestReconcileBlockedQuotaRecordsEvent(t *testing.T) {
	existing := model.Machine{
		Metadata: model.ObjectMeta{Name: "existing", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	pending := model.Machine{
		Metadata: model.ObjectMeta{Name: "pending", Namespace: "prod", UID: "pending-uid"},
		Spec:     model.MachineSpec{PowerState: "Running", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "prod-quota", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(1)},
	}
	var pendingStatus model.MachineStatus
	var events []model.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{existing, pending}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinequotas":
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: []model.MachineQuota{quota}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/pending/status":
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			pendingStatus = p.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinequotas/prod-quota/status":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/prod/events":
			var ev model.Event
			_ = json.NewDecoder(r.Body).Decode(&ev)
			events = append(events, ev)
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one RecordEvent POST, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.InvolvedObject.Kind != "Machine" || ev.InvolvedObject.Name != "pending" || ev.InvolvedObject.Namespace != "prod" || ev.InvolvedObject.UID != "pending-uid" {
		t.Errorf("unexpected involvedObject: %+v", ev.InvolvedObject)
	}
	if ev.Reason != "QuotaBlocked" {
		t.Errorf("expected reason QuotaBlocked, got %q", ev.Reason)
	}
	if ev.Type != "Warning" {
		t.Errorf("expected type Warning, got %q", ev.Type)
	}
	if ev.Message != pendingStatus.Message {
		t.Errorf("expected event message to match status.Message %q, got %q", pendingStatus.Message, ev.Message)
	}
}

// TestReconcileAdmitsHigherPriorityMachineFirst confirms spec.Priority
// actually changes who wins scarce capacity, not just what order the log
// lines print in. Two pending Machines (no NodeName yet, so both compete in
// the same tick) chase one MachineQuota slot (MaxMachines: 1); "low" is
// deliberately listed *before* "important" in the fake API's own response
// -- the exact ordering a Priority-less fleet would have relied on -- so a
// pass here can only mean SortByPriorityDesc, not incidental list order,
// decided the outcome.
func TestReconcileAdmitsHigherPriorityMachineFirst(t *testing.T) {
	low := model.Machine{
		Metadata: model.ObjectMeta{Name: "low", Namespace: "prod"},
		Spec:     model.MachineSpec{PowerState: "Running", Priority: 0, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	important := model.Machine{
		Metadata: model.ObjectMeta{Name: "important", Namespace: "prod"},
		Spec:     model.MachineSpec{PowerState: "Running", Priority: 10, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "prod-quota", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(1)},
	}
	scheduledMachines := map[string]string{}
	blockedMachines := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{low, important}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinequotas":
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: []model.MachineQuota{quota}})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/") && strings.HasSuffix(r.URL.Path, "/status"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"), "/status")
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			blockedMachines[name] = p.Status.Message
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"):
			name := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/")
			var p map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			scheduledMachines[name] = p["spec"]["nodeName"]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinequotas/prod-quota/status":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/prod/events":
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if scheduledMachines["important"] != "worker-1" {
		t.Fatalf("expected the higher-priority machine to be scheduled, got scheduled=%v blocked=%v", scheduledMachines, blockedMachines)
	}
	if _, stillScheduled := scheduledMachines["low"]; stillScheduled {
		t.Fatalf("expected the lower-priority machine to lose the quota race, got scheduled=%v", scheduledMachines)
	}
	if msg := blockedMachines["low"]; !strings.Contains(msg, "MachineQuota") {
		t.Fatalf("expected low to be blocked by MachineQuota, got message %q (blocked=%v)", msg, blockedMachines)
	}
}

func TestLiveCutoverSetsAdoptOnlyGuard(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running", RuntimeID: "vm-1"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}, Status: model.MachineMigrationStatus{Phase: "Cutover", SourceNode: "worker-1", TargetNode: "worker-2", EffectiveStrategy: "live", RuntimeID: "vm-1"}}
	guarded := false
	phase := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			spec, _ := p["spec"].(map[string]any)
			meta, _ := p["metadata"].(map[string]any)
			annotations, _ := meta["annotations"].(map[string]any)
			guarded = spec["nodeName"] == "worker-2" && annotations[model.AnnotationAdoptOnly] == "true"
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase = p.Status.Phase
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !guarded || phase != "Adopting" {
		t.Fatalf("guarded=%v phase=%q", guarded, phase)
	}
}

func TestSnapshotCreatesStandardCSIVolumeSnapshot(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Volumes: []model.MachineVolume{{Name: "data", ClaimName: "db-data"}}}}
	snapshot := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "before-upgrade", Namespace: "prod"}, Spec: model.MachineSnapshotSpec{MachineName: "db", VolumeSnapshotClassName: "csi-snap"}}
	createdPVC := ""
	statusReady := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: []model.MachineSnapshot{snapshot}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots/before-upgrade-data":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots":
			var vs model.VolumeSnapshot
			_ = json.NewDecoder(r.Body).Decode(&vs)
			if vs.Spec.Source.PersistentVolumeClaimName != nil {
				createdPVC = *vs.Spec.Source.PersistentVolumeClaimName
			}
			ready := true
			vs.Status.ReadyToUse = &ready
			_ = json.NewEncoder(w).Encode(vs)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/before-upgrade/status":
			var p struct {
				Status model.MachineSnapshotStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			statusReady = p.Status.Phase == "Succeeded" && p.Status.ReadyToUse
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if createdPVC != "db-data" || !statusReady {
		t.Fatalf("createdPVC=%q ready=%v", createdPVC, statusReady)
	}
}

func TestColdMigrationStopsSource(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "cold-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "cold", TargetNode: "worker-2"}}
	stopped := false
	phase := ""
	readyNode := func(name string) model.Node {
		var n model.Node
		n.Metadata.Name = name
		n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
		n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
		return n
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{readyNode("worker-1"), readyNode("worker-2")}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			var p map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			stopped = p["spec"]["powerState"] == "Stopped"
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/cold-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase = p.Status.Phase
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stopped || phase != "Stopping" {
		t.Fatalf("stopped=%v phase=%q", stopped, phase)
	}
}

func TestColdMigrationReassignsAfterSourceStopped(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Stopped"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Stopped"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "cold-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "cold", TargetNode: "worker-2"}, Status: model.MachineMigrationStatus{Phase: "Stopping", SourceNode: "worker-1", TargetNode: "worker-2", EffectiveStrategy: "cold"}}
	reassigned := false
	phase := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			var p map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&p)
			reassigned = p["spec"]["nodeName"] == "worker-2" && p["spec"]["powerState"] == "Running"
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/cold-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase = p.Status.Phase
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reassigned || phase != "Restarting" {
		t.Fatalf("reassigned=%v phase=%q", reassigned, phase)
	}
}

func readyCapableNode(name string) model.Node {
	var n model.Node
	n.Metadata.Name = name
	n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
	n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
	return n
}

func TestLiveMigrationEntersStarting(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Runtime: model.RuntimeSpec{Backend: "qemu"}}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}}
	phase, strategy := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2")}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase = p.Status.Phase
			strategy = p.Status.EffectiveStrategy
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Starting" || strategy != "live" {
		t.Fatalf("phase=%q strategy=%q", phase, strategy)
	}
}

func TestLiveMigrationAdoptingCompletesToSucceeded(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-2", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-2", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}, Status: model.MachineMigrationStatus{Phase: "Adopting", SourceNode: "worker-1", TargetNode: "worker-2", EffectiveStrategy: "live"}}
	phase := ""
	clearedAnnotations := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			meta, _ := p["metadata"].(map[string]any)
			annotations, _ := meta["annotations"].(map[string]any)
			_, hasAdoptOnly := annotations[model.AnnotationAdoptOnly]
			_, hasRef := annotations[model.AnnotationMigrationRef]
			clearedAnnotations = hasAdoptOnly && annotations[model.AnnotationAdoptOnly] == nil && hasRef && annotations[model.AnnotationMigrationRef] == nil
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase = p.Status.Phase
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Succeeded" || !clearedAnnotations {
		t.Fatalf("phase=%q clearedAnnotations=%v", phase, clearedAnnotations)
	}
}

func TestColdMigrationRestartingCompletesToSucceeded(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-2", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-2", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "cold-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "cold", TargetNode: "worker-2"}, Status: model.MachineMigrationStatus{Phase: "Restarting", SourceNode: "worker-1", TargetNode: "worker-2", EffectiveStrategy: "cold"}}
	phase := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/cold-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase = p.Status.Phase
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Succeeded" {
		t.Fatalf("phase=%q", phase)
	}
}

func TestReconcileMigrationSkipsTerminalPhases(t *testing.T) {
	for _, terminal := range []string{"Succeeded", "Failed", "Blocked", "NeedsRecovery", "Cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Labels: map[string]string{model.AssignedNodeLabel: "worker-1"}}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
			migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}, Status: model.MachineMigrationStatus{Phase: terminal}}
			// worker-1 must be Ready in this fixture, or fencing detection
			// (an orthogonal concern -- see fencing.go) would itself PATCH
			// the Machine's status, which is not what this test is about.
			// AssignedNodeLabel must already match spec.nodeName so label
			// sync (also orthogonal) does not PATCH either.
			patched := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
					_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
					_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{readyCapableNode("worker-1")}})
				case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
					_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
				case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
					http.NotFound(w, r)
				case r.Method == http.MethodPatch:
					patched = true
					w.WriteHeader(http.StatusOK)
				default:
					http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			kc, _ := kube.New(srv.URL, "", "", false)
			kc.HTTP = srv.Client()
			ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := ctl.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if patched {
				t.Fatalf("expected no PATCH for terminal phase %q", terminal)
			}
		})
	}
}

func TestMigrationBlockedWhenMachineUnscheduled(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{PowerState: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}}
	phase, message := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Blocked" || message != "Machine has not been scheduled yet" {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

// reconcileMigrationBlockedCheck drives one Reconcile for a single seeded
// Machine/MachineMigration/node-list fixture and returns the migration
// status PATCH's phase/message, matching the shared setup all "Blocked"
// scenarios need.
func reconcileMigrationBlockedCheck(t *testing.T, machine model.Machine, nodes []model.Node, migration model.MachineMigration) (phase, message string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: nodes})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/machinemigrations/"+migration.Metadata.Name+"/status"):
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return phase, message
}

func TestMigrationBlockedWhenNoEligibleTarget(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}}
	// Only the source node exists (Choose excludes it), so no target is eligible.
	phase, message := reconcileMigrationBlockedCheck(t, machine, []model.Node{readyCapableNode("worker-1")}, migration)
	if phase != "Blocked" || !strings.Contains(message, "no migration target available") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestMigrationBlockedWhenStrategyUnsupported(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "warm"}}
	phase, message := reconcileMigrationBlockedCheck(t, machine, []model.Node{readyCapableNode("worker-2")}, migration)
	if phase != "Blocked" || !strings.Contains(message, `unsupported migration strategy "warm"`) {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestMigrationBlockedWhenLiveOnNonQemuBackend(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Runtime: model.RuntimeSpec{Backend: "container"}}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}}
	phase, message := reconcileMigrationBlockedCheck(t, machine, []model.Node{readyCapableNode("worker-2")}, migration)
	if phase != "Blocked" || !strings.Contains(message, "live migration requires qemu backend") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestReconcileFailedWrapperPatchesFailedStatus(t *testing.T) {
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: ""}}
	phase, message := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Failed" || !strings.Contains(message, "spec.machineName is required") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestReconcileMigrationUnknownPhaseErrors(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	migration := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}, Status: model.MachineMigrationStatus{Phase: "Bogus"}}
	phase, message := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{migration}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Failed" || !strings.Contains(message, `unknown migration status phase "Bogus"`) {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestEffectiveStrategyAutoFallsBackToCold(t *testing.T) {
	machine := model.Machine{}
	for _, strategy := range []string{"", "auto"} {
		got, err := effectiveStrategy(machine, model.MachineMigration{Spec: model.MachineMigrationSpec{Strategy: strategy}})
		if err != nil || got != "cold" {
			t.Fatalf("strategy=%q got=%q err=%v", strategy, got, err)
		}
	}
}

func TestEffectiveStrategyRejectsUnsupported(t *testing.T) {
	_, err := effectiveStrategy(model.Machine{}, model.MachineMigration{Spec: model.MachineMigrationSpec{Strategy: "warm"}})
	if err == nil || !strings.Contains(err.Error(), `unsupported migration strategy "warm"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestEffectiveStrategyLiveRequiresQemu(t *testing.T) {
	for _, backend := range []string{"", "auto", "qemu"} {
		machine := model.Machine{Spec: model.MachineSpec{Runtime: model.RuntimeSpec{Backend: backend}}}
		got, err := effectiveStrategy(machine, model.MachineMigration{Spec: model.MachineMigrationSpec{Strategy: "live"}})
		if err != nil || got != "live" {
			t.Fatalf("backend=%q got=%q err=%v", backend, got, err)
		}
	}
	machine := model.Machine{Spec: model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "container"}}}
	if _, err := effectiveStrategy(machine, model.MachineMigration{Spec: model.MachineMigrationSpec{Strategy: "live"}}); err == nil {
		t.Fatal("expected an error for a non-qemu backend")
	}
}

func TestSnapshotFailsOnMissingVolumeFields(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Volumes: []model.MachineVolume{{Name: "data", ClaimName: ""}}}}
	snapshot := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "before-upgrade", Namespace: "prod"}, Spec: model.MachineSnapshotSpec{MachineName: "db"}}
	phase, message := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: []model.MachineSnapshot{snapshot}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/before-upgrade/status":
			var p struct {
				Status model.MachineSnapshotStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Failed" || !strings.Contains(message, "machine volume requires name and claimName") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestSnapshotFailsOnVolumeSnapshotError(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Volumes: []model.MachineVolume{{Name: "data", ClaimName: "db-data"}}}}
	snapshot := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "before-upgrade", Namespace: "prod"}, Spec: model.MachineSnapshotSpec{MachineName: "db"}}
	phase, message := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: []model.MachineSnapshot{snapshot}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots/before-upgrade-data":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots":
			var vs model.VolumeSnapshot
			_ = json.NewDecoder(r.Body).Decode(&vs)
			msg := "backend quota exceeded"
			vs.Status.Error = &model.VolumeSnapshotError{Message: &msg}
			_ = json.NewEncoder(w).Encode(vs)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/before-upgrade/status":
			var p struct {
				Status model.MachineSnapshotStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Failed" || !strings.Contains(message, "backend quota exceeded") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestSnapshotPendingWhenNotYetReady(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running", Volumes: []model.MachineVolume{{Name: "data", ClaimName: "db-data"}}}}
	snapshot := model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "before-upgrade", Namespace: "prod"}, Spec: model.MachineSnapshotSpec{MachineName: "db"}}
	phase, message := "", ""
	var readyToUse *bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: []model.MachineSnapshot{snapshot}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots/before-upgrade-data":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/snapshot.storage.k8s.io/v1/namespaces/prod/volumesnapshots":
			var vs model.VolumeSnapshot
			_ = json.NewDecoder(r.Body).Decode(&vs)
			// vs.Status.ReadyToUse left nil -- not yet ready.
			_ = json.NewEncoder(w).Encode(vs)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/before-upgrade/status":
			var p struct {
				Status model.MachineSnapshotStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			readyToUse = &p.Status.ReadyToUse
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase != "Pending" || !strings.Contains(message, "waiting for volume snapshots") || readyToUse == nil || *readyToUse {
		t.Fatalf("phase=%q message=%q readyToUse=%v", phase, message, readyToUse)
	}
}

// runConcurrencyQuotaCheck seeds one already-active migration (touching
// activeSourceNode/activeTargetNode) plus a Pending migration for "db" on
// worker-1, then reconciles with the given concurrency limits and returns
// the Pending migration's resulting phase/message.
func runConcurrencyQuotaCheck(t *testing.T, maxPerNode, maxCluster int, activeSourceNode, activeTargetNode string) (phase, message string) {
	t.Helper()
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	active := model.MachineMigration{Metadata: model.ObjectMeta{Name: "active-mig", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "other"}, Status: model.MachineMigrationStatus{Phase: "Running", SourceNode: activeSourceNode, TargetNode: activeTargetNode, EffectiveStrategy: "live"}}
	pending := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2")}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{active, pending}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/active-mig/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxConcurrentPerNode: maxPerNode, MaxConcurrentCluster: maxCluster}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return phase, message
}

// runSameMachineCheck seeds a migration of "db" in activePhase plus a Pending
// migration of the same Machine, reconciles, and returns what happened to the
// Pending one.
func runSameMachineCheck(t *testing.T, activePhase string) (phase, message string) {
	t.Helper()
	maxPerNode, maxCluster, activeSourceNode, activeTargetNode := 0, 0, "worker-1", "worker-2"
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
	active := model.MachineMigration{Metadata: model.ObjectMeta{Name: "active-mig", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db"}, Status: model.MachineMigrationStatus{Phase: activePhase, SourceNode: activeSourceNode, TargetNode: activeTargetNode}}
	pending := model.MachineMigration{Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db", Strategy: "live"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2")}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{active, pending}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
			var p struct {
				Status model.MachineMigrationStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			phase, message = p.Status.Phase, p.Status.Message
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/active-mig/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxConcurrentPerNode: maxPerNode, MaxConcurrentCluster: maxCluster}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return phase, message
}

func TestSecondMigrationOfTheSameMachineIsBlockedWhileOneIsActive(t *testing.T) {
	// Adopting is the phase that mattered in the lab: the VM is already
	// running on the target, so a second (cold) migration used to stop and
	// delete it.
	for _, active := range []string{"Starting", "Running", "Cutover", "Adopting", "Stopping", "Restarting", "NeedsRecovery"} {
		t.Run(active, func(t *testing.T) {
			phase, message := runSameMachineCheck(t, active)
			if phase != "Blocked" || !strings.Contains(message, "another migration of this Machine is in progress") || !strings.Contains(message, "active-mig ("+active+")") {
				t.Fatalf("phase=%q message=%q", phase, message)
			}
		})
	}
}

func TestMigrationOfTheSameMachineIsAdmittedOnceThePreviousOneFinished(t *testing.T) {
	for _, done := range []string{"Succeeded", "Failed", "Blocked", "Cancelled"} {
		t.Run(done, func(t *testing.T) {
			phase, _ := runSameMachineCheck(t, done)
			if phase != "Starting" {
				t.Fatalf("a finished %s migration must not block a new one: phase=%q", done, phase)
			}
		})
	}
}

func TestMigrationOfAnotherMachineIsNotBlockedBySameMachineGuard(t *testing.T) {
	// runConcurrencyQuotaCheck's active migration is for Machine "other".
	phase, _ := runConcurrencyQuotaCheck(t, 0, 0, "worker-1", "worker-2")
	if phase != "Starting" {
		t.Fatalf("phase=%q, want Starting", phase)
	}
}

func TestMigrationBlockedWhenPerNodeConcurrencyLimitReached(t *testing.T) {
	// The active migration's source node is worker-1, same as "db"'s node --
	// with a per-node limit of 1, the new Pending migration must be blocked.
	phase, message := runConcurrencyQuotaCheck(t, 1, 0, "worker-1", "worker-2")
	if phase != "Blocked" || !strings.Contains(message, "concurrent migration limit") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestMigrationBlockedWhenClusterConcurrencyLimitReached(t *testing.T) {
	// The active migration touches unrelated nodes, but a cluster-wide
	// limit of 1 must still block admitting a second migration.
	phase, message := runConcurrencyQuotaCheck(t, 0, 1, "worker-3", "worker-4")
	if phase != "Blocked" || !strings.Contains(message, "cluster migration concurrency limit") {
		t.Fatalf("phase=%q message=%q", phase, message)
	}
}

func TestMigrationAdmittedWhenUnderConcurrencyLimit(t *testing.T) {
	// Same active migration, but limits are high enough to admit the second.
	phase, _ := runConcurrencyQuotaCheck(t, 5, 5, "worker-1", "worker-2")
	if phase != "Starting" {
		t.Fatalf("phase=%q, want Starting", phase)
	}
}

// A migration in Cutover whose Machine patch hits a throttled/failed API
// server must not be failed: the VM may already have moved.
func TestTransientAPIErrorDoesNotFailAMigration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		patchCode  int
		wantFailed bool
	}{
		{"503 from the API server", http.StatusServiceUnavailable, false},
		{"429 throttled", http.StatusTooManyRequests, false},
		{"403 forbidden is not transient", http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}, Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running"}}
			mig := model.MachineMigration{
				Metadata: model.ObjectMeta{Name: "move-db", Namespace: "prod"},
				Spec:     model.MachineMigrationSpec{MachineName: "db", Strategy: "live"},
				Status:   model.MachineMigrationStatus{Phase: "Cutover", EffectiveStrategy: "live", SourceNode: "worker-1", TargetNode: "worker-2"},
			}
			var phase string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
					_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
					_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{readyCapableNode("worker-1"), readyCapableNode("worker-2")}})
				case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
					_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: []model.MachineMigration{mig}})
				case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshots":
					http.NotFound(w, r)
				case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
					http.Error(w, "stalled", tc.patchCode)
				case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/move-db/status":
					var p struct {
						Status model.MachineMigrationStatus `json:"status"`
					}
					_ = json.NewDecoder(r.Body).Decode(&p)
					phase = p.Status.Phase
					w.WriteHeader(http.StatusOK)
				default:
					http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			kc, _ := kube.New(srv.URL, "", "", false)
			kc.HTTP = srv.Client()
			ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := ctl.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.wantFailed && phase != "Failed" {
				t.Fatalf("a non-transient error must still fail the migration, phase=%q", phase)
			}
			if !tc.wantFailed && phase != "" {
				t.Fatalf("a transient error must leave the migration alone, but it was patched to %q", phase)
			}
		})
	}
}
