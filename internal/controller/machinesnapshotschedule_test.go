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
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func snapshotSchedule(name string, lastRun time.Time, intervalSeconds int) model.MachineSnapshotSchedule {
	return model.MachineSnapshotSchedule{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSnapshotScheduleSpec{Selector: map[string]string{"tier": "web"}, IntervalSeconds: intervalSeconds},
		Status:   model.MachineSnapshotScheduleStatus{LastRunTime: lastRun},
	}
}

func TestReconcileMachineSnapshotSchedulesNotDueCreatesNothing(t *testing.T) {
	var createCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{
				snapshotSchedule("hourly", time.Now(), 3600),
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			createCalled = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if createCalled {
		t.Fatal("expected no MachineSnapshot to be created for a schedule that isn't due yet")
	}
}

func TestReconcileMachineSnapshotSchedulesDueCreatesOnePerMatch(t *testing.T) {
	var created []string
	var patchedStatus model.MachineSnapshotScheduleStatus
	var patchedName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{
				snapshotSchedule("hourly", time.Time{}, 3600), // never run -- immediately due
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			created = append(created, s.Spec.MachineName)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			patchedName = "hourly"
			var body map[string]model.MachineSnapshotScheduleStatus
			_ = json.NewDecoder(r.Body).Decode(&body)
			patchedStatus = body["status"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{
		webMachine("web-1", "node-a", "Running"),
		webMachine("web-2", "node-a", "Running"),
		{Metadata: model.ObjectMeta{Name: "db-1", Namespace: "prod", Labels: map[string]string{"tier": "db"}}},
	}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created = %v, want 2 snapshots (one per web-* Machine, db-1 excluded)", created)
	}
	if patchedName != "hourly" {
		t.Fatal("expected a status patch for the due schedule")
	}
	if patchedStatus.LastRunSnapshotCount != 2 {
		t.Fatalf("LastRunSnapshotCount = %d, want 2", patchedStatus.LastRunSnapshotCount)
	}
	if patchedStatus.LastRunTime.IsZero() {
		t.Fatal("expected LastRunTime to be set")
	}
	if patchedStatus.LastRunError != "" {
		t.Fatalf("LastRunError = %q, want empty", patchedStatus.LastRunError)
	}
	wantNextRun := patchedStatus.LastRunTime.Add(time.Hour)
	if !patchedStatus.NextRunTime.Equal(wantNextRun) {
		t.Fatalf("NextRunTime = %v, want %v (LastRunTime + the 3600s interval)", patchedStatus.NextRunTime, wantNextRun)
	}
}

// TestReconcileMachineSnapshotSchedulesDueCreatesUniqueNamesPerMachine
// guards against a real name collision that
// TestReconcileMachineSnapshotSchedulesDueCreatesOnePerMatch's own fake
// server can't catch: `now` (the create-name timestamp) is computed once
// per reconcile tick, so if the generated MachineSnapshot name were built
// from only the schedule name and that timestamp -- as it once was --
// every Machine matching the same schedule in the same tick would get the
// exact same name. A real Kubernetes apiserver enforces per-namespace name
// uniqueness and would reject every create after the first with 409
// AlreadyExists, silently dropping snapshots for every Machine after the
// first one on every single fire of any schedule matching more than one
// Machine. This fake server reproduces that uniqueness enforcement
// (something the plain always-200 fake used elsewhere in this file does
// not), so it actually fails if the collision regresses.
func TestReconcileMachineSnapshotSchedulesDueCreatesUniqueNamesPerMachine(t *testing.T) {
	created := map[string]bool{}
	var createdNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{
				snapshotSchedule("hourly", time.Time{}, 3600), // never run -- immediately due
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			if created[s.Metadata.Name] {
				// Mirrors a real apiserver's 409 AlreadyExists on a
				// duplicate name within the same namespace.
				http.Error(w, "machinesnapshots.kairon.zyvor.dev \""+s.Metadata.Name+"\" already exists", http.StatusConflict)
				return
			}
			created[s.Metadata.Name] = true
			createdNames = append(createdNames, s.Metadata.Name)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{
		webMachine("web-1", "node-a", "Running"),
		webMachine("web-2", "node-a", "Running"),
	}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if len(createdNames) != 2 {
		t.Fatalf("created %v (%d), want 2 distinctly-named MachineSnapshots -- one per matching Machine, not a collision where the second Machine's create was rejected as a duplicate of the first's", createdNames, len(createdNames))
	}
	if createdNames[0] == createdNames[1] {
		t.Fatalf("both Machines got the same MachineSnapshot name %q", createdNames[0])
	}
}

// TestReconcileMachineSnapshotSchedulesSkipsWhenStartingDeadlineExceeded
// confirms the opt-in missed-deadline path: a schedule far enough overdue
// that its due window is now past spec.startingDeadlineSeconds creates NO
// MachineSnapshots this tick (unlike the always-fires-however-overdue
// default behavior), but still advances status.lastRunTime (so the next
// tick starts counting a fresh interval instead of re-detecting the same
// missed window forever) and records the skip in lastRunError.
func TestReconcileMachineSnapshotSchedulesSkipsWhenStartingDeadlineExceeded(t *testing.T) {
	var createCalled bool
	var patchedStatus model.MachineSnapshotScheduleStatus
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			sched := snapshotSchedule("hourly", time.Now().Add(-24*time.Hour), 60)
			sched.Spec.StartingDeadlineSeconds = 300
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{sched}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			createCalled = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			patched = true
			var body map[string]model.MachineSnapshotScheduleStatus
			_ = json.NewDecoder(r.Body).Decode(&body)
			patchedStatus = body["status"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if createCalled {
		t.Fatal("expected no MachineSnapshot to be created once startingDeadlineSeconds is exceeded")
	}
	if !patched {
		t.Fatal("expected status to still be patched (advancing lastRunTime) even on a skipped run")
	}
	if patchedStatus.LastRunSnapshotCount != 0 {
		t.Fatalf("LastRunSnapshotCount = %d, want 0 (nothing was snapshotted)", patchedStatus.LastRunSnapshotCount)
	}
	if patchedStatus.LastRunError == "" {
		t.Fatal("expected LastRunError to name the skipped run")
	}
	if patchedStatus.LastRunTime.IsZero() {
		t.Fatal("expected LastRunTime to advance to now, so the next tick doesn't re-detect the same missed window forever")
	}
	wantNextRun := patchedStatus.LastRunTime.Add(time.Minute)
	if !patchedStatus.NextRunTime.Equal(wantNextRun) {
		t.Fatalf("NextRunTime = %v, want %v (LastRunTime + the 60s interval)", patchedStatus.NextRunTime, wantNextRun)
	}
}

// TestReconcileMachineSnapshotSchedulesFiresWithinStartingDeadline confirms
// startingDeadlineSeconds being *set* doesn't change behavior for a run
// that's due but not yet past its own deadline -- only a run that's
// actually missed its window skips.
func TestReconcileMachineSnapshotSchedulesFiresWithinStartingDeadline(t *testing.T) {
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			// Due 30s ago (interval 60s, lastRun 90s ago), deadline 300s -- well within it.
			sched := snapshotSchedule("hourly", time.Now().Add(-90*time.Second), 60)
			sched.Spec.StartingDeadlineSeconds = 300
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{sched}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			created = append(created, s.Spec.MachineName)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v, want 1 (a due run within its own startingDeadlineSeconds must still fire normally)", created)
	}
}

func TestReconcileMachineSnapshotSchedulesZeroMatchesStillPatchesLastRunTime(t *testing.T) {
	var patched bool
	var status model.MachineSnapshotScheduleStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{
				snapshotSchedule("hourly", time.Time{}, 3600),
			}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			patched = true
			var body map[string]model.MachineSnapshotScheduleStatus
			_ = json.NewDecoder(r.Body).Decode(&body)
			status = body["status"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// No machine has the tier=web label the schedule selects.
	machines := []model.Machine{{Metadata: model.ObjectMeta{Name: "db-1", Namespace: "prod"}}}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if !patched {
		t.Fatal("expected LastRunTime to be patched even when the selector matches zero Machines, so this schedule doesn't re-fire every tick forever")
	}
	if status.LastRunSnapshotCount != 0 {
		t.Fatalf("LastRunSnapshotCount = %d, want 0", status.LastRunSnapshotCount)
	}
	if !status.NextRunTime.Equal(status.LastRunTime.Add(time.Hour)) {
		t.Fatalf("NextRunTime = %v, want LastRunTime (%v) + the 3600s interval -- a zero-match run still projects a real next run", status.NextRunTime, status.LastRunTime)
	}
}

func TestReconcileMachineSnapshotSchedulesTolerantWhenCRDNotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), nil); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules should tolerate a missing CRD, got: %v", err)
	}
}

func TestReconcileMachineSnapshotSchedulesCreateFailureIsLoggedNotFatal(t *testing.T) {
	var patchedStatus model.MachineSnapshotScheduleStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{
				snapshotSchedule("hourly", time.Time{}, 3600),
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			http.Error(w, "boom", http.StatusInternalServerError)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			var body map[string]model.MachineSnapshotScheduleStatus
			_ = json.NewDecoder(r.Body).Decode(&body)
			patchedStatus = body["status"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("a per-Machine create failure should not fail the whole reconcile, got: %v", err)
	}
	if patchedStatus.LastRunSnapshotCount != 0 {
		t.Fatalf("LastRunSnapshotCount = %d, want 0 (the only match failed)", patchedStatus.LastRunSnapshotCount)
	}
	if patchedStatus.LastRunError == "" {
		t.Fatal("expected LastRunError to be set")
	}
}

// TestReconcileMachineSnapshotSchedulesPruningDeletesOnlyOldestReadyOwnSnapshots
// exercises KeepLast end to end: an existing schedule-owned, ready-to-use
// snapshot beyond the limit is deleted; a not-ready-to-use one of the same
// schedule/Machine is left alone (never counted, never a deletion
// candidate); and a snapshot carrying a *different* schedule's label (or no
// label at all -- a manual one) is never touched regardless of age.
func TestReconcileMachineSnapshotSchedulesPruningDeletesOnlyOldestReadyOwnSnapshots(t *testing.T) {
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	existing := []model.MachineSnapshot{
		{ // oldest, this schedule's own, ready -- should be pruned
			Metadata: model.ObjectMeta{Name: "hourly-1", Namespace: "prod", CreationTimestamp: older, Labels: map[string]string{model.SnapshotScheduleLabel: "hourly"}},
			Spec:     model.MachineSnapshotSpec{MachineName: "web-1"},
			Status:   model.MachineSnapshotStatus{ReadyToUse: true},
		},
		{ // newer, this schedule's own, ready -- should be kept (within KeepLast=1 once the brand-new one also counts... see below, this one is older than the tick's new snapshot but still the most recent PRE-EXISTING one)
			Metadata: model.ObjectMeta{Name: "hourly-2", Namespace: "prod", CreationTimestamp: newer, Labels: map[string]string{model.SnapshotScheduleLabel: "hourly"}},
			Spec:     model.MachineSnapshotSpec{MachineName: "web-1"},
			Status:   model.MachineSnapshotStatus{ReadyToUse: true},
		},
		{ // this schedule's own, but NOT ready yet -- must never be deleted, never counted
			Metadata: model.ObjectMeta{Name: "hourly-inprogress", Namespace: "prod", CreationTimestamp: time.Now(), Labels: map[string]string{model.SnapshotScheduleLabel: "hourly"}},
			Spec:     model.MachineSnapshotSpec{MachineName: "web-1"},
			Status:   model.MachineSnapshotStatus{ReadyToUse: false},
		},
		{ // a different schedule's own snapshot, ready, very old -- must never be touched by "hourly"'s pruning
			Metadata: model.ObjectMeta{Name: "daily-1", Namespace: "prod", CreationTimestamp: older.Add(-24 * time.Hour), Labels: map[string]string{model.SnapshotScheduleLabel: "daily"}},
			Spec:     model.MachineSnapshotSpec{MachineName: "web-1"},
			Status:   model.MachineSnapshotStatus{ReadyToUse: true},
		},
		{ // manually created (no schedule label at all), ready, very old -- must never be touched
			Metadata: model.ObjectMeta{Name: "manual-1", Namespace: "prod", CreationTimestamp: older.Add(-48 * time.Hour)},
			Spec:     model.MachineSnapshotSpec{MachineName: "web-1"},
			Status:   model.MachineSnapshotStatus{ReadyToUse: true},
		},
	}
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			sched := snapshotSchedule("hourly", time.Time{}, 3600) // never run -- immediately due
			sched.Spec.KeepLast = 1
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{sched}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: existing})
		case r.Method == http.MethodDelete && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/hourly-1":
			deleted = append(deleted, "hourly-1")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "hourly-1" {
		t.Fatalf("deleted = %v, want exactly [hourly-1] (the oldest ready snapshot this schedule owns beyond KeepLast=1) -- hourly-2 (newer, ready), hourly-inprogress (not ready), daily-1/manual-1 (not this schedule's own) must all survive", deleted)
	}
}

// TestReconcileMachineSnapshotSchedulesManualTriggerFiresRegardlessOfSuspend
// confirms the core value proposition of a manual trigger: a schedule an
// operator has paused can still be asked for one snapshot right now,
// without permanently unpausing it -- the annotation bypasses spec.suspend
// entirely, and status.lastHandledTriggerTime is set to the exact
// annotation value in the same patch as the run it satisfies.
func TestReconcileMachineSnapshotSchedulesManualTriggerFiresRegardlessOfSuspend(t *testing.T) {
	var created []string
	var patchedStatus model.MachineSnapshotScheduleStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			sched := snapshotSchedule("hourly", time.Now(), 3600) // just ran -- would NOT be due on its own
			sched.Spec.Suspend = true                             // AND suspended -- would never be due at all
			sched.Metadata.Annotations = map[string]string{model.AnnotationSnapshotScheduleTriggerNow: "2026-01-01T12:00:00Z"}
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{sched}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			created = append(created, s.Spec.MachineName)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			var body map[string]model.MachineSnapshotScheduleStatus
			_ = json.NewDecoder(r.Body).Decode(&body)
			patchedStatus = body["status"]
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v, want 1 (a manual trigger must fire even though the schedule is suspended and not otherwise due)", created)
	}
	if patchedStatus.LastHandledTriggerTime != "2026-01-01T12:00:00Z" {
		t.Fatalf("LastHandledTriggerTime = %q, want the handled annotation value", patchedStatus.LastHandledTriggerTime)
	}
	if patchedStatus.LastRunSnapshotCount != 1 {
		t.Fatalf("LastRunSnapshotCount = %d, want 1", patchedStatus.LastRunSnapshotCount)
	}
}

// TestReconcileMachineSnapshotSchedulesManualTriggerAlreadyHandledDoesNothing
// confirms a trigger annotation whose value already matches
// status.lastHandledTriggerTime is NOT treated as a new request -- without
// this, a schedule would re-fire every single reconcile tick forever after
// just one manual trigger, since the annotation is never cleared by
// kaironctl/kubectl on its own.
func TestReconcileMachineSnapshotSchedulesManualTriggerAlreadyHandledDoesNothing(t *testing.T) {
	var createCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			sched := snapshotSchedule("hourly", time.Now(), 3600) // not otherwise due
			sched.Metadata.Annotations = map[string]string{model.AnnotationSnapshotScheduleTriggerNow: "2026-01-01T12:00:00Z"}
			sched.Status.LastHandledTriggerTime = "2026-01-01T12:00:00Z" // this exact request was already handled
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{sched}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			createCalled = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if createCalled {
		t.Fatal("expected no MachineSnapshot to be created for a trigger request that was already handled")
	}
}

// TestReconcileMachineSnapshotSchedulesManualTriggerBypassesStartingDeadline
// confirms a manual trigger never takes the startingDeadlineSeconds skip
// branch, even when the schedule is independently very overdue -- "run
// right now" has no "too late" to miss.
func TestReconcileMachineSnapshotSchedulesManualTriggerBypassesStartingDeadline(t *testing.T) {
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinesnapshotschedules":
			sched := snapshotSchedule("hourly", time.Now().Add(-24*time.Hour), 60) // very overdue
			sched.Spec.StartingDeadlineSeconds = 300
			sched.Metadata.Annotations = map[string]string{model.AnnotationSnapshotScheduleTriggerNow: "2026-01-01T12:00:00Z"}
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotScheduleList{Items: []model.MachineSnapshotSchedule{sched}})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			var s model.MachineSnapshot
			_ = json.NewDecoder(r.Body).Decode(&s)
			created = append(created, s.Spec.MachineName)
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotschedules/hourly/status":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running")}
	if err := ctl.reconcileMachineSnapshotSchedules(context.Background(), machines); err != nil {
		t.Fatalf("reconcileMachineSnapshotSchedules: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v, want 1 (a manual trigger must fire even though the schedule is also past its own startingDeadlineSeconds)", created)
	}
}

func TestPruneScheduledSnapshotsNoOpBelowKeepLast(t *testing.T) {
	var deleteCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotList{Items: []model.MachineSnapshot{
				{
					Metadata: model.ObjectMeta{Name: "hourly-1", Namespace: "prod", Labels: map[string]string{model.SnapshotScheduleLabel: "hourly"}},
					Spec:     model.MachineSnapshotSpec{MachineName: "web-1"},
					Status:   model.MachineSnapshotStatus{ReadyToUse: true},
				},
			}})
		case r.Method == http.MethodDelete:
			deleteCalled = true
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctl.pruneScheduledSnapshots(context.Background(), "prod", "hourly", "web-1", 1, 0, time.Now())
	if deleteCalled {
		t.Fatal("expected no delete when the count of owned, ready snapshots is already at or below KeepLast")
	}
}
