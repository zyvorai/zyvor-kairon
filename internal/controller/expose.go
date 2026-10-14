// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileExposedServices keeps the EndpointSlice behind every Service
// `kaironctl expose` created pointed at its Machine's current guest IP,
// which changes across restarts and migrations. A Machine that's gone
// leaves its Service with no endpoints; deleting the Machine deletes the
// Service through its owner reference.
func (c *Controller) reconcileExposedServices(ctx context.Context, machines []model.Machine) {
	svcs, err := c.Kube.ListExposedServices(ctx)
	if err != nil {
		c.exposeMu.Lock()
		defer c.exposeMu.Unlock()
		if !c.exposeWarned {
			c.Log.Warn("cannot list exposed Services; kaironctl expose endpoints will not follow guest IP changes (check the controller's services/endpointslices RBAC)", "error", err)
			c.exposeWarned = true
		}
		return
	}
	c.exposeMu.Lock()
	c.exposeWarned = false
	c.exposeMu.Unlock()
	index := make(map[string]*model.Machine, len(machines))
	for i := range machines {
		index[machines[i].Namespace()+"/"+machines[i].Metadata.Name] = &machines[i]
	}
	for _, svc := range svcs {
		m := index[svc.Metadata.Namespace+"/"+svc.Metadata.Labels[kube.LabelExposeMachine]]
		if _, err := c.Kube.SyncExposeSlice(ctx, svc, m); err != nil {
			c.Log.Error("exposed service endpoint sync failed", "namespace", svc.Metadata.Namespace, "service", svc.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("expose")
			}
		}
	}
}
