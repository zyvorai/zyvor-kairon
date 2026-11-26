// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package ebpfedge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Attribution source and scope names. FluxVM and PacketWolf set these
// so operators can tell a host-side (vhost/tap) identity from a guest
// process, and which system supplied it.
const (
	AttrSourceFluxVM     = "fluxvm"
	AttrSourcePacketWolf = "packetwolf"
	AttrSourceGuestAgent = "guest-agent"

	AttrScopeGuest = "guest"
	AttrScopeHost  = "host"
)

// ProcessAttr is optional process-level attribution for a flow or drop.
// Every field is optional: an empty value means the dataplane did not
// (or cannot) identify that property. Kairon never inspects guest
// memory itself; FluxVM or PacketWolf fills this in.
type ProcessAttr struct {
	// Source is who supplied the attribution (fluxvm, packetwolf, guest-agent).
	Source string `json:"source,omitempty"`
	// Scope is "guest" for a process inside the VM, or "host" for a
	// hypervisor/vhost/tap thread. Empty means unknown.
	Scope      string `json:"scope,omitempty"`
	PID        uint32 `json:"pid,omitempty"`
	TID        uint32 `json:"tid,omitempty"`
	UID        uint32 `json:"uid,omitempty"`
	GID        uint32 `json:"gid,omitempty"`
	Comm       string `json:"comm,omitempty"`
	Executable string `json:"executable,omitempty"`
	Cgroup     string `json:"cgroup,omitempty"`
	CgroupID   uint64 `json:"cgroupID,omitempty"`
	NetNS      uint32 `json:"netns,omitempty"`
	Container  string `json:"container,omitempty"`
}

// IsZero reports whether no process field was set.
func (p ProcessAttr) IsZero() bool {
	return p.Source == "" && p.Scope == "" && p.PID == 0 && p.TID == 0 &&
		p.UID == 0 && p.GID == 0 && p.Comm == "" && p.Executable == "" &&
		p.Cgroup == "" && p.CgroupID == 0 && p.NetNS == 0 && p.Container == ""
}

// Display is a compact one-line form for CLI and dashboard tables.
// Empty attribution is "-".
func (p ProcessAttr) Display() string {
	if p.IsZero() {
		return "-"
	}
	name := p.Comm
	if name == "" {
		name = p.Executable
	}
	if name == "" {
		name = p.Container
	}
	var b strings.Builder
	if p.Scope != "" {
		b.WriteString(p.Scope)
		b.WriteByte(':')
	}
	if name != "" {
		b.WriteString(name)
	}
	var extra []string
	if p.PID != 0 {
		extra = append(extra, fmt.Sprintf("pid=%d", p.PID))
	}
	if p.Cgroup != "" {
		cg := p.Cgroup
		if len(cg) > 40 {
			cg = "…" + cg[len(cg)-39:]
		}
		extra = append(extra, "cgroup="+cg)
	} else if p.CgroupID != 0 {
		extra = append(extra, fmt.Sprintf("cgroup=%d", p.CgroupID))
	}
	if p.Container != "" && name != p.Container {
		extra = append(extra, "ctr="+p.Container)
	}
	if len(extra) > 0 {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), ":") {
			// name[pid=…]
			fmt.Fprintf(&b, "[%s]", strings.Join(extra, ","))
		} else {
			b.WriteString(strings.Join(extra, ","))
		}
	}
	if b.Len() == 0 {
		if p.Source != "" {
			return p.Source
		}
		return "-"
	}
	return b.String()
}

// LatencyAttr is optional RTT and timing data a dataplane (FluxVM
// conntrack, TCP timestamps, or a PacketWolf probe) may attach to a
// flow. Values are microseconds unless noted.
type LatencyAttr struct {
	RTTUs     uint64 `json:"rttUs,omitempty"`
	RTTVarUs  uint64 `json:"rttVarUs,omitempty"`
	MinRTTUs  uint64 `json:"minRttUs,omitempty"`
	MaxRTTUs  uint64 `json:"maxRttUs,omitempty"`
	LastAckUs uint64 `json:"lastAckUs,omitempty"`
	Samples   uint32 `json:"samples,omitempty"`
}

// IsZero reports whether no latency field was set.
func (l LatencyAttr) IsZero() bool {
	return l.RTTUs == 0 && l.RTTVarUs == 0 && l.MinRTTUs == 0 &&
		l.MaxRTTUs == 0 && l.LastAckUs == 0 && l.Samples == 0
}

// RTT is the smoothed round-trip time, or zero if unknown.
func (l LatencyAttr) RTT() time.Duration {
	return time.Duration(l.RTTUs) * time.Microsecond
}

// Display formats the primary RTT for tables ("1.4ms", "800µs", "-").
func (l LatencyAttr) Display() string {
	if l.RTTUs == 0 && l.MinRTTUs == 0 {
		return "-"
	}
	us := l.RTTUs
	if us == 0 {
		us = l.MinRTTUs
	}
	return FormatMicroseconds(us)
}

// FormatMicroseconds renders a microsecond count as a short latency string.
func FormatMicroseconds(us uint64) string {
	if us == 0 {
		return "-"
	}
	switch {
	case us < 1000:
		return fmt.Sprintf("%dµs", us)
	case us < 1_000_000:
		if us%1000 == 0 {
			return fmt.Sprintf("%dms", us/1000)
		}
		return fmt.Sprintf("%.1fms", float64(us)/1000)
	default:
		if us%1_000_000 == 0 {
			return fmt.Sprintf("%ds", us/1_000_000)
		}
		return fmt.Sprintf("%.2fs", float64(us)/1_000_000)
	}
}

// Attribution is the extension document FluxVM or PacketWolf attaches
// to a flow or drop. Process is the primary (prefer guest) identity;
// HostProcess is a hypervisor/vhost thread when both are known.
type Attribution struct {
	Process     ProcessAttr `json:"process,omitempty"`
	HostProcess ProcessAttr `json:"hostProcess,omitempty"`
	Latency     LatencyAttr `json:"latency,omitempty"`
}

// IsZero reports whether nothing was attributed.
func (a Attribution) IsZero() bool {
	return a.Process.IsZero() && a.HostProcess.IsZero() && a.Latency.IsZero()
}

// Normalize moves a lone host-scoped Process into HostProcess.
func (a Attribution) Normalize() Attribution {
	if a.HostProcess.IsZero() && !a.Process.IsZero() && a.Process.Scope == AttrScopeHost {
		a.HostProcess = a.Process
		a.Process = ProcessAttr{}
	}
	if a.Process.Scope == "" && !a.Process.IsZero() && a.Process.Scope != AttrScopeHost {
		// leave scope unset when the supplier did not say
	}
	return a
}

// Merge fills empty fields of a from b. Non-zero fields in a win, so a
// FluxVM guest identity is not overwritten by a later PacketWolf host
// thread; complementary fields still land.
func (a Attribution) Merge(b Attribution) Attribution {
	a = a.Normalize()
	b = b.Normalize()
	a.Process = mergeProcess(a.Process, b.Process)
	a.HostProcess = mergeProcess(a.HostProcess, b.HostProcess)
	if !a.HostProcess.IsZero() && a.Process.Scope == AttrScopeHost && a.HostProcess.Scope == AttrScopeHost {
		// already split
	}
	// If b brought a guest process and a only has a host one sitting in Process.
	if a.Process.IsZero() && !b.Process.IsZero() {
		a.Process = b.Process
	}
	if a.HostProcess.IsZero() && !b.HostProcess.IsZero() {
		a.HostProcess = b.HostProcess
	}
	a.Latency = mergeLatency(a.Latency, b.Latency)
	return a.Normalize()
}

func mergeProcess(dst, src ProcessAttr) ProcessAttr {
	if dst.IsZero() {
		return src
	}
	if dst.Source == "" {
		dst.Source = src.Source
	}
	if dst.Scope == "" {
		dst.Scope = src.Scope
	}
	if dst.PID == 0 {
		dst.PID = src.PID
	}
	if dst.TID == 0 {
		dst.TID = src.TID
	}
	if dst.UID == 0 {
		dst.UID = src.UID
	}
	if dst.GID == 0 {
		dst.GID = src.GID
	}
	if dst.Comm == "" {
		dst.Comm = src.Comm
	}
	if dst.Executable == "" {
		dst.Executable = src.Executable
	}
	if dst.Cgroup == "" {
		dst.Cgroup = src.Cgroup
	}
	if dst.CgroupID == 0 {
		dst.CgroupID = src.CgroupID
	}
	if dst.NetNS == 0 {
		dst.NetNS = src.NetNS
	}
	if dst.Container == "" {
		dst.Container = src.Container
	}
	return dst
}

func mergeLatency(dst, src LatencyAttr) LatencyAttr {
	if dst.IsZero() {
		return src
	}
	if dst.RTTUs == 0 {
		dst.RTTUs = src.RTTUs
	}
	if dst.RTTVarUs == 0 {
		dst.RTTVarUs = src.RTTVarUs
	}
	if dst.MinRTTUs == 0 {
		dst.MinRTTUs = src.MinRTTUs
	}
	if dst.MaxRTTUs == 0 {
		dst.MaxRTTUs = src.MaxRTTUs
	}
	if dst.LastAckUs == 0 {
		dst.LastAckUs = src.LastAckUs
	}
	if dst.Samples == 0 {
		dst.Samples = src.Samples
	}
	return dst
}

// ApplyToFlow copies non-zero attribution onto a flow, leaving fields
// already set on the record intact (a FluxVM payload wins over a hook).
func (a Attribution) ApplyToFlow(fr *FlowRecord) {
	if fr == nil {
		return
	}
	cur := Attribution{Process: fr.Process, HostProcess: fr.HostProcess, Latency: fr.Latency}
	merged := cur.Merge(a)
	fr.Process = merged.Process
	fr.HostProcess = merged.HostProcess
	fr.Latency = merged.Latency
}

// ApplyToDrop copies non-zero attribution onto a drop event.
func (a Attribution) ApplyToDrop(ev *DropEvent) {
	if ev == nil {
		return
	}
	cur := Attribution{Process: ev.Process, HostProcess: ev.HostProcess, Latency: ev.Latency}
	merged := cur.Merge(a)
	ev.Process = merged.Process
	ev.HostProcess = merged.HostProcess
	ev.Latency = merged.Latency
}

// Attributor is the extension point FluxVM and PacketWolf implement so
// Kairon can attach process identity (and optional RTT) to a flow or
// drop without owning BPF programs. A miss is a zero Attribution and a
// nil error, not a failure.
type Attributor interface {
	Name() string
	Lookup(ctx context.Context, key FlowKey) (Attribution, error)
}

// Chain tries each Attributor in order and merges complementary fields.
// An error from any Lookup aborts the chain.
type Chain []Attributor

// Name joins the chained sources.
func (c Chain) Name() string {
	parts := make([]string, 0, len(c))
	for _, a := range c {
		if a == nil {
			continue
		}
		parts = append(parts, a.Name())
	}
	return strings.Join(parts, "+")
}

// Lookup merges attributions from each non-nil member.
func (c Chain) Lookup(ctx context.Context, key FlowKey) (Attribution, error) {
	var out Attribution
	for _, a := range c {
		if a == nil {
			continue
		}
		got, err := a.Lookup(ctx, key)
		if err != nil {
			return out, err
		}
		out = out.Merge(got)
	}
	return out, nil
}

// MapAttributor is an in-memory Attributor for tests and for wiring a
// pre-decoded FluxVM or PacketWolf dump.
type MapAttributor struct {
	Source string
	Flows  map[FlowKey]Attribution
}

// Name returns the configured source, or "map" when empty.
func (m MapAttributor) Name() string {
	if m.Source == "" {
		return "map"
	}
	return m.Source
}

// Lookup returns the stored attribution, or zero if the flow is unknown.
func (m MapAttributor) Lookup(_ context.Context, key FlowKey) (Attribution, error) {
	if m.Flows == nil {
		return Attribution{}, nil
	}
	return m.Flows[key], nil
}

// AnnotateFlows fills empty process and latency fields on each record
// from attr. Records that already carry those fields are left as-is.
func AnnotateFlows(ctx context.Context, flows []FlowRecord, attr Attributor) error {
	if attr == nil {
		return nil
	}
	for i := range flows {
		got, err := attr.Lookup(ctx, flows[i].FlowKey)
		if err != nil {
			return err
		}
		got.ApplyToFlow(&flows[i])
	}
	return nil
}

// AnnotateDrops fills empty process and latency fields on each drop
// from attr, looking up the drop's 5-tuple.
func AnnotateDrops(ctx context.Context, drops []DropEvent, attr Attributor) error {
	if attr == nil {
		return nil
	}
	for i := range drops {
		key := FlowKey{
			Identity: drops[i].Identity,
			Proto:    drops[i].Proto,
			SrcIP:    drops[i].SrcIP,
			DstIP:    drops[i].DstIP,
			SrcPort:  drops[i].SrcPort,
			DstPort:  drops[i].DstPort,
		}
		got, err := attr.Lookup(ctx, key)
		if err != nil {
			return err
		}
		got.ApplyToDrop(&drops[i])
	}
	return nil
}
