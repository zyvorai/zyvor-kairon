// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"sort"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileMacNetworks allocates each MacNetwork's subnet, port and member
// Macs, and returns them by namespace/name for the scheduler. Keys are not
// Kairon's: each Mac derives them from its fabric secret.
func (c *Controller) reconcileMacNetworks(ctx context.Context, nodes []model.Node) map[string]model.MacNetwork {
	nets, err := c.Kube.ListMacNetworks(ctx)
	if err != nil {
		if !kube.IsNotFound(err) {
			c.Log.Error("list mac networks", "error", err)
		}
		return nil
	}
	// Networks that already hold a subnet keep it; the oldest wins a clash.
	sort.Slice(nets, func(i, j int) bool {
		a, b := nets[i].Metadata, nets[j].Metadata
		if !a.CreationTimestamp.Equal(b.CreationTimestamp) {
			return a.CreationTimestamp.Before(b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	taken := map[int]bool{}
	out := make(map[string]model.MacNetwork, len(nets))
	for _, n := range nets {
		st := model.PlanMacNetwork(n, nodes, taken)
		if st.SubnetIndex != 0 {
			taken[st.SubnetIndex] = true
		}
		if !reflect.DeepEqual(st, n.Status) {
			if err := c.Kube.PatchMacNetworkStatus(ctx, n.Namespace(), n.Metadata.Name, st); err != nil {
				c.Log.Error("mac network status patch failed", "namespace", n.Namespace(), "network", n.Metadata.Name, "error", err)
			}
		}
		n.Status = st
		out[n.Namespace()+"/"+n.Metadata.Name] = n
	}
	return out
}
