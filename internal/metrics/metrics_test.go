// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zyvorai/kairon/internal/model"
)

func migration(ns, name, phase string) model.MachineMigration {
	return model.MachineMigration{
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Status:   model.MachineMigrationStatus{Phase: phase},
	}
}

func TestObserveMigrationsCountsByPhase(t *testing.T) {
	r := NewRecorder()
	r.ObserveMigrations([]model.MachineMigration{
		migration("prod", "a", "Running"),
		migration("prod", "b", "Running"),
		migration("prod", "c", "NeedsRecovery"),
	})
	if got := testutil.ToFloat64(r.phaseCount.WithLabelValues("Running")); got != 2 {
		t.Errorf("Running count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.phaseCount.WithLabelValues("NeedsRecovery")); got != 1 {
		t.Errorf("NeedsRecovery count = %v, want 1", got)
	}
}

func TestObserveMigrationsTracksPhaseAgeAcrossTicks(t *testing.T) {
	r := NewRecorder()
	tick := 0
	r.now = func() time.Time {
		tick++
		return time.Unix(int64(tick*10), 0)
	}
	m := migration("prod", "a", "Running")
	r.ObserveMigrations([]model.MachineMigration{m}) // tick 1: since=10
	r.ObserveMigrations([]model.MachineMigration{m}) // tick 2: same phase, age = 20-10=10
	if got := testutil.ToFloat64(r.phaseAgeSeconds.WithLabelValues("prod", "a", "Running")); got != 10 {
		t.Errorf("phase age = %v, want 10", got)
	}

	// Phase changes -- age resets to 0 at the tick it changes.
	m.Status.Phase = "Cutover"
	r.ObserveMigrations([]model.MachineMigration{m}) // tick 3: phase changed, since=30
	if got := testutil.ToFloat64(r.phaseAgeSeconds.WithLabelValues("prod", "a", "Cutover")); got != 0 {
		t.Errorf("phase age after transition = %v, want 0", got)
	}
}

func TestObserveMigrationsCountsCompletedOnceOnTerminalTransition(t *testing.T) {
	r := NewRecorder()
	m := migration("prod", "a", "Running")
	r.ObserveMigrations([]model.MachineMigration{m})
	m.Status.Phase = "Succeeded"
	m.Status.TotalTimeMs = 5000
	m.Status.DowntimeMs = 200
	r.ObserveMigrations([]model.MachineMigration{m})
	r.ObserveMigrations([]model.MachineMigration{m}) // still Succeeded -- must not double-count
	if got := testutil.ToFloat64(r.completedTotal.WithLabelValues("succeeded")); got != 1 {
		t.Errorf("completed count = %v, want 1 (no double-counting)", got)
	}
}

func TestObserveMigrationsCountsCancelledOnceAsTerminal(t *testing.T) {
	r := NewRecorder()
	m := migration("prod", "a", "Running")
	r.ObserveMigrations([]model.MachineMigration{m})
	m.Status.Phase = "Cancelled"
	r.ObserveMigrations([]model.MachineMigration{m})
	r.ObserveMigrations([]model.MachineMigration{m}) // still Cancelled -- must not double-count
	if got := testutil.ToFloat64(r.completedTotal.WithLabelValues("cancelled")); got != 1 {
		t.Errorf("completed count = %v, want 1 (Cancelled is a real terminal phase, no double-counting)", got)
	}
}

func TestObserveMigrationsPrunesRemovedMigrations(t *testing.T) {
	r := NewRecorder()
	r.ObserveMigrations([]model.MachineMigration{migration("prod", "a", "Running")})
	if _, ok := r.phaseSince[phaseKey{"prod", "a"}]; !ok {
		t.Fatal("expected phaseSince to track the migration")
	}
	r.ObserveMigrations([]model.MachineMigration{}) // migration gone (e.g. list no longer returns it -- shouldn't normally happen, but prune defensively)
	if _, ok := r.phaseSince[phaseKey{"prod", "a"}]; ok {
		t.Error("expected phaseSince entry to be pruned once the migration disappears")
	}
}

func TestObserveMigrationsSetsDataPlaneEncryptedForActivePhasesOnly(t *testing.T) {
	r := NewRecorder()
	m := migration("prod", "a", "Starting")
	m.Status.DataPlaneEncrypted = true
	r.ObserveMigrations([]model.MachineMigration{m})
	if got := testutil.ToFloat64(r.dataPlaneEncrypted.WithLabelValues("prod", "a")); got != 1 {
		t.Errorf("dataPlaneEncrypted = %v, want 1", got)
	}
}

func quotaWithMax(ns, name string, maxMachines *int, maxCPU, maxMem string, usedMachines int, usedCPU uint32, usedMem uint64) model.MachineQuota {
	return model.MachineQuota{
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Spec:     model.MachineQuotaSpec{MaxMachines: maxMachines, MaxTotalCPU: maxCPU, MaxTotalMemory: maxMem},
		Status: model.MachineQuotaStatus{
			UsedMachines:       usedMachines,
			UsedTotalCPUCores:  usedCPU,
			UsedTotalMemoryMiB: usedMem,
		},
	}
}

func TestObserveQuotasReportsUsedAndHardLimits(t *testing.T) {
	r := NewRecorder()
	max := 10
	r.ObserveQuotas([]model.MachineQuota{quotaWithMax("prod", "team-a", &max, "16", "32Gi", 4, 8, 16384)})

	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "machines", "used")); got != 4 {
		t.Errorf("machines used = %v, want 4", got)
	}
	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "machines", "hard")); got != 10 {
		t.Errorf("machines hard = %v, want 10", got)
	}
	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "cpu_cores", "used")); got != 8 {
		t.Errorf("cpu_cores used = %v, want 8", got)
	}
	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "cpu_cores", "hard")); got != 16 {
		t.Errorf("cpu_cores hard = %v, want 16", got)
	}
	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "memory_mib", "used")); got != 16384 {
		t.Errorf("memory_mib used = %v, want 16384", got)
	}
	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "memory_mib", "hard")); got != 32*1024 {
		t.Errorf("memory_mib hard = %v, want %v", got, 32*1024)
	}
}

// TestObserveQuotasOmitsHardSeriesForUnsetDimension confirms a MachineQuota
// that doesn't cap a given dimension (e.g. no maxTotalCpu) never gets a
// type="hard" series for it -- only type="used", never a misleading
// type="hard" 0 that would read as "capped at zero."
func TestObserveQuotasOmitsHardSeriesForUnsetDimension(t *testing.T) {
	r := NewRecorder()
	r.ObserveQuotas([]model.MachineQuota{quotaWithMax("prod", "uncapped", nil, "", "", 3, 6, 8192)})

	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "uncapped", "machines", "used")); got != 3 {
		t.Errorf("machines used = %v, want 3", got)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, `resource="machines",type="hard"`) {
		t.Errorf("expected no hard series for unset maxMachines in:\n%s", body)
	}
	if strings.Contains(body, `resource="cpu_cores",type="hard"`) {
		t.Errorf("expected no hard series for unset maxTotalCpu in:\n%s", body)
	}
	if strings.Contains(body, `resource="memory_mib",type="hard"`) {
		t.Errorf("expected no hard series for unset maxTotalMemory in:\n%s", body)
	}
}

// TestObserveQuotasPrunesRemovedQuotas mirrors
// TestObserveMigrationsPrunesRemovedMigrations: a MachineQuota that no
// longer appears in the list (deleted, or the CRD listing failed and the
// caller passed an empty slice) must not leave a stale series behind.
func TestObserveQuotasPrunesRemovedQuotas(t *testing.T) {
	r := NewRecorder()
	max := 5
	r.ObserveQuotas([]model.MachineQuota{quotaWithMax("prod", "team-a", &max, "", "", 1, 0, 0)})
	if got := testutil.ToFloat64(r.quotaResource.WithLabelValues("prod", "team-a", "machines", "used")); got != 1 {
		t.Fatalf("machines used = %v, want 1", got)
	}
	r.ObserveQuotas(nil)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `quota="team-a"`) {
		t.Error("expected team-a's series to be pruned once it's no longer observed")
	}
}

func budget(ns, name string, expected, healthy, desired, allowed int) model.MachineDisruptionBudget {
	return model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Status: model.MachineDisruptionBudgetStatus{
			ExpectedMachines:   expected,
			CurrentHealthy:     healthy,
			DesiredHealthy:     desired,
			DisruptionsAllowed: allowed,
		},
	}
}

func TestObserveDisruptionBudgetsReportsAllFourFields(t *testing.T) {
	r := NewRecorder()
	r.ObserveDisruptionBudgets([]model.MachineDisruptionBudget{budget("prod", "web-tier", 3, 3, 2, 1)})

	if got := testutil.ToFloat64(r.disruptionBudgetStatus.WithLabelValues("prod", "web-tier", "expected_machines")); got != 3 {
		t.Errorf("expected_machines = %v, want 3", got)
	}
	if got := testutil.ToFloat64(r.disruptionBudgetStatus.WithLabelValues("prod", "web-tier", "current_healthy")); got != 3 {
		t.Errorf("current_healthy = %v, want 3", got)
	}
	if got := testutil.ToFloat64(r.disruptionBudgetStatus.WithLabelValues("prod", "web-tier", "desired_healthy")); got != 2 {
		t.Errorf("desired_healthy = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.disruptionBudgetStatus.WithLabelValues("prod", "web-tier", "disruptions_allowed")); got != 1 {
		t.Errorf("disruptions_allowed = %v, want 1", got)
	}
}

// TestObserveDisruptionBudgetsPrunesRemovedBudgets mirrors
// TestObserveQuotasPrunesRemovedQuotas: a MachineDisruptionBudget that no
// longer appears in the list (deleted, or the CRD listing failed and the
// caller passed an empty slice) must not leave a stale series behind.
func TestObserveDisruptionBudgetsPrunesRemovedBudgets(t *testing.T) {
	r := NewRecorder()
	r.ObserveDisruptionBudgets([]model.MachineDisruptionBudget{budget("prod", "web-tier", 3, 3, 2, 1)})
	if got := testutil.ToFloat64(r.disruptionBudgetStatus.WithLabelValues("prod", "web-tier", "disruptions_allowed")); got != 1 {
		t.Fatalf("disruptions_allowed = %v, want 1", got)
	}
	r.ObserveDisruptionBudgets(nil)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `budget="web-tier"`) {
		t.Error("expected web-tier's series to be pruned once it's no longer observed")
	}
}

func machineSetWithStatus(ns, name string, replicas, ready, updated int) model.MachineSet {
	return model.MachineSet{
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Status: model.MachineSetStatus{
			Replicas:        replicas,
			ReadyReplicas:   ready,
			UpdatedReplicas: updated,
		},
	}
}

func TestObserveMachineSetsReportsAllThreeFields(t *testing.T) {
	r := NewRecorder()
	r.ObserveMachineSets([]model.MachineSet{machineSetWithStatus("prod", "web", 3, 2, 3)})

	if got := testutil.ToFloat64(r.machineSetStatus.WithLabelValues("prod", "web", "replicas")); got != 3 {
		t.Errorf("replicas = %v, want 3", got)
	}
	if got := testutil.ToFloat64(r.machineSetStatus.WithLabelValues("prod", "web", "ready_replicas")); got != 2 {
		t.Errorf("ready_replicas = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.machineSetStatus.WithLabelValues("prod", "web", "updated_replicas")); got != 3 {
		t.Errorf("updated_replicas = %v, want 3", got)
	}
}

// TestObserveMachineSetsPrunesRemovedMachineSets mirrors
// TestObserveDisruptionBudgetsPrunesRemovedBudgets: a MachineSet that no
// longer appears in the list (deleted, or the CRD listing failed and the
// caller passed an empty slice) must not leave a stale series behind.
func TestObserveMachineSetsPrunesRemovedMachineSets(t *testing.T) {
	r := NewRecorder()
	r.ObserveMachineSets([]model.MachineSet{machineSetWithStatus("prod", "web", 3, 3, 3)})
	if got := testutil.ToFloat64(r.machineSetStatus.WithLabelValues("prod", "web", "replicas")); got != 3 {
		t.Fatalf("replicas = %v, want 3", got)
	}
	r.ObserveMachineSets(nil)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `machineset="web"`) {
		t.Error("expected web's series to be pruned once it's no longer observed")
	}
}

// TestObserveMachineSetsNoopsOnNodeRecorder confirms ObserveMachineSets is
// a safe no-op on a Recorder that never registered machineSetStatus
// (NewNodeRecorder/NewUIRecorder) -- neither kairon-node nor kairon-ui has
// a cluster-wide MachineSet listing to report on.
func TestObserveMachineSetsNoopsOnNodeRecorder(t *testing.T) {
	r := NewNodeRecorder()
	r.ObserveMachineSets([]model.MachineSet{machineSetWithStatus("prod", "web", 3, 3, 3)})
}

func TestHandlerServesPrometheusExposition(t *testing.T) {
	r := NewRecorder()
	r.ObserveMigrations([]model.MachineMigration{migration("prod", "a", "Running")})
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "kairon_migration_phase_count") {
		t.Error("expected kairon_migration_phase_count in exposition output")
	}
}

func TestObserveReconcileRecordsDurationAndErrors(t *testing.T) {
	r := NewRecorder()
	r.ObserveReconcile(50*time.Millisecond, nil)
	r.ObserveReconcile(10*time.Millisecond, errors.New("boom"))
	if got := testutil.ToFloat64(r.reconcileErrors); got != 1 {
		t.Errorf("reconcileErrors = %v, want 1", got)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "kairon_reconcile_duration_seconds_count 2") {
		t.Errorf("expected 2 reconcile duration samples in:\n%s", rec.Body.String())
	}
}

func TestObserveReconcileItemErrorCountsByKind(t *testing.T) {
	r := NewRecorder()
	r.ObserveReconcileItemError("machine")
	r.ObserveReconcileItemError("machine")
	r.ObserveReconcileItemError("migration")
	if got := testutil.ToFloat64(r.reconcileItemErrors.WithLabelValues("machine")); got != 2 {
		t.Errorf("machine count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.reconcileItemErrors.WithLabelValues("migration")); got != 1 {
		t.Errorf("migration count = %v, want 1", got)
	}
	// A whole-tick failure (ObserveReconcile with a non-nil err) and a
	// per-item failure are deliberately independent counters -- a tick
	// with two failed Machines but no outright List error should show 0
	// here, not 1.
	if got := testutil.ToFloat64(r.reconcileErrors); got != 0 {
		t.Errorf("reconcileErrors = %v, want 0 (no whole-tick failure recorded)", got)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `kairon_reconcile_item_errors_total{kind="machine"} 2`) {
		t.Errorf("missing machine sample in:\n%s", body)
	}
	if !strings.Contains(body, `kairon_reconcile_item_errors_total{kind="migration"} 1`) {
		t.Errorf("missing migration sample in:\n%s", body)
	}
}

func TestObserveWebhookDecisionCountsByResourceOperationAndDecision(t *testing.T) {
	r := NewRecorder()
	r.ObserveWebhookDecision("machines", "CREATE", false)
	r.ObserveWebhookDecision("machines", "CREATE", false)
	r.ObserveWebhookDecision("machinemigrations", "CREATE", true)
	if got := testutil.ToFloat64(r.webhookDecisions.WithLabelValues("machines", "CREATE", "deny")); got != 2 {
		t.Errorf("deny count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.webhookDecisions.WithLabelValues("machinemigrations", "CREATE", "allow")); got != 1 {
		t.Errorf("allow count = %v, want 1", got)
	}
}

func TestObserveAPIRequestCountsOkAndError(t *testing.T) {
	r := NewRecorder()
	r.ObserveAPIRequest("GET", 5*time.Millisecond, nil)
	r.ObserveAPIRequest("GET", 5*time.Millisecond, errors.New("boom"))
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `kairon_apiserver_request_duration_seconds_count{method="GET",outcome="ok"} 1`) {
		t.Errorf("missing ok sample in:\n%s", body)
	}
	if !strings.Contains(body, `kairon_apiserver_request_duration_seconds_count{method="GET",outcome="error"} 1`) {
		t.Errorf("missing error sample in:\n%s", body)
	}
}

func TestObserveReconcileItemErrorNoOpsOnARecorderWithoutReconcileMetrics(t *testing.T) {
	// NewUIRecorder registers no reconcile metrics at all (kairon-ui has
	// no reconcile loop) -- must not panic on a nil reconcileItemErrors,
	// matching every other Observe* method's nil-checked no-op contract.
	r := NewUIRecorder()
	r.ObserveReconcileItemError("machine")
}

func TestObserveHTTPRequestBucketsByStatusClass(t *testing.T) {
	r := NewUIRecorder()
	r.ObserveHTTPRequest("GET", "/api/v1/machines/{namespace}/{name}", 200, time.Millisecond)
	r.ObserveHTTPRequest("POST", "/api/v1/machines", 500, time.Millisecond)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `kairon_ui_request_duration_seconds_count{method="GET",route="/api/v1/machines/{namespace}/{name}",status_class="2xx"} 1`) {
		t.Errorf("missing 2xx sample in:\n%s", body)
	}
	if !strings.Contains(body, `kairon_ui_request_duration_seconds_count{method="POST",route="/api/v1/machines",status_class="5xx"} 1`) {
		t.Errorf("missing 5xx sample in:\n%s", body)
	}
}

// TestNodeRecorderOmitsControllerOnlyMetrics confirms NewNodeRecorder
// doesn't expose migration-lifecycle or webhook metrics it has no way to
// keep meaningful -- and that its Observe* methods are still safe to call
// (no-op) for the metrics it didn't register.
func TestNodeRecorderOmitsControllerOnlyMetrics(t *testing.T) {
	r := NewNodeRecorder()
	r.ObserveMigrations([]model.MachineMigration{migration("prod", "a", "Running")}) // no-op: phaseCount is nil
	r.ObserveWebhookDecision("machines", "CREATE", false)                            // no-op: webhookDecisions is nil
	max := 1
	r.ObserveQuotas([]model.MachineQuota{quotaWithMax("prod", "a", &max, "", "", 1, 0, 0)})      // no-op: quotaResource is nil
	r.ObserveDisruptionBudgets([]model.MachineDisruptionBudget{budget("prod", "a", 1, 1, 1, 0)}) // no-op: disruptionBudgetStatus is nil
	r.ObserveReconcile(time.Millisecond, nil)
	r.ObserveAPIRequest("GET", time.Millisecond, nil)

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "kairon_migration_phase_count") {
		t.Error("NewNodeRecorder should not expose migration-lifecycle metrics")
	}
	if strings.Contains(body, "kairon_webhook_decisions_total") {
		t.Error("NewNodeRecorder should not expose webhook decision metrics")
	}
	if strings.Contains(body, "kairon_quota_resource") {
		t.Error("NewNodeRecorder should not expose MachineQuota utilization metrics")
	}
	if strings.Contains(body, "kairon_disruption_budget_status") {
		t.Error("NewNodeRecorder should not expose MachineDisruptionBudget status metrics")
	}
	if !strings.Contains(body, "kairon_reconcile_duration_seconds") {
		t.Error("NewNodeRecorder should expose reconcile metrics")
	}
	if !strings.Contains(body, "kairon_apiserver_request_duration_seconds") {
		t.Error("NewNodeRecorder should expose apiserver call metrics")
	}
}

// TestUIRecorderOmitsReconcileAndMigrationMetrics mirrors
// TestNodeRecorderOmitsControllerOnlyMetrics for kairon-ui's smaller set.
func TestUIRecorderOmitsReconcileAndMigrationMetrics(t *testing.T) {
	r := NewUIRecorder()
	r.ObserveReconcile(time.Millisecond, nil) // no-op: reconcileDuration is nil
	r.ObserveHTTPRequest("GET", "/api/v1/overview", 200, time.Millisecond)
	r.ObserveAPIRequest("GET", time.Millisecond, nil)
	max := 1
	r.ObserveQuotas([]model.MachineQuota{quotaWithMax("prod", "a", &max, "", "", 1, 0, 0)})      // no-op: quotaResource is nil
	r.ObserveDisruptionBudgets([]model.MachineDisruptionBudget{budget("prod", "a", 1, 1, 1, 0)}) // no-op: disruptionBudgetStatus is nil

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "kairon_reconcile_duration_seconds") {
		t.Error("NewUIRecorder should not expose reconcile metrics")
	}
	if strings.Contains(body, "kairon_migration_phase_count") {
		t.Error("NewUIRecorder should not expose migration-lifecycle metrics")
	}
	if strings.Contains(body, "kairon_quota_resource") {
		t.Error("NewUIRecorder should not expose MachineQuota utilization metrics")
	}
	if strings.Contains(body, "kairon_disruption_budget_status") {
		t.Error("NewUIRecorder should not expose MachineDisruptionBudget status metrics")
	}
	if !strings.Contains(body, "kairon_ui_request_duration_seconds") {
		t.Error("NewUIRecorder should expose its own HTTP request metrics")
	}
	if !strings.Contains(body, "kairon_apiserver_request_duration_seconds") {
		t.Error("NewUIRecorder should expose apiserver call metrics")
	}
}

func TestCachedUsageDoesNotRefreshLiveSampleTimestamp(t *testing.T) {
	r := NewNodeRecorder()
	timestamp := r.machineUsageTimestamp.WithLabelValues("default", "vm")
	r.ObserveMachineResourceUsage("default", "vm", &model.ResourceUsage{CPUPercent: 50})
	if got := testutil.ToFloat64(timestamp); got != 0 {
		t.Fatalf("cached usage marked fresh: %v", got)
	}
	r.ObserveMachineUsageFreshness("default", "vm")
	fresh := testutil.ToFloat64(timestamp)
	if fresh < float64(time.Now().Unix()-2) {
		t.Fatalf("fresh sample not recorded: %v", fresh)
	}
	r.ObserveMachineResourceUsage("default", "vm", &model.ResourceUsage{CPUPercent: 50})
	if got := testutil.ToFloat64(timestamp); got != fresh {
		t.Fatal("cached sample advanced freshness")
	}
}

func TestObserveMigrationsReportsProgressAndAdaptiveState(t *testing.T) {
	r := NewRecorder()
	m := migration("prod", "a", "Running")
	m.Status.RAMRemaining = 1 << 30
	m.Status.Progress = &model.MachineMigrationProgress{DirtyRateMbps: 700, ThroughputMbps: 900, CPUThrottlePct: 50, EstimatedCompletionSeconds: 42}
	t0 := time.Unix(100, 0)
	m.Status.Adaptive = &model.MachineMigrationAdaptiveStatus{
		Stage:   model.AdaptiveStageAutoConverge,
		Actions: []model.MachineMigrationAdaptiveAction{{At: t0, Action: "auto-converge"}},
	}
	r.ObserveMigrations([]model.MachineMigration{m})
	for field, want := range map[string]float64{"dirty_rate_mbps": 700, "throughput_mbps": 900, "cpu_throttle_pct": 50, "estimated_completion_seconds": 42, "ram_remaining_bytes": 1 << 30} {
		if got := testutil.ToFloat64(r.migrationProgress.WithLabelValues("prod", "a", field)); got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	if got := testutil.ToFloat64(r.adaptiveStage.WithLabelValues("prod", "a", model.AdaptiveStageAutoConverge)); got != 1 {
		t.Errorf("stage gauge = %v", got)
	}
	// Re-observing the same log never double-counts; only new entries count.
	r.ObserveMigrations([]model.MachineMigration{m})
	m.Status.Adaptive.Actions = append(m.Status.Adaptive.Actions, model.MachineMigrationAdaptiveAction{At: t0.Add(time.Minute), Action: "throttle"})
	r.ObserveMigrations([]model.MachineMigration{m})
	if got := testutil.ToFloat64(r.adaptiveActions.WithLabelValues("auto-converge")); got != 1 {
		t.Errorf("auto-converge count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.adaptiveActions.WithLabelValues("throttle")); got != 1 {
		t.Errorf("throttle count = %v, want 1", got)
	}
	m.Status.Phase = "Succeeded"
	r.ObserveMigrations([]model.MachineMigration{m})
	if n := testutil.CollectAndCount(r.migrationProgress); n != 0 {
		t.Errorf("finished migration still reports %d progress series", n)
	}
}
