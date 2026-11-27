// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package ebpfedge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DecodeFlowRecords unmarshals a FluxVM or PacketWolf flow dump. It
// accepts a bare array or an object wrapping flows under "flows",
// "items" or "data", and tolerates snake_case plus flattened process
// and latency aliases (pid, comm, rtt_us, rttMs, …).
func DecodeFlowRecords(raw []byte) ([]FlowRecord, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty flow dump")
	}
	var arr []json.RawMessage
	switch raw[0] {
	case '[':
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
	case '{':
		var wrap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wrap); err != nil {
			return nil, err
		}
		var found bool
		for _, key := range []string{"flows", "items", "data"} {
			if v, ok := wrap[key]; ok && len(v) > 0 && v[0] == '[' {
				if err := json.Unmarshal(v, &arr); err != nil {
					return nil, err
				}
				found = true
				break
			}
		}
		if !found {
			// A single flow object.
			arr = []json.RawMessage{json.RawMessage(raw)}
		}
	default:
		return nil, fmt.Errorf("flow dump is not JSON")
	}
	out := make([]FlowRecord, 0, len(arr))
	for i, item := range arr {
		fr, err := decodeOneFlow(item)
		if err != nil {
			return nil, fmt.Errorf("flow %d: %w", i, err)
		}
		out = append(out, fr)
	}
	return out, nil
}

// DecodeDropEvents unmarshals attributed drops from FluxVM or PacketWolf.
// Wrappers "drops", "items" and "data" are accepted, as are policy /
// count / packets aliases.
func DecodeDropEvents(raw []byte) ([]DropEvent, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty drop dump")
	}
	var arr []json.RawMessage
	switch raw[0] {
	case '[':
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
	case '{':
		var wrap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wrap); err != nil {
			return nil, err
		}
		var found bool
		for _, key := range []string{"drops", "items", "data", "events"} {
			if v, ok := wrap[key]; ok && len(v) > 0 && v[0] == '[' {
				if err := json.Unmarshal(v, &arr); err != nil {
					return nil, err
				}
				found = true
				break
			}
		}
		if !found {
			arr = []json.RawMessage{json.RawMessage(raw)}
		}
	default:
		return nil, fmt.Errorf("drop dump is not JSON")
	}
	out := make([]DropEvent, 0, len(arr))
	for i, item := range arr {
		ev, err := decodeOneDrop(item)
		if err != nil {
			return nil, fmt.Errorf("drop %d: %w", i, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// DataplaneStats is a typed view of FluxVM's /network/stats payload,
// including optional latency fields when the dataplane supplies them.
// Unknown keys are retained in Extra.
type DataplaneStats struct {
	RxPackets uint64 `json:"rxPackets,omitempty"`
	TxPackets uint64 `json:"txPackets,omitempty"`
	RxBytes   uint64 `json:"rxBytes,omitempty"`
	TxBytes   uint64 `json:"txBytes,omitempty"`
	Dropped   uint64 `json:"dropped,omitempty"`
	Flows     uint64 `json:"flows,omitempty"`
	Latency   LatencyAttr
	Extra     map[string]any
}

// DecodeDataplaneStats reads a stats object and lifts well-known
// latency keys (rttUs, rtt_us, rttMs, srtt_us, …) into Latency.
func DecodeDataplaneStats(raw []byte) (DataplaneStats, error) {
	raw = bytes.TrimSpace(raw)
	var s DataplaneStats
	if len(raw) == 0 {
		return s, fmt.Errorf("empty stats dump")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return s, err
	}
	s.RxPackets = uintFrom(obj, "rxPackets", "rx_packets", "rxPkts")
	s.TxPackets = uintFrom(obj, "txPackets", "tx_packets", "txPkts")
	s.RxBytes = uintFrom(obj, "rxBytes", "rx_bytes")
	s.TxBytes = uintFrom(obj, "txBytes", "tx_bytes")
	s.Dropped = uintFrom(obj, "dropped", "drops", "dropPackets", "drop_packets")
	s.Flows = uintFrom(obj, "flows", "flowCount", "flow_count")
	s.Latency = latencyFromMap(obj)
	if nested, ok := obj["latency"].(map[string]any); ok {
		s.Latency = mergeLatency(s.Latency, latencyFromMap(nested))
		s.Latency = mergeLatency(s.Latency, SummarizeRTT(rttSamplesFrom(nested)))
	}
	s.Latency = mergeLatency(s.Latency, SummarizeRTT(rttSamplesFrom(obj)))
	s.Extra = obj
	return s, nil
}

func decodeOneFlow(raw json.RawMessage) (FlowRecord, error) {
	var fr FlowRecord
	if err := json.Unmarshal(raw, &fr); err != nil {
		return FlowRecord{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return FlowRecord{}, err
	}
	if fr.Proto == "" {
		fr.Proto = strFrom(obj, "proto", "protocol")
	}
	if fr.SrcIP == "" {
		fr.SrcIP = strFrom(obj, "srcIP", "src_ip", "src")
	}
	if fr.DstIP == "" {
		fr.DstIP = strFrom(obj, "dstIP", "dst_ip", "dst")
	}
	if fr.SrcPort == 0 {
		fr.SrcPort = uint16(uintFrom(obj, "srcPort", "src_port"))
	}
	if fr.DstPort == 0 {
		fr.DstPort = uint16(uintFrom(obj, "dstPort", "dst_port"))
	}
	if fr.Identity == 0 {
		fr.Identity = uint32(uintFrom(obj, "identity", "id"))
	}
	if fr.Bytes == 0 {
		fr.Bytes = uintFrom(obj, "bytes", "byte_count")
	}
	if fr.Packets == 0 {
		fr.Packets = uintFrom(obj, "packets", "packet_count", "count")
	}
	if fr.Reason == "" {
		fr.Reason = strFrom(obj, "reason", "dropReason", "drop_reason")
	}
	if !fr.Denied && (truthy(obj["denied"]) || fr.Reason != "" && fr.Reason != ReasonAllow) {
		if fr.Reason != "" && fr.Reason != ReasonAllow {
			fr.Denied = true
		}
	}
	if fr.FirstSeen.IsZero() {
		fr.FirstSeen = timeFrom(obj, "firstSeen", "first_seen")
	}
	if fr.LastSeen.IsZero() {
		fr.LastSeen = timeFrom(obj, "lastSeen", "last_seen")
	}
	attr := attributionFromMap(obj)
	attr.ApplyToFlow(&fr)
	return fr, nil
}

func decodeOneDrop(raw json.RawMessage) (DropEvent, error) {
	var ev DropEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return DropEvent{}, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return DropEvent{}, err
	}
	if ev.Machine == "" {
		ev.Machine = strFrom(obj, "machine", "vm", "name")
	}
	if ev.Namespace == "" {
		ev.Namespace = strFrom(obj, "namespace", "ns")
	}
	if ev.Reason == "" {
		ev.Reason = strFrom(obj, "reason", "dropReason", "drop_reason")
	}
	if ev.PolicyName == "" {
		ev.PolicyName = strFrom(obj, "policyName", "policy", "policy_name")
	}
	if ev.SrcIP == "" {
		ev.SrcIP = strFrom(obj, "srcIP", "src_ip", "src")
	}
	if ev.DstIP == "" {
		ev.DstIP = strFrom(obj, "dstIP", "dst_ip", "dst")
	}
	if ev.Proto == "" {
		ev.Proto = strFrom(obj, "proto", "protocol")
	}
	if ev.SrcPort == 0 {
		ev.SrcPort = uint16(uintFrom(obj, "srcPort", "src_port"))
	}
	if ev.DstPort == 0 {
		ev.DstPort = uint16(uintFrom(obj, "dstPort", "dst_port"))
	}
	if ev.Identity == 0 {
		ev.Identity = uint32(uintFrom(obj, "identity", "id"))
	}
	if ev.Packets == 0 {
		ev.Packets = uintFrom(obj, "packets", "packet_count", "count")
	}
	if ev.Bytes == 0 {
		ev.Bytes = uintFrom(obj, "bytes", "byte_count")
	}
	attr := attributionFromMap(obj)
	attr.ApplyToDrop(&ev)
	return ev, nil
}

func attributionFromMap(obj map[string]any) Attribution {
	var a Attribution
	if p, ok := asMap(obj["process"]); ok {
		a.Process = processFromMap(p)
	}
	if p, ok := asMap(obj["hostProcess"]); ok {
		a.HostProcess = processFromMap(p)
	} else if p, ok := asMap(obj["host_process"]); ok {
		a.HostProcess = processFromMap(p)
	}
	if l, ok := asMap(obj["latency"]); ok {
		a.Latency = latencyFromMap(l)
	}
	// Flattened aliases (PacketWolf / older FluxVM dumps). Do not
	// reuse ambiguous root keys such as "name", "source" or "process".
	flat := ProcessAttr{
		Scope:      strFrom(obj, "scope"),
		PID:        uint32(uintFrom(obj, "pid", "process_id", "processId")),
		TID:        uint32(uintFrom(obj, "tid", "thread_id", "threadId")),
		UID:        uint32(uintFrom(obj, "uid")),
		GID:        uint32(uintFrom(obj, "gid")),
		Comm:       strFrom(obj, "comm", "comm_name", "processName"),
		Executable: strFrom(obj, "executable", "exe", "binary"),
		Cgroup:     strFrom(obj, "cgroup", "cgroup_path", "cgroupPath"),
		CgroupID:   uintFrom(obj, "cgroupID", "cgroup_id", "cgroupId"),
		NetNS:      uint32(uintFrom(obj, "netns", "net_ns", "netNS")),
		Container:  strFrom(obj, "container", "container_id", "containerId"),
	}
	if s := strFrom(obj, "attrSource", "attr_source"); s != "" {
		flat.Source = s
	}
	if a.Process.IsZero() && a.HostProcess.IsZero() && !flat.IsZero() {
		if flat.Scope == AttrScopeHost {
			a.HostProcess = flat
		} else {
			a.Process = flat
		}
	} else {
		a.Process = mergeProcess(a.Process, flat)
	}
	if a.Latency.IsZero() {
		a.Latency = latencyFromMap(obj)
	} else {
		a.Latency = mergeLatency(a.Latency, latencyFromMap(obj))
	}
	return a.Normalize()
}

func processFromMap(obj map[string]any) ProcessAttr {
	if obj == nil {
		return ProcessAttr{}
	}
	return ProcessAttr{
		Source:     strFrom(obj, "source"),
		Scope:      strFrom(obj, "scope"),
		PID:        uint32(uintFrom(obj, "pid", "process_id", "processId")),
		TID:        uint32(uintFrom(obj, "tid", "thread_id", "threadId")),
		UID:        uint32(uintFrom(obj, "uid")),
		GID:        uint32(uintFrom(obj, "gid")),
		Comm:       strFrom(obj, "comm", "comm_name", "name"),
		Executable: strFrom(obj, "executable", "exe", "binary"),
		Cgroup:     strFrom(obj, "cgroup", "cgroup_path", "cgroupPath"),
		CgroupID:   uintFrom(obj, "cgroupID", "cgroup_id", "cgroupId"),
		NetNS:      uint32(uintFrom(obj, "netns", "net_ns", "netNS")),
		Container:  strFrom(obj, "container", "container_id", "containerId"),
	}
}

func latencyFromMap(obj map[string]any) LatencyAttr {
	if obj == nil {
		return LatencyAttr{}
	}
	l := LatencyAttr{
		RTTVarUs:  uintFrom(obj, "rttVarUs", "rtt_var_us", "rttvar_us", "rttVar"),
		MinRTTUs:  uintFrom(obj, "minRttUs", "min_rtt_us", "minRtt"),
		MaxRTTUs:  uintFrom(obj, "maxRttUs", "max_rtt_us", "maxRtt"),
		LastAckUs: uintFrom(obj, "lastAckUs", "last_ack_us"),
		AvgRTTUs:  uintFrom(obj, "avgRttUs", "avg_rtt_us", "rttAvgUs", "rtt_avg_us"),
		P50Us:     uintFrom(obj, "p50Us", "p50_us", "rttP50Us", "rtt_p50_us"),
		P95Us:     uintFrom(obj, "p95Us", "p95_us", "rttP95Us", "rtt_p95_us"),
		Samples:   uint32(uintFrom(obj, "samples", "rttSamples", "rtt_samples", "rttSampleCount", "rtt_sample_count")),
	}
	if l.AvgRTTUs == 0 {
		if ms := floatFrom(obj, "avgRttMs", "avg_rtt_ms"); ms > 0 {
			l.AvgRTTUs = uint64(ms * 1000)
		}
	}
	if l.P50Us == 0 {
		if ms := floatFrom(obj, "p50Ms", "p50_ms", "rttP50Ms"); ms > 0 {
			l.P50Us = uint64(ms * 1000)
		}
	}
	if l.P95Us == 0 {
		if ms := floatFrom(obj, "p95Ms", "p95_ms", "rttP95Ms"); ms > 0 {
			l.P95Us = uint64(ms * 1000)
		}
	}
	if v := uintFrom(obj, "rttUs", "rtt_us", "srttUs", "srtt_us"); v > 0 {
		l.RTTUs = v
	} else if ms := floatFrom(obj, "rttMs", "rtt_ms", "rtt"); ms > 0 {
		// A bare "rtt" of 1.4 is treated as milliseconds; an integer
		// >= 1000 is treated as microseconds (common in kernel dumps).
		if ms >= 1000 && ms == float64(uint64(ms)) && !hasAnyKey(obj, "rttMs", "rtt_ms") {
			l.RTTUs = uint64(ms)
		} else {
			l.RTTUs = uint64(ms * 1000)
		}
	}
	if l.MinRTTUs == 0 {
		if ms := floatFrom(obj, "minRttMs", "min_rtt_ms"); ms > 0 {
			l.MinRTTUs = uint64(ms * 1000)
		}
	}
	if l.MaxRTTUs == 0 {
		if ms := floatFrom(obj, "maxRttMs", "max_rtt_ms"); ms > 0 {
			l.MaxRTTUs = uint64(ms * 1000)
		}
	}
	return l
}

func rttSamplesFrom(obj map[string]any) []uint64 {
	if obj == nil {
		return nil
	}
	for _, key := range []string{"rtts", "rttUsSamples", "rtt_us_samples", "rttHistogram"} {
		arr, ok := obj[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		out := make([]uint64, 0, len(arr))
		for _, v := range arr {
			switch t := v.(type) {
			case float64:
				if t > 0 {
					out = append(out, uint64(t))
				}
			case json.Number:
				n, err := t.Int64()
				if err == nil && n > 0 {
					out = append(out, uint64(n))
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func hasAnyKey(obj map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	return false
}

func strFrom(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			switch t := v.(type) {
			case string:
				if s := strings.TrimSpace(t); s != "" && s != "-" {
					return s
				}
			}
		}
	}
	return ""
}

func uintFrom(obj map[string]any, keys ...string) uint64 {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			switch t := v.(type) {
			case float64:
				if t > 0 {
					return uint64(t)
				}
			case json.Number:
				n, err := t.Int64()
				if err == nil && n > 0 {
					return uint64(n)
				}
			case string:
				var n uint64
				if _, err := fmt.Sscanf(t, "%d", &n); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

func floatFrom(obj map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			switch t := v.(type) {
			case float64:
				if t > 0 {
					return t
				}
			case json.Number:
				n, err := t.Float64()
				if err == nil && n > 0 {
					return n
				}
			case string:
				var n float64
				if _, err := fmt.Sscanf(t, "%f", &n); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

func truthy(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func timeFrom(obj map[string]any, keys ...string) time.Time {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if ts, err := time.Parse(layout, t); err == nil {
					return ts
				}
			}
		}
	}
	return time.Time{}
}
