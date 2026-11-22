// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/tenantfence"
)

// reconcileTenantFences projects spec.tenant onto the Machine label and
// owns one NetworkSecurityGroup per opted-in namespace/tenant. The group
// is the deny list kairon-node merges into FluxVM policy. Failures are
// logged and retried next tick; they do not abort placement.
func (c *Controller) reconcileTenantFences(ctx context.Context, machines []model.Machine) {
	for _, m := range machines {
		if m.Metadata.DeletionTimestamp != nil {
			continue
		}
		if !tenantfence.NeedsLabel(m.Metadata.Labels, m.Spec.Tenant) {
			continue
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, tenantfence.LabelPatch(m.Spec.Tenant)); err != nil {
			c.Log.Error("tenant label sync failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		}
	}
	views := make([]tenantfence.View, 0, len(machines))
	for _, m := range machines {
		views = append(views, tenantfence.View{
			Namespace:   m.Namespace(),
			Name:        m.Metadata.Name,
			Tenant:      m.Spec.Tenant,
			GuestIP:     m.Status.GuestIP,
			GuestIPs:    m.Status.GuestIPs,
			Annotations: m.Metadata.Annotations,
			Deleting:    m.Metadata.DeletionTimestamp != nil,
		})
	}
	desired := tenantfence.Desired(views)
	existing, err := c.Kube.ListNetworkSecurityGroups(ctx)
	if err != nil {
		if !kube.IsNotFound(err) {
			c.Log.Error("tenant fence list failed", "error", err)
		}
		return
	}
	create, patches, del := tenantfence.Plan(desired, existing)
	for _, obj := range create {
		if _, err := c.Kube.CreateNetworkSecurityGroup(ctx, obj.Namespace(), obj); err != nil {
			c.Log.Error("tenant fence create failed", "namespace", obj.Namespace(), "group", obj.Metadata.Name, "error", err)
		}
	}
	for _, p := range patches {
		if err := c.Kube.PatchNetworkSecurityGroup(ctx, p.Namespace, p.Name, p.Patch); err != nil {
			c.Log.Error("tenant fence patch failed", "namespace", p.Namespace, "group", p.Name, "error", err)
		}
	}
	for _, g := range del {
		if err := c.Kube.DeleteNetworkSecurityGroup(ctx, g.Namespace, g.Name); err != nil && !kube.IsNotFound(err) {
			c.Log.Error("tenant fence delete failed", "namespace", g.Namespace, "group", g.Name, "error", err)
		}
	}
}
