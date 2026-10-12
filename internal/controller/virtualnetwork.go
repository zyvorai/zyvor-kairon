// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileVirtualNetworks is the IPAM of every VirtualNetwork: it assigns
// VNIs, gives each referencing Machine an address and records where the
// Machine runs, which is what kairon-node pushes to FluxVM as the network's
// endpoints. Status is written only when it changes.
func (c *Controller) reconcileVirtualNetworks(ctx context.Context, machines []model.Machine, nodes []model.Node) {
	vnets, err := c.Kube.ListVirtualNetworks(ctx)
	if err != nil {
		if !kube.IsNotFound(err) {
			c.Log.Error("list virtual networks failed", "error", err)
		}
		return
	}
	for _, v := range PlanVirtualNetworks(vnets, machines, nodes) {
		if reflect.DeepEqual(v.Status, v.prev) {
			continue
		}
		if err := c.Kube.PatchVirtualNetworkStatus(ctx, v.Namespace, v.Name, v.Status); err != nil {
			c.Log.Error("virtual network status patch failed", "namespace", v.Namespace, "name", v.Name, "error", err)
		}
	}
}

// VirtualNetworkPlan is the status one VirtualNetwork should have.
type VirtualNetworkPlan struct {
	Namespace, Name string
	Status          model.VirtualNetworkStatus
	prev            model.VirtualNetworkStatus
}

// PlanVirtualNetworks computes every network's status from the current
// objects. VNIs are cluster-wide: a pinned spec.vni already in use by another
// network fails the later one; allocated VNIs stick in status.
func PlanVirtualNetworks(vnets []model.VirtualNetwork, machines []model.Machine, nodes []model.Node) []VirtualNetworkPlan {
	sort.Slice(vnets, func(i, j int) bool {
		a, b := vnets[i].Metadata, vnets[j].Metadata
		if !a.CreationTimestamp.Equal(b.CreationTimestamp) {
			return a.CreationTimestamp.Before(b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	nodeIP := map[string]string{}
	for _, n := range nodes {
		nodeIP[n.Metadata.Name] = model.NodeInternalIP(n)
	}
	members := map[string][]model.VirtualNetworkMember{}
	for _, m := range machines {
		ref := m.Spec.Network.VirtualNetwork
		if ref == nil {
			continue
		}
		key := m.Namespace() + "/" + ref.Name
		members[key] = append(members[key], model.VirtualNetworkMember{
			Machine:    m.Metadata.Name,
			MAC:        strings.ToLower(m.EffectiveNetwork().MAC),
			IP:         ref.IP,
			FloatingIP: ref.FloatingIP,
			Node:       m.Status.NodeName,
			NodeIP:     nodeIP[m.Status.NodeName],
		})
	}

	// VNIs already held: status first (sticky), then pinned specs.
	owner := map[uint32]string{}
	for _, v := range vnets {
		if v.Status.VNI != 0 && v.Metadata.DeletionTimestamp == nil {
			k := v.Namespace() + "/" + v.Metadata.Name
			if _, taken := owner[v.Status.VNI]; !taken {
				owner[v.Status.VNI] = k
			}
		}
	}
	floating := map[string]string{}
	out := make([]VirtualNetworkPlan, 0, len(vnets))
	for _, v := range vnets {
		key := v.Namespace() + "/" + v.Metadata.Name
		st := model.VirtualNetworkStatus{
			VNI:                v.Status.VNI,
			Allocations:        v.Status.Allocations,
			ObservedGeneration: v.Metadata.Generation,
		}
		fail := func(msg string) {
			st.Phase, st.Message = "Failed", msg
		}
		gw, err := v.Spec.EffectiveGateway()
		switch {
		case err != nil:
			fail(err.Error())
		case model.ValidateVirtualNetworkSpec(v.Spec) != nil:
			fail(model.ValidateVirtualNetworkSpec(v.Spec).Error())
		case v.Spec.VNI != 0 && st.VNI != 0 && v.Spec.VNI != st.VNI:
			fail(fmt.Sprintf("spec.vni cannot change from %d once allocated", st.VNI))
		default:
			if st.VNI == 0 {
				if v.Spec.VNI != 0 {
					if o, taken := owner[v.Spec.VNI]; taken && o != key {
						fail(fmt.Sprintf("spec.vni %d is used by VirtualNetwork %s", v.Spec.VNI, o))
						break
					}
					st.VNI = v.Spec.VNI
				} else {
					used := map[uint32]bool{}
					for n := range owner {
						used[n] = true
					}
					for _, o := range vnets {
						if o.Spec.VNI != 0 {
							used[o.Spec.VNI] = true
						}
					}
					vni, err := model.AllocateVNI(used)
					if err != nil {
						fail(err.Error())
						break
					}
					st.VNI = vni
				}
				owner[st.VNI] = key
			} else if o := owner[st.VNI]; o != key {
				fail(fmt.Sprintf("vni %d is also claimed by VirtualNetwork %s", st.VNI, o))
				break
			}
			st.Gateway = gw.String()
			ms := members[key]
			// A floating IP belongs to one Machine cluster-wide; later claims are refused.
			refusedFIP := map[string]string{}
			for i := range ms {
				f := ms[i].FloatingIP
				if f == "" {
					continue
				}
				who := key + "/" + ms[i].Machine
				if o, taken := floating[f]; taken && o != who {
					refusedFIP[ms[i].Machine] = fmt.Sprintf("floating IP %s is used by %s", f, o)
					ms[i].FloatingIP = ""
					continue
				}
				floating[f] = who
			}
			allocs, refused, err := model.AllocateVirtualNetwork(v.Spec, v.Status.Allocations, ms)
			if err != nil {
				fail(err.Error())
				break
			}
			for k, why := range refusedFIP {
				if _, ok := refused[k]; !ok {
					refused[k] = why
				}
			}
			st.Allocations = allocs
			st.Phase = "Ready"
			st.Message = refusalMessage(refused)
		}
		out = append(out, VirtualNetworkPlan{Namespace: v.Namespace(), Name: v.Metadata.Name, Status: st, prev: v.Status})
	}
	return out
}

func refusalMessage(refused map[string]string) string {
	if len(refused) == 0 {
		return ""
	}
	names := make([]string, 0, len(refused))
	for n := range refused {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("Machine %s: %s", n, refused[n]))
	}
	return strings.Join(parts, "; ")
}
