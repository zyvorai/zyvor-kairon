// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/model"
)

// edgeDropLimit is how many drop entries one tick reads per Machine.
const edgeDropLimit = 256

// edgeCache holds the policy list from the last network reconcile, so a
// Machine that did not ask for the edge can be checked without another list
// call, and the last drop counts seen per series, so FluxVM's cumulative
// counters become Prometheus increments.
type edgeCache struct {
	mu       sync.Mutex
	policies []model.MachineNetworkPolicy
	drops    ebpfedge.DropCounters
}

func (c *edgeCache) setPolicies(p []model.MachineNetworkPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policies = p
}

func (c *edgeCache) selecting(m model.Machine) (model.MachineNetworkPolicy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.policies {
		if p.Namespace() != m.Namespace() || p.Metadata.DeletionTimestamp != nil {
			continue
		}
		if policySelectsMachine(p, m) {
			return p, true
		}
	}
	return model.MachineNetworkPolicy{}, false
}

// dropDeltas returns how much each series grew since the last call. A series
// that shrank (FluxVM evicted flow entries, or restarted) counts from zero.
// Series under prefix that are no longer reported are forgotten.
func (c *edgeCache) dropDeltas(prefix string, totals map[string]uint64) map[string]uint64 {
	return c.drops.Observe(prefix, totals)
}

// edgeRequested reports whether this Machine asked for the VM-edge eBPF
// contract. Empty Machines stay on today's path so a legacy FluxVM is not
// probed for endpoints it does not have.
func edgeRequested(m model.Machine) bool {
	n := m.Spec.Network
	if n.AntiSpoof || n.LearnIP || n.QoS != nil {
		return true
	}
	return strings.EqualFold(n.DataplaneMode, "ebpf")
}

// policyNeedsEdge reports whether a policy sets fields only the VM edge
// enforces, so selecting it turns the edge on for any tap Machine.
func policyNeedsEdge(p model.MachineNetworkPolicy) bool {
	pol := p.Spec.Policy
	return len(pol.AllowSNI) > 0 || len(pol.AllowDNS) > 0 ||
		pol.MaxIngressMbps != nil || pol.MaxIngressPps != nil
}

func mergeSelectingPolicy(spec ebpfedge.EdgeSpec, p model.MachineNetworkPolicy) ebpfedge.EdgeSpec {
	spec.PolicyName = p.Metadata.Name
	spec.DefaultAllow = p.Spec.Policy.DefaultAllow
	spec.AllowCIDRs = p.Spec.Policy.AllowCidrs
	spec.DenyCIDRs = p.Spec.Policy.DenyCidrs
	spec.AllowPorts = p.Spec.Policy.AllowPorts
	spec.AllowSNI = p.Spec.Policy.AllowSNI
	spec.AllowDNS = p.Spec.Policy.AllowDNS
	if len(spec.AllowDNS) == 0 {
		spec.AllowDNS = p.Spec.Policy.AllowFqdns
	}
	spec.AllowICMP = p.Spec.Policy.AllowIcmp
	if spec.QoS.IngressMbps == 0 && p.Spec.Policy.MaxIngressMbps != nil {
		spec.QoS.IngressMbps = *p.Spec.Policy.MaxIngressMbps
	}
	if spec.QoS.EgressMbps == 0 && p.Spec.Policy.MaxEgressMbps != nil {
		spec.QoS.EgressMbps = *p.Spec.Policy.MaxEgressMbps
	}
	if spec.QoS.EgressPps == 0 && p.Spec.Policy.MaxEgressPps != nil {
		spec.QoS.EgressPps = *p.Spec.Policy.MaxEgressPps
	}
	if spec.QoS.IngressPps == 0 && p.Spec.Policy.MaxIngressPps != nil {
		spec.QoS.IngressPps = *p.Spec.Policy.MaxIngressPps
	}
	return spec
}

func (a *Agent) selectingPolicy(ctx context.Context, m model.Machine) (model.MachineNetworkPolicy, bool) {
	if a.Kube == nil {
		return model.MachineNetworkPolicy{}, false
	}
	policies, err := a.Kube.ListMachineNetworkPoliciesNamespace(ctx, m.Namespace())
	if err != nil {
		a.log().Warn("edge policy list failed", "machine", m.Metadata.Name, "error", err)
		return model.MachineNetworkPolicy{}, false
	}
	for _, p := range policies {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		if policySelectsMachine(p, m) {
			return p, true
		}
	}
	return model.MachineNetworkPolicy{}, false
}

func buildEdgeSpec(m model.Machine, guestIP string) ebpfedge.EdgeSpec {
	spec := ebpfedge.EdgeSpec{
		Namespace:    m.Namespace(),
		Machine:      m.Metadata.Name,
		AntiSpoof:    m.Spec.Network.AntiSpoof,
		LearnIP:      m.Spec.Network.LearnIP,
		AssignedMAC:  m.Spec.Network.MAC,
		AssignedIP:   guestIP,
		DefaultAllow: true,
	}
	if q := m.Spec.Network.QoS; q != nil {
		if q.IngressMbps != nil {
			spec.QoS.IngressMbps = *q.IngressMbps
		}
		if q.EgressMbps != nil {
			spec.QoS.EgressMbps = *q.EgressMbps
		}
		if q.IngressPps != nil {
			spec.QoS.IngressPps = *q.IngressPps
		}
		if q.EgressPps != nil {
			spec.QoS.EgressPps = *q.EgressPps
		}
	}
	return spec
}

// applyEdge posts the compiled document and returns the status fragment.
// A FluxVM that does not know the endpoint yet is a warning unless the
// Machine asked to fail closed.
// nicMAC is the MAC FluxVM assigned the NIC; anti-spoof uses it when the
// Machine does not set one.
func (a *Agent) applyEdge(ctx context.Context, m model.Machine, runtimeID, guestIP, nicMAC string) (*model.MachineEdgeStatus, error) {
	if a.Flux == nil || runtimeID == "" {
		return nil, nil
	}
	var policy model.MachineNetworkPolicy
	var selected bool
	switch {
	case edgeRequested(m):
		policy, selected = a.selectingPolicy(ctx, m)
	case m.Spec.Network.Mode == "tap":
		policy, selected = a.edge.selecting(m)
		if !selected || !policyNeedsEdge(policy) {
			return nil, nil
		}
	default:
		return nil, nil
	}
	spec := buildEdgeSpec(m, guestIP)
	if spec.AssignedMAC == "" {
		spec.AssignedMAC = nicMAC
	}
	if selected {
		spec = mergeSelectingPolicy(spec, policy)
	}
	compiled, err := ebpfedge.Compile(spec)
	if err != nil {
		return nil, fmt.Errorf("compile edge: %w", err)
	}
	var statusSource string
	if m.Spec.Network.LearnIP && guestIP == "" {
		if ip, source, err := a.Flux.LearnedIP(ctx, runtimeID); err != nil {
			a.log().Warn("learn-ip failed", "machine", m.Metadata.Name, "error", err)
		} else if ip != "" {
			guestIP = ip
			compiled.AssignedIP = ip
			if source == "" {
				source = ebpfedge.IPSourceARP
			}
			statusSource = source
		}
	}
	status := &model.MachineEdgeStatus{
		Identity:      compiled.Identity,
		AntiSpoof:     compiled.AntiSpoof,
		PolicyName:    compiled.PolicyName,
		GuestIPSource: statusSource,
	}
	if guestIP != "" && status.GuestIPSource == "" {
		status.GuestIPSource = ebpfedge.IPSourceAgent
	}
	if res, ok := a.Restores.Get(m.Namespace(), m.Metadata.Name); ok {
		status.ConntrackRestored = res.Restored
		status.BlackholeWindowMs = res.BlackholeWindowMs
	}
	if raw, err := json.Marshal(compiled); err == nil {
		sum := sha256.Sum256(raw)
		status.SpecHash = hex.EncodeToString(sum[:8])
	}
	if err := a.Flux.ApplyEdge(ctx, runtimeID, compiled); err != nil {
		if m.Spec.Network.DataplaneRequired {
			return nil, fmt.Errorf("dataplane required but edge apply failed: %w", err)
		}
		a.log().Warn("edge apply failed; continuing", "machine", m.Metadata.Name, "error", err)
		status.ApplyError = err.Error()
		return status, nil
	}
	status.Applied = true
	a.observeEdgeDrops(ctx, m, runtimeID)
	return status, nil
}

// observeEdgeDrops feeds kairon_net_drops_total from FluxVM's attributed
// drops and emits edge Warning Events from this tick's increments. Best
// effort: a failed read only skips this tick.
func (a *Agent) observeEdgeDrops(ctx context.Context, m model.Machine, runtimeID string) {
	rec := a.Metrics.Edge()
	if rec == nil && a.Kube == nil {
		return
	}
	raw, err := a.Flux.AttributedDrops(ctx, runtimeID, edgeDropLimit)
	if err != nil {
		a.log().Debug("edge drops read failed", "machine", m.Metadata.Name, "error", err)
		return
	}
	var body struct {
		Items []struct {
			Reason     string `json:"reason"`
			PolicyName string `json:"policyName"`
			Direction  string `json:"direction"`
			SrcIP      string `json:"srcIP"`
			DstIP      string `json:"dstIP"`
			Proto      string `json:"proto"`
			DstPort    uint16 `json:"dstPort"`
			Packets    uint64 `json:"packets"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return
	}
	type series struct{ reason, policy, dst string }
	totals := map[string]uint64{}
	labels := map[string]series{}
	prefix := m.Namespace() + "/" + m.Metadata.Name + "/"
	// Bound retained series even when an upstream ignores the requested limit.
	for _, it := range body.Items[:min(len(body.Items), edgeDropLimit)] {
		key := fmt.Sprintf("%s%s|%s|%s|%s|%s|%s|%d", prefix, it.Reason, it.PolicyName, it.Direction, it.SrcIP, it.DstIP, it.Proto, it.DstPort)
		totals[key] = it.Packets
		labels[key] = series{it.Reason, it.PolicyName, it.DstIP}
	}
	var drops []agentplane.Drop
	for key, delta := range a.edge.dropDeltas(prefix, totals) {
		l := labels[key]
		if rec != nil {
			rec.ObserveDrops(m.Namespace(), m.Metadata.Name, l.reason, l.policy, delta)
		}
		drops = append(drops, agentplane.Drop{Reason: strings.ToLower(l.reason), Dst: l.dst, Count: int(delta)})
	}
	a.emitEdgeEvents(ctx, m, drops, a.edgeFlows(ctx, runtimeID))
}

func (a *Agent) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// exportConntrackSnapshot reads the source map and stamps the stable
// identity so the destination restore check matches this Machine. A miss
// is empty, matching network snapshot export: live migration still proceeds.
func (a *Agent) exportConntrackSnapshot(ctx context.Context, m model.Machine, runtimeID string) json.RawMessage {
	if !edgeRequested(m) || a.Flux == nil || runtimeID == "" {
		return nil
	}
	snap, err := a.Flux.ExportConntrack(ctx, runtimeID)
	if err != nil {
		a.log().Warn("conntrack export failed; continuing without it", "machine", m.Metadata.Name, "error", err)
		return nil
	}
	snap.Identity = ebpfedge.StableIdentity(m.Namespace(), m.Metadata.Name)
	if snap.Generation == 0 {
		snap.Generation = uint64(time.Now().UTC().Unix())
	}
	if snap.ExportedAt.IsZero() {
		snap.ExportedAt = time.Now().UTC()
	}
	raw, err := ebpfedge.MarshalSnapshot(snap)
	if err != nil {
		a.log().Warn("conntrack encode failed", "machine", m.Metadata.Name, "error", err)
		return nil
	}
	return raw
}
