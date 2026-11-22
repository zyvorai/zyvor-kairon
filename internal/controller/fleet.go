// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/model"
)

func (c *Controller) reconcileFleet(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) error {
	if c.Fleet == nil {
		return nil
	}
	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil {
		return err
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		return err
	}
	c.Fleet.AdmitDisruption = func(m model.Machine) error {
		if reason := AdmitDisruption(states, m); reason != "" {
			return fmt.Errorf("%s", reason)
		}
		return nil
	}
	c.Fleet.AdmitScaleDown = func(targets []model.Machine, count int) error {
		// Any member could be selected by the target's independent scale-down
		// controller. Reserve the worst-case allowance from each matching budget.
		for _, st := range states {
			matched := 0
			for _, m := range targets {
				if m.Namespace() == st.budget.Namespace() && model.LabelsMatch(m.Metadata.Labels, st.budget.Spec.Selector) {
					matched++
				}
			}
			spend := count
			if spend > matched {
				spend = matched
			}
			if st.allowed < spend {
				return fmt.Errorf("MachineDisruptionBudget %s has insufficient allowance", st.budget.Metadata.Name)
			}
		}
		for _, st := range states {
			matched := 0
			for _, m := range targets {
				if m.Namespace() == st.budget.Namespace() && model.LabelsMatch(m.Metadata.Labels, st.budget.Spec.Selector) {
					matched++
				}
			}
			spend := count
			if spend > matched {
				spend = matched
			}
			st.allowed -= spend
		}
		return nil
	}
	return c.Fleet.Reconcile(ctx, machines, nodes, migrations)
}

func (c *Controller) validateFleet(_ *http.Request, req *admission.Request) admission.Decision {
	if req.Operation != "CREATE" && req.Operation != "UPDATE" {
		return admission.Allow()
	}
	var obj model.FleetResource
	if err := json.Unmarshal(req.Object, &obj); err != nil {
		return admission.Deny("malformed fleet resource")
	}
	if obj.Metadata.Namespace == "" {
		obj.Metadata.Namespace = req.Namespace
	}
	if err := fleet.Validate(obj); err != nil {
		return admission.Deny(err.Error())
	}
	resource, err := fleet.ResourceForKind(obj.Kind)
	if err != nil || req.Resource.Group != "fleet.kairon.zyvor.dev" || req.Resource.Resource != resource {
		return admission.Deny("fleet resource kind does not match REST resource")
	}
	if req.Operation == "UPDATE" {
		var old model.FleetResource
		if err := json.Unmarshal(req.OldObject, &old); err != nil {
			return admission.Deny("old resource required")
		}
		var oldSpec, newSpec map[string]any
		_ = json.Unmarshal(old.Spec, &oldSpec)
		_ = json.Unmarshal(obj.Spec, &newSpec)
		if obj.Kind == "MachineRecoveryPlan" {
			delete(oldSpec, "start")
			delete(newSpec, "start")
		}
		if !reflect.DeepEqual(oldSpec, newSpec) && (obj.Kind == "MachineTemplateVersion" || obj.Kind == "MachineActionApproval" || obj.Kind == "MachineVirtualNetwork" || obj.Kind == "NodeFenceRequest" || obj.Kind == "MachineRecoveryPlan" || obj.Kind == "MachineImportPlan" || obj.Kind == "MachineTemplateClaim" || obj.Kind == "MachineNetworkClaim") {
			return admission.Deny("this fleet spec is immutable; create a new named resource")
		}
	}
	if obj.Kind == "MachineActionApproval" {
		var spec fleet.ApprovalSpec
		_ = json.Unmarshal(obj.Spec, &spec)
		if req.Operation == "CREATE" {
			if spec.Approver != req.UserInfo.Username || spec.Approver == spec.Principal {
				return admission.Deny("approver must match authenticated Kubernetes identity and differ from the agent")
			}
			now := time.Now()
			if !now.Before(spec.ExpiresAt) || spec.ExpiresAt.After(now.Add(10*time.Minute)) {
				return admission.Deny("approval expiry must be within ten minutes")
			}
			if obj.Status.Phase != "" {
				return admission.Deny("new approval cannot contain status")
			}
		}
		if req.Operation == "UPDATE" {
			var old model.FleetResource
			_ = json.Unmarshal(req.OldObject, &old)
			if !reflect.DeepEqual(old.Status, obj.Status) {
				if old.Status.Phase != "" || obj.Status.Phase != "Consumed" || req.UserInfo.Username != spec.Principal || obj.Status.LastActionTime == nil || !time.Now().Before(spec.ExpiresAt) {
					return admission.Deny("approval status can only transition once to Consumed by its bound principal before expiry")
				}
			}
		}
	}
	return admission.Allow()
}
