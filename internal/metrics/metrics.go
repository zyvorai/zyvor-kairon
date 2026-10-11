// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes Prometheus metrics for Kairon's components.
// kairon-controller's Recorder (NewRecorder) additionally covers its
// unique cluster-wide view of MachineMigration, MachineQuota,
// MachineDisruptionBudget, and MachineSet state, since Controller.Reconcile
// already lists every MachineMigration, MachineQuota,
// MachineDisruptionBudget, and MachineSet every tick; kairon-node
// (NewNodeRecorder) and kairon-ui (NewUIRecorder) get a smaller,
// purpose-specific subset instead of the full controller set, so neither
// exposes migration-lifecycle, quota-utilization, disruption-budget, or
// machineset-rollout metrics it has no way to keep meaningful (a node's
// /metrics permanently reporting kairon_migration_phase_count=0, or
// kairon_quota_resource/kairon_disruption_budget_status/kairon_machineset_status
// for a namespace it has no cluster-wide visibility into, would be
// misleading, not just unused).
// Every Recorder shares the same struct and Observe* methods; a method
// whose backing metric wasn't registered by the constructor that built
// this Recorder is simply a no-op (nil-checked), so callers never need to
// know which concrete subset they're holding.
package metrics

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/zyvorai/kairon/internal/model"
)

var terminalPhases = map[string]bool{"Succeeded": true, "Failed": true, "Blocked": true, "Cancelled": true}

type phaseKey struct{ namespace, name string }

type phaseState struct {
	phase string
	since time.Time
}

// Recorder tracks Prometheus metrics for one Kairon component. Not a
// global var by design -- callers construct one and pass it explicitly
// (e.g. Controller.Metrics), matching this codebase's existing style
// (Agent.MigrationPeer etc.).
type Recorder struct {
	registry *prometheus.Registry

	// Migration-lifecycle metrics -- only registered by NewRecorder
	// (kairon-controller), see ObserveMigrations.
	phaseCount         *prometheus.GaugeVec
	phaseAgeSeconds    *prometheus.GaugeVec
	completedTotal     *prometheus.CounterVec
	transferDuration   *prometheus.HistogramVec
	cutoverDowntime    prometheus.Histogram
	dataPlaneEncrypted *prometheus.GaugeVec
	migrationProgress  *prometheus.GaugeVec
	adaptiveStage      *prometheus.GaugeVec
	adaptiveActions    *prometheus.CounterVec
	// adaptiveSeen is the newest status.adaptive.actions timestamp already
	// counted per migration, so re-observing the same status never
	// double-counts.
	adaptiveSeen map[phaseKey]time.Time

	// MachineQuota utilization -- only registered by NewRecorder
	// (kairon-controller), see ObserveQuotas. Like migration-lifecycle
	// metrics, neither kairon-node (no cluster-wide MachineQuota listing of
	// its own -- see internal/controller/quota.go's QuotaTrackersForNamespace,
	// which is namespace-scoped) nor kairon-ui (no reconcile loop at all)
	// has a meaningful value to report here.
	quotaResource *prometheus.GaugeVec

	// MachineDisruptionBudget status -- only registered by NewRecorder
	// (kairon-controller), see ObserveDisruptionBudgets. Same rationale as
	// quotaResource: neither kairon-node nor kairon-ui has a cluster-wide,
	// per-tick recomputation of every MachineDisruptionBudget's status to
	// report here.
	disruptionBudgetStatus *prometheus.GaugeVec

	// MachineSet rollout status -- only registered by NewRecorder
	// (kairon-controller), see ObserveMachineSets. Same rationale as
	// quotaResource/disruptionBudgetStatus: kairon-node never lists
	// MachineSets at all (it only reconciles individual Machines already
	// assigned to it), and kairon-ui has no reconcile loop of its own, so
	// neither has a cluster-wide, per-tick recomputation of every
	// MachineSet's rollout status to report here.
	machineSetStatus *prometheus.GaugeVec

	// VM-edge drops and conntrack restores -- only registered by
	// NewNodeRecorder, see Edge.
	edge *EdgeRecorder

	// Reconcile-loop metrics -- registered by NewRecorder and
	// NewNodeRecorder (kairon-controller/kairon-node both run one), see
	// ObserveReconcile/ObserveReconcileItemError.
	reconcileDuration   prometheus.Histogram
	reconcileErrors     prometheus.Counter
	reconcileItemErrors *prometheus.CounterVec

	// Admission-webhook decision metrics -- only registered by
	// NewRecorder (kairon-node/kairon-ui don't serve the webhook), see
	// ObserveWebhookDecision.
	webhookDecisions *prometheus.CounterVec

	// Kubernetes apiserver call metrics -- registered by all three
	// constructors, since every component talks to the apiserver via
	// internal/kube.Client. See ObserveAPIRequest.
	apiRequestDuration *prometheus.HistogramVec

	// kairon-ui's own HTTP request metrics -- only registered by
	// NewUIRecorder. See ObserveHTTPRequest.
	uiRequestDuration *prometheus.HistogramVec

	// Live Machine resource usage -- only registered by NewNodeRecorder.
	// See ObserveMachineResourceUsage.
	machineResourceUsage  *prometheus.GaugeVec
	machineUsageTimestamp *prometheus.GaugeVec

	mu         sync.Mutex
	phaseSince map[phaseKey]phaseState
	completed  map[phaseKey]bool
	now        func() time.Time
}

func newReconcileMetrics() (prometheus.Histogram, prometheus.Counter, *prometheus.CounterVec) {
	return prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kairon_reconcile_duration_seconds",
			Help:    "Duration of one reconcile loop iteration.",
			Buckets: prometheus.DefBuckets,
		}), prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kairon_reconcile_errors_total",
			Help: "Total reconcile loop iterations that returned an error outright (e.g. a List call itself failing). " +
				"Most per-item reconcile failures (one Machine/MachineMigration/etc. among many) are caught, logged, " +
				"and status-patched without the tick itself returning an error -- see kairon_reconcile_item_errors_total " +
				"for those.",
		}), prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_reconcile_item_errors_total",
			Help: "Total per-item reconcile failures inside a reconcile loop iteration, by resource kind " +
				"(machine/migration/snapshot/snapshotrestore) -- a Machine or MachineMigration etc. whose own " +
				"reconcile step failed, caught and logged individually rather than failing the whole tick. " +
				"Labeled by kind only, never by name/namespace, to keep cardinality bounded.",
		}, []string{"kind"})
}

func newAPIRequestDurationMetric() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kairon_apiserver_request_duration_seconds",
		Help:    "Kubernetes apiserver request duration, by HTTP method and outcome (ok/error).",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "outcome"})
}

// NewRecorder builds the full metric set kairon-controller uses: migration
// lifecycle, its own reconcile loop, admission-webhook decisions (if
// webhook.enabled), and apiserver call health.
func NewRecorder() *Recorder {
	reg := prometheus.NewRegistry()
	reconcileDuration, reconcileErrors, reconcileItemErrors := newReconcileMetrics()
	r := &Recorder{
		registry: reg,
		phaseCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_migration_phase_count",
			Help: "Number of MachineMigrations currently in each phase.",
		}, []string{"phase"}),
		phaseAgeSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_migration_phase_age_seconds",
			Help: "Seconds a MachineMigration has been in its current phase.",
		}, []string{"namespace", "name", "phase"}),
		completedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_migration_completed_total",
			Help: "Total MachineMigrations that reached a terminal phase, by result.",
		}, []string{"result"}),
		transferDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kairon_migration_transfer_duration_seconds",
			Help:    "Live migration transfer duration (status.totalTimeMs) for migrations that reached Succeeded or Failed.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		cutoverDowntime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kairon_migration_cutover_downtime_seconds",
			Help:    "Live migration cutover downtime (status.downtimeMs) for migrations that reached Succeeded or Failed.",
			Buckets: prometheus.DefBuckets,
		}),
		dataPlaneEncrypted: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_migration_dataplane_encrypted",
			Help: "1 if an active live migration's QEMU data-plane transport is TLS-encrypted, 0 otherwise.",
		}, []string{"namespace", "name"}),
		migrationProgress: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_migration_progress",
			Help: "Active live migration telemetry (status.progress), by namespace, name and field " +
				"(dirty_rate_mbps/ram_remaining_bytes/ram_transferred_bytes/throughput_mbps/expected_downtime_ms/" +
				"cpu_throttle_pct/estimated_completion_seconds/compression_ratio_percent/duplicate_pages).",
		}, []string{"namespace", "name", "field"}),
		adaptiveStage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_migration_adaptive_stage",
			Help: "1 for an active adaptive migration's current engine stage (status.adaptive.stage).",
		}, []string{"namespace", "name", "stage"}),
		adaptiveActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_migration_adaptive_actions_total",
			Help: "Adaptive engine actions recorded in status.adaptive.actions, by action.",
		}, []string{"action"}),
		quotaResource: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_quota_resource",
			Help: "MachineQuota usage and limit, by namespace, quota name, resource (machines/cpu_cores/memory_mib), " +
				"and type (used/hard) -- mirrors kube-state-metrics' own kube_resourcequota shape so the same " +
				"kairon_quota_resource{type=\"used\"} / kairon_quota_resource{type=\"hard\"} ratio pattern applies. " +
				"A resource dimension the MachineQuota doesn't cap at all (e.g. no maxTotalCpu) never gets a " +
				"type=\"hard\" series for that resource, only type=\"used\" -- there's no limit to report, not a " +
				"limit of zero.",
		}, []string{"namespace", "quota", "resource", "type"}),
		disruptionBudgetStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_disruption_budget_status",
			Help: "MachineDisruptionBudget status, by namespace, budget name, and field " +
				"(expected_machines/current_healthy/desired_healthy/disruptions_allowed) -- mirrors " +
				"kube-state-metrics' kube_poddisruptionbudget_status_* gauges (which report the same " +
				"four fields as one gauge per field rather than one gauge per label value), collapsed " +
				"into a single vector the same way kairon_quota_resource collapses used/hard into one " +
				"vector via its own type label.",
		}, []string{"namespace", "budget", "field"}),
		machineSetStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_machineset_status",
			Help: "MachineSet rollout status, by namespace, machineset name, and field " +
				"(replicas/ready_replicas/updated_replicas) -- mirrors kube-state-metrics' own " +
				"kube_replicaset_status_replicas/kube_replicaset_status_ready_replicas and " +
				"kube_deployment_status_replicas_updated gauges (three separate metric names there), " +
				"collapsed into a single vector via the field label, the same collapsing " +
				"kairon_quota_resource/kairon_disruption_budget_status already do with their own " +
				"type/field labels.",
		}, []string{"namespace", "machineset", "field"}),
		reconcileDuration:   reconcileDuration,
		reconcileErrors:     reconcileErrors,
		reconcileItemErrors: reconcileItemErrors,
		webhookDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_webhook_decisions_total",
			Help: "Admission webhook decisions, by resource, operation, and outcome (allow/deny).",
		}, []string{"resource", "operation", "decision"}),
		apiRequestDuration: newAPIRequestDurationMetric(),
		phaseSince:         map[phaseKey]phaseState{},
		completed:          map[phaseKey]bool{},
		adaptiveSeen:       map[phaseKey]time.Time{},
		now:                time.Now,
	}
	reg.MustRegister(r.phaseCount, r.phaseAgeSeconds, r.completedTotal, r.transferDuration, r.cutoverDowntime, r.dataPlaneEncrypted,
		r.migrationProgress, r.adaptiveStage, r.adaptiveActions,
		r.quotaResource, r.disruptionBudgetStatus, r.machineSetStatus, r.reconcileDuration, r.reconcileErrors, r.reconcileItemErrors, r.webhookDecisions, r.apiRequestDuration)
	return r
}

// NewNodeRecorder builds the smaller metric set kairon-node uses: its own
// reconcile loop plus apiserver call health -- no migration-lifecycle or
// webhook metrics, since kairon-node reconciles individual Machines on one
// node, not MachineMigrations cluster-wide, and never serves the webhook.
func NewNodeRecorder() *Recorder {
	reg := prometheus.NewRegistry()
	reconcileDuration, reconcileErrors, reconcileItemErrors := newReconcileMetrics()
	machineResourceUsage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kairon_machine_resource_usage",
		Help: "Live Machine resource usage reported by FluxVM stats. " +
			"resource is one of cpu_percent, memory_bytes, disk_read_bytes, disk_write_bytes. " +
			"Preferred over continuous MachineStatus.resourceUsage etcd writes.",
	}, []string{"namespace", "machine", "resource"})
	r := &Recorder{
		registry:             reg,
		reconcileDuration:    reconcileDuration,
		reconcileErrors:      reconcileErrors,
		reconcileItemErrors:  reconcileItemErrors,
		apiRequestDuration:   newAPIRequestDurationMetric(),
		machineResourceUsage: machineResourceUsage,
	}
	reg.MustRegister(r.reconcileDuration, r.reconcileErrors, r.reconcileItemErrors, r.apiRequestDuration, r.machineResourceUsage)
	r.machineUsageTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "kairon_machine_usage_sample_timestamp_seconds", Help: "Time of the last successful live FluxVM usage sample."}, []string{"namespace", "machine"})
	reg.MustRegister(r.machineUsageTimestamp)
	r.edge = NewEdgeRecorder(reg)
	return r
}

// Edge is the VM-edge metric set, registered by NewNodeRecorder only. Nil on
// other recorders and on a nil Recorder; EdgeRecorder methods accept nil.
func (r *Recorder) Edge() *EdgeRecorder {
	if r == nil {
		return nil
	}
	return r.edge
}

// NewUIRecorder builds the metric set kairon-ui uses: its own HTTP request
// health plus apiserver call health. kairon-ui has no reconcile loop or
// webhook of its own.
func NewUIRecorder() *Recorder {
	reg := prometheus.NewRegistry()
	r := &Recorder{
		registry:           reg,
		apiRequestDuration: newAPIRequestDurationMetric(),
		uiRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "kairon_ui_request_duration_seconds",
			Help: "kairon-ui HTTP request duration, by method, route, and response status class. " +
				"route is the registered mux pattern (e.g. \"/api/v1/machines/{namespace}/{name}\"), " +
				"never the raw request path -- that keeps cardinality bounded against dynamic " +
				"{namespace}/{name} segments the same way status_class already bounds it against " +
				"the full HTTP status range.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status_class"}),
	}
	reg.MustRegister(r.apiRequestDuration, r.uiRequestDuration)
	return r
}

// Handler serves the Prometheus exposition format for this Recorder's own
// registry (not the default global one, so tests stay hermetic).
func (r *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.registry, promhttp.HandlerOpts{})
}

// ObserveMigrations is a pure function of the current migration list plus
// wall-clock time -- call it once per reconcile tick (Controller.Reconcile,
// right after ListMachineMigrations) rather than scattering Record*() calls
// across every PatchMachineMigrationStatus call site in controller.go/agent.go.
func (r *Recorder) ObserveMigrations(migrations []model.MachineMigration) {
	if r.phaseCount == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	counts := map[string]int{}
	seen := map[phaseKey]bool{}
	r.dataPlaneEncrypted.Reset()
	r.migrationProgress.Reset()
	r.adaptiveStage.Reset()

	for _, m := range migrations {
		phase := m.Status.Phase
		if phase == "" {
			phase = "Pending"
		}
		counts[phase]++

		key := phaseKey{m.Namespace(), m.Metadata.Name}
		seen[key] = true

		state, ok := r.phaseSince[key]
		if !ok || state.phase != phase {
			state = phaseState{phase: phase, since: now}
			r.phaseSince[key] = state
		}
		r.phaseAgeSeconds.WithLabelValues(key.namespace, key.name, phase).Set(now.Sub(state.since).Seconds())

		if terminalPhases[phase] && !r.completed[key] {
			r.completed[key] = true
			result := strings.ToLower(phase)
			r.completedTotal.WithLabelValues(result).Inc()
			if phase == "Succeeded" || phase == "Failed" {
				r.transferDuration.WithLabelValues(result).Observe(float64(m.Status.TotalTimeMs) / 1000)
				if m.Status.DowntimeMs > 0 {
					r.cutoverDowntime.Observe(float64(m.Status.DowntimeMs) / 1000)
				}
			}
		}

		if phase == "Starting" || phase == "Running" {
			encrypted := 0.0
			if m.Status.DataPlaneEncrypted {
				encrypted = 1
			}
			r.dataPlaneEncrypted.WithLabelValues(key.namespace, key.name).Set(encrypted)
			r.observeProgress(key, m.Status)
		}
		r.countAdaptiveActions(key, m.Status.Adaptive)
	}

	for key := range r.phaseSince {
		if !seen[key] {
			delete(r.phaseSince, key)
			delete(r.completed, key)
			delete(r.adaptiveSeen, key)
		}
	}

	r.phaseCount.Reset()
	for phase, n := range counts {
		r.phaseCount.WithLabelValues(phase).Set(float64(n))
	}
}

func (r *Recorder) observeProgress(key phaseKey, st model.MachineMigrationStatus) {
	set := func(field string, v uint64) {
		r.migrationProgress.WithLabelValues(key.namespace, key.name, field).Set(float64(v))
	}
	set("ram_remaining_bytes", st.RAMRemaining)
	set("ram_transferred_bytes", st.RAMTransferred)
	if p := st.Progress; p != nil {
		set("dirty_rate_mbps", p.DirtyRateMbps)
		set("throughput_mbps", p.ThroughputMbps)
		set("expected_downtime_ms", p.ExpectedDowntimeMs)
		set("cpu_throttle_pct", p.CPUThrottlePct)
		set("estimated_completion_seconds", p.EstimatedCompletionSeconds)
		set("compression_ratio_percent", p.CompressionRatioPercent)
		set("duplicate_pages", p.DuplicatePages)
	}
	if a := st.Adaptive; a != nil && a.Stage != "" {
		r.adaptiveStage.WithLabelValues(key.namespace, key.name, a.Stage).Set(1)
	}
}

func (r *Recorder) countAdaptiveActions(key phaseKey, a *model.MachineMigrationAdaptiveStatus) {
	if a == nil {
		return
	}
	last := r.adaptiveSeen[key]
	newest := last
	for _, act := range a.Actions {
		if act.At.After(last) {
			r.adaptiveActions.WithLabelValues(act.Action).Inc()
			if act.At.After(newest) {
				newest = act.At
			}
		}
	}
	r.adaptiveSeen[key] = newest
}

// ObserveQuotas is a pure function of the current MachineQuota list, each
// carrying the exact status.used* values that reconcile tick is about to
// (or just did) patch onto the real object -- call it once per Reconcile
// tick, right alongside the quota status patches themselves
// (internal/controller/controller.go), the same "one Observe* call per
// tick, not scattered across every call site" shape ObserveMigrations
// already established. Passing the object's Spec/Status pair directly
// (rather than internal/controller's own QuotaTracker, whose fields are
// unexported) keeps this package with no import-time dependency on
// internal/controller.
//
// Like dataPlaneEncrypted, quotaResource is Reset() first: a deleted
// MachineQuota, or one that dropped a limit it used to set, must stop
// reporting a stale series rather than being left at its last-observed
// value forever.
func (r *Recorder) ObserveQuotas(quotas []model.MachineQuota) {
	if r.quotaResource == nil {
		return
	}
	r.quotaResource.Reset()
	for _, q := range quotas {
		ns, name := q.Namespace(), q.Metadata.Name
		r.quotaResource.WithLabelValues(ns, name, "machines", "used").Set(float64(q.Status.UsedMachines))
		if q.Spec.MaxMachines != nil {
			r.quotaResource.WithLabelValues(ns, name, "machines", "hard").Set(float64(*q.Spec.MaxMachines))
		}
		r.quotaResource.WithLabelValues(ns, name, "cpu_cores", "used").Set(float64(q.Status.UsedTotalCPUCores))
		if q.Spec.MaxTotalCPU != "" {
			// Already validated by BuildQuotaTrackers before this ever ran
			// this tick (an invalid maxTotalCpu fails the whole Reconcile
			// tick outright, well before any status/metric is ever
			// observed) -- a parse error here can only mean this exact
			// MachineQuota's limit somehow never got validated, so skip
			// reporting a hard limit for it rather than reporting a
			// meaningless zero.
			if max, err := model.ParseVCPUs(q.Spec.MaxTotalCPU); err == nil {
				r.quotaResource.WithLabelValues(ns, name, "cpu_cores", "hard").Set(float64(max))
			}
		}
		r.quotaResource.WithLabelValues(ns, name, "memory_mib", "used").Set(float64(q.Status.UsedTotalMemoryMiB))
		if q.Spec.MaxTotalMemory != "" {
			if max, err := model.ParseMemoryMiB(q.Spec.MaxTotalMemory); err == nil {
				r.quotaResource.WithLabelValues(ns, name, "memory_mib", "hard").Set(float64(max))
			}
		}
	}
}

// ObserveDisruptionBudgets is a pure function of the current
// MachineDisruptionBudget list, each carrying the exact status fields that
// reconcile tick is about to (or just did) patch onto the real object --
// call it once per Reconcile tick, right alongside the disruption-budget
// status patches themselves (internal/controller/disruption.go's
// reconcileDisruptionBudgetsStatus), the same "Status() already computed,
// just also hand it to metrics" shape ObserveQuotas established.
//
// Like quotaResource, disruptionBudgetStatus is Reset() first: a deleted
// MachineDisruptionBudget must stop reporting a stale series rather than
// being left at its last-observed value forever.
func (r *Recorder) ObserveDisruptionBudgets(budgets []model.MachineDisruptionBudget) {
	if r.disruptionBudgetStatus == nil {
		return
	}
	r.disruptionBudgetStatus.Reset()
	for _, b := range budgets {
		ns, name := b.Namespace(), b.Metadata.Name
		r.disruptionBudgetStatus.WithLabelValues(ns, name, "expected_machines").Set(float64(b.Status.ExpectedMachines))
		r.disruptionBudgetStatus.WithLabelValues(ns, name, "current_healthy").Set(float64(b.Status.CurrentHealthy))
		r.disruptionBudgetStatus.WithLabelValues(ns, name, "desired_healthy").Set(float64(b.Status.DesiredHealthy))
		r.disruptionBudgetStatus.WithLabelValues(ns, name, "disruptions_allowed").Set(float64(b.Status.DisruptionsAllowed))
	}
}

// ObserveMachineSets is a pure function of the current MachineSet list,
// each carrying the exact status.replicas/readyReplicas/updatedReplicas
// tallies that tick's reconcileMachineSets is about to (or just did) patch
// onto the real object -- call it once per Reconcile tick, right alongside
// the MachineSet status patches themselves
// (internal/controller/machineset.go's reconcileMachineSets), the same
// "Status already computed, just also hand it to metrics" shape
// ObserveQuotas/ObserveDisruptionBudgets established.
//
// Like quotaResource/disruptionBudgetStatus, machineSetStatus is Reset()
// first: a deleted MachineSet must stop reporting a stale series rather
// than being left at its last-observed value forever.
func (r *Recorder) ObserveMachineSets(machineSets []model.MachineSet) {
	if r.machineSetStatus == nil {
		return
	}
	r.machineSetStatus.Reset()
	for _, ms := range machineSets {
		ns, name := ms.Namespace(), ms.Metadata.Name
		r.machineSetStatus.WithLabelValues(ns, name, "replicas").Set(float64(ms.Status.Replicas))
		r.machineSetStatus.WithLabelValues(ns, name, "ready_replicas").Set(float64(ms.Status.ReadyReplicas))
		r.machineSetStatus.WithLabelValues(ns, name, "updated_replicas").Set(float64(ms.Status.UpdatedReplicas))
	}
}

// ObserveReconcile records one reconcile loop iteration's duration and,
// if err is non-nil, counts it as a failed iteration. Call once per tick
// from Controller.Run/Agent.Run, regardless of outcome -- a nil Recorder
// method receiver is never valid here (callers nil-check the *Recorder
// itself, e.g. `if c.Metrics != nil`), but a Recorder built by a
// constructor that didn't register these two metrics (there is none
// today -- every constructor does) would silently no-op via the nil
// field check below.
func (r *Recorder) ObserveReconcile(d time.Duration, err error) {
	if r.reconcileDuration == nil {
		return
	}
	r.reconcileDuration.Observe(d.Seconds())
	if err != nil {
		r.reconcileErrors.Inc()
	}
}

// ObserveReconcileItemError counts one item's own reconcile step failing
// inside a loop iteration (a single Machine's/MachineMigration's/etc. own
// reconcileX call returning an error, caught, logged, and status-patched
// without failing the whole tick) -- see kairon_reconcile_item_errors_total's
// own doc string for why this exists alongside ObserveReconcile's
// whole-tick counter. kind should be one of a small, fixed, already-known
// set ("machine", "migration", "snapshot", "snapshotrestore") -- never a
// resource name, which would blow up cardinality.
func (r *Recorder) ObserveReconcileItemError(kind string) {
	if r.reconcileItemErrors == nil {
		return
	}
	r.reconcileItemErrors.WithLabelValues(kind).Inc()
}

// ObserveWebhookDecision records one admission webhook decision. No-op on
// a Recorder that didn't register webhookDecisions (NewNodeRecorder/
// NewUIRecorder) -- neither kairon-node nor kairon-ui serves the webhook.
func (r *Recorder) ObserveWebhookDecision(resource, operation string, allowed bool) {
	if r.webhookDecisions == nil {
		return
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	r.webhookDecisions.WithLabelValues(resource, operation, decision).Inc()
}

// ObserveAPIRequest records one Kubernetes apiserver call. Wire it to
// internal/kube.Client.Observe (see cmd/*/main.go) -- every constructor
// registers apiRequestDuration, so this is safe to call from any
// component's Recorder.
func (r *Recorder) ObserveAPIRequest(method string, d time.Duration, err error) {
	if r.apiRequestDuration == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	r.apiRequestDuration.WithLabelValues(method, outcome).Observe(d.Seconds())
}

// ObserveMachineResourceUsage publishes live FluxVM stats as Prometheus
// gauges so operators can scrape high-frequency usage without continuous
// MachineStatus.resourceUsage etcd writes. No-op when usage is nil or on
// Recorders that did not register machineResourceUsage.
func (r *Recorder) ObserveMachineResourceUsage(namespace, name string, usage *model.ResourceUsage) {
	if r.machineResourceUsage == nil || usage == nil {
		return
	}
	r.machineResourceUsage.WithLabelValues(namespace, name, "cpu_percent").Set(usage.CPUPercent)
	r.machineResourceUsage.WithLabelValues(namespace, name, "memory_bytes").Set(float64(usage.MemoryBytes))
	r.machineResourceUsage.WithLabelValues(namespace, name, "disk_read_bytes").Set(float64(usage.DiskReadBytes))
	r.machineResourceUsage.WithLabelValues(namespace, name, "disk_write_bytes").Set(float64(usage.DiskWriteBytes))
}

// ObserveHTTPRequest records one kairon-ui HTTP request. No-op on any
// Recorder but NewUIRecorder's. route must be a registered mux pattern
// (see internal/uiapi's routePattern), not a raw request path -- an
// unbounded label value here would defeat the whole reason this is a
// label instead of a log line.
func (r *Recorder) ObserveHTTPRequest(method, route string, status int, d time.Duration) {
	if r.uiRequestDuration == nil {
		return
	}
	class := "2xx"
	switch {
	case status >= 500:
		class = "5xx"
	case status >= 400:
		class = "4xx"
	case status >= 300:
		class = "3xx"
	}
	r.uiRequestDuration.WithLabelValues(method, route, class).Observe(d.Seconds())
}

// ObserveMachineUsageFreshness records only successful live stats reads.
func (r *Recorder) ObserveMachineUsageFreshness(namespace, name string) {
	if r.machineUsageTimestamp != nil {
		r.machineUsageTimestamp.WithLabelValues(namespace, name).Set(float64(time.Now().Unix()))
	}
}
