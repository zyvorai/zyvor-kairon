// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package metrics

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

// EdgeRecorder is the VM-edge eBPF metric set. It is separate from
// Recorder so existing controller/node registries stay unchanged.
type EdgeRecorder struct {
	Drops             *prometheus.CounterVec
	ConntrackRestored prometheus.Counter
	BlackholeMs       prometheus.Histogram
	// BlackholeWindow is the last recorded migration traffic gap for a
	// Machine, in milliseconds (status.network.edge.blackholeWindowMs).
	BlackholeWindow *prometheus.GaugeVec
	// FlowRTT observes per-flow RTT when FluxVM or PacketWolf attaches
	// latency attribution. Labels are namespace and machine only.
	FlowRTT *prometheus.HistogramVec
	// FlowsAttributed counts newly seen flows by attribution source
	// (fluxvm, packetwolf, guest-agent, unknown, or none). No process
	// identity labels.
	FlowsAttributed *prometheus.CounterVec

	mu       sync.Mutex
	seenRTT  map[string]uint64
	seenAttr map[string]struct{}
}

// NewEdgeRecorder registers attributed-drop and migration-continuity metrics.
func NewEdgeRecorder(reg prometheus.Registerer) *EdgeRecorder {
	r := &EdgeRecorder{
		Drops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_net_drops_total",
			Help: "VM-edge drops attributed to a MachineNetworkPolicy or dataplane guard.",
		}, []string{"namespace", "machine", "reason", "policy"}),
		ConntrackRestored: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kairon_net_conntrack_restored_total",
			Help: "Conntrack entries restored onto a destination during live migration.",
		}),
		BlackholeMs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kairon_net_migration_blackhole_ms",
			Help:    "Milliseconds between conntrack export and restore.",
			Buckets: []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500},
		}),
		BlackholeWindow: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_net_migration_blackhole_window_ms",
			Help: "Last live-migration traffic gap in milliseconds (conntrack export to restore), as recorded on status.network.edge.blackholeWindowMs.",
		}, []string{"namespace", "machine"}),
		FlowRTT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kairon_net_flow_rtt_seconds",
			Help:    "VM-edge flow RTT in seconds, observed when FluxVM or PacketWolf includes latency attribution. Sampled when a flow is first seen or its RTT changes.",
			Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, []string{"namespace", "machine"}),
		FlowsAttributed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_net_flows_attributed_total",
			Help: "VM-edge flows counted once when first seen, labelled by process-attribution source (fluxvm, packetwolf, guest-agent, unknown, or none). No PID or comm labels.",
		}, []string{"namespace", "machine", "source"}),
		seenRTT:  map[string]uint64{},
		seenAttr: map[string]struct{}{},
	}
	reg.MustRegister(r.Drops, r.ConntrackRestored, r.BlackholeMs, r.BlackholeWindow, r.FlowRTT, r.FlowsAttributed)
	return r
}

// ObserveDrop increments the attributed drop counter. An empty policy is
// recorded as "-" so the label set stays stable.
func (r *EdgeRecorder) ObserveDrop(namespace, machine, reason, policy string) {
	if r == nil {
		return
	}
	if policy == "" {
		policy = "-"
	}
	r.Drops.WithLabelValues(namespace, machine, reason, policy).Inc()
}

// ObserveDrops adds n packets to the attributed drop counter.
func (r *EdgeRecorder) ObserveDrops(namespace, machine, reason, policy string, n uint64) {
	if r == nil || n == 0 {
		return
	}
	if policy == "" {
		policy = "-"
	}
	r.Drops.WithLabelValues(namespace, machine, reason, policy).Add(float64(n))
}

// ObserveConntrackRestore records how many entries moved, how long the
// guest was black-holed, and the per-Machine last traffic-gap gauge.
func (r *EdgeRecorder) ObserveConntrackRestore(namespace, machine string, restored int, blackholeMs int64) {
	if r == nil || restored < 0 {
		return
	}
	r.ConntrackRestored.Add(float64(restored))
	r.BlackholeMs.Observe(float64(blackholeMs))
	r.SetBlackholeWindow(namespace, machine, blackholeMs)
}

// SetBlackholeWindow writes the last recorded traffic gap for a Machine.
// A negative window is ignored; zero is a valid recorded gap.
func (r *EdgeRecorder) SetBlackholeWindow(namespace, machine string, blackholeMs int64) {
	if r == nil || r.BlackholeWindow == nil || namespace == "" || machine == "" || blackholeMs < 0 {
		return
	}
	r.BlackholeWindow.WithLabelValues(namespace, machine).Set(float64(blackholeMs))
}

// ObserveFlows records RTT and attribution-source metrics from a flow
// dump. RTT is observed only when a flow is new or its RTT changed, so
// a long-lived flow is not re-counted every reconcile tick. Process
// PID and comm are never used as labels.
func (r *EdgeRecorder) ObserveFlows(namespace, machine string, flows []ebpfedge.FlowRecord) {
	if r == nil {
		return
	}
	live := make(map[string]struct{}, len(flows))
	for i := range flows {
		key := flowMetricKey(namespace, machine, &flows[i])
		live[key] = struct{}{}
		r.observeOneFlow(namespace, machine, &flows[i])
	}
	// Drop keys that left this Machine's current dump so the maps
	// cannot grow without bound across ticks.
	prefix := namespace + "/" + machine + "/"
	r.mu.Lock()
	evictStale(r.seenRTT, prefix, live)
	evictStale(r.seenAttr, prefix, live)
	r.mu.Unlock()
}

func evictStale[V any](m map[string]V, prefix string, live map[string]struct{}) {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			if _, ok := live[k]; !ok {
				delete(m, k)
			}
		}
	}
}

func (r *EdgeRecorder) observeOneFlow(namespace, machine string, fr *ebpfedge.FlowRecord) {
	key := flowMetricKey(namespace, machine, fr)
	if us := flowRTTUs(fr.Latency); us > 0 {
		r.mu.Lock()
		prev, seen := r.seenRTT[key]
		if !seen || prev != us {
			r.seenRTT[key] = us
			r.mu.Unlock()
			r.FlowRTT.WithLabelValues(namespace, machine).Observe(float64(us) / 1e6)
		} else {
			r.mu.Unlock()
		}
	}
	r.mu.Lock()
	if _, seen := r.seenAttr[key]; seen {
		r.mu.Unlock()
		return
	}
	r.seenAttr[key] = struct{}{}
	r.mu.Unlock()
	r.FlowsAttributed.WithLabelValues(namespace, machine, attributionSource(*fr)).Inc()
}

func flowRTTUs(l ebpfedge.LatencyAttr) uint64 {
	if l.RTTUs > 0 {
		return l.RTTUs
	}
	if l.AvgRTTUs > 0 {
		return l.AvgRTTUs
	}
	return l.P50Us
}

func attributionSource(fr ebpfedge.FlowRecord) string {
	if fr.Process.IsZero() && fr.HostProcess.IsZero() {
		return "none"
	}
	src := fr.Process.Source
	if src == "" {
		src = fr.HostProcess.Source
	}
	switch src {
	case ebpfedge.AttrSourceFluxVM, ebpfedge.AttrSourcePacketWolf, ebpfedge.AttrSourceGuestAgent:
		return src
	default:
		return "unknown"
	}
}

func flowMetricKey(namespace, machine string, fr *ebpfedge.FlowRecord) string {
	return fmt.Sprintf("%s/%s/%d/%s/%s:%d/%s:%d", namespace, machine, fr.Identity, fr.Proto, fr.SrcIP, fr.SrcPort, fr.DstIP, fr.DstPort)
}
