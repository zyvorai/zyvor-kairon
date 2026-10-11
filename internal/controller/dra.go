// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"regexp"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

var draBDFPattern = regexp.MustCompile(`(?i)^(?:[0-9a-f]{4}:)?[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// buildDRAHints resolves, for every Machine with spec.deviceClaims, which
// node (if any) internal/scheduler should prefer -- a best-effort DRA
// topology-awareness signal, not an authoritative placement decision:
// kairon-controller has no real role in DRA allocation itself (that's the
// cluster's own DRA driver/scheduler-plugin machinery, entirely outside
// this project), it only reads back an allocation that already happened
// and nudges the Machine toward whichever node hosts it. Returns a
// "namespace/name" -> node name map; a Machine absent from it (including
// every Machine with no deviceClaims at all) gets no hint --
// draPreferredNode == "" is the overwhelmingly common path into
// scheduler.Choose and adds no scheduling behavior at all.
//
// Both ResourceClaim/ResourceSlice API calls are skipped entirely -- not
// even attempted -- unless at least one Machine actually has deviceClaims
// set, so a cluster that simply doesn't have the DRA API installed/enabled
// never sees an error from this.
func (c *Controller) buildDRAHints(ctx context.Context, machines []model.Machine) (map[string]string, error) {
	placements, err := c.buildDRAPlacements(ctx, machines)
	if err != nil || placements == nil {
		return nil, err
	}
	hints := map[string]string{}
	for k, p := range placements {
		if p.Node != "" {
			hints[k] = p.Node
		}
	}
	return hints, nil
}

// buildDRAPlacements is buildDRAHints plus what a hard topology policy
// needs: the allocated devices' BDFs (the device result when it is a PCI
// address, else the claim's kairon.zyvor.dev/vfio-bdf annotation, the same
// sources kairon-node passes to FluxVM) and whether any claim is still
// unallocated.
func (c *Controller) buildDRAPlacements(ctx context.Context, machines []model.Machine) (map[string]scheduler.DRAPlacement, error) {
	anyDeviceClaims := false
	for _, m := range machines {
		if len(m.Spec.DeviceClaims) > 0 {
			anyDeviceClaims = true
			break
		}
	}
	if !anyDeviceClaims {
		return nil, nil
	}

	slices, err := c.Kube.ListResourceSlices(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return nil, err
	}
	// "driver/poolName" -> the node hosting it. Only a node-local pool
	// (spec.nodeName set) contributes a hint -- a network-attached pool
	// (no single node "owns" it) contributes none, same as an
	// unresolvable claim.
	poolNode := map[string]string{}
	for _, sl := range slices {
		if sl.Spec.NodeName == "" {
			continue
		}
		poolNode[sl.Spec.Driver+"/"+sl.Spec.Pool.Name] = sl.Spec.NodeName
	}

	claims, err := c.Kube.ListResourceClaims(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return nil, err
	}
	type claimInfo struct {
		node      string
		bdfs      []string
		allocated bool
	}
	// "namespace/claimName" -> its allocation.
	byClaim := map[string]claimInfo{}
	for _, cl := range claims {
		info := claimInfo{}
		if cl.Status.Allocation != nil {
			info.allocated = true
			for _, res := range cl.Status.Allocation.Devices.Results {
				if node, ok := poolNode[res.Driver+"/"+res.Pool]; ok && info.node == "" {
					info.node = node // first cut: one node per claim, first resolvable device wins
				}
				if d := strings.TrimSpace(res.Device); draBDFPattern.MatchString(d) {
					info.bdfs = append(info.bdfs, d)
				}
			}
			for _, raw := range strings.Split(cl.Metadata.Annotations[model.AnnotationVFIOBDF], ",") {
				if v := strings.TrimSpace(raw); v != "" {
					info.bdfs = append(info.bdfs, v)
				}
			}
		}
		byClaim[cl.Metadata.Namespace+"/"+cl.Metadata.Name] = info
	}

	out := map[string]scheduler.DRAPlacement{}
	for _, m := range machines {
		if len(m.Spec.DeviceClaims) == 0 {
			continue
		}
		var p scheduler.DRAPlacement
		for _, ref := range m.Spec.DeviceClaims {
			info, ok := byClaim[m.Namespace()+"/"+ref.Name]
			if !ok || !info.allocated {
				p.Pending = true
				continue
			}
			if p.Node == "" {
				p.Node = info.node // first cut: one node per Machine, first resolvable claim wins
			}
			p.BDFs = append(p.BDFs, info.bdfs...)
		}
		out[m.Namespace()+"/"+m.Metadata.Name] = p
	}
	return out, nil
}
